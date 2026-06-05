#!/usr/bin/env python3
"""
Download SQD JSONL for wallet 0xa0932d9aa1ca003376d1237c799efacb302a1198

Queries SQD for narrow block ranges around known event blocks, filters
for wallet-relevant logs, and saves clean JSONL files.

Outputs in ./tests/:
  wallet_0xa0932d9_orderfilled.jsonl   - Exchange OrderFilled blocks (wallet as maker)
  wallet_0xa0932d9_split_merge.jsonl   - CTF PositionSplit + PositionsMerge blocks
  wallet_0xa0932d9_condition_prep.jsonl - ConditionPreparation events for relevant conditionIds
  wallet_0xa0932d9_all.jsonl           - All events combined
"""

import urllib.request
import urllib.error
import json
import os
import sys
import time

# ── Config ──────────────────────────────────────────────────────────
WALLET = "0xa0932d9aa1ca003376d1237c799efacb302a1198"
WALLET_PADDED = "0x" + WALLET[2:].lower().rjust(64, "0")

SQD_URL = "https://portal.sqd.dev/datasets/polygon-mainnet/finalized-stream"
OUT_DIR = os.path.dirname(os.path.abspath(__file__))

# Contract addresses (lowercase for comparison)
EXCHANGE_ADDR = "0x4bfb41d5b3570defd03c39a9a4d8de6bd8b8982e"
NEG_RISK_EXCHANGE_ADDR = "0xc5d563a36ae78145c45a50134d48a1215220f80a"
CTF_ADDR = "0x4d97dcd97ec945f40cf65f87097ace5ea0476045"
NEG_RISK_ADAPTER_ADDR = "0xd91e80cf2e7be2e162c6513ced06f1dd0da35296"

# Event topic0 hashes
TOPIC_ORDER_FILLED = "0xd0a08e8c493f9c94f29311604c9de1b4e8c8d4c06bd0c789af57f2d65bfec0f6"
TOPIC_CTF_CONDITION_PREP = "0xab3760c3bd2bb38b5bcf54dc79802ed67338b4cf29f3054ded67ed24661e4177"
TOPIC_CTF_POSITION_SPLIT = "0x2e6bb91f8cbcda0c93623c54d0403a43514fabc40084ec96b6d5379a74786298"
TOPIC_CTF_POSITIONS_MERGE = "0x6f13ca62553fcc2bcd2372180a43949c1e4cebba603901ede2f4e14f36b282ca"
TOPIC_NRA_POSITION_SPLIT = "0xbbed930dbfb7907ae2d60ddf78345610214f26419a0128df39b6cc3d9e5df9b0"
TOPIC_NRA_POSITIONS_MERGE = "0xba33ac50d8894676597e6e35dc09cff59854708b642cd069d21eb9c7ca072a04"

# Condition IDs involved in this wallet's split/merge events
CONDITION_IDS = {
    "1e7db4f6ca3919aa41887f9701605568da64287e1e1662aa7558a749ec61146c",
    "db8de0428bf3c7726baea7de0dd5a44aaa06ab524130f121069873dca57bac17",
    "4e1a18b45ff57dc880b0198968ad7a33ad14e3692b789560e9be816e44c5ba0c",
    "949bf4f2058f1eb0fb007af262b7ebfaef6421f1d83859179d8b2864736532b4",
    "93be4ea8a93f56dc8fcb16a2dc12cff8d8439051dea1d1b5a1120031ad067719",
    "bc15e75d287a394286ffdcdbf857724c48b59e3b6e3da5d687baa4e0e782027d",
}
CONDITION_ID_TOPICS = {f"0x{cid.rjust(64, '0')}" for cid in CONDITION_IDS}

# Known block ranges grouped for efficiency
# Exchange OrderFilled blocks
EXCHANGE_BLOCKS = [
    36763237, 36794810, 36794899, 36794928, 36795336,
    36845299, 36845304, 36845464, 36871755,
    36911751, 36911891, 36912001, 36991710,
]
# CTF Split/Merge blocks
SPLIT_MERGE_BLOCKS = [
    36346802, 36347465, 36350521, 36351006, 36351077,
    36351099, 36351124, 36460737, 36464166,
    36843694,  # PositionSplit
]
# ConditionPreparation blocks for relevant conditionIds
COND_PREP_BLOCKS = [
    36179699, 36302074, 36302209, 36302454, 36341661, 36835747,
]


def group_blocks(blocks, max_gap=5):
    """Group consecutive block numbers into (from, to) ranges."""
    if not blocks:
        return []
    sorted_blocks = sorted(set(blocks))
    ranges = []
    start = sorted_blocks[0]
    prev = start
    for b in sorted_blocks[1:]:
        if b - prev > max_gap:
            ranges.append((start, prev))
            start = b
        prev = b
    ranges.append((start, prev))
    return ranges


def merge_ranges(ranges, max_gap=500):
    """Merge overlapping or close ranges."""
    if not ranges:
        return []
    sorted_ranges = sorted(ranges)
    merged = [sorted_ranges[0]]
    for r in sorted_ranges[1:]:
        last = merged[-1]
        if r[0] - last[1] <= max_gap:
            merged[-1] = (last[0], max(last[1], r[1]))
        else:
            merged.append(r)
    return merged


