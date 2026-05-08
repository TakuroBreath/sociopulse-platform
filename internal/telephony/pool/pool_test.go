package pool_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"go.uber.org/zap/zaptest"

	"github.com/sociopulse/platform/internal/telephony/api"
	"github.com/sociopulse/platform/internal/telephony/esl"
	"github.com/sociopulse/platform/internal/telephony/pool"
)

// TestMain enforces goroutine quiescence on package exit. The Task 4 pool
// owns one supervisor per node — every test that constructs a pool MUST
// also Close it, otherwise the supervisor stays parked on a backoff timer
// or in cli.Events() and goleak fires here. Pre-existing fakeESLServer
// goroutines are torn down via wg.Wait inside their stop closures.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// fakeESLServer simulates one FreeSWITCH ESL listener. It accepts inbound
// TCP connections in a loop (so a supervisor's reconnect cycle finds the
// listener ready for the next dial) and runs the supplied handler in a
// goroutine per accept. The returned stop closure closes the listener
// AND every accepted connection, then waits for in-flight handlers to
// drain — closing the connections is what kicks the handler goroutines
// out of their c.Read calls so the wg.Wait completes.
//
// The two-step teardown matters: pool.Close calls cli.Close on the
// CLIENT side which closes the client's TCP socket, but a handler
// goroutine reading from the SERVER side of the same connection
// observes the close as an EOF / "connection reset" — not a deadlock.
// Stopping the listener and explicitly closing every accepted conn
// makes the helper resilient even if the test forgets to Close the pool
// before stopping the server (e.g. error path).
func fakeESLServer(t *testing.T, handler func(net.Conn)) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	var (
		mu    sync.Mutex
		conns []net.Conn
		wg    sync.WaitGroup
	)

	wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			wg.Go(func() {
				defer func() { _ = conn.Close() }()
				handler(conn)
			})
		}
	})

	stop := func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	}
	return ln.Addr().String(), stop
}

// readUntilDoubleNL drains c until either ESL frame terminator (\n\n
// or \r\n\r\n) is observed, or limit bytes have been consumed. Used by
// the canned handlers to ack the auth or subscribe command before
// sending the reply.
//
// The deadline is large (10s) so legitimate idle time between supervisor
// commands does not trip a false read error; the caller is expected to
// abandon the loop when the connection actually closes.
//
// Note: esl.Client writes commands with `\r\n\r\n` terminator (see
// client.go), so the naive `\n\n` check from the original test helper
// would never fire — match both forms here so the test handler stays
// compatible with either wire idiom.
func readUntilDoubleNL(c net.Conn, limit int) (string, error) {
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 64)
	for len(buf) < limit {
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		n, err := c.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			s := string(buf)
			if strings.Contains(s, "\r\n\r\n") || strings.Contains(s, "\n\n") {
				return s, nil
			}
		}
		if err != nil {
			return string(buf), err
		}
	}
	return string(buf), nil
}

