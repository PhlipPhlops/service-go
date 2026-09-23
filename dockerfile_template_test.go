package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
)

// netrcMount is the BuildKit mount the CLI builds against: the secret is named
// "netrc", it lands where git and the go tool look for credentials, and it is
// optional so a build with no private module needs no secret at all.
const netrcMount = "--mount=type=secret,id=netrc,target=/root/.netrc,required=false"

// TestDockerfileFetchesPrivateModulesThroughAnOptionalSecret holds the contract
// the CLI builds against for private Go modules: every dependency download runs
// with the "netrc" BuildKit secret mounted at /root/.netrc, where both git and
// the go tool read credentials, and GOPRIVATE is a build argument declared
// before that step so the toolchain sees the host's value. The mount is
// optional so a build without private modules — and any consumer rebuilding the
// vendored recipe without a secret — renders and builds the same Dockerfile.
// The credential must never reach an image: no ENV, no COPY, and nothing in the
// runtime stage may name it. Without this, `go mod download` in the builder
// stage has no way to authenticate and a service that imports a private module
// fails with "could not read Username for 'https://github.com'".
func TestDockerfileFetchesPrivateModulesThroughAnOptionalSecret(t *testing.T) {
	// Judge the recipe the agent actually emits, not the template alone: a
	// rendering that dropped the mount would still leave the source correct.
	b, ctx := loadedBuilder(t)
	out := filepath.Join(t.TempDir(), "recipe")
	resp, err := b.Build(ctx, &builderv0.BuildRequest{
		OutputDirectory: out,
		BuildContext: &builderv0.BuildContext{
			Kind: &builderv0.BuildContext_DockerBuildContext{
				DockerBuildContext: &builderv0.DockerBuildContext{DockerRepository: "registry.example.com"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if resp.GetResult().GetDockerBuildPlan() == nil {
		t.Fatalf("expected a plan, got state %v message %q",
			resp.GetState().GetState(), resp.GetState().GetMessage())
	}
	contents, err := os.ReadFile(filepath.Join(out, "builder", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	rendered := string(contents)

	builderStage, runtimeStage, found := strings.Cut(rendered, "# Final stage")
	if !found {
		t.Fatalf("runtime stage marker is missing:\n%s", rendered)
	}

	// GOPRIVATE is declared as a build argument before the download so the RUN
	// inherits it as an environment variable; it must not be an ENV, which
	// would persist a host setting into the stage configuration.
	if !strings.Contains(builderStage, "\nARG GOPRIVATE\n") {
		t.Errorf("builder stage does not declare ARG GOPRIVATE:\n%s", builderStage)
	}
	if strings.Contains(rendered, "ENV GOPRIVATE") {
		t.Error("GOPRIVATE is an ENV; it must stay a build argument")
	}
	if arg, download := strings.Index(builderStage, "ARG GOPRIVATE"), strings.Index(builderStage, "go mod download"); arg < 0 || download < 0 || arg > download {
		t.Errorf("ARG GOPRIVATE (%d) must precede go mod download (%d)", arg, download)
	}

	// Every dependency download mounts the secret. A RUN spans continuation
	// lines, so judge whole instructions rather than raw lines.
	downloads := 0
	for _, instruction := range strings.Split(strings.ReplaceAll(builderStage, "\\\n", " "), "\n") {
		if !strings.Contains(instruction, "go mod download") {
			continue
		}
		downloads++
		if !strings.HasPrefix(instruction, "RUN "+netrcMount+" ") {
			t.Errorf("dependency download must mount the netrc secret: %q", instruction)
		}
	}
	if downloads == 0 {
		t.Error("rendered recipe downloads no dependencies")
	}

	// The credential is a mount, never image content.
	for _, line := range strings.Split(rendered, "\n") {
		if !strings.Contains(line, "netrc") {
			continue
		}
		if !strings.HasPrefix(line, "RUN ") && !strings.HasPrefix(line, "#") {
			t.Errorf("netrc may only appear on a RUN mount or a comment: %q", line)
		}
	}
	if strings.Contains(runtimeStage, "netrc") {
		t.Errorf("runtime stage names the credential:\n%s", runtimeStage)
	}
	if strings.Contains(runtimeStage, "GOPRIVATE") {
		t.Errorf("runtime stage names GOPRIVATE:\n%s", runtimeStage)
	}
}
