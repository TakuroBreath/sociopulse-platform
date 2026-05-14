package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	reportsapi "github.com/sociopulse/platform/internal/reports/api"
	"github.com/sociopulse/platform/internal/reports/events"
	"github.com/sociopulse/platform/pkg/outbox"
	"github.com/sociopulse/platform/pkg/postgres"
)

// fakeOutbox is a minimal recording events.OutboxWriter fake. The Tx
// parameter is the zero-value postgres.Tx — the publisher does not
// touch it, it merely forwards to Append.
type fakeOutbox struct {
	appended []outbox.Event
	nextErr  error
}

func (f *fakeOutbox) Append(_ context.Context, _ postgres.Tx, ev outbox.Event) error {
	if f.nextErr != nil {
		return f.nextErr
	}
	f.appended = append(f.appended, ev)
	return nil
}

// Compile-time assertion: the fake satisfies the OutboxWriter contract.
var _ events.OutboxWriter = (*fakeOutbox)(nil)

func TestPublishReadyTx_AppendsToTenantSubject(t *testing.T) {
	t.Parallel()
	fb := &fakeOutbox{}
	pub := events.NewReportReadyPublisher(fb)
	tenantID := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	ready := reportsapi.ReportReadyEvent{
		JobID:       "job-1",
		TenantID:    tenantID.String(),
		Kind:        "operator_efficiency",
		Format:      "xlsx",
		Filename:    "operator_efficiency_20260514.xlsx",
		BytesSize:   42_123,
		DownloadURL: "https://signed/url",
	}
	require.NoError(t, pub.PublishReadyTx(context.Background(), postgres.Tx{}, tenantID, "job-1", ready))

	require.Len(t, fb.appended, 1)
	ev := fb.appended[0]
	require.Equal(t, "tenant.11111111-1111-1111-1111-111111111111.reports.report.ready", ev.Subject)
	require.NotNil(t, ev.TenantID)
	require.Equal(t, tenantID, *ev.TenantID)

	var got reportsapi.ReportReadyEvent
	require.NoError(t, json.Unmarshal(ev.Payload, &got))
	require.Equal(t, ready, got)
}

func TestPublishReadyTx_PropagatesAppendError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("outbox-down")
	fb := &fakeOutbox{nextErr: sentinel}
	pub := events.NewReportReadyPublisher(fb)
	err := pub.PublishReadyTx(context.Background(), postgres.Tx{}, uuid.New(), "job-x", reportsapi.ReportReadyEvent{
		JobID: "job-x",
	})
	require.ErrorIs(t, err, sentinel)
}