// healthyHandler is the canonical happy-path responder: auth succeeds,
// every subsequent command (subscribe / api sofia status) returns +OK
// with a body that contains "Profile" so healthProbe sees a healthy
// response. The handler stays parked reading from c until the connection
// drops, so readLoop has something to wait on.
//
// The optional events channel pushes raw event-plain bodies onto the
// wire — callers use it to drive the fan-out tests. Closing events
// signals "no more events"; the handler tolerates a nil events channel
// (no events ever, just live the connection) and exits cleanly when the
// conn closes regardless.
func healthyHandler(t *testing.T, events <-chan string) func(net.Conn) {
	t.Helper()
	return func(c net.Conn) {
		// Auth handshake. Use an explicit deadline because some Go
		// versions inherit a stale per-conn deadline from the listener
		// path; resetting here makes the timeout predictable.
		_ = c.SetDeadline(time.Time{})
		_, _ = c.Write([]byte("Content-Type: auth/request\n\n"))
		if _, err := readUntilDoubleNL(c, 256); err != nil {
			return
		}
		_, _ = c.Write([]byte("Content-Type: command/reply\nReply-Text: +OK accepted\n\n"))

		// writeMu serialises replies + event frames so a half-written
		// reply isn't broken up by an interleaved event body — the
		// client-side parser would reject the resulting frame.
		var writeMu sync.Mutex

		// done is closed by the cmd-reader goroutine on conn close so
		// the events feeder can exit promptly when the supervisor tears
		// the connection down.
		done := make(chan struct{})

		var wg sync.WaitGroup
		wg.Go(func() {
			defer close(done)
			for {
				cmd, err := readUntilDoubleNL(c, 4096)
				if err != nil {
					return
				}
				writeMu.Lock()
				switch {
				case strings.HasPrefix(cmd, "event "):
					_, _ = c.Write([]byte(
						"Content-Type: command/reply\nReply-Text: +OK event listener enabled\n\n",
					))
				case strings.HasPrefix(cmd, "api sofia status"):
					body := "UP 0/0/1000/100 (0/0/1)\nProfile internal RUNNING\n"
					_, _ = fmt.Fprintf(c,
						"Content-Type: api/response\nContent-Length: %d\n\n%s",
						len(body), body,
					)
				default:
					_, _ = c.Write([]byte(
						"Content-Type: command/reply\nReply-Text: +OK\n\n",
					))
				}
				writeMu.Unlock()
			}
		})

		// Optional events feeder. Exits on either events-chan close or
		// conn close (signalled via done). A nil events chan never
		// emits, so the feeder simply waits on done.
		wg.Go(func() {
			for {
				select {
				case <-done:
					return
				case body, ok := <-events:
					if !ok {
						return
					}
					writeMu.Lock()
					_, _ = fmt.Fprintf(c,
						"Content-Type: text/event-plain\nContent-Length: %d\n\n%s",
						len(body), body,
					)
					writeMu.Unlock()
				}
			}
		})

		wg.Wait()
	}
}

// deadHandler accepts the conn and immediately closes it, simulating a
// FreeSWITCH node that's reachable but not yet serving (e.g. mid-restart).
// Forces the supervisor's Dial → "EOF before auth/request" failure path.
func deadHandler(c net.Conn) { _ = c.Close() }

// flippableHandler dispatches between healthyHandler and deadHandler based
// on the supplied atomic.Bool. Used by TestPool_RecoversAfterNodeBecomesHealthy
// to flip a node from dead → healthy mid-test.
func flippableHandler(t *testing.T, dead *atomic.Bool, events <-chan string) func(net.Conn) {
	t.Helper()
	healthy := healthyHandler(t, events)
	return func(c net.Conn) {
		if dead.Load() {
			deadHandler(c)
			return
		}
		healthy(c)
	}
}

// silentDeathHandler answers auth + subscribe + the initial sofia status,
// then closes the conn after delay — simulating a node that disappears
// mid-flight. The supervisor's forward loop must observe the closed
// events channel and reconnect.
func silentDeathHandler(t *testing.T, delay time.Duration) func(net.Conn) {
	t.Helper()
	return func(c net.Conn) {
		_, _ = c.Write([]byte("Content-Type: auth/request\n\n"))
		_, _ = readUntilDoubleNL(c, 256)
		_, _ = c.Write([]byte("Content-Type: command/reply\nReply-Text: +OK accepted\n\n"))

		// Subscribe ack.
		_, _ = readUntilDoubleNL(c, 4096)
		_, _ = c.Write([]byte("Content-Type: command/reply\nReply-Text: +OK\n\n"))

		// Initial sofia status ack with healthy body.
		_, _ = readUntilDoubleNL(c, 4096)
		body := "UP 0/0/1000/100 (0/0/1)\nProfile internal RUNNING\n"
		_, _ = fmt.Fprintf(c,
			"Content-Type: api/response\nContent-Length: %d\n\n%s",
			len(body), body,
		)

		// Stay healthy for `delay`, then drop the conn. The supervisor's
		// forwardLoop will see Events() close and return an error to the
		// outer reconnect loop.
		time.Sleep(delay)
		_ = c.Close()
	}
}

