#!/bin/sh
# Installs the wayfindr binary from GitHub Releases on macOS and Linux.
#
#   curl -fsSL https://atomicpages.github.io/wayfindr/install.sh | sh
#   curl -fsSL https://atomicpages.github.io/wayfindr/install.sh | sh -s -- v0.2.0
#
# Environment:
#   WAYFINDR_VERSION       release tag to install (default: latest); an argument wins
#   WAYFINDR_INSTALL_DIR   directory for the binary (default: $HOME/.local/bin)
#   WAYFINDR_DOWNLOAD_URL  base URL holding the archives and checksums.txt, in
#                          place of the GitHub release (a mirror, or file:// in CI)
#
# The archive's SHA-256 must match checksums.txt or nothing is installed.
# Everything runs from main at the end, so a download cut short runs nothing.
# Windows: irm https://atomicpages.github.io/wayfindr/install.ps1 | iex
set -eu

repo=atomicpages/wayfindr

say() { printf 'wayfindr: %s\n' "$*"; }
die() {
	printf 'wayfindr: error: %s\n' "$*" >&2
	exit 1
}

detect_os() {
	case $(uname -s) in
	Darwin) echo darwin ;;
	Linux) echo linux ;;
	MINGW* | MSYS* | CYGWIN*) die "on Windows, run this in PowerShell instead: irm https://atomicpages.github.io/wayfindr/install.ps1 | iex" ;;
	*) die "no release for this OS: $(uname -s)" ;;
	esac
}

detect_arch() {
	case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) die "no release for this architecture: $(uname -m)" ;;
	esac
	# An x86_64 shell under Rosetta 2 on Apple silicon: install the native build.
	if [ "$1" = darwin ] && [ "$arch" = amd64 ] &&
		[ "$(sysctl -n sysctl.proc_translated 2>/dev/null || true)" = 1 ]; then
		arch=arm64
	fi
	echo "$arch"
}

fetch() {
	curl --fail --silent --show-error --location \
		--proto '=https,file' --proto-redir '=https' \
		--output "$2" "$1" || die "download failed: $1"
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d ' ' -f 1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d ' ' -f 1
	else
		die "need sha256sum or shasum to verify the download"
	fi
}

main() {
	command -v curl >/dev/null 2>&1 || die "curl is required"
	command -v tar >/dev/null 2>&1 || die "tar is required"

	version=${1:-${WAYFINDR_VERSION:-latest}}
	install_dir=${WAYFINDR_INSTALL_DIR:-$HOME/.local/bin}
	os=$(detect_os)
	arch=$(detect_arch "$os")
	asset=wayfindr-$os-$arch.tar.gz

	if [ -n "${WAYFINDR_DOWNLOAD_URL:-}" ]; then
		base=${WAYFINDR_DOWNLOAD_URL%/}
	elif [ "$version" = latest ]; then
		base=https://github.com/$repo/releases/latest/download
	else
		case $version in v*) ;; *) version=v$version ;; esac
		base=https://github.com/$repo/releases/download/$version
	fi

	tmp=$(mktemp -d 2>/dev/null || mktemp -d -t wayfindr)
	staged=
	trap 'rm -rf "$tmp"; [ -z "$staged" ] || rm -f "$staged"' EXIT
	trap 'exit 130' INT TERM

	say "downloading $asset from $base"
	fetch "$base/$asset" "$tmp/$asset"
	fetch "$base/checksums.txt" "$tmp/checksums.txt"

	want=$(awk -v f="$asset" '$2 == f { print $1 }' "$tmp/checksums.txt")
	[ -n "$want" ] || die "$asset is not listed in checksums.txt"
	got=$(sha256 "$tmp/$asset")
	[ "$got" = "$want" ] || die "checksum mismatch for $asset: expected $want, got $got"

	tar -xzf "$tmp/$asset" -C "$tmp" wayfindr
	mkdir -p "$install_dir"
	# Stage next to the target, then rename over it. The swap is atomic, and a
	# running daemon keeps the old binary until it restarts.
	staged=$install_dir/.wayfindr.$$
	cp "$tmp/wayfindr" "$staged"
	chmod 0755 "$staged"
	mv -f "$staged" "$install_dir/wayfindr"
	staged=

	say "installed $("$install_dir/wayfindr" version) at $install_dir/wayfindr"
	case :$PATH: in
	*":$install_dir:"*) ;;
	*)
		say "$install_dir is not on your PATH; add it in your shell profile:"
		# shellcheck disable=SC2016 # $PATH is meant literally, for the profile.
		printf '\n  export PATH="%s:$PATH"\n\n' "$install_dir"
		;;
	esac
	say "next: wayfindr init"
}

main "$@"
