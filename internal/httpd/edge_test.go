package httpd_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/httpd"
)

// TestForeignHostLearnsNothing: on a foreign Host, unknown paths, wrong
// methods, and unclean paths all look the same as a real route: 404.
func TestForeignHostLearnsNothing(t *testing.T) {
	h := newStubbedHandler(t)
	reqs := []struct{ method, path string }{
		{"GET", "/nope"},
		{"PATCH", "/healthz"},
		{"GET", "/admin/../healthz"},
		{"GET", "/admin/status/"},
	}

	for _, r := range reqs {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), r.method, "http://evil.example:7777/", nil)
			req.URL.Path = r.path

			if code, _ := serve(h, req); code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", code)
			}
		})
	}
}

// TestRejectionLogRedacted: rejected requests are logged without header
// values or the query string (design §9).
func TestRejectionLogRedacted(t *testing.T) {
	var buf bytes.Buffer
	h, err := httpd.NewHandler(httpd.Config{
		Port:     testPort,
		Token:    testToken,
		Logger:   zerolog.New(&buf).Level(zerolog.DebugLevel),
		Handlers: stubHandlers(),
		// auth.CallbackPath, which the daemon passes.
		CallbackPath: "/oauth/callback",
	})

	if err != nil {
		t.Fatal(err)
	}

	const secret = "s3cr3t-value"
	reqs := []*http.Request{
		httptest.NewRequestWithContext(context.Background(), "GET", "http://"+secret+".example:7777/admin/status?code="+secret, nil),
		httptest.NewRequestWithContext(context.Background(), "GET", "http://127.0.0.1:7777/admin/status?code="+secret, nil),
		httptest.NewRequestWithContext(context.Background(), "GET", "http://127.0.0.1:7777/admin/status?code="+secret, nil),
	}
	reqs[1].Header.Set("Origin", "https://"+secret+".example")
	reqs[2].Header.Set("Authorization", "Bearer "+secret)

	for _, req := range reqs {
		if code, _ := serve(h, req); code < 400 {
			t.Fatalf("status = %d, want a rejection", code)
		}
	}

	if buf.Len() == 0 {
		t.Fatal("no rejection logged")
	}

	if strings.Contains(buf.String(), secret) {
		t.Errorf("log contains a header value or query:\n%s", buf.String())
	}
}

func TestNilHandlerIsNotImplementedBehindGuards(t *testing.T) {
	h, err := httpd.NewHandler(httpd.Config{Port: testPort, Token: testToken, Logger: zerolog.Nop()})

	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequestWithContext(context.Background(), "GET", "http://127.0.0.1:7777/admin/status", nil)

	if code, _ := serve(h, req); code != http.StatusUnauthorized {
		t.Errorf("without bearer: status = %d, want 401", code)
	}

	req.Header.Set("Authorization", "Bearer "+testToken)

	if code, _ := serve(h, req); code != http.StatusNotImplemented {
		t.Errorf("with bearer: status = %d, want 501", code)
	}
}

func TestNewHandlerRejectsEmptyToken(t *testing.T) {
	if _, err := httpd.NewHandler(httpd.Config{Port: testPort, Logger: zerolog.Nop()}); err == nil {
		t.Fatal("NewHandler with an empty token: want error")
	}
}
