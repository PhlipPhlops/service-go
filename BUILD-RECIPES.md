# Image recipe context

The Go agent assembles a self-contained build tree in the caller's
`BuildRequest.output_directory`. It carries repository-local Go replacements
under `code/_replace/` and rewrites only the emitted go.mod. The original service
tree remains unchanged.

The recipe explicitly declares `context_root=OUTPUT` and `context="."`. The
executor must build that emitted tree, not the original service: the latter still
contains sibling-relative replacements that cannot resolve in an image.

This declaration requires `codefly.dev/docker-build-recipe/v4` support in the
executor. An older host rejects the plan before building. Upgrade the CLI before
adopting an agent release containing this change; do not edit the source go.mod,
copy siblings by hand, or change the Dockerfile to compensate.

Core owns the protocol and integrity check; CLI owns Docker execution. Producer
tests check the declared root over gRPC and compile the relocated emitted sources
with the real Go toolchain. End-to-end qualification must also run the CLI image
build, because compiling emitted sources alone cannot prove the host selects them.
