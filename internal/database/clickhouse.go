package database

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/internal/parser"
	"github.com/holiman/uint256"
)

// Store wraps a ClickHouse native-protocol connection and the target database name.
type Store struct {
	conn *ch.Client
	db   string
}

// BlockRow is a single row in the blocks table, used during fork tracking.
type BlockRow struct {
	ChainID        uint64
	BlockNumber    uint64
	BlockTimestamp time.Time
	BlockHash      string
}

// TypedEventTable describes a ClickHouse table generated from an ABI event.
type TypedEventTable struct {
	Name string
	Args []TypedEventArg
}

// TypedEventArg maps one ABI event parameter to its ClickHouse column.
type TypedEventArg struct {
	Name           string
	ColumnName     string
	SolidityType   string
	ClickHouseType string
}

// SyncCursor records a block number and hash for checkpoint persistence.
type SyncCursor struct {
	Number uint64 `json:"number"`
	Hash   string `json:"hash"`
}

// SyncState is the persisted ingestion checkpoint: current head, finalized
// block, and any rollback chain from fork recovery.
type SyncState struct {
	Current       SyncCursor
	Finalized     *SyncCursor
	RollbackChain []SyncCursor
}

// EnsureTablesOptions controls which optional tables are created during schema setup.
type EnsureTablesOptions struct {
	StoreBlocks bool
	StoreLogs   bool
}

