package http

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	billingapi "github.com/sociopulse/platform/internal/billing/api"
)

// mapBillingError maps a billing sentinel to a (status, code) pair. The
// codes are dotted, low-cardinality, and follow the project's HTTP error
// policy:
//
//	ErrInvalidPeriod  → 400 billing.invalid_period
//	ErrInvalidTariff  → 400 billing.invalid_tariff
//	ErrNoTariffs      → 409 billing.no_tariffs
//	default           → 500 billing.internal
//
// The 5xx default returns a scrubbed message ("internal error") via
// renderError; the 4xx branches surface the sentinel chain text so the
// caller can introspect.
func mapBillingError(err error) (int, string) {
	switch {
	case errors.Is(err, billingapi.ErrInvalidPeriod):
		return http.StatusBadRequest, "billing.invalid_period"
	case errors.Is(err, billingapi.ErrInvalidTariff):
		return http.StatusBadRequest, "billing.invalid_tariff"
	case errors.Is(err, billingapi.ErrNoTariffs):
		return http.StatusConflict, "billing.no_tariffs"
	default:
		return http.StatusInternalServerError, "billing.internal"
	}
}

// renderError aborts the gin chain with the canonical envelope. 5xx
// scrubs the err message and logs at error level; 4xx surfaces the
// sentinel chain text.
//
// log may be nil in tests; renderError only logs when it is non-nil.
func renderError(c *gin.Context, log *zap.Logger, err error) {
	status, code := mapBillingError(err)
	env := ErrorEnvelope{Code: code}
	if status >= http.StatusInternalServerError {
		if log != nil {
			log.Error("billing/transport: internal", zap.Error(err))
		}
		env.Message = "internal error"
	} else {
		env.Message = err.Error()
	}
	c.AbortWithStatusJSON(status, env)
}
