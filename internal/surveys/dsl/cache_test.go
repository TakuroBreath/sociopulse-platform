package dsl

import (
	"sync"
	"testing"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
	"github.com/stretchr/testify/require"
)

// TestProgramCache_PutGet exercises the basic put/get cycle: a value
// stored under a key MUST be retrievable until eviction.
func TestProgramCache_PutGet(t *testing.T) {
	t.Parallel()
	c := newProgramCache(8)
	prog, err := expr.Compile("1 + 1")
	require.NoError(t, err)

	c.Put("expr-a", prog)
	got, ok := c.Get("expr-a")
	require.True(t, ok)
	require.Same(t, prog, got)
	require.Equal(t, 1, c.Len())
}

// TestProgramCache_MissReturnsFalse documents the "absent" path: a Get
// for an unknown key returns (nil, false) rather than panicking.
func TestProgramCache_MissReturnsFalse(t *testing.T) {
	t.Parallel()
	c := newProgramCache(2)
	got, ok := c.Get("missing")
	require.False(t, ok)
	require.Nil(t, got)
}

// TestProgramCache_EvictsOldest verifies the LRU eviction order: when
// the cache is full and a new key is inserted, the least-recently-used
// key (the one not touched by either Get or Put) is dropped first.
func TestProgramCache_EvictsOldest(t *testing.T) {
	t.Parallel()
	c := newProgramCache(2)

	progA, err := expr.Compile("1")
	require.NoError(t, err)
	progB, err := expr.Compile("2")
	require.NoError(t, err)
	progC, err := expr.Compile("3")
	require.NoError(t, err)

	c.Put("a", progA)
	c.Put("b", progB)
	// Access "a" so "b" becomes the LRU candidate.
	_, ok := c.Get("a")
	require.True(t, ok)

	c.Put("c", progC) // should evict "b"

	_, ok = c.Get("b")
	require.False(t, ok, "b should have been evicted as the LRU entry")
	_, ok = c.Get("a")
	require.True(t, ok, "a should still be present (recently accessed)")
	_, ok = c.Get("c")
	require.True(t, ok, "c should be present (just inserted)")
	require.Equal(t, 2, c.Len())
}

// TestProgramCache_PutOverwrite verifies that Put with an existing key
// updates the stored program and refreshes the LRU position.
func TestProgramCache_PutOverwrite(t *testing.T) {
	t.Parallel()
	c := newProgramCache(2)

	progA1, err := expr.Compile("1")
	require.NoError(t, err)
	progA2, err := expr.Compile("2")
	require.NoError(t, err)

	c.Put("a", progA1)
	c.Put("a", progA2)
	got, ok := c.Get("a")
	require.True(t, ok)
	require.Same(t, progA2, got)
	require.Equal(t, 1, c.Len())
}

// TestProgramCache_Concurrent runs concurrent Put/Get operations under
// the race detector to expose missing locks. The actual value
// correctness across goroutines is not the contract — survival without
// the race detector firing is.
func TestProgramCache_Concurrent(t *testing.T) {
	t.Parallel()
	c := newProgramCache(64)
	prog, err := expr.Compile("true")
	require.NoError(t, err)

	const workers = 32
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(id int) {
			defer wg.Done()
			key := keyFor(id)
			for j := 0; j < 50; j++ {
				c.Put(key, prog)
				_, _ = c.Get(key)
			}
		}(i)
	}
	wg.Wait()
	require.LessOrEqual(t, c.Len(), 64)
}

// keyFor produces a deterministic cache key for the concurrent test.
// We avoid fmt.Sprintf because the test is hot enough that the
// allocator could mask bugs in the cache itself.
func keyFor(id int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	if id < 0 {
		id = -id
	}
	return string(letters[id%len(letters)])
}

// TestProgramCache_DefaultSize verifies that the constructor floors
// non-positive sizes to a useful default. The contract: zero or
// negative values become 1024 so misconfigured callers don't get a
// no-op cache.
func TestProgramCache_DefaultSize(t *testing.T) {
	t.Parallel()
	c := newProgramCache(0)
	require.Equal(t, 1024, c.cap())

	c2 := newProgramCache(-1)
	require.Equal(t, 1024, c2.cap())
}

// _ keeps the *vm.Program import alive in the test file even after
// refactors. Without it `goimports` would yank the import.
var _ *vm.Program