// NewClickHouse connects to ClickHouse via the native protocol, creates the
// target database if it doesn't exist, and returns a Store.
func NewClickHouse(ctx context.Context, host string, port int, user, password, db string) (*Store, error) {
	conn, err := ch.Dial(ctx, ch.Options{
		Address:  fmt.Sprintf("%s:%d", host, port),
		Database: "default",
		User:     user,
		Password: password,
	})
	if err != nil {
		return nil, fmt.Errorf("connect clickhouse: %w", err)
	}
	if err := conn.Do(ctx, ch.Query{Body: fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", quoteIdent(db))}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("create database %s: %w", db, err)
	}
	return &Store{conn: conn, db: db}, nil
}

// DropClickHouseDatabase drops the named database (used by --restart).
func DropClickHouseDatabase(ctx context.Context, host string, port int, user, password, db string) error {
	conn, err := ch.Dial(ctx, ch.Options{
		Address:  fmt.Sprintf("%s:%d", host, port),
		Database: "default",
		User:     user,
		Password: password,
	})
	if err != nil {
		return fmt.Errorf("connect clickhouse: %w", err)
	}
	defer conn.Close()
	return conn.Do(ctx, ch.Query{Body: fmt.Sprintf("DROP DATABASE IF EXISTS %s", quoteIdent(db))})
}

func (s *Store) Close() error {
	return s.conn.Close()
}

func (s *Store) Conn() *ch.Client {
	if s == nil {
		return nil
	}
	return s.conn
}

func (s *Store) DB() string {
	if s == nil {
		return ""
	}
	return s.db
}

func (s *Store) EnsureTables(ctx context.Context) error {
	return s.EnsureTablesWithCollapsing(ctx, false)
}

func (s *Store) EnsureTablesWithCollapsing(ctx context.Context, collapsing bool) error {
	return s.EnsureTablesWithCollapsingAndOmit(ctx, collapsing, false)
}

func (s *Store) EnsureTablesWithCollapsingAndOmit(ctx context.Context, collapsing bool, storeLogs bool) error {
	return s.EnsureTablesWithOptions(ctx, collapsing, EnsureTablesOptions{StoreBlocks: true, StoreLogs: storeLogs})
}

func (s *Store) EnsureTablesWithOptions(ctx context.Context, collapsing bool, opts EnsureTablesOptions) error {
	db := quoteIdent(s.db)
	engine := "MergeTree()"
	signColumn := ""
	if collapsing {
		engine = "CollapsingMergeTree(sign)"
		signColumn = "sign Int8 DEFAULT 1,"
	}
	if opts.StoreBlocks {
		blocksDDL := fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s.blocks (
			chain_id UInt64, block_number UInt64,
			block_timestamp DateTime64(3, 'UTC'), block_hash String,
			%s
			inserted_at DateTime64(3, 'UTC') DEFAULT now64(3)
		) ENGINE = %s ORDER BY (chain_id, block_number)`, db, signColumn, engine)
		if err := s.conn.Do(ctx, ch.Query{Body: blocksDDL}); err != nil {
			return fmt.Errorf("create blocks: %w", err)
		}
	}
	if opts.StoreLogs {
		logsDDL := fmt.Sprintf(`
			CREATE TABLE IF NOT EXISTS %s.logs (
				chain_id UInt64, block_number UInt64,
				block_timestamp DateTime64(3, 'UTC'), block_hash String,
				transaction_hash FixedString(32), transaction_index UInt64, log_index UInt64,
				address FixedString(20), event_name LowCardinality(String),
				topic0 FixedString(32), params String,
				%s
				inserted_at DateTime64(3, 'UTC') DEFAULT now64(3)
			) ENGINE = %s ORDER BY (chain_id, block_number, transaction_index, log_index)`, db, signColumn, engine)
		if err := s.conn.Do(ctx, ch.Query{Body: logsDDL}); err != nil {
			return fmt.Errorf("create logs: %w", err)
		}
	}
	stateDDL := fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s.sync_state (
			chain_id UInt64, last_block UInt64, last_hash String,
			finalized_block UInt64, finalized_hash String,
			rollback_chain String,
			updated_at DateTime64(3, 'UTC') DEFAULT now64(3)
		) ENGINE = MergeTree() ORDER BY (chain_id, updated_at)`, db)
	if err := s.conn.Do(ctx, ch.Query{Body: stateDDL}); err != nil {
		return fmt.Errorf("create sync_state: %w", err)
	}
	return nil
}

func (s *Store) ApplySQLFile(ctx context.Context, path string) error {
	return s.applySQLFile(ctx, path, "")
}

func (s *Store) ApplySQLFileWithDatabase(ctx context.Context, path, sourceDB string) error {
	return s.applySQLFile(ctx, path, sourceDB)
}

func (s *Store) applySQLFile(ctx context.Context, path, sourceDB string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sql := rewriteSQLDatabase(string(raw), sourceDB, s.db)
	for _, stmt := range splitSQLStatements(sql) {
		if err := s.conn.Do(ctx, ch.Query{Body: stmt}); err != nil {
			return fmt.Errorf("%s: %w", firstSQLLine(stmt), err)
		}
	}
	return nil
}

func rewriteSQLDatabase(sql, sourceDB, targetDB string) string {
	if sourceDB == "" || targetDB == "" || sourceDB == targetDB {
		return sql
	}
	return strings.ReplaceAll(sql, quoteIdent(sourceDB), quoteIdent(targetDB))
}

// Inserter batches block-level rows (blocks + raw logs) for native-protocol insertion.
type Inserter struct {
	store *Store

	blockChain proto.ColUInt64
	blockNum   proto.ColUInt64
	blockTime  proto.ColDateTime64
	blockHash  proto.ColStr

	colChain  proto.ColUInt64
	colBlock  proto.ColUInt64
	colTime   proto.ColDateTime64
	colBHash  proto.ColStr
	colTxHash proto.ColFixedStr
	colTxIdx  proto.ColUInt64
	colLogIdx proto.ColUInt64
	colAddr   proto.ColFixedStr
	colName   proto.ColStr
	colTopic0 proto.ColFixedStr
	colParams proto.ColStr
}

func (s *Store) NewInserter() *Inserter {
	in := &Inserter{store: s}
	in.blockTime.WithPrecision(proto.Precision(3))
	in.blockTime.WithLocation(time.UTC)
	in.colTime.WithPrecision(proto.Precision(3))
	in.colTime.WithLocation(time.UTC)
	in.colTxHash.SetSize(32)
	in.colAddr.SetSize(20)
	in.colTopic0.SetSize(32)
	return in
}

func (in *Inserter) InsertBlock(ctx context.Context, chainID, blockNumber uint64, blockTimestamp time.Time, blockHash string) error {
	return in.InsertBlocks(ctx, []BlockRow{{
		ChainID:        chainID,
		BlockNumber:    blockNumber,
		BlockTimestamp: blockTimestamp,
		BlockHash:      blockHash,
	}})
}

func (in *Inserter) InsertBlocks(ctx context.Context, blocks []BlockRow) error {
	if len(blocks) == 0 {
		return nil
	}
	in.blockChain.Reset()
	in.blockNum.Reset()
	in.blockTime.Reset()
	in.blockHash.Reset()

	for _, block := range blocks {
		in.blockChain.Append(block.ChainID)
		in.blockNum.Append(block.BlockNumber)
		in.blockTime.Append(block.BlockTimestamp)
		in.blockHash.Append(block.BlockHash)
	}
	return in.store.conn.Do(ctx, ch.Query{
		Body: fmt.Sprintf("INSERT INTO %s.blocks (chain_id, block_number, block_timestamp, block_hash) VALUES", quoteIdent(in.store.db)),
		Input: []proto.InputColumn{
			{Name: "chain_id", Data: &in.blockChain}, {Name: "block_number", Data: &in.blockNum},
			{Name: "block_timestamp", Data: &in.blockTime}, {Name: "block_hash", Data: &in.blockHash},
		},
		Settings: []ch.Setting{
			{Key: "async_insert", Value: "1", Important: true},
			{Key: "wait_for_async_insert", Value: "0", Important: true},
		},
	})
}

func (in *Inserter) InsertLogs(ctx context.Context, events []parser.DecodedEvent) error {
	if len(events) == 0 {
		return nil
	}

	cols := []proto.InputColumn{
		{Name: "chain_id", Data: &in.colChain}, {Name: "block_number", Data: &in.colBlock},
		{Name: "block_timestamp", Data: &in.colTime}, {Name: "block_hash", Data: &in.colBHash},
		{Name: "transaction_hash", Data: &in.colTxHash}, {Name: "transaction_index", Data: &in.colTxIdx},
		{Name: "log_index", Data: &in.colLogIdx}, {Name: "address", Data: &in.colAddr},
		{Name: "event_name", Data: &in.colName}, {Name: "topic0", Data: &in.colTopic0},
		{Name: "params", Data: &in.colParams},
	}

	total := len(events)
	processed := 0
	chunkSize := 10000

	return in.store.conn.Do(ctx, ch.Query{
		Body:  fmt.Sprintf("INSERT INTO %s.logs (chain_id, block_number, block_timestamp, block_hash, transaction_hash, transaction_index, log_index, address, event_name, topic0, params) VALUES", quoteIdent(in.store.db)),
		Input: cols,
		Settings: []ch.Setting{
			{Key: "async_insert", Value: "1", Important: true},
			{Key: "wait_for_async_insert", Value: "0", Important: true},
		},
		OnInput: func(ctx context.Context) error {
			in.colChain.Reset()
			in.colBlock.Reset()
			in.colTime.Reset()
			in.colBHash.Reset()
			in.colTxHash.Reset()
			in.colTxIdx.Reset()
			in.colLogIdx.Reset()
			in.colAddr.Reset()
			in.colName.Reset()
			in.colTopic0.Reset()
			in.colParams.Reset()

			if processed >= total {
				return io.EOF
			}

			end := processed + chunkSize
			if end > total {
				end = total
			}

			for _, ev := range events[processed:end] {
				paramsJSON, _ := json.Marshal(ev.Params)
				in.colChain.Append(ev.ChainID)
				in.colBlock.Append(ev.BlockNumber)
				in.colTime.Append(ev.BlockTimestamp)
				in.colBHash.Append(ev.BlockHash)
				in.colTxHash.Append(common.HexToHash(ev.TxHash).Bytes())
				in.colTxIdx.Append(ev.TxIndex)
				in.colLogIdx.Append(ev.LogIndex)
				in.colAddr.Append(common.HexToAddress(ev.Address).Bytes())
				in.colName.Append(ev.EventName)
				in.colTopic0.Append(common.HexToHash(ev.Topic0).Bytes())
				in.colParams.Append(string(paramsJSON))
			}
			processed = end
			return nil
		},
	})
}

func (s *Store) NewTypedInserter(table TypedEventTable) *TypedInserter {
	in := &TypedInserter{
		store: s,
		table: table,
	}
	in.colTime.WithPrecision(proto.Precision(3))
	in.colTime.WithLocation(time.UTC)
	in.colBHash.SetSize(32)
	in.colAddr.SetSize(20)
	in.colTxHash.SetSize(32)

	// Query system.columns using a background context to see which columns actually exist in the table!
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	existingCols := make(map[string]bool)
	var name proto.ColStr
	q := fmt.Sprintf(
		"SELECT name FROM system.columns WHERE database = %s AND table = %s",
		quoteString(s.db), quoteString(table.Name),
	)
	if err := s.conn.Do(ctx, ch.Query{
		Body:   q,
		Result: proto.Results{{Name: "name", Data: &name}},
	}); err == nil && name.Rows() > 0 {
		for i := 0; i < name.Rows(); i++ {
			existingCols[name.Row(i)] = true
		}
	} else {
		// Fallback: if query fails (e.g. database not initialized yet), assume all exist
		existingCols = map[string]bool{
			"chain_id": true, "block_number": true, "block_timestamp": true, "block_hash": true,
			"contract_address": true, "transaction_hash": true, "transaction_index": true, "log_index": true,
		}
	}

	var commonCols []proto.InputColumn
	if existingCols["chain_id"] {
		commonCols = append(commonCols, proto.InputColumn{Name: "chain_id", Data: &in.colChain})
	}
	commonCols = append(commonCols, proto.InputColumn{Name: "block_number", Data: &in.colBlock})
	commonCols = append(commonCols, proto.InputColumn{Name: "block_timestamp", Data: &in.colTime})
	if existingCols["block_hash"] {
		commonCols = append(commonCols, proto.InputColumn{Name: "block_hash", Data: &in.colBHash})
	}
	if existingCols["contract_address"] {
		commonCols = append(commonCols, proto.InputColumn{Name: "contract_address", Data: &in.colAddr})
	}
	if existingCols["transaction_hash"] {
		commonCols = append(commonCols, proto.InputColumn{Name: "transaction_hash", Data: &in.colTxHash})
	}
	commonCols = append(commonCols, proto.InputColumn{Name: "transaction_index", Data: &in.colTxIdx})
	commonCols = append(commonCols, proto.InputColumn{Name: "log_index", Data: &in.colLogIdx})

	in.inputCols = commonCols

	for _, arg := range table.Args {
		col := newTypedValueColumn(arg)
		in.valueCols = append(in.valueCols, col)
		in.inputCols = append(in.inputCols, col.input())
	}

	names := make([]string, 0, len(in.inputCols))
	for _, col := range in.inputCols {
		names = append(names, quoteIdent(col.Name))
	}
	in.query = fmt.Sprintf("INSERT INTO %s.%s (%s) VALUES", quoteIdent(s.db), quoteIdent(table.Name), strings.Join(names, ", "))
	return in
}

// TypedInserter batches rows for a single typed event table (one per ABI event).
type TypedInserter struct {
	store *Store
	table TypedEventTable

	colChain  proto.ColUInt64
	colBlock  proto.ColUInt64
	colTime   proto.ColDateTime64
	colBHash  proto.ColFixedStr
	colAddr   proto.ColFixedStr
	colTxHash proto.ColFixedStr
	colTxIdx  proto.ColUInt64
	colLogIdx proto.ColUInt64

	valueCols []typedValueColumn
	inputCols []proto.InputColumn
	query     string
}

func (in *TypedInserter) Insert(ctx context.Context, events []parser.DecodedEvent) error {
	if len(events) == 0 {
		return nil
	}

	total := len(events)
	processed := 0
	chunkSize := 10000

	return in.store.conn.Do(ctx, ch.Query{
		Body:  in.query,
		Input: in.inputCols,
		Settings: []ch.Setting{
			{Key: "async_insert", Value: "1", Important: true},
			{Key: "wait_for_async_insert", Value: "0", Important: true},
		},
		OnInput: func(ctx context.Context) error {
			in.colChain.Reset()
			in.colBlock.Reset()
			in.colTime.Reset()
			in.colBHash.Reset()
			in.colAddr.Reset()
			in.colTxHash.Reset()
			in.colTxIdx.Reset()
			in.colLogIdx.Reset()
			for _, col := range in.valueCols {
				col.reset()
			}

			if processed >= total {
				return io.EOF
			}

			end := processed + chunkSize
			if end > total {
				end = total
			}

			for _, ev := range events[processed:end] {
				in.colChain.Append(ev.ChainID)
				in.colBlock.Append(ev.BlockNumber)
				in.colTime.Append(ev.BlockTimestamp)
				in.colBHash.Append(common.HexToHash(ev.BlockHash).Bytes())
				in.colAddr.Append(common.HexToAddress(ev.Address).Bytes())
				in.colTxHash.Append(common.HexToHash(ev.TxHash).Bytes())
				in.colTxIdx.Append(ev.TxIndex)
				in.colLogIdx.Append(ev.LogIndex)
				for i, arg := range in.table.Args {
					in.valueCols[i].append(ev.Params[arg.Name])
				}
			}
			processed = end
			return nil
		},
	})
}

type typedValueColumn interface {
	input() proto.InputColumn
	append(any)
	reset()
}

type fixedStringValueColumn struct {
	name string
	size int
	col  proto.ColFixedStr
}

func newFixedStringValueColumn(name string, size int) *fixedStringValueColumn {
	c := &fixedStringValueColumn{name: name, size: size}
	c.col.SetSize(size)
	return c
}

func (c *fixedStringValueColumn) input() proto.InputColumn {
	return proto.InputColumn{Name: c.name, Data: &c.col}
}

func (c *fixedStringValueColumn) append(v any) {
	appendFixedStringValue(&c.col, v, c.size)
}

func (c *fixedStringValueColumn) reset() {
	c.col.Reset()
}

func fixedStringBytes(v any, size int) []byte {
	var col proto.ColFixedStr
	col.SetSize(size)
	appendFixedStringValue(&col, v, size)
	return col.Row(0)
}

func appendFixedStringValue(col *proto.ColFixedStr, v any, size int) {
	if size == common.AddressLength {
		switch t := v.(type) {
		case common.Address:
			appendFixedBytes(col, t[:])
			return
		case string:
			if appendCanonicalHexFixed(col, t, size) {
				return
			}
			addr := common.HexToAddress(t)
			appendFixedBytes(col, addr[:])
			return
		case []byte:
			appendLeftPaddedFixed(col, t, size)
			return
		}
		s := fmt.Sprint(v)
		if appendCanonicalHexFixed(col, s, size) {
			return
		}
		addr := common.HexToAddress(s)
		appendFixedBytes(col, addr[:])
		return
	}
	if size == common.HashLength {
		switch t := v.(type) {
		case common.Hash:
			appendFixedBytes(col, t[:])
			return
		case string:
			if appendCanonicalHexFixed(col, t, size) {
				return
			}
			hash := common.HexToHash(t)
			appendFixedBytes(col, hash[:])
			return
		case []byte:
			appendLeftPaddedFixed(col, t, size)
			return
		}
		s := fmt.Sprint(v)
		if appendCanonicalHexFixed(col, s, size) {
			return
		}
		hash := common.HexToHash(s)
		appendFixedBytes(col, hash[:])
		return
	}

	raw, ok := rawBytes(v)
	if !ok {
		raw = common.FromHex(fmt.Sprint(v))
	}
	if len(raw) == size {
		appendFixedBytes(col, raw)
		return
	}
	var scratch [64]byte
	if size <= len(scratch) {
		buf := scratch[:size]
		copy(buf, raw)
		appendFixedBytes(col, buf)
		return
	}
	padded := make([]byte, size)
	copy(padded, raw)
	appendFixedBytes(col, padded)
}

func appendCanonicalHexFixed(col *proto.ColFixedStr, s string, size int) bool {
	if len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		s = s[2:]
	}
	if len(s) != size*2 {
		return false
	}
	var scratch [common.HashLength]byte
	dst := scratch[:size]
	for i := 0; i < size; i++ {
		hi, ok := fromHexChar(s[i*2])
		if !ok {
			return false
		}
		lo, ok := fromHexChar(s[i*2+1])
		if !ok {
			return false
		}
		dst[i] = hi<<4 | lo
	}
	appendFixedBytes(col, dst)
	return true
}

