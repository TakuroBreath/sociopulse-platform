package api

import "errors"

// Sentinel errors returned by billing interfaces.
// Other modules use errors.Is to discriminate.
var (
	// ErrNoTariffs is returned when a tenant has no tariffs row yet.
	// The boundary maps it to HTTP 409 / gRPC FailedPrecondition.
	ErrNoTariffs = errors.New("billing: no tariffs configured for tenant")
	// ErrInvalidTariff is returned when TariffsPatchRequest carries an unparseable value
	// (e.g. negative trunk cost, zero wage per survey).
	ErrInvalidTariff = errors.New("billing: invalid tariff")
	// ErrInvalidPeriod is returned when Period.From≥Period.To, or when a query
	// targets a future or unreasonably old period.
	ErrInvalidPeriod = errors.New("billing: invalid period")
)
