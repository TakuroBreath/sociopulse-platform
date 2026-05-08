package dsl

import (
	"container/list"
	"sync"

	"github.com/expr-lang/expr/vm"
)

// programCache is a fixed-size LRU cache keyed by raw expression
// string and storing compiled [vm.Program] values. The zero value is
// not usable — always go through [newProgramCache] which enforces a
// positive capacity (defaulting to 1024 for non-positive inputs).
//
// The cache is mutex-guarded and safe to use from multiple goroutines.
// All operations are O(1) on average.
type programCache struct {
	mu       sync.Mutex
	capacity int
	ll       *list.List
	idx      map[string]*list.Element
}

// cacheEntry is the value type stored in the doubly-linked list. We
// store the key alongside the value so eviction (which only sees a
// list element) can remove the corresponding map entry.
type cacheEntry struct {
	key  string
	prog *vm.Program
}

// newProgramCache returns a programCache with the given capacity. A
// non-positive capacity is treated as 1024 (the project default; the
// number is large enough that even a survey with hundreds of
// expressions won't see eviction in steady state).
func newProgramCache(capacity int) *programCache {
	if capacity <= 0 {
		capacity = 1024
	}
	return &programCache{
		capacity: capacity,
		ll:       list.New(),
		idx:      make(map[string]*list.Element, capacity),
	}
}

// Get returns the cached program for key, marking it as most recently
// used. The second return value is false when the key isn't present.
func (c *programCache) Get(key string) (*vm.Program, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.idx[key]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(elem)
	entry, ok := elem.Value.(*cacheEntry)
	if !ok {
		// Defensive: list element of unexpected type. Treat as miss
		// so the caller falls back to recompilation; clean up the
		// corrupt entry so the next Put/Get isn't poisoned.
		c.ll.Remove(elem)
		delete(c.idx, key)
		return nil, false
	}
	return entry.prog, true
}

// Put inserts or updates the program for key. If the key already
// exists, its value is overwritten and its LRU position refreshed.
// When inserting a new key would exceed capacity, the least-recently-
// used entry is evicted first.
func (c *programCache) Put(key string, prog *vm.Program) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.idx[key]; ok {
		c.ll.MoveToFront(elem)
		if entry, ok := elem.Value.(*cacheEntry); ok {
			entry.prog = prog
		}
		return
	}
	entry := &cacheEntry{key: key, prog: prog}
	elem := c.ll.PushFront(entry)
	c.idx[key] = elem
	if c.ll.Len() > c.capacity {
		c.evictOldest()
	}
}

// Len returns the number of cached entries.
func (c *programCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// cap returns the configured capacity. Exposed (lower-case so it
// stays package-private) for tests that assert default sizing.
func (c *programCache) cap() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capacity
}

// evictOldest removes the tail of the LRU list. Caller must hold c.mu.
func (c *programCache) evictOldest() {
	tail := c.ll.Back()
	if tail == nil {
		return
	}
	if entry, ok := tail.Value.(*cacheEntry); ok {
		delete(c.idx, entry.key)
	}
	c.ll.Remove(tail)
}
