package database

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
	"github.com/ethereum/go-ethereum/common"
	"github.com/franz101/sqd-go/internal/parser"
	"github.com/holiman/uint256"
)

type Store struct {
	conn *ch.Client
	db   string
}

type BlockRow struct {
	ChainID        uint64
	BlockNumber    uint64
	BlockTimestamp time.Time
	BlockHash      string
}

type TypedEventTable struct {
	Name string
	Args []TypedEventArg
}

type TypedEventArg struct {
	Name           string
	ColumnName     string
	SolidityType   string
	ClickHouseType string
}

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
	return s.conn
}

func (s *Store) DB() string {
	return s.db
}

func (s *Store) EnsureTables(ctx context.Context) error {
	db := quoteIdent(s.db)
	blocksDDL := fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s.blocks (
			chain_id UInt64, block_number UInt64,
			block_timestamp DateTime64(3, 'UTC'), block_hash String,
			inserted_at DateTime64(3, 'UTC') DEFAULT now64(3)
		) ENGINE = MergeTree() ORDER BY (chain_id, block_number)`, db)
	if err := s.conn.Do(ctx, ch.Query{Body: blocksDDL}); err != nil {
		return fmt.Errorf("create blocks: %w", err)
	}
	logsDDL := fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s.logs (
			chain_id UInt64, block_number UInt64,
			block_timestamp DateTime64(3, 'UTC'), block_hash String,
			transaction_hash FixedString(32), transaction_index UInt64, log_index UInt64,
			address FixedString(20), event_name LowCardinality(String),
			topic0 FixedString(32), params String,
			inserted_at DateTime64(3, 'UTC') DEFAULT now64(3)
		) ENGINE = MergeTree() ORDER BY (chain_id, block_number, transaction_index, log_index)`, db)
	if err := s.conn.Do(ctx, ch.Query{Body: logsDDL}); err != nil {
		return fmt.Errorf("create logs: %w", err)
	}
	stateDDL := fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s.sync_state (
			chain_id UInt64, last_block UInt64,
			updated_at DateTime64(3, 'UTC') DEFAULT now64(3)
		) ENGINE = MergeTree() ORDER BY (chain_id, updated_at)`, db)
	if err := s.conn.Do(ctx, ch.Query{Body: stateDDL}); err != nil {
		return fmt.Errorf("create sync_state: %w", err)
	}
	return nil
}

func (s *Store) ApplySQLFile(ctx context.Context, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, stmt := range splitSQLStatements(string(raw)) {
		if err := s.conn.Do(ctx, ch.Query{Body: stmt}); err != nil {
			return fmt.Errorf("%s: %w", firstSQLLine(stmt), err)
		}
	}
	return nil
}

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

func (in *Inserter) InsertTypedLogs(ctx context.Context, table TypedEventTable, events []parser.DecodedEvent) error {
	if len(events) == 0 {
		return nil
	}

	var (
		colChain    proto.ColUInt64
		colBlock    proto.ColUInt64
		colTime     proto.ColDateTime64
		colBHash    proto.ColFixedStr
		colAddr     proto.ColFixedStr
		colTxHash   proto.ColFixedStr
		colTxIdx    proto.ColUInt64
		colLogIdx   proto.ColUInt64
		valueCols   []typedValueColumn
		inputColumn []proto.InputColumn
	)
	colTime.WithPrecision(proto.Precision(3))
	colTime.WithLocation(time.UTC)
	colBHash.SetSize(32)
	colAddr.SetSize(20)
	colTxHash.SetSize(32)

	inputColumn = []proto.InputColumn{
		{Name: "chain_id", Data: &colChain},
		{Name: "block_number", Data: &colBlock},
		{Name: "block_timestamp", Data: &colTime},
		{Name: "block_hash", Data: &colBHash},
		{Name: "contract_address", Data: &colAddr},
		{Name: "transaction_hash", Data: &colTxHash},
		{Name: "transaction_index", Data: &colTxIdx},
		{Name: "log_index", Data: &colLogIdx},
	}
	for _, arg := range table.Args {
		col := newTypedValueColumn(arg)
		valueCols = append(valueCols, col)
		inputColumn = append(inputColumn, col.input())
	}

	for _, ev := range events {
		colChain.Append(ev.ChainID)
		colBlock.Append(ev.BlockNumber)
		colTime.Append(ev.BlockTimestamp)
		colBHash.Append(common.HexToHash(ev.BlockHash).Bytes())
		colAddr.Append(common.HexToAddress(ev.Address).Bytes())
		colTxHash.Append(common.HexToHash(ev.TxHash).Bytes())
		colTxIdx.Append(ev.TxIndex)
		colLogIdx.Append(ev.LogIndex)
		for i, arg := range table.Args {
			valueCols[i].append(ev.Params[arg.Name])
		}
	}

	names := make([]string, 0, len(inputColumn))
	for _, col := range inputColumn {
		names = append(names, quoteIdent(col.Name))
	}
	return in.store.conn.Do(ctx, ch.Query{
		Body:  fmt.Sprintf("INSERT INTO %s.%s (%s) VALUES", quoteIdent(in.store.db), quoteIdent(table.Name), strings.Join(names, ", ")),
		Input: inputColumn,
	})
}

type typedValueColumn interface {
	input() proto.InputColumn
	append(any)
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
	c.col.Append(fixedStringBytes(v, c.size))
}

func fixedStringBytes(v any, size int) []byte {
	if size == common.AddressLength {
		if raw, ok := rawBytes(v); ok {
			return common.BytesToAddress(raw).Bytes()
		}
		return common.HexToAddress(fmt.Sprint(v)).Bytes()
	}
	if size == common.HashLength {
		if raw, ok := rawBytes(v); ok {
			return common.BytesToHash(raw).Bytes()
		}
		return common.HexToHash(fmt.Sprint(v)).Bytes()
	}

	raw, ok := rawBytes(v)
	if !ok {
		raw = common.FromHex(fmt.Sprint(v))
	}
	if len(raw) == size {
		return raw
	}
	padded := make([]byte, size)
	copy(padded, raw)
	return padded
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
	var colChain, colLast proto.ColUInt64
	colChain.Append(chainID)
	colLast.Append(lastBlock)
	return s.conn.Do(ctx, ch.Query{
		Body: fmt.Sprintf("INSERT INTO %s.sync_state (chain_id, last_block) VALUES", quoteIdent(s.db)),
		Input: []proto.InputColumn{
			{Name: "chain_id", Data: &colChain},
			{Name: "last_block", Data: &colLast},
		},
	})
}

func (s *Store) TruncateSyncState(ctx context.Context, chainID, lastBlock uint64) error {
	db := quoteIdent(s.db)
	q := fmt.Sprintf("ALTER TABLE %s.sync_state DELETE WHERE chain_id = %d AND last_block < %d SETTINGS mutations_sync = 1", db, chainID, lastBlock)
	return s.conn.Do(ctx, ch.Query{Body: q})
}

func (s *Store) TruncateAfterBlock(ctx context.Context, chainID, lastBlock uint64) error {
	tables, err := s.tablesWithBlockNumber(ctx)
	if err != nil {
		return err
	}
	for _, table := range tables {
		where := fmt.Sprintf("block_number > %d", lastBlock)
		if table.HasChainID {
			where = fmt.Sprintf("chain_id = %d AND %s", chainID, where)
		}
		q := fmt.Sprintf("ALTER TABLE %s.%s DELETE WHERE %s SETTINGS mutations_sync = 1", quoteIdent(s.db), quoteIdent(table.Name), where)
		if err := s.conn.Do(ctx, ch.Query{Body: q}); err != nil {
			return fmt.Errorf("truncate %s: %w", table.Name, err)
		}
	}
	return nil
}

type blockNumberTable struct {
	Name       string
	HasChainID bool
}

func (s *Store) tablesWithBlockNumber(ctx context.Context) ([]blockNumberTable, error) {
	var table proto.ColStr
	var hasChain proto.ColUInt64
	q := fmt.Sprintf(`
		SELECT
			c.table AS table,
			countIf(c.name = 'chain_id') AS has_chain
		FROM system.columns c
		INNER JOIN system.tables t
			ON c.database = t.database AND c.table = t.name
		WHERE c.database = %s
			AND c.name IN ('block_number', 'chain_id')
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
		},
	}); err != nil {
		return nil, fmt.Errorf("list block tables: %w", err)
	}
	out := make([]blockNumberTable, 0, table.Rows())
	for i := 0; i < table.Rows(); i++ {
		out = append(out, blockNumberTable{
			Name:       table.Row(i),
			HasChainID: hasChain.Row(i) > 0,
		})
	}
	return out, nil
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
