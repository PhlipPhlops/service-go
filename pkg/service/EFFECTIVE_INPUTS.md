# Go effective inputs (Core schema v1)

`Service.GetEffectiveInputs` implements the additive Agent RPC and advertises
version 1. `ValidationCapabilities()` remains the independent inventory. Discovery
never removes required phases or named suites.

## Ownership and execution

An operation owner can set `Service.InputPlans` before serving RPCs. The planner
receives the snapshot request and returns `EffectiveInputPlan` values keyed by
phase and suite. Each plan explicitly names its Go invocation, consumed identity
inputs, runtime service requirements and completeness assertion. Caller context
resolves matching declared keys; unrelated inputs are not broadcast to tasks
merely because they have the same kind. Specializations pass their authoritative
inventory to `DiscoverEffectiveInputs`.

`GoInputInvocation` carries the source, target, runner and effective environment.
Runtime.Build uses this description for execution; a plan can use the same
invocation for discovery. Initialized runners remain usable for discovery, and
injected GOFLAGS/platform/CGO settings reach the child process. Native discovery
runs `go list -deps -json`, separately with and without `-test`. Standalone modules
use GOWORK=off and read-only module mode; vendored projects retain vendor selection.
Workspace default package expansion requires an explicit resolved target.

Without an operation-owned plan, compile receives the production trace and
lint/tests receive the test trace. Other phases receive shared metadata and
explicit unresolved prerequisites, rather than a fabricated production graph.
These default observations remain incomplete: they do not establish arbitrary
request-specific execution, image contexts, generators or runtime reads.

A plan's `Complete` assertion belongs to the implementation of that operation,
not to the RPC client. It must account for all consumption beyond native imports,
including selectors, dynamic fixtures, effective environment, toolchains,
plugin identities and dependency closures. Native discovery failures revoke the
assertion. Core separately requires resolved plugin/toolchain and other identities
before any declaration becomes eligible for reuse. Source stability during
discovery remains part of the caller's snapshot binding.

## Closed generic operation

The generic binary registers `GenericGoInputPlans` alongside its no-op Builder.Sync.
That plan identifies the agent executable and its embedded Go runtime by content,
and declares Sync complete: the operation consumes no source or environment.
Source-only edits therefore do not invalidate Sync. The shared Service constructor
does not register this plan; specializations that generate code must supply their
own Sync consumption. A nonempty historical revision always declines completeness
because historical discovery semantics have not been reproduced.

## Files and sensitive identities

Native observations include local replacements, vendored sources, non-Go compiler
files, embeds, module/workspace metadata and conventional testdata. File paths are
workspace-relative. Symlinks include link text/mode and consumed targets inside
the workspace; external targets remain unresolved.

Classification precedes content hashing across production/test graphs and input
kinds. A lockfile alias cannot downgrade a sensitive configuration or fixture
target. Configuration, embeds and fixtures remain unresolved unless matching
protected identities are supplied. Secret environment/configuration targets are
not given ordinary hashes through another input kind. Core validates protected
identities and rejects contradictory declarations without echoing their contents.

Legal filesystem names that cannot be represented by the Core path contract
produce an unresolved `unrepresentable-filesystem-path` entry. They do not abort
other tasks or get normalized into a different filename. Collection, hashing and
context merging honor cancellation; context matching uses keyed maps.

## Validation and release

`go test ./pkg/service -count=1` covers real Agent RPCs, Runtime initialization and
build execution, explicit targets and context ownership, protected aliases,
unsupported paths, cancellation, legacy fallback and complete no-op Sync identity.
Run `go test ./...`, `go build ./...` and `go vet ./...` for repository checks.

Core v0.3.26 is the compiling prerequisite tracked in #54. Immutable agent
publication and downstream propagation to codefly-dev/service-go-grpc#102 are not
performed by this draft. Full codefly-dev/core#445 adoption still requires those
release artifacts and operation-owned declarations for the remaining unsupported
execution and integration cases; Sync completeness is not evidence for them.
