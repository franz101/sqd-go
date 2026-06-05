// Prefetch modes benchmark for Polymarket-style state access.
//
// This intentionally avoids the old single-table "position only" benchmark.
// Real Polymarket processing has dependent reads:
//   - Condition/FPMM/NegRiskEvent reads decide whether the handler continues.
//   - Those first-stage values determine which Position keys are needed.
//   - Position miss/hit affects sell, redemption, conversion, and avg-price math.
//
// The "v3" mode here models naive deferred gets: a cache miss is queued and
// returns false until an automatic threshold flush happens. That is deliberately
// tested against handlers that branch on the Get result, because returning false
// early can change handler behavior.
//
// The "v3-auto" mode generalizes the winning "v3-deep" approach: instead of
// hand-written key extraction per event type, the handlers themselves act as
// the key extractor. Discovery rounds execute the batch against a throwaway
// write overlay (reads: overlay -> hot cache -> known-absent -> track miss)
// and batch-resolve whatever was tracked, until a round tracks nothing new.
// The real pass then runs cache-hot, with synchronous resolves as a
// correctness fallback, so the dirty-state hash always matches the baseline.

package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/proto"
	"github.com/ethereum/go-ethereum/crypto"
)

var (
	prefetchModeFlag = flag.String("prefetch-mode", "all", "mode(s) to run: all,current,improved,dryrun,v3,v3-fallback,v3-retry,v3-deep,v3-auto")
	prefetchV3Flag   = flag.Bool("prefetch-v3", false, "run the baseline plus the V3/deferred mode")
)

const (
	benchPriceScale = int64(1_000_000)
	benchHalfPrice  = benchPriceScale / 2
)

var (
	benchZeroAddress       [20]byte
	benchZeroHash          [32]byte
	benchExchangeAddress   = benchAddressFromID(9_000_001)
	benchNegRiskExchange   = benchAddressFromID(9_000_002)
	benchNegRiskAdapter    = benchAddressFromID(9_000_003)
	benchWrappedCollateral = benchAddressFromID(9_000_004)
	benchDefaultCollateral = benchAddressFromID(9_000_005)
)

// PrefetchModeConfig configures the benchmark.
type PrefetchModeConfig struct {
	Host          string
	Port          int
	User          string
	Password      string
	Database      string
	Positions     int
	Events        int
	Users         int
	Conditions    int
	FPMMs         int
	Markets       int
	ResolveChunk  int
	PrefetchBatch int
	Threshold     int
	Modes         string
	V3            bool
}

// PrefetchModeResult captures metrics for each mode.
type PrefetchModeResult struct {
	Mode           string
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
	HandlerExecs   uint64
	FinalHash      uint64
}

type processMode int

const (
	modeCurrent processMode = iota
	modeImproved
	modeDryRun
	modeV3
	modeV3Fallback
	modeV3Retry
	modeV3Deep
	modeV3Auto
)

func (m processMode) String() string {
	switch m {
	case modeCurrent:
		return "current"
	case modeImproved:
		return "improved"
	case modeDryRun:
		return "dryrun"
	case modeV3:
		return "v3"
	case modeV3Fallback:
		return "v3-fallback"
	case modeV3Retry:
		return "v3-retry"
	case modeV3Deep:
		return "v3-deep"
	case modeV3Auto:
		return "v3-auto"
	default:
		return "unknown"
	}
}

type benchEventKind uint8

const (
	eventOrderFilled benchEventKind = iota
	eventNegRiskOrderFilled
	eventFPMMBuy
	eventFPMMSell
	eventFPMMFundingAdded
	eventFPMMFundingRemoved
	eventPositionSplit
	eventPositionsMerge
	eventPayoutRedemptionCTF
	eventNegRiskPositionSplit
	eventNegRiskPositionsMerge
	eventPositionsConverted
	eventPayoutRedemptionNR
	eventConditionResolution
	eventQuestionPrepared
)

func (k benchEventKind) String() string {
	switch k {
	case eventOrderFilled:
		return "OrderFilled"
	case eventNegRiskOrderFilled:
		return "NegRiskOrderFilled"
	case eventFPMMBuy:
		return "FPMMBuy"
	case eventFPMMSell:
		return "FPMMSell"
	case eventFPMMFundingAdded:
		return "FPMMFundingAdded"
	case eventFPMMFundingRemoved:
		return "FPMMFundingRemoved"
	case eventPositionSplit:
		return "PositionSplit"
	case eventPositionsMerge:
		return "PositionsMerge"
	case eventPayoutRedemptionCTF:
		return "PayoutRedemptionCTF"
	case eventNegRiskPositionSplit:
		return "NegRiskPositionSplit"
	case eventNegRiskPositionsMerge:
		return "NegRiskPositionsMerge"
	case eventPositionsConverted:
		return "PositionsConverted"
	case eventPayoutRedemptionNR:
		return "PayoutRedemptionNR"
	case eventConditionResolution:
		return "ConditionResolution"
	case eventQuestionPrepared:
		return "QuestionPrepared"
	default:
		return "Unknown"
	}
}

type benchPositionKey struct {
	user  [20]byte
	token [32]byte
}

type benchPosition struct {
	key            benchPositionKey
	amount         int64
	totalBought    int64
	avgPrice       int64
	realizedPnL    int64
	updatedAtBlock uint64
	blockNumber    uint64
	txIndex        uint64
	logIndex       uint64
}

type benchCondition struct {
	id               [32]byte
	outcomeSlotCount uint8
	resolved         bool
	payout0          uint64
	payout1          uint64
	blockNumber      uint64
	txIndex          uint64
	logIndex         uint64
}

type benchFPMM struct {
	id          [20]byte
	conditionID [32]byte
	collateral  [20]byte
	blockNumber uint64
	txIndex     uint64
	logIndex    uint64
}

type benchNegRiskEvent struct {
	id            [32]byte
	questionCount uint32
	questionIDs   [4][32]byte
	blockNumber   uint64
	txIndex       uint64
	logIndex      uint64
}

type benchCryptoEvent struct {
	kind              benchEventKind
	user              [20]byte
	taker             [20]byte
	makerAssetID      [32]byte
	takerAssetID      [32]byte
	makerAmountFilled uint64
	takerAmountFilled uint64
	conditionID       [32]byte
	collateral        [20]byte
	fpmm              [20]byte
	marketID          [32]byte
	questionID        [32]byte
	questionIndex     uint32
	outcomeIndex      uint8
	indexSet          uint64
	amount            uint64
	amounts           [2]uint64
	payouts           [2]uint64
	blockNumber       uint64
	txIndex           uint64
	logIndex          uint64
}

func (e benchCryptoEvent) meta() benchMeta {
	return benchMeta{
		blockNumber: e.blockNumber,
		txIndex:     e.txIndex,
		logIndex:    e.logIndex,
	}
}

type benchMeta struct {
	blockNumber uint64
	txIndex     uint64
	logIndex    uint64
}

type benchKeySet struct {
	positions  map[benchPositionKey]struct{}
	conditions map[[32]byte]struct{}
	fpmms      map[[20]byte]struct{}
	negRisk    map[[32]byte]struct{}
}

func newBenchKeySet() benchKeySet {
	return benchKeySet{
		positions:  make(map[benchPositionKey]struct{}),
		conditions: make(map[[32]byte]struct{}),
		fpmms:      make(map[[20]byte]struct{}),
		negRisk:    make(map[[32]byte]struct{}),
	}
}

func (s benchKeySet) total() int {
	return len(s.positions) + len(s.conditions) + len(s.fpmms) + len(s.negRisk)
}

type benchState struct {
	conn         *ch.Client
	db           string
	mode         processMode
	resolveChunk int
	threshold    int

	positions  map[benchPositionKey]benchPosition
	conditions map[[32]byte]benchCondition
	fpmms      map[[20]byte]benchFPMM
	negRisk    map[[32]byte]benchNegRiskEvent

	dirtyPositions  map[benchPositionKey]struct{}
	dirtyConditions map[[32]byte]struct{}
	dirtyFPMMs      map[[20]byte]struct{}
	dirtyNegRisk    map[[32]byte]struct{}

	absentPositions  map[benchPositionKey]struct{}
	absentConditions map[[32]byte]struct{}
	absentFPMMs      map[[20]byte]struct{}
	absentNegRisk    map[[32]byte]struct{}

	dryRunMode bool
	dryRunKeys benchKeySet
	pending    benchKeySet
	retryMiss  bool

	// v3-auto generic discovery: reads fall through overlay -> cache ->
	// known-absent and otherwise get tracked into `discovered`; writes go to
	// the overlay and are discarded at the end of each discovery round.
	discovery         bool
	discovered        benchKeySet
	overlayPositions  map[benchPositionKey]benchPosition
	overlayConditions map[[32]byte]benchCondition
	overlayFPMMs      map[[20]byte]benchFPMM
	overlayNegRisk    map[[32]byte]benchNegRiskEvent

	mu             sync.Mutex
	gets           uint64
	saves          uint64
	hotHits        uint64
	coldMisses     uint64
	resolveQueries uint64
	resolvedRows   uint64
}

type benchStateSnapshot struct {
	positions        map[benchPositionKey]benchPosition
	conditions       map[[32]byte]benchCondition
	fpmms            map[[20]byte]benchFPMM
	negRisk          map[[32]byte]benchNegRiskEvent
	dirtyPositions   map[benchPositionKey]struct{}
	dirtyConditions  map[[32]byte]struct{}
	dirtyFPMMs       map[[20]byte]struct{}
	dirtyNegRisk     map[[32]byte]struct{}
	absentPositions  map[benchPositionKey]struct{}
	absentConditions map[[32]byte]struct{}
	absentFPMMs      map[[20]byte]struct{}
	absentNegRisk    map[[32]byte]struct{}
}

func newBenchState(conn *ch.Client, db string, mode processMode, resolveChunk, threshold int) *benchState {
	if threshold <= 0 {
		threshold = 100
	}
	return &benchState{
		conn:             conn,
		db:               db,
		mode:             mode,
		resolveChunk:     resolveChunk,
		threshold:        threshold,
		positions:        make(map[benchPositionKey]benchPosition),
		conditions:       make(map[[32]byte]benchCondition),
		fpmms:            make(map[[20]byte]benchFPMM),
		negRisk:          make(map[[32]byte]benchNegRiskEvent),
		dirtyPositions:   make(map[benchPositionKey]struct{}),
		dirtyConditions:  make(map[[32]byte]struct{}),
		dirtyFPMMs:       make(map[[20]byte]struct{}),
		dirtyNegRisk:     make(map[[32]byte]struct{}),
		absentPositions:  make(map[benchPositionKey]struct{}),
		absentConditions: make(map[[32]byte]struct{}),
		absentFPMMs:      make(map[[20]byte]struct{}),
		absentNegRisk:    make(map[[32]byte]struct{}),
		dryRunKeys:       newBenchKeySet(),
		pending:          newBenchKeySet(),
		discovered:       newBenchKeySet(),
	}
}

func (s *benchState) snapshot() benchStateSnapshot {
	return benchStateSnapshot{
		positions:        cloneMap(s.positions),
		conditions:       cloneMap(s.conditions),
		fpmms:            cloneMap(s.fpmms),
		negRisk:          cloneMap(s.negRisk),
		dirtyPositions:   cloneMap(s.dirtyPositions),
		dirtyConditions:  cloneMap(s.dirtyConditions),
		dirtyFPMMs:       cloneMap(s.dirtyFPMMs),
		dirtyNegRisk:     cloneMap(s.dirtyNegRisk),
		absentPositions:  cloneMap(s.absentPositions),
		absentConditions: cloneMap(s.absentConditions),
		absentFPMMs:      cloneMap(s.absentFPMMs),
		absentNegRisk:    cloneMap(s.absentNegRisk),
	}
}

func (s *benchState) restore(snapshot benchStateSnapshot) {
	s.positions = snapshot.positions
	s.conditions = snapshot.conditions
	s.fpmms = snapshot.fpmms
	s.negRisk = snapshot.negRisk
	s.dirtyPositions = snapshot.dirtyPositions
	s.dirtyConditions = snapshot.dirtyConditions
	s.dirtyFPMMs = snapshot.dirtyFPMMs
	s.dirtyNegRisk = snapshot.dirtyNegRisk
	s.absentPositions = snapshot.absentPositions
	s.absentConditions = snapshot.absentConditions
	s.absentFPMMs = snapshot.absentFPMMs
	s.absentNegRisk = snapshot.absentNegRisk
	s.retryMiss = false
	s.pending = newBenchKeySet()
}

func cloneMap[K comparable, V any](src map[K]V) map[K]V {
	dst := make(map[K]V, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func (s *benchState) EnterDryRunMode() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dryRunMode = true
	s.dryRunKeys = newBenchKeySet()
}

func (s *benchState) ExitDryRunMode() benchKeySet {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := s.dryRunKeys
	s.dryRunMode = false
	s.dryRunKeys = newBenchKeySet()
	return keys
}

func (s *benchState) inDryRun() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dryRunMode
}

func (s *benchState) trackPosition(key benchPositionKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dryRunMode {
		s.dryRunKeys.positions[key] = struct{}{}
	}
}

func (s *benchState) trackCondition(id [32]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dryRunMode {
		s.dryRunKeys.conditions[id] = struct{}{}
	}
}

func (s *benchState) trackFPMM(id [20]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dryRunMode {
		s.dryRunKeys.fpmms[id] = struct{}{}
	}
}

func (s *benchState) trackNegRisk(id [32]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dryRunMode {
		s.dryRunKeys.negRisk[id] = struct{}{}
	}
}

func (s *benchState) usesDeferredGets() bool {
	return s.mode == modeV3 || s.mode == modeV3Fallback || s.mode == modeV3Retry
}

func (s *benchState) usesKnownAbsentCache() bool {
	return s.mode == modeV3 || s.mode == modeV3Fallback || s.mode == modeV3Retry || s.mode == modeV3Deep || s.mode == modeV3Auto
}