func appendLeftPaddedFixed(col *proto.ColFixedStr, raw []byte, size int) {
	var scratch [common.HashLength]byte
	dst := scratch[:size]
	if len(raw) >= size {
		copy(dst, raw[len(raw)-size:])
	} else {
		copy(dst[size-len(raw):], raw)
	}
	appendFixedBytes(col, dst)
}

func appendFixedBytes(col *proto.ColFixedStr, b []byte) {
	if col.Size == 0 {
		col.Size = len(b)
	}
	if len(b) != col.Size {
		panic("invalid size")
	}
	col.Buf = append(col.Buf, b...)
}

func fromHexChar(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}

func rawBytes(v any) ([]byte, bool) {
	switch t := v.(type) {
	case []byte:
		return t, true
	case common.Address:
		return t.Bytes(), true
	case common.Hash:
		return t.Bytes(), true
	default:
		return nil, false
	}
}

type uint256ValueColumn struct {
	name string
	col  proto.ColUInt256
}

func (c *uint256ValueColumn) input() proto.InputColumn {
	return proto.InputColumn{Name: c.name, Data: &c.col}
}

func (c *uint256ValueColumn) append(v any) {
	switch n := v.(type) {
	case *uint256.Int:
		if n == nil {
			c.col.Append(proto.UInt256{})
			return
		}
		c.col.Append(protoUInt256(*n))
	case uint256.Int:
		c.col.Append(protoUInt256(n))
	case string:
		n = strings.TrimSpace(n)
		parsed, err := uint256.FromDecimal(n)
		if err != nil {
			c.col.Append(proto.UInt256{})
			return
		}
		c.col.Append(protoUInt256(*parsed))
	default:
		c.col.Append(proto.UInt256{})
	}
}

