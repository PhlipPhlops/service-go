package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
)

// replaceFixture lays out a repository holding two Go modules: a service whose
// module replaces a sibling library by filesystem path, exactly as two codefly
// services sharing one module do. It returns the repository root and the
// service directory the Builder loads.
func replaceFixture(t *testing.T, replacement string) (repository, service string) {
	t.Helper()
	repository = t.TempDir()
	if err := os.MkdirAll(filepath.Join(repository, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	service = filepath.Join(repository, "services", "worker")
	write(t, filepath.Join(service, "service.codefly.yaml"), serviceConfig)
	write(t, filepath.Join(service, "code", "go.mod"),
		"module myservice\n\ngo 1.26\n\nrequire mylib v0.0.0\n\nreplace mylib => "+replacement+"\n")
	write(t, filepath.Join(service, "code", "main.go"),
		"package main\n\nimport \"mylib\"\n\nfunc main() { _ = mylib.Name }\n")
	return repository, service
}

// writeLibrary writes the replaced module at directory.
func writeLibrary(t *testing.T, directory string) {
	t.Helper()
	write(t, filepath.Join(directory, "go.mod"), "module mylib\n\ngo 1.26\n")
	write(t, filepath.Join(directory, "lib.go"), "package mylib\n\nconst Name = \"mylib\"\n")
}

// relocate copies the recipe's code context to a directory where the
// replacement's original relative path does not exist, which is what the
// builder stage sees: the Dockerfile copies `code/` and nothing beside it.
func relocate(t *testing.T, codeContext string) string {
	t.Helper()
	root := t.TempDir()
	destination := filepath.Join(root, "app", "code")
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("cp", "-R", codeContext, destination).CombinedOutput(); err != nil {
		t.Fatalf("relocate context: %v: %s", err, output)
	}
	return destination
}

// runGo runs a go command the way the builder stage does, with no module
// proxy: everything the build needs must already be in the context.
func runGo(directory string, args ...string) (string, error) {
	command := exec.Command("go", args...)
	command.Dir = directory
	command.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=", "GOPROXY=off")
	output, err := command.CombinedOutput()
	return string(output), err
}

// TestBuildRecipeCarriesLocalReplacement is the defect this recipe had: a
// module that replaces a sibling by filesystem path emitted a context without
// that sibling, and the builder stage failed on `go mod download` with
//
//	go: mylib@v0.0.0 (replaced by ../../lib): reading /lib/go.mod: no such file or directory
//
// The test proves both halves against the real toolchain: the context as the
// old recipe emitted it fails that way, and the context this recipe emits
// downloads and builds where the sibling path does not exist.
func TestBuildRecipeCarriesLocalReplacement(t *testing.T) {
	repository, service := replaceFixture(t, "../../lib")
	writeLibrary(t, filepath.Join(repository, "services", "lib"))

	b, ctx := loadBuilderAt(t, service)
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
	if resp.GetState().GetState() != builderv0.BuildStatus_SUCCESS {
		t.Fatalf("build state = %v, message = %q", resp.GetState().GetState(), resp.GetState().GetMessage())
	}

	codeContext := filepath.Join(out, "code")
	for _, rel := range []string{"go.mod", "main.go", "_replace/services/lib/go.mod", "_replace/services/lib/lib.go"} {
		if _, err := os.Stat(filepath.Join(codeContext, rel)); err != nil {
			t.Errorf("expected %s in the code context: %v", rel, err)
		}
	}
	rewritten, err := os.ReadFile(filepath.Join(codeContext, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rewritten), "replace mylib => ./_replace/services/lib") {
		t.Errorf("go.mod does not point at the carried replacement:\n%s", rewritten)
	}

	// The old recipe, reconstructed: the same context without the carried
	// replacement and with the directive as the module spells it.
	old := relocate(t, codeContext)
	if err := os.RemoveAll(filepath.Join(old, "_replace")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(old, "go.mod"),
		"module myservice\n\ngo 1.26\n\nrequire mylib v0.0.0\n\nreplace mylib => ../../lib\n")
	output, err := runGo(old, "mod", "download")
	if err == nil {
		t.Fatalf("the old context must not resolve its replacement, got:\n%s", output)
	}
	if !strings.Contains(output, "(replaced by ../../lib)") || !strings.Contains(output, "no such file or directory") {
		t.Fatalf("expected the reading-go.mod failure this fix is for, got:\n%s", output)
	}

	// The emitted context, where the replacement's original path does not exist.
	relocated := relocate(t, codeContext)
	if _, err := os.Stat(filepath.Join(relocated, "..", "..", "lib")); !os.IsNotExist(err) {
		t.Fatalf("relocated context still reaches the sibling: %v", err)
	}
	if output, err := runGo(relocated, "mod", "download"); err != nil {
		t.Fatalf("go mod download in the emitted context: %v\n%s", err, output)
	}
	if output, err := runGo(relocated, "build", "-o", filepath.Join(t.TempDir(), "app"), "."); err != nil {
		t.Fatalf("go build in the emitted context: %v\n%s", err, output)
	}
}

// TestBuildRecipeRefusesReplacementOutsideRepository proves a replacement that
// leaves the repository is reported, naming the directive. Copying it would put
// one machine's filesystem into an image; skipping it silently would build the
// image against a different dependency than the developer builds against.
func TestBuildRecipeRefusesReplacementOutsideRepository(t *testing.T) {
	outside := t.TempDir()
	writeLibrary(t, filepath.Join(outside, "lib"))
	_, service := replaceFixture(t, filepath.Join(outside, "lib"))

	b, ctx := loadBuilderAt(t, service)
	resp, err := b.Build(ctx, &builderv0.BuildRequest{
		OutputDirectory: filepath.Join(t.TempDir(), "recipe"),
		BuildContext: &builderv0.BuildContext{
			Kind: &builderv0.BuildContext_DockerBuildContext{
				DockerBuildContext: &builderv0.DockerBuildContext{DockerRepository: "registry.example.com"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if resp.GetState().GetState() == builderv0.BuildStatus_SUCCESS {
		t.Fatal("a replacement outside the repository must not render a recipe")
	}
	message := resp.GetState().GetMessage()
	if !strings.Contains(message, "replace mylib =>") || !strings.Contains(message, "outside the repository") {
		t.Errorf("error must name the directive and the boundary, got %q", message)
	}
}

// TestBuildRecipeWithoutLocalReplaceIsUnchanged holds the no-op case: a module
// with no filesystem replacement renders exactly the context it rendered
// before, down to the bytes of its go.mod.
func TestBuildRecipeWithoutLocalReplaceIsUnchanged(t *testing.T) {
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
	if resp.GetState().GetState() != builderv0.BuildStatus_SUCCESS {
		t.Fatalf("build state = %v, message = %q", resp.GetState().GetState(), resp.GetState().GetMessage())
	}
	source, err := os.ReadFile(filepath.Join(b.SourceLocation, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	emitted, err := os.ReadFile(filepath.Join(out, "code", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if string(source) != string(emitted) {
		t.Errorf("go.mod was rewritten with no replacement to carry:\nwant %q\ngot  %q", source, emitted)
	}
	if _, err := os.Stat(filepath.Join(out, "code", "_replace")); !os.IsNotExist(err) {
		t.Errorf("a module with no replacement must not get a carry directory: %v", err)
	}
}
