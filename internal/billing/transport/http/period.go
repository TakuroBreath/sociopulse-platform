package http

import (
	"time"

	"github.com/gin-gonic/gin"

	billingapi "github.com/sociopulse/platform/internal/billing/api"
)

// parsePeriod reads ?period=week|month|quarter|year from the gin context.
// Default (missing query parameter) is "month". Unknown values return
// ErrInvalidPeriod (the handler renders 400 billing.invalid_period).
//
// All periods are anchored to UTC and half-open ([From, To)). Week starts
// on Monday (ISO 8601); quarter is the calendar quarter containing
// `now.Month()`; year is the calendar year.
//
// docs/references/plan-14-billing.md §4.6 — explicit ?from=&to= is
// deferred to v2 to keep the dashboard surface tight.
func parsePeriod(c *gin.Context, now time.Time) (billingapi.Period, error) {
	nowUTC := now.UTC()
	switch c.Query("period") {
	case "", "month":
		return billingapi.Month(nowUTC.Year(), nowUTC.Month()), nil
	case "week":
		// ISO 8601 week starts Monday. time.Weekday() is 0=Sunday..6=Saturday.
		// Convert to 1..7 Monday-anchored so the subtraction below yields
		// the Monday of the current week.
		wd := int(nowUTC.Weekday())
		if wd == 0 {
			wd = 7
		}
		from := time.Date(nowUTC.Year(), nowUTC.Month(), nowUTC.Day()-(wd-1), 0, 0, 0, 0, time.UTC)
		return billingapi.Period{From: from, To: from.AddDate(0, 0, 7)}, nil
	case "quarter":
		// Calendar quarters start at month index 1, 4, 7, 10.
		q := ((int(nowUTC.Month()) - 1) / 3) * 3
		from := time.Date(nowUTC.Year(), time.Month(q+1), 1, 0, 0, 0, 0, time.UTC)
		return billingapi.Period{From: from, To: from.AddDate(0, 3, 0)}, nil
	case "year":
		from := time.Date(nowUTC.Year(), time.January, 1, 0, 0, 0, 0, time.UTC)
		return billingapi.Period{From: from, To: from.AddDate(1, 0, 0)}, nil
	default:
		return billingapi.Period{}, billingapi.ErrInvalidPeriod
	}
}
