package config

import "errors"

// NATSConfig is the event-bus client config. JetStream stream names live in
// stream-specific subsections so each module can declare its own stream.
type NATSConfig struct {
	URLs       []string        `mapstructure:"urls"`
	Account    string          `mapstructure:"account"`
	Credential string          `mapstructure:"credential_file"`
	JetStream  JetStreamConfig `mapstructure:"jetstream"`
}

// JetStreamConfig declares stream identifiers per module. Each module owns its
// own stream definition; cmd/api just plumbs them through.
type JetStreamConfig struct {
	StreamTelephonyEvent string `mapstructure:"stream_telephony_event"`
	StreamAuditEvent     string `mapstructure:"stream_audit_event"`
}

func (n *NATSConfig) validate() error {
	if len(n.URLs) == 0 {
		return errors.New("at least one url required")
	}
	if n.Account == "" {
		return errors.New("account required")
	}
	return nil
}
