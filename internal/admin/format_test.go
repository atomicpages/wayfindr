package admin_test

import (
	"testing"

	"github.com/atomicpages/wayfindr/internal/admin"
)

// FormatBytes is a decision table over the unit and the decimal, tested
// directly (CODING_STANDARDS.md, Tests); the CLI's and the console's call
// lists cover it through their pages.
func TestFormatBytes(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{312, "312 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{10_189, "10 KiB"},
		{48 * 1024, "48 KiB"},
		{1023*1024 + 100, "1023 KiB"},
		{1024*1024 - 1, "1.0 MiB"},
		{1_258_291, "1.2 MiB"},
		{25 << 20, "25 MiB"},
		{3 << 30, "3.0 GiB"},
		{5000 << 30, "5000 GiB"},
	}

	for _, tt := range tests {
		if got := admin.FormatBytes(tt.n); got != tt.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}
