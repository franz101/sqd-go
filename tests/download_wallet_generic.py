#!/usr/bin/env python3
"""
Generic SQD JSONL fetcher for an arbitrary Polymarket wallet.

Uses the cursor pagination pattern from debugger/fetchUntil.go:
  - POST query with fromBlock (no toBlock)
  - response's last block number + 1 becomes the next fromBlock
  - repeat until we pass the wallet's last-active block.

Server-side topic filters keep the wallet-relevant events cheap.
ConditionPreparation / ConditionResolution are fetched unfiltered and
filtered client-side to the condition IDs the wallet actually touched.

Outputs (in ./tests/):
  wallet_<short>_orderfilled.jsonl
  wallet_<short>_split_merge.jsonl
  wallet_<short>_condition_prep.jsonl
  wallet_<short>_redemption.jsonl
  wallet_<short>_all.jsonl
"""
import urllib.request, urllib.error, json, os, sys, time

WALLET = sys.argv[1] if len(sys.argv) > 1 else "0xa79af3bab636f41f1f7bd1c568857dbdf4650beb"
START_BLOCK = int(sys.argv[2]) if len(sys.argv) > 2 else 33000000
LAST_ACTIVE = int(sys.argv[3]) if len(sys.argv) > 3 else 39923960
WALLET = WALLET.lower()
SHORT = WALLET[:9]
WPAD = "0x" + WALLET[2:].rjust(64, "0")

SQD = "https://portal.sqd.dev/datasets/polygon-mainnet/finalized-stream"
OUT = os.path.dirname(os.path.abspath(__file__))

EXCHANGE = "0x4bfb41d5b3570defd03c39a9a4d8de6bd8b8982e"
NEG_RISK_EXCHANGE = "0xc5d563a36ae78145c45a50134d48a1215220f80a"
CTF = "0x4d97dcd97ec945f40cf65f87097ace5ea0476045"
NRA = "0xd91e80cf2e7be2e162c6513ced06f1dd0da35296"

OF = "0xd0a08e8c493f9c94f29311604c9de1b4e8c8d4c06bd0c789af57f2d65bfec0f6"
PREP = "0xab3760c3bd2bb38b5bcf54dc79802ed67338b4cf29f3054ded67ed24661e4177"
PSPLIT = "0x2e6bb91f8cbcda0c93623c54d0403a43514fabc40084ec96b6d5379a74786298"
PMERGE = "0x6f13ca62553fcc2bcd2372180a43949c1e4cebba603901ede2f4e14f36b282ca"
NPSPLIT = "0xbbed930dbfb7907ae2d60ddf78345610214f26419a0128df39b6cc3d9e5df9b0"
NPMERGE = "0xba33ac50d8894676597e6e35dc09cff59854708b642cd069d21eb9c7ca072a04"
RESOLVE = "0xb44d84d3289691f71497564b85d4233648d9dbae8cbdbb4329f301c3a0185894"
# PayoutRedemption(address,address,bytes32,bytes32,uint256[],uint256)
REDEEM = "0x2682012a4a4f1973119f1c9b90745d1bd91fa2bab387344f044cb3586864d18d"

LOG_FILTERS = [
    {"address": [EXCHANGE, NEG_RISK_EXCHANGE], "topic0": [OF], "topic2": [WPAD]},  # maker
    {"address": [EXCHANGE, NEG_RISK_EXCHANGE], "topic0": [OF], "topic3": [WPAD]},  # taker
    {"address": [CTF], "topic0": [PSPLIT, PMERGE], "topic1": [WPAD]},
    {"address": [NRA], "topic0": [NPSPLIT, NPMERGE], "topic1": [WPAD]},
    {"address": [CTF], "topic0": [REDEEM], "topic1": [WPAD]},
    {"address": [CTF], "topic0": [PREP]},     # all condition preps (filter later)
    {"address": [CTF], "topic0": [RESOLVE]},  # all resolutions (filter later)
]
FIELDS = {
    "block": {"number": True, "timestamp": True, "hash": True},
    "log": {"address": True, "topics": True, "data": True,
            "transactionIndex": True, "logIndex": True, "transactionHash": True},
}


def fetch(from_block):
    q = {"type": "evm", "fromBlock": from_block, "includeAllBlocks": False,
         "logs": LOG_FILTERS, "fields": FIELDS}
    req = urllib.request.Request(SQD, data=json.dumps(q).encode(),
                                 headers={"Content-Type": "application/json", "User-Agent": "sqd-go/1.0"})
    for attempt in range(5):
        try:
            with urllib.request.urlopen(req, timeout=120) as r:
                txt = r.read().decode().strip()
            if not txt:
                return []
            return [json.loads(l) for l in txt.split("\n") if l]
        except urllib.error.HTTPError as e:
            if e.code == 204:
                return []
            print(f"  HTTP {e.code} at {from_block}, retry {attempt}", file=sys.stderr)
            time.sleep(2)
        except Exception as e:
            print(f"  err at {from_block}: {e}, retry {attempt}", file=sys.stderr)
            time.sleep(2)
    return []


