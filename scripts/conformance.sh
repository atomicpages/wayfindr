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
# Existing-server mode: set CONFORMANCE_URL (an existing daemon's /mcp URL,
# http or https, for example one behind a tunnel) and CONFORMANCE_TOKEN (its
# bearer token) to run the same suite against that server instead. No daemon
# starts, and the proxy forwards to CONFORMANCE_URL with the token. Set both or
# neither; setting one alone is an error. The token reaches the proxy only in
# its environment (the script unsets CONFORMANCE_TOKEN as soon as it reads it,
# so npx never inherits it), and the script never prints it. Readiness is the
# suite path answering with any status below 500, not /healthz, which is
# loopback-only and so unreachable through a tunnel. Through -tls the proxy
# sends Host as written in the URL (the default port 443 is not appended).
#   CONFORMANCE_URL=http://127.0.0.1:7777/mcp CONFORMANCE_TOKEN=... make conformance
#
# Results go to bin/conformance/<version>/. The exit status is non-zero if
# any version fails or warns on a check not listed in its expected-failures
# file, scripts/conformance-baseline/<version>.yml (per-check entries,
# <scenario>:<check-id>; the file's header documents the format), or if a
# listed check now passes.
#
# Environment:
#   WAYFINDR_BIN           wayfindr binary (default bin/wayfindr); unused in
#                          existing-server mode
#   CONFORMANCE_URL        /mcp URL of an existing server (needs CONFORMANCE_TOKEN)
#   CONFORMANCE_TOKEN      bearer token for CONFORMANCE_URL (needs CONFORMANCE_URL)
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
url=${CONFORMANCE_URL:-}
token=${CONFORMANCE_TOKEN:-}
# The token must not leak into npx and its children: drop it from the
# environment now; in existing-server mode it then reaches only the proxy.
unset CONFORMANCE_TOKEN

if [ -n "$url" ] && [ -z "$token" ]; then
	echo "conformance: CONFORMANCE_URL is set but CONFORMANCE_TOKEN is not; set both or neither" >&2
	exit 1
fi
if [ -z "$url" ] && [ -n "$token" ]; then
	echo "conformance: CONFORMANCE_TOKEN is set but CONFORMANCE_URL is not; set both or neither" >&2
	exit 1
fi

# Existing-server mode: split CONFORMANCE_URL into the proxy's target
# authority (host:port, with the default port filled in), whether to use TLS,
# and the path the suite must request. The URL is never echoed.
suite_path=/mcp
if [ -n "$url" ]; then
	case $url in
	http://*) scheme=http; rest=${url#http://}; default_port=80 ;;
	https://*) scheme=https; rest=${url#https://}; default_port=443 ;;
	*) echo "conformance: CONFORMANCE_URL must start with http:// or https://" >&2; exit 1 ;;
	esac
	authority=${rest%%/*}
	case $rest in
	*/*) suite_path=/${rest#*/} ;;
	*) suite_path=/ ;;
	esac
	case $authority in
	''|*@*) echo "conformance: CONFORMANCE_URL needs a host and no user:password; send the token in CONFORMANCE_TOKEN" >&2; exit 1 ;;
	\[*\]) authority=$authority:$default_port ;;
	*:*) ;;
	*) authority=$authority:$default_port ;;
	esac
	case $suite_path in
	*\?*|*\#*) echo "conformance: CONFORMANCE_URL must not have a query or fragment" >&2; exit 1 ;;
	esac
fi

if [ -z "$url" ]; then
	case $wayfindr in /*) ;; *) wayfindr=$root/$wayfindr ;; esac
	[ -x "$wayfindr" ] || { echo "conformance: $wayfindr not found; run make build" >&2; exit 1; }
fi
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

# wayfindr_not_found reports whether the 404 whose headers and body are in the
# two files is the one Wayfindr gives (httpd: http.NotFound) and not a tunnel
# provider's. It mirrors isWayfindrNotFound in internal/daemon/remotetest.go.
wayfindr_not_found() {
	headers=$1
	body=$2
	if grep -qi '^ngrok-error-code:' "$headers"; then
		return 1
	fi
	grep -qi '^content-type:[[:space:]]*text/plain' "$headers" || return 1
	[ "$(tr -d '\r' <"$body" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')" = "404 page not found" ]
}

kit=$tmp/conformancekit
go build -o "$kit" ./internal/testutil/cmd/conformancekit

if [ -n "$url" ]; then
	proxy_port=$("$kit" freeport)
	if [ "$scheme" = https ]; then
		WAYFINDR_CLIENT_TOKEN=$token "$kit" proxy -listen "127.0.0.1:$proxy_port" -target "$authority" -tls &
	else
		WAYFINDR_CLIENT_TOKEN=$token "$kit" proxy -listen "127.0.0.1:$proxy_port" -target "$authority" &
	fi
	proxy_pid=$!
	unset token WAYFINDR_CLIENT_TOKEN

	# /healthz is loopback-only (ADR 0021), so a tunnel answers it with 404 and
	# cannot gate readiness. Wait for the proxy to answer, then for the server
	# to answer the suite path with anything below 500 (a 4xx from a bare POST
	# still proves the tunnel reaches a live server; 502/504/530 do not). A 404
	# counts only when it is Wayfindr's own, the way daemon/remotetest.go reads
	# it: a text/plain body of "404 page not found" and no Ngrok-Error-Code
	# header. A tunnel provider's 404, such as ngrok's page for an offline
	# tunnel, means no server answered.
	ready=
	code=000
	i=0
	while [ $i -lt 60 ]; do
		kill -0 "$proxy_pid" 2>/dev/null || break
		code=$(curl -sS -D "$tmp/probe.headers" -o "$tmp/probe.body" -w '%{http_code}' --connect-timeout 2 -m 10 \
			-X POST -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
			-d '{}' "http://127.0.0.1:$proxy_port$suite_path" 2>/dev/null) || code=000
		case $code in
		000) ;;
		404)
			if wayfindr_not_found "$tmp/probe.headers" "$tmp/probe.body"; then
				ready=1
				break
			fi
			;;
		[1-4]??)
			ready=1
			break
			;;
		esac
		sleep 1
		i=$((i + 1))
	done
	[ -n "$ready" ] || { echo "conformance: $authority did not answer $suite_path through the proxy (last HTTP status $code)" >&2; exit 1; }
	echo "conformance: existing server $authority, suite via proxy on 127.0.0.1:$proxy_port"
	suite_url="http://127.0.0.1:$proxy_port$suite_path"
else
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
	suite_url="http://127.0.0.1:$proxy_port/mcp"
fi

rm -rf "$results"
failed=
for v in $versions; do
	echo
	echo "=== conformance $v"
	mkdir -p "$results/$v"
	if npx -y "$pkg" server \
		--url "$suite_url" \
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
