#!/usr/bin/env python3
"""
Compare PnL from 3 sources: Local CH, Remote CH, Goldsky Subgraph.
Outputs positions table + summary for a wallet.
"""
import os, sys, json, time, re
from decimal import Decimal
from pathlib import Path
import requests
from requests.auth import HTTPBasicAuth
from dotenv import load_dotenv

project_root = Path(__file__).resolve().parent.parent
load_dotenv(project_root / ".env")

# --- Config ---
LOCAL_CH_URL  = f"http://localhost:{os.getenv('CLICKHOUSE_HTTP_PORT','8135')}/"
LOCAL_CH_AUTH = (os.getenv("CLICKHOUSE_USER", "default"), os.getenv("CLICKHOUSE_PASSWORD", "sqd-clickhouse"))

REMOTE_CH_URL  = "https://crypto-clickhouse.clickhouse.com/"
REMOTE_CH_AUTH = ("crypto", "")

GOLDSKY_URL = "https://api.goldsky.com/api/public/project_cl6mb8i9h0003e201j6li0diw/subgraphs/pnl-subgraph/0.0.14/gn"

WALLET = sys.argv[1].lower() if len(sys.argv) > 1 else "0x27b92311397f495dde200ad9cc3684f71d3ad493"

HEADERS_JSON = {"Content-Type": "text/plain; charset=UTF-8", "Accept": "*/*"}
SCALE = Decimal(1e6)  # Goldsky subgraph stores values × 1e6

def dec(s) -> Decimal:
    """Parse Decimal, stripping trailing .000... from CH Decimal(76,18)"""
    return Decimal(str(s))

def ch_data_to_positions(data_rows: list, token_key="token_id", amount_key="amount",
                         price_key="avg_price", pnl_key="realized_pnl",
                         bought_key="total_bought", block_key=None, scale=True) -> list:
    """Convert ClickHouse JSON 'data' rows to normalized position dicts."""
    positions = []
    for r in data_rows:
        amt = dec(r[amount_key])
        prc = dec(r[price_key])
        rpnl = dec(r[pnl_key])
        tb = dec(r[bought_key])
        if scale:
            amt /= SCALE
            prc /= SCALE
            rpnl /= SCALE
            tb /= SCALE
        # Token ID: might be hex string (local CH FixedString(32)) or decimal string (remote CH)
        tid = str(r[token_key]).rstrip("0").rstrip(".")  # strip Decimal(76,18) trailing zeros
        # If it's a pure decimal number, convert to hex
        if re.match(r"^\d+$", tid):
            tid = hex(int(tid))
        positions.append({
            "token_id": tid,
            "amount": amt, "avg_price": prc,
            "realized_pnl": rpnl, "total_bought": tb,
            "block": int(r[block_key]) if block_key and r.get(block_key) else None,
        })
    return positions

def now(): return time.time()

# ============================================================
# LOCAL ClickHouse — memory_user_positions
# ============================================================
def fetch_local_ch(wallet: str):
    sql = f"""
    SELECT token_id, amount, avg_price, realized_pn_l, total_bought, block_number
    FROM polymarket.memory_user_positions FINAL
    WHERE user = '{wallet}' AND total_bought > 0
    ORDER BY block_number DESC
    FORMAT JSON
    """
    t0 = now()
    r = requests.post(LOCAL_CH_URL, params={"result_overflow_mode": "break"},
                      data=sql.encode(), headers=HEADERS_JSON,
                      auth=HTTPBasicAuth(*LOCAL_CH_AUTH), timeout=120)
    r.raise_for_status()
    elapsed = now() - t0
    d = r.json()
    rows = d.get("data", [])
    positions = ch_data_to_positions(rows, pnl_key="realized_pn_l", block_key="block_number",
                                     scale=False)  # local stores already-scaled values?
    return positions, elapsed

# ============================================================
# REMOTE ClickHouse — user_positions
# ============================================================
def fetch_remote_ch(wallet: str):
    sql = f"""
    SELECT token_id, amount, avg_price, realized_pnl, total_bought
    FROM polymarket.user_positions FINAL
    WHERE user = '{wallet}' AND is_deleted = 0 AND total_bought > 0
    ORDER BY block_number DESC
    FORMAT JSON
    """
    t0 = now()
    r = requests.post(REMOTE_CH_URL, params={"result_overflow_mode": "break"},
                      data=sql.encode(), headers=HEADERS_JSON,
                      auth=HTTPBasicAuth(*REMOTE_CH_AUTH), timeout=120)
    r.raise_for_status()
    elapsed = now() - t0
    d = r.json()
    rows = d.get("data", [])
    positions = ch_data_to_positions(rows, scale=True)
    return positions, elapsed