def fetch_sqd(from_block, to_block):
    """Fetch JSONL from SQD for a block range. Returns list of block dicts."""
    # Build filters for all event types we care about
    log_filters = [
        {"address": [EXCHANGE_ADDR], "topic0": [TOPIC_ORDER_FILLED]},
        {"address": [NEG_RISK_EXCHANGE_ADDR], "topic0": [TOPIC_ORDER_FILLED]},
        {"address": [CTF_ADDR], "topic0": [TOPIC_CTF_POSITION_SPLIT, TOPIC_CTF_POSITIONS_MERGE]},
        {"address": [CTF_ADDR], "topic0": [TOPIC_CTF_CONDITION_PREP]},
        {"address": [NEG_RISK_ADAPTER_ADDR], "topic0": [TOPIC_NRA_POSITION_SPLIT, TOPIC_NRA_POSITIONS_MERGE]},
    ]

    query = {
        "type": "evm",
        "fromBlock": from_block,
        "toBlock": to_block,
        "includeAllBlocks": False,
        "logs": log_filters,
        "fields": {
            "block": {"number": True, "timestamp": True, "hash": True},
            "log": {
                "address": True, "topics": True, "data": True,
                "transactionIndex": True, "logIndex": True, "transactionHash": True,
            },
        },
    }

    body = json.dumps(query).encode()
    req = urllib.request.Request(
        SQD_URL, data=body,
        headers={
            "Content-Type": "application/json",
            "User-Agent": "sqd-go/1.0",
        },
    )

    try:
        with urllib.request.urlopen(req, timeout=300) as resp:
            raw = resp.read()
            blocks = []
            for line in raw.decode().strip().split("\n"):
                if line.strip():
                    blocks.append(json.loads(line))
            return blocks
    except urllib.error.HTTPError as e:
        if e.code == 204:
            return []
        print(f"    SQD HTTP {e.code} at {from_block}-{to_block}", file=sys.stderr)
        return []
    except Exception as e:
        print(f"    SQD error at {from_block}-{to_block}: {e}", file=sys.stderr)
        return []


def filter_block(block):
    """
    Filter a block's logs for wallet-relevant entries.
    Returns (orderfilled_logs, splitmerge_logs, condprep_logs).
    Each is a list of log entries.
    """
    of_logs = []
    sm_logs = []
    cp_logs = []

    for log_entry in block.get("logs", []):
        addr = log_entry.get("address", "").lower()
        topics = log_entry.get("topics", [])
        topic0 = topics[0].lower() if topics else ""

        # Exchange OrderFilled — check topic2 (maker)
        if addr in (EXCHANGE_ADDR, NEG_RISK_EXCHANGE_ADDR) and topic0 == TOPIC_ORDER_FILLED.lower():
            if len(topics) >= 3 and topics[2].lower() == WALLET_PADDED.lower():
                of_logs.append(log_entry)

        # CTF PositionSplit/PositionsMerge — check topic1 (stakeholder)
        elif addr == CTF_ADDR and topic0 in (
            TOPIC_CTF_POSITION_SPLIT.lower(), TOPIC_CTF_POSITIONS_MERGE.lower()
        ):
            if len(topics) >= 2 and topics[1].lower() == WALLET_PADDED.lower():
                sm_logs.append(log_entry)

        # NRA PositionSplit/PositionsMerge — check topic1 (stakeholder)
        elif addr == NEG_RISK_ADAPTER_ADDR and topic0 in (
            TOPIC_NRA_POSITION_SPLIT.lower(), TOPIC_NRA_POSITIONS_MERGE.lower()
        ):
            if len(topics) >= 2 and topics[1].lower() == WALLET_PADDED.lower():
                sm_logs.append(log_entry)

        # CTF ConditionPreparation — check topic1 (conditionId)
        elif addr == CTF_ADDR and topic0 == TOPIC_CTF_CONDITION_PREP.lower():
            if len(topics) >= 2 and topics[1].lower() in CONDITION_ID_TOPICS:
                cp_logs.append(log_entry)

    return of_logs, sm_logs, cp_logs


def save_jsonl(blocks, filename):
    path = os.path.join(OUT_DIR, filename)
    with open(path, "w") as f:
        for block in blocks:
            f.write(json.dumps(block) + "\n")
    size_mb = os.path.getsize(path) / (1024 * 1024)
    print(f"  -> {filename}: {len(blocks)} blocks, {size_mb:.3f} MB")


