package main

// regime_store.go — global per-cycle regime calculator + store (#879).
//
// One dedicated regime subprocess (shared_scripts/check_regime.py) runs per
// distinct (data platform, symbol, timeframe, windows-spec) signature per
// scheduler cycle; every due strategy sharing that signature — including
// type=manual while flat and type=options — reads the same bundle from this
// store instead of recomputing regime inline in its check subprocess. The
// computed payload is injected into each check script via
// --regime-payload-json (presence disables inline prepare_check_regime).
//
// The store is in-memory only and rebuilt every cycle (like the pre-#879
// stratState.Regime refresh); pos.Regime stays the frozen-at-open stamp.
// Failure policy (issue #879, option b): a failed/missing bundle yields an
// EMPTY payload — the entry gate fails open, syncStrategyRegimeState shows
// regime=-, and there is no reuse-last or inline-recompute fallback. Open
// positions are unaffected because regime-keyed life-of-position features
// read pos.Regime, not this store.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const regimeCheckScript = "shared_scripts/check_regime.py"

// regimeBundleMinBars mirrors the five check scripts' insufficient-data guard
// (len(candles) < 30 → error), so a signature the check would refuse to trade
// on also produces no bundle.
const regimeBundleMinBars = 30

// Options regime signature constants mirror check_options.py's hardcoded
// inline computation (REGIME_TIMEFRAME/REGIME_LIMIT/REGIME_MIN_BARS and
// latest_regime's period=14 / adx_threshold=20 defaults) so the migrated
// label is byte-identical to the pre-#879 inline one. Options regime is
// intentionally NOT gated on cfg.Regime.Enabled — the inline path never was.
const (
	optionsRegimeTimeframe       = "4h"
	optionsRegimeOhlcvLimit      = 100
	optionsRegimeMinBars         = 30
	optionsRegimeWindowsSpecJSON = `{"default":{"classifier":"adx","period":14,"adx_threshold":20}}`
)

// regimeBundleKey identifies one regime computation signature. Platform is
// the DATA SOURCE the strategy's check script fetches OHLCV from (derived
// from the dispatch branch, not sc.Platform — the default spot dispatch
// fetches BinanceUS data regardless of platform). SpecJSON is the resolved
// windows-spec JSON (deterministic: encoding/json sorts map keys), so a
// different spec — e.g. the options ADX/4h default vs the global windows —
// is a different raw computation.
type regimeBundleKey struct {
	Platform  string
	Symbol    string
	Timeframe string
	SpecJSON  string
}

func (k regimeBundleKey) String() string {
	return k.Platform + "/" + k.Symbol + "/" + k.Timeframe
}

// regimeBundleRequest is the work order for one check_regime.py invocation.
type regimeBundleRequest struct {
	Key               regimeBundleKey
	OhlcvLimit        int
	MinBars           int
	AllowSpotFallback bool // options platforms: adapter-or-BinanceUS fallback (parity with check_options)
}

// RegimeBundleViews carries both classifier vocabularies for one window —
// the dashboard's uniform 3-state/7-state portfolio view. adx3 is the real
// ADX classifier at the window's full period (exact parity with a standalone
// ADX window even past COMPOSITE_ADX_PERIOD_CAP), never a prefix-collapse.
type RegimeBundleViews struct {
	ADX3       string `json:"adx3"`
	Composite7 string `json:"composite7"`
}

// RegimeBundle is one computed store entry. RawRegimeJSON preserves the
// subprocess's exact `regime` object bytes for --regime-payload-json
// injection (re-marshaling the Go-side RegimePayload would drop snapshot
// fields Go doesn't model, e.g. per-window "classifier").
type RegimeBundle struct {
	Key           regimeBundleKey
	Payload       RegimePayload
	RawRegimeJSON string
	Views         map[string]RegimeBundleViews
	BarTime       string
	Err           string
	At            time.Time
}

// regimeBundleOutput is the JSON contract of check_regime.py.
type regimeBundleOutput struct {
	Platform  string                       `json:"platform"`
	Symbol    string                       `json:"symbol"`
	Timeframe string                       `json:"timeframe"`
	BarTime   string                       `json:"bar_time"`
	Regime    json.RawMessage              `json:"regime"`
	Views     map[string]RegimeBundleViews `json:"views"`
	Error     string                       `json:"error"`
}