func (c *uint256ValueColumn) reset() {
	c.col.Reset()
}

type boolValueColumn struct {
	name string
	col  proto.ColUInt8
}

func (c *boolValueColumn) input() proto.InputColumn {
	return proto.InputColumn{Name: c.name, Data: &c.col}
}

func (c *boolValueColumn) append(v any) {
	if b, ok := v.(bool); ok && b {
		c.col.Append(1)
		return
	}
	c.col.Append(0)
}

func (c *boolValueColumn) reset() {
	c.col.Reset()
}

type stringValueColumn struct {
	name string
	col  proto.ColStr
}

func (c *stringValueColumn) input() proto.InputColumn {
	return proto.InputColumn{Name: c.name, Data: &c.col}
}

func (c *stringValueColumn) append(v any) {
	c.col.Append(fmt.Sprint(v))
}

func (c *stringValueColumn) reset() {
	c.col.Reset()
}

func newTypedValueColumn(arg TypedEventArg) typedValueColumn {
	switch {
	case arg.ClickHouseType == "UInt8":
		return &boolValueColumn{name: arg.ColumnName}
	case arg.ClickHouseType == "UInt256":
		return &uint256ValueColumn{name: arg.ColumnName}
	case strings.HasPrefix(arg.ClickHouseType, "FixedString("):
		return newFixedStringValueColumn(arg.ColumnName, fixedStringSize(arg.ClickHouseType))
	default:
		return &stringValueColumn{name: arg.ColumnName}
	}
}

