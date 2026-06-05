# Wallet 0xa0932d9aa1ca003376d1237c799efacb302a1198 — SQD JSONL Data

Wallet: `0xa0932d9aa1ca003376d1237c799efacb302a1198`
First Active Block: 36,351,006
Last Active Block: 36,991,710

## Event Summary

| Event Type | Count |
|---|---|
| Exchange OrderFilled (maker) | 13 |
| CTF PositionSplit | 1 |
| CTF PositionsMerge | 9 |
| ConditionPreparation | 6 |

## Files

- `wallet_0xa0932d9_orderfilled.jsonl` — Exchange OrderFilled events where wallet is maker
- `wallet_0xa0932d9_split_merge.jsonl` — CTF PositionSplit + PositionsMerge events  
- `wallet_0xa0932d9_condition_prep.jsonl` — ConditionPreparation events for relevant conditionIds
- `wallet_0xa0932d9_all.jsonl` — All events combined (single block = merged events from all categories)

## Format

Each line is a JSON object: `{"header": {"number", "hash", "timestamp"}, "logs": [...]}`

Log entries include: `address`, `topics[]`, `data`, `transactionHash`, `transactionIndex`, `logIndex`
