package checks

import (
	"context"
	"fmt"
)

// NATSConn is the minimal NATS surface readiness needs. The real *nats.Conn
// satisfies this trivially: IsConnected() bool and Status() returns its
// internal Status enum (which is an int under the hood).
//
// IsConnected and Status are atomic-load accessors on the nats.go side,
// so they cannot block — there is no need for a goroutine + channel
// preemption gate around them. The Postgres / Redis checks call their
// underlying client directly for the same reason.
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
// 5=DRAINING_SUBS, 6=DRAINING_PUBS).
//
// ctx is consulted as a defence-in-depth precaution but in practice the
// underlying accessors return synchronously without I/O.
func (n NATSCheck) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if n.Conn.IsConnected() {
		return nil
	}
	return fmt.Errorf("nats not connected (status=%d)", n.Conn.Status())
}
