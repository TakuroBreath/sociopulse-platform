package httputil

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// FromHTTPHandler adapts a stdlib http.Handler into a gin.HandlerFunc
// so we can reuse stdlib middleware (otelhttp, prometheus exposition,
// pprof) inside a gin router. The adapter preserves *gin.Context so
// downstream gin handlers still see the values added by previous
// middleware.
func FromHTTPHandler(h http.Handler) gin.HandlerFunc {
	panic("not implemented: see Plan 02 Task 3")
}

// FromHTTPHandlerFunc is the http.HandlerFunc-typed sibling of
// FromHTTPHandler.
func FromHTTPHandlerFunc(h http.HandlerFunc) gin.HandlerFunc {
	panic("not implemented: see Plan 02 Task 3")
}
