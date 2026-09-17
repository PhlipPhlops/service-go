---
name: release-go-agent
description: Cut a release of the codefly-dev/service-go agent binary, and satisfy the three contracts a release has to hold — the embedded agent.codefly.yaml version matching the tag, the CGO cross toolchain matching go.mod's Go version, and the archive name core's downloader resolves. Use when asked to release, tag, publish, or bump the version of this agent, when a published version installs but misbehaves and you suspect a version or archive mismatch, or when bumping the Go version in go.mod, which moves two independent goreleaser-cross pins.
---

# Releasing the Go agent

The release is made by pushing a tag. `.github/workflows/releaser.yml` fires on
`v*`, runs `go test -v ./...`, then runs GoReleaser inside
`ghcr.io/goreleaser/goreleaser-cross` and publishes with the `GH_PAT` secret.

**Never build or upload artifacts by hand.** The cross image supplies the
darwin and linux C compilers that CGO needs; a local `goreleaser release` or a
hand-uploaded binary is not the same artifact and will not link tree-sitter.

## The walk

1. Confirm `main` is green and the core pin in `go.mod` is the one you intend to
   ship. Agent releases are usually paired with a core bump.
2. Set `version:` in `agent.codefly.yaml` to the new version **without** the
   leading `v`.
3. Commit that alone, as `release: vX.Y.Z` — a release commit touches one file.
4. Tag that exact commit `vX.Y.Z` and push the tag.

## Three contracts a release has to hold

### The embedded version must equal the tag

`agent.codefly.yaml` is embedded into the binary (`main.go`) and is what the
agent reports about itself. Tag `v0.0.51` ships `version: 0.0.51`.

**Nothing in CI checks this.** A mismatch releases successfully and is only
visible later as an agent that reports a version nobody can locate. Verify
before tagging:

```bash
grep '^version:' agent.codefly.yaml   # must match the tag, minus the v
```

### The archive name is a contract with core's downloader

`.goreleaser.yaml` builds `service-{name}_{version}_{os}_{arch}.tar.gz`, with
the binary `service-{name}` inside. Core resolves that exact shape in
`agents/manager/downloader_url.go`. Changing `name_template`, the archive
format, or the binary name breaks installation for every user — and breaks
nothing in CI, because CI never installs the release it just built.

### CGO targets must match the Go version

Releases are CGO builds for `darwin/{amd64,arm64}` and `linux/{amd64,arm64}`,
each with its own `CC`/`CXX` in `.goreleaser.yaml`. When you change the Go
version in `go.mod`, **two independent pins move with it**:

| Pin | Used for |
| --- | --- |
| `ghcr.io/goreleaser/goreleaser-cross` in `.github/workflows/releaser.yml` | building *this agent's* release binaries |
| `ghcr.io/goreleaser/goreleaser-cross` in `pkg/builder/builder.go` | packaging *user services* at runtime |

They are set independently and neither one's drift is caught by a build. The
packaging suite covers the second:

```bash
SERVICE_GO_PACKAGE_TESTS=required \
  go test -race -count=1 -timeout 15m ./pkg/builder -run TestPackageRealCGO
```

Run it before tagging a release that moved the Go version. Dropping a target
from `.goreleaser.yaml` to make a release pass removes a platform users install
on; fix the toolchain instead.

## After the tag

Check the workflow run and that the release carries all four archives. If the
job failed after publishing a partial release, delete the release and the tag
and cut the next patch — do not re-tag the same version, since consumers may
already have resolved it.
