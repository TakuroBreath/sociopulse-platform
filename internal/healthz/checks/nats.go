package checks

import (
	"context"
	"fmt"
	"time"
)

// NATSConn is the minimal NATS surface readiness needs. The real *nats.Conn
// satisfies this trivially: IsConnected() bool and Status() returns its
// internal Status enum (which is an int under the hood).
type NATSConn interface {
	IsConnected() bool
	Status() int
}

// NATSCheck reports OK only when the underlying client is in CONNECTED state.
type NATSCheck struct {
	Conn NATSConn
}

// Name reports the dependency identifier surfaced in /readyz output.
func (NATSCheck) Name() string { return "nats" }

// Check returns nil iff IsConnected() is true. The numeric Status() is
// embedded in the error for diagnosability (matches the *nats.Conn enum:
// 0=DISCONNECTED, 1=CONNECTED, 2=CLOSED, 3=RECONNECTING, 4=CONNECTING,
// 5=DRAINING_SUBS, 6=DRAINING_PUBS). The state is queried in a goroutine so
// context cancellation can preempt a stuck implementation.
func (n NATSCheck) Check(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	type natsState struct {
		ok     bool
		status int
	}
	ch := make(chan natsState, 1)
	go func() {
		ch <- natsState{ok: n.Conn.IsConnected(), status: n.Conn.Status()}
	}()
	select {
	case s := <-ch:
		if s.ok {
			return nil
		}
		return fmt.Errorf("nats not connected (status=%d)", s.status)
	case <-cctx.Done():
		return cctx.Err()
	}
}
