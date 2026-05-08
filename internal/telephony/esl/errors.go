// Package esl is the hand-rolled FreeSWITCH Event Socket Library client used
// by the telephony bridge to talk to a single FreeSWITCH node.
//
// The package owns the wire-level details: the line-based protocol parser
// (Frame + Event), the auth-handshake-and-readLoop client, and the
// jittered-exponential reconnect Backoff. It does NOT own pool management,
// retry policy across nodes, or NATS fan-out — those live in
// internal/telephony/pool, internal/telephony/router and
// internal/telephony/nats_bridge respectively.
//
// The package exposes no global state. Callers that want metrics inject a
// *prometheus.Registerer (see RegisterMetrics) so test imports never collide.
package esl

import "errors"

// Sentinel errors returned by the ESL client. Higher layers discriminate via
// errors.Is.
//
// These intentionally mirror — but DO NOT alias — the package-level sentinels
// in internal/telephony/api/errors.go. The api package is the public-facing
// contract; this package is internal plumbing. Task 3-4's adapter translates
// between the two when surfacing failures over NATS.
var (
	// ErrAuthFailed is returned when the FreeSWITCH server replies with
	// `-ERR …` to our `auth <password>` command. Triggers wherever the
	// reply-text first byte is not `+`.
	ErrAuthFailed = errors.New("esl: auth failed")

	// ErrNotConnected is returned by sendCommand when the client has been
	// closed (or readLoop has detected an EOF / disconnect-notice and is
	// tearing down) before the command was issued.
	ErrNotConnected = errors.New("esl: not connected")

	// ErrCommandFailed is returned when FS replies `-ERR <reason>` to a
	// command. Wrap with %w to attach the FS-supplied reason:
	//
	//   return fmt.Errorf("%w: %s", ErrCommandFailed, replyText)
	//
	// Higher-layer callers compare via errors.Is(err, ErrCommandFailed).
	ErrCommandFailed = errors.New("esl: command failed")

	// ErrTimeout is returned by sendCommand when the supplied context
	// deadline expires before the server replies. It deliberately does NOT
	// wrap context.DeadlineExceeded — callers that want to distinguish the
	// two should check the context directly.
	ErrTimeout = errors.New("esl: timeout")

	// ErrInvalidFrame is returned by the parser when a frame violates the
	// ESL wire format (malformed header line, missing colon, oversized
	// frame, etc.). Reserved for cases the protocol guarantees will not
	// occur on a healthy server.
	ErrInvalidFrame = errors.New("esl: invalid frame")

	// ErrInvalidArgument is returned by high-level command methods (e.g.
	// Originate, Hangup, MixMonitorStart, MixMonitorStop, Play) when the
	// caller supplies an empty or otherwise invalid argument that fails
	// pre-flight validation BEFORE any wire I/O. Wrap with %w to attach
	// context:
	//
	//   return fmt.Errorf("%w: call_url required", ErrInvalidArgument)
	//
	// Task 4's NATS adapter compares via errors.Is(err, ErrInvalidArgument)
	// to map these to a 4xx-equivalent NATS reply (operator misuse) rather
	// than the 5xx bucket reserved for FS-side or transport failures.
	ErrInvalidArgument = errors.New("esl: invalid argument")
)
