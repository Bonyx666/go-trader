package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// buildSharedWalletTestState assembles two HL members (BTC, ETH) plus one
// non-member paper strategy, each with a virtual position so the reconciler can
// attribute on-chain P&L.
func buildSharedWalletTestState() (*AppState, []StrategyConfig) {
	strategies := []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 600},
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 400},
		{ID: "paper-sol", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "SOL", "1h", "--mode=paper"}, Capital: 1000},
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-btc": {ID: "hl-btc", Cash: 300, Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Side: "long", Quantity: 0.1, AvgCost: 60000},
		}},
		"hl-eth": {ID: "hl-eth", Cash: 420, Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Side: "long", Quantity: 2, AvgCost: 3000},
		}},
		"paper-sol": {ID: "paper-sol", Cash: 1000, Positions: map[string]*Position{}},
	}}
	return state, strategies
}

func TestReconcileSharedWalletDisplayValues_SetsGatesAndSums(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	state, strategies := buildSharedWalletTestState()
	sharedWallets := detectSharedWallets(strategies)
	if len(sharedWallets) != 1 {
		t.Fatalf("expected 1 shared wallet, got %d", len(sharedWallets))
	}
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	walletBalances := map[SharedWalletKey]float64{key: 1030.0} // base 1000 + 50 - 20
	hlPositions := []HLPosition{
		{Coin: "BTC", Size: 0.1, UnrealizedPnL: 50},
		{Coin: "ETH", Size: 2, UnrealizedPnL: -20},
	}

	results := reconcileSharedWalletDisplayValues(strategies, state, sharedWallets, walletBalances, hlPositions, nil, false)

	if len(results) != 1 || math.Abs(results[0].Drift) > 0.01 {
		t.Fatalf("expected 1 result with ~0 drift, got %+v", results)
	}
	btc := state.Strategies["hl-btc"]
	eth := state.Strategies["hl-eth"]
	sol := state.Strategies["paper-sol"]
	if !btc.SharedWalletValueSet || !eth.SharedWalletValueSet {
		t.Fatal("expected both HL members to have SharedWalletValueSet=true")
	}
	if sol.SharedWalletValueSet {
		t.Error("non-member paper strategy must NOT be gated on")
	}
	// base=1000; btc: 0.6*1000+50=650; eth: 0.4*1000-20=380.
	if math.Abs(btc.SharedWalletValue-650) > 0.01 {
		t.Errorf("btc value = %v, want 650", btc.SharedWalletValue)
	}
	if math.Abs(eth.SharedWalletValue-380) > 0.01 {
		t.Errorf("eth value = %v, want 380", eth.SharedWalletValue)
	}
	if sum := btc.SharedWalletValue + eth.SharedWalletValue; math.Abs(sum-1030.0) > 0.01 {
		t.Errorf("member sum %v != balance 1030", sum)
	}
	// displayStrategyValue must now return the exchange-derived value.
	if got := displayStrategyValue(btc, nil); math.Abs(got-650) > 0.01 {
		t.Errorf("displayStrategyValue(btc) = %v, want 650", got)
	}
}

// A live HL `manual` strategy on the same account holds a real on-chain
// position (returned by fetchHyperliquidState) but is not a perps member. It
// must be folded in as a member so its position is attributed (no orphan drift)
// and it receives an exchange-derived value (#920 review).
func TestReconcileSharedWalletDisplayValues_ManualMemberAttributed(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	state, strategies := buildSharedWalletTestState()
	// Add a live manual strategy on SOL (same account), with a virtual position.
	strategies = append(strategies, StrategyConfig{
		ID: "hl-manual-sol", Platform: "hyperliquid", Type: "manual",
		Symbol: "SOL", Args: []string{"hold", "SOL", "1h", "--mode=live"}, Capital: 200,
	})
	state.Strategies["hl-manual-sol"] = &StrategyState{
		ID: "hl-manual-sol", Cash: 100,
		Positions: map[string]*Position{"SOL": {Symbol: "SOL", Side: "long", Quantity: 5, AvgCost: 150}},
	}
	sharedWallets := detectSharedWallets(strategies)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	// Balance includes the SOL manual position's uPnL (+15).
	walletBalances := map[SharedWalletKey]float64{key: 1045.0} // base 1000 + 50 - 20 + 15
	hlPositions := []HLPosition{
		{Coin: "BTC", Size: 0.1, UnrealizedPnL: 50},
		{Coin: "ETH", Size: 2, UnrealizedPnL: -20},
		{Coin: "SOL", Size: 5, UnrealizedPnL: 15}, // manual's on-chain position
	}

	results := reconcileSharedWalletDisplayValues(strategies, state, sharedWallets, walletBalances, hlPositions, nil, false)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if math.Abs(results[0].Drift) > 0.01 {
		t.Fatalf("SOL manual position must be attributed (no orphan drift), got drift %v", results[0].Drift)
	}
	msol := state.Strategies["hl-manual-sol"]
	if !msol.SharedWalletValueSet {
		t.Fatal("manual member must be gated on")
	}
	// Σ all three members == balance.
	sum := state.Strategies["hl-btc"].SharedWalletValue +
		state.Strategies["hl-eth"].SharedWalletValue + msol.SharedWalletValue
	if math.Abs(sum-1045.0) > 0.01 {
		t.Errorf("member sum %v != balance 1045", sum)
	}
	// Manual gets its own uPnL (+15) plus a capital-weighted base share.
	if math.Abs(msol.SharedWalletValue-(200.0/1200.0*1000.0+15)) > 0.01 {
		t.Errorf("manual value = %v, want %v", msol.SharedWalletValue, 200.0/1200.0*1000.0+15)
	}
}

