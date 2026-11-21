package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/bits"
)

const (
	// Difficulty is schema 1's proof-of-work: the hash must start with this
	// many zero bits. About 16.8M hashes, a second or a few on one core.
	Difficulty = 24

	// powDomain separates the hash from any other use of SHA-256.
	powDomain = "wayfindr-report-v1|"
	// nonceLen is the nonce's length: 8 bytes, as 16 lowercase hex chars.
	nonceLen = 16
	// checkEvery is how many hashes Solve tries between looks at its ctx.
	checkEvery = 1 << 16
)

// powInput is what the proof-of-work hashes, with the nonce left off.
func powInput(installID, date string) string {
	return powDomain + installID + "|" + date + "|"
}

// zeroBits counts the leading zero bits of h.
func zeroBits(h [sha256.Size]byte) int {
	n := 0

	for _, b := range h {
		if b != 0 {
			return n + bits.LeadingZeros8(b)
		}

		n += 8
	}

	return n
}

// Verify reports whether nonce solves difficulty for installID on date: the
// SHA-256 of "wayfindr-report-v1|<install_id>|<date>|<nonce>" starts with
// at least difficulty zero bits. The collector runs the same check.
func Verify(installID, date, nonce string, difficulty int) bool {
	if len(nonce) != nonceLen {
		return false
	}

	return zeroBits(sha256.Sum256([]byte(powInput(installID, date)+nonce))) >= difficulty
}

// Solve finds the first nonce, counting up from zero, that solves
// difficulty for installID on date. It gives up with ctx's error.
func Solve(ctx context.Context, installID, date string, difficulty int) (string, error) {
	prefix := powInput(installID, date)
	buf := make([]byte, len(prefix)+nonceLen)
	copy(buf, prefix)
	tail := buf[len(prefix):]

	var counter [8]byte

	for n := uint64(0); ; n++ {
		if n%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return "", fmt.Errorf("solve proof-of-work: %w", err)
			}
		}

		binary.BigEndian.PutUint64(counter[:], n)
		hex.Encode(tail, counter[:])

		if zeroBits(sha256.Sum256(buf)) >= difficulty {
			return string(tail), nil
		}
	}
}
