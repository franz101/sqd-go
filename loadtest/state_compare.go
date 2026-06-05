package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
)

type StateCompareConfig struct {
	Host          string
	Port          int
	User          string
	Password      string
	Database      string
	Positions     int
	Events        int
	Engine        string
	PrefetchBatch int
	ResolveChunk  int
	InsertChunk   int
	QueueCap      int
	FlushInterval time.Duration
}

type stateCompareResult struct {
	Engine         string
	Duration       time.Duration
	Bytes          uint64
	Mallocs        uint64
	NumGC          uint32
	Events         int
	Gets           uint64
	Saves          uint64
	HotHits        uint64
	ColdMisses     uint64
	ResolveQueries uint64
	ResolvedRows   uint64
	Flushes        uint64
	InsertBatches  uint64
	InsertedRows   uint64
	FinalRows      uint64
	LatestRows     uint64
	FinalHash      uint64
}

type stateKey struct {
	user  [20]byte
	token [32]byte
}

type statePosition struct {
	key            stateKey
	amount         uint64
	totalBought    uint64
	avgPrice       uint64
	updatedAtBlock uint64
	blockNumber    uint64
	txIndex        uint64
	logIndex       uint64
}

type stateEvent struct {
	key         stateKey
	delta       uint64
	price       uint64
	blockNumber uint64
	txIndex     uint64
	logIndex    uint64
}

type stateLoadState struct {
	conn           *ch.Client
	db             string
	resolveChunk   int
	hot            map[stateKey]statePosition
	dirty          map[stateKey]struct{}
	gets           uint64
	saves          uint64
	hotHits        uint64
	coldMisses     uint64
	resolveQueries uint64
	resolvedRows   uint64
}

func RunStateCompare(ctx context.Context, cfg StateCompareConfig) error {
	if cfg.Positions <= 0 {
		return fmt.Errorf("positions must be > 0")
	}
	if cfg.Events <= 0 {
		return fmt.Errorf("events must be > 0")
	}
	if cfg.PrefetchBatch <= 0 {
		return fmt.Errorf("prefetch-batch must be > 0")
	}
	if cfg.ResolveChunk <= 0 {
		return fmt.Errorf("resolve-chunk must be > 0")
	}
	if cfg.InsertChunk <= 0 {
		return fmt.Errorf("insert-chunk must be > 0")
	}
	if cfg.QueueCap <= 0 {
		return fmt.Errorf("queue-cap must be > 0")
	}

	engines, err := expandPositionOption(cfg.Engine, "engine", []string{"current", "improved"})
	if err != nil {
		return err
	}

	conn, err := ch.Dial(ctx, ch.Options{
		Address:  fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Database: "default",
		User:     cfg.User,
		Password: cfg.Password,
	})
	if err != nil {
		return fmt.Errorf("connect clickhouse: %w", err)
	}
	defer conn.Close()

	if err := ensureStateCompareTable(ctx, conn, cfg.Database); err != nil {
		return err
	}

	events := newStateEvents(cfg.Events, cfg.Positions)
	results := make([]stateCompareResult, 0, len(engines))
	for _, engine := range engines {
		log.Printf("State compare setup: engine=%s positions=%d events=%d db=%s", engine, cfg.Positions, cfg.Events, cfg.Database)
		if err := resetStateCompareTable(ctx, conn, cfg.Database); err != nil {
			return err
		}
		if err := populateStateCompareTable(ctx, conn, cfg.Database, cfg.Positions, cfg.InsertChunk); err != nil {
			return err
		}
		result, err := runStateEngine(ctx, cfg, conn, events, engine)
		if err != nil {
			return err
		}
		results = append(results, result)
	}

	reportStateResults(results)
	return nil
}

