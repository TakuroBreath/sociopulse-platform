package modules

import "testing"

func TestMapLocatorRoundTrip(t *testing.T) {
	t.Parallel()
	l := NewMapLocator()
	l.Register("foo", 42)
	v, ok := l.Lookup("foo")
	if !ok {
		t.Fatal("expected foo registered")
	}
	if v.(int) != 42 {
		t.Fatalf("got %v, want 42", v)
	}
	if _, ok := l.Lookup("missing"); ok {
		t.Fatal("expected missing not registered")
	}
}

// _ = Module — compile-only assertion; the interface is consumed by
// internal/<module>/module.go in Plan 02.
