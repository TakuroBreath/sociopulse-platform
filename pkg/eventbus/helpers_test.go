package eventbus

import (
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

// startEmbeddedJetStream boots an in-process NATS server with JetStream
// enabled on a random TCP port. The store directory lives under
// t.TempDir() so each test gets an isolated stream namespace and
// cleanup is automatic. Returns the client URL (nats://host:port) and
// registers t.Cleanup for graceful shutdown.
//
// The server is fully in-process — no Docker, no external NATS
// install required. Boot time is typically under 200ms.
func startEmbeddedJetStream(t *testing.T) string {
	t.Helper()

	storeDir := filepath.Join(t.TempDir(), "jetstream")

	opts := &server.Options{
		Host:                  "127.0.0.1",
		Port:                  -1, // random port
		NoLog:                 true,
		NoSigs:                true,
		MaxControlLine:        4096,
		DisableShortFirstPing: true,
		JetStream:             true,
		StoreDir:              storeDir,
	}

	srv, err := server.NewServer(opts)
	require.NoError(t, err, "failed to construct embedded NATS server")

	go srv.Start()

	if !srv.ReadyForConnections(5 * time.Second) {
		srv.Shutdown()
		srv.WaitForShutdown()
		t.Fatal("embedded NATS server did not become ready in 5s")
	}

	t.Cleanup(func() {
		srv.Shutdown()
		srv.WaitForShutdown()
	})

	return srv.ClientURL()
}

// ensureStream provisions a JetStream stream for the given subjects on
// the supplied URL. Tests that don't go through the production
// Publisher (which auto-creates streams in real life via the
// nats-bridge) need this so the broker actually persists messages.
//
// The stream is created with InterestPolicy retention so messages are
// dropped once all interested consumers have ack'd them — keeps the
// embedded JetStream store small across hundreds of tests.
func ensureStream(t *testing.T, url, name string, subjects []string) {
	t.Helper()

	nc, err := nats.Connect(url)
	require.NoError(t, err)
	defer nc.Close()

	js, err := nc.JetStream()
	require.NoError(t, err)

	cfg := &nats.StreamConfig{
		Name:      name,
		Subjects:  subjects,
		Retention: nats.InterestPolicy,
		Storage:   nats.MemoryStorage,
		MaxAge:    1 * time.Minute,
	}
	if _, err := js.AddStream(cfg); err != nil {
		// If it already exists from a prior test that recycled the
		// store dir (shouldn't happen with t.TempDir but be safe),
		// update it instead.
		if errors.Is(err, nats.ErrStreamNameAlreadyInUse) {
			_, err = js.UpdateStream(cfg)
		}
		require.NoError(t, err, "ensure stream %q", name)
	}
}

// freePort returns a TCP port currently free on 127.0.0.1. Used by
// tests that need to point a constructor at an URL whose backing
// process refuses the connection (e.g. closed-connection guards).
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	addr, ok := l.Addr().(*net.TCPAddr)
	require.True(t, ok, "expected *net.TCPAddr from local listener")
	return addr.Port
}

// counterValue gathers the metric named `name` from reg, finds the
// timeseries matching the given label key=value pair, and returns its
// counter value. Fatals the test if not found.
func counterValue(t *testing.T, reg *prometheus.Registry, name, labelKey, labelValue string) float64 {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		for _, mp := range fam.GetMetric() {
			match := false
			for _, lp := range mp.GetLabel() {
				if lp.GetName() == labelKey && lp.GetValue() == labelValue {
					match = true
					break
				}
			}
			if match {
				return mp.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("counter %q with %s=%s not found", name, labelKey, labelValue)
	return 0
}

// counterValueOrZero is the non-fatal sibling of counterValue: returns
// 0 when the (name, label) combination doesn't yet exist in reg.
// Used in poll-loops where the metric will appear after a brief
// async window.
func counterValueOrZero(t *testing.T, reg *prometheus.Registry, name, labelKey, labelValue string) float64 {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		for _, mp := range fam.GetMetric() {
			match := false
			for _, lp := range mp.GetLabel() {
				if lp.GetName() == labelKey && lp.GetValue() == labelValue {
					match = true
					break
				}
			}
			if match {
				return mp.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// histogramSampleCount returns the cumulative observation count for
// the named histogram in reg. Used to assert "we did at least one
// observe()" without depending on bucket layout.
func histogramSampleCount(t *testing.T, reg *prometheus.Registry, name string) uint64 {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		var total uint64
		for _, mp := range fam.GetMetric() {
			total += mp.GetHistogram().GetSampleCount()
		}
		return total
	}
	t.Fatalf("histogram %q not found", name)
	return 0
}

// awaitOK polls fn until it returns nil or the deadline expires. Used
// in tests where JetStream redelivery + queue routing introduce
// non-determinism on the receive side.
func awaitOK(t *testing.T, deadline time.Duration, fn func() error) {
	t.Helper()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	to := time.NewTimer(deadline)
	defer to.Stop()
	var lastErr error
	for {
		err := fn()
		if err == nil {
			return
		}
		lastErr = err
		select {
		case <-tick.C:
			continue
		case <-to.C:
			t.Fatalf("awaitOK: deadline exceeded after %s: %v", deadline, lastErr)
		}
	}
}