func runStateEngine(ctx context.Context, cfg StateCompareConfig, conn *ch.Client, events []stateEvent, engine string) (stateCompareResult, error) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	writerConn, err := ch.Dial(ctx, ch.Options{
		Address:  fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Database: "default",
		User:     cfg.User,
		Password: cfg.Password,
	})
	if err != nil {
		return stateCompareResult{}, fmt.Errorf("connect writer clickhouse: %w", err)
	}
	defer writerConn.Close()

	state := &stateLoadState{
		conn:         conn,
		db:           cfg.Database,
		resolveChunk: cfg.ResolveChunk,
		hot:          make(map[stateKey]statePosition, minInt(cfg.Positions, cfg.Events)),
		dirty:        make(map[stateKey]struct{}, minInt(cfg.Positions, cfg.Events)),
	}
	writer := newStateDBWriter(writerConn, cfg.Database, cfg.InsertChunk, cfg.QueueCap)
	writer.start(ctx)

	start := time.Now()
	var runErr error
	switch engine {
	case "current":
		runErr = runStateCurrent(ctx, state, writer, events, cfg.FlushInterval)
	case "improved":
		runErr = runStateImproved(ctx, state, writer, events, cfg.PrefetchBatch, cfg.FlushInterval)
	default:
		runErr = fmt.Errorf("invalid engine %q", engine)
	}
	if runErr == nil {
		runErr = writer.close()
	} else {
		_ = writer.close()
	}
	duration := time.Since(start)
	runtime.ReadMemStats(&after)
	if runErr != nil {
		return stateCompareResult{}, runErr
	}

	finalRows, err := countStateRows(ctx, conn, cfg.Database)
	if err != nil {
		return stateCompareResult{}, err
	}
	latestRows, finalHash, err := checksumStateLatest(ctx, conn, cfg.Database)
	if err != nil {
		return stateCompareResult{}, err
	}

	return stateCompareResult{
		Engine:         engine,
		Duration:       duration,
		Bytes:          after.TotalAlloc - before.TotalAlloc,
		Mallocs:        after.Mallocs - before.Mallocs,
		NumGC:          after.NumGC - before.NumGC,
		Events:         len(events),
		Gets:           state.gets,
		Saves:          state.saves,
		HotHits:        state.hotHits,
		ColdMisses:     state.coldMisses,
		ResolveQueries: state.resolveQueries,
		ResolvedRows:   state.resolvedRows,
		Flushes:        writer.flushes,
		InsertBatches:  writer.insertBatches,
		InsertedRows:   writer.insertedRows,
		FinalRows:      finalRows,
		LatestRows:     latestRows,
		FinalHash:      finalHash,
	}, nil
}

func runStateCurrent(ctx context.Context, state *stateLoadState, writer *stateDBWriter, events []stateEvent, flushInterval time.Duration) error {
	lastFlush := time.Now()
	for i := range events {
		ev := events[i]
		pos, ok, err := state.Get(ctx, ev.key)
		if err != nil {
			return err
		}
		if !ok {
			pos = statePosition{key: ev.key}
		}
		applyStateEvent(&pos, ev)
		state.Save(pos)
		if shouldFlush(lastFlush, flushInterval) {
			if err := state.flushDirty(ctx, writer); err != nil {
				return err
			}
			lastFlush = time.Now()
		}
	}
	return state.flushDirty(ctx, writer)
}

func runStateImproved(ctx context.Context, state *stateLoadState, writer *stateDBWriter, events []stateEvent, prefetchBatch int, flushInterval time.Duration) error {
	lastFlush := time.Now()
	for start := 0; start < len(events); start += prefetchBatch {
		end := minInt(start+prefetchBatch, len(events))
		if err := state.Prefetch(ctx, events[start:end]); err != nil {
			return err
		}
		for i := start; i < end; i++ {
			ev := events[i]
			pos, ok := state.GetHot(ev.key)
			state.gets++
			if ok {
				state.hotHits++
			} else {
				pos = statePosition{key: ev.key}
			}
			applyStateEvent(&pos, ev)
			state.Save(pos)
		}
		if shouldFlush(lastFlush, flushInterval) {
			if err := state.flushDirty(ctx, writer); err != nil {
				return err
			}
			lastFlush = time.Now()
		}
	}
	return state.flushDirty(ctx, writer)
}

func shouldFlush(last time.Time, interval time.Duration) bool {
	return interval > 0 && time.Since(last) >= interval
}

func (s *stateLoadState) Get(ctx context.Context, key stateKey) (statePosition, bool, error) {
	s.gets++
	if value, ok := s.GetHot(key); ok {
		s.hotHits++
		return value, true, nil
	}
	s.coldMisses++
	if err := s.resolveKeys(ctx, []stateKey{key}); err != nil {
		return statePosition{}, false, err
	}
	value, ok := s.GetHot(key)
	return value, ok, nil
}

func (s *stateLoadState) GetHot(key stateKey) (statePosition, bool) {
	value, ok := s.hot[key]
	return value, ok
}

