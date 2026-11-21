package storetest

import (
	"context"
	"testing"

	"github.com/atomicpages/wayfindr/internal/store"
)

func testHasSealedDataReportsCredentialAndSecretRows(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	tests := []struct {
		name  string
		write func(s store.Store, user string) error
		want  bool
	}{
		{name: "empty store", write: func(store.Store, string) error { return nil }, want: false},
		{name: "a credential", want: true, write: func(s store.Store, user string) error {
			return s.PutCredential(ctx, store.Credential{
				UserID: user, Connector: "linear", Resource: "https://mcp.linear.app/mcp", Token: []byte("sealed"),
			})
		}},
		{name: "a secret", want: true, write: func(s store.Store, user string) error {
			return s.PutSecret(ctx, user, "linear_key", []byte("sealed"))
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, user := newStore(t, newDatabase)

			if err := tt.write(s, user); err != nil {
				t.Fatalf("arrange: %v", err)
			}

			got, err := s.HasSealedData(ctx)

			if err != nil {
				t.Fatalf("HasSealedData: %v", err)
			}

			if got != tt.want {
				t.Errorf("HasSealedData = %v, want %v", got, tt.want)
			}
		})
	}
}
