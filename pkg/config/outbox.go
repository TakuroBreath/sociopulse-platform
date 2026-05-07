package config

import (
	"errors"
	"fmt"
	"time"
)

// OutboxConfig governs the transactional-outbox relay defined in pkg/outbox.
//
// One Relay runs per cmd/api replica; the relay drains the event_outbox table
// to NATS using `FOR UPDATE SKIP LOCKED`, so it is safe to run on every
// replica without leader election. See spec §16 and Plan 03 Task 6 for the
// concrete relay implementation.
type OutboxConfig struct {
	// BatchSize bounds the number of rows drained per pass. Larger batches
	// amortise transaction overhead at the cost of higher tail latency for
	// the slowest event in the batch.
	BatchSize int `mapstructure:"batch_size"`
	// Tick is the poll interval when the outbox is empty. Under load the
	// relay drains continuously; Tick only governs the idle case.
	Tick time.Duration `mapstructure:"tick"`
	// MaxRetry caps the number of publish attempts before a row is parked
	// in the dead-letter state. Operators inspect the last_error column to
	// diagnose stuck rows.
	MaxRetry int `mapstructure:"max_retry"`
}

func (o *OutboxConfig) validate() error {
	if o.BatchSize <= 0 {
		return errors.New("batch_size must be > 0")
	}
	if o.BatchSize > 10_000 {
		return fmt.Errorf("batch_size > 10000 risks long transactions; got %d", o.BatchSize)
	}
	if o.Tick <= 0 {
		return errors.New("tick must be > 0")
	}
	if o.Tick > time.Minute {
		return fmt.Errorf("tick > 1m delays delivery unacceptably; got %s", o.Tick)
	}
	if o.MaxRetry <= 0 {
		return errors.New("max_retry must be > 0")
	}
	return nil
}