# ============================================================
# Goldsky Subgraph
# ============================================================
def fetch_goldsky(wallet: str):
    positions = []
    PAGE_SIZE = 50  # smaller pages avoid timeout
    skip = 0
    t0 = now()
    while True:
        r = requests.post(GOLDSKY_URL,
            json={"query": """
            query GetUserPnL($user: String!, $first: Int!, $skip: Int!) {
              userPositions(where: {user: $user, totalBought_gt: "0"}, first: $first, skip: $skip) {
                tokenId realizedPnl amount avgPrice totalBought
              }
            }""", "variables": {"user": wallet, "first": PAGE_SIZE, "skip": skip}},
            headers={"Content-Type": "application/json", "User-Agent": "Mozilla/5.0"},
            timeout=60)
        r.raise_for_status()
        data = r.json()
        if "errors" in data:
            print(f"  Goldsky error: {data['errors'][0]['message'][:120]}", file=sys.stderr)
            break
        batch = data["data"]["userPositions"]
        for p in batch:
            positions.append({
                "token_id": "0x" + p["tokenId"],
                "amount": dec(p["amount"]) / SCALE,
                "avg_price": dec(p["avgPrice"]) / SCALE,
                "realized_pnl": dec(p["realizedPnl"]) / SCALE,
                "total_bought": dec(p["totalBought"]) / SCALE,
                "block": None,
            })
        if len(batch) < PAGE_SIZE:
            break
        skip += PAGE_SIZE
        print(f"  Goldsky fetched {len(positions)}...", file=sys.stderr)
    elapsed = now() - t0
    return positions, elapsed

# ============================================================
# Compute PnL
# ============================================================
def compute_pnl(positions):
    realized = Decimal("0")
    holdings = Decimal("0")
    for p in positions:
        realized += p["realized_pnl"]
        holdings += p["amount"] * p["avg_price"]
    return {"realized_pnl": realized, "holdings_cost": holdings,
            "net_equity": realized + holdings, "count": len(positions)}

# ============================================================
# MAIN
# ============================================================
print(f"Wallet: {WALLET}")
print(f"{'─'*80}")

sources = [
    ("Local CH",   fetch_local_ch),
    ("Remote CH",  fetch_remote_ch),
    ("Goldsky",    fetch_goldsky),
]

results = {}
for name, fetcher in sources:
    try:
        positions, elapsed = fetcher(WALLET)
        pnl = compute_pnl(positions)
        results[name] = {"pnl": pnl, "positions": positions, "elapsed": elapsed}
        print(f"  {name:12s}  {pnl['count']:>4d} pos  |  Realized: ${pnl['realized_pnl']:>12,.2f}  "
              f"Holdings: ${pnl['holdings_cost']:>12,.2f}  |  Net: ${pnl['net_equity']:>12,.2f}  "
              f"({elapsed:.1f}s)")
    except Exception as e:
        print(f"  {name:12s}  ERROR: {e}")
        results[name] = None

# --- Collect all token_ids ---
all_tokens = {}
for src_name, res in results.items():
    if not res: continue
    for p in res["positions"]:
        tid = p["token_id"]
        all_tokens.setdefault(tid, {})[src_name] = p

if not all_tokens:
    print("\nNo positions found in any source.")
    sys.exit(0)

# --- Side-by-side table ---
active_sources = [s for s, r in results.items() if r]
print(f"\n{'─'*130}")
print("Position Detail")
print(f"{'Token ID':>68s}  ", end="")
for src in active_sources:
    print(f"{'Amount':>9s} {'AvgPr':>7s} {'RealPnL':>9s} ({src:10s})  ", end="")
print()

for tid in sorted(all_tokens.keys()):
    # Short display: first 10 + last 8 chars of hex token ID
    short_tid = tid[:12] + ".." + tid[-8:] if len(tid) > 22 else tid
    print(f"{short_tid:>68s}  ", end="")
    for src in active_sources:
        p = all_tokens[tid].get(src)
        if p:
            print(f"{float(p['amount']):>9.4f} {float(p['avg_price']):>7.4f} {float(p['realized_pnl']):>9.2f}  ", end="")
        else:
            print(f"{'─':>9s} {'─':>7s} {'─':>9s}  ", end="")
    print()

# --- Summary ---
print(f"\n{'─'*80}")
print(f"{'Source':>12s}  {'Count':>5s}  {'Realized':>14s}  {'Holdings':>14s}  {'Net':>14s}")
for src in active_sources:
    pnl = results[src]["pnl"]
    print(f"{src:>12s}  {pnl['count']:>5d}  ${pnl['realized_pnl']:>13,.2f}  ${pnl['holdings_cost']:>13,.2f}  ${pnl['net_equity']:>13,.2f}")

# --- Save JSON ---
(out_dir := project_root / "tmp").mkdir(exist_ok=True)
out = {"wallet": WALLET, "sources": {}}
for src, res in results.items():
    if res:
        out["sources"][src] = {
            "pnl": {k: float(v) for k, v in res["pnl"].items()},
            "elapsed": res["elapsed"],
            "positions": [{k: str(v) if isinstance(v, Decimal) else v
                          for k, v in p.items()} for p in res["positions"]],
        }
with open(out_dir / f"compare_{WALLET[:10]}.json", "w") as f:
    json.dump(out, f, indent=2, default=str)
print(f"\nSaved: tmp/compare_{WALLET[:10]}.json")
