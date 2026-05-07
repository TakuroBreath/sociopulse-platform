package checks

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeNATS struct {
	connected bool
	status    int
}

func (f fakeNATS) IsConnected() bool { return f.connected }
func (f fakeNATS) Status() int       { return f.status }

func TestNATSCheckOK(t *testing.T) {
	t.Parallel()
	c := NATSCheck{Conn: fakeNATS{connected: true}}
	require.NoError(t, c.Check(context.Background()))
	assert.Equal(t, "nats", c.Name())
}

func TestNATSCheckDown(t *testing.T) {
	t.Parallel()
	c := NATSCheck{Conn: fakeNATS{connected: false, status: 2}}
	err := c.Check(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nats")
	assert.Contains(t, err.Error(), "status=2")
}

func TestNATSCheckContextCancelled(t *testing.T) {
	t.Parallel()
	c := NATSCheck{Conn: fakeNATS{connected: true}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// With a pre-cancelled ctx, the inner timeout is also cancelled. The
	// goroutine may still race to deliver a result first, so accept either
	// "ok" or the cancellation error — what we want to prove is that the
	// implementation does not hang.
	done := make(chan struct{})
	go func() {
		_ = c.Check(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("NATSCheck did not return on cancelled context")
	}
}
