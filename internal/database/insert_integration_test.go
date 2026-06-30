package database

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/internal/parser"
)

// --- NewInserter / InsertBlock / InsertBlocks -------------------------------

// TestInsertBlockSingleRow verifies (*Store).NewInserter + (*Inserter).InsertBlock
// writes exactly one row with the expected chain_id/block_number/timestamp/hash.
// FlushAsyncInserts is required before the row becomes visible: verified live,
// async_insert rows are not queryable immediately after Do() returns (see
// TestFlushAsyncInsertsMakesRowsVisible).
func TestInsertBlockSingleRow(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.EnsureTables(ctx); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}

	in := store.NewInserter()
	ts := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := in.InsertBlock(ctx, 7, 12345, ts, "0xdeadbeef"); err != nil {
		t.Fatalf("InsertBlock: %v", err)
	}
	if err := store.FlushAsyncInserts(ctx); err != nil {
		t.Fatalf("FlushAsyncInserts: %v", err)
	}

	var chain, blk proto.ColUInt64
	var bts proto.ColDateTime64
	bts.WithPrecision(proto.Precision(3))
	bts.WithLocation(time.UTC)
	var bhash proto.ColStr
	if err := store.Conn().Do(ctx, ch.Query{
		Body: fmt.Sprintf("SELECT chain_id, block_number, block_timestamp, block_hash FROM %s.blocks", quoteIdent(store.DB())),
		Result: proto.Results{
			{Name: "chain_id", Data: &chain},
			{Name: "block_number", Data: &blk},
			{Name: "block_timestamp", Data: &bts},
			{Name: "block_hash", Data: &bhash},
		},
	}); err != nil {
		t.Fatalf("query blocks: %v", err)
	}

	if chain.Rows() != 1 {
		t.Fatalf("blocks row count = %d, want 1", chain.Rows())
	}
	if chain.Row(0) != 7 {
		t.Errorf("chain_id = %d, want 7", chain.Row(0))
	}
	if blk.Row(0) != 12345 {
		t.Errorf("block_number = %d, want 12345", blk.Row(0))
	}
	if !bts.Row(0).Equal(ts) {
		t.Errorf("block_timestamp = %v, want %v", bts.Row(0), ts)
	}
	if bhash.Row(0) != "0xdeadbeef" {
		t.Errorf("block_hash = %q, want %q", bhash.Row(0), "0xdeadbeef")
	}
}

// TestInsertBlocksMultipleRows verifies InsertBlocks writes several BlockRow
// values across chains in one call, and that an empty slice is a no-op.
func TestInsertBlocksMultipleRows(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.EnsureTables(ctx); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}

	in := store.NewInserter()
	ts := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	rows := []BlockRow{
		{ChainID: 1, BlockNumber: 100, BlockTimestamp: ts, BlockHash: "0xa1"},
		{ChainID: 1, BlockNumber: 101, BlockTimestamp: ts.Add(time.Second), BlockHash: "0xa2"},
		{ChainID: 2, BlockNumber: 100, BlockTimestamp: ts.Add(2 * time.Second), BlockHash: "0xb1"},
	}
	if err := in.InsertBlocks(ctx, rows); err != nil {
		t.Fatalf("InsertBlocks: %v", err)
	}

	// InsertBlocks with an empty slice must be a no-op (verified against source:
	// it returns nil immediately when len(blocks)==0, issuing no query at all).
	if err := in.InsertBlocks(ctx, nil); err != nil {
		t.Fatalf("InsertBlocks(nil): %v", err)
	}

	if err := store.FlushAsyncInserts(ctx); err != nil {
		t.Fatalf("FlushAsyncInserts: %v", err)
	}

	var chain, blk proto.ColUInt64
	var bhash proto.ColStr
	if err := store.Conn().Do(ctx, ch.Query{
		Body: fmt.Sprintf("SELECT chain_id, block_number, block_hash FROM %s.blocks ORDER BY chain_id, block_number", quoteIdent(store.DB())),
		Result: proto.Results{
			{Name: "chain_id", Data: &chain},
			{Name: "block_number", Data: &blk},
			{Name: "block_hash", Data: &bhash},
		},
	}); err != nil {
		t.Fatalf("query blocks: %v", err)
	}

	if chain.Rows() != 3 {
		t.Fatalf("blocks row count = %d, want 3", chain.Rows())
	}
	wantChain := []uint64{1, 1, 2}
	wantBlock := []uint64{100, 101, 100}
	wantHash := []string{"0xa1", "0xa2", "0xb1"}
	for i := range wantChain {
		if chain.Row(i) != wantChain[i] {
			t.Errorf("row %d chain_id = %d, want %d", i, chain.Row(i), wantChain[i])
		}
		if blk.Row(i) != wantBlock[i] {
			t.Errorf("row %d block_number = %d, want %d", i, blk.Row(i), wantBlock[i])
		}
		if bhash.Row(i) != wantHash[i] {
			t.Errorf("row %d block_hash = %q, want %q", i, bhash.Row(i), wantHash[i])
		}
	}
}

