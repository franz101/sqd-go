#!/usr/bin/env python3
"""
Fetch Polymarket PnL from data-api for a wallet.
Combines closed-positions (realized) + positions (cashPnl + realizedPnl).
Mirrors the PnL semantics of ch_pnl.py:
  - realized_pnl = sum of all trade-level realizedPnl from closed-positions
  - holdings (unrealized) = sum of cashPnl from open positions
  - total_pnl = realized_pnl + cashPnl
"""
import os, sys, json, time, re
from decimal import Decimal
from pathlib import Path
import urllib.request, urllib.error

WALLET = sys.argv[1].lower() if len(sys.argv) > 1 else ""
if not WALLET:
    sys.exit(1)

HEADERS = {
    "Accept": "application/json, text/plain, */*",
    "User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.5 Safari/605.1.15",
    "Referer": "https://polymarket.com/",
    "Origin": "https://polymarket.com",
}

def dec(s) -> Decimal:
    return Decimal(str(s))

def fetch_paginated(base_url, params_fmt, limit=50):
    """Paginate through data-api endpoint. Returns all items."""
    all_items = []
    offset = 0
    while True:
        url = base_url + params_fmt.format(wallet=WALLET, limit=limit, offset=offset)
        req = urllib.request.Request(url, headers=HEADERS)
        try:
            with urllib.request.urlopen(req, timeout=30) as r:
                data = json.loads(r.read().decode())
        except (urllib.error.URLError, Exception) as e:
            print(f"  [WARN] fetch error at offset {offset}: {e}", file=sys.stderr)
            break
        if not data:
            break
        all_items.extend(data)
        offset += len(data)
        if len(data) < limit:
            break
    return all_items

def fetch_closed_positions():
    """Fetch all closed-positions via pagination."""
    base = "https://data-api.polymarket.com/closed-positions?"
    params = "user={wallet}&sortBy=realizedpnl&sortDirection=DESC&limit={limit}&offset={offset}"
    items = fetch_paginated(base, params)
    return items

def fetch_open_positions():
    """Fetch all open positions via pagination."""
    base = "https://data-api.polymarket.com/positions?"
    params = "user={wallet}&sortBy=CASHPNL&sortDirection=DESC&limit={limit}&offset={offset}"
    items = fetch_paginated(base, params)
    return items

def compute_pnl(closed, open_pos):
    """
    Compute PnL matching ch_pnl.py semantics:
    - realized_pnl: sum of all realizedPnl from closed positions (trade-level, no dedup)
    - holdings (unrealized cost): sum of cashPnl from open positions
    - total_pnl = realized_pnl + holdings (cashPnl)
    - Also includes realizedPnl from open positions in the realized total
    """
    realized = Decimal("0")
    for p in closed:
        realized += dec(p.get("realizedPnl", 0) or 0)

    # Open positions: cashPnl = unrealized, realizedPnl = realized portion
    open_cash = Decimal("0")
    open_realized = Decimal("0")
    for p in open_pos:
        open_cash += dec(p.get("cashPnl", 0) or 0)
        open_realized += dec(p.get("realizedPnl", 0) or 0)

    total_realized = realized + open_realized
    total_pnl = total_realized + open_cash

    return {
        "realized_pnl": total_realized,
        "closed_realized_pnl": realized,
        "open_realized_pnl": open_realized,
        "open_cash_pnl": open_cash,
        "holdings_cost": open_cash,  # cashPnl is the unrealized PnL
        "net_equity": total_pnl,
        "closed_count": len(closed),
        "open_count": len(open_pos),
    }

# MAIN
print(f"Wallet: {WALLET}")
print("=" * 80)

t0 = time.time()
print("Fetching closed positions...")
closed = fetch_closed_positions()
print(f"  Got {len(closed)} closed position entries ({time.time()-t0:.1f}s)")

t0 = time.time()
print("Fetching open positions...")
open_pos = fetch_open_positions()
print(f"  Got {len(open_pos)} open positions ({time.time()-t0:.1f}s)")

pnl = compute_pnl(closed, open_pos)

print()
print(f"{'─'*80}")
print(f"  Closed positions:     {pnl['closed_count']:>6d} entries")
print(f"    Realized PnL:       ${pnl['closed_realized_pnl']:>12,.2f}")
print(f"  Open positions:       {pnl['open_count']:>6d} entries")
print(f"    Cash PnL (unreal):  ${pnl['open_cash_pnl']:>12,.2f}")
print(f"    Realized PnL:       ${pnl['open_realized_pnl']:>12,.2f}")
print(f"{'─'*80}")
print(f"  TOTAL PnL:            ${pnl['net_equity']:>12,.2f}")
print(f"    (= closed realized + open cash + open realized)")
print()

# Save JSON
out_dir = Path("tmp")
out_dir.mkdir(exist_ok=True)
out = {
    "wallet": WALLET,
    "pnl": {k: float(v) if isinstance(v, Decimal) else v for k, v in pnl.items()},
    "closed_positions": [{k: str(v) if isinstance(v, Decimal) else v for k, v in p.items()} for p in closed],
    "open_positions": [{k: str(v) if isinstance(v, Decimal) else v for k, v in p.items()} for p in open_pos],
}
out_path = out_dir / f"polymarket_pnl_{WALLET[:10]}.json"
with open(out_path, "w") as f:
    json.dump(out, f, indent=2, default=str)
print(f"Saved: {out_path}")
