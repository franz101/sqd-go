#!/usr/bin/env python3
"""Show output prices for wallet 0xa0932d9 events."""
import json, os

WALLET = "0xa0932d9aa1ca003376d1237c799efacb302a1198".lower()
TEST_DIR = "/home/dev/CODING/polymarket_lowram/sqd-go-v2/tests"
M = 1_000_000

# Load
blocks = []
with open(os.path.join(TEST_DIR, "wallet_0xa0932d9_all.jsonl")) as f:
    for line in f:
        if line.strip():
            blocks.append(json.loads(line))
blocks.sort(key=lambda b: b["header"]["number"]) 

TOPIC_OF = "0xd0a08e8c493f9c94f29311604c9de1b4e8c8d4c06bd0c789af57f2d65bfec0f6"
TOPIC_SPLIT = "0x2e6bb91f8cbcda0c93623c54d0403a43514fabc40084ec96b6d5379a74786298"
TOPIC_MERGE = "0x6f13ca62553fcc2bcd2372180a43949c1e4cebba603901ede2f4e14f36b282ca"

print("=== OrderFilled (wallet as maker) ===")
for b in blocks:
    for log in b.get("logs", []):
        t0 = log["topics"][0].lower() if log["topics"] else ""
        if t0 != TOPIC_OF.lower():
            continue
        data = log["data"]
        maker_asset = int(data[2:66], 16)
        taker_asset = int(data[66:130], 16)
        maker_amt = int(data[130:194], 16)
        taker_amt = int(data[194:258], 16)
        if maker_asset == 0:
            d, tid, base, quote = "BUY",  data[66:130], taker_amt, maker_amt
        else:
            d, tid, base, quote = "SELL", data[2:66], maker_amt, taker_amt
        price = quote / base if base else 0
        print(f"  blk={b['header']['number']} {d} token=0x{tid[:20]}... amt={base/M:,.2f} quote={quote/M:,.2f} price={price:.6f}")

print("\n=== Split/Merge (always at $0.50) ===")
for b in blocks:
    for log in b.get("logs", []):
        t0 = log["topics"][0].lower() if log["topics"] else ""
        if t0 == TOPIC_SPLIT.lower():
            cond = log["topics"][3] if len(log["topics"])>3 else "?"
            # data: collateralToken(32) + offset(32) + len(32) + [1](32) + [2](32) + amount(32)
            # amount is the 6th word: chars 322-386
            amt = int(log["data"][322:386], 16)
            print(f"  blk={b['header']['number']} SPLIT  cond={cond[:24]}... amt={amt/M:,.2f} @ $0.50 per YES/NO")
        elif t0 == TOPIC_MERGE.lower():
            cond = log["topics"][3] if len(log["topics"])>3 else "?"
            amt = int(log["data"][322:386], 16)
            print(f"  blk={b['header']['number']} MERGE  cond={cond[:24]}... amt={amt/M:,.2f} @ $0.50 per YES/NO")
