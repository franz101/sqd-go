MY DESIGN KEEP IT!!!!

STEPS:
JSONL -> PARSE EVENTS (HYPEREFFICIENT):
    -> Allocate as little memory CPU as possible and
    -> Tranform into Ringbuffer
    -> [{blocknumber, Transfers[],Positions[],evtOrderes[uint]},{{blocknumber+1, Transfers[],Positions[],evtOrderes[uint]}}]
CUSTOM PROCESSOR (EXAMPLE PNL CALCULATION):
    -> CLOCK ALGORITHM FOR HOT BUFFER
    -> FULL STATE ON DISK
    -> STATE FOR RECOVERY IN THE DATABASE
FORK DETECTION LIKE IN ./../pipes-sdk
ONFORK:
    -> ROLLBACK STATE FROM DATABASE
    -> DELETE ROWS ABOVE FORK BLOCK DATABASE
    -> REPROCESS BASED ON THE LAST X EVENTS FROM THE BLOCKS STORED ON THE RINGBUFFER.
    -> Store finalized block
    -> prune historic state if below the finalized block
    -> QUERY ORDER BY BLOCK DESC LIMIT BY hash WHERE BLOCK < FINALIZED BLOCK.
    
In general store last finalized block last complete block. For recovery.
On start or crash
Delete everything above last finalized block
Start refetching
Sync hotstate diskstate from db / requests

IMPORTANT:
PARALELL:
    - FETCHING -> DECODING -> RINGBUFFER 
    - PARALLEL -> WAIT FOR NEW SLOT IN RINGBUFFER -> CUSTOM_PROCESSOR -> INGEST


SYNC TABLE:
- Insert Once block is fully parsed
- On crash check for latest sync table entry and LIGHT DELETE EVERYTHING ABOVE.
- CONTINUE FROM THAT BLOCK + 1
- add flag --no-resume if you want to start from the beginning


------ YOU DESIGN BE CAUTIOUS

STEPS:

FETCH -> PARSE JSONL -> DECODE EVENTS:
    -> Keep decoded logs grouped by block.
    -> Insert the typed event batch and block rows once the fetched page is complete.
    -> Pass the final custom log batch to the domain processor after database ingestion.

POLYMARKET CUSTOM PROCESSOR:
    -> Decode custom logs into generated event structs.
    -> Push per-block events into OrderedHistoricRingBuffer.
    -> Reconstruct original log order from the slot Sequence.
    -> Batch-query missing hot state before handlers run:
        - Conditions by condition_id.
        - NegRisk events by market_id.
        - User positions by (user, token_id).
    -> Apply ExchangeMapping.ts parity:
        - makerAssetId == 0 is BUY, takerAssetId is the position.
        - makerAssetId != 0 is SELL, makerAssetId is the position.
        - price = quoteAmount / baseAmount.
        - buy updates weighted avg price, amount, total bought.
        - sell realizes amount * (price - avgPrice), capped to tracked amount.
        - Exchange and NegRiskExchange OrderFilled use the same handler math.
    -> Apply FixedProductMarketMaker parity:
        - Factory creation stores FPMM -> condition_id + collateral_token.
        - FPMM buy/sell/funding events are keyed by emitting contract address.
        - contract_address stays out of stored metadata; the processor tracks it only in-memory by block/tx/log ref.
        - Missing FPMM records, conditions, and positions are resolved in batches before handlers run.
        - Funding add/remove uses the subgraph sendback and LP-share pricing math with decimal prices.
    -> Keep updated entities in ClockCache-backed hot state.
    -> Commit dirty state to ClickHouse in batches.
    -> Print calculated PNL summaries while dev-polymarket runs.

STATE LAYOUT:
    -> Ring buffer stores recent decoded block slots for ordered replay.
    -> ClockCache stores hot Conditions, Positions, Markets, NegRiskEvents, and FixedProductMarketMakers.
    -> ClickHouse stores full durable state in memory_* tables.
    -> Batch resolvers query latest state rows with ORDER BY block_number DESC, transaction_index DESC, log_index DESC LIMIT 1 BY primary key.

FORK DETECTION:
    -> Store current, finalized block, and rollback chain in sync_state.
    -> On fork, rollback database rows above the common cursor.
    -> Restore processor snapshot at or below the rollback block.
    -> Reprocess blocks available in the replay/ring buffers.
    -> Prune unfinalized rollback state once it is below the finalized block.

STARTUP / CRASH:
    -> Read latest sync_state checkpoint.
    -> Delete or sign-flip rows above the selected recovery block.
    -> Continue from recovery block + 1.
    -> Hot state is recovered lazily through batch resolvers as missing keys are touched.
    -> Use --no-resume to drop state and start from the configured block.
