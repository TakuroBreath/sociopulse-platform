package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthzReturnsOK(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	healthzHandler(rec, req)

	if got, want := rec.Code, http.StatusOK; got != want {
		t.Fatalf("status: got %d, want %d", got, want)
	}
	if got, want := rec.Body.String(), "ok\n"; got != want {
		t.Fatalf("body: got %q, want %q", got, want)
	}
}

func TestHealthzReturnsContentTypePlain(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	healthzHandler(rec, req)

	if got, want := rec.Header().Get("Content-Type"), "text/plain; charset=utf-8"; got != want {
		t.Fatalf("Content-Type: got %q, want %q", got, want)
	}
}
