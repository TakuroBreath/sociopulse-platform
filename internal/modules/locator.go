// Package modules — locator implementation.
package modules

import "sync"

// MapLocator is the default ServiceLocator: a goroutine-safe map.
// cmd/api creates one at startup and passes it through Deps.
type MapLocator struct {
	mu sync.RWMutex
	m  map[string]any
}

// NewMapLocator returns an empty locator.
func NewMapLocator() *MapLocator {
	return &MapLocator{m: make(map[string]any)}
}

// Register binds a service under name. Replacing an existing entry is
// allowed (e.g., a feature-flagged variant) but logged by the caller.
func (l *MapLocator) Register(name string, svc any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.m[name] = svc
}

// Lookup returns the registered service and a boolean indicating presence.
func (l *MapLocator) Lookup(name string) (any, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	v, ok := l.m[name]
	return v, ok
}