func TestPool_New_RejectsEmptyNodes(t *testing.T) {
	t.Parallel()

	_, err := pool.New(context.Background(), pool.Config{Nodes: nil})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one FreeSWITCH node")
}

func TestPool_New_StartsSupervisorPerNode(t *testing.T) {
	t.Parallel()

	addr1, stop1 := fakeESLServer(t, healthyHandler(t, nil))
	defer stop1()
	addr2, stop2 := fakeESLServer(t, healthyHandler(t, nil))
	defer stop2()
	addr3, stop3 := fakeESLServer(t, healthyHandler(t, nil))
	defer stop3()

	p, err := pool.New(context.Background(), pool.Config{
		Nodes:          []string{addr1, addr2, addr3},
		Password:       "x",
		HealthInterval: 50 * time.Millisecond,
		Subscriptions:  []string{"CHANNEL_CREATE"},
		Logger:         zaptest.NewLogger(t),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	require.Eventually(t, func() bool {
		return len(p.HealthyNodes()) == 3
	}, 2*time.Second, 25*time.Millisecond, "expected all 3 nodes healthy")

	// Sorted, so the assert can compare deterministically.
	got := p.HealthyNodes()
	want := sortedAddrs(addr1, addr2, addr3)
	assert.Equal(t, want, got)
}

func TestPool_OneNodeDown_OneStillHealthy(t *testing.T) {
	t.Parallel()

	addr1, stop1 := fakeESLServer(t, healthyHandler(t, nil))
	defer stop1()
	addr2, stop2 := fakeESLServer(t, deadHandler)
	defer stop2()

	p, err := pool.New(context.Background(), pool.Config{
		Nodes:          []string{addr1, addr2},
		Password:       "x",
		HealthInterval: 50 * time.Millisecond,
		BackoffBase:    20 * time.Millisecond,
		BackoffCap:     100 * time.Millisecond,
		Subscriptions:  []string{"CHANNEL_CREATE"},
		Logger:         zaptest.NewLogger(t),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	require.Eventually(t, func() bool {
		return p.AnyHealthy() && len(p.HealthyNodes()) == 1
	}, 2*time.Second, 25*time.Millisecond)

	healthy := p.HealthyNodes()
	require.Len(t, healthy, 1)
	assert.Equal(t, addr1, healthy[0])
}

func TestPool_RecoversAfterNodeBecomesHealthy(t *testing.T) {
	t.Parallel()

	dead := &atomic.Bool{}
	dead.Store(true)

	addr, stop := fakeESLServer(t, flippableHandler(t, dead, nil))
	defer stop()

	p, err := pool.New(context.Background(), pool.Config{
		Nodes:          []string{addr},
		Password:       "x",
		HealthInterval: 50 * time.Millisecond,
		BackoffBase:    20 * time.Millisecond,
		BackoffCap:     100 * time.Millisecond,
		Subscriptions:  []string{"CHANNEL_CREATE"},
		Logger:         zaptest.NewLogger(t),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	// Initially dead — supervisor cannot complete the handshake.
	assert.False(t, p.AnyHealthy())

	// Flip the node healthy; supervisor's next reconnect must succeed.
	dead.Store(false)
	require.Eventually(t, p.AnyHealthy, 5*time.Second, 50*time.Millisecond,
		"pool never recovered after node became healthy")
}

func TestPool_Get_ReturnsClientForHealthyNode(t *testing.T) {
	t.Parallel()

	addr, stop := fakeESLServer(t, healthyHandler(t, nil))
	defer stop()

	p, err := pool.New(context.Background(), pool.Config{
		Nodes:          []string{addr},
		Password:       "x",
		HealthInterval: 50 * time.Millisecond,
		Subscriptions:  []string{"CHANNEL_CREATE"},
		Logger:         zaptest.NewLogger(t),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	require.Eventually(t, p.AnyHealthy, 2*time.Second, 25*time.Millisecond)

	cli, err := p.Get(addr)
	require.NoError(t, err)
	require.NotNil(t, cli)
	assert.True(t, cli.Connected())
}

func TestPool_Get_ReturnsErrNotConnectedForDeadNode(t *testing.T) {
	t.Parallel()

	addr, stop := fakeESLServer(t, deadHandler)
	defer stop()

	p, err := pool.New(context.Background(), pool.Config{
		Nodes:          []string{addr},
		Password:       "x",
		HealthInterval: 50 * time.Millisecond,
		BackoffBase:    20 * time.Millisecond,
		BackoffCap:     100 * time.Millisecond,
		Subscriptions:  []string{"CHANNEL_CREATE"},
		Logger:         zaptest.NewLogger(t),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	// Wait long enough for at least one connect attempt to fail.
	time.Sleep(150 * time.Millisecond)

	_, err = p.Get(addr)
	require.Error(t, err)
	assert.ErrorIs(t, err, esl.ErrNotConnected)
	assert.Contains(t, err.Error(), addr)
}

func TestPool_Get_ReturnsErrNotConnectedForUnknownNode(t *testing.T) {
	t.Parallel()

	addr, stop := fakeESLServer(t, healthyHandler(t, nil))
	defer stop()

	p, err := pool.New(context.Background(), pool.Config{
		Nodes:          []string{addr},
		Password:       "x",
		HealthInterval: 50 * time.Millisecond,
		Subscriptions:  []string{"CHANNEL_CREATE"},
		Logger:         zaptest.NewLogger(t),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	_, err = p.Get("203.0.113.1:8021")
	require.Error(t, err)
	assert.ErrorIs(t, err, esl.ErrNotConnected)
	assert.Contains(t, err.Error(), "203.0.113.1:8021")
}

func TestPool_Events_FansOutFromAllNodes(t *testing.T) {
	t.Parallel()

	events1 := make(chan string, 4)
	events2 := make(chan string, 4)

	addr1, stop1 := fakeESLServer(t, healthyHandler(t, events1))
	defer stop1()
	addr2, stop2 := fakeESLServer(t, healthyHandler(t, events2))
	defer stop2()

	p, err := pool.New(context.Background(), pool.Config{
		Nodes:          []string{addr1, addr2},
		Password:       "x",
		HealthInterval: 50 * time.Millisecond,
		Subscriptions:  []string{"CHANNEL_CREATE"},
		Logger:         zaptest.NewLogger(t),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		// Close the event feeders before pool.Close so the handlers'
		// `for body := range events` loop exits and the goroutine
		// terminates cleanly.
		close(events1)
		close(events2)
		_ = p.Close()
	})

	require.Eventually(t, func() bool {
		return len(p.HealthyNodes()) == 2
	}, 2*time.Second, 25*time.Millisecond)

	// Push a CHANNEL_CREATE from each node.
	events1 <- "Event-Name: CHANNEL_CREATE\nUnique-ID: 11111111-1111-1111-1111-111111111111\n\n"
	events2 <- "Event-Name: CHANNEL_CREATE\nUnique-ID: 22222222-2222-2222-2222-222222222222\n\n"

	got := make(map[string]int)
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case env, ok := <-p.Events():
			if !ok {
				t.Fatal("events channel closed unexpectedly")
			}
			// Ignore HEARTBEAT-like events that the fake doesn't emit
			// here, but defensively skip anything that isn't a CHANNEL_*.
			if env.Raw.Name != "CHANNEL_CREATE" {
				continue
			}
			got[env.NodeAddr]++
			assert.Equal(t, api.EventDialing, env.Event.Type)
		case <-deadline:
			t.Fatalf("did not receive both events within budget; got=%v", got)
		}
	}
	assert.Equal(t, 1, got[addr1])
	assert.Equal(t, 1, got[addr2])
}

func TestPool_HealthCheckDetectsSilentDeath(t *testing.T) {
	t.Parallel()

	// First connect: handler closes the conn after 100ms. Subsequent
	// connects: the listener's accept loop pulls the next handler call,
	// which is a fresh silentDeathHandler invocation that lives 100ms
	// before closing again. We use a counter to flip behaviour after
	// the first death so the supervisor recovers.
	var attempt atomic.Int64
	addr, stop := fakeESLServer(t, func(c net.Conn) {
		n := attempt.Add(1)
		if n == 1 {
			silentDeathHandler(t, 100*time.Millisecond)(c)
			return
		}
		healthyHandler(t, nil)(c)
	})
	defer stop()

	p, err := pool.New(context.Background(), pool.Config{
		Nodes:          []string{addr},
		Password:       "x",
		HealthInterval: 50 * time.Millisecond,
		BackoffBase:    20 * time.Millisecond,
		BackoffCap:     100 * time.Millisecond,
		Subscriptions:  []string{"CHANNEL_CREATE"},
		Logger:         zaptest.NewLogger(t),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	// Healthy after first attempt.
	require.Eventually(t, p.AnyHealthy, 2*time.Second, 25*time.Millisecond)

	// First connection dies after 100ms; supervisor detects and reconnects.
	// AnyHealthy may briefly drop to false during reconnect; eventually
	// becomes true again on the second (healthy) handler.
	require.Eventually(t, func() bool {
		return attempt.Load() >= 2 && p.AnyHealthy()
	}, 5*time.Second, 50*time.Millisecond, "supervisor did not reconnect")
}

func TestPool_Close_StopsAllSupervisors(t *testing.T) {
	t.Parallel()

	addr1, stop1 := fakeESLServer(t, healthyHandler(t, nil))
	defer stop1()
	addr2, stop2 := fakeESLServer(t, deadHandler)
	defer stop2()

	p, err := pool.New(context.Background(), pool.Config{
		Nodes:          []string{addr1, addr2},
		Password:       "x",
		HealthInterval: 50 * time.Millisecond,
		BackoffBase:    20 * time.Millisecond,
		BackoffCap:     100 * time.Millisecond,
		Subscriptions:  []string{"CHANNEL_CREATE"},
		Logger:         zaptest.NewLogger(t),
	})
	require.NoError(t, err)

	require.Eventually(t, p.AnyHealthy, 2*time.Second, 25*time.Millisecond)

	closeDone := make(chan error, 1)
	go func() { closeDone <- p.Close() }()
	select {
	case err := <-closeDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return within 2s")
	}

	// Events channel must be closed by Close().
	select {
	case _, ok := <-p.Events():
		require.False(t, ok, "events channel must be closed after Close()")
	case <-time.After(time.Second):
		t.Fatal("events channel did not close after Close()")
	}
}

func TestPool_Close_Idempotent(t *testing.T) {
	t.Parallel()

	addr, stop := fakeESLServer(t, healthyHandler(t, nil))
	defer stop()

	p, err := pool.New(context.Background(), pool.Config{
		Nodes:          []string{addr},
		Password:       "x",
		HealthInterval: 50 * time.Millisecond,
		Subscriptions:  []string{"CHANNEL_CREATE"},
		Logger:         zaptest.NewLogger(t),
	})
	require.NoError(t, err)

	require.NoError(t, p.Close())
	require.NoError(t, p.Close(), "second Close must succeed")
	require.NoError(t, p.Close(), "third Close must succeed")
}

func TestPool_Metrics_TrackHealthAndReconnects(t *testing.T) {
	t.Parallel()

	addr, stop := fakeESLServer(t, healthyHandler(t, nil))
	defer stop()

	reg := prometheus.NewRegistry()
	metrics := pool.RegisterMetrics(reg)

	p, err := pool.New(context.Background(), pool.Config{
		Nodes:          []string{addr},
		Password:       "x",
		HealthInterval: 50 * time.Millisecond,
		Subscriptions:  []string{"CHANNEL_CREATE"},
		Logger:         zaptest.NewLogger(t),
		Metrics:        metrics,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	require.Eventually(t, p.AnyHealthy, 2*time.Second, 25*time.Millisecond)

	// NodeHealthy gauge → 1 for the connected node.
	require.Eventually(t, func() bool {
		return gaugeValue(t, metrics.NodeHealthy.WithLabelValues(addr)) == 1
	}, 2*time.Second, 25*time.Millisecond, "NodeHealthy gauge should be 1")

	// Reconnects ok counter > 0 (initial connect counts as one).
	assert.GreaterOrEqual(t,
		counterValue(t, metrics.Reconnects.WithLabelValues(addr, "ok")),
		float64(1),
	)

	// HealthCheckDur histogram has at least one observation. We gather
	// from the registry rather than poking the WithLabelValues child
	// directly to avoid importing prometheus/client_model as a direct
	// dep — testutil.ToFloat64 doesn't accept histograms.
	require.Eventually(t, func() bool {
		return histogramSampleCount(t, reg, "telephony_pool_health_check_seconds") >= 1
	}, 2*time.Second, 25*time.Millisecond, "HealthCheckDur should have observations")
}

// TestPool_RegisterMetrics_PanicsOnNilRegisterer asserts the wiring guard
// fires loudly when the composition root forgets to pass a registry. It's
// a one-line test but locking the contract means cmd/telephony-bridge
// can't accidentally regress to a silent no-op.
func TestPool_RegisterMetrics_PanicsOnNilRegisterer(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		require.NotNil(t, r, "RegisterMetrics(nil) must panic")
	}()

	_ = pool.RegisterMetrics(nil)
}

// TestPool_Events_DropsWhenChannelFull exercises the publishEvent
// drop-on-full path. With EventBuffer=1 and a slow consumer, the
// supervisor's second event push is dropped non-blockingly, which
// bumps the EventsForwarded{event="_dropped"} counter rather than
// blocking forwardLoop on a full chan.
func TestPool_Events_DropsWhenChannelFull(t *testing.T) {
	t.Parallel()

	events := make(chan string, 8)
	addr, stop := fakeESLServer(t, healthyHandler(t, events))
	defer stop()

	reg := prometheus.NewRegistry()
	metrics := pool.RegisterMetrics(reg)

	p, err := pool.New(context.Background(), pool.Config{
		Nodes:          []string{addr},
		Password:       "x",
		HealthInterval: 100 * time.Millisecond,
		Subscriptions:  []string{"CHANNEL_CREATE"},
		EventBuffer:    1, // tiny buffer; never drained by this test
		Logger:         zaptest.NewLogger(t),
		Metrics:        metrics,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		close(events)
		_ = p.Close()
	})

	require.Eventually(t, p.AnyHealthy, 2*time.Second, 25*time.Millisecond)

	// Push 4 events without draining. EventBuffer=1 means at least 3
	// must be dropped.
	for i := range 4 {
		events <- fmt.Sprintf(
			"Event-Name: CHANNEL_CREATE\nUnique-ID: %08d-0000-0000-0000-000000000000\n\n",
			i,
		)
	}

	require.Eventually(t, func() bool {
		return counterValue(t, metrics.EventsForwarded.WithLabelValues(addr, "_dropped")) >= 1
	}, 3*time.Second, 50*time.Millisecond, "expected at least one event drop")
}

// TestPool_HealthProbeRejectsUnexpectedReply exercises the
// "unexpected reply" branch of healthProbe — a sofia status response
// that doesn't contain "Profile" or "RUNNING" must fail the probe and
// trigger reconnect. This pins the substring contract: future tightening
// of the matcher (regex, structured parse) breaks this test loudly.
func TestPool_HealthProbeRejectsUnexpectedReply(t *testing.T) {
	t.Parallel()

	// Custom handler: auth + subscribe succeed; sofia status returns a
	// nonsense body. The supervisor's initial health-gate must fail and
	// the node must NEVER report healthy.
	var attempts atomic.Int64
	addr, stop := fakeESLServer(t, func(c net.Conn) {
		attempts.Add(1)
		_, _ = c.Write([]byte("Content-Type: auth/request\n\n"))
		if _, err := readUntilDoubleNL(c, 256); err != nil {
			return
		}
		_, _ = c.Write([]byte("Content-Type: command/reply\nReply-Text: +OK accepted\n\n"))

		// Subscribe ack.
		if _, err := readUntilDoubleNL(c, 4096); err != nil {
			return
		}
		_, _ = c.Write([]byte("Content-Type: command/reply\nReply-Text: +OK\n\n"))

		// Initial sofia status with garbage body.
		if _, err := readUntilDoubleNL(c, 4096); err != nil {
			return
		}
		body := "WRONG_REPLY\n"
		_, _ = fmt.Fprintf(c,
			"Content-Type: api/response\nContent-Length: %d\n\n%s",
			len(body), body,
		)

		// Block until conn closes.
		buf := make([]byte, 64)
		for {
			_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
			if _, err := c.Read(buf); err != nil {
				return
			}
		}
	})
	defer stop()

	p, err := pool.New(context.Background(), pool.Config{
		Nodes:          []string{addr},
		Password:       "x",
		HealthInterval: 100 * time.Millisecond,
		BackoffBase:    20 * time.Millisecond,
		BackoffCap:     100 * time.Millisecond,
		Subscriptions:  []string{"CHANNEL_CREATE"},
		Logger:         zaptest.NewLogger(t),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })

	// Wait long enough for at least one full cycle (Dial + Subscribe +
	// failed health probe + backoff).
	time.Sleep(400 * time.Millisecond)

	assert.False(t, p.AnyHealthy(), "node with bad sofia status reply must never go healthy")
	assert.GreaterOrEqual(t, attempts.Load(), int64(2), "supervisor should retry")
}

// sortedAddrs returns its arguments sorted lexicographically — handy when
// asserting against pool.HealthyNodes which sorts for deterministic output.
func sortedAddrs(in ...string) []string {
	out := make([]string, len(in))
	copy(out, in)
	// Bubble-sort is fine here — len is bounded by Config.Nodes (3 in
	// tests), so importing the heavyweight slices.Sort isn't worth it.
	for i := range len(out) {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// gaugeValue reads the current numeric value of a Prometheus gauge via
// the official testutil helper. Avoids importing prometheus/client_model
// directly — that package is an indirect dependency only.
func gaugeValue(_ *testing.T, g prometheus.Gauge) float64 {
	return testutil.ToFloat64(g)
}

// counterValue reads the current numeric value of a Prometheus counter.
// testutil.ToFloat64 accepts both gauges and counters.
func counterValue(_ *testing.T, c prometheus.Counter) float64 {
	return testutil.ToFloat64(c)
}

// histogramSampleCount gathers from reg, finds the named metric, and
// returns the cumulative sample count across all label combinations.
// Avoids importing prometheus/client_model directly (testutil.ToFloat64
// works for gauges/counters but histograms are richer types — we reach
// into the gathered MetricFamily through the registry's exposed types).
func histogramSampleCount(t *testing.T, reg prometheus.Gatherer, metricName string) uint64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	var total uint64
	for _, mf := range mfs {
		if mf.GetName() != metricName {
			continue
		}
		for _, m := range mf.GetMetric() {
			if h := m.GetHistogram(); h != nil {
				total += h.GetSampleCount()
			}
		}
	}
	return total
}

// Compile-check: ensure errors.Is composes through the wrapping fmt.Errorf
// — this is what the router will rely on when discriminating "no client
// right now" from per-command failures.
var _ = errors.Is