// RegimeStore is the two-layer global store, rebuilt empty every cycle.
// Guarded by its own mutex: writes happen on the main loop goroutine before
// the check fan-out; reads come from the same goroutine plus the dashboard
// HTTP handlers.
type RegimeStore struct {
	mu      sync.RWMutex
	entries map[regimeBundleKey]*RegimeBundle
	builtAt time.Time
}

// globalRegimeStore is the process-wide store. Package-level (like the other
// cross-cutting singletons in this package) so run*Check arg builders, the
// manual path, and the dashboard handler read it without threading a handle
// through every dispatch signature.
var globalRegimeStore = &RegimeStore{}

func (s *RegimeStore) resetForCycle(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = make(map[regimeBundleKey]*RegimeBundle)
	s.builtAt = now
}

func (s *RegimeStore) set(b *RegimeBundle) {
	if b == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[regimeBundleKey]*RegimeBundle)
	}
	s.entries[b.Key] = b
}

func (s *RegimeStore) get(key regimeBundleKey) (*RegimeBundle, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.entries[key]
	return b, ok
}

// Snapshot returns the cycle's bundles sorted by key for operator-facing
// output (Go map iteration is randomized).
func (s *RegimeStore) Snapshot() ([]*RegimeBundle, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*RegimeBundle, 0, len(s.entries))
	for _, b := range s.entries {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Key, out[j].Key
		if a.Platform != b.Platform {
			return a.Platform < b.Platform
		}
		if a.Symbol != b.Symbol {
			return a.Symbol < b.Symbol
		}
		if a.Timeframe != b.Timeframe {
			return a.Timeframe < b.Timeframe
		}
		return a.SpecJSON < b.SpecJSON
	})
	return out, s.builtAt
}

// strategyRegimeDataPlatform maps a strategy to the data source its check
// script fetches OHLCV from. Mirrors the dispatch switch in the main loop.
func strategyRegimeDataPlatform(sc StrategyConfig) string {
	switch sc.Type {
	case "spot":
		switch sc.Platform {
		case "okx":
			return "okx"
		case "robinhood":
			return "robinhood"
		default:
			// Default spot dispatch runs check_strategy.py, which fetches
			// BinanceUS via shared_tools/data_fetcher for every platform.
			return "binanceus"
		}
	case "perps":
		if sc.Platform == "okx" {
			return "okx"
		}
		return "hyperliquid"
	case "futures":
		return "topstep"
	case "manual":
		return "hyperliquid"
	case "options":
		return strings.TrimSpace(sc.Platform)
	}
	return ""
}

// strategyArgSymbolTimeframe extracts the positional <symbol> <timeframe>
// pair shared by every check-script argv shape (manual auto-fills the same
// layout: ["hold", symbol, timeframe, ...]).
func strategyArgSymbolTimeframe(args []string) (string, string) {
	if len(args) < 3 {
		return "", ""
	}
	symbol := strings.TrimSpace(args[1])
	timeframe := strings.TrimSpace(args[2])
	if symbol == "" || timeframe == "" || strings.HasPrefix(symbol, "-") || strings.HasPrefix(timeframe, "-") {
		return "", ""
	}
	return symbol, timeframe
}

// strategyRegimeBundleRequest resolves sc's regime signature for this cycle.
// ok=false means the strategy reads no bundle (regime disabled for non-options
// types, or an unresolvable symbol/timeframe) and its check script receives no
// --regime-payload-json flag.
func strategyRegimeBundleRequest(sc StrategyConfig, rc *RegimeConfig) (regimeBundleRequest, bool) {
	if sc.Type == "options" {
		platform := strategyRegimeDataPlatform(sc)
		if platform == "" || len(sc.Args) < 2 {
			return regimeBundleRequest{}, false
		}
		underlying := strings.TrimSpace(sc.Args[1])
		if underlying == "" || strings.HasPrefix(underlying, "-") {
			return regimeBundleRequest{}, false
		}
		return regimeBundleRequest{
			Key: regimeBundleKey{
				Platform:  platform,
				Symbol:    strings.ToUpper(underlying), // check_options upper-cases the underlying
				Timeframe: optionsRegimeTimeframe,
				SpecJSON:  optionsRegimeWindowsSpecJSON,
			},
			OhlcvLimit:        optionsRegimeOhlcvLimit,
			MinBars:           optionsRegimeMinBars,
			AllowSpotFallback: true,
		}, true
	}
	if rc == nil || !rc.Enabled {
		return regimeBundleRequest{}, false
	}
	specJSON := regimeWindowsSpecJSON(rc)
	if specJSON == "" {
		return regimeBundleRequest{}, false
	}
	platform := strategyRegimeDataPlatform(sc)
	if platform == "" {
		return regimeBundleRequest{}, false
	}
	symbol, timeframe := strategyArgSymbolTimeframe(sc.Args)
	if symbol == "" || timeframe == "" {
		return regimeBundleRequest{}, false
	}
	return regimeBundleRequest{
		Key: regimeBundleKey{
			Platform:  platform,
			Symbol:    symbol,
			Timeframe: timeframe,
			SpecJSON:  specJSON,
		},
		OhlcvLimit: regimeRequiredOhlcvLimit(rc),
		MinBars:    regimeBundleMinBars,
	}, true
}

