package drivers_test

import (
	"errors"
	"testing"

	"github.com/atomicpages/wayfindr/internal/store/drivers"
)

func TestOpenRejectsAnUnknownDriverWithoutEchoingTheDSN(t *testing.T) {
	const dsn = "mysql://root:hunter2@db.example/wayfindr" //nolint:gosec // a fake password, asserted absent from the error

	s, err := drivers.Open(t.Context(), "mysql", dsn)

	if !errors.Is(err, drivers.ErrUnsupported) {
		t.Fatalf("Open: err = %v, want ErrUnsupported", err)
	}

	if s != nil {
		t.Error("Open returned a store along with the error")
	}

	if got := err.Error(); got != `unsupported store driver "mysql"` {
		t.Errorf("error = %q, want it to name the driver and not the DSN", got)
	}
}
