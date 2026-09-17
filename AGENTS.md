# Working in codefly-dev/service-go

`service-go` is the **generic Go agent** — the codefly plugin binary that serves
the agent gRPC surfaces (Agent, Runtime, Code, Tooling, Builder) for plain Go
services. All logic sits under `./pkg` so **specializations** (`service-go-grpc`,
…) embed and compose these types instead of forking them. A method changed here
changes every Go specialization, including ones not in this repo.

It does **not** own: the agent protocol, the Go toolchain runners, the code
server, or templating mechanics — those are `codefly-dev/core`, pinned in
`go.mod`. It does not build or push images: `Builder.Build` emits a Docker
*recipe* the CLI executes with buildx. It does not own the `codefly` CLI. When
the change you need belongs to core, the CLI, or a specialization, it gets made
there.

## How to behave

Fleet standard — [handbook#68](https://github.com/obin-ai/handbook/issues/68).
These land hard here: this repo is a *base* others embed, so a defect surfaces
in a specialization or a user's service, and the temptation is to patch it where
it was seen rather than where it lives.

- **A gap in the tooling is a bug in the tooling — never a reason to reach
  around it.** When something needs a step core, the CLI, or a companion does
  not perform, the answer is a capability fixed in whichever one owns it, and
  named in the PR. It is never a hand-assembled substitute — not as a
  "workaround", not "just this once", not "until the capability lands".
- **Never hack. Provide the best fix, even when it spans repos.** The fix living
  in `codefly-dev/core` or `codefly-dev/cli` is not a reason to work around it
  here. Open the PR there and consume the reviewed result. When it genuinely
  cannot be fixed now, the deliverable is a precise issue against that owner
  plus an explicitly labelled stopgap — never an unlabelled one.
- **Classify every change that makes something work**, in the PR body: a *fix*
  at the place that owns the behaviour, or a *hack*. A hack does not become a
  fix by working, by being small, by being local, or by the real fix belonging
  elsewhere.
- **Never hardcode what the system resolves** — injected environment, derived
  ports, service addresses, credentials copied out of another component. A Go
  service's endpoints and env come from core's configuration flow through
  `Runtime.Init`; the source root comes from `ResolveSourceLocation`, never a
  literal `code/`. Typing one encodes something true only on one machine for ten
  minutes, and it fails quietly — a runtime missing a credential can skip
  registration *silently*, so the service boots, serves, and is simply absent.
- **Diagnose, do not pattern-match.** "It started working when I set X" is not a
  diagnosis — set X back and confirm it breaks. Do not trust an error message
  before checking its claim: the recipe walk once failed with a filesystem name
  limit, and the cause was the walk copying its own output back into itself.
- **Say what you did not verify.** Unverified is not the same as working. `go
  test ./...` is not the packaging suite, and neither is a build; if you could
  not exercise something, the PR says so.

## Build and test

Derived from `.github/workflows/ci.yml`. The Go version comes from `go.mod`
(1.27.0) — CI reads it with `go-version-file`, so it is never pinned twice.

```bash
go build ./...          # CI: Build
go test ./...           # CI: Test
go vet ./...            # CI: Vet
```

**Two suites skip themselves when their prerequisite is absent**, so a green
`go test ./...` does not mean they ran. Both are runnable locally:

| Suite | Opt in with | Needs |
| --- | --- | --- |
| `TestPackageRealCGO` (`./pkg/builder`) | `SERVICE_GO_PACKAGE_TESTS=required` | Docker |
| `TestManifestGuardRender` (root) | `CODEFLY_MANIFEST_*` (see below) | — |
| `./pkg/runtime` nix integration | — | a sibling `core` checkout for `runners/base/testdata` |

The manifest guard runs in CI through a reusable workflow in
`codefly-dev/.github`, so its local invocation is written down nowhere else —
`.claude/skills/verify-changes/` has the full walk for both.

**Never mock.** Tests drive the real gRPC servers against real temp workspaces
and a real Go toolchain. If a boundary is hard to reach, reach it anyway.

## Where things live

| Path | Owns |
| --- | --- |
| `main.go` | the whole wiring; embeds the template trees (`//go:embed` cannot reach up from a subpackage) |
| `pkg/service` | shared state (`SourceLocation`, `ActiveEnv`, `Settings`), the Go source-root convention, capability advertisement, effective-input discovery |
| `pkg/runtime` | the dev lifecycle — `Init`/`Start`/`Stop`, plus `Build`, `Test`, `Lint` |
| `pkg/code` | Go-specific layer over core's `GoCodeServer`: the in-process formatter/import fixer and the dependency verbs |
| `pkg/tooling` | adapter only — translates tooling requests onto Code and Runtime |
| `pkg/builder` | `Create` (scaffold), `Build` (emit the recipe), `Package` (cross-compile with CGO), `Audit`, `SBOM`, `Deploy` |
| `templates/` | the *content* rendered by core's templator: `factory` (scaffold, never overwrites), `builder` (Dockerfile, regenerated), `deployment` (kustomize) |
| root `*_test.go` | meta-invariants about CI and repo policy that no package test would notice |

`pkg/service/EFFECTIVE_INPUTS.md` covers input discovery in depth — read it
rather than re-deriving from `go list`.

## Rules that bite

- **`Builder.Build` emits a recipe; it does not build an image.** It renders the
  Dockerfile and copies the source into `<output>/code`, and the CLI runs
  buildx. A service with `source-dir: "."` roots its sources where the CLI puts
  that recipe, so the walk skips the output directory **by directory identity**
  (`os.SameFile`), not by name — a source package named `builder` must still be
  copied.
- **`agent.codefly.yaml`'s `version` must equal the release tag** (`v0.0.51` →
  `0.0.51`). It is embedded in the binary and nothing in CI checks it. See
  `.claude/skills/release-go-agent/`.
- **Releases require CGO** — core's source inspection uses real tree-sitter
  grammars, so every target is built through the `goreleaser-cross` image, whose
  Go version must match `go.mod`. Disabling CGO to make a build pass ships an
  agent whose parser cannot work.
- **The archive name is a contract with core's downloader**
  (`service-{name}_{version}_{os}_{arch}.tar.gz`). Renaming it breaks install,
  not CI.
- **`Settings` is embedded `yaml:",inline"` by specializations.** Changing its
  shape breaks their YAML fixtures; `pkg/service/service_test.go` guards this.
- **Root policy tests enforce repo rules**, not behaviour: every workflow `uses:`
  is pinned to a 40-hex SHA, each Dependabot ecosystem is one catch-all group,
  and PR CI must run `go test ./...`. A new workflow with a tag reference fails
  `go test ./...`.

## Procedures

Step-by-step walks live in `.claude/skills/`, loaded on demand rather than
carried here:

- `verify-changes` — the full local gate walk derived from `ci.yml`, including
  the two suites that skip silently.
- `release-go-agent` — cutting a version, and the three contracts a release has
  to satisfy.

## Workflow

- Branch and PR; never commit to `main`. Conventional Commits for the title.
- Keep this file under ~150 lines (hard cap 200). Push depth into a nested
  `AGENTS.md` beside what it describes, into `.claude/skills/`, or into a
  package doc. `agent_context_test.go` enforces that budget under `go test
  ./...`, so CI holds it with no workflow change.
- `CLAUDE.md` is a pointer to this file. Keep one canonical source.
- Treat this file as code: the PR that changes a process updates it.