// --- InsertLogs --------------------------------------------------------------

// TestInsertLogsWritesDecodedEvent verifies InsertLogs writes a parser.DecodedEvent
// into the logs table with the correct topic0/address/event_name/params, and that
// transaction_hash/topic0/address round-trip as their raw fixed-size byte forms
// (verified live: querying these FixedString columns back yields the raw bytes of
// the hex strings supplied, matching common.HexToHash/abiunpack.AddressFromHex).
func TestInsertLogsWritesDecodedEvent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.EnsureTablesWithCollapsingAndOmit(ctx, false, true); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}

	in := store.NewInserter()
	ts := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
	addr := "0x1111111111111111111111111111111111111111"
	txHash := "0x" + fmt.Sprintf("%064x", 2)
	topic0 := "0x" + fmt.Sprintf("%064x", 3)
	fromAddr := common.HexToAddress("0x2222222222222222222222222222222222222222")
	toAddr := common.HexToAddress("0x3333333333333333333333333333333333333333")

	ev := parser.DecodedEvent{
		ChainID:        1,
		BlockNumber:    100,
		BlockTimestamp: ts,
		BlockHash:      "0xblockhash",
		TxHash:         txHash,
		TxIndex:        5,
		LogIndex:       7,
		Address:        addr,
		EventName:      "Transfer",
		Topic0:         topic0,
		Params: map[string]any{
			"from":  fromAddr,
			"to":    toAddr,
			"value": "12345",
		},
	}
	if err := in.InsertLogs(ctx, []parser.DecodedEvent{ev}); err != nil {
		t.Fatalf("InsertLogs: %v", err)
	}

	// Empty slice must be a no-op (verified against source: returns nil
	// immediately when len(events)==0).
	if err := in.InsertLogs(ctx, nil); err != nil {
		t.Fatalf("InsertLogs(nil): %v", err)
	}

	if err := store.FlushAsyncInserts(ctx); err != nil {
		t.Fatalf("FlushAsyncInserts: %v", err)
	}

	var chain, blk, txidx, logidx proto.ColUInt64
	var addrCol, txhashCol, topic0Col proto.ColFixedStr
	addrCol.SetSize(20)
	txhashCol.SetSize(32)
	topic0Col.SetSize(32)
	var params proto.ColStr
	// event_name is LowCardinality(String) in the schema (see
	// createLogsTable template); a plain proto.ColStr destination fails to
	// decode it ("unexpected type LowCardinality(String)"), verified live.
	name := proto.NewLowCardinality[string](new(proto.ColStr))

	if err := store.Conn().Do(ctx, ch.Query{
		Body: fmt.Sprintf(
			"SELECT chain_id, block_number, transaction_index, log_index, address, transaction_hash, topic0, event_name, params FROM %s.logs",
			quoteIdent(store.DB()),
		),
		Result: proto.Results{
			{Name: "chain_id", Data: &chain},
			{Name: "block_number", Data: &blk},
			{Name: "transaction_index", Data: &txidx},
			{Name: "log_index", Data: &logidx},
			{Name: "address", Data: &addrCol},
			{Name: "transaction_hash", Data: &txhashCol},
			{Name: "topic0", Data: &topic0Col},
			{Name: "event_name", Data: name},
			{Name: "params", Data: &params},
		},
	}); err != nil {
		t.Fatalf("query logs: %v", err)
	}

	if chain.Rows() != 1 {
		t.Fatalf("logs row count = %d, want 1", chain.Rows())
	}
	if chain.Row(0) != 1 {
		t.Errorf("chain_id = %d, want 1", chain.Row(0))
	}
	if blk.Row(0) != 100 {
		t.Errorf("block_number = %d, want 100", blk.Row(0))
	}
	if txidx.Row(0) != 5 {
		t.Errorf("transaction_index = %d, want 5", txidx.Row(0))
	}
	if logidx.Row(0) != 7 {
		t.Errorf("log_index = %d, want 7", logidx.Row(0))
	}
	wantAddr := common.HexToAddress(addr).Bytes()
	if string(addrCol.Row(0)) != string(wantAddr) {
		t.Errorf("address = %x, want %x", addrCol.Row(0), wantAddr)
	}
	wantTxHash := common.HexToHash(txHash).Bytes()
	if string(txhashCol.Row(0)) != string(wantTxHash) {
		t.Errorf("transaction_hash = %x, want %x", txhashCol.Row(0), wantTxHash)
	}
	wantTopic0 := common.HexToHash(topic0).Bytes()
	if string(topic0Col.Row(0)) != string(wantTopic0) {
		t.Errorf("topic0 = %x, want %x", topic0Col.Row(0), wantTopic0)
	}
	if name.Row(0) != "Transfer" {
		t.Errorf("event_name = %q, want %q", name.Row(0), "Transfer")
	}
	wantParams := `{"from":"0x2222222222222222222222222222222222222222","to":"0x3333333333333333333333333333333333333333","value":"12345"}`
	if params.Row(0) != wantParams {
		t.Errorf("params = %q, want %q", params.Row(0), wantParams)
	}
}

