package selfupdate_test

import (
	"runtime"
	"testing"

	"github.com/atomicpages/wayfindr/internal/selfupdate"
)

func TestCompare(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		current string
		latest  string
		want    int
	}{
		{name: "equal", current: "v0.2.0", latest: "v0.2.0", want: 0},
		{name: "older patch", current: "v0.2.0", latest: "v0.2.1", want: -1},
		{name: "older minor numerically", current: "v0.9.0", latest: "v0.10.0", want: -1},
		{name: "newer", current: "v0.3.0", latest: "v0.2.9", want: 1},
		{name: "no v prefix", current: "0.2.0", latest: "v0.2.0", want: 0},
		{name: "dev is older", current: "dev", latest: "v0.0.1", want: -1},
		{name: "empty is older", current: "", latest: "v0.0.1", want: -1},
		{name: "bare commit is older", current: "abc1234", latest: "v0.0.1", want: -1},
		// git describe --always prints a bare hash when no tag is reachable;
		// an all-digit one is semver shorthand (v1234567) unless the base
		// must be a full vMAJOR.MINOR.PATCH.
		{name: "all-digit bare commit is older", current: "1234567", latest: "v0.2.0", want: -1},
		{name: "all-digit dirty bare commit is older", current: "1234567-dirty", latest: "v0.2.0", want: -1},
		{name: "major-only shorthand is older", current: "v1", latest: "v0.2.0", want: -1},
		{name: "major.minor shorthand is older", current: "v1.2", latest: "v0.2.0", want: -1},
		{name: "git describe is newer than its base", current: "v0.1.0-3-gabc1234", latest: "v0.1.0", want: 1},
		{name: "git describe is older than the next patch", current: "v0.1.0-3-gabc1234", latest: "v0.1.1", want: -1},
		{name: "git describe dirty is newer than its base", current: "v0.1.0-3-gabc1234-dirty", latest: "v0.1.0", want: 1},
		{name: "git describe dirty is older than the next patch", current: "v0.1.0-3-gabc1234-dirty", latest: "v0.1.1", want: -1},
		{name: "dirty tag is newer than the tag", current: "v0.1.0-dirty", latest: "v0.1.0", want: 1},
		{name: "git describe on a pre-release", current: "v0.2.0-rc.1-2-g0123abcd", latest: "v0.2.0", want: -1},
		{name: "pre-release is older than its release", current: "v0.2.0-rc.1", latest: "v0.2.0", want: -1},
		{name: "pre-release is newer than the previous release", current: "v0.2.0-rc.1", latest: "v0.1.9", want: 1},
		{name: "pre-releases order by identifier", current: "v0.2.0-rc.2", latest: "v0.2.0-rc.10", want: -1},
		{name: "build metadata is ignored", current: "v0.2.0+linux", latest: "v0.2.0", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := sign(selfupdate.Compare(tt.current, tt.latest)); got != tt.want {
				t.Errorf("Compare(%q, %q) = %d, want %d", tt.current, tt.latest, got, tt.want)
			}
		})
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

func TestNormalizeTag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "0.2.0", want: "v0.2.0"},
		{in: "v0.2.0", want: "v0.2.0"},
		{in: " v1.10.3 ", want: "v1.10.3"},
		{in: "0.2.0-rc.1", want: "v0.2.0-rc.1"},
		{in: "v1.2", wantErr: true},
		{in: "1", wantErr: true},
		{in: "latest", wantErr: true},
		{in: "dev", wantErr: true},
		{in: "", wantErr: true},
		{in: "v0.2.0/../../x", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()

			got, err := selfupdate.NormalizeTag(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeTag(%q) = %q, want an error", tt.in, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("NormalizeTag(%q) error = %v", tt.in, err)
			}

			if got != tt.want {
				t.Errorf("NormalizeTag(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestAssetName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		goos, goarch string
		want         string
	}{
		{goos: "linux", goarch: "amd64", want: "wayfindr-linux-amd64.tar.gz"},
		{goos: "linux", goarch: "arm64", want: "wayfindr-linux-arm64.tar.gz"},
		{goos: "darwin", goarch: "arm64", want: "wayfindr-darwin-arm64.tar.gz"},
		{goos: "windows", goarch: "amd64", want: "wayfindr-windows-amd64.zip"},
		{goos: "windows", goarch: "arm64", want: "wayfindr-windows-arm64.zip"},
	}

	for _, tt := range tests {
		if got := selfupdate.AssetName(tt.goos, tt.goarch); got != tt.want {
			t.Errorf("AssetName(%q, %q) = %q, want %q", tt.goos, tt.goarch, got, tt.want)
		}
	}
}

func TestHostArch(t *testing.T) {
	t.Parallel()

	// Rosetta only ever changes darwin/amd64, so every other pair passes
	// through wherever the test runs.
	for _, p := range [][2]string{{"linux", "amd64"}, {"linux", "arm64"}, {"windows", "amd64"}, {"darwin", "arm64"}} {
		if got := selfupdate.HostArch(p[0], p[1]); got != p[1] {
			t.Errorf("HostArch(%q, %q) = %q, want %q", p[0], p[1], got, p[1])
		}
	}

	// darwin/amd64 is arm64 only under Rosetta, which a native build never is.
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		if got := selfupdate.HostArch("darwin", "amd64"); got != "amd64" {
			t.Errorf("HostArch(darwin, amd64) in a native arm64 process = %q, want amd64", got)
		}
	}
}