func protoUInt256(v uint256.Int) proto.UInt256 {
	return proto.UInt256{
		Low:  proto.UInt128{Low: v[0], High: v[1]},
		High: proto.UInt128{Low: v[2], High: v[3]},
	}
}

func fixedStringSize(clickHouseType string) int {
	open := strings.IndexByte(clickHouseType, '(')
	close := strings.IndexByte(clickHouseType, ')')
	if open < 0 || close <= open+1 {
		return 0
	}
	var n int
	fmt.Sscanf(clickHouseType[open+1:close], "%d", &n)
	return n
}

func (s *Store) UpdateSyncState(ctx context.Context, chainID, lastBlock uint64) error {
	return s.SaveSyncState(ctx, chainID, SyncState{Current: SyncCursor{Number: lastBlock}})
}

func (s *Store) SaveSyncState(ctx context.Context, chainID uint64, state SyncState) error {
	var colChain, colLast, colFinalized proto.ColUInt64
	var colLastHash, colFinalizedHash, colRollback proto.ColStr
	colChain.Append(chainID)
	colLast.Append(state.Current.Number)
	colLastHash.Append(state.Current.Hash)
	if state.Finalized != nil {
		colFinalized.Append(state.Finalized.Number)
		colFinalizedHash.Append(state.Finalized.Hash)
	} else {
		colFinalized.Append(0)
		colFinalizedHash.Append("")
	}
	rollback, err := json.Marshal(state.RollbackChain)
	if err != nil {
		return fmt.Errorf("marshal rollback chain: %w", err)
	}
	colRollback.Append(string(rollback))
	return s.conn.Do(ctx, ch.Query{
		Body: fmt.Sprintf("INSERT INTO %s.sync_state (chain_id, last_block, last_hash, finalized_block, finalized_hash, rollback_chain) VALUES", quoteIdent(s.db)),
		Input: []proto.InputColumn{
			{Name: "chain_id", Data: &colChain}, {Name: "last_block", Data: &colLast},
			{Name: "last_hash", Data: &colLastHash}, {Name: "finalized_block", Data: &colFinalized},
			{Name: "finalized_hash", Data: &colFinalizedHash}, {Name: "rollback_chain", Data: &colRollback},
		},
	})
}