// --- NewTypedInserter / TypedInserter.Insert ----------------------------------

// TestTypedInserterInsertsRealEventTable verifies (*Store).NewTypedInserter +
// (*TypedInserter).Insert against a realistic ABI-event-derived table (an ERC20
// Transfer-shaped event: address from, address to, uint256 value), built the
// same way internal/ingestion's buildTypedTableIndex/parseEventArgs construct a
// database.TypedEventTable from a parsed event signature. The CREATE TABLE
// mirrors the sql/createEventTable template (see
// internal/template/templates/sql/clickhouse.go.tmpl) with all four optional
// metadata columns included, so NewTypedInserter's live system.columns probe
// picks up chain_id/block_hash/contract_address/transaction_hash.
func TestTypedInserterInsertsRealEventTable(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tableSQL := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s (
		chain_id UInt64,
		block_number UInt64, block_timestamp DateTime64(3, 'UTC'),
		block_hash FixedString(32),
		contract_address FixedString(20),
		transaction_hash FixedString(32),
		transaction_index UInt64, log_index UInt64
		, %s FixedString(20), %s FixedString(20), %s UInt256
	) ENGINE = MergeTree()
	ORDER BY (block_number, transaction_index, log_index)`,
		quoteIdent(store.DB()), quoteIdent("transfer_events"),
		quoteIdent("from"), quoteIdent("to"), quoteIdent("value"))
	if err := store.Conn().Do(ctx, ch.Query{Body: tableSQL}); err != nil {
		t.Fatalf("create typed event table: %v", err)
	}

	table := TypedEventTable{
		Name: "transfer_events",
		Args: []TypedEventArg{
			{Name: "from", ColumnName: "from", SolidityType: "address", ClickHouseType: "FixedString(20)"},
			{Name: "to", ColumnName: "to", SolidityType: "address", ClickHouseType: "FixedString(20)"},
			{Name: "value", ColumnName: "value", SolidityType: "uint256", ClickHouseType: "UInt256"},
		},
	}
	in := store.NewTypedInserter(table)

	ts := time.Date(2024, 7, 8, 9, 10, 11, 0, time.UTC)
	blockHash := "0x" + fmt.Sprintf("%064x", 9)
	contractAddr := "0x4444444444444444444444444444444444444444"
	txHash := "0x" + fmt.Sprintf("%064x", 10)
	fromAddr := common.HexToAddress("0x5555555555555555555555555555555555555555")
	toAddr := common.HexToAddress("0x6666666666666666666666666666666666666666")

	ev := parser.DecodedEvent{
		ChainID:        3,
		BlockNumber:    200,
		BlockTimestamp: ts,
		BlockHash:      blockHash,
		TxHash:         txHash,
		TxIndex:        3,
		LogIndex:       4,
		Address:        contractAddr,
		EventName:      "Transfer",
		Topic0:         "0x" + fmt.Sprintf("%064x", 11),
		Params: map[string]any{
			"from":  fromAddr,
			"to":    toAddr,
			"value": "999999999999999999999999",
		},
	}
	if err := in.Insert(ctx, []parser.DecodedEvent{ev}); err != nil {
		t.Fatalf("TypedInserter.Insert: %v", err)
	}

	// Empty slice must be a no-op (verified against source: returns nil
	// immediately when len(events)==0).
	if err := in.Insert(ctx, nil); err != nil {
		t.Fatalf("TypedInserter.Insert(nil): %v", err)
	}

	if err := store.FlushAsyncInserts(ctx); err != nil {
		t.Fatalf("FlushAsyncInserts: %v", err)
	}

	var chain, blk, txidx, logidx proto.ColUInt64
	var bhashCol, addrCol, txhashCol, fromCol, toCol proto.ColFixedStr
	bhashCol.SetSize(32)
	addrCol.SetSize(20)
	txhashCol.SetSize(32)
	fromCol.SetSize(20)
	toCol.SetSize(20)
	var value proto.ColUInt256
	if err := store.Conn().Do(ctx, ch.Query{
		Body: fmt.Sprintf(
			"SELECT chain_id, block_number, transaction_index, log_index, block_hash, contract_address, transaction_hash, `from`, `to`, value FROM %s.transfer_events",
			quoteIdent(store.DB()),
		),
		Result: proto.Results{
			{Name: "chain_id", Data: &chain},
			{Name: "block_number", Data: &blk},
			{Name: "transaction_index", Data: &txidx},
			{Name: "log_index", Data: &logidx},
			{Name: "block_hash", Data: &bhashCol},
			{Name: "contract_address", Data: &addrCol},
			{Name: "transaction_hash", Data: &txhashCol},
			{Name: "from", Data: &fromCol},
			{Name: "to", Data: &toCol},
			{Name: "value", Data: &value},
		},
	}); err != nil {
		t.Fatalf("query transfer_events: %v", err)
	}

	if chain.Rows() != 1 {
		t.Fatalf("transfer_events row count = %d, want 1", chain.Rows())
	}
	if chain.Row(0) != 3 {
		t.Errorf("chain_id = %d, want 3", chain.Row(0))
	}
	if blk.Row(0) != 200 {
		t.Errorf("block_number = %d, want 200", blk.Row(0))
	}
	if txidx.Row(0) != 3 {
		t.Errorf("transaction_index = %d, want 3", txidx.Row(0))
	}
	if logidx.Row(0) != 4 {
		t.Errorf("log_index = %d, want 4", logidx.Row(0))
	}
	wantBHash := common.HexToHash(blockHash).Bytes()
	if string(bhashCol.Row(0)) != string(wantBHash) {
		t.Errorf("block_hash = %x, want %x", bhashCol.Row(0), wantBHash)
	}
	wantAddr := common.HexToAddress(contractAddr).Bytes()
	if string(addrCol.Row(0)) != string(wantAddr) {
		t.Errorf("contract_address = %x, want %x", addrCol.Row(0), wantAddr)
	}
	wantTxHash := common.HexToHash(txHash).Bytes()
	if string(txhashCol.Row(0)) != string(wantTxHash) {
		t.Errorf("transaction_hash = %x, want %x", txhashCol.Row(0), wantTxHash)
	}
	if string(fromCol.Row(0)) != string(fromAddr.Bytes()) {
		t.Errorf("from = %x, want %x", fromCol.Row(0), fromAddr.Bytes())
	}
	if string(toCol.Row(0)) != string(toAddr.Bytes()) {
		t.Errorf("to = %x, want %x", toCol.Row(0), toAddr.Bytes())
	}
	gotValue := value.Row(0)
	wantValueLow := proto.UInt128{Low: 2003764205206896639, High: 54210}
	if gotValue.Low != wantValueLow {
		t.Errorf("value.Low = %+v, want %+v (decimal 999999999999999999999999)", gotValue.Low, wantValueLow)
	}
	if gotValue.High != (proto.UInt128{}) {
		t.Errorf("value.High = %+v, want zero", gotValue.High)
	}
}

// --- InsertConn / CommitConn accessors -----------------------------------------

// TestInsertConnAndCommitConnAreDistinctFromConn re-verifies (already covered
// indirectly by TestStoreAccessors in integration_schema_test.go) that
// InsertConn/CommitConn are trivial accessors over distinct *ch.Client values
// from Conn, specifically in the context of a Store that has just performed
// inserts via Conn -- i.e. InsertConn/CommitConn are not silently rebound to
// the same client used by Inserter/TypedInserter (which, per source, always use
// store.conn, not store.insertConn).
func TestInsertConnAndCommitConnAreDistinctFromConn(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.EnsureTables(ctx); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}
	in := store.NewInserter()
	if err := in.InsertBlock(ctx, 1, 1, time.Now().UTC(), "0x1"); err != nil {
		t.Fatalf("InsertBlock: %v", err)
	}

	if store.InsertConn() == nil {
		t.Fatal("InsertConn() is nil")
	}
	if store.CommitConn() == nil {
		t.Fatal("CommitConn() is nil")
	}
	if store.InsertConn() == store.Conn() {
		t.Error("InsertConn() unexpectedly equals Conn()")
	}
	if store.CommitConn() == store.Conn() {
		t.Error("CommitConn() unexpectedly equals Conn()")
	}

	// InsertConn is independently usable for queries (it's a fully separate
	// *ch.Client, just configured with async_insert settings at dial time).
	var cnt proto.ColUInt64
	if err := store.InsertConn().Do(ctx, ch.Query{
		Body:   fmt.Sprintf("SELECT count() AS c FROM %s.blocks", quoteIdent(store.DB())),
		Result: proto.Results{{Name: "c", Data: &cnt}},
	}); err != nil {
		t.Fatalf("query via InsertConn: %v", err)
	}
	if cnt.Rows() != 1 {
		t.Fatalf("InsertConn query result rows = %d, want 1", cnt.Rows())
	}
}

// --- FlushAsyncInserts ---------------------------------------------------------

// TestFlushAsyncInsertsMakesRowsVisible verifies FlushAsyncInserts is not a
// no-op: it actually forces ClickHouse's server-side async-insert buffer to
// flush, making a previously async-inserted row queryable. Verified live: a
// SELECT count() issued immediately after InsertBlock (before any flush)
// reliably returns 0 (the row sits in the server's async_insert queue, not yet
// in a part), and the same query after FlushAsyncInserts returns 1.
func TestFlushAsyncInsertsMakesRowsVisible(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.EnsureTables(ctx); err != nil {
		t.Fatalf("EnsureTables: %v", err)
	}

	in := store.NewInserter()
	if err := in.InsertBlock(ctx, 1, 1, time.Now().UTC(), "0xnoflush"); err != nil {
		t.Fatalf("InsertBlock: %v", err)
	}

	var cnt proto.ColUInt64
	countQuery := fmt.Sprintf("SELECT count() AS c FROM %s.blocks", quoteIdent(store.DB()))
	if err := store.Conn().Do(ctx, ch.Query{Body: countQuery, Result: proto.Results{{Name: "c", Data: &cnt}}}); err != nil {
		t.Fatalf("query before flush: %v", err)
	}
	if cnt.Row(0) != 0 {
		t.Fatalf("count before FlushAsyncInserts = %d, want 0 (async-inserted row should not be visible yet)", cnt.Row(0))
	}

	if err := store.FlushAsyncInserts(ctx); err != nil {
		t.Fatalf("FlushAsyncInserts: %v", err)
	}

	cnt = proto.ColUInt64{}
	if err := store.Conn().Do(ctx, ch.Query{Body: countQuery, Result: proto.Results{{Name: "c", Data: &cnt}}}); err != nil {
		t.Fatalf("query after flush: %v", err)
	}
	if cnt.Row(0) != 1 {
		t.Fatalf("count after FlushAsyncInserts = %d, want 1", cnt.Row(0))
	}
}
