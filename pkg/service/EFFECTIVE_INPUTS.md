# Go effective inputs (Core schema v1)

`Service.GetEffectiveInputs` implements the additive Agent RPC and advertises
version 1. `ValidationCapabilities()` remains the independent inventory: lint,
compile, audit, SBOM, artifact build, source package, no-op sync, and the default
`unit` suite with dependency mode `NONE`. Discovery never removes these tasks.

## What is observed

For the current worktree before runner initialization, with workspace mode off,
discovery runs `go list -mod=readonly -deps -json ./...` separately with and without
`-test`. It inherits the native Go environment (including GOFLAGS/build tags,
GOOS, GOARCH and CGO_ENABLED), and sets GOWORK=off like the default runtime
build/test commands. It does not run tests or generators. These are observations
of the default native package invocation, not evidence of a different requested
invocation or a container build.

Production and test graphs retain separate source sets. Go chooses packages and
files; a test-like import name does not exclude production content. Observations
include local module replacements within the workspace, Go and non-Go compiler
files, embeds, module/workspace metadata, and conventional package testdata.
Module dependencies carry immutable Go module checksum identities when available;
missing checksums remain unresolved. Files use workspace-relative paths under
owner `workspace`. Links inside that namespace include link text/mode and consumed
target files. Targets outside the namespace remain unresolved.

Configuration, embeds and fixtures can contain secrets: their paths/modes are
reported as sensitive with unresolved identities. A matching caller-supplied
protected identity is retained without reading or hashing the secret. Ordinary
source and lockfile content is hashed. Compiler diagnostics and environment values
are never returned. Caller context is validated using Core's protected-identity
rules; conflicting observed content is rejected.

Caller-resolved configuration, environment, plugin/toolchain, generators and
shared library/contract identities are retained. Fixture context belongs to tests;
artifact/validation prerequisites belong to artifact build/source package.
Service implementation context belongs only to suites whose advertised dependency
mode requires services, independently of their names. The generic unit suite has
no runtime services or service implementation edges.

## Explicit limits

**All declarations currently have `complete: false`. No task can be excluded or
reused based on this implementation.** Native import discovery cannot prove the
following consumption; fixed unresolved entries make these limitations visible:

- Request-specific targets, selectors, formulas, flags and effective runtime
  configuration; the v1 request carries identities but no structured invocation.
- The actual agent executable, backend and complete compiler/linker toolchain.
- Arbitrary runtime file reads, dynamic fixture selection, external/live state,
  generator execution and undeclared generated inputs.
- Image recipes/contexts, cross-compilation, artifact and validation prerequisites,
  or the audit vulnerability database's immutable snapshot.
- Workspace package expansion, initialized runner backends, or external filesystem
  inputs. Workspace lockfiles are observed, but the graph is not asserted.
- Historical revisions, including historical settings and discovery semantics.
  A nonempty revision returns an incomplete declaration for every inventory task
  without substituting current-worktree inputs. Current snapshots are discovered
  independently, without retaining a previous trace.
- Integration runtime service closure and transitive implementation consumption.
  Extra advertised suites retain explicit unresolved service-closure inputs;
  caller context alone does not establish completeness or startup requirements.

The caller must bind the snapshot token to stable source/context while discovery
runs. Core's `ciinputs` validation and comparison retain conservative whole-service
selection for all these declarations. A narrower observed production trace alone
is never permission to omit production validation after a test-only edit.

## Specializations and release

An embedding specialization that overrides its advertisement must explicitly
advertise v1 and delegate discovery using
`Service.DiscoverEffectiveInputs(ctx, request, itsValidationCapabilities)`.
This preserves its suite names and dependency modes; merely inheriting the generic
RPC does not discover protocol-specific integration semantics. This is the shared
entry point for service-go-grpc adoption in codefly-dev/service-go-grpc#102.

Core v0.3.26 is the minimum dependency providing the RPC. The minimal module
upgrade is necessary to compile this implementation and overlaps the prerequisite
part of #54; immutable agent publication remains in that release work. Full
adoption acceptance in codefly-dev/core#445 still requires execution-bound native
and integration completeness plus a published agent version. This conservative
implementation must not be recorded as completing that acceptance.

Run `go test ./pkg/service -count=1` for the in-process gRPC and native Go fixtures,
and `go test ./...`, `go build ./...`, `go vet ./...` for repository checks.
