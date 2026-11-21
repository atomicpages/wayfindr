#!/bin/sh
# Runs the MCP conformance suite (server mode) against a throwaway daemon's
# /mcp, once per client protocol version. Called by `make conformance`.
#
# Nothing touches the real home, config, data, or keychain: the config file,
# data directory, and XDG bases point into a temp dir, and the master key
# and client token come from WAYFINDR_SECRET_KEY and WAYFINDR_CLIENT_TOKEN,
# so the keychain is never consulted. The daemon has one stdio connector,
# a fakeupstream served by conformancekit.
#
# The suite cannot send an Authorization header, so it talks to a
# conformancekit proxy that adds the bearer (see its package doc).
#
# Results go to bin/conformance/<version>/. The exit status is non-zero if
# any version fails or warns on a check not listed in its expected-failures
# file, scripts/conformance-baseline/<version>.yml (per-check entries,
# <scenario>:<check-id>; the file's header documents the format), or if a
# listed check now passes.
#
# Environment:
#   WAYFINDR_BIN           wayfindr binary (default bin/wayfindr)
#   CONFORMANCE_VERSIONS   protocol versions to run (default: all three)
#   CONFORMANCE_PKG        npm package (default pinned below)
#   CONFORMANCE_ARGS       scenario selection (default "--suite all", which
#                          adds the draft and pending scenarios that cover
#                          the 2026-07-28 stateless wire); for example
#                          "--scenario tools-list"
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

wayfindr=${WAYFINDR_BIN:-bin/wayfindr}
versions=${CONFORMANCE_VERSIONS:-"2025-06-18 2025-11-25 2026-07-28"}
pkg=${CONFORMANCE_PKG:-@modelcontextprotocol/conformance@0.2.0-alpha.12}
results=$root/bin/conformance
selection=${CONFORMANCE_ARGS:---suite all}

case $wayfindr in /*) ;; *) wayfindr=$root/$wayfindr ;; esac
[ -x "$wayfindr" ] || { echo "conformance: $wayfindr not found; run make build" >&2; exit 1; }
command -v npx >/dev/null 2>&1 || { echo "conformance: npx not found; install Node.js" >&2; exit 1; }

tmp=$(mktemp -d "${TMPDIR:-/tmp}/wayfindr-conformance.XXXXXX")
daemon_pid=
proxy_pid=
cleanup() {
	status=$?
	for pid in $proxy_pid $daemon_pid; do
		kill "$pid" 2>/dev/null || true
	done
	for pid in $proxy_pid $daemon_pid; do
		wait "$pid" 2>/dev/null || true
	done
	if [ "$status" -ne 0 ] && [ -f "$tmp/daemon.log" ]; then
		echo "--- daemon log (last 40 lines)" >&2
		tail -n 40 "$tmp/daemon.log" >&2 || true
	fi
	rm -rf "$tmp"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

kit=$tmp/conformancekit
go build -o "$kit" ./internal/testutil/cmd/conformancekit

# Isolate everything Wayfindr resolves (internal/paths, internal/secrets,
# internal/clienttoken). HOME is left alone so npx keeps its cache.
export WAYFINDR_CONFIG="$tmp/config/wayfindr.toml"
export WAYFINDR_DATA_DIR="$tmp/data"
export XDG_CONFIG_HOME="$tmp/xdg/config"
export XDG_DATA_HOME="$tmp/xdg/data"
export XDG_STATE_HOME="$tmp/xdg/state"
export XDG_CACHE_HOME="$tmp/xdg/cache"
WAYFINDR_SECRET_KEY=$(head -c 32 /dev/urandom | base64 | tr -d '\n')
WAYFINDR_CLIENT_TOKEN=wf_conformance_$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
export WAYFINDR_SECRET_KEY WAYFINDR_CLIENT_TOKEN

port=$("$kit" freeport)
proxy_port=$("$kit" freeport)
while [ "$proxy_port" = "$port" ]; do proxy_port=$("$kit" freeport); done

# init never overwrites an existing config, so write ours first.
mkdir -p "$tmp/config"
cat >"$WAYFINDR_CONFIG" <<EOF
[server]
listen = "127.0.0.1:$port"

[storage]
driver = "sqlite"

[connectors.fake]
command = "$kit"
args    = ["upstream"]
EOF

"$wayfindr" init >"$tmp/init.txt" 2>&1 || { cat "$tmp/init.txt" >&2; exit 1; }

"$wayfindr" start --foreground --log-level debug --log-file "$tmp/daemon.log" &
daemon_pid=$!
"$kit" proxy -listen "127.0.0.1:$proxy_port" -target "127.0.0.1:$port" &
proxy_pid=$!

ready=
i=0
while [ $i -lt 100 ]; do
	if curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 &&
		curl -fsS "http://127.0.0.1:$proxy_port/healthz" >/dev/null 2>&1; then
		ready=1
		break
	fi
	kill -0 "$daemon_pid" 2>/dev/null || break
	sleep 0.2
	i=$((i + 1))
done
[ -n "$ready" ] || { echo "conformance: daemon did not become healthy" >&2; exit 1; }
echo "conformance: daemon on 127.0.0.1:$port, suite via proxy on 127.0.0.1:$proxy_port"

rm -rf "$results"
failed=
for v in $versions; do
	echo
	echo "=== conformance $v"
	mkdir -p "$results/$v"
	if npx -y "$pkg" server \
		--url "http://127.0.0.1:$proxy_port/mcp" \
		--spec-version "$v" \
		--expected-failures "$root/scripts/conformance-baseline/$v.yml" \
		--output-dir "$results/$v" $selection; then
		echo "=== conformance $v: pass"
	else
		echo "=== conformance $v: FAIL"
		failed="$failed $v"
	fi
done

echo
if [ -n "$failed" ]; then
	echo "conformance: unexpected failures at:$failed (results in bin/conformance/)" >&2
	exit 1
fi
echo "conformance: all versions match the baseline (results in bin/conformance/)"
