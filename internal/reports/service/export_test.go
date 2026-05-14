package service

import (
	"context"

	analyticsapi "github.com/sociopulse/platform/internal/analytics/api"
	reportsapi "github.com/sociopulse/platform/internal/reports/api"
)

// RenderForTest exposes the unexported renderByKind dispatcher to the
// external service_test package so tests can exercise the
// KindCustom → project_summary mapping (which Run() never reaches
// because Custom always trips ErrAsyncRequired at the threshold gate).
//
// This is the standard Go pattern for test-only exports: only the
// service_test package (file suffix _test.go) sees this symbol; it does
// not pollute the public API.
func RenderForTest(ctx context.Context, ana analyticsapi.ServiceRO, in reportsapi.RenderInput) (reportsapi.RenderResult, error) {
	return renderByKind(ctx, ana, in)
}