func (s *benchState) GetPosition(ctx context.Context, key benchPositionKey) (*benchPosition, bool, error) {
	s.gets++
	if s.mode == modeDryRun && s.inDryRun() {
		s.trackPosition(key)
		return nil, false, nil
	}
	if s.mode == modeV3Auto && s.discovery {
		pos, ok := s.discoverPosition(key)
		return pos, ok, nil
	}
	if pos, ok := s.positions[key]; ok {
		s.hotHits++
		return &pos, true, nil
	}
	if s.usesKnownAbsentCache() {
		if _, ok := s.absentPositions[key]; ok {
			return nil, false, nil
		}
	}
	if s.mode == modeV3Fallback {
		return s.fallbackPosition(ctx, key)
	}
	if s.usesDeferredGets() {
		return s.deferPosition(ctx, key)
	}
	s.coldMisses++
	if err := s.resolvePositions(ctx, []benchPositionKey{key}); err != nil {
		return nil, false, err
	}
	if pos, ok := s.positions[key]; ok {
		return &pos, true, nil
	}
	return nil, false, nil
}

func (s *benchState) GetCondition(ctx context.Context, id [32]byte) (*benchCondition, bool, error) {
	s.gets++
	if s.mode == modeDryRun && s.inDryRun() {
		s.trackCondition(id)
		return nil, false, nil
	}
	if s.mode == modeV3Auto && s.discovery {
		cond, ok := s.discoverCondition(id)
		return cond, ok, nil
	}
	if cond, ok := s.conditions[id]; ok {
		s.hotHits++
		return &cond, true, nil
	}
	if s.usesKnownAbsentCache() {
		if _, ok := s.absentConditions[id]; ok {
			return nil, false, nil
		}
	}
	if s.mode == modeV3Fallback {
		return s.fallbackCondition(ctx, id)
	}
	if s.usesDeferredGets() {
		return s.deferCondition(ctx, id)
	}
	s.coldMisses++
	if err := s.resolveConditions(ctx, [][32]byte{id}); err != nil {
		return nil, false, err
	}
	if cond, ok := s.conditions[id]; ok {
		return &cond, true, nil
	}
	return nil, false, nil
}

func (s *benchState) GetFPMM(ctx context.Context, id [20]byte) (*benchFPMM, bool, error) {
	s.gets++
	if s.mode == modeDryRun && s.inDryRun() {
		s.trackFPMM(id)
		return nil, false, nil
	}
	if s.mode == modeV3Auto && s.discovery {
		fpmm, ok := s.discoverFPMM(id)
		return fpmm, ok, nil
	}
	if fpmm, ok := s.fpmms[id]; ok {
		s.hotHits++
		return &fpmm, true, nil
	}
	if s.usesKnownAbsentCache() {
		if _, ok := s.absentFPMMs[id]; ok {
			return nil, false, nil
		}
	}
	if s.mode == modeV3Fallback {
		return s.fallbackFPMM(ctx, id)
	}
	if s.usesDeferredGets() {
		return s.deferFPMM(ctx, id)
	}
	s.coldMisses++
	if err := s.resolveFPMMs(ctx, [][20]byte{id}); err != nil {
		return nil, false, err
	}
	if fpmm, ok := s.fpmms[id]; ok {
		return &fpmm, true, nil
	}
	return nil, false, nil
}

func (s *benchState) GetNegRisk(ctx context.Context, id [32]byte) (*benchNegRiskEvent, bool, error) {
	s.gets++
	if s.mode == modeDryRun && s.inDryRun() {
		s.trackNegRisk(id)
		return nil, false, nil
	}
	if s.mode == modeV3Auto && s.discovery {
		nr, ok := s.discoverNegRisk(id)
		return nr, ok, nil
	}
	if nr, ok := s.negRisk[id]; ok {
		s.hotHits++
		return &nr, true, nil
	}
	if s.usesKnownAbsentCache() {
		if _, ok := s.absentNegRisk[id]; ok {
			return nil, false, nil
		}
	}
	if s.mode == modeV3Fallback {
		return s.fallbackNegRisk(ctx, id)
	}
	if s.usesDeferredGets() {
		return s.deferNegRisk(ctx, id)
	}
	s.coldMisses++
	if err := s.resolveNegRisk(ctx, [][32]byte{id}); err != nil {
		return nil, false, err
	}
	if nr, ok := s.negRisk[id]; ok {
		return &nr, true, nil
	}
	return nil, false, nil
}

// discoverPosition serves a Get during a v3-auto discovery round. Writes from
// earlier events in the same round (overlay) take precedence over the hot
// cache so save-then-read chains are visible to discovery; keys confirmed
// missing in storage (known-absent) behave like a real miss; everything else
// is tracked for the next batch resolve.
func (s *benchState) discoverPosition(key benchPositionKey) (*benchPosition, bool) {
	if pos, ok := s.overlayPositions[key]; ok {
		s.hotHits++
		return &pos, true
	}
	if pos, ok := s.positions[key]; ok {
		s.hotHits++
		return &pos, true
	}
	if _, ok := s.absentPositions[key]; ok {
		return nil, false
	}
	if _, ok := s.discovered.positions[key]; !ok {
		s.discovered.positions[key] = struct{}{}
		s.coldMisses++
	}
	return nil, false
}

func (s *benchState) discoverCondition(id [32]byte) (*benchCondition, bool) {
	if cond, ok := s.overlayConditions[id]; ok {
		s.hotHits++
		return &cond, true
	}
	if cond, ok := s.conditions[id]; ok {
		s.hotHits++
		return &cond, true
	}
	if _, ok := s.absentConditions[id]; ok {
		return nil, false
	}
	if _, ok := s.discovered.conditions[id]; !ok {
		s.discovered.conditions[id] = struct{}{}
		s.coldMisses++
	}
	return nil, false
}

func (s *benchState) discoverFPMM(id [20]byte) (*benchFPMM, bool) {
	if fpmm, ok := s.overlayFPMMs[id]; ok {
		s.hotHits++
		return &fpmm, true
	}
	if fpmm, ok := s.fpmms[id]; ok {
		s.hotHits++
		return &fpmm, true
	}
	if _, ok := s.absentFPMMs[id]; ok {
		return nil, false
	}
	if _, ok := s.discovered.fpmms[id]; !ok {
		s.discovered.fpmms[id] = struct{}{}
		s.coldMisses++
	}
	return nil, false
}

func (s *benchState) discoverNegRisk(id [32]byte) (*benchNegRiskEvent, bool) {
	if nr, ok := s.overlayNegRisk[id]; ok {
		s.hotHits++
		return &nr, true
	}
	if nr, ok := s.negRisk[id]; ok {
		s.hotHits++
		return &nr, true
	}
	if _, ok := s.absentNegRisk[id]; ok {
		return nil, false
	}
	if _, ok := s.discovered.negRisk[id]; !ok {
		s.discovered.negRisk[id] = struct{}{}
		s.coldMisses++
	}
	return nil, false
}

func (s *benchState) SavePosition(pos *benchPosition, meta benchMeta) {
	if pos == nil || s.inDryRun() {
		return
	}
	pos.updatedAtBlock = meta.blockNumber
	pos.blockNumber = meta.blockNumber
	pos.txIndex = meta.txIndex
	pos.logIndex = meta.logIndex
	if s.discovery {
		s.overlayPositions[pos.key] = *pos
		return
	}
	s.saves++
	s.positions[pos.key] = *pos
	s.dirtyPositions[pos.key] = struct{}{}
	delete(s.absentPositions, pos.key)
	if s.mode != modeV3Retry {
		s.mu.Lock()
		if _, ok := s.pending.positions[pos.key]; ok {
			delete(s.pending.positions, pos.key)
		}
		s.mu.Unlock()
	}
}

func (s *benchState) SaveCondition(cond *benchCondition, meta benchMeta) {
	if cond == nil || s.inDryRun() {
		return
	}
	cond.blockNumber = meta.blockNumber
	cond.txIndex = meta.txIndex
	cond.logIndex = meta.logIndex
	if s.discovery {
		s.overlayConditions[cond.id] = *cond
		return
	}
	s.saves++
	s.conditions[cond.id] = *cond
	s.dirtyConditions[cond.id] = struct{}{}
	delete(s.absentConditions, cond.id)
	if s.mode != modeV3Retry {
		s.mu.Lock()
		if _, ok := s.pending.conditions[cond.id]; ok {
			delete(s.pending.conditions, cond.id)
		}
		s.mu.Unlock()
	}
}

func (s *benchState) SaveFPMM(fpmm *benchFPMM, meta benchMeta) {
	if fpmm == nil || s.inDryRun() {
		return
	}
	fpmm.blockNumber = meta.blockNumber
	fpmm.txIndex = meta.txIndex
	fpmm.logIndex = meta.logIndex
	if s.discovery {
		s.overlayFPMMs[fpmm.id] = *fpmm
		return
	}
	s.saves++
	s.fpmms[fpmm.id] = *fpmm
	s.dirtyFPMMs[fpmm.id] = struct{}{}
	delete(s.absentFPMMs, fpmm.id)
	if s.mode != modeV3Retry {
		s.mu.Lock()
		if _, ok := s.pending.fpmms[fpmm.id]; ok {
			delete(s.pending.fpmms, fpmm.id)
		}
		s.mu.Unlock()
	}
}

func (s *benchState) SaveNegRisk(nr *benchNegRiskEvent, meta benchMeta) {
	if nr == nil || s.inDryRun() {
		return
	}
	nr.blockNumber = meta.blockNumber
	nr.txIndex = meta.txIndex
	nr.logIndex = meta.logIndex
	if s.discovery {
		s.overlayNegRisk[nr.id] = *nr
		return
	}
	s.saves++
	s.negRisk[nr.id] = *nr
	s.dirtyNegRisk[nr.id] = struct{}{}
	delete(s.absentNegRisk, nr.id)
	if s.mode != modeV3Retry {
		s.mu.Lock()
		if _, ok := s.pending.negRisk[nr.id]; ok {
			delete(s.pending.negRisk, nr.id)
		}
		s.mu.Unlock()
	}
}

func (s *benchState) deferPosition(ctx context.Context, key benchPositionKey) (*benchPosition, bool, error) {
	shouldFlush := s.queuePendingPosition(key)
	if s.mode == modeV3Retry {
		s.retryMiss = true
		return nil, false, nil
	}
	if shouldFlush {
		if err := s.Flush(ctx); err != nil {
			return nil, false, err
		}
		if pos, ok := s.positions[key]; ok {
			return &pos, true, nil
		}
	}
	return nil, false, nil
}

func (s *benchState) deferCondition(ctx context.Context, id [32]byte) (*benchCondition, bool, error) {
	shouldFlush := s.queuePendingCondition(id)
	if s.mode == modeV3Retry {
		s.retryMiss = true
		return nil, false, nil
	}
	if shouldFlush {
		if err := s.Flush(ctx); err != nil {
			return nil, false, err
		}
		if cond, ok := s.conditions[id]; ok {
			return &cond, true, nil
		}
	}
	return nil, false, nil
}

func (s *benchState) deferFPMM(ctx context.Context, id [20]byte) (*benchFPMM, bool, error) {
	shouldFlush := s.queuePendingFPMM(id)
	if s.mode == modeV3Retry {
		s.retryMiss = true
		return nil, false, nil
	}
	if shouldFlush {
		if err := s.Flush(ctx); err != nil {
			return nil, false, err
		}
		if fpmm, ok := s.fpmms[id]; ok {
			return &fpmm, true, nil
		}
	}
	return nil, false, nil
}

func (s *benchState) deferNegRisk(ctx context.Context, id [32]byte) (*benchNegRiskEvent, bool, error) {
	shouldFlush := s.queuePendingNegRisk(id)
	if s.mode == modeV3Retry {
		s.retryMiss = true
		return nil, false, nil
	}
	if shouldFlush {
		if err := s.Flush(ctx); err != nil {
			return nil, false, err
		}
		if nr, ok := s.negRisk[id]; ok {
			return &nr, true, nil
		}
	}
	return nil, false, nil
}

func (s *benchState) fallbackPosition(ctx context.Context, key benchPositionKey) (*benchPosition, bool, error) {
	if _, _, err := s.deferPosition(ctx, key); err != nil {
		return nil, false, err
	}
	if pos, ok := s.positions[key]; ok {
		return &pos, true, nil
	}
	if _, ok := s.absentPositions[key]; ok {
		return nil, false, nil
	}
	if err := s.resolvePositions(ctx, []benchPositionKey{key}); err != nil {
		return nil, false, err
	}
	s.removePendingPosition(key)
	if pos, ok := s.positions[key]; ok {
		return &pos, true, nil
	}
	return nil, false, nil
}

func (s *benchState) fallbackCondition(ctx context.Context, id [32]byte) (*benchCondition, bool, error) {
	if _, _, err := s.deferCondition(ctx, id); err != nil {
		return nil, false, err
	}
	if cond, ok := s.conditions[id]; ok {
		return &cond, true, nil
	}
	if _, ok := s.absentConditions[id]; ok {
		return nil, false, nil
	}
	if err := s.resolveConditions(ctx, [][32]byte{id}); err != nil {
		return nil, false, err
	}
	s.removePendingCondition(id)
	if cond, ok := s.conditions[id]; ok {
		return &cond, true, nil
	}
	return nil, false, nil
}

func (s *benchState) fallbackFPMM(ctx context.Context, id [20]byte) (*benchFPMM, bool, error) {
	if _, _, err := s.deferFPMM(ctx, id); err != nil {
		return nil, false, err
	}
	if fpmm, ok := s.fpmms[id]; ok {
		return &fpmm, true, nil
	}
	if _, ok := s.absentFPMMs[id]; ok {
		return nil, false, nil
	}
	if err := s.resolveFPMMs(ctx, [][20]byte{id}); err != nil {
		return nil, false, err
	}
	s.removePendingFPMM(id)
	if fpmm, ok := s.fpmms[id]; ok {
		return &fpmm, true, nil
	}
	return nil, false, nil
}

func (s *benchState) fallbackNegRisk(ctx context.Context, id [32]byte) (*benchNegRiskEvent, bool, error) {
	if _, _, err := s.deferNegRisk(ctx, id); err != nil {
		return nil, false, err
	}
	if nr, ok := s.negRisk[id]; ok {
		return &nr, true, nil
	}
	if _, ok := s.absentNegRisk[id]; ok {
		return nil, false, nil
	}
	if err := s.resolveNegRisk(ctx, [][32]byte{id}); err != nil {
		return nil, false, err
	}
	s.removePendingNegRisk(id)
	if nr, ok := s.negRisk[id]; ok {
		return &nr, true, nil
	}
	return nil, false, nil
}

func (s *benchState) queuePendingPosition(key benchPositionKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pending.positions[key]; !ok {
		s.pending.positions[key] = struct{}{}
		s.coldMisses++
	}
	return s.pending.total() >= s.threshold
}

