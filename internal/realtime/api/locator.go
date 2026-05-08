// locator.go declares the keys this module registers in
// modules.ServiceLocator at Register time. Plan 11 Task 7 (HTTP
// handlers) and downstream modules look these up by name.
//
// The constants are exported from the api package — not from the
// realtime package — so consumers can resolve them without taking a
// transitive dependency on internal/realtime/service (where the
// concrete *Hub lives) or on internal/realtime/events (where the
// dispatcher lives).
package api

const (
	// LocatorHub is the locator key for the realtime *service.Hub. The
	// stored value satisfies api.Hub. Plan 11 Task 7's WS handler
	// fetches this to attach freshly-upgraded connections.
	LocatorHub = "realtime.Hub"

	// LocatorConnectionMetrics is the locator key for the realtime
	// *service.Metrics struct (per-connection counters: dropped frames,
	// auth failures, pong misses, rate-limit closures). The WS handler
	// reads this when constructing the Connection so per-conn counters
	// are wired into the production registry.
	LocatorConnectionMetrics = "realtime.ConnectionMetrics"
)
