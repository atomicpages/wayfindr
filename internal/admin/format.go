package admin

import (
	"fmt"
	"math"
)

// FormatBytes words a size in bytes in binary units for people: whole bytes
// below 1 KiB, then KiB, MiB, and GiB, with one decimal below 10 and none
// from there, such as 312 B, 1.2 KiB, 48 KiB, and 1.2 MiB. Both the CLI's
// and the console's call lists show a result's size this way; each words
// an unknown size itself.
func FormatBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}

	v := float64(n)
	unit := ""

	for _, next := range []string{"KiB", "MiB", "GiB"} {
		v /= 1024
		unit = next

		// 1023.6 KiB is shown as 1.0 MiB, not as 1024 KiB.
		if next == "GiB" || math.Round(v) < 1024 {
			break
		}
	}

	if v < 9.95 {
		return fmt.Sprintf("%.1f %s", v, unit)
	}

	return fmt.Sprintf("%.0f %s", v, unit)
}