func (s *benchState) queuePendingCondition(id [32]byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pending.conditions[id]; !ok {
		s.pending.conditions[id] = struct{}{}
		s.coldMisses++
	}
	return s.pending.total() >= s.threshold
}

func (s *benchState) queuePendingFPMM(id [20]byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pending.fpmms[id]; !ok {
		s.pending.fpmms[id] = struct{}{}
		s.coldMisses++
	}
	return s.pending.total() >= s.threshold
}

func (s *benchState) queuePendingNegRisk(id [32]byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pending.negRisk[id]; !ok {
		s.pending.negRisk[id] = struct{}{}
		s.coldMisses++
	}
	return s.pending.total() >= s.threshold
}

func (s *benchState) removePendingPosition(key benchPositionKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending.positions, key)
}

func (s *benchState) removePendingCondition(id [32]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending.conditions, id)
}

func (s *benchState) removePendingFPMM(id [20]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending.fpmms, id)
}

func (s *benchState) removePendingNegRisk(id [32]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending.negRisk, id)
}

func (s *benchState) takePending() benchKeySet {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.pending
	s.pending = newBenchKeySet()
	return pending
}

func (s *benchState) Flush(ctx context.Context) error {
	pending := s.takePending()
	return s.resolveKeySet(ctx, pending)
}

func (s *benchState) resolveKeySet(ctx context.Context, pending benchKeySet) error {
	if len(pending.conditions) > 0 {
		if err := s.resolveConditions(ctx, sortedHashKeys(pending.conditions)); err != nil {
			return err
		}
	}
	if len(pending.fpmms) > 0 {
		if err := s.resolveFPMMs(ctx, sortedAddressKeys(pending.fpmms)); err != nil {
			return err
		}
	}
	if len(pending.negRisk) > 0 {
		if err := s.resolveNegRisk(ctx, sortedHashKeys(pending.negRisk)); err != nil {
			return err
		}
	}
	if len(pending.positions) > 0 {
		if err := s.resolvePositions(ctx, sortedPositionKeys(pending.positions)); err != nil {
			return err
		}
	}
	return nil
}

// RunPrefetchModesBenchmark runs the requested approaches.
func RunPrefetchModesBenchmark(ctx context.Context, cfg PrefetchModeConfig) error {
	cfg = normalizePrefetchConfig(cfg)

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

	if err := ensureBenchmarkTables(ctx, conn, cfg.Database); err != nil {
		return err
	}

	events := newBenchmarkEvents(cfg)
	modes, err := parsePrefetchModes(cfg)
	if err != nil {
		return err
	}

	results := make([]PrefetchModeResult, 0, len(modes))
	for _, mode := range modes {
		log.Printf("[BENCH] Running mode=%s positions=%d events=%d", mode, cfg.Positions, cfg.Events)
		if err := resetBenchmarkTables(ctx, conn, cfg.Database); err != nil {
			return err
		}
		if err := populateBenchmarkTables(ctx, conn, cfg, events); err != nil {
			return err
		}

		result, err := runPrefetchMode(ctx, cfg, conn, events, mode)
		if err != nil {
			return fmt.Errorf("mode %s failed: %w", mode, err)
		}
		result.Mode = mode.String()
		results = append(results, result)
	}

	reportPrefetchModeResults(results)
	return nil
}

func normalizePrefetchConfig(cfg PrefetchModeConfig) PrefetchModeConfig {
	if cfg.Positions <= 0 {
		cfg.Positions = 1000
	}
	if cfg.Events <= 0 {
		cfg.Events = 10000
	}
	if cfg.ResolveChunk <= 0 {
		cfg.ResolveChunk = 500
	}
	if cfg.PrefetchBatch <= 0 {
		cfg.PrefetchBatch = 1000
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = 100
	}
	if cfg.Users <= 0 {
		cfg.Users = benchMax(32, cfg.Positions/8)
	}
	if cfg.Conditions <= 0 {
		cfg.Conditions = benchMax(16, cfg.Positions/20)
	}
	if cfg.FPMMs <= 0 {
		cfg.FPMMs = benchMax(8, cfg.Conditions/4)
	}
	if cfg.Markets <= 0 {
		cfg.Markets = benchMax(4, cfg.Conditions/8)
	}
	if strings.TrimSpace(cfg.Modes) == "" {
		cfg.Modes = "all"
	}
	return cfg
}

func parsePrefetchModes(cfg PrefetchModeConfig) ([]processMode, error) {
	if cfg.V3 {
		return []processMode{modeCurrent, modeV3, modeV3Fallback, modeV3Retry}, nil
	}

	text := strings.TrimSpace(strings.ToLower(cfg.Modes))
	if text == "" || text == "all" {
		return []processMode{modeCurrent, modeImproved, modeDryRun, modeV3, modeV3Fallback, modeV3Retry, modeV3Deep, modeV3Auto}, nil
	}

	seen := make(map[processMode]struct{})
	var modes []processMode
	for _, part := range strings.Split(text, ",") {
		part = strings.TrimSpace(part)
		var mode processMode
		switch part {
		case "current", "baseline":
			mode = modeCurrent
		case "improved", "manual", "prefetch":
			mode = modeImproved
		case "dryrun", "dry-run":
			mode = modeDryRun
		case "v3", "deferred", "deferred-get", "deferred-gets":
			mode = modeV3
		case "v3-fallback", "fallback", "deferred-fallback":
			mode = modeV3Fallback
		case "v3-retry", "retry", "suspend", "suspend-retry":
			mode = modeV3Retry
		case "v3-deep", "deep", "batch-prefetch":
			mode = modeV3Deep
		case "v3-auto", "auto", "generic", "v3-generic":
			mode = modeV3Auto
		default:
			return nil, fmt.Errorf("unknown prefetch mode %q", part)
		}
		if _, ok := seen[mode]; ok {
			continue
		}
		seen[mode] = struct{}{}
		modes = append(modes, mode)
	}
	return modes, nil
}

func runPrefetchMode(ctx context.Context, cfg PrefetchModeConfig, conn *ch.Client, events []benchCryptoEvent, mode processMode) (PrefetchModeResult, error) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	state := newBenchState(conn, cfg.Database, mode, cfg.ResolveChunk, cfg.Threshold)
	var handlerExecs uint64
	handler := func(ev benchCryptoEvent) error {
		handlerExecs++
		return processCryptoEvent(ctx, state, ev)
	}

	start := time.Now()
	var err error
	switch mode {
	case modeCurrent:
		err = runCurrentMode(ctx, state, events, handler)
	case modeImproved:
		err = runImprovedMode(ctx, state, events, cfg.PrefetchBatch, handler)
	case modeDryRun:
		err = runDryRunMode(ctx, state, events, cfg.PrefetchBatch, handler)
	case modeV3:
		err = runV3Mode(ctx, state, events, handler)
	case modeV3Fallback:
		err = runV3FallbackMode(ctx, state, events, handler)
	case modeV3Retry:
		err = runV3RetryMode(ctx, state, events, handler)
	case modeV3Deep:
		err = runV3DeepMode(ctx, state, events, handler)
	case modeV3Auto:
		err = runV3AutoMode(ctx, state, events, cfg.PrefetchBatch, handler)
	default:
		err = fmt.Errorf("unsupported mode %v", mode)
	}
	duration := time.Since(start)
	if flushErr := state.Flush(ctx); err == nil && flushErr != nil {
		err = flushErr
	}
	runtime.ReadMemStats(&after)

	return PrefetchModeResult{
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
		HandlerExecs:   handlerExecs,
		FinalHash:      state.checksumDirtyState(),
	}, err
}

func runCurrentMode(_ context.Context, _ *benchState, events []benchCryptoEvent, handler func(benchCryptoEvent) error) error {
	for _, ev := range events {
		if err := handler(ev); err != nil {
			return err
		}
	}
	return nil
}

func runImprovedMode(ctx context.Context, state *benchState, events []benchCryptoEvent, batchSize int, handler func(benchCryptoEvent) error) error {
	for start := 0; start < len(events); start += batchSize {
		end := benchMin(start+batchSize, len(events))
		batch := events[start:end]
		if err := prefetchPrimaryState(ctx, state, batch); err != nil {
			return err
		}
		if err := prefetchPositionState(ctx, state, batch); err != nil {
			return err
		}
		for _, ev := range batch {
			if err := handler(ev); err != nil {
				return err
			}
		}
	}
	return nil
}

func runDryRunMode(ctx context.Context, state *benchState, events []benchCryptoEvent, batchSize int, handler func(benchCryptoEvent) error) error {
	for start := 0; start < len(events); start += batchSize {
		end := benchMin(start+batchSize, len(events))
		batch := events[start:end]

		state.EnterDryRunMode()
		for _, ev := range batch {
			if err := handler(ev); err != nil {
				_ = state.ExitDryRunMode()
				return err
			}
		}
		discovered := state.ExitDryRunMode()

		if err := state.prefetchKeys(ctx, discovered); err != nil {
			return err
		}
		for _, ev := range batch {
			if err := handler(ev); err != nil {
				return err
			}
		}
	}
	return nil
}

func runV3Mode(_ context.Context, _ *benchState, events []benchCryptoEvent, handler func(benchCryptoEvent) error) error {
	for _, ev := range events {
		if err := handler(ev); err != nil {
			return err
		}
	}
	return nil
}

func runV3FallbackMode(_ context.Context, _ *benchState, events []benchCryptoEvent, handler func(benchCryptoEvent) error) error {
	for _, ev := range events {
		if err := handler(ev); err != nil {
			return err
		}
	}
	return nil
}

func runV3RetryMode(ctx context.Context, state *benchState, events []benchCryptoEvent, handler func(benchCryptoEvent) error) error {
	const maxAttempts = 8

	for _, ev := range events {
		for attempt := 0; attempt < maxAttempts; attempt++ {
			snapshot := state.snapshot()
			state.retryMiss = false
			if err := handler(ev); err != nil {
				return err
			}
			if !state.retryMiss {
				break
			}

			pending := state.takePending()
			state.restore(snapshot)
			if err := state.resolveKeySet(ctx, pending); err != nil {
				return err
			}
			if attempt == maxAttempts-1 {
				return fmt.Errorf("v3 retry exceeded %d attempts at block=%d log=%d", maxAttempts, ev.blockNumber, ev.logIndex)
			}
		}
	}
	return nil
}

// runV3DeepMode resolves ALL primary state across all events in a single pass,
// then computes ALL position keys from resolved primary state and resolves those,
// then runs handlers with cache-hot state.  No per-batch overhead, no retry
// snapshots, no deferred gets — just two rounds of batch ClickHouse queries and
// a single pass of handler execution.
func runV3DeepMode(ctx context.Context, state *benchState, events []benchCryptoEvent, handler func(benchCryptoEvent) error) error {
	// Phase 1: resolve ALL primary state (conditions, fpmms, negRisk)
	if err := prefetchPrimaryState(ctx, state, events); err != nil {
		return err
	}

	// Phase 2: with primary state now in memory, compute ALL position keys
	// that any handler could access, then resolve them in one batch
	if err := prefetchPositionState(ctx, state, events); err != nil {
		return err
	}

	// Phase 3: run handlers — every Get hits the pre-populated cache
	for _, ev := range events {
		if err := handler(ev); err != nil {
			return err
		}
	}
	return nil
}

