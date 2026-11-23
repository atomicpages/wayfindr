// Package selfupdate finds, downloads, verifies, and installs Wayfindr
// release binaries, the job `wayfindr update` does from the binary itself.
//
// It mirrors the installers (docs/public/install.sh and install.ps1): the
// latest release comes from the releases/latest redirect rather than the
// GitHub API, the archive must match its line in the release's checksums.txt
// before anything is written, and the new binary is swapped in beside the old
// one (an atomic rename on Unix, a rename-aside on Windows). Release assets are
// named as .goreleaser.yaml builds them: wayfindr-<os>-<arch>.tar.gz, or .zip
// on Windows, holding the wayfindr (wayfindr.exe) binary.
//
// The package is a leaf: it imports only the standard library, redact,
// golang.org/x/mod/semver, and golang.org/x/sys.
package selfupdate

import (
	"fmt"
	"strings"

	"golang.org/x/mod/semver"
)

// DefaultBaseURL is the GitHub releases page that a zero Source reads from.
const DefaultBaseURL = "https://github.com/atomicpages/wayfindr/releases"

// NormalizeTag turns a version as a person types it ("0.2.0" or "v0.2.0")
// into a release tag ("v0.2.0"), as the installers do. It rejects anything
// that is not a full semantic version, so "v1.2" or "latest" is an error.
func NormalizeTag(v string) (string, error) {
	tag := strings.TrimSpace(v)
	if tag != "" && tag[0] >= '0' && tag[0] <= '9' {
		tag = "v" + tag
	}

	if !isFullSemver(tag) {
		return "", fmt.Errorf("invalid version %q: want a semantic version such as v0.2.0", v)
	}

	return tag, nil
}

// Compare orders the running version against a release tag: negative when
// current is older than latest, zero when they are the same, positive when
// current is newer.
//
// A current version whose base is not a full vMAJOR.MINOR.PATCH is older
// than everything, so it always updates: "dev", or a bare commit from git
// describe --always, even an all-digit one ("1234567", which semver alone
// would read as the shorthand v1234567). A git
// describe build ("v0.1.0-3-gabc1234", optionally with "-dirty") is its base
// tag plus local commits: newer than v0.1.0, older than v0.1.1. A version
// with only "-dirty" on a tag counts the same way. Real pre-releases
// ("v0.2.0-rc.1") follow normal semver ordering.
func Compare(current, latest string) int {
	base, ahead := splitDescribe(withV(strings.TrimSpace(current)))
	if !isFullSemver(base) {
		return -1
	}

	if c := semver.Compare(base, withV(strings.TrimSpace(latest))); c != 0 {
		return c
	}

	if ahead {
		return 1
	}

	return 0
}

// splitDescribe strips the suffix git describe adds past a tag
// ("-<n>-g<sha>", then "-dirty") and reports whether there was one.
func splitDescribe(v string) (base string, ahead bool) {
	if s, ok := strings.CutSuffix(v, "-dirty"); ok {
		v, ahead = s, true
	}

	// "-<n>-g<sha>": the last two hyphen-separated parts.
	i := strings.LastIndexByte(v, '-')
	if i < 0 || !isAbbrevSHA(v[i+1:]) {
		return v, ahead
	}

	j := strings.LastIndexByte(v[:i], '-')
	if j < 0 || !isDigits(v[j+1:i]) {
		return v, ahead
	}

	return v[:j], true
}

// isAbbrevSHA reports whether s is "g" followed by an abbreviated commit, as
// git describe prints it (at least four hex digits).
func isAbbrevSHA(s string) bool {
	hex, ok := strings.CutPrefix(s, "g")
	if !ok || len(hex) < 4 {
		return false
	}

	for _, r := range hex {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}

	return true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}

	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}

// withV adds the "v" semver needs to a version that starts with a digit.
func withV(v string) string {
	if v != "" && v[0] >= '0' && v[0] <= '9' {
		return "v" + v
	}

	return v
}

// isFullSemver reports whether v is a complete vMAJOR.MINOR.PATCH version
// (with an optional pre-release and build), not semver's "v1.2" shorthand,
// which would never name a release tag.
func isFullSemver(v string) bool {
	if !semver.IsValid(v) {
		return false
	}

	noBuild, _, _ := strings.Cut(v, "+")

	return semver.Canonical(v) == noBuild
}

// AssetName is the release archive for an OS and CPU, as .goreleaser.yaml
// names it: wayfindr-<goos>-<goarch>.tar.gz, or .zip on Windows.
func AssetName(goos, goarch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}

	return "wayfindr-" + goos + "-" + goarch + ext
}

// HostArch is the architecture to download for: goarch, except that an amd64
// build running under Rosetta 2 on Apple silicon gets arm64, the native
// build, as install.sh does.
func HostArch(goos, goarch string) string {
	if goos == "darwin" && goarch == "amd64" && translated() {
		return "arm64"
	}

	return goarch
}
