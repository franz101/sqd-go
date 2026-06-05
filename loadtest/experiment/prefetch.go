// Prototype for automatic prefetch discovery using dry-run mode
// Concept: Run handler in dry-run mode to track Get() calls, then batch fetch, then process for real

package main

import (
	"context"
	"fmt"
	"sync"
)

// ===== User API (Simple as before) =====

// User code stays simple - just call state.Position.Get()
// No config needed for which entities to fetch
func ProcessUserHandler(state *State, block *Block) error {
	for _, ev := range block.Events {
		switch ev.Type {
		case "OrderFilled":
			handleOrderFilled(state, ev)
		case "PositionSplit":
			handlePositionSplit(state, ev)
		}
	}
	return nil
}

func handleOrderFilled(state *State, ev Event) {
	user := ev.Data["user"].(string)
	tokenID := ev.Data["token_id"].(string)
	// This Get() call will be tracked in dry-run mode
	_, ok := state.Position.Get(user, tokenID)
	if !ok {
		// Handle miss...
	}
	fmt.Printf("  Processed order for user=%s token=%s pos_exists=%v\n", user, tokenID, ok)
}

func handlePositionSplit(state *State, ev Event) {
	conditionID := ev.Data["condition_id"].(string)
	cond, ok := state.Condition.Get(conditionID)
	if ok {
		fmt.Printf("  Found condition: %s\n", cond.ID)
	}
}

// ===== Framework API (Prefetch support) =====

type PrefetchConfig struct {
	Enabled      bool
	BatchSize    int // Events per prefetch window
	ResolveChunk int // Keys per ClickHouse query
}

type PrefetchResult struct {
	PrefetchedEntities map[string]int // Entity name -> key count
	TotalKeys          int
	CacheHitRate       float64
	QueriesCount       int
	DryRunGets         int
	RealRunGets        int
}

// State with dry-run mode support
type State struct {
	// Hot caches (as before)
	Position  *PositionState
	Condition *ConditionState

	// Dry-run mode tracking
	dryRunMode bool
	dryRunKeys map[string][]Key // entity name -> list of keys accessed
	mu         sync.Mutex
}

func (s *State) EnterDryRunMode() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dryRunMode = true
	s.dryRunKeys = make(map[string][]Key)
}

func (s *State) ExitDryRunMode() map[string][]Key {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dryRunMode = false
	keys := s.dryRunKeys
	s.dryRunKeys = nil
	return keys
}

func (s *State) IsDryRunMode() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dryRunMode
}

func (s *State) TrackDryRunKey(entity string, key Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dryRunMode {
		// Check for duplicates
		for _, k := range s.dryRunKeys[entity] {
			if k.Equals(key) {
				return
			}
		}
		s.dryRunKeys[entity] = append(s.dryRunKeys[entity], key)
	}
}

// Position state with dry-run support
type PositionState struct {
	state     *State
	cache     map[string]Position
	gets      int
	cacheHits int
}

type Position struct {
	User    string
	TokenID string
	Amount  uint64
}

func (p *PositionState) Get(user, tokenID string) (*Position, bool) {
	p.gets++
	key := PositionKey(user, tokenID)

	// In dry-run mode, just track the key
	if p.state.IsDryRunMode() {
		p.state.TrackDryRunKey("Position", key)
		return nil, false // Always return miss in dry-run mode
	}

	// Normal mode: check cache first using string key
	if val, ok := p.cache[key.String()]; ok {
		p.cacheHits++
		return &val, true
	}

	// Cache miss - would normally do point query here
	// In prefetch mode, this should be a cache hit
	return nil, false
}

func (p *PositionState) Stats() (gets, hits int) {
	return p.gets, p.cacheHits
}

// Condition state with dry-run support
type ConditionState struct {
	state     *State
	cache     map[string]Condition
	gets      int
	cacheHits int
}

type Condition struct {
	ID   string
	Data string
}