def main():
    print(f"Wallet {WALLET}  blocks {START_BLOCK}..{LAST_ACTIVE}")
    of_blocks, sm_blocks, redeem_blocks = {}, {}, {}
    prep_by_cond, resolve_by_cond = {}, {}
    touched_conditions = set()

    fb = START_BLOCK
    t0 = time.time()
    reqs = 0
    while fb <= LAST_ACTIVE + 5:
        objs = fetch(fb)
        reqs += 1
        if not objs:
            print("empty response, stopping")
            break
        last = objs[-1]["header"]["number"]
        for b in objs:
            hdr = b["header"]
            bn = hdr["number"]
            for lg in b.get("logs", []):
                addr = lg["address"].lower()
                topics = lg.get("topics", [])
                if not topics:
                    continue
                t0h = topics[0].lower()
                if t0h == OF and addr in (EXCHANGE, NEG_RISK_EXCHANGE):
                    of_blocks.setdefault(bn, {"header": hdr, "logs": []})["logs"].append(lg)
                elif addr == CTF and t0h in (PSPLIT, PMERGE):
                    sm_blocks.setdefault(bn, {"header": hdr, "logs": []})["logs"].append(lg)
                    if len(topics) > 3:
                        touched_conditions.add(topics[3].lower())
                elif addr == NRA and t0h in (NPSPLIT, NPMERGE):
                    sm_blocks.setdefault(bn, {"header": hdr, "logs": []})["logs"].append(lg)
                    if len(topics) > 3:
                        touched_conditions.add(topics[3].lower())
                elif addr == CTF and t0h == REDEEM:
                    redeem_blocks.setdefault(bn, {"header": hdr, "logs": []})["logs"].append(lg)
                    # PayoutRedemption: conditionId is non-indexed, data word 0
                    d = lg.get("data", "")
                    if len(d) >= 66:
                        touched_conditions.add(("0x" + d[2:66]).lower())
                elif addr == CTF and t0h == PREP:
                    cond = topics[1].lower() if len(topics) > 1 else None
                    if cond:
                        prep_by_cond[cond] = {"header": hdr, "log": lg}
                elif addr == CTF and t0h == RESOLVE:
                    cond = topics[1].lower() if len(topics) > 1 else None
                    if cond:
                        resolve_by_cond[cond] = {"header": hdr, "log": lg}
        if reqs % 25 == 0:
            print(f"  block {last} ({100*(last-START_BLOCK)/(LAST_ACTIVE-START_BLOCK):.1f}%) "
                  f"reqs={reqs} of={len(of_blocks)} sm={len(sm_blocks)} "
                  f"redeem={len(redeem_blocks)} conds={len(touched_conditions)} {time.time()-t0:.0f}s",
                  flush=True)
        if last >= LAST_ACTIVE + 5:
            break
        fb = last + 1

    # Build condition_prep / resolution restricted to touched conditions
    prep_blocks, resolve_blocks = {}, {}
    for cond in touched_conditions:
        if cond in prep_by_cond:
            e = prep_by_cond[cond]
            bn = e["header"]["number"]
            prep_blocks.setdefault(bn, {"header": e["header"], "logs": []})["logs"].append(e["log"])
        if cond in resolve_by_cond:
            e = resolve_by_cond[cond]
            bn = e["header"]["number"]
            resolve_blocks.setdefault(bn, {"header": e["header"], "logs": []})["logs"].append(e["log"])

    def save(d, name):
        path = os.path.join(OUT, name)
        rows = [d[k] for k in sorted(d.keys())]
        with open(path, "w") as f:
            for r in rows:
                f.write(json.dumps(r) + "\n")
        n_logs = sum(len(r["logs"]) for r in rows)
        print(f"  -> {name}: {len(rows)} blocks, {n_logs} logs")
        return rows

    print("\n=== Saving ===")
    save(of_blocks, f"wallet_{SHORT}_orderfilled.jsonl")
    save(sm_blocks, f"wallet_{SHORT}_split_merge.jsonl")
    save(prep_blocks, f"wallet_{SHORT}_condition_prep.jsonl")
    save(resolve_blocks, f"wallet_{SHORT}_resolution.jsonl")
    save(redeem_blocks, f"wallet_{SHORT}_redemption.jsonl")

    # Combined all.jsonl: merge logs per block across categories, sorted by logIndex
    all_blocks = {}
    for d in (prep_blocks, resolve_blocks, sm_blocks, of_blocks, redeem_blocks):
        for bn, rec in d.items():
            ab = all_blocks.setdefault(bn, {"header": rec["header"], "logs": []})
            ab["logs"].extend(rec["logs"])
    for bn, rec in all_blocks.items():
        rec["logs"].sort(key=lambda l: l.get("logIndex", 0))
    save(all_blocks, f"wallet_{SHORT}_all.jsonl")

    # Count maker order fills for summary
    of_maker = sum(1 for r in of_blocks.values() for l in r["logs"]
                   if len(l["topics"]) > 2 and l["topics"][2].lower() == WPAD)
    of_taker = sum(1 for r in of_blocks.values() for l in r["logs"]
                   if len(l["topics"]) > 3 and l["topics"][3].lower() == WPAD)
    n_split = sum(1 for r in sm_blocks.values() for l in r["logs"] if l["topics"][0].lower() in (PSPLIT, NPSPLIT))
    n_merge = sum(1 for r in sm_blocks.values() for l in r["logs"] if l["topics"][0].lower() in (PMERGE, NPMERGE))
    print(f"\n=== Summary ===")
    print(f"OrderFilled maker={of_maker} taker={of_taker}")
    print(f"Split={n_split} Merge={n_merge} ConditionPrep={sum(len(r['logs']) for r in prep_blocks.values())} "
          f"Resolution={sum(len(r['logs']) for r in resolve_blocks.values())} "
          f"Redemption={sum(len(r['logs']) for r in redeem_blocks.values())}")
    print(f"Touched conditions: {len(touched_conditions)}")
    print(f"Total requests: {reqs}, time {time.time()-t0:.0f}s")


if __name__ == "__main__":
    main()