// FlushAsyncInserts forces all server-side async-insert buffers to flush to
// storage, making prior async (wait_for_async_insert=0) inserts durable. Called
// before advancing the durable checkpoint so event rows for blocks <= checkpoint
// are guaranteed persisted (no gap on crash). Cheap because the checkpoint only
// advances at the commit cadence.
func (s *Store) FlushAsyncInserts(ctx context.Context) error {
	return s.conn.Do(ctx, ch.Query{Body: "SYSTEM FLUSH ASYNC INSERT QUEUE"})
}

func (s *Store) TruncateSyncState(ctx context.Context, chainID, lastBlock uint64) error {
	db := quoteIdent(s.db)
	q := fmt.Sprintf("DELETE FROM %s.sync_state WHERE chain_id = %d AND last_block < %d SETTINGS lightweight_deletes_sync = 1", db, chainID, lastBlock)
	return s.conn.Do(ctx, ch.Query{Body: q})
}

func (s *Store) TruncateAfterBlock(ctx context.Context, chainID, lastBlock uint64) error {
	tables, err := s.tablesWithBlockNumber(ctx)
	if err != nil {
		return err
	}
	start := time.Now()
	rollbackSQL := rollbackSQLLoggingEnabled()
	for _, table := range tables {
		where := fmt.Sprintf("block_number > %d", lastBlock)
		if table.HasChainID {
			where = fmt.Sprintf("chain_id = %d AND %s", chainID, where)
		}
		q := fmt.Sprintf("DELETE FROM %s.%s WHERE %s SETTINGS lightweight_deletes_sync = 1", quoteIdent(s.db), quoteIdent(table.Name), where)
		if rollbackSQL {
			log.Printf("[ROLLBACK] delete table %q for blocks > %d: %s", table.Name, lastBlock, q)
		}
		if err := s.conn.Do(ctx, ch.Query{Body: q}); err != nil {
			return fmt.Errorf("rollback %s: %w", table.Name, err)
		}
	}
	log.Printf("[ROLLBACK] issued lightweight delete for %d table(s) with blocks > %d in %s", len(tables), lastBlock, time.Since(start).Round(time.Millisecond))
	return nil
}