func (c *ConditionState) Get(id string) (*Condition, bool) {
	c.gets++
	key := ConditionKey(id)

	// In dry-run mode, just track the key
	if c.state.IsDryRunMode() {
		c.state.TrackDryRunKey("Condition", key)
		return nil, false
	}

	// Normal mode: check cache using string key
	if val, ok := c.cache[key.String()]; ok {
		c.cacheHits++
		return &val, true
	}
	return nil, false
}

// Key types
type Key interface {
	Equals(other Key) bool
	String() string
}

type positionKey struct {
	user    string
	tokenID string
}

func PositionKey(user, tokenID string) Key {
	return &positionKey{user, tokenID}
}

func (k *positionKey) Equals(other Key) bool {
	if o, ok := other.(*positionKey); ok {
		return k.user == o.user && k.tokenID == o.tokenID
	}
	return false
}

func (k *positionKey) String() string {
	return fmt.Sprintf("Position(%s,%s)", k.user, k.tokenID)
}

type conditionKey struct {
	id string
}

func ConditionKey(id string) Key {
	return &conditionKey{id: id}
}

func (k *conditionKey) Equals(other Key) bool {
	if o, ok := other.(*conditionKey); ok {
		return k.id == o.id
	}
	return false
}

func (k *conditionKey) String() string {
	return fmt.Sprintf("Condition(%s)", k.id)
}

// ===== Framework: ProcessWithPrefetch =====

func ProcessWithPrefetch(ctx context.Context, state *State, block *Block, handler func(*State, *Block) error, cfg PrefetchConfig) (*PrefetchResult, error) {
	if !cfg.Enabled {
		// Process normally (no prefetch)
		fmt.Println("[Prefetch] Disabled - processing normally")
		if err := handler(state, block); err != nil {
			return nil, err
		}
		return &PrefetchResult{}, nil
	}

	result := &PrefetchResult{
		PrefetchedEntities: make(map[string]int),
	}

	// Phase 1: Dry run - discover which entities to fetch
	fmt.Println("[Prefetch] Phase 1: Dry run - discovering entities to fetch...")
	state.EnterDryRunMode()
	if err := handler(state, block); err != nil {
		state.ExitDryRunMode()
		return nil, fmt.Errorf("dry run failed: %w", err)
	}
	discoveredKeys := state.ExitDryRunMode()

	// Count discovered keys
	fmt.Println("[Prefetch] Discovered keys:")
	for entity, keys := range discoveredKeys {
		count := len(keys)
		result.PrefetchedEntities[entity] = count
		result.TotalKeys += count
		fmt.Printf("  %s: %d keys\n", entity, count)
	}
	result.DryRunGets = state.Position.gets + state.Condition.gets

	// Phase 2: Batch fetch all discovered keys
	fmt.Println("[Prefetch] Phase 2: Batch fetching all discovered keys...")
	if err := batchFetchAll(ctx, state, discoveredKeys, cfg.ResolveChunk, result); err != nil {
		return nil, fmt.Errorf("batch fetch failed: %w", err)
	}

	// Phase 3: Process for real with cache filled
	fmt.Println("[Prefetch] Phase 3: Processing with cache filled...")
	if err := handler(state, block); err != nil {
		return nil, fmt.Errorf("real process failed: %w", err)
	}
	result.RealRunGets = state.Position.gets + state.Condition.gets

	// Calculate stats
	totalGets := result.RealRunGets
	if totalGets > 0 {
		totalHits := state.Position.cacheHits + state.Condition.cacheHits
		result.CacheHitRate = float64(totalHits) / float64(totalGets)
	}

	// Print summary
	fmt.Println("[Prefetch] Summary:")
	fmt.Printf("  Total keys prefetched: %d\n", result.TotalKeys)
	fmt.Printf("  Queries made: %d\n", result.QueriesCount)
	fmt.Printf("  Cache hit rate: %.1f%%\n", result.CacheHitRate*100)
	fmt.Printf("  Dry-run gets: %d\n", result.DryRunGets)
	fmt.Printf("  Real-run gets: %d\n", result.RealRunGets)

	return result, nil
}

