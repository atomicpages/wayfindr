package admin_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

func TestAddConnector(t *testing.T) {
	st, user := newStore(t)
	spec := admin.ConnectorSpec{Name: "linear", URL: "https://mcp.linear.app/mcp", SecretHeaders: map[string]string{"Authorization": "Bearer k"}}

	t.Run("reports the new connector's row", func(t *testing.T) {
		opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
		var got admin.ConnectorSpec
		opts.AddConnector = func(_ context.Context, s admin.ConnectorSpec) (admin.ConnectorReport, error) {
			got = s

			return admin.ConnectorReport{
				Status:  upstream.ConnectorStatus{Connector: "linear", State: upstream.StateReady},
				OAuth:   admin.OAuthNotChecked,
				Secrets: []string{"linear_authorization"},
			}, nil
		}
		c, _ := serve(t, opts)
		added, err := c.AddConnector(context.Background(), spec)

		if err != nil {
			t.Fatalf("AddConnector: %v", err)
		}

		if diff := cmp.Diff(spec, got); diff != "" {
			t.Errorf("the spec the daemon got (-want +got):\n%s", diff)
		}

		if added.Result.Name != "linear" || added.Result.State != upstream.StateReady {
			t.Errorf("result = %+v, want linear ready", added.Result)
		}

		if diff := cmp.Diff([]string{"linear_authorization"}, added.Secrets); diff != "" || added.OAuth != admin.OAuthNotChecked {
			t.Errorf("secrets %v, oauth %q; want [linear_authorization], not_checked", added.Secrets, added.OAuth)
		}
	})

	t.Run("a refusal answers its status and advice", func(t *testing.T) {
		opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
		opts.AddConnector = func(context.Context, admin.ConnectorSpec) (admin.ConnectorReport, error) {
			return admin.ConnectorReport{}, &admin.AddError{Status: http.StatusConflict, Kind: "conflict", Case: "connector_exists",
				Advice: admin.Advice{Summary: `A connector named "linear" is already configured.`}}
		}
		c, _ := serve(t, opts)
		_, err := c.AddConnector(context.Background(), spec)

		if got := statusOf(err); got != http.StatusConflict || !strings.Contains(err.Error(), "already configured") {
			t.Errorf("err = %v (status %d), want 409 with the advice", err, got)
		}
	})

	t.Run("an unknown field is 400", func(t *testing.T) {
		opts := options(t, st, user, &fakeUpstreams{}, admin.ConfigState{})
		opts.AddConnector = func(context.Context, admin.ConnectorSpec) (admin.ConnectorReport, error) {
			t.Error("AddConnector called for a malformed body")

			return admin.ConnectorReport{}, nil
		}
		_, srv := serve(t, opts)
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/admin/connectors", strings.NewReader(`{"name":"x","shell":"rm -rf /"}`))
		res, err := srv.Client().Do(req)

		if err != nil {
			t.Fatal(err)
		}

		_ = res.Body.Close()

		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", res.StatusCode)
		}
	})

	t.Run("no adder is 501", func(t *testing.T) {
		c, _ := serve(t, options(t, st, user, &fakeUpstreams{}, admin.ConfigState{}))
		_, err := c.AddConnector(context.Background(), spec)

		if got := statusOf(err); got != http.StatusNotImplemented {
			t.Errorf("status = %d (%v), want 501", got, err)
		}
	})
}
