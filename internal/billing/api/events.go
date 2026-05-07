package api

// Package api — billing module events.
//
// billing does not publish on its own subjects. Tariff updates are mirrored
// to the audit module via the canonical tenant.<t>.audit.event subject;
// the constant below is the audit Action label.
//
// billing consumes (durable, explicit ack):
//
//	tenant.<t>.dialer.call.finalized
//
// The subject placeholder is owned by the dialer module — see
// internal/dialer/api/events.go for the canonical declaration. Helpers
// here would be redundant.
const (
	// AuditActionTariffUpdated is the audit Action set when TariffStore.Update succeeds.
	AuditActionTariffUpdated = "billing.tariff_updated"
)