// collectRegimeBundleRequests unions the distinct signatures of the due
// strategies — the per-cycle population step. Deterministic order so logs
// and tests are stable.
func collectRegimeBundleRequests(due []StrategyConfig, rc *RegimeConfig) []regimeBundleRequest {
	seen := make(map[regimeBundleKey]bool)
	out := make([]regimeBundleRequest, 0, len(due))
	for _, sc := range due {
		req, ok := strategyRegimeBundleRequest(sc, rc)
		if !ok || seen[req.Key] {
			continue
		}
		seen[req.Key] = true
		out = append(out, req)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Key, out[j].Key
		if a.Platform != b.Platform {
			return a.Platform < b.Platform
		}
		if a.Symbol != b.Symbol {
			return a.Symbol < b.Symbol
		}
		if a.Timeframe != b.Timeframe {
			return a.Timeframe < b.Timeframe
		}
		return a.SpecJSON < b.SpecJSON
	})
	return out
}

// regimeBundleCheckArgs builds the check_regime.py argv for one request.
func regimeBundleCheckArgs(req regimeBundleRequest) []string {
	args := []string{
		"--platform", req.Key.Platform,
		"--symbol", req.Key.Symbol,
		"--timeframe", req.Key.Timeframe,
		"--regime-windows-spec-json", req.Key.SpecJSON,
		"--ohlcv-limit", strconv.Itoa(req.OhlcvLimit),
		"--min-bars", strconv.Itoa(req.MinBars),
	}
	if req.AllowSpotFallback {
		args = append(args, "--allow-spot-fallback")
	}
	return args
}

// parseRegimeBundleOutput parses check_regime.py stdout into a bundle. Pure
// helper so Go CI exercises the contract without spawning Python.
func parseRegimeBundleOutput(key regimeBundleKey, data []byte, now time.Time) (*RegimeBundle, error) {
	var out regimeBundleOutput
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("regime bundle %s: bad JSON: %w", key, err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("regime bundle %s: %s", key, out.Error)
	}
	if len(out.Regime) == 0 || string(out.Regime) == "null" {
		return nil, fmt.Errorf("regime bundle %s: missing regime payload", key)
	}
	var payload RegimePayload
	if err := json.Unmarshal(out.Regime, &payload); err != nil {
		return nil, fmt.Errorf("regime bundle %s: bad regime payload: %w", key, err)
	}
	if payload.IsEmpty() {
		return nil, fmt.Errorf("regime bundle %s: empty regime payload", key)
	}
	return &RegimeBundle{
		Key:           key,
		Payload:       payload,
		RawRegimeJSON: string(out.Regime),
		Views:         out.Views,
		BarTime:       out.BarTime,
		At:            now,
	}, nil
}

// runRegimeBundleCheckFn is the subprocess invoker — package var so tests can
// stub the Python boundary (Go CI must not depend on spawning Python).
var runRegimeBundleCheckFn = runRegimeBundleCheck

func runRegimeBundleCheck(req regimeBundleRequest) (*RegimeBundle, error) {
	stdout, stderr, err := runPythonReadOnly(regimeCheckScript, regimeBundleCheckArgs(req))
	now := time.Now().UTC()
	// Subprocess contract: JSON on stdout even on error; parse regardless of
	// exit code and prefer the script's structured error over the exit error.
	if bundle, perr := parseRegimeBundleOutput(req.Key, stdout, now); perr == nil {
		return bundle, nil
	} else if err == nil {
		return nil, perr
	}
	detail := strings.TrimSpace(string(stderr))
	if msg := regimeBundleErrorMessage(stdout); msg != "" {
		detail = msg
	}
	if detail == "" {
		detail = err.Error()
	}
	return nil, fmt.Errorf("regime bundle %s: %s", req.Key, detail)
}