func (s *Store) CollapseAfterBlock(ctx context.Context, chainID, lastBlock uint64) error {
	tables, err := s.tablesWithBlockNumber(ctx)
	if err != nil {
		return err
	}
	start := time.Now()
	rollbackSQL := rollbackSQLLoggingEnabled()
	signFlipped := 0
	deleted := 0
	for _, table := range tables {
		if !table.HasSign {
			where := fmt.Sprintf("block_number > %d", lastBlock)
			if table.HasChainID {
				where = fmt.Sprintf("chain_id = %d AND %s", chainID, where)
			}
			q := fmt.Sprintf("DELETE FROM %s.%s WHERE %s SETTINGS lightweight_deletes_sync = 1", quoteIdent(s.db), quoteIdent(table.Name), where)
			if rollbackSQL {
				log.Printf("[ROLLBACK] delete non-collapsing table %q for blocks > %d: %s", table.Name, lastBlock, q)
			}
			if err := s.conn.Do(ctx, ch.Query{Body: q}); err != nil {
				return fmt.Errorf("rollback %s: %w", table.Name, err)
			}
			deleted++
			continue
		}
		columns, err := s.tableColumns(ctx, table.Name)
		if err != nil {
			return err
		}
		where := fmt.Sprintf("block_number > %d", lastBlock)
		if table.HasChainID {
			where = fmt.Sprintf("chain_id = %d AND %s", chainID, where)
		}
		quotedColumns := make([]string, 0, len(columns))
		selectExprs := make([]string, 0, len(columns))
		for _, column := range columns {
			quoted := quoteIdent(column)
			quotedColumns = append(quotedColumns, quoted)
			if column == "sign" {
				selectExprs = append(selectExprs, "toInt8(-sign) AS sign")
				continue
			}
			selectExprs = append(selectExprs, quoted)
		}
		q := fmt.Sprintf(
			"INSERT INTO %s.%s (%s) SELECT %s FROM %s.%s FINAL WHERE %s",
			quoteIdent(s.db), quoteIdent(table.Name), strings.Join(quotedColumns, ", "),
			strings.Join(selectExprs, ", "), quoteIdent(s.db), quoteIdent(table.Name), where,
		)
		if rollbackSQL {
			log.Printf("[ROLLBACK] sign-flip collapsing table %q for blocks > %d: %s", table.Name, lastBlock, q)
		}
		if err := s.conn.Do(ctx, ch.Query{Body: q}); err != nil {
			return fmt.Errorf("collapse rollback %s: %w", table.Name, err)
		}
		signFlipped++
	}
	log.Printf("[ROLLBACK] issued rollback for %d table(s) with blocks > %d in %s (%d sign-flip, %d lightweight delete)", len(tables), lastBlock, time.Since(start).Round(time.Millisecond), signFlipped, deleted)
	return nil
}

func rollbackSQLLoggingEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SQD_LOG_ROLLBACK_SQL"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

type blockNumberTable struct {
	Name       string
	HasChainID bool
	HasSign    bool
}

func (s *Store) tablesWithBlockNumber(ctx context.Context) ([]blockNumberTable, error) {
	var table proto.ColStr
	var hasChain proto.ColUInt64
	var hasSign proto.ColUInt64
	q := fmt.Sprintf(`
		SELECT
			c.table AS table,
			countIf(c.name = 'chain_id') AS has_chain,
			countIf(c.name = 'sign') AS has_sign
		FROM system.columns c
		INNER JOIN system.tables t
			ON c.database = t.database AND c.table = t.name
		WHERE c.database = %s
			AND c.name IN ('block_number', 'chain_id', 'sign')
			AND t.engine NOT LIKE '%%View'
		GROUP BY c.table
		HAVING countIf(c.name = 'block_number') > 0
		ORDER BY c.table`,
		quoteString(s.db),
	)
	if err := s.conn.Do(ctx, ch.Query{
		Body: q,
		Result: proto.Results{
			{Name: "table", Data: &table},
			{Name: "has_chain", Data: &hasChain},
			{Name: "has_sign", Data: &hasSign},
		},
	}); err != nil {
		return nil, fmt.Errorf("list block tables: %w", err)
	}
	out := make([]blockNumberTable, 0, table.Rows())
	for i := 0; i < table.Rows(); i++ {
		out = append(out, blockNumberTable{
			Name:       table.Row(i),
			HasChainID: hasChain.Row(i) > 0,
			HasSign:    hasSign.Row(i) > 0,
		})
	}
	return out, nil
}

func (s *Store) tableColumns(ctx context.Context, table string) ([]string, error) {
	var name proto.ColStr
	q := fmt.Sprintf(
		`SELECT name
		 FROM system.columns
		 WHERE database = %s AND table = %s
		 ORDER BY position`,
		quoteString(s.db), quoteString(table),
	)
	if err := s.conn.Do(ctx, ch.Query{
		Body:   q,
		Result: proto.Results{{Name: "name", Data: &name}},
	}); err != nil {
		return nil, fmt.Errorf("list columns for %s: %w", table, err)
	}
	columns := make([]string, 0, name.Rows())
	for i := 0; i < name.Rows(); i++ {
		columns = append(columns, name.Row(i))
	}
	return columns, nil
}

