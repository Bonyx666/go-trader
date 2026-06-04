"""
Consolidation Range (breakout follower) — entry side.

Detects a mature consolidation box (trailing ``min_bars`` whose high-low span is
within ``box_width_pct`` of mid) and signals an entry when price sits near an
edge: long in the bottom ``edge_entry_frac`` of the box, short in the top. The
exit (tight initial stop + full trailing ratchet) is owned by the close/stop
machinery — this module only emits entries.

Validated config (BTC 4h): box_width_pct=0.05, min_bars=16, edge_entry_frac=0.2.
Research + backtest: docs/research/consolidation-findings.md.
"""

import numpy as np
import pandas as pd


def consolidation_range_core(
    df: pd.DataFrame,
    box_width_pct: float = 0.05,
    min_bars: int = 16,
    edge_entry_frac: float = 0.2,
) -> pd.DataFrame:
    result = df.copy()

    roll_hi = result["high"].rolling(window=min_bars).max()
    roll_lo = result["low"].rolling(window=min_bars).min()
    mid = (roll_hi + roll_lo) / 2.0
    height = roll_hi - roll_lo

    safe_mid = mid.replace(0, np.nan)
    safe_height = height.replace(0, np.nan)
    width = height / safe_mid
    pos = (result["close"] - roll_lo) / safe_height  # 0=bottom edge, 1=top edge

    result["box_top"] = roll_hi
    result["box_bottom"] = roll_lo
    result["box_mid"] = mid
    result["in_range"] = (width <= box_width_pct).fillna(False)

    result["signal"] = 0
    long_entry = result["in_range"] & (pos <= edge_entry_frac)
    short_entry = result["in_range"] & (pos >= 1 - edge_entry_frac)
    result.loc[long_entry, "signal"] = 1
    result.loc[short_entry, "signal"] = -1
    return result