// OKX with a failed position fetch this cycle must be skipped (members fall back
// to PortfolioValue) rather than reconciled with U=0.
func TestReconcileSharedWalletDisplayValues_OKXPositionsNotFetchedSkips(t *testing.T) {
	t.Setenv("OKX_API_KEY", "okxkey")
	strategies := []StrategyConfig{
		{ID: "okx-a", Platform: "okx", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 500},
		{ID: "okx-b", Platform: "okx", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 500},
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"okx-a": {ID: "okx-a", Cash: 500, Positions: map[string]*Position{}},
		"okx-b": {ID: "okx-b", Cash: 500, Positions: map[string]*Position{}},
	}}
	sharedWallets := detectSharedWallets(strategies)
	key := SharedWalletKey{Platform: "okx", Account: "okxkey"}
	walletBalances := map[SharedWalletKey]float64{key: 1000.0}

	// okxPositionsFetched=false → OKX wallet must be skipped.
	results := reconcileSharedWalletDisplayValues(strategies, state, sharedWallets, walletBalances, nil, nil, false)
	if len(results) != 0 {
		t.Fatalf("expected OKX wallet skipped when positions not fetched, got %d results", len(results))
	}
	if state.Strategies["okx-a"].SharedWalletValueSet || state.Strategies["okx-b"].SharedWalletValueSet {
		t.Error("OKX members must fall back (Set=false) when positions fetch failed")
	}

	// With okxPositionsFetched=true it reconciles.
	results = reconcileSharedWalletDisplayValues(strategies, state, sharedWallets, walletBalances, nil, nil, true)
	if len(results) != 1 || !state.Strategies["okx-a"].SharedWalletValueSet {
		t.Fatalf("expected OKX reconcile when positions fetched, got %+v", results)
	}
}

func TestReconcileSharedWalletDisplayValues_FetchFailedFallsBack(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	state, strategies := buildSharedWalletTestState()
	// Pre-set a stale value to prove it gets cleared.
	state.Strategies["hl-btc"].SharedWalletValue = 999
	state.Strategies["hl-btc"].SharedWalletValueSet = true
	sharedWallets := detectSharedWallets(strategies)

	// Empty walletBalances simulates a failed balance fetch this cycle.
	results := reconcileSharedWalletDisplayValues(strategies, state, sharedWallets, map[SharedWalletKey]float64{}, nil, nil, false)

	if len(results) != 0 {
		t.Fatalf("expected no drift results when balance missing, got %d", len(results))
	}
	if state.Strategies["hl-btc"].SharedWalletValueSet {
		t.Error("stale SharedWalletValueSet must be cleared when fetch fails")
	}
	// Fallback to modeled PortfolioValue (cash 300 + 0.1*price; price absent → AvgCost).
	want := PortfolioValue(state.Strategies["hl-btc"], nil)
	if got := displayStrategyValue(state.Strategies["hl-btc"], nil); got != want {
		t.Errorf("display fallback = %v, want PortfolioValue %v", got, want)
	}
}

