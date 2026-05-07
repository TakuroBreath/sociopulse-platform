package healthz

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLivenessAlways200(t *testing.T) {
	t.Parallel()
	h := NewLivenessHandler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ok\n", rec.Body.String())
	assert.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"))
}