func (s *Store) LastSyncState(ctx context.Context, chainID uint64) (*SyncState, bool, error) {
	var (
		lastBlock      proto.ColUInt64
		lastHash       proto.ColStr
		finalizedBlock proto.ColUInt64
		finalizedHash  proto.ColStr
		rollbackChain  proto.ColStr
	)
	err := s.conn.Do(ctx, ch.Query{
		Body: fmt.Sprintf(
			`SELECT last_block, last_hash, finalized_block, finalized_hash, rollback_chain
			 FROM %s.sync_state
			 WHERE chain_id = %d
			 ORDER BY updated_at DESC
			 LIMIT 1`,
			quoteIdent(s.db), chainID,
		),
		Result: proto.Results{
			{Name: "last_block", Data: &lastBlock},
			{Name: "last_hash", Data: &lastHash},
			{Name: "finalized_block", Data: &finalizedBlock},
			{Name: "finalized_hash", Data: &finalizedHash},
			{Name: "rollback_chain", Data: &rollbackChain},
		},
	})
	if err != nil {
		return nil, false, err
	}
	if lastBlock.Rows() == 0 {
		return nil, false, nil
	}
	state := &SyncState{
		Current: SyncCursor{
			Number: lastBlock.Row(0),
			Hash:   lastHash.Row(0),
		},
	}
	if finalizedHash.Row(0) != "" {
		state.Finalized = &SyncCursor{Number: finalizedBlock.Row(0), Hash: finalizedHash.Row(0)}
	}
	if raw := rollbackChain.Row(0); strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &state.RollbackChain); err != nil {
			return nil, false, fmt.Errorf("decode rollback chain: %w", err)
		}
	}
	return state, true, nil
}

func (s *Store) LastBlock(ctx context.Context, chainID uint64) (uint64, bool, error) {
	var last proto.ColUInt64
	var count proto.ColUInt64
	if err := s.conn.Do(ctx, ch.Query{
		Body: fmt.Sprintf(
			"SELECT coalesce(max(last_block), 0) AS last, count() AS count FROM %s.sync_state WHERE chain_id = %d",
			quoteIdent(s.db), chainID,
		),
		Result: proto.Results{
			{Name: "last", Data: &last},
			{Name: "count", Data: &count},
		},
	}); err != nil {
		return 0, false, err
	}
	if last.Rows() == 0 || count.Rows() == 0 || count.Row(0) == 0 {
		return s.lastBlockFromBlocks(ctx, chainID)
	}
	return last.Row(0), true, nil
}

func (s *Store) lastBlockFromBlocks(ctx context.Context, chainID uint64) (uint64, bool, error) {
	exists, err := s.tableExists(ctx, "blocks")
	if err != nil {
		return 0, false, err
	}
	if !exists {
		return 0, false, nil
	}

	var last proto.ColUInt64
	var count proto.ColUInt64
	if err := s.conn.Do(ctx, ch.Query{
		Body: fmt.Sprintf(
			"SELECT coalesce(max(block_number), 0) AS last, count() AS count FROM %s.blocks WHERE chain_id = %d",
			quoteIdent(s.db), chainID,
		),
		Result: proto.Results{
			{Name: "last", Data: &last},
			{Name: "count", Data: &count},
		},
	}); err != nil {
		return 0, false, err
	}
	if last.Rows() == 0 || count.Rows() == 0 || count.Row(0) == 0 {
		return 0, false, nil
	}
	return last.Row(0), true, nil
}

func (s *Store) tableExists(ctx context.Context, tableName string) (bool, error) {
	var count proto.ColUInt64
	if err := s.conn.Do(ctx, ch.Query{
		Body: fmt.Sprintf(
			"SELECT count() AS count FROM system.tables WHERE database = %s AND name = %s",
			quoteString(s.db), quoteString(tableName),
		),
		Result: proto.Results{{Name: "count", Data: &count}},
	}); err != nil {
		return false, fmt.Errorf("check table %s: %w", tableName, err)
	}
	return count.Rows() > 0 && count.Row(0) > 0, nil
}

func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func quoteString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func splitSQLStatements(sql string) []string {
	var clean strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		clean.WriteString(line)
		clean.WriteByte('\n')
	}

	var statements []string
	var b strings.Builder
	var quote rune
	escaped := false
	for _, r := range clean.String() {
		if quote != 0 {
			b.WriteRune(r)
			if r == quote && !escaped {
				quote = 0
			}
			escaped = r == '\\' && !escaped
			if r != '\\' {
				escaped = false
			}
			continue
		}
		switch r {
		case '\'', '"', '`':
			quote = r
			b.WriteRune(r)
		case ';':
			stmt := strings.TrimSpace(b.String())
			if stmt != "" {
				statements = append(statements, stmt)
			}
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	stmt := strings.TrimSpace(b.String())
	if stmt != "" {
		statements = append(statements, stmt)
	}
	return statements
}

func firstSQLLine(stmt string) string {
	stmt = strings.TrimSpace(stmt)
	if idx := strings.IndexByte(stmt, '\n'); idx >= 0 {
		stmt = stmt[:idx]
	}
	if len(stmt) > 120 {
		stmt = stmt[:120] + "..."
	}
	return stmt
}
