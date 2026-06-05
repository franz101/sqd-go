package clock

import (
	"fmt"
	"hash/fnv"
	"sync"
	"sync/atomic"
)

// Node tracks the data entry, its byte size, and a single lazy promotion flag.
type Node[K comparable, V any] struct {
	key     K
	value   V
	size    int    // byte size of this entry (key + value). 0 = unmeasured.
	visited uint32 // 1 = Hot/Visited, 0 = Cold/Candidate
	next    *Node[K, V]
	prev    *Node[K, V]
}

// SieveShard represents a single isolated bucket segment of the cache.
type SieveShard[K comparable, V any] struct {
	mu           sync.RWMutex
	items        map[K]*Node[K, V]
	capacity     int   // max item count (used only when maxBytes=0)
	maxBytes     int64 // max total bytes for this shard; 0 = disabled, use capacity instead
	currentBytes int64 // atomically-updated total bytes in this shard
	head         *Node[K, V]
	tail         *Node[K, V]
	hand         *Node[K, V] // Points to the next eviction candidate
}

// SieveCache groups shards to minimize CPU lock contention.
type SieveCache[K comparable, V any] struct {
	shards    []*SieveShard[K, V]
	shardMask uint32
}

// NewSieveCache creates a cache limited by item count per shard.
func NewSieveCache[K comparable, V any](capacity int, shardCount int) *SieveCache[K, V] {
	return newSieveCache[K, V](capacity, shardCount, 0)
}

// NewSieveCacheBytes creates a cache limited by total byte size.
// totalBytes is split evenly across shards. Each shard evicts when its
// share is exceeded. Set() must include the entry size in bytes.
func NewSieveCacheBytes[K comparable, V any](totalBytes int64, shardCount int) *SieveCache[K, V] {
	return newSieveCache[K, V](0, shardCount, totalBytes)
}

func newSieveCache[K comparable, V any](capacity int, shardCount int, totalBytes int64) *SieveCache[K, V] {
	if shardCount&(shardCount-1) != 0 {
		shardCount = 16
	}

	sc := &SieveCache[K, V]{
		shards:    make([]*SieveShard[K, V], shardCount),
		shardMask: uint32(shardCount - 1),
	}

	shardCap := capacity / shardCount
	if shardCap < 2 {
		shardCap = 2
	}
	shardBytes := totalBytes / int64(shardCount)

	for i := 0; i < shardCount; i++ {
		sc.shards[i] = &SieveShard[K, V]{
			items:    make(map[K]*Node[K, V]),
			capacity: shardCap,
			maxBytes: shardBytes,
		}
	}
	return sc
}

func (sc *SieveCache[K, V]) getShard(key K) *SieveShard[K, V] {
	h := fnv.New32a()
	h.Write([]byte(fmt.Sprint(key)))
	idx := h.Sum32() & sc.shardMask
	return sc.shards[idx]
}

// Get handles high-frequency cache hits using an atomic bit swap.
func (sc *SieveCache[K, V]) Get(key K) (V, bool) {
	shard := sc.getShard(key)

	shard.mu.RLock()
	node, exists := shard.items[key]
	shard.mu.RUnlock()

	if !exists {
		var zero V
		return zero, false
	}

	// FAST PATH: Atomic flag flip. No write locks or pointer manipulation.
	atomic.StoreUint32(&node.visited, 1)
	return node.value, true
}

// Set inserts or updates a key. When using a byte-bounded cache created via
// NewSieveCacheBytes, sizeBytes MUST be the total number of bytes the entry
// occupies (key + value serialized). Pass 0 when using item-count-bounded cache.
func (sc *SieveCache[K, V]) Set(key K, value V, sizeBytes int) {
	shard := sc.getShard(key)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	if node, exists := shard.items[key]; exists {
		// Update in-place: adjust byte delta if size changed
		if shard.maxBytes > 0 {
			atomic.AddInt64(&shard.currentBytes, int64(sizeBytes-node.size))
			node.size = sizeBytes
		}
		node.value = value
		atomic.StoreUint32(&node.visited, 1)
		return
	}

	// Evict until there is room
	if shard.maxBytes > 0 {
		for atomic.LoadInt64(&shard.currentBytes)+int64(sizeBytes) > shard.maxBytes {
			if !shard.evictOne() {
				break // no more evictable entries
			}
		}
	} else {
		if len(shard.items) >= shard.capacity {
			shard.evict()
		}
	}

	newNode := &Node[K, V]{key: key, value: value, size: sizeBytes, visited: 0}
	if shard.head == nil {
		shard.head = newNode
		shard.tail = newNode
	} else {
		newNode.next = shard.head
		shard.head.prev = newNode
		shard.head = newNode
	}
	shard.items[key] = newNode

	if shard.maxBytes > 0 {
		atomic.AddInt64(&shard.currentBytes, int64(sizeBytes))
	}
}

// evict cycles the hand pointer through the FIFO list to find one cold node.
// Used when maxBytes == 0 (item-count mode).
func (shard *SieveShard[K, V]) evict() {
	shard.evictOne()
}

// evictOne removes a single cold entry and returns true. Returns false if
// every entry is hot (all visited=1, all got cleared and given second chance).
func (shard *SieveShard[K, V]) evictOne() bool {
	obj := shard.hand
	if obj == nil {
		obj = shard.tail
	}

	start := obj
	for obj != nil {
		if atomic.LoadUint32(&obj.visited) == 1 {
			atomic.StoreUint32(&obj.visited, 0) // Give second chance
			obj = obj.prev
			if obj == nil {
				obj = shard.tail
			}
			// Full loop — everything is hot
			if obj == start {
				return false
			}
		} else {
			// Evict the unvisited candidate
			shard.hand = obj.prev
			if shard.maxBytes > 0 {
				atomic.AddInt64(&shard.currentBytes, -int64(obj.size))
			}
			shard.removeNode(obj)
			delete(shard.items, obj.key)
			return true
		}
	}
	return false
}

func (shard *SieveShard[K, V]) removeNode(node *Node[K, V]) {
	if node.prev != nil {
		node.prev.next = node.next
	} else {
		shard.head = node.next
	}
	if node.next != nil {
		node.next.prev = node.prev
	} else {
		shard.tail = node.prev
	}
}

// CurrentBytes returns the total bytes currently stored across all shards.
func (sc *SieveCache[K, V]) CurrentBytes() int64 {
	var total int64
	for _, shard := range sc.shards {
		total += atomic.LoadInt64(&shard.currentBytes)
	}
	return total
}

func SieveMain() {
	// --- Item-count bounded (original behavior) ---
	cache := NewSieveCache[string, string](10000, 16)
	cache.Set("session_123", "user_data_payload", 0) // sizeBytes ignored in count mode
	if val, found := cache.Get("session_123"); found {
		fmt.Println("Retrieved from SIEVE Cache (count-bounded):", val)
	}

	// --- Byte-bounded: 1 MB total, 16 shards = ~64 KB per shard ---
	byteCache := NewSieveCacheBytes[string, string](1<<20, 16) // 1 MB

	// Set with explicit byte size (key + value lengths, or serialized proto size, etc.)
	key := "block:0xabc123"
	payload := "large_event_payload_here"
	entrySize := len(key) + len(payload) // simplistic; real use = proto.Size()

	byteCache.Set(key, payload, entrySize)
	if val, found := byteCache.Get(key); found {
		fmt.Println("Retrieved from SIEVE Cache (byte-bounded):", val)
	}
	fmt.Printf("Cache usage: %d / %d bytes\n", byteCache.CurrentBytes(), int64(1<<20))
}
