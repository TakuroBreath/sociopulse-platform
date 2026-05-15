//go:build smoke

package smoke

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// PickFreeAddr asks the kernel for a free TCP port on 127.0.0.1 and
// returns "127.0.0.1:N". The listener is closed immediately — race-prone
// in theory, fine in practice for serial test boots a few seconds apart.
//
// Mirrors cmd/api/main_test.go::pickFreeAddr verbatim so cmd/api's
// existing boot-test pattern carries over to smoke unchanged.
func PickFreeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "smoke: pick free addr")
	addr := l.Addr().String()
	require.NoError(t, l.Close(), "smoke: close pick listener")
	return addr
}

// ListenerReadyChan polls addr in a background goroutine and closes the
// returned channel as soon as TCP-accept succeeds OR the deadline expires.
// The caller selects against this channel + a separate errCh so a boot
// failure surfaces immediately instead of waiting out the polling budget.
//
// The returned channel is never sent on — it is closed as a one-shot
// signal. A nil close after the deadline means "polling gave up"; the
// caller should still inspect err state to disambiguate (or accept that
// the listener never came up and fail the test).
//
// 25 ms tick is a compromise: tight enough to keep cold-boot latency
// low on a healthy CI runner, loose enough to avoid hammering the
// kernel's connect path during the legitimate listen-and-bind window.
//
// Mirrors cmd/api/main_test.go::listenerReadyChan.
func ListenerReadyChan(addr string, timeout time.Duration) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		deadline := time.Now().Add(timeout)
		tick := time.NewTicker(25 * time.Millisecond)
		defer tick.Stop()
		for {
			conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				return
			}
			if time.Now().After(deadline) {
				return
			}
			<-tick.C
		}
	}()
	return ch
}