func (s *stateLoadState) Save(value statePosition) {
	s.saves++
	s.hot[value.key] = value
	s.dirty[value.key] = struct{}{}
}

func (s *stateLoadState) Prefetch(ctx context.Context, events []stateEvent) error {
	if len(events) == 0 {
		return nil
	}
	unique := make(map[stateKey]struct{}, len(events))
	keys := make([]stateKey, 0, len(events))
	for i := range events {
		key := events[i].key
		if _, ok := s.hot[key]; ok {
			continue
		}
		if _, ok := unique[key]; ok {
			continue
		}
		unique[key] = struct{}{}
		keys = append(keys, key)
	}
	s.coldMisses += uint64(len(keys))
	return s.resolveKeys(ctx, keys)
}

func (s *stateLoadState) flushDirty(ctx context.Context, writer *stateDBWriter) error {
	if len(s.dirty) == 0 {
		return nil
	}
	rows := make([]statePosition, 0, len(s.dirty))
	for key := range s.dirty {
		rows = append(rows, s.hot[key])
	}
	clear(s.dirty)
	return writer.enqueue(ctx, rows)
}

func (s *stateLoadState) resolveKeys(ctx context.Context, keys []stateKey) error {
	for start := 0; start < len(keys); start += s.resolveChunk {
		end := minInt(start+s.resolveChunk, len(keys))
		if err := s.resolveKeyChunk(ctx, keys[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (s *stateLoadState) resolveKeyChunk(ctx context.Context, keys []stateKey) error {
	if len(keys) == 0 {
		return nil
	}
	s.resolveQueries++
	var values strings.Builder
	values.Grow(len(keys) * 128)
	for i, key := range keys {
		if i > 0 {
			values.WriteString(",")
		}
		values.WriteString("(unhex('")
		values.WriteString(hex.EncodeToString(key.user[:]))
		values.WriteString("'),unhex('")
		values.WriteString(hex.EncodeToString(key.token[:]))
		values.WriteString("'))")
	}

	var (
		colUser           proto.ColFixedStr
		colToken          proto.ColFixedStr
		colAmount         proto.ColUInt64
		colTotalBought    proto.ColUInt64
		colAvgPrice       proto.ColUInt64
		colUpdatedAtBlock proto.ColUInt64
		colBlockNumber    proto.ColUInt64
		colTxIndex        proto.ColUInt64
		colLogIndex       proto.ColUInt64
	)
	colUser.SetSize(20)
	colToken.SetSize(32)
	results := proto.Results{
		{Name: "user", Data: &colUser},
		{Name: "token_id", Data: &colToken},
		{Name: "amount", Data: &colAmount},
		{Name: "total_bought", Data: &colTotalBought},
		{Name: "avg_price", Data: &colAvgPrice},
		{Name: "updated_at_block", Data: &colUpdatedAtBlock},
		{Name: "block_number", Data: &colBlockNumber},
		{Name: "transaction_index", Data: &colTxIndex},
		{Name: "log_index", Data: &colLogIndex},
	}
	query := fmt.Sprintf("SELECT `user`, `token_id`, `amount`, `total_bought`, `avg_price`, `updated_at_block`, `block_number`, `transaction_index`, `log_index` FROM %s.loadtest_state_positions WHERE (`user`, `token_id`) IN (%s) ORDER BY `block_number` DESC, `transaction_index` DESC, `log_index` DESC LIMIT 1 BY `user`, `token_id`", cfgIdent(s.db), values.String())
	return s.conn.Do(ctx, ch.Query{
		Body:   query,
		Result: results,
		OnResult: func(ctx context.Context, block proto.Block) error {
			s.resolvedRows += uint64(block.Rows)
			for i := 0; i < block.Rows; i++ {
				var key stateKey
				copy(key.user[:], colUser.Row(i))
				copy(key.token[:], colToken.Row(i))
				s.hot[key] = statePosition{
					key:            key,
					amount:         colAmount.Row(i),
					totalBought:    colTotalBought.Row(i),
					avgPrice:       colAvgPrice.Row(i),
					updatedAtBlock: colUpdatedAtBlock.Row(i),
					blockNumber:    colBlockNumber.Row(i),
					txIndex:        colTxIndex.Row(i),
					logIndex:       colLogIndex.Row(i),
				}
			}
			return nil
		},
	})
}

type stateDBWriter struct {
	conn          *ch.Client
	db            string
	insertChunk   int
	queue         chan []statePosition
	done          chan struct{}
	err           error
	mu            sync.Mutex
	flushes       uint64
	insertBatches uint64
	insertedRows  uint64
}

func newStateDBWriter(conn *ch.Client, db string, insertChunk int, queueCap int) *stateDBWriter {
	return &stateDBWriter{
		conn:        conn,
		db:          db,
		insertChunk: insertChunk,
		queue:       make(chan []statePosition, queueCap),
		done:        make(chan struct{}),
	}
}

func (w *stateDBWriter) start(ctx context.Context) {
	go func() {
		defer close(w.done)
		cols := newStatePositionColumns(w.insertChunk)
		for rows := range w.queue {
			if len(rows) == 0 {
				continue
			}
			w.flushes++
			for start := 0; start < len(rows); start += w.insertChunk {
				end := minInt(start+w.insertChunk, len(rows))
				cols.fill(rows[start:end])
				if err := w.conn.Do(ctx, ch.Query{Body: stateInsertQuery(w.db), Input: cols.input}); err != nil {
					w.setErr(err)
					return
				}
				w.insertBatches++
				w.insertedRows += uint64(end - start)
			}
		}
	}()
}

func (w *stateDBWriter) enqueue(ctx context.Context, rows []statePosition) error {
	if err := w.getErr(); err != nil {
		return err
	}
	select {
	case w.queue <- rows:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *stateDBWriter) close() error {
	close(w.queue)
	<-w.done
	return w.getErr()
}

func (w *stateDBWriter) setErr(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err == nil {
		w.err = err
	}
}

func (w *stateDBWriter) getErr() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

type statePositionColumns struct {
	colUser           proto.ColFixedStr
	colToken          proto.ColFixedStr
	colAmount         proto.ColUInt64
	colTotalBought    proto.ColUInt64
	colAvgPrice       proto.ColUInt64
	colUpdatedAtBlock proto.ColUInt64
	colBlockNumber    proto.ColUInt64
	colTxIndex        proto.ColUInt64
	colLogIndex       proto.ColUInt64
	input             proto.Input
}

func newStatePositionColumns(capacity int) *statePositionColumns {
	cols := &statePositionColumns{
		colAmount:         make(proto.ColUInt64, 0, capacity),
		colTotalBought:    make(proto.ColUInt64, 0, capacity),
		colAvgPrice:       make(proto.ColUInt64, 0, capacity),
		colUpdatedAtBlock: make(proto.ColUInt64, 0, capacity),
		colBlockNumber:    make(proto.ColUInt64, 0, capacity),
		colTxIndex:        make(proto.ColUInt64, 0, capacity),
		colLogIndex:       make(proto.ColUInt64, 0, capacity),
	}
	cols.colUser.SetSize(20)
	cols.colToken.SetSize(32)
	cols.colUser.Buf = make([]byte, 0, capacity*20)
	cols.colToken.Buf = make([]byte, 0, capacity*32)
	cols.input = proto.Input{
		{Name: "user", Data: &cols.colUser},
		{Name: "token_id", Data: &cols.colToken},
		{Name: "amount", Data: &cols.colAmount},
		{Name: "total_bought", Data: &cols.colTotalBought},
		{Name: "avg_price", Data: &cols.colAvgPrice},
		{Name: "updated_at_block", Data: &cols.colUpdatedAtBlock},
		{Name: "block_number", Data: &cols.colBlockNumber},
		{Name: "transaction_index", Data: &cols.colTxIndex},
		{Name: "log_index", Data: &cols.colLogIndex},
	}
	return cols
}

func (c *statePositionColumns) fill(rows []statePosition) {
	c.input.Reset()
	for i := range rows {
		row := rows[i]
		c.colUser.Append(row.key.user[:])
		c.colToken.Append(row.key.token[:])
		c.colAmount.Append(row.amount)
		c.colTotalBought.Append(row.totalBought)
		c.colAvgPrice.Append(row.avgPrice)
		c.colUpdatedAtBlock.Append(row.updatedAtBlock)
		c.colBlockNumber.Append(row.blockNumber)
		c.colTxIndex.Append(row.txIndex)
		c.colLogIndex.Append(row.logIndex)
	}
}

func newStateEvents(rows int, positions int) []stateEvent {
	events := make([]stateEvent, rows)
	for i := 0; i < rows; i++ {
		key := stateKeyFromID(uint64(i % positions))
		events[i] = stateEvent{
			key:         key,
			delta:       uint64(1 + i%7),
			price:       uint64(100 + i%50),
			blockNumber: uint64(1 + i/500),
			txIndex:     uint64(i % 500),
			logIndex:    uint64(i),
		}
	}
	return events
}

func stateKeyFromID(id uint64) stateKey {
	var key stateKey
	putTailUint64(key.user[:], id+1)
	putTailUint64(key.token[:], (id+1)*17)
	return key
}

func initialStatePosition(id uint64) statePosition {
	key := stateKeyFromID(id)
	amount := uint64(10 + id%90)
	price := uint64(100 + id%50)
	return statePosition{
		key:            key,
		amount:         amount,
		totalBought:    amount * price,
		avgPrice:       price,
		updatedAtBlock: 0,
		blockNumber:    0,
	}
}

func applyStateEvent(pos *statePosition, ev stateEvent) {
	pos.amount += ev.delta
	pos.totalBought += ev.delta * ev.price
	if pos.amount > 0 {
		pos.avgPrice = pos.totalBought / pos.amount
	}
	pos.updatedAtBlock = ev.blockNumber
	pos.blockNumber = ev.blockNumber
	pos.txIndex = ev.txIndex
	pos.logIndex = ev.logIndex
}

func populateStateCompareTable(ctx context.Context, conn *ch.Client, db string, positions int, chunkSize int) error {
	cols := newStatePositionColumns(chunkSize)
	processed := 0
	return conn.Do(ctx, ch.Query{
		Body:  stateInsertQuery(db),
		Input: cols.input,
		OnInput: func(ctx context.Context) error {
			if processed >= positions {
				cols.input.Reset()
				return io.EOF
			}
			end := minInt(processed+chunkSize, positions)
			rows := make([]statePosition, 0, end-processed)
			for id := processed; id < end; id++ {
				rows = append(rows, initialStatePosition(uint64(id)))
			}
			cols.fill(rows)
			processed = end
			return nil
		},
	})
}

func ensureStateCompareTable(ctx context.Context, conn *ch.Client, db string) error {
	if err := conn.Do(ctx, ch.Query{Body: fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", cfgIdent(db))}); err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	query := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s.loadtest_state_positions (
  user FixedString(20),
  token_id FixedString(32),
  amount UInt64,
  total_bought UInt64,
  avg_price UInt64,
  updated_at_block UInt64,
  block_number UInt64,
  transaction_index UInt64,
  log_index UInt64
) ENGINE = ReplacingMergeTree(block_number)
ORDER BY (user, token_id)`, cfgIdent(db))
	if err := conn.Do(ctx, ch.Query{Body: query}); err != nil {
		return fmt.Errorf("create state compare table: %w", err)
	}
	return nil
}

func resetStateCompareTable(ctx context.Context, conn *ch.Client, db string) error {
	if err := conn.Do(ctx, ch.Query{Body: fmt.Sprintf("TRUNCATE TABLE %s.loadtest_state_positions", cfgIdent(db))}); err != nil {
		return fmt.Errorf("truncate state compare table: %w", err)
	}
	return nil
}

func countStateRows(ctx context.Context, conn *ch.Client, db string) (uint64, error) {
	var count proto.ColUInt64
	if err := conn.Do(ctx, ch.Query{
		Body: fmt.Sprintf("SELECT count() FROM %s.loadtest_state_positions", cfgIdent(db)),
		Result: proto.Results{
			{Name: "count()", Data: &count},
		},
	}); err != nil {
		return 0, err
	}
	if count.Rows() == 0 {
		return 0, nil
	}
	return count.Row(0), nil
}

func checksumStateLatest(ctx context.Context, conn *ch.Client, db string) (uint64, uint64, error) {
	var (
		colUser        proto.ColFixedStr
		colToken       proto.ColFixedStr
		colAmount      proto.ColUInt64
		colTotalBought proto.ColUInt64
		colAvgPrice    proto.ColUInt64
		colBlockNumber proto.ColUInt64
		colTxIndex     proto.ColUInt64
		colLogIndex    proto.ColUInt64
	)
	colUser.SetSize(20)
	colToken.SetSize(32)
	results := proto.Results{
		{Name: "user", Data: &colUser},
		{Name: "token_id", Data: &colToken},
		{Name: "amount", Data: &colAmount},
		{Name: "total_bought", Data: &colTotalBought},
		{Name: "avg_price", Data: &colAvgPrice},
		{Name: "block_number", Data: &colBlockNumber},
		{Name: "transaction_index", Data: &colTxIndex},
		{Name: "log_index", Data: &colLogIndex},
	}
	var rows uint64
	var hash uint64
	query := fmt.Sprintf("SELECT `user`, `token_id`, `amount`, `total_bought`, `avg_price`, `block_number`, `transaction_index`, `log_index` FROM %s.loadtest_state_positions ORDER BY `block_number` DESC, `transaction_index` DESC, `log_index` DESC LIMIT 1 BY `user`, `token_id`", cfgIdent(db))
	err := conn.Do(ctx, ch.Query{
		Body:   query,
		Result: results,
		OnResult: func(ctx context.Context, block proto.Block) error {
			rows += uint64(block.Rows)
			for i := 0; i < block.Rows; i++ {
				rowHash := stateMixBytes(1469598103934665603, colUser.Row(i))
				rowHash = stateMixBytes(rowHash, colToken.Row(i))
				rowHash = stateMixUint64(rowHash, colAmount.Row(i))
				rowHash = stateMixUint64(rowHash, colTotalBought.Row(i))
				rowHash = stateMixUint64(rowHash, colAvgPrice.Row(i))
				rowHash = stateMixUint64(rowHash, colBlockNumber.Row(i))
				rowHash = stateMixUint64(rowHash, colTxIndex.Row(i))
				rowHash = stateMixUint64(rowHash, colLogIndex.Row(i))
				hash += rowHash
			}
			return nil
		},
	})
	return rows, hash, err
}

func stateInsertQuery(db string) string {
	return fmt.Sprintf("INSERT INTO %s.loadtest_state_positions (`user`, `token_id`, `amount`, `total_bought`, `avg_price`, `updated_at_block`, `block_number`, `transaction_index`, `log_index`) VALUES", cfgIdent(db))
}

func reportStateResults(results []stateCompareResult) {
	for _, result := range results {
		log.Printf("[STATE] engine=%s events=%d duration=%s gets=%d saves=%d hot_hits=%d cold_misses=%d resolve_queries=%d resolved_rows=%d flushes=%d insert_batches=%d inserted_rows=%d final_rows=%d latest_rows=%d hash=%016x alloc=%s mallocs=%d gc=%d",
			result.Engine,
			result.Events,
			result.Duration.Round(time.Millisecond),
			result.Gets,
			result.Saves,
			result.HotHits,
			result.ColdMisses,
			result.ResolveQueries,
			result.ResolvedRows,
			result.Flushes,
			result.InsertBatches,
			result.InsertedRows,
			result.FinalRows,
			result.LatestRows,
			result.FinalHash,
			humanBytes(result.Bytes),
			result.Mallocs,
			result.NumGC,
		)
	}
	for _, improved := range results {
		if improved.Engine != "improved" {
			continue
		}
		for _, current := range results {
			if current.Engine != "current" || improved.Duration <= 0 {
				continue
			}
			log.Printf("[STATE] improvement speedup=%.2fx allocation_reduction=%.2fx malloc_reduction=%.2fx query_reduction=%.2fx",
				float64(current.Duration)/float64(improved.Duration),
				ratioUint64(current.Bytes, improved.Bytes),
				ratioUint64(current.Mallocs, improved.Mallocs),
				ratioUint64(current.ResolveQueries, improved.ResolveQueries),
			)
			if current.FinalHash != improved.FinalHash || current.LatestRows != improved.LatestRows {
				log.Printf("[STATE] warning latest-state mismatch current_rows=%d improved_rows=%d current_hash=%016x improved_hash=%016x",
					current.LatestRows,
					improved.LatestRows,
					current.FinalHash,
					improved.FinalHash,
				)
			}
		}
	}
}

func stateMixBytes(h uint64, data []byte) uint64 {
	for _, v := range data {
		h ^= uint64(v)
		h *= 1099511628211
	}
	return h
}

func stateMixUint64(h uint64, v uint64) uint64 {
	h ^= v
	h *= 1099511628211
	h ^= v >> 32
	h *= 1099511628211
	return h
}

func putTailUint64(dst []byte, v uint64) {
	for i := len(dst) - 1; i >= 0 && v != 0; i-- {
		dst[i] = byte(v)
		v >>= 8
	}
}

func cfgIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}
