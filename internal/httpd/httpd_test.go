package httpd_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/httpd"
)

const (
	testPort    = 7777
	testToken   = "wf_test-token"
	testVersion = "v1.2.3"
)

func newHandler(t *testing.T) http.Handler {
	t.Helper()
	h, err := httpd.NewHandler(httpd.Config{
		Port:    testPort,
		Token:   testToken,
		Version: testVersion,
		Logger:  zerolog.Nop(),
	})

	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	return h
}

func TestHealthz(t *testing.T) {
	h := newHandler(t)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:7777/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	body, _ := io.ReadAll(rec.Body)

	if diff := cmp.Diff(`{"version":"v1.2.3"}`+"\n", string(body)); diff != "" {
		t.Errorf("body mismatch (-want +got):\n%s", diff)
	}
}
