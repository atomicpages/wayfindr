#!/bin/sh
# Runs the MCP conformance suite in client mode: the suite's test servers
# and authorization servers measure Wayfindr's upstream side, its MCP client
# and its OAuth client. Called by `make conformance-client`.
#
# For each protocol version it runs, with --requirements, every client
# scenario that version requires (and the not-scored ones the requirement
# set lists), then each not-scored extension scenario on its own, so its
# result is checked too. The suite runs scripts/conformance-client/ once
# per scenario, which starts a throwaway daemon (temp config and data dir,
# master key and client token from the environment, never the keychain),
# plays the user in any auth flow through the admin API, and calls the test
# server's tools through /mcp. See that command's doc comment.
#
# Results go to bin/conformance-client/<version>/ (extensions under
# <version>/extensions/). The exit status is non-zero if any run fails or
# warns on a check not listed in scripts/conformance-baseline/client-<version>.yml
# (per-check entries, <scenario>:<check-id>; the file's header documents the
# format), or if a listed check now passes.
#
# Environment:
#   WAYFINDR_BIN           wayfindr binary (default bin/wayfindr)
#   CONFORMANCE_VERSIONS   protocol versions to run (default: both)
#   CONFORMANCE_PKG        npm package (default pinned below)
#   CONFORMANCE_EXTENSIONS extension scenarios to run on their own (default:
#                          the auth extensions the requirement sets list as
#                          not scored); "" runs none
#   CONFORMANCE_TIMEOUT    per-scenario timeout in ms (default 150000; the
#                          harness gives up at 100s and stops its daemon)
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

wayfindr=${WAYFINDR_BIN:-bin/wayfindr}
versions=${CONFORMANCE_VERSIONS:-"2025-11-25 2026-07-28"}
pkg=${CONFORMANCE_PKG:-@modelcontextprotocol/conformance@0.2.0-alpha.12}
extensions=${CONFORMANCE_EXTENSIONS-"auth/client-credentials-basic auth/client-credentials-jwt auth/enterprise-managed-authorization auth/dpop auth/dpop-nonce auth/wif-jwt-bearer"}
timeout=${CONFORMANCE_TIMEOUT:-150000}
results=$root/bin/conformance-client

case $wayfindr in /*) ;; *) wayfindr=$root/$wayfindr ;; esac
[ -x "$wayfindr" ] || { echo "conformance-client: $wayfindr not found; run make build" >&2; exit 1; }
command -v npx >/dev/null 2>&1 || { echo "conformance-client: npx not found; install Node.js" >&2; exit 1; }

tmp=$(mktemp -d "${TMPDIR:-/tmp}/wayfindr-conformance-client.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# The suite splits --command on spaces, so the harness path must have none.
client=$tmp/conformance-client
case $client in *" "*) echo "conformance-client: temp path $client has a space; set TMPDIR" >&2; exit 1 ;; esac
go build -o "$client" ./scripts/conformance-client

# The harness gives each daemon its own temp dir, master key, and client
# token; HOME is left alone so npx keeps its cache.
export WAYFINDR_BIN="$wayfindr"

rm -rf "$results"
failed=
for v in $versions; do
	baseline=$root/scripts/conformance-baseline/client-$v.yml

	echo
	echo "=== conformance-client $v"
	mkdir -p "$results/$v"
	if npx -y "$pkg" client \
		--command "$client" \
		--requirements "$v" \
		--timeout "$timeout" \
		--expected-failures "$baseline" \
		--output-dir "$results/$v"; then
		echo "=== conformance-client $v: pass"
	else
		echo "=== conformance-client $v: FAIL"
		failed="$failed $v"
	fi

	for s in $extensions; do
		echo
		echo "=== conformance-client $v $s (extension, not scored)"
		mkdir -p "$results/$v/extensions"
		if npx -y "$pkg" client \
			--command "$client" \
			--scenario "$s" \
			--spec-version "$v" --force \
			--timeout "$timeout" \
			--expected-failures "$baseline" \
			--output-dir "$results/$v/extensions" >"$tmp/extension.log" 2>&1; then
			echo "=== conformance-client $v $s: pass"
		else
			cat "$tmp/extension.log"
			echo "=== conformance-client $v $s: FAIL"
			failed="$failed $v:$s"
		fi
	done
done

echo
if [ -n "$failed" ]; then
	echo "conformance-client: unexpected results at:$failed (results in bin/conformance-client/)" >&2
	exit 1
fi
echo "conformance-client: all versions match the baseline (results in bin/conformance-client/)"
