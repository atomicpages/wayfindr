# Only v* tags name a version; the gate/* milestone tags are ignored.
VERSION ?= $(shell git describe --tags --match 'v[0-9]*' --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
PKG     := github.com/atomicpages/wayfindr/internal/cli
# Keep in step with builds[].ldflags in .goreleaser.yaml.
LDFLAGS := -s -w -X $(PKG).version=$(VERSION) -X $(PKG).commit=$(COMMIT)

export CGO_ENABLED := 0

# Throwaway Postgres for test-postgres: pgvector 0.8.1 on Postgres 17.
PG_IMAGE ?= pgvector/pgvector:0.8.1-pg17

.PHONY: build test race lint check snapshot test-postgres bench-postgres conformance conformance-client brands

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/wayfindr ./cmd/wayfindr

test:
	go test ./...

race:
	CGO_ENABLED=1 go test -race ./...

lint:
	golangci-lint run ./...

check: lint test race build

# Refreshes internal/webui/brands.gz, the theSVG slug index the console's
# connector logos match against, from theSVG's main (needs the network).
# Pin a commit with BRANDS_SHA=<sha>. Commit the result with CREDITS updated.
brands:
	go run ./scripts/brand-index $(if $(BRANDS_SHA),-sha $(BRANDS_SHA))

# Builds every release target and checksums.txt into dist/ without publishing.
# Needs goreleaser v2 on PATH; CI runs this same target. The version is the
# newest v* tag (v0.0.0 before the first release), never a gate/* tag.
snapshot:
	GORELEASER_CURRENT_TAG=$$(git describe --tags --match 'v[0-9]*' --abbrev=0 2>/dev/null || echo v0.0.0) \
		goreleaser release --snapshot --clean

# Runs the store tests (the search parity suite in internal/store/parity
# included) and the init tests against a throwaway pgvector container on a
# random loopback port. The container is removed however the tests end.
# bench-postgres runs the Postgres search benchmark (p50/p95 latency at about
# 5,000 tools, design §7.5) the same way.
PG_GO_TEST = go test -count=1 ./internal/store/... ./internal/setup/...
bench-postgres: PG_GO_TEST = go test -count=1 -run '^$$' -bench . -benchtime 300x ./internal/store/postgres/
test-postgres bench-postgres:
	@set -e; \
	name=wayfindr-test-pg-$$$$; \
	trap 'docker rm -f '"$$name"' >/dev/null 2>&1 || true' EXIT INT TERM; \
	docker run -d --name "$$name" -e POSTGRES_PASSWORD=wayfindr -p 127.0.0.1::5432 $(PG_IMAGE) >/dev/null; \
	ready=; for i in $$(seq 1 60); do \
		if docker exec "$$name" pg_isready -U postgres -h 127.0.0.1 >/dev/null 2>&1; then ready=1; break; fi; \
		sleep 1; \
	done; \
	if [ -z "$$ready" ]; then echo "postgres did not become ready" >&2; docker logs "$$name" >&2; exit 1; fi; \
	port=$$(docker port "$$name" 5432/tcp | head -n1 | sed 's/.*://'); \
	WAYFINDR_TEST_PG_DSN="postgres://postgres:wayfindr@127.0.0.1:$$port/postgres?sslmode=disable" \
		$(PG_GO_TEST)

# Runs the MCP conformance suite against /mcp of a throwaway daemon, once per
# client protocol version (needs Node.js for npx). Not part of check: it
# downloads the suite. See scripts/conformance.sh and docs/internal/compat.md.
conformance: build
	./scripts/conformance.sh

# Runs the suite's client scenarios against Wayfindr's upstream side (its MCP
# and OAuth client) at 2025-11-25 and 2026-07-28, plus the not-scored auth
# extensions; scripts/conformance-client/ plays the agent and the user (needs
# Node.js for npx). Not part of check, like conformance. See
# scripts/conformance-client.sh and docs/internal/compat.md.
conformance-client: build
	./scripts/conformance-client.sh
