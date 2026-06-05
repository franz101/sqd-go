package main

import (
	"sync"
	"sync/atomic"
)

type entry[K comparable, V any] struct {
	key        K
	value      V
	referenced uint32 // accessed atomically (0 or 1)
	inUse      uint32 // accessed atomically (0 = empty/evicting, 1 = in use, 2 = initializing)
}

type EvictFunc[K comparable, V any] func(key K, value V)

// Cache implements a completely Lockless Clock cache (also known as Second Chance).
// It coordinates concurrency without any mutexes using sync.Map and CAS operations on ring slots.
type Cache[K comparable, V any] struct {
	items    sync.Map      // thread-safe lookup map: Key K -> Index uint64
	ring     []entry[K, V] // preallocated circular ring buffer
	capacity uint64
	hand     uint64 // accessed atomically
	size     uint64 // accessed atomically
	onEvict  EvictFunc[K, V]
}

// New creates a new lockless Clock cache with the specified capacity.
func New[K comparable, V any](capacity uint64) *Cache[K, V] {
	return NewWithEvict[K, V](capacity, nil)
}

// NewWithEvict creates a new lockless Clock cache and calls onEvict whenever
// a live slot is replaced by the CLOCK hand.
func NewWithEvict[K comparable, V any](capacity uint64, onEvict EvictFunc[K, V]) *Cache[K, V] {
	if capacity == 0 {
		capacity = 1
	}
	return &Cache[K, V]{
		ring:     make([]entry[K, V], capacity),
		capacity: capacity,
		onEvict:  onEvict,
	}
}

// Set adds or updates a key-value pair in the cache locklessly.
func (c *Cache[K, V]) Set(key K, value V) {
	// 1. Check if key already exists
	if idxVal, ok := c.items.Load(key); ok {
		idx := idxVal.(uint64)
		e := &c.ring[idx]
		if atomic.LoadUint32(&e.inUse) == 1 {
			if e.key == key {
				e.value = value
				atomic.StoreUint32(&e.referenced, 1)
				return
			}
		}
	}

	// 2. Miss path: Find a slot to claim or evict
	for {
		hand := atomic.AddUint64(&c.hand, 1)
		idx := (hand - 1) % c.capacity
		e := &c.ring[idx]

		// Case A: Slot is in use. Try to CAS inUse from 1 to 0 (claiming eviction lockout)
		if atomic.CompareAndSwapUint32(&e.inUse, 1, 0) {
			// Check if slot has second chance
			if atomic.LoadUint32(&e.referenced) == 1 {
				// Clear reference bit and give second chance
				atomic.StoreUint32(&e.referenced, 0)
				// Release slot back
				atomic.StoreUint32(&e.inUse, 1)
				continue
			}

			// Evict!
			oldKey := e.key
			oldValue := e.value
			c.items.Delete(e.key)
			if c.onEvict != nil {
				c.onEvict(oldKey, oldValue)
			}

			// Overwrite slot
			e.key = key
			e.value = value
			atomic.StoreUint32(&e.referenced, 0)

			c.items.Store(key, idx)
			atomic.StoreUint32(&e.inUse, 1)
			return
		}

		// Case B: Slot is empty (inUse == 0). Try to claim it
		if atomic.LoadUint32(&e.inUse) == 0 {
			if atomic.CompareAndSwapUint32(&e.inUse, 0, 2) {
				// Claimed empty slot
				e.key = key
				e.value = value
				atomic.StoreUint32(&e.referenced, 0)

				c.items.Store(key, idx)
				atomic.AddUint64(&c.size, 1)
				atomic.StoreUint32(&e.inUse, 1)
				return
			}
		}
	}
}

// Get retrieves a value from the cache and sets its reference bit locklessly.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	idxVal, ok := c.items.Load(key)
	if !ok {
		var zero V
		return zero, false
	}

	idx := idxVal.(uint64)
	e := &c.ring[idx]

	if atomic.LoadUint32(&e.inUse) == 1 {
		// Key validation to prevent returning data from concurrently evicted/reused slots
		if e.key == key {
			atomic.StoreUint32(&e.referenced, 1)
			return e.value, true
		}
	}

	var zero V
	return zero, false
}

// Peek retrieves a value without setting the reference bit locklessly.
func (c *Cache[K, V]) Peek(key K) (V, bool) {
	idxVal, ok := c.items.Load(key)
	if !ok {
		var zero V
		return zero, false
	}

	idx := idxVal.(uint64)
	e := &c.ring[idx]

	if atomic.LoadUint32(&e.inUse) == 1 {
		if e.key == key {
			return e.value, true
		}
	}

	var zero V
	return zero, false
}

// Delete removes a key from the cache locklessly.
func (c *Cache[K, V]) Delete(key K) bool {
	idxVal, ok := c.items.Load(key)
	if !ok {
		return false
	}

	idx := idxVal.(uint64)
	e := &c.ring[idx]

	if atomic.CompareAndSwapUint32(&e.inUse, 1, 0) {
		if e.key == key {
			c.items.Delete(key)
			var zeroKey K
			var zeroVal V
			e.key = zeroKey
			e.value = zeroVal
			atomic.StoreUint32(&e.referenced, 0)
			atomic.AddUint64(&c.size, ^uint64(0)) // Decrement size
			return true
		}
		// Restore if key did not match
		atomic.StoreUint32(&e.inUse, 1)
	}

	return false
}

// Range calls fn for each live entry. Iteration order is unspecified and may
// observe concurrent writes, so callers that need a stable view should provide
// their own external synchronization.
func (c *Cache[K, V]) Range(fn func(key K, value V) bool) {
	if fn == nil {
		return
	}
	c.items.Range(func(keyAny, idxAny any) bool {
		key, ok := keyAny.(K)
		if !ok {
			return true
		}
		idx, ok := idxAny.(uint64)
		if !ok || idx >= c.capacity {
			return true
		}
		e := &c.ring[idx]
		if atomic.LoadUint32(&e.inUse) == 1 && e.key == key {
			return fn(key, e.value)
		}
		return true
	})
}

// Len returns the current number of items stored in the cache.
func (c *Cache[K, V]) Len() uint64 {
	return atomic.LoadUint64(&c.size)
}

type EntityCache[K comparable, V any] struct {
	name  string
	cache *Cache[K, V]
}

func NewEntityCache[K comparable, V any](name string, capacity uint64) *EntityCache[K, V] {
	return NewEntityCacheWithEvict[K, V](name, capacity, nil)
}

func NewEntityCacheWithEvict[K comparable, V any](name string, capacity uint64, onEvict EvictFunc[K, V]) *EntityCache[K, V] {
	return &EntityCache[K, V]{
		name:  name,
		cache: NewWithEvict[K, V](capacity, onEvict),
	}
}

func (c *EntityCache[K, V]) Name() string {
	if c == nil {
		return ""
	}
	return c.name
}

func (c *EntityCache[K, V]) Set(key K, value V) {
	c.cache.Set(key, value)
}

func (c *EntityCache[K, V]) Get(key K) (V, bool) {
	return c.cache.Get(key)
}

func (c *EntityCache[K, V]) Peek(key K) (V, bool) {
	return c.cache.Peek(key)
}

func (c *EntityCache[K, V]) Delete(key K) bool {
	return c.cache.Delete(key)
}

func (c *EntityCache[K, V]) Range(fn func(key K, value V) bool) {
	c.cache.Range(fn)
}

func (c *EntityCache[K, V]) Len() uint64 {
	return c.cache.Len()
}
