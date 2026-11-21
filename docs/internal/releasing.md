# Releasing

A release is a pushed `v*` tag. Everything else is automated.

## Pipeline

```
ci.yml (push to main/next, PRs)       release.yml (push v* tag)
  ├─ checks.yml                         ├─ checks.yml
  ├─ goreleaser.yml  snapshot: true     ├─ goreleaser.yml  snapshot: false
  │    └─ artifact "dist"               │    ├─ draft release + checksums.txt
  └─ install-test.yml  artifact: dist   │    ├─ build provenance attestation
                                        │    └─ artifact "dist" (the draft's assets)
                                        ├─ install-test.yml  artifact: dist
                                        └─ publish: draft → public (Latest unless -rc)
```

| Unit                                 | What it does                                                                                                                                                            |
| ------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `.github/actions/setup-go`           | Latest stable Go and the module/build caches. Every Go job uses it.                                                                                                     |
| `.github/workflows/checks.yml`       | golangci-lint, `go test ./...` on Linux, macOS, and Windows, `make race`. All three test jobs gate.                                                                     |
| `.github/workflows/goreleaser.yml`   | GoReleaser (`.goreleaser.yaml`): 6 targets and `checksums.txt`. Snapshot or draft release.                                                                              |
| `.github/workflows/install-test.yml` | Runs `docs/public/install.sh` and `install.ps1` piped into the shell, as users do: version output, a tampered checksum, and (Windows) an upgrade over a running binary. |
| `docs/public/install.{sh,ps1}`       | The one-line installers. Pages serves them from `main`, not per release.                                                                                                |

A release is public only after its provenance is attested and the installers
have installed its exact archives. If any step fails, the draft stays a draft:
fix, delete the draft and the tag, and tag again.

Version strings come from `v*` tags only. The `gate/*` milestone tags never
become a version (Makefile `--match 'v[0-9]*'`, `GORELEASER_CURRENT_TAG`) or a
changelog boundary (`GORELEASER_PREVIOUS_TAG` in `goreleaser.yml`). Release
builds print `wayfindr v0.1.0 (…)`, like `make build` on a tagged commit.

CI and releases build with the latest stable Go (`setup-go`), not the minimum in
`go.mod`, so they get toolchain and standard-library security fixes. Rebuilding
an old tag later can therefore produce different checksums; the published
`checksums.txt` and attestations are the record.

## Cut a release

1. Make sure `main` is green in CI. The snapshot build and the install tests
   there are the dry run.
2. Tag and push:

   ```sh
   git tag -a v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```

3. Watch the Release workflow. When it is green, the release is public and its
   archives are attested.
4. Run the real one-liner once, which proves the anonymous download path and the
   installer Pages serves:
   `curl -fsSL https://atomicpages.github.io/wayfindr/install.sh | sh`. On
   Windows, run `irm … | iex` as well.

The README and the installation guide advertise the installers, and `latest` has
nothing to download until the first non-rc release exists. Merge the docs to
`main` together with, or after, the first release.

A tag with a suffix (`v0.2.0-rc.1`) publishes a pre-release. The installers'
`latest` skips it, so an rc is a safe end-to-end rehearsal:
`curl -fsSL https://atomicpages.github.io/wayfindr/install.sh | sh -s -- v0.2.0-rc.1`.

## Locally

`make snapshot` builds what the release would into `dist/`, without publishing.
It needs `goreleaser` v2 on `PATH`. To try the installer on it:

```sh
WAYFINDR_DOWNLOAD_URL=file://$PWD/dist WAYFINDR_INSTALL_DIR=/tmp/wf sh docs/public/install.sh
```

## Repository settings

- The repository must be public: the installers download release assets
  anonymously.
- Add a tag ruleset for `v*` that restricts creation, update, and deletion to
  maintainers, so a release can't be retagged.
- Pages must deploy from GitHub Actions (`docs.yml`), which also publishes the
  installers.

## Updating pinned versions

Actions are pinned by commit SHA with the tag in a comment. GoReleaser
(`goreleaser.yml`), golangci-lint (`checks.yml`), and actionlint (`ci.yml`) are
pinned by version. Choose versions released at least two weeks earlier.