func TestDisplayStrategyValue_PrefersSetValue(t *testing.T) {
	s := &StrategyState{ID: "x", Cash: 100, Positions: map[string]*Position{}}
	if got := displayStrategyValue(s, nil); got != 100 {
		t.Errorf("unset → PortfolioValue, got %v want 100", got)
	}
	s.SharedWalletValue = 777
	s.SharedWalletValueSet = true
	if got := displayStrategyValue(s, nil); got != 777 {
		t.Errorf("set → SharedWalletValue, got %v want 777", got)
	}
}

// --- Drift alarm tracker ---

func TestSharedWalletDriftTracker_ConfirmThenThrottleThenRecover(t *testing.T) {
	tr := &SharedWalletDriftTracker{}
	now := time.Now().UTC()
	// First detection is within the confirmation window → no alert yet.
	if notify, _ := tr.Record("hyperliquid/0xabc", 5.00, now); notify {
		t.Fatal("first detection must NOT alert (confirmation window)")
	}
	// Second consecutive detection crosses the threshold → alert.
	if notify, _ := tr.Record("hyperliquid/0xabc", 5.00, now.Add(time.Minute)); !notify {
		t.Fatal("second consecutive detection must alert")
	}
	// Same drift again → throttled (no signature change, not 10th, <1h).
	if notify, _ := tr.Record("hyperliquid/0xabc", 5.00, now.Add(2*time.Minute)); notify {
		t.Error("third identical detection should be throttled")
	}
	// Materially changed drift → re-alert.
	if notify, _ := tr.Record("hyperliquid/0xabc", 9.00, now.Add(3*time.Minute)); !notify {
		t.Error("materially changed drift should re-alert")
	}
	// Recovery: within tolerance clears and reports recovered.
	recovered, prior := tr.Clear("hyperliquid/0xabc")
	if !recovered || prior == 0 {
		t.Errorf("expected recovery after alerted streak, got recovered=%v prior=%d", recovered, prior)
	}
	// Clearing a never-seen wallet is a no-op.
	if r, _ := tr.Clear("okx/none"); r {
		t.Error("clearing unknown wallet must not report recovery")
	}
}

// A one-cycle orphan (e.g. a freshly-filled limit order not yet booked into the
// virtual book) must produce NEITHER an alert NOR a recovery notice.
func TestSharedWalletDriftTracker_OneCycleTransientSilent(t *testing.T) {
	tr := &SharedWalletDriftTracker{}
	now := time.Now().UTC()
	if notify, _ := tr.Record("hyperliquid/0xabc", 25.00, now); notify {
		t.Fatal("single transient detection must not alert")
	}
	// Next cycle the book catches up → within tolerance → Clear.
	recovered, _ := tr.Clear("hyperliquid/0xabc")
	if recovered {
		t.Error("a never-alerted transient must not fire a recovery notice")
	}
}

func TestReportSharedWalletDrift_WithinToleranceNoPanic(t *testing.T) {
	// nil notifier must be safe; within-tolerance drift records nothing.
	reportSharedWalletDrift(nil, []sharedWalletDriftResult{
		{Key: SharedWalletKey{Platform: "hyperliquid", Account: "0x"}, Drift: 0.004, Balance: 100, MemberSum: 100},
	})
}

// --- Parse extensions carry unrealized P&L ---

func TestParseOKXPositionsOutput_CarriesUnrealizedPnL(t *testing.T) {
	stdout := []byte(`{"positions":[{"coin":"BTC","size":0.3,"entry_price":60000,"side":"long","unrealized_pnl":123.45}],"platform":"okx"}`)
	res, _, err := parseOKXPositionsOutput(stdout, "", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(res.Positions) != 1 || math.Abs(res.Positions[0].UnrealizedPnL-123.45) > 1e-9 {
		t.Fatalf("expected unrealized_pnl 123.45, got %+v", res.Positions)
	}
}

func TestFetchHyperliquidState_ParsesUnrealizedPnL(t *testing.T) {
	resp := map[string]interface{}{
		"marginSummary": map[string]string{"accountValue": "1000.00"},
		"assetPositions": []map[string]interface{}{
			{"position": map[string]string{
				"coin": "BTC", "szi": "0.1", "entryPx": "60000", "unrealizedPnl": "42.50",
			}},
		},
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()
	origURL := hlMainnetURL
	hlMainnetURL = ts.URL
	defer func() { hlMainnetURL = origURL }()

	_, positions, err := fetchHyperliquidState("0xabc")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(positions) != 1 || math.Abs(positions[0].UnrealizedPnL-42.50) > 1e-9 {
		t.Fatalf("expected UnrealizedPnL 42.50, got %+v", positions)
	}
}
