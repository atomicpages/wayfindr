package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/atomicpages/wayfindr/internal/store"
)

func testEnsureUserCreatesOneUUIDv7UserAndReturnsItAfterReopen(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	db := newDatabase(t)

	first, err := db.Open(ctx)

	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	id, err := first.EnsureUser(ctx)

	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}

	again, err := first.EnsureUser(ctx)

	if err != nil {
		t.Fatalf("EnsureUser again: %v", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	parsed, err := uuid.Parse(id)

	if err != nil {
		t.Fatalf("user ID %q is not a UUID: %v", id, err)
	}

	if parsed.Version() != 7 {
		t.Errorf("user ID version = %d, want 7", parsed.Version())
	}

	if again != id {
		t.Errorf("second EnsureUser = %q, want %q", again, id)
	}

	// Reopening reruns migrations (a no-op) and keeps the same user.
	reopened := open(t, db)
	afterReopen, err := reopened.EnsureUser(ctx)

	if err != nil {
		t.Fatalf("EnsureUser after reopen: %v", err)
	}

	if afterReopen != id {
		t.Errorf("EnsureUser after reopen = %q, want %q", afterReopen, id)
	}
}

func testOpenRefusesDatabaseFromNewerBinary(t *testing.T, newDatabase NewDatabase) {
	ctx := context.Background()
	db := newDatabase(t)
	s, err := db.Open(ctx)

	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Arrange: simulate a newer binary having migrated this database.
	raw := db.SQL(t)

	if _, err := raw.ExecContext(ctx, `UPDATE schema_version SET version = 9999`); err != nil {
		t.Fatalf("bump schema_version: %v", err)
	}

	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err = db.Open(ctx)

	if !errors.Is(err, store.ErrSchemaTooNew) {
		if s != nil {
			_ = s.Close()
		}

		t.Fatalf("Open: err = %v, want ErrSchemaTooNew", err)
	}
}