// regimeBundleErrorMessage extracts the structured "error" field when the
// script crashed with a JSON error body.
func regimeBundleErrorMessage(stdout []byte) string {
	var out regimeBundleOutput
	if err := json.Unmarshal(stdout, &out); err != nil {
		return ""
	}
	return strings.TrimSpace(out.Error)
}

// regimeBundleAlertConfig is the synthetic StrategyConfig identity used to
// thread regime-subprocess failures through the existing per-key
// scriptFailureTracker throttling (#829 pattern).
func regimeBundleAlertConfig(key regimeBundleKey) StrategyConfig {
	return StrategyConfig{
		ID:       "regime[" + key.String() + "]",
		Platform: key.Platform,
		Script:   regimeCheckScript,
	}
}

// populateRegimeStore rebuilds the global store for this cycle: clear, union
// due-strategy signatures, one subprocess per distinct signature (parallel;
// pythonSemaphore caps concurrency at 4), project entries. Runs on the main
// loop goroutine BEFORE the per-strategy check fan-out, without holding the
// state mutex. A failed signature leaves NO entry — consumers see an empty
// payload and fail open (issue #879 failure policy b).
func populateRegimeStore(store *RegimeStore, due []StrategyConfig, rc *RegimeConfig, notifier *MultiNotifier) {
	store.resetForCycle(time.Now().UTC())
	reqs := collectRegimeBundleRequests(due, rc)
	if len(reqs) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, req := range reqs {
		req := req
		wg.Add(1)
		go func() {
			defer wg.Done()
			bundle, err := runRegimeBundleCheckFn(req)
			if err != nil {
				fmt.Printf("[WARN] regime store: %v\n", err)
				notifyScriptFailure(notifier, regimeBundleAlertConfig(req.Key), scriptFailureError, err.Error())
				return
			}
			store.set(bundle)
			clearScriptFailure(notifier, regimeBundleAlertConfig(req.Key))
		}()
	}
	wg.Wait()
	if summary := regimeStoreSummary(store); summary != "" {
		fmt.Printf("Regime: %s\n", summary)
	}
}

// regimeStoreSummary renders one line of primary labels for the cycle log,
// e.g. "hyperliquid/BTC/1h=trending_up; deribit/ETH/4h=ranging". Sorted by
// key (map iteration is randomized).
func regimeStoreSummary(store *RegimeStore) string {
	bundles, _ := store.Snapshot()
	parts := make([]string, 0, len(bundles))
	for _, b := range bundles {
		label := b.Payload.PrimaryLabel(nil)
		if label == "" {
			label = "-"
		}
		parts = append(parts, b.Key.String()+"="+label)
	}
	return strings.Join(parts, "; ")
}

// PayloadForStrategy returns the live regime payload for sc's signature this
// cycle. Empty when sc has no signature (regime disabled) or the bundle is
// missing/failed — every consumer's existing empty-case behavior is the
// fail-open path.
func (s *RegimeStore) PayloadForStrategy(sc StrategyConfig, rc *RegimeConfig) RegimePayload {
	req, ok := strategyRegimeBundleRequest(sc, rc)
	if !ok {
		return RegimePayload{}
	}
	b, found := s.get(req.Key)
	if !found || b == nil {
		return RegimePayload{}
	}
	return b.Payload
}

// InjectionJSONForStrategy returns (raw payload JSON, true) when sc's check
// script should receive --regime-payload-json this cycle. The flag is passed
// whenever sc HAS a signature — with an EMPTY value after a bundle failure,
// which tells the script "do not recompute inline; resolve empty/fail-open"
// (regime_from_injected_payload). ok=false omits the flag entirely (regime
// disabled), leaving the script's inline path untouched for manual CLI runs.
func (s *RegimeStore) InjectionJSONForStrategy(sc StrategyConfig, rc *RegimeConfig) (string, bool) {
	req, ok := strategyRegimeBundleRequest(sc, rc)
	if !ok {
		return "", false
	}
	b, found := s.get(req.Key)
	if !found || b == nil {
		return "", true
	}
	return b.RawRegimeJSON, true
}