// Batch fetch all discovered keys
func batchFetchAll(ctx context.Context, state *State, discoveredKeys map[string][]Key, resolveChunk int, result *PrefetchResult) error {
	for entity, keys := range discoveredKeys {
		fmt.Printf("[Prefetch] Batch fetching %d keys for %s...\n", len(keys), entity)
		for i := 0; i < len(keys); i += resolveChunk {
			end := i + resolveChunk
			if end > len(keys) {
				end = len(keys)
			}
			chunk := keys[i:end]

			// Simulate batch fetch query
			fmt.Printf("  Query %d: fetching %d keys\n", result.QueriesCount+1, len(chunk))
			batchFetchKeys(ctx, state, entity, chunk)

			result.QueriesCount++
		}
	}
	return nil
}

// Simulate batch fetch from ClickHouse
func batchFetchKeys(ctx context.Context, state *State, entity string, keys []Key) {
	for _, key := range keys {
		// Simulate loading from ClickHouse and populating cache
		keyStr := key.String()
		switch entity {
		case "Position":
			if pk, ok := key.(*positionKey); ok {
				// Simulate finding position in DB
				state.Position.cache[keyStr] = Position{
					User:    pk.user,
					TokenID: pk.tokenID,
					Amount:  1000,
				}
			}
		case "Condition":
			if ck, ok := key.(*conditionKey); ok {
				state.Condition.cache[keyStr] = Condition{
					ID:   ck.id,
					Data: "condition_data",
				}
			}
		}
	}
}

// ===== Test Data =====

type Block struct {
	Events []Event
}

type Event struct {
	Type string
	Data map[string]any
}

func main() {
	fmt.Println("===== Prefetch Prototype Demo =====")

	// Create test block with events that access various entities
	block := &Block{
		Events: []Event{
			{
				Type: "OrderFilled",
				Data: map[string]any{
					"user":     "alice",
					"token_id": "token_1",
				},
			},
			{
				Type: "OrderFilled",
				Data: map[string]any{
					"user":     "bob",
					"token_id": "token_2",
				},
			},
			{
				Type: "OrderFilled",
				Data: map[string]any{
					"user":     "alice",   // Duplicate user
					"token_id": "token_1", // Should dedupe
				},
			},
			{
				Type: "PositionSplit",
				Data: map[string]any{
					"condition_id": "cond_1",
				},
			},
			{
				Type: "PositionSplit",
				Data: map[string]any{
					"condition_id": "cond_2",
				},
			},
		},
	}

	// Create state with caches
	posState := &PositionState{
		cache: make(map[string]Position),
	}
	condState := &ConditionState{
		cache: make(map[string]Condition),
	}
	state := &State{
		Position:  posState,
		Condition: condState,
	}
	posState.state = state
	condState.state = state

	// Configure prefetch
	cfg := PrefetchConfig{
		Enabled:      true,
		BatchSize:    1000,
		ResolveChunk: 2, // Small chunk for demo
	}

	// Run with prefetch
	result, err := ProcessWithPrefetch(context.Background(), state, block, ProcessUserHandler, cfg)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	// Show detailed stats
	fmt.Println("\n===== Detailed Stats =====")
	fmt.Printf("Prefetched Entities:\n")
	for entity, count := range result.PrefetchedEntities {
		fmt.Printf("  %s: %d keys\n", entity, count)
	}
	fmt.Printf("\nQuery Efficiency:\n")
	fmt.Printf("  Without prefetch: ~%d point queries (one per Get)\n", result.RealRunGets)
	fmt.Printf("  With prefetch: %d batch queries\n", result.QueriesCount)
	fmt.Printf("  Query reduction: %.1fx\n", float64(result.RealRunGets)/float64(result.QueriesCount))

	fmt.Println("\n===== Verify Correctness =====")
	fmt.Printf("Position gets: %d (hits: %d)\n", state.Position.gets, state.Position.cacheHits)
	fmt.Printf("Condition gets: %d (hits: %d)\n", state.Condition.gets, state.Condition.cacheHits)
}
