package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// telephonyBridgeOfflineCode is the canonical error code returned by
// every listen-in endpoint while Plan 08 (FreeSWITCH cluster) has not
// landed. The same code is used by the dialer's nats router stub and
// the telephony module's stub CommandPublisher.
const telephonyBridgeOfflineCode = "telephony.bridge.offline"

// listenHandler exposes the listen-in HTTP endpoints. Per Plan 11
// Decision 5 the listen-in subsystem is deferred until Plan 08
// (FreeSWITCH cluster) provides a real telephony bridge; until then
// every endpoint returns 503 Service Unavailable + the canonical
// telephony.bridge.offline error envelope.
//
// TODO(plan-08): replace these stubs with the real ListenInService
// wiring once the FreeSWITCH cluster + listen-in service land. The
// public route shape stays the same so the operator UI does not need
// to learn a new contract.
type listenHandler struct {
	logger *zap.Logger
}

// newListenHandler constructs a listen-in handler stub. A nil logger
// is tolerated (falls back to nop).
func newListenHandler(logger *zap.Logger) *listenHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &listenHandler{logger: logger}
}

// mount registers the listen-in stub endpoints on the supplied gin
// RouterGroup. The caller is expected to have already attached
// JWTMiddleware so an unauthenticated request gets a clean 401 before
// reaching the stub.
//
// Routes:
//
//	POST   /calls/:id/listen
//	DELETE /listen-sessions/:id
func (h *listenHandler) mount(group *gin.RouterGroup) {
	group.POST("/calls/:id/listen", h.handleStart)
	group.DELETE("/listen-sessions/:id", h.handleStop)
}

// handleStart is the Plan-08-deferred stub for POST /calls/:id/listen.
// It returns 503 Service Unavailable with a telephony.bridge.offline
// envelope.
func (h *listenHandler) handleStart(c *gin.Context) {
	h.respondOffline(c, "start")
}

// handleStop is the Plan-08-deferred stub for DELETE
// /listen-sessions/:id. Symmetric with handleStart.
func (h *listenHandler) handleStop(c *gin.Context) {
	h.respondOffline(c, "stop")
}

// respondOffline writes the canonical 503 envelope. The op label
// distinguishes start vs stop in observability.
func (h *listenHandler) respondOffline(c *gin.Context, op string) {
	h.logger.Debug("realtime/listen: returning telephony bridge offline (Plan 08 deferred)",
		zap.String("op", op),
		zap.String("path", c.FullPath()),
	)
	c.JSON(http.StatusServiceUnavailable, errorEnvelope{
		Code:    telephonyBridgeOfflineCode,
		Message: "Listen-in is deferred until Plan 08 (FreeSWITCH cluster)",
	})
}
