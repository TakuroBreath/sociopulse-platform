package grpcserver_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain wraps every test in goleak — Plan 12.1 carry-forward
// rule. The two ignore filters cover gRPC's per-stream goroutines
// that linger briefly after GracefulStop returns: the bufconn
// listener tears them down asynchronously and goleak fires before
// the runtime collects the parked stream handlers. Both filters are
// scoped to top-functions in google.golang.org/grpc so we don't mask
// genuine leaks in our code.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("google.golang.org/grpc.(*Server).handleStream"),
		goleak.IgnoreTopFunction("google.golang.org/grpc/internal/transport.(*serverHandlerTransport).runStream"),
	)
}
