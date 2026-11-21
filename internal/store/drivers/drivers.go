// Package drivers opens the store that storage.driver names. It sits above
// internal/store and its drivers, so internal/store stays free of them, and
// wayfindr init (internal/setup) and the daemon share one switch.
package drivers

import (
	"context"
	"errors"
	"fmt"

	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/store"
	"github.com/atomicpages/wayfindr/internal/store/postgres"
	"github.com/atomicpages/wayfindr/internal/store/sqlite"
)

// ErrUnsupported means the driver name is not one Wayfindr knows.
var ErrUnsupported = errors.New("unsupported store driver")

// Open opens the store for driver at dsn and runs pending migrations. dsn is
// a file path for sqlite and a URL for postgres. A postgres DSN may hold a
// password, so Open's own errors never include the DSN; for what the
// drivers' errors carry, see postgres.Open.
func Open(ctx context.Context, driver, dsn string) (store.Store, error) {
	switch driver {
	case config.DriverSQLite:
		return asStore(sqlite.Open(ctx, dsn))
	case config.DriverPostgres:
		return asStore(postgres.Open(ctx, dsn))
	default:
		return nil, fmt.Errorf("%w %q", ErrUnsupported, driver)
	}
}

// asStore returns a nil interface on error, never one holding a nil pointer,
// so callers can test the store against nil.
func asStore[S store.Store](s S, err error) (store.Store, error) {
	if err != nil {
		return nil, err
	}

	return s, nil
}
