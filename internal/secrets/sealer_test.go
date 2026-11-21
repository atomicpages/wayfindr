package secrets_test

import (
	"bytes"
	"errors"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/secrets"
)

// testKey is a fixed 32-byte master key: 0x00, 0x01, …, 0x1f.
func testKey() []byte {
	k := make([]byte, secrets.KeySize)

	for i := range k {
		k[i] = byte(i)
	}

	return k
}

func newSealer(t *testing.T) *secrets.Sealer {
	t.Helper()
	s, err := secrets.NewSealer(testKey())

	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}

	return s
}

func TestSealerRoundTrip(t *testing.T) {
	s := newSealer(t)
	aad := secrets.AAD("secrets", "user-1", "github")
	plaintext := []byte("ghp_example-secret-value")

	sealed := s.Seal(plaintext, aad)

	if bytes.Contains(sealed, plaintext) {
		t.Fatal("sealed value contains the plaintext")
	}

	got, err := s.Open(sealed, aad)

	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if diff := cmp.Diff(plaintext, got); diff != "" {
		t.Errorf("Open() mismatch (-want +got):\n%s", diff)
	}
}

func TestSealerOpenRejects(t *testing.T) {
	s := newSealer(t)
	aad := secrets.AAD("credentials", "user-1", "linear")
	sealed := s.Seal([]byte("refresh-token"), aad)

	flip := func(i int) []byte {
		b := bytes.Clone(sealed)
		b[i] ^= 0x01

		return b
	}
	withVersion := func(v byte) []byte {
		b := bytes.Clone(sealed)
		b[0] = v

		return b
	}
	other, err := secrets.NewSealer(make([]byte, secrets.KeySize))

	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}

	tests := []struct {
		name    string
		sealer  *secrets.Sealer
		sealed  []byte
		aad     []byte
		wantErr error
	}{
		{"tampered nonce byte", s, flip(1), aad, secrets.ErrOpen},
		{"tampered ciphertext byte", s, flip(14), aad, secrets.ErrOpen},
		{"tampered tag byte", s, flip(len(sealed) - 1), aad, secrets.ErrOpen},
		{"aad of another user", s, sealed, secrets.AAD("credentials", "user-2", "linear"), secrets.ErrOpen},
		{"aad of another row", s, sealed, secrets.AAD("credentials", "user-1", "github"), secrets.ErrOpen},
		{"aad of another table", s, sealed, secrets.AAD("secrets", "user-1", "linear"), secrets.ErrOpen},
		{"version byte 0x00", s, withVersion(0x00), aad, secrets.ErrUnknownVersion},
		{"version byte 0x02", s, withVersion(0x02), aad, secrets.ErrUnknownVersion},
		{"wrong key", other, sealed, aad, secrets.ErrOpen},
		{"truncated", s, sealed[:10], aad, secrets.ErrOpen},
		{"empty", s, nil, aad, secrets.ErrOpen},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.sealer.Open(tt.sealed, tt.aad)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Open() error = %v, want %v", err, tt.wantErr)
			}

			if got != nil {
				t.Errorf("Open() returned plaintext %q on error", got)
			}
		})
	}
}

func TestSealerFormat(t *testing.T) {
	s := newSealer(t)
	plaintext := []byte("hello")
	aad := secrets.AAD("secrets", "user-1", "x")

	a := s.Seal(plaintext, aad)
	b := s.Seal(plaintext, aad)

	// 0x01 ‖ nonce(12) ‖ ciphertext(5) ‖ tag(16)
	if len(a) != 1+12+5+16 {
		t.Errorf("len(sealed) = %d, want %d", len(a), 1+12+5+16)
	}

	if a[0] != 0x01 {
		t.Errorf("version byte = 0x%02x, want 0x01", a[0])
	}

	if bytes.Equal(a[1:13], b[1:13]) {
		t.Error("two seals reused the same nonce")
	}
}

func TestAAD(t *testing.T) {
	got := string(secrets.AAD("credentials", "0192-user", "github", "token"))
	want := "wayfindr/v1|credentials|0192-user|github|token"

	if got != want {
		t.Errorf("AAD() = %q, want %q", got, want)
	}
}

func TestNewSealerRejectsWrongKeySize(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33} {
		if _, err := secrets.NewSealer(make([]byte, n)); !errors.Is(err, secrets.ErrInvalidKey) {
			t.Errorf("NewSealer(%d bytes) error = %v, want ErrInvalidKey", n, err)
		}
	}
}

func TestSealerConcurrentUse(t *testing.T) {
	s := newSealer(t)
	aad := secrets.AAD("secrets", "user-1", "x")
	var wg sync.WaitGroup

	for i := range byte(16) {
		wg.Go(func() {
			pt := []byte{i}
			got, err := s.Open(s.Seal(pt, aad), aad)

			if err != nil || !bytes.Equal(got, pt) {
				t.Errorf("goroutine %d: Open() = %v, %v", i, got, err)
			}
		})
	}

	wg.Wait()
}
