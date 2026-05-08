package runtime

import (
	"container/list"
	"crypto/sha256"
	"sync"
)

// schemaCache is a fixed-size LRU keyed by sha256(schema bytes) and
// storing parsed *schemaDoc values plus a precomputed slice of node
// ids (used to seed the DSL env for forward-ref-free expressions).
// The zero value is not usable; always construct via newSchemaCache.
//
// Why sha256? The runtime's contract takes raw bytes — different
// callers may submit byte-slices that point at different memory but
// represent the same canonical JSON. Hashing collapses them to a
// single cache slot and avoids re-parsing per call. SHA-256 is
// collision-resistant and the digest cost (~MB/s) is negligible
// against a JSON parse.
//
// Methods are mutex-guarded and safe for concurrent use. Operations
// are O(1) on average.
type schemaCache struct {
	mu       sync.Mutex
	capacity int
	ll       *list.List
	idx      map[[sha256.Size]byte]*list.Element
}

// schemaCacheEntry is the value stored on the LRU list. The key
// duplicates the map key so eviction (which only sees a list element)
// can drop the matching map entry too.
type schemaCacheEntry struct {
	key  [sha256.Size]byte
	doc  *schemaDoc
	keys []string // sorted list of node ids in `doc`, precomputed
}

// newSchemaCache returns a schemaCache with the given capacity. A non-
// positive capacity is treated as the project default (256), which
// comfortably fits a few dozen surveys × a couple of versions each.
//
// Callers who don't want caching at all (e.g. some pathological
// debugging path) construct the Runtime with cacheSize = -1 and
// receive a nil cache pointer; runtime methods skip the cache when
// nil.
func newSchemaCache(capacity int) *schemaCache {
	if capacity == 0 {
		capacity = 256
	}
	if capacity < 0 {
		// Sentinel for "no cache". The constructor returns nil so the
		// runtime fast-paths the lookup on nil receiver.
		return nil
	}
	return &schemaCache{
		capacity: capacity,
		ll:       list.New(),
		idx:      make(map[[sha256.Size]byte]*list.Element, capacity),
	}
}

// get returns the cached entry for key (sha256 digest), refreshing
// its LRU position. Returns (nil, false) when the key is absent. Safe
// to call on a nil receiver — the nil-cache fast path returns a miss.
func (c *schemaCache) get(key [sha256.Size]byte) (*schemaCacheEntry, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.idx[key]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(elem)
	entry, ok := elem.Value.(*schemaCacheEntry)
	if !ok {
		// Defensive: corrupt entry. Treat as miss and clean up so the
		// next put doesn't double the corruption.
		c.ll.Remove(elem)
		delete(c.idx, key)
		return nil, false
	}
	return entry, true
}

// put inserts the entry for key. When inserting would exceed
// capacity, the least-recently-used entry is evicted. If the key is
// already present, the existing entry is left untouched (since the
// key is sha256(schema) the contents are by definition identical, so
// overwriting would risk a data race against concurrent readers
// holding the cached *schemaDoc). The LRU position is still
// refreshed via MoveToFront so a concurrent put doesn't penalise the
// hot entry.
//
// Safe to call on a nil receiver — the nil-cache fast path is a
// no-op (the runtime parses every call when caching is off).
func (c *schemaCache) put(key [sha256.Size]byte, doc *schemaDoc, keys []string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.idx[key]; ok {
		c.ll.MoveToFront(elem)
		// Do NOT mutate entry.doc / entry.keys: a concurrent get
		// returned the existing pointer to its caller, which may now
		// be reading those fields without holding c.mu. The contents
		// are identical anyway (key is the content digest) so the
		// only effect of the rewrite would be the race itself.
		return
	}
	entry := &schemaCacheEntry{key: key, doc: doc, keys: keys}
	elem := c.ll.PushFront(entry)
	c.idx[key] = elem
	if c.ll.Len() > c.capacity {
		c.evictOldest()
	}
}

// Len returns the number of cached entries. Exposed (capitalised) so
// tests can assert hit/miss behaviour without reaching into the
// internals. Safe on nil receiver — returns 0.
func (c *schemaCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// evictOldest removes the LRU tail. Caller must hold c.mu.
func (c *schemaCache) evictOldest() {
	tail := c.ll.Back()
	if tail == nil {
		return
	}
	if entry, ok := tail.Value.(*schemaCacheEntry); ok {
		delete(c.idx, entry.key)
	}
	c.ll.Remove(tail)
}

// hashSchema returns the sha256 digest of the schema bytes. Pulled
// out of the cache methods so the runtime can compute the digest once
// per call (lookup → hit/miss → parse → put) without repeating the
// hash op.
func hashSchema(schema []byte) [sha256.Size]byte {
	return sha256.Sum256(schema)
}