// runV3AutoMode is the generic version of v3-deep: it converges to the same
// "resolve everything first, then run cache-hot" execution without any
// hand-written, per-event key extraction. The user's own handlers are the key
// extractor, so the indexer needs zero schema- or event-specific prefetch
// code and zero configuration beyond the window size.
//
// Each prefetch window goes through discovery rounds:
//
//   round: execute every handler in discovery mode
//     reads: overlay -> hot cache -> known-absent -> track as missing
//     writes: overlay only, discarded at the end of the round
//   batch-resolve everything tracked; keys not found in storage are
//   remembered as known-absent so they are never re-queried
//   repeat until a round tracks nothing new — one round per read-dependency
//   level (round 1 resolves conditions/FPMMs/markets, round 2 the position
//   keys derived from them,...), so convergence is fast and bounded.
//
// The overlay makes save-then-read chains inside a window visible to
// discovery (e.g. QuestionPrepared -> PositionsConverted on the same market,
// or NegRiskPositionSplit creating a condition a later event reads), which
// the plain dryrun mode cannot see.
//
// Correctness does not depend on discovery being complete. Once a round
// tracks nothing new, that round served every read from overlay/cache/absent,
// i.e. it executed branch-for-branch what the real pass will execute, so the
// real pass is fully cache-served. If discovery is cut off by the round cap
// (or a handler reads keys non-deterministically), the real pass simply falls
// back to synchronous resolves on a miss — slower, never wrong. State written
// by the real pass is therefore identical to the baseline and the dirty-state
// hash matches by construction.
func runV3AutoMode(ctx context.Context, state *benchState, events []benchCryptoEvent, batchSize int, handler func(benchCryptoEvent) error) error {
	const maxDiscoveryRounds = 8
	for start := 0; start < len(events); start += batchSize {
		end := benchMin(start+batchSize, len(events))
		batch := events[start:end]

		for round := 0; ; round++ {
			if round >= maxDiscoveryRounds {
				log.Printf("[v3-auto] window %d-%d: discovery did not converge after %d rounds; real pass will resolve remaining misses synchronously", start, end, maxDiscoveryRounds)
				break
			}
			state.beginDiscoveryRound()
			for _, ev := range batch {
				// Discovery is best effort: handler errors are surfaced by
				// the real pass below, which runs with identical state.
				_ = handler(ev)
			}
			discovered := state.endDiscoveryRound()
			if discovered.total() == 0 {
				break
			}
			if err := state.resolveKeySet(ctx, discovered); err != nil {
				return err
			}
		}

		for _, ev := range batch {
			if err := handler(ev); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *benchState) beginDiscoveryRound() {
	s.discovery = true
	s.discovered = newBenchKeySet()
	s.overlayPositions = make(map[benchPositionKey]benchPosition)
	s.overlayConditions = make(map[[32]byte]benchCondition)
	s.overlayFPMMs = make(map[[20]byte]benchFPMM)
	s.overlayNegRisk = make(map[[32]byte]benchNegRiskEvent)
}

func (s *benchState) endDiscoveryRound() benchKeySet {
	s.discovery = false
	keys := s.discovered
	s.discovered = newBenchKeySet()
	s.overlayPositions = nil
	s.overlayConditions = nil
	s.overlayFPMMs = nil
	s.overlayNegRisk = nil
	return keys
}

func (s *benchState) prefetchKeys(ctx context.Context, keys benchKeySet) error {
	if len(keys.conditions) > 0 {
		ids := filterMissingHashKeys(keys.conditions, s.conditions)
		s.coldMisses += uint64(len(ids))
		if err := s.resolveConditions(ctx, ids); err != nil {
			return err
		}
	}
	if len(keys.fpmms) > 0 {
		ids := filterMissingAddressKeys(keys.fpmms, s.fpmms)
		s.coldMisses += uint64(len(ids))
		if err := s.resolveFPMMs(ctx, ids); err != nil {
			return err
		}
	}
	if len(keys.negRisk) > 0 {
		ids := filterMissingHashKeys(keys.negRisk, s.negRisk)
		s.coldMisses += uint64(len(ids))
		if err := s.resolveNegRisk(ctx, ids); err != nil {
			return err
		}
	}
	if len(keys.positions) > 0 {
		ids := filterMissingPositionKeys(keys.positions, s.positions)
		s.coldMisses += uint64(len(ids))
		if err := s.resolvePositions(ctx, ids); err != nil {
			return err
		}
	}
	return nil
}

func prefetchPrimaryState(ctx context.Context, state *benchState, events []benchCryptoEvent) error {
	keys := newBenchKeySet()
	for _, ev := range events {
		switch ev.kind {
		case eventPositionSplit, eventPositionsMerge, eventPayoutRedemptionCTF,
			eventNegRiskPositionSplit, eventNegRiskPositionsMerge, eventPayoutRedemptionNR,
			eventConditionResolution:
			keys.conditions[ev.conditionID] = struct{}{}
		case eventFPMMBuy, eventFPMMSell, eventFPMMFundingAdded, eventFPMMFundingRemoved:
			keys.fpmms[ev.fpmm] = struct{}{}
		case eventPositionsConverted, eventQuestionPrepared:
			keys.negRisk[ev.marketID] = struct{}{}
		}
	}
	return state.prefetchKeys(ctx, keys)
}

func prefetchPositionState(ctx context.Context, state *benchState, events []benchCryptoEvent) error {
	keys := newBenchKeySet()
	for _, ev := range events {
		switch ev.kind {
		case eventOrderFilled, eventNegRiskOrderFilled:
			token := ev.makerAssetID
			if token == benchZeroHash {
				token = ev.takerAssetID
			}
			keys.positions[benchPositionKey{user: ev.user, token: token}] = struct{}{}

		case eventFPMMBuy:
			if fpmm, ok := state.fpmms[ev.fpmm]; ok && ev.amount > 0 {
				keys.positions[benchPositionKey{user: ev.user, token: fpmmPositionToken(fpmm, ev.outcomeIndex)}] = struct{}{}
			}
		case eventFPMMSell:
			if fpmm, ok := state.fpmms[ev.fpmm]; ok && ev.amount > 0 {
				keys.positions[benchPositionKey{user: ev.user, token: fpmmPositionToken(fpmm, ev.outcomeIndex)}] = struct{}{}
			}
		case eventFPMMFundingAdded:
			if fpmm, ok := state.fpmms[ev.fpmm]; ok && ev.amounts[0]+ev.amounts[1] > 0 {
				outcome := uint8(0)
				if ev.amounts[0] > ev.amounts[1] {
					outcome = 1
				}
				keys.positions[benchPositionKey{user: ev.user, token: fpmmPositionToken(fpmm, outcome)}] = struct{}{}
				keys.positions[benchPositionKey{user: ev.user, token: lpShareToken(fpmm.id)}] = struct{}{}
			}
		case eventFPMMFundingRemoved:
			if fpmm, ok := state.fpmms[ev.fpmm]; ok && ev.amounts[0]+ev.amounts[1] > 0 {
				for outcome := uint8(0); outcome < 2; outcome++ {
					keys.positions[benchPositionKey{user: ev.user, token: fpmmPositionToken(fpmm, outcome)}] = struct{}{}
				}
				keys.positions[benchPositionKey{user: ev.user, token: lpShareToken(fpmm.id)}] = struct{}{}
			}

		case eventPositionSplit, eventPositionsMerge:
			if cond, ok := state.conditions[ev.conditionID]; ok && cond.outcomeSlotCount == 2 {
				for outcome := uint8(0); outcome < 2; outcome++ {
					keys.positions[benchPositionKey{user: ev.user, token: ctfPositionToken(ev.collateral, ev.conditionID, outcome)}] = struct{}{}
				}
			}
		case eventPayoutRedemptionCTF:
			if cond, ok := state.conditions[ev.conditionID]; ok && cond.resolved {
				for outcome := uint8(0); outcome < 2; outcome++ {
					keys.positions[benchPositionKey{user: ev.user, token: ctfPositionToken(ev.collateral, ev.conditionID, outcome)}] = struct{}{}
				}
			}
		case eventNegRiskPositionSplit, eventNegRiskPositionsMerge:
			for outcome := uint8(0); outcome < 2; outcome++ {
				keys.positions[benchPositionKey{user: ev.user, token: negRiskPositionToken(ev.conditionID, outcome)}] = struct{}{}
			}
		case eventPositionsConverted:
			nr, ok := state.negRisk[ev.marketID]
			if !ok || nr.questionCount == 0 {
				continue
			}
			for q := uint32(0); q < nr.questionCount; q++ {
				for outcome := uint8(0); outcome < 2; outcome++ {
					token, ok := negRiskConvertedToken(nr, ev.marketID, q, outcome)
					if ok {
						keys.positions[benchPositionKey{user: ev.user, token: token}] = struct{}{}
					}
				}
			}
		case eventPayoutRedemptionNR:
			if cond, ok := state.conditions[ev.conditionID]; ok && cond.resolved {
				for outcome := uint8(0); outcome < 2; outcome++ {
					keys.positions[benchPositionKey{user: ev.user, token: negRiskPositionToken(ev.conditionID, outcome)}] = struct{}{}
				}
			}
		}
	}
	return state.prefetchKeys(ctx, keys)
}

func processCryptoEvent(ctx context.Context, state *benchState, ev benchCryptoEvent) error {
	switch ev.kind {
	case eventOrderFilled, eventNegRiskOrderFilled:
		return handleOrderFilled(ctx, state, ev)
	case eventFPMMBuy:
		return handleFPMMBuy(ctx, state, ev)
	case eventFPMMSell:
		return handleFPMMSell(ctx, state, ev)
	case eventFPMMFundingAdded:
		return handleFPMMFundingAdded(ctx, state, ev)
	case eventFPMMFundingRemoved:
		return handleFPMMFundingRemoved(ctx, state, ev)
	case eventPositionSplit:
		return handlePositionSplit(ctx, state, ev)
	case eventPositionsMerge:
		return handlePositionsMerge(ctx, state, ev)
	case eventPayoutRedemptionCTF:
		return handlePayoutRedemptionCTF(ctx, state, ev)
	case eventNegRiskPositionSplit:
		return handleNegRiskPositionSplit(ctx, state, ev)
	case eventNegRiskPositionsMerge:
		return handleNegRiskPositionsMerge(ctx, state, ev)
	case eventPositionsConverted:
		return handlePositionsConverted(ctx, state, ev)
	case eventPayoutRedemptionNR:
		return handlePayoutRedemptionNR(ctx, state, ev)
	case eventConditionResolution:
		return handleConditionResolution(ctx, state, ev)
	case eventQuestionPrepared:
		return handleQuestionPrepared(ctx, state, ev)
	default:
		return nil
	}
}

func handleOrderFilled(ctx context.Context, state *benchState, ev benchCryptoEvent) error {
	baseAmount := ev.makerAmountFilled
	quoteAmount := ev.takerAmountFilled
	token := ev.makerAssetID
	isBuy := false
	if ev.makerAssetID == benchZeroHash {
		isBuy = true
		token = ev.takerAssetID
		baseAmount = ev.takerAmountFilled
		quoteAmount = ev.makerAmountFilled
	}
	price := scaledPrice(quoteAmount, baseAmount)
	key := benchPositionKey{user: ev.user, token: token}
	if isBuy {
		return updateUserPositionWithBuy(ctx, state, key, price, int64(baseAmount), 0, ev.meta())
	}
	return updateUserPositionWithSell(ctx, state, key, price, int64(baseAmount), ev.meta())
}

func handleFPMMBuy(ctx context.Context, state *benchState, ev benchCryptoEvent) error {
	if ev.amount == 0 {
		return nil
	}
	fpmm, ok, err := state.GetFPMM(ctx, ev.fpmm)
	if err != nil || !ok {
		return err
	}
	token := fpmmPositionToken(*fpmm, ev.outcomeIndex)
	price := scaledPrice(ev.takerAmountFilled, ev.amount)
	return updateUserPositionWithBuy(ctx, state, benchPositionKey{user: ev.user, token: token}, price, int64(ev.amount), 0, ev.meta())
}

func handleFPMMSell(ctx context.Context, state *benchState, ev benchCryptoEvent) error {
	if ev.amount == 0 {
		return nil
	}
	fpmm, ok, err := state.GetFPMM(ctx, ev.fpmm)
	if err != nil || !ok {
		return err
	}
	token := fpmmPositionToken(*fpmm, ev.outcomeIndex)
	price := scaledPrice(ev.takerAmountFilled, ev.amount)
	return updateUserPositionWithSell(ctx, state, benchPositionKey{user: ev.user, token: token}, price, int64(ev.amount), ev.meta())
}

func handleFPMMFundingAdded(ctx context.Context, state *benchState, ev benchCryptoEvent) error {
	if ev.amounts[0]+ev.amounts[1] == 0 {
		return nil
	}
	fpmm, ok, err := state.GetFPMM(ctx, ev.fpmm)
	if err != nil || !ok {
		return err
	}
	outcome := uint8(0)
	if ev.amounts[0] > ev.amounts[1] {
		outcome = 1
	}
	low := benchMinU64(ev.amounts[0], ev.amounts[1])
	high := benchMaxU64(ev.amounts[0], ev.amounts[1])
	tokenAmount := int64(high - low)
	if tokenAmount > 0 {
		price := fpmmPrice(ev.amounts, outcome)
		token := fpmmPositionToken(*fpmm, outcome)
		if err := updateUserPositionWithBuy(ctx, state, benchPositionKey{user: ev.user, token: token}, price, tokenAmount, 0, ev.meta()); err != nil {
			return err
		}
	}
	if ev.amount > 0 {
		lpCost := int64(high) - int64(tokenAmount)*fpmmPrice(ev.amounts, outcome)/benchPriceScale
		if lpCost < 0 {
			lpCost = 0
		}
		lpPrice := scaledPrice(uint64(lpCost), ev.amount)
		return updateUserPositionWithBuy(ctx, state, benchPositionKey{user: ev.user, token: lpShareToken(fpmm.id)}, lpPrice, int64(ev.amount), 0, ev.meta())
	}
	return nil
}

func handleFPMMFundingRemoved(ctx context.Context, state *benchState, ev benchCryptoEvent) error {
	if ev.amounts[0]+ev.amounts[1] == 0 {
		return nil
	}
	fpmm, ok, err := state.GetFPMM(ctx, ev.fpmm)
	if err != nil || !ok {
		return err
	}
	tokenCost := int64(0)
	for outcome := uint8(0); outcome < 2; outcome++ {
		tokenAmount := int64(ev.amounts[outcome])
		price := fpmmPrice(ev.amounts, outcome)
		tokenCost += tokenAmount * price / benchPriceScale
		token := fpmmPositionToken(*fpmm, outcome)
		if err := updateUserPositionWithBuy(ctx, state, benchPositionKey{user: ev.user, token: token}, price, tokenAmount, 0, ev.meta()); err != nil {
			return err
		}
	}
	if ev.amount > 0 {
		collateralOut := int64(ev.takerAmountFilled)
		lpPrice := scaledPrice(uint64(benchMax64(0, collateralOut-tokenCost)), ev.amount)
		return updateUserPositionWithSell(ctx, state, benchPositionKey{user: ev.user, token: lpShareToken(fpmm.id)}, lpPrice, int64(ev.amount), ev.meta())
	}
	return nil
}

func handlePositionSplit(ctx context.Context, state *benchState, ev benchCryptoEvent) error {
	if isIgnoredStakeholder(ev.user) {
		return nil
	}
	cond, ok, err := state.GetCondition(ctx, ev.conditionID)
	if err != nil || !ok || cond.outcomeSlotCount != 2 {
		return err
	}
	for outcome := uint8(0); outcome < 2; outcome++ {
		key := benchPositionKey{user: ev.user, token: ctfPositionToken(ev.collateral, ev.conditionID, outcome)}
		if err := updateUserPositionWithBuy(ctx, state, key, benchHalfPrice, int64(ev.amount), 0, ev.meta()); err != nil {
			return err
		}
	}
	return nil
}

func handlePositionsMerge(ctx context.Context, state *benchState, ev benchCryptoEvent) error {
	if isIgnoredStakeholder(ev.user) {
		return nil
	}
	cond, ok, err := state.GetCondition(ctx, ev.conditionID)
	if err != nil || !ok || cond.outcomeSlotCount != 2 {
		return err
	}
	for outcome := uint8(0); outcome < 2; outcome++ {
		key := benchPositionKey{user: ev.user, token: ctfPositionToken(ev.collateral, ev.conditionID, outcome)}
		if err := updateUserPositionWithSell(ctx, state, key, benchHalfPrice, int64(ev.amount), ev.meta()); err != nil {
			return err
		}
	}
	return nil
}

func handleNegRiskPositionSplit(ctx context.Context, state *benchState, ev benchCryptoEvent) error {
	if ev.user == benchNegRiskExchange {
		return nil
	}
	if _, ok, err := state.GetCondition(ctx, ev.conditionID); err != nil {
		return err
	} else if !ok {
		state.SaveCondition(&benchCondition{
			id:               ev.conditionID,
			outcomeSlotCount: 2,
			resolved:         false,
		}, ev.meta())
	}
	for outcome := uint8(0); outcome < 2; outcome++ {
		key := benchPositionKey{user: ev.user, token: negRiskPositionToken(ev.conditionID, outcome)}
		if err := updateUserPositionWithBuy(ctx, state, key, benchHalfPrice, int64(ev.amount), 0, ev.meta()); err != nil {
			return err
		}
	}
	return nil
}

func handleNegRiskPositionsMerge(ctx context.Context, state *benchState, ev benchCryptoEvent) error {
	if ev.user == benchNegRiskExchange {
		return nil
	}
	if _, ok, err := state.GetCondition(ctx, ev.conditionID); err != nil {
		return err
	} else if !ok {
		state.SaveCondition(&benchCondition{
			id:               ev.conditionID,
			outcomeSlotCount: 2,
			resolved:         false,
		}, ev.meta())
	}
	for outcome := uint8(0); outcome < 2; outcome++ {
		key := benchPositionKey{user: ev.user, token: negRiskPositionToken(ev.conditionID, outcome)}
		if err := updateUserPositionWithSell(ctx, state, key, benchHalfPrice, int64(ev.amount), ev.meta()); err != nil {
			return err
		}
	}
	return nil
}

func handlePositionsConverted(ctx context.Context, state *benchState, ev benchCryptoEvent) error {
	nr, ok, err := state.GetNegRisk(ctx, ev.marketID)
	if err != nil || !ok || nr.questionCount == 0 {
		return err
	}

	type sellPlan struct {
		key   benchPositionKey
		price int64
	}
	var noSells []sellPlan
	var yesBuys []benchPositionKey
	sumPrice := int64(0)

	for q := uint32(0); q < nr.questionCount; q++ {
		selected := (ev.indexSet & (uint64(1) << q)) != 0
		if !selected {
			token, ok := negRiskConvertedToken(*nr, ev.marketID, q, 0)
			if ok {
				yesBuys = append(yesBuys, benchPositionKey{user: ev.user, token: token})
			}
			continue
		}
		token, ok := negRiskConvertedToken(*nr, ev.marketID, q, 1)
		if !ok {
			return nil
		}
		key := benchPositionKey{user: ev.user, token: token}
		currentAvg := int64(0)
		pos, posOK, err := state.GetPosition(ctx, key)
		if err != nil {
			return err
		}
		if posOK {
			currentAvg = pos.avgPrice
		}
		noSells = append(noSells, sellPlan{key: key, price: currentAvg})
		sumPrice += currentAvg
	}

	noCount := int64(len(noSells))
	if noCount == 0 {
		return nil
	}
	for _, sell := range noSells {
		if err := updateUserPositionWithSell(ctx, state, sell.key, sell.price, int64(ev.amount), ev.meta()); err != nil {
			return err
		}
	}
	if len(yesBuys) == 0 {
		return nil
	}
	avgNoPrice := sumPrice / noCount
	yesPrice := computeNegRiskYesPrice(avgNoPrice, noCount, int64(nr.questionCount))
	for _, key := range yesBuys {
		if err := updateUserPositionWithBuy(ctx, state, key, yesPrice, int64(ev.amount), 0, ev.meta()); err != nil {
			return err
		}
	}
	return nil
}

func handleConditionResolution(ctx context.Context, state *benchState, ev benchCryptoEvent) error {
	cond, ok, err := state.GetCondition(ctx, ev.conditionID)
	if err != nil || !ok {
		return err
	}
	cond.resolved = true
	cond.payout0 = ev.payouts[0]
	cond.payout1 = ev.payouts[1]
	state.SaveCondition(cond, ev.meta())
	return nil
}

func handleQuestionPrepared(ctx context.Context, state *benchState, ev benchCryptoEvent) error {
	nr, ok, err := state.GetNegRisk(ctx, ev.marketID)
	if err != nil || !ok {
		return err
	}
	if ev.questionIndex >= uint32(len(nr.questionIDs)) {
		return nil
	}
	nr.questionIDs[ev.questionIndex] = ev.questionID
	if nr.questionCount < ev.questionIndex+1 {
		nr.questionCount = ev.questionIndex + 1
	}
	state.SaveNegRisk(nr, ev.meta())
	return nil
}

func handlePayoutRedemptionCTF(ctx context.Context, state *benchState, ev benchCryptoEvent) error {
	if ev.user == benchNegRiskAdapter {
		return nil
	}
	cond, ok, err := state.GetCondition(ctx, ev.conditionID)
	if err != nil || !ok || !cond.resolved {
		return err
	}
	denom := int64(cond.payout0 + cond.payout1)
	if denom == 0 {
		return nil
	}
	for outcome := uint8(0); outcome < 2; outcome++ {
		payout := cond.payout0
		if outcome == 1 {
			payout = cond.payout1
		}
		price := int64(payout) * benchPriceScale / denom
		key := benchPositionKey{user: ev.user, token: ctfPositionToken(ev.collateral, ev.conditionID, outcome)}
		pos, posOK, err := state.GetPosition(ctx, key)
		if err != nil {
			return err
		}
		if posOK && pos.amount > 0 {
			if err := updateUserPositionWithSell(ctx, state, key, price, pos.amount, ev.meta()); err != nil {
				return err
			}
		}
	}
	return nil
}

func handlePayoutRedemptionNR(ctx context.Context, state *benchState, ev benchCryptoEvent) error {
	cond, ok, err := state.GetCondition(ctx, ev.conditionID)
	if err != nil || !ok || !cond.resolved {
		return err
	}
	denom := int64(cond.payout0 + cond.payout1)
	if denom == 0 {
		return nil
	}
	for outcome := uint8(0); outcome < 2; outcome++ {
		payout := cond.payout0
		if outcome == 1 {
			payout = cond.payout1
		}
		price := int64(payout) * benchPriceScale / denom
		key := benchPositionKey{user: ev.user, token: negRiskPositionToken(ev.conditionID, outcome)}
		if err := updateUserPositionWithSell(ctx, state, key, price, int64(ev.amounts[outcome]), ev.meta()); err != nil {
			return err
		}
	}
	return nil
}

func updateUserPositionWithBuy(ctx context.Context, state *benchState, key benchPositionKey, price, amount, pnlAdjustment int64, meta benchMeta) error {
	if amount <= 0 {
		return nil
	}
	pos, ok, err := state.GetPosition(ctx, key)
	if err != nil {
		return err
	}
	if !ok {
		pos = &benchPosition{key: key}
	}
	if pnlAdjustment != 0 {
		pos.realizedPnL += pnlAdjustment
	}
	denom := pos.amount + amount
	if denom > 0 {
		pos.avgPrice = (pos.avgPrice*pos.amount + price*amount) / denom
	}
	pos.amount += amount
	pos.totalBought += amount
	state.SavePosition(pos, meta)
	return nil
}

func updateUserPositionWithSell(ctx context.Context, state *benchState, key benchPositionKey, price, amount int64, meta benchMeta) error {
	pos, ok, err := state.GetPosition(ctx, key)
	if err != nil || !ok {
		return err
	}
	adjusted := amount
	if adjusted > pos.amount {
		adjusted = pos.amount
	}
	if adjusted <= 0 {
		return nil
	}
	pos.realizedPnL += adjusted * (price - pos.avgPrice) / benchPriceScale
	pos.amount -= adjusted
	state.SavePosition(pos, meta)
	return nil
}

func scaledPrice(quoteAmount, baseAmount uint64) int64 {
	if baseAmount == 0 {
		return 0
	}
	return int64(quoteAmount) * benchPriceScale / int64(baseAmount)
}

func fpmmPrice(amounts [2]uint64, outcome uint8) int64 {
	total := amounts[0] + amounts[1]
	if total == 0 || outcome > 1 {
		return 0
	}
	return int64(amounts[1-outcome]) * benchPriceScale / int64(total)
}

func computeNegRiskYesPrice(noPrice, noCount, questionCount int64) int64 {
	if noCount == 0 || questionCount <= noCount {
		return 0
	}
	return (noPrice*noCount - (noCount-1)*benchPriceScale) / (questionCount - noCount)
}

func isIgnoredStakeholder(addr [20]byte) bool {
	return addr == benchNegRiskAdapter || addr == benchExchangeAddress || addr == benchNegRiskExchange
}

func newBenchmarkEvents(cfg PrefetchModeConfig) []benchCryptoEvent {
	events := make([]benchCryptoEvent, cfg.Events)
	for i := 0; i < cfg.Events; i++ {
		userID := uint64((i*37 + i/11) % cfg.Users)
		condID := uint64((i*13 + 7) % cfg.Conditions)
		fpmmID := uint64((i*17 + 3) % cfg.FPMMs)
		marketID := uint64((i*19 + 5) % cfg.Markets)
		tokenID := uint64((i*29 + 11) % benchMax(1, cfg.Positions))
		amount := uint64(1 + (i*7)%97)
		price := uint64(100_000 + (i*7919)%900_000)
		blockNumber := uint64(1 + i/400)

		ev := benchCryptoEvent{
			user:              benchAddressFromID(userID + 1),
			taker:             benchAddressFromID(uint64((i*41+17)%cfg.Users) + 1),
			conditionID:       benchConditionIDFromID(condID),
			collateral:        benchDefaultCollateral,
			fpmm:              benchFPMMAddressFromID(fpmmID),
			marketID:          benchMarketIDFromID(marketID),
			outcomeIndex:      uint8(i % 2),
			indexSet:          uint64(1 << uint((i/3)%3)),
			amount:            amount,
			amounts:           [2]uint64{amount + uint64(i%5), amount + uint64((i+3)%7)},
			payouts:           [2]uint64{1 + uint64(i%3), 1 + uint64((i+1)%3)},
			makerAmountFilled: amount * price / uint64(benchPriceScale),
			takerAmountFilled: amount,
			blockNumber:       blockNumber,
			txIndex:           uint64(i % 400),
			logIndex:          uint64(i),
		}

		switch bucket := i % 100; {
		case bucket < 22:
			ev.kind = eventOrderFilled
			ev.makerAssetID = benchZeroHash
			ev.takerAssetID = benchTokenFromID(tokenID)
		case bucket < 38:
			ev.kind = eventOrderFilled
			ev.makerAssetID = benchTokenFromID(tokenID)
			ev.takerAssetID = benchZeroHash
		case bucket < 46:
			ev.kind = eventNegRiskOrderFilled
			if i%2 == 0 {
				ev.makerAssetID = benchZeroHash
				ev.takerAssetID = negRiskPositionToken(benchNegRiskConditionID(ev.marketID, uint32(i%3)), uint8(i%2))
			} else {
				ev.makerAssetID = negRiskPositionToken(benchNegRiskConditionID(ev.marketID, uint32(i%3)), uint8(i%2))
				ev.takerAssetID = benchZeroHash
			}
		case bucket < 54:
			ev.kind = eventFPMMBuy
			ev.takerAmountFilled = amount * (100_000 + uint64(i%500_000)) / uint64(benchPriceScale)
		case bucket < 62:
			ev.kind = eventFPMMSell
			ev.takerAmountFilled = amount * (100_000 + uint64(i%500_000)) / uint64(benchPriceScale)
		case bucket < 68:
			ev.kind = eventFPMMFundingAdded
			ev.amount = amount + 10
		case bucket < 72:
			ev.kind = eventFPMMFundingRemoved
			ev.amount = amount + 10
			ev.takerAmountFilled = amount*2 + 50
		case bucket < 78:
			ev.kind = eventPositionSplit
		case bucket < 82:
			ev.kind = eventPositionsMerge
		case bucket < 86:
			ev.kind = eventPayoutRedemptionCTF
		case bucket < 90:
			ev.kind = eventNegRiskPositionSplit
			ev.conditionID = benchNegRiskConditionID(ev.marketID, uint32(i%3))
		case bucket < 93:
			ev.kind = eventNegRiskPositionsMerge
			ev.conditionID = benchNegRiskConditionID(ev.marketID, uint32(i%3))
		case bucket < 96:
			ev.kind = eventPositionsConverted
			ev.indexSet = uint64(1 << uint(i%3))
		case bucket < 98:
			ev.kind = eventPayoutRedemptionNR
			ev.conditionID = benchNegRiskConditionID(ev.marketID, uint32(i%3))
		case bucket < 99:
			ev.kind = eventConditionResolution
		default:
			ev.kind = eventQuestionPrepared
			ev.questionIndex = uint32(i % 4)
			ev.questionID = benchNegRiskQuestionID(ev.marketID, ev.questionIndex)
		}
		events[i] = ev
	}
	return events
}

func populateBenchmarkTables(ctx context.Context, conn *ch.Client, cfg PrefetchModeConfig, events []benchCryptoEvent) error {
	conditions := collectBenchmarkConditions(cfg, events)
	fpmms := collectBenchmarkFPMMs(cfg, events)
	negRisk := collectBenchmarkNegRisk(cfg, events)
	positions := collectBenchmarkPositions(cfg, events, conditions, fpmms, negRisk)

	log.Printf("[BENCH] Seed rows: positions=%d conditions=%d fpmms=%d negRisk=%d", len(positions), len(conditions), len(fpmms), len(negRisk))
	if err := insertConditions(ctx, conn, cfg.Database, conditions); err != nil {
		return err
	}
	if err := insertFPMMs(ctx, conn, cfg.Database, fpmms); err != nil {
		return err
	}
	if err := insertNegRisk(ctx, conn, cfg.Database, negRisk); err != nil {
		return err
	}
	if err := insertPositions(ctx, conn, cfg.Database, positions); err != nil {
		return err
	}
	return nil
}

func collectBenchmarkConditions(cfg PrefetchModeConfig, events []benchCryptoEvent) map[[32]byte]benchCondition {
	conditions := make(map[[32]byte]benchCondition)
	for i := 0; i < cfg.Conditions; i++ {
		id := benchConditionIDFromID(uint64(i))
		if shouldSkipSeed(id[:], 23) {
			continue
		}
		conditions[id] = seededCondition(id, i)
	}
	for _, ev := range events {
		switch ev.kind {
		case eventPositionSplit, eventPositionsMerge, eventPayoutRedemptionCTF, eventConditionResolution,
			eventNegRiskPositionSplit, eventNegRiskPositionsMerge, eventPayoutRedemptionNR:
			if shouldSkipSeed(ev.conditionID[:], 23) {
				continue
			}
			conditions[ev.conditionID] = seededCondition(ev.conditionID, int(ev.logIndex))
		}
	}
	return conditions
}

func seededCondition(id [32]byte, n int) benchCondition {
	resolved := n%5 == 0
	return benchCondition{
		id:               id,
		outcomeSlotCount: 2,
		resolved:         resolved,
		payout0:          1 + uint64(n%3),
		payout1:          1 + uint64((n+1)%3),
	}
}

func collectBenchmarkFPMMs(cfg PrefetchModeConfig, events []benchCryptoEvent) map[[20]byte]benchFPMM {
	fpmms := make(map[[20]byte]benchFPMM)
	for i := 0; i < cfg.FPMMs; i++ {
		fpmm := seededFPMM(uint64(i), cfg)
		if shouldSkipSeed(fpmm.id[:], 19) {
			continue
		}
		fpmms[fpmm.id] = fpmm
	}
	for _, ev := range events {
		switch ev.kind {
		case eventFPMMBuy, eventFPMMSell, eventFPMMFundingAdded, eventFPMMFundingRemoved:
			idx := benchFPMMIDFromAddress(ev.fpmm)
			fpmm := seededFPMM(idx, cfg)
			if shouldSkipSeed(fpmm.id[:], 19) {
				continue
			}
			fpmms[fpmm.id] = fpmm
		}
	}
	return fpmms
}

func seededFPMM(id uint64, cfg PrefetchModeConfig) benchFPMM {
	condID := id % uint64(benchMax(1, cfg.Conditions))
	return benchFPMM{
		id:          benchFPMMAddressFromID(id),
		conditionID: benchConditionIDFromID(condID),
		collateral:  benchDefaultCollateral,
	}
}

func collectBenchmarkNegRisk(cfg PrefetchModeConfig, events []benchCryptoEvent) map[[32]byte]benchNegRiskEvent {
	negRisk := make(map[[32]byte]benchNegRiskEvent)
	for i := 0; i < cfg.Markets; i++ {
		nr := seededNegRisk(benchMarketIDFromID(uint64(i)), i)
		if shouldSkipSeed(nr.id[:], 29) {
			continue
		}
		negRisk[nr.id] = nr
	}
	for _, ev := range events {
		switch ev.kind {
		case eventPositionsConverted, eventQuestionPrepared:
			nr := seededNegRisk(ev.marketID, int(ev.logIndex))
			if shouldSkipSeed(nr.id[:], 29) {
				continue
			}
			negRisk[nr.id] = nr
		}
	}
	return negRisk
}

func seededNegRisk(id [32]byte, n int) benchNegRiskEvent {
	nr := benchNegRiskEvent{
		id:            id,
		questionCount: uint32(3 + n%2),
	}
	for i := uint32(0); i < nr.questionCount && i < uint32(len(nr.questionIDs)); i++ {
		nr.questionIDs[i] = benchNegRiskQuestionID(id, i)
	}
	return nr
}

func collectBenchmarkPositions(cfg PrefetchModeConfig, events []benchCryptoEvent, conditions map[[32]byte]benchCondition, fpmms map[[20]byte]benchFPMM, negRisk map[[32]byte]benchNegRiskEvent) []benchPosition {
	keys := make(map[benchPositionKey]struct{})
	for i := 0; i < cfg.Positions*2; i++ {
		keys[benchPositionKey{
			user:  benchAddressFromID(uint64(i%benchMax(1, cfg.Users)) + 1),
			token: benchTokenFromID(uint64(i % benchMax(1, cfg.Positions))),
		}] = struct{}{}
	}
	for _, ev := range events {
		switch ev.kind {
		case eventOrderFilled, eventNegRiskOrderFilled:
			token := ev.makerAssetID
			if token == benchZeroHash {
				token = ev.takerAssetID
			}
			keys[benchPositionKey{user: ev.user, token: token}] = struct{}{}
		case eventFPMMBuy, eventFPMMSell:
			if fpmm, ok := fpmms[ev.fpmm]; ok {
				keys[benchPositionKey{user: ev.user, token: fpmmPositionToken(fpmm, ev.outcomeIndex)}] = struct{}{}
			}
		case eventFPMMFundingAdded, eventFPMMFundingRemoved:
			if fpmm, ok := fpmms[ev.fpmm]; ok {
				for outcome := uint8(0); outcome < 2; outcome++ {
					keys[benchPositionKey{user: ev.user, token: fpmmPositionToken(fpmm, outcome)}] = struct{}{}
				}
				keys[benchPositionKey{user: ev.user, token: lpShareToken(fpmm.id)}] = struct{}{}
			}
		case eventPositionSplit, eventPositionsMerge, eventPayoutRedemptionCTF:
			if _, ok := conditions[ev.conditionID]; ok {
				for outcome := uint8(0); outcome < 2; outcome++ {
					keys[benchPositionKey{user: ev.user, token: ctfPositionToken(ev.collateral, ev.conditionID, outcome)}] = struct{}{}
				}
			}
		case eventNegRiskPositionSplit, eventNegRiskPositionsMerge, eventPayoutRedemptionNR:
			for outcome := uint8(0); outcome < 2; outcome++ {
				keys[benchPositionKey{user: ev.user, token: negRiskPositionToken(ev.conditionID, outcome)}] = struct{}{}
			}
		case eventPositionsConverted:
			nr, ok := negRisk[ev.marketID]
			if !ok {
				continue
			}
			for q := uint32(0); q < nr.questionCount; q++ {
				for outcome := uint8(0); outcome < 2; outcome++ {
					token, ok := negRiskConvertedToken(nr, ev.marketID, q, outcome)
					if ok {
						keys[benchPositionKey{user: ev.user, token: token}] = struct{}{}
					}
				}
			}
		}
	}

	sorted := sortedPositionKeys(keys)
	if len(sorted) > cfg.Positions {
		sorted = sorted[:cfg.Positions]
	}
	rows := make([]benchPosition, 0, len(sorted))
	for i, key := range sorted {
		if shouldSkipPositionSeed(key) {
			continue
		}
		amount := int64(10 + i%90)
		price := int64(100_000 + (i*7919)%800_000)
		rows = append(rows, benchPosition{
			key:         key,
			amount:      amount,
			totalBought: amount,
			avgPrice:    price,
			realizedPnL: int64(i % 17),
		})
	}
	return rows
}

func shouldSkipSeed(data []byte, mod byte) bool {
	if len(data) == 0 || mod == 0 {
		return false
	}
	return data[len(data)-1]%mod == 0
}

func shouldSkipPositionSeed(key benchPositionKey) bool {
	return (key.user[19]^key.token[31])%11 == 0
}

func insertPositions(ctx context.Context, conn *ch.Client, db string, rows []benchPosition) error {
	const chunkSize = 1000
	var (
		colUser           proto.ColFixedStr
		colToken          proto.ColFixedStr
		colAmount         proto.ColInt64
		colTotalBought    proto.ColInt64
		colAvgPrice       proto.ColInt64
		colRealizedPnL    proto.ColInt64
		colUpdatedAtBlock proto.ColUInt64
		colBlockNumber    proto.ColUInt64
		colTxIndex        proto.ColUInt64
		colLogIndex       proto.ColUInt64
	)
	colUser.SetSize(20)
	colToken.SetSize(32)
	input := proto.Input{
		{Name: "user", Data: &colUser},
		{Name: "token_id", Data: &colToken},
		{Name: "amount", Data: &colAmount},
		{Name: "total_bought", Data: &colTotalBought},
		{Name: "avg_price", Data: &colAvgPrice},
		{Name: "realized_pnl", Data: &colRealizedPnL},
		{Name: "updated_at_block", Data: &colUpdatedAtBlock},
		{Name: "block_number", Data: &colBlockNumber},
		{Name: "transaction_index", Data: &colTxIndex},
		{Name: "log_index", Data: &colLogIndex},
	}
	processed := 0
	query := fmt.Sprintf("INSERT INTO %s.prefetch_crypto_positions (`user`, `token_id`, `amount`, `total_bought`, `avg_price`, `realized_pnl`, `updated_at_block`, `block_number`, `transaction_index`, `log_index`) VALUES", ident(db))
	return conn.Do(ctx, ch.Query{
		Body:  query,
		Input: input,
		OnInput: func(context.Context) error {
			if processed >= len(rows) {
				input.Reset()
				return io.EOF
			}
			end := benchMin(processed+chunkSize, len(rows))
			for _, row := range rows[processed:end] {
				colUser.Append(row.key.user[:])
				colToken.Append(row.key.token[:])
				colAmount.Append(row.amount)
				colTotalBought.Append(row.totalBought)
				colAvgPrice.Append(row.avgPrice)
				colRealizedPnL.Append(row.realizedPnL)
				colUpdatedAtBlock.Append(row.updatedAtBlock)
				colBlockNumber.Append(row.blockNumber)
				colTxIndex.Append(row.txIndex)
				colLogIndex.Append(row.logIndex)
			}
			processed = end
			return nil
		},
	})
}

func insertConditions(ctx context.Context, conn *ch.Client, db string, rows map[[32]byte]benchCondition) error {
	var (
		colID               proto.ColFixedStr
		colOutcomeSlotCount proto.ColUInt8
		colResolved         proto.ColUInt8
		colPayout0          proto.ColUInt64
		colPayout1          proto.ColUInt64
		colBlockNumber      proto.ColUInt64
		colTxIndex          proto.ColUInt64
		colLogIndex         proto.ColUInt64
	)
	colID.SetSize(32)
	input := proto.Input{
		{Name: "id", Data: &colID},
		{Name: "outcome_slot_count", Data: &colOutcomeSlotCount},
		{Name: "resolved", Data: &colResolved},
		{Name: "payout0", Data: &colPayout0},
		{Name: "payout1", Data: &colPayout1},
		{Name: "block_number", Data: &colBlockNumber},
		{Name: "transaction_index", Data: &colTxIndex},
		{Name: "log_index", Data: &colLogIndex},
	}
	keys := sortedHashKeys(mapFromConditionRows(rows))
	processed := 0
	query := fmt.Sprintf("INSERT INTO %s.prefetch_crypto_conditions (`id`, `outcome_slot_count`, `resolved`, `payout0`, `payout1`, `block_number`, `transaction_index`, `log_index`) VALUES", ident(db))
	return conn.Do(ctx, ch.Query{
		Body:  query,
		Input: input,
		OnInput: func(context.Context) error {
			if processed >= len(keys) {
				input.Reset()
				return io.EOF
			}
			end := benchMin(processed+1000, len(keys))
			for _, key := range keys[processed:end] {
				row := rows[key]
				colID.Append(row.id[:])
				colOutcomeSlotCount.Append(row.outcomeSlotCount)
				if row.resolved {
					colResolved.Append(1)
				} else {
					colResolved.Append(0)
				}
				colPayout0.Append(row.payout0)
				colPayout1.Append(row.payout1)
				colBlockNumber.Append(row.blockNumber)
				colTxIndex.Append(row.txIndex)
				colLogIndex.Append(row.logIndex)
			}
			processed = end
			return nil
		},
	})
}

func insertFPMMs(ctx context.Context, conn *ch.Client, db string, rows map[[20]byte]benchFPMM) error {
	var (
		colID          proto.ColFixedStr
		colConditionID proto.ColFixedStr
		colCollateral  proto.ColFixedStr
		colBlockNumber proto.ColUInt64
		colTxIndex     proto.ColUInt64
		colLogIndex    proto.ColUInt64
	)
	colID.SetSize(20)
	colConditionID.SetSize(32)
	colCollateral.SetSize(20)
	input := proto.Input{
		{Name: "id", Data: &colID},
		{Name: "condition_id", Data: &colConditionID},
		{Name: "collateral", Data: &colCollateral},
		{Name: "block_number", Data: &colBlockNumber},
		{Name: "transaction_index", Data: &colTxIndex},
		{Name: "log_index", Data: &colLogIndex},
	}
	keys := sortedAddressKeys(mapFromFPMMRows(rows))
	processed := 0
	query := fmt.Sprintf("INSERT INTO %s.prefetch_crypto_fpmms (`id`, `condition_id`, `collateral`, `block_number`, `transaction_index`, `log_index`) VALUES", ident(db))
	return conn.Do(ctx, ch.Query{
		Body:  query,
		Input: input,
		OnInput: func(context.Context) error {
			if processed >= len(keys) {
				input.Reset()
				return io.EOF
			}
			end := benchMin(processed+1000, len(keys))
			for _, key := range keys[processed:end] {
				row := rows[key]
				colID.Append(row.id[:])
				colConditionID.Append(row.conditionID[:])
				colCollateral.Append(row.collateral[:])
				colBlockNumber.Append(row.blockNumber)
				colTxIndex.Append(row.txIndex)
				colLogIndex.Append(row.logIndex)
			}
			processed = end
			return nil
		},
	})
}

func insertNegRisk(ctx context.Context, conn *ch.Client, db string, rows map[[32]byte]benchNegRiskEvent) error {
	var (
		colID            proto.ColFixedStr
		colQuestionCount proto.ColUInt32
		colQ0            proto.ColFixedStr
		colQ1            proto.ColFixedStr
		colQ2            proto.ColFixedStr
		colQ3            proto.ColFixedStr
		colBlockNumber   proto.ColUInt64
		colTxIndex       proto.ColUInt64
		colLogIndex      proto.ColUInt64
	)
	colID.SetSize(32)
	colQ0.SetSize(32)
	colQ1.SetSize(32)
	colQ2.SetSize(32)
	colQ3.SetSize(32)
	input := proto.Input{
		{Name: "id", Data: &colID},
		{Name: "question_count", Data: &colQuestionCount},
		{Name: "q0", Data: &colQ0},
		{Name: "q1", Data: &colQ1},
		{Name: "q2", Data: &colQ2},
		{Name: "q3", Data: &colQ3},
		{Name: "block_number", Data: &colBlockNumber},
		{Name: "transaction_index", Data: &colTxIndex},
		{Name: "log_index", Data: &colLogIndex},
	}
	keys := sortedHashKeys(mapFromNegRiskRows(rows))
	processed := 0
	query := fmt.Sprintf("INSERT INTO %s.prefetch_crypto_negrisk (`id`, `question_count`, `q0`, `q1`, `q2`, `q3`, `block_number`, `transaction_index`, `log_index`) VALUES", ident(db))
	return conn.Do(ctx, ch.Query{
		Body:  query,
		Input: input,
		OnInput: func(context.Context) error {
			if processed >= len(keys) {
				input.Reset()
				return io.EOF
			}
			end := benchMin(processed+1000, len(keys))
			for _, key := range keys[processed:end] {
				row := rows[key]
				colID.Append(row.id[:])
				colQuestionCount.Append(row.questionCount)
				colQ0.Append(row.questionIDs[0][:])
				colQ1.Append(row.questionIDs[1][:])
				colQ2.Append(row.questionIDs[2][:])
				colQ3.Append(row.questionIDs[3][:])
				colBlockNumber.Append(row.blockNumber)
				colTxIndex.Append(row.txIndex)
				colLogIndex.Append(row.logIndex)
			}
			processed = end
			return nil
		},
	})
}

func (s *benchState) resolvePositions(ctx context.Context, keys []benchPositionKey) error {
	for start := 0; start < len(keys); start += s.resolveChunk {
		end := benchMin(start+s.resolveChunk, len(keys))
		if err := s.resolvePositionChunk(ctx, keys[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (s *benchState) resolvePositionChunk(ctx context.Context, keys []benchPositionKey) error {
	keys = dedupePositionKeys(keys)
	if len(keys) == 0 {
		return nil
	}
	s.resolveQueries++
	values := tupleValuesPosition(keys)
	var (
		colUser           proto.ColFixedStr
		colToken          proto.ColFixedStr
		colAmount         proto.ColInt64
		colTotalBought    proto.ColInt64
		colAvgPrice       proto.ColInt64
		colRealizedPnL    proto.ColInt64
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
		{Name: "realized_pnl", Data: &colRealizedPnL},
		{Name: "updated_at_block", Data: &colUpdatedAtBlock},
		{Name: "block_number", Data: &colBlockNumber},
		{Name: "transaction_index", Data: &colTxIndex},
		{Name: "log_index", Data: &colLogIndex},
	}
	query := fmt.Sprintf("SELECT `user`, `token_id`, `amount`, `total_bought`, `avg_price`, `realized_pnl`, `updated_at_block`, `block_number`, `transaction_index`, `log_index` FROM %s.prefetch_crypto_positions WHERE (`user`, `token_id`) IN (%s) ORDER BY `block_number` DESC, `transaction_index` DESC, `log_index` DESC LIMIT 1 BY `user`, `token_id`", ident(s.db), values)
	found := make(map[benchPositionKey]struct{}, len(keys))
	err := s.conn.Do(ctx, ch.Query{
		Body:   query,
		Result: results,
		OnResult: func(_ context.Context, block proto.Block) error {
			s.resolvedRows += uint64(block.Rows)
			for i := 0; i < block.Rows; i++ {
				var key benchPositionKey
				copy(key.user[:], colUser.Row(i))
				copy(key.token[:], colToken.Row(i))
				found[key] = struct{}{}
				s.positions[key] = benchPosition{
					key:            key,
					amount:         colAmount.Row(i),
					totalBought:    colTotalBought.Row(i),
					avgPrice:       colAvgPrice.Row(i),
					realizedPnL:    colRealizedPnL.Row(i),
					updatedAtBlock: colUpdatedAtBlock.Row(i),
					blockNumber:    colBlockNumber.Row(i),
					txIndex:        colTxIndex.Row(i),
					logIndex:       colLogIndex.Row(i),
				}
			}
			return nil
		},
	})
	if err != nil {
		return err
	}
	if s.usesKnownAbsentCache() {
		for _, key := range keys {
			if _, ok := found[key]; !ok {
				s.absentPositions[key] = struct{}{}
			}
		}
	}
	return nil
}

func (s *benchState) resolveConditions(ctx context.Context, keys [][32]byte) error {
	for start := 0; start < len(keys); start += s.resolveChunk {
		end := benchMin(start+s.resolveChunk, len(keys))
		if err := s.resolveConditionChunk(ctx, keys[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (s *benchState) resolveConditionChunk(ctx context.Context, keys [][32]byte) error {
	keys = dedupeHashKeys(keys)
	if len(keys) == 0 {
		return nil
	}
	s.resolveQueries++
	var (
		colID               proto.ColFixedStr
		colOutcomeSlotCount proto.ColUInt8
		colResolved         proto.ColUInt8
		colPayout0          proto.ColUInt64
		colPayout1          proto.ColUInt64
		colBlockNumber      proto.ColUInt64
		colTxIndex          proto.ColUInt64
		colLogIndex         proto.ColUInt64
	)
	colID.SetSize(32)
	results := proto.Results{
		{Name: "id", Data: &colID},
		{Name: "outcome_slot_count", Data: &colOutcomeSlotCount},
		{Name: "resolved", Data: &colResolved},
		{Name: "payout0", Data: &colPayout0},
		{Name: "payout1", Data: &colPayout1},
		{Name: "block_number", Data: &colBlockNumber},
		{Name: "transaction_index", Data: &colTxIndex},
		{Name: "log_index", Data: &colLogIndex},
	}
	query := fmt.Sprintf("SELECT `id`, `outcome_slot_count`, `resolved`, `payout0`, `payout1`, `block_number`, `transaction_index`, `log_index` FROM %s.prefetch_crypto_conditions WHERE `id` IN (%s) ORDER BY `block_number` DESC, `transaction_index` DESC, `log_index` DESC LIMIT 1 BY `id`", ident(s.db), valuesHash(keys))
	found := make(map[[32]byte]struct{}, len(keys))
	err := s.conn.Do(ctx, ch.Query{
		Body:   query,
		Result: results,
		OnResult: func(_ context.Context, block proto.Block) error {
			s.resolvedRows += uint64(block.Rows)
			for i := 0; i < block.Rows; i++ {
				var id [32]byte
				copy(id[:], colID.Row(i))
				found[id] = struct{}{}
				s.conditions[id] = benchCondition{
					id:               id,
					outcomeSlotCount: colOutcomeSlotCount.Row(i),
					resolved:         colResolved.Row(i) != 0,
					payout0:          colPayout0.Row(i),
					payout1:          colPayout1.Row(i),
					blockNumber:      colBlockNumber.Row(i),
					txIndex:          colTxIndex.Row(i),
					logIndex:         colLogIndex.Row(i),
				}
			}
			return nil
		},
	})
	if err != nil {
		return err
	}
	if s.usesKnownAbsentCache() {
		for _, key := range keys {
			if _, ok := found[key]; !ok {
				s.absentConditions[key] = struct{}{}
			}
		}
	}
	return nil
}

func (s *benchState) resolveFPMMs(ctx context.Context, keys [][20]byte) error {
	for start := 0; start < len(keys); start += s.resolveChunk {
		end := benchMin(start+s.resolveChunk, len(keys))
		if err := s.resolveFPMMChunk(ctx, keys[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (s *benchState) resolveFPMMChunk(ctx context.Context, keys [][20]byte) error {
	keys = dedupeAddressKeys(keys)
	if len(keys) == 0 {
		return nil
	}
	s.resolveQueries++
	var (
		colID          proto.ColFixedStr
		colConditionID proto.ColFixedStr
		colCollateral  proto.ColFixedStr
		colBlockNumber proto.ColUInt64
		colTxIndex     proto.ColUInt64
		colLogIndex    proto.ColUInt64
	)
	colID.SetSize(20)
	colConditionID.SetSize(32)
	colCollateral.SetSize(20)
	results := proto.Results{
		{Name: "id", Data: &colID},
		{Name: "condition_id", Data: &colConditionID},
		{Name: "collateral", Data: &colCollateral},
		{Name: "block_number", Data: &colBlockNumber},
		{Name: "transaction_index", Data: &colTxIndex},
		{Name: "log_index", Data: &colLogIndex},
	}
	query := fmt.Sprintf("SELECT `id`, `condition_id`, `collateral`, `block_number`, `transaction_index`, `log_index` FROM %s.prefetch_crypto_fpmms WHERE `id` IN (%s) ORDER BY `block_number` DESC, `transaction_index` DESC, `log_index` DESC LIMIT 1 BY `id`", ident(s.db), valuesAddress(keys))
	found := make(map[[20]byte]struct{}, len(keys))
	err := s.conn.Do(ctx, ch.Query{
		Body:   query,
		Result: results,
		OnResult: func(_ context.Context, block proto.Block) error {
			s.resolvedRows += uint64(block.Rows)
			for i := 0; i < block.Rows; i++ {
				var id [20]byte
				var conditionID [32]byte
				var collateral [20]byte
				copy(id[:], colID.Row(i))
				copy(conditionID[:], colConditionID.Row(i))
				copy(collateral[:], colCollateral.Row(i))
				found[id] = struct{}{}
				s.fpmms[id] = benchFPMM{
					id:          id,
					conditionID: conditionID,
					collateral:  collateral,
					blockNumber: colBlockNumber.Row(i),
					txIndex:     colTxIndex.Row(i),
					logIndex:    colLogIndex.Row(i),
				}
			}
			return nil
		},
	})
	if err != nil {
		return err
	}
	if s.usesKnownAbsentCache() {
		for _, key := range keys {
			if _, ok := found[key]; !ok {
				s.absentFPMMs[key] = struct{}{}
			}
		}
	}
	return nil
}

func (s *benchState) resolveNegRisk(ctx context.Context, keys [][32]byte) error {
	for start := 0; start < len(keys); start += s.resolveChunk {
		end := benchMin(start+s.resolveChunk, len(keys))
		if err := s.resolveNegRiskChunk(ctx, keys[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (s *benchState) resolveNegRiskChunk(ctx context.Context, keys [][32]byte) error {
	keys = dedupeHashKeys(keys)
	if len(keys) == 0 {
		return nil
	}
	s.resolveQueries++
	var (
		colID            proto.ColFixedStr
		colQuestionCount proto.ColUInt32
		colQ0            proto.ColFixedStr
		colQ1            proto.ColFixedStr
		colQ2            proto.ColFixedStr
		colQ3            proto.ColFixedStr
		colBlockNumber   proto.ColUInt64
		colTxIndex       proto.ColUInt64
		colLogIndex      proto.ColUInt64
	)
	colID.SetSize(32)
	colQ0.SetSize(32)
	colQ1.SetSize(32)
	colQ2.SetSize(32)
	colQ3.SetSize(32)
	results := proto.Results{
		{Name: "id", Data: &colID},
		{Name: "question_count", Data: &colQuestionCount},
		{Name: "q0", Data: &colQ0},
		{Name: "q1", Data: &colQ1},
		{Name: "q2", Data: &colQ2},
		{Name: "q3", Data: &colQ3},
		{Name: "block_number", Data: &colBlockNumber},
		{Name: "transaction_index", Data: &colTxIndex},
		{Name: "log_index", Data: &colLogIndex},
	}
	query := fmt.Sprintf("SELECT `id`, `question_count`, `q0`, `q1`, `q2`, `q3`, `block_number`, `transaction_index`, `log_index` FROM %s.prefetch_crypto_negrisk WHERE `id` IN (%s) ORDER BY `block_number` DESC, `transaction_index` DESC, `log_index` DESC LIMIT 1 BY `id`", ident(s.db), valuesHash(keys))
	found := make(map[[32]byte]struct{}, len(keys))
	err := s.conn.Do(ctx, ch.Query{
		Body:   query,
		Result: results,
		OnResult: func(_ context.Context, block proto.Block) error {
			s.resolvedRows += uint64(block.Rows)
			for i := 0; i < block.Rows; i++ {
				var id [32]byte
				copy(id[:], colID.Row(i))
				found[id] = struct{}{}
				nr := benchNegRiskEvent{
					id:            id,
					questionCount: colQuestionCount.Row(i),
					blockNumber:   colBlockNumber.Row(i),
					txIndex:       colTxIndex.Row(i),
					logIndex:      colLogIndex.Row(i),
				}
				copy(nr.questionIDs[0][:], colQ0.Row(i))
				copy(nr.questionIDs[1][:], colQ1.Row(i))
				copy(nr.questionIDs[2][:], colQ2.Row(i))
				copy(nr.questionIDs[3][:], colQ3.Row(i))
				s.negRisk[id] = nr
			}
			return nil
		},
	})
	if err != nil {
		return err
	}
	if s.usesKnownAbsentCache() {
		for _, key := range keys {
			if _, ok := found[key]; !ok {
				s.absentNegRisk[key] = struct{}{}
			}
		}
	}
	return nil
}

func ensureBenchmarkTables(ctx context.Context, conn *ch.Client, db string) error {
	if err := conn.Do(ctx, ch.Query{Body: fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", ident(db))}); err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	queries := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.prefetch_crypto_positions (
			user FixedString(20),
			token_id FixedString(32),
			amount Int64,
			total_bought Int64,
			avg_price Int64,
			realized_pnl Int64,
			updated_at_block UInt64,
			block_number UInt64,
			transaction_index UInt64,
			log_index UInt64
		) ENGINE = ReplacingMergeTree(block_number)
		ORDER BY (user, token_id)`, ident(db)),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.prefetch_crypto_conditions (
			id FixedString(32),
			outcome_slot_count UInt8,
			resolved UInt8,
			payout0 UInt64,
			payout1 UInt64,
			block_number UInt64,
			transaction_index UInt64,
			log_index UInt64
		) ENGINE = ReplacingMergeTree(block_number)
		ORDER BY id`, ident(db)),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.prefetch_crypto_fpmms (
			id FixedString(20),
			condition_id FixedString(32),
			collateral FixedString(20),
			block_number UInt64,
			transaction_index UInt64,
			log_index UInt64
		) ENGINE = ReplacingMergeTree(block_number)
		ORDER BY id`, ident(db)),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.prefetch_crypto_negrisk (
			id FixedString(32),
			question_count UInt32,
			q0 FixedString(32),
			q1 FixedString(32),
			q2 FixedString(32),
			q3 FixedString(32),
			block_number UInt64,
			transaction_index UInt64,
			log_index UInt64
		) ENGINE = ReplacingMergeTree(block_number)
		ORDER BY id`, ident(db)),
	}
	for _, query := range queries {
		if err := conn.Do(ctx, ch.Query{Body: query}); err != nil {
			return fmt.Errorf("create benchmark table: %w", err)
		}
	}
	return nil
}

func resetBenchmarkTables(ctx context.Context, conn *ch.Client, db string) error {
	for _, table := range []string{"prefetch_crypto_positions", "prefetch_crypto_conditions", "prefetch_crypto_fpmms", "prefetch_crypto_negrisk"} {
		if err := conn.Do(ctx, ch.Query{Body: fmt.Sprintf("TRUNCATE TABLE %s.%s", ident(db), table)}); err != nil {
			return fmt.Errorf("truncate %s: %w", table, err)
		}
	}
	return nil
}

func (s *benchState) checksumDirtyState() uint64 {
	hash := uint64(1469598103934665603)
	positionKeys := sortedPositionKeys(s.dirtyPositions)
	for _, key := range positionKeys {
		pos, ok := s.positions[key]
		if !ok {
			continue
		}
		hash = mixString(hash, "position")
		hash = mixBytes(hash, key.user[:])
		hash = mixBytes(hash, key.token[:])
		hash = mixInt64(hash, pos.amount)
		hash = mixInt64(hash, pos.totalBought)
		hash = mixInt64(hash, pos.avgPrice)
		hash = mixInt64(hash, pos.realizedPnL)
		hash = mixUint64(hash, pos.blockNumber)
		hash = mixUint64(hash, pos.txIndex)
		hash = mixUint64(hash, pos.logIndex)
	}

	conditionKeys := sortedHashKeys(s.dirtyConditions)
	for _, id := range conditionKeys {
		cond, ok := s.conditions[id]
		if !ok {
			continue
		}
		hash = mixString(hash, "condition")
		hash = mixBytes(hash, id[:])
		hash = mixUint64(hash, uint64(cond.outcomeSlotCount))
		if cond.resolved {
			hash = mixUint64(hash, 1)
		} else {
			hash = mixUint64(hash, 0)
		}
		hash = mixUint64(hash, cond.payout0)
		hash = mixUint64(hash, cond.payout1)
		hash = mixUint64(hash, cond.blockNumber)
		hash = mixUint64(hash, cond.txIndex)
		hash = mixUint64(hash, cond.logIndex)
	}

	negRiskKeys := sortedHashKeys(s.dirtyNegRisk)
	for _, id := range negRiskKeys {
		nr, ok := s.negRisk[id]
		if !ok {
			continue
		}
		hash = mixString(hash, "negrisk")
		hash = mixBytes(hash, id[:])
		hash = mixUint64(hash, uint64(nr.questionCount))
		for i := range nr.questionIDs {
			hash = mixBytes(hash, nr.questionIDs[i][:])
		}
		hash = mixUint64(hash, nr.blockNumber)
		hash = mixUint64(hash, nr.txIndex)
		hash = mixUint64(hash, nr.logIndex)
	}

	fpmmKeys := sortedAddressKeys(s.dirtyFPMMs)
	for _, id := range fpmmKeys {
		fpmm, ok := s.fpmms[id]
		if !ok {
			continue
		}
		hash = mixString(hash, "fpmm")
		hash = mixBytes(hash, id[:])
		hash = mixBytes(hash, fpmm.conditionID[:])
		hash = mixBytes(hash, fpmm.collateral[:])
		hash = mixUint64(hash, fpmm.blockNumber)
		hash = mixUint64(hash, fpmm.txIndex)
		hash = mixUint64(hash, fpmm.logIndex)
	}
	return hash
}

func reportPrefetchModeResults(results []PrefetchModeResult) {
	log.Println("========================================")
	log.Println("PREFETCH MODES BENCHMARK RESULTS")
	log.Println("========================================")
	log.Printf("%-10s %10s %10s %10s %8s %8s %8s %8s %8s",
		"Mode", "Duration", "Queries", "Execs", "Alloc", "Hit%", "Speedup", "QueryRed", "Hash")

	baseline := findResult(results, "current")
	if baseline == nil && len(results) > 0 {
		baseline = &results[0]
	}
	if baseline == nil {
		return
	}

	for _, r := range results {
		hitRate := 0.0
		if r.Gets > 0 {
			hitRate = 100.0 * float64(r.HotHits) / float64(r.Gets)
		}
		speedup := 1.0
		if r.Duration > 0 && baseline.Duration > 0 {
			speedup = float64(baseline.Duration) / float64(r.Duration)
		}
		queryRed := 1.0
		if r.ResolveQueries > 0 && baseline.ResolveQueries > 0 {
			queryRed = float64(baseline.ResolveQueries) / float64(r.ResolveQueries)
		}
		log.Printf("%-10s %10s %10d %10d %8s %7.1f%% %8.2fx %8.2fx %016x",
			r.Mode,
			r.Duration.Round(time.Millisecond),
			r.ResolveQueries,
			r.HandlerExecs,
			benchHumanBytes(r.Bytes),
			hitRate,
			speedup,
			queryRed,
			r.FinalHash,
		)
	}

	log.Println("========================================")
	log.Println("Correctness Check:")
	allMatch := true
	for _, r := range results {
		if r.FinalHash != baseline.FinalHash {
			log.Printf("  %s hash mismatch: got=%016x want=%016x", r.Mode, r.FinalHash, baseline.FinalHash)
			allMatch = false
		}
	}
	if allMatch {
		log.Println("  All modes produce identical dirty-state hashes")
	}

	log.Println("========================================")
	log.Println("Key Insights:")
	for _, r := range results {
		if r.Mode == baseline.Mode || baseline.HandlerExecs == 0 || r.ResolveQueries == 0 {
			continue
		}
		execRatio := float64(r.HandlerExecs) / float64(baseline.HandlerExecs)
		queryRatio := float64(baseline.ResolveQueries) / float64(r.ResolveQueries)
		log.Printf("  %s: handler executes %.1fx, queries reduced by %.1fx", r.Mode, execRatio, queryRatio)
	}
}

func findResult(results []PrefetchModeResult, mode string) *PrefetchModeResult {
	for i := range results {
		if results[i].Mode == mode {
			return &results[i]
		}
	}
	return nil
}

func sortedPositionKeys(m map[benchPositionKey]struct{}) []benchPositionKey {
	keys := make([]benchPositionKey, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if c := strings.Compare(hex.EncodeToString(keys[i].user[:]), hex.EncodeToString(keys[j].user[:])); c != 0 {
			return c < 0
		}
		return strings.Compare(hex.EncodeToString(keys[i].token[:]), hex.EncodeToString(keys[j].token[:])) < 0
	})
	return keys
}

func sortedHashKeys(m map[[32]byte]struct{}) [][32]byte {
	keys := make([][32]byte, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return strings.Compare(hex.EncodeToString(keys[i][:]), hex.EncodeToString(keys[j][:])) < 0
	})
	return keys
}

func sortedAddressKeys(m map[[20]byte]struct{}) [][20]byte {
	keys := make([][20]byte, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return strings.Compare(hex.EncodeToString(keys[i][:]), hex.EncodeToString(keys[j][:])) < 0
	})
	return keys
}

func filterMissingPositionKeys(keys map[benchPositionKey]struct{}, cache map[benchPositionKey]benchPosition) []benchPositionKey {
	out := make(map[benchPositionKey]struct{})
	for key := range keys {
		if _, ok := cache[key]; !ok {
			out[key] = struct{}{}
		}
	}
	return sortedPositionKeys(out)
}

func filterMissingHashKeys[T any](keys map[[32]byte]struct{}, cache map[[32]byte]T) [][32]byte {
	out := make(map[[32]byte]struct{})
	for key := range keys {
		if _, ok := cache[key]; !ok {
			out[key] = struct{}{}
		}
	}
	return sortedHashKeys(out)
}

func filterMissingAddressKeys[T any](keys map[[20]byte]struct{}, cache map[[20]byte]T) [][20]byte {
	out := make(map[[20]byte]struct{})
	for key := range keys {
		if _, ok := cache[key]; !ok {
			out[key] = struct{}{}
		}
	}
	return sortedAddressKeys(out)
}

func dedupePositionKeys(keys []benchPositionKey) []benchPositionKey {
	m := make(map[benchPositionKey]struct{}, len(keys))
	for _, key := range keys {
		m[key] = struct{}{}
	}
	return sortedPositionKeys(m)
}

func dedupeHashKeys(keys [][32]byte) [][32]byte {
	m := make(map[[32]byte]struct{}, len(keys))
	for _, key := range keys {
		m[key] = struct{}{}
	}
	return sortedHashKeys(m)
}

func dedupeAddressKeys(keys [][20]byte) [][20]byte {
	m := make(map[[20]byte]struct{}, len(keys))
	for _, key := range keys {
		m[key] = struct{}{}
	}
	return sortedAddressKeys(m)
}

func tupleValuesPosition(keys []benchPositionKey) string {
	var b strings.Builder
	b.Grow(len(keys) * 160)
	for i, key := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("(unhex('")
		b.WriteString(hex.EncodeToString(key.user[:]))
		b.WriteString("'),unhex('")
		b.WriteString(hex.EncodeToString(key.token[:]))
		b.WriteString("'))")
	}
	return b.String()
}

func valuesHash(keys [][32]byte) string {
	var b strings.Builder
	b.Grow(len(keys) * 80)
	for i, key := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("unhex('")
		b.WriteString(hex.EncodeToString(key[:]))
		b.WriteString("')")
	}
	return b.String()
}

func valuesAddress(keys [][20]byte) string {
	var b strings.Builder
	b.Grow(len(keys) * 56)
	for i, key := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("unhex('")
		b.WriteString(hex.EncodeToString(key[:]))
		b.WriteString("')")
	}
	return b.String()
}

func mapFromConditionRows(rows map[[32]byte]benchCondition) map[[32]byte]struct{} {
	out := make(map[[32]byte]struct{}, len(rows))
	for key := range rows {
		out[key] = struct{}{}
	}
	return out
}

func mapFromFPMMRows(rows map[[20]byte]benchFPMM) map[[20]byte]struct{} {
	out := make(map[[20]byte]struct{}, len(rows))
	for key := range rows {
		out[key] = struct{}{}
	}
	return out
}

func mapFromNegRiskRows(rows map[[32]byte]benchNegRiskEvent) map[[32]byte]struct{} {
	out := make(map[[32]byte]struct{}, len(rows))
	for key := range rows {
		out[key] = struct{}{}
	}
	return out
}

func benchAddressFromID(id uint64) [20]byte {
	var out [20]byte
	out[0] = 0x42
	binary.BigEndian.PutUint64(out[12:], id)
	return out
}

func benchTokenFromID(id uint64) [32]byte {
	return benchHash("token", id)
}

func benchConditionIDFromID(id uint64) [32]byte {
	return benchHash("condition", id)
}

func benchMarketIDFromID(id uint64) [32]byte {
	return benchHash("market", id)
}

func benchFPMMAddressFromID(id uint64) [20]byte {
	var out [20]byte
	out[0] = 0xf0
	binary.BigEndian.PutUint64(out[12:], id)
	return out
}

func benchFPMMIDFromAddress(addr [20]byte) uint64 {
	return binary.BigEndian.Uint64(addr[12:])
}

func benchNegRiskQuestionID(marketID [32]byte, questionIndex uint32) [32]byte {
	var idx [4]byte
	binary.BigEndian.PutUint32(idx[:], questionIndex)
	return benchHashBytes([]byte("nr-question"), marketID[:], idx[:])
}

func benchNegRiskConditionID(marketID [32]byte, questionIndex uint32) [32]byte {
	return conditionFromQuestion(benchNegRiskQuestionID(marketID, questionIndex))
}

func conditionFromQuestion(questionID [32]byte) [32]byte {
	return benchHashBytes([]byte("condition-from-question"), benchNegRiskAdapter[:], questionID[:])
}

func ctfPositionToken(collateral [20]byte, conditionID [32]byte, outcome uint8) [32]byte {
	return benchHashBytes([]byte("ctf-position"), collateral[:], conditionID[:], []byte{outcome})
}

func fpmmPositionToken(fpmm benchFPMM, outcome uint8) [32]byte {
	return ctfPositionToken(fpmm.collateral, fpmm.conditionID, outcome)
}

func lpShareToken(addr [20]byte) [32]byte {
	return benchHashBytes([]byte("lp-share"), addr[:])
}

func negRiskPositionToken(conditionID [32]byte, outcome uint8) [32]byte {
	return benchHashBytes([]byte("neg-risk-position"), benchWrappedCollateral[:], conditionID[:], []byte{outcome})
}

func negRiskConvertedToken(nr benchNegRiskEvent, marketID [32]byte, questionIndex uint32, outcome uint8) ([32]byte, bool) {
	if questionIndex < uint32(len(nr.questionIDs)) && nr.questionIDs[questionIndex] != benchZeroHash {
		return negRiskPositionToken(conditionFromQuestion(nr.questionIDs[questionIndex]), outcome), true
	}
	return negRiskPositionToken(benchNegRiskConditionID(marketID, questionIndex), outcome), true
}

func benchHash(label string, id uint64) [32]byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], id)
	return benchHashBytes([]byte(label), buf[:])
}

func benchHashBytes(parts ...[]byte) [32]byte {
	raw := crypto.Keccak256(parts...)
	var out [32]byte
	copy(out[:], raw)
	return out
}

func ident(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func mixString(h uint64, s string) uint64 {
	return mixBytes(h, []byte(s))
}

func mixBytes(h uint64, data []byte) uint64 {
	for _, v := range data {
		h ^= uint64(v)
		h *= 1099511628211
	}
	return h
}

func mixUint64(h uint64, v uint64) uint64 {
	h ^= v
	h *= 1099511628211
	h ^= v >> 32
	h *= 1099511628211
	return h
}

func mixInt64(h uint64, v int64) uint64 {
	return mixUint64(h, uint64(v))
}

func benchMin(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func benchMax(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func benchMinU64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

func benchMaxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func benchMax64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func benchHumanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func TestPrefetchModes(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping benchmark in short mode")
	}

	cfg := PrefetchModeConfig{
		Host:          "127.0.0.1",
		Port:          9003,
		User:          "default",
		Password:      "sqd-clickhouse",
		Database:      "loadtest",
		Positions:     1000,
		Events:        10000,
		ResolveChunk:  500,
		PrefetchBatch: 1000,
		Threshold:     100,
		Modes:         *prefetchModeFlag,
		V3:            *prefetchV3Flag,
	}

	ctx := context.Background()
	if err := RunPrefetchModesBenchmark(ctx, cfg); err != nil {
		t.Fatalf("Benchmark failed: %v", err)
	}
}

func BenchmarkPrefetchModes(b *testing.B) {
	cfg := PrefetchModeConfig{
		Host:          "127.0.0.1",
		Port:          9003,
		User:          "default",
		Password:      "sqd-clickhouse",
		Database:      "loadtest",
		Positions:     1000,
		Events:        10000,
		ResolveChunk:  500,
		PrefetchBatch: 1000,
		Threshold:     100,
		Modes:         *prefetchModeFlag,
		V3:            *prefetchV3Flag,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx := context.Background()
		if err := RunPrefetchModesBenchmark(ctx, cfg); err != nil {
			b.Fatalf("Benchmark failed: %v", err)
		}
	}
}
