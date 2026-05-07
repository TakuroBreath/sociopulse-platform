package eventbus

import "context"

// NATSPublisher is the JetStream-backed implementation of Publisher.
// Real wiring (connection, JetStream context, retry/backoff) lands in
// Plan 03 Task 7.
type NATSPublisher struct {
	// unexported nats.JetStreamContext + config — initialised in Plan 03 Task 7
}

// NewNATSPublisher constructs a Publisher backed by a NATS JetStream
// connection. urls is a comma-separated list of cluster endpoints;
// account scopes the publisher to a single NATS account.
func NewNATSPublisher(ctx context.Context, urls []string, account string) (*NATSPublisher, error) {
	panic("not implemented: see Plan 03 Task 7")
}

// Publish satisfies the Publisher interface.
func (p *NATSPublisher) Publish(ctx context.Context, subject string, payload []byte) error {
	panic("not implemented: see Plan 03 Task 7")
}

// Close drains the publisher and closes the underlying connection.
func (p *NATSPublisher) Close() error {
	panic("not implemented: see Plan 03 Task 7")
}

// NATSSubscriber is the JetStream-backed implementation of Subscriber.
type NATSSubscriber struct {
	// unexported nats.JetStreamContext + consumer config — initialised in Plan 03 Task 7
}

// NewNATSSubscriber constructs a Subscriber backed by a NATS JetStream
// connection.
func NewNATSSubscriber(ctx context.Context, urls []string, account string) (*NATSSubscriber, error) {
	panic("not implemented: see Plan 03 Task 7")
}

// Subscribe satisfies the Subscriber interface.
func (s *NATSSubscriber) Subscribe(ctx context.Context, subject string, queue string, handler func(subject string, payload []byte) error) error {
	panic("not implemented: see Plan 03 Task 7")
}

// Close stops all consumers and closes the underlying connection.
func (s *NATSSubscriber) Close() error {
	panic("not implemented: see Plan 03 Task 7")
}