def main():
    print(f"Wallet: {WALLET}")
    print(f"Output: {OUT_DIR}")
    print()

    # ── Build query ranges ───────────────────────────────────────
    all_blocks = sorted(set(EXCHANGE_BLOCKS + SPLIT_MERGE_BLOCKS + COND_PREP_BLOCKS))
    ranges = group_blocks(all_blocks, max_gap=5)
    ranges = merge_ranges(ranges, max_gap=500)

    print(f"Query ranges: {len(ranges)} total")
    for fr, to in ranges:
        n = to - fr + 1
        print(f"  {fr}-{to} ({n} blocks)")

    print()

    # ── Fetch and filter ─────────────────────────────────────────
    orderfilled_blocks = []
    splitmerge_blocks = []
    condprep_blocks = []
    all_blocks = []
    seen_blocks = {}

    total_blocks_fetched = 0
    for i, (fr, to) in enumerate(ranges):
        print(f"[{i+1}/{len(ranges)}] Fetching {fr}-{to}...", end=" ", flush=True)
        t0 = time.time()

        blocks = fetch_sqd(fr, to)
        elapsed = time.time() - t0
        total_blocks_fetched += len(blocks)
        print(f"{len(blocks)} blocks ({elapsed:.1f}s)")

        for block in blocks:
            bn = block["header"]["number"]
            of_logs, sm_logs, cp_logs = filter_block(block)

            if of_logs:
                ob = {"header": block["header"], "logs": of_logs}
                orderfilled_blocks.append(ob)
                if bn not in seen_blocks:
                    seen_blocks[bn] = {}
                seen_blocks[bn]["of"] = of_logs

            if sm_logs:
                sb = {"header": block["header"], "logs": sm_logs}
                splitmerge_blocks.append(sb)
                if bn not in seen_blocks:
                    seen_blocks[bn] = {}
                seen_blocks[bn]["sm"] = sm_logs

            if cp_logs:
                cb = {"header": block["header"], "logs": cp_logs}
                condprep_blocks.append(cb)
                if bn not in seen_blocks:
                    seen_blocks[bn] = {}
                seen_blocks[bn]["cp"] = cp_logs

        # Rate limit
        time.sleep(0.5)

    # ── Build combined all_blocks ────────────────────────────────
    for bn in sorted(seen_blocks.keys()):
        entries = seen_blocks[bn]
        all_logs = []
        all_logs.extend(entries.get("of", []))
        all_logs.extend(entries.get("sm", []))
        all_logs.extend(entries.get("cp", []))
        # Need the header from one of the source blocks
        header = None
        for blist in [orderfilled_blocks, splitmerge_blocks, condprep_blocks]:
            for b in blist:
                if b["header"]["number"] == bn:
                    header = b["header"]
                    break
            if header:
                break
        if header:
            all_blocks.append({"header": header, "logs": all_logs})

    # ── Print summary ────────────────────────────────────────────
    print()
    print("=" * 60)
    print(f"Total blocks fetched from SQD: {total_blocks_fetched}")
    print(f"Wallet-relevant blocks: {len(seen_blocks)}")
    print()
    print(f"Exchange OrderFilled: {len(orderfilled_blocks)} blocks")
    print(f"CTF Split/Merge:      {len(splitmerge_blocks)} blocks")
    print(f"ConditionPreparation: {len(condprep_blocks)} blocks")
    print()

    # ── Save ─────────────────────────────────────────────────────
    save_jsonl(orderfilled_blocks, "wallet_0xa0932d9_orderfilled.jsonl")
    save_jsonl(splitmerge_blocks, "wallet_0xa0932d9_split_merge.jsonl")
    save_jsonl(condprep_blocks, "wallet_0xa0932d9_condition_prep.jsonl")
    save_jsonl(all_blocks, "wallet_0xa0932d9_all.jsonl")

    # ── Also create a README ─────────────────────────────────────
    readme_path = os.path.join(OUT_DIR, "wallet_0xa0932d9_README.md")
    with open(readme_path, "w") as f:
        f.write(f"""# Wallet 0xa0932d9aa1ca003376d1237c799efacb302a1198 — SQD JSONL Data

Wallet: `{WALLET}`
First Active Block: 36,351,006
Last Active Block: 36,991,710

## Event Summary

| Event Type | Count |
|---|---|
| Exchange OrderFilled (maker) | {len(orderfilled_blocks)} |
| CTF PositionSplit | {len([b for b in splitmerge_blocks if any(l['topics'][0].lower() == TOPIC_CTF_POSITION_SPLIT.lower() for l in b['logs'])])} |
| CTF PositionsMerge | {len([b for b in splitmerge_blocks if any(l['topics'][0].lower() == TOPIC_CTF_POSITIONS_MERGE.lower() for l in b['logs'])])} |
| ConditionPreparation | {len(condprep_blocks)} |

## Files

- `wallet_0xa0932d9_orderfilled.jsonl` — Exchange OrderFilled events where wallet is maker
- `wallet_0xa0932d9_split_merge.jsonl` — CTF PositionSplit + PositionsMerge events  
- `wallet_0xa0932d9_condition_prep.jsonl` — ConditionPreparation events for relevant conditionIds
- `wallet_0xa0932d9_all.jsonl` — All events combined (single block = merged events from all categories)

## Format

Each line is a JSON object: `{{"header": {{"number", "hash", "timestamp"}}, "logs": [...]}}`

Log entries include: `address`, `topics[]`, `data`, `transactionHash`, `transactionIndex`, `logIndex`
""")
    print(f"  -> wallet_0xa0932d9_README.md")

    print()
    print("Done!")


if __name__ == "__main__":
    main()
