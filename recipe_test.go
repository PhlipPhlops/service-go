package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/agents/services/sbom"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"

	gobuilder "github.com/codefly-dev/service-go/pkg/builder"
	goservice "github.com/codefly-dev/service-go/pkg/service"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

const serviceConfig = `kind: service
name: myservice
module: mymodule
agent:
  kind: runtime::service
  name: go
  version: 0.0.1
  publisher: codefly.ai
`

// loadedBuilder writes a minimal Go service into a fresh workspace and returns
// a Builder that has been through the Load RPC, ready to drive Build.
func loadedBuilder(t *testing.T) (*gobuilder.Builder, context.Context) {
	t.Helper()
	ws := t.TempDir()
	write(t, filepath.Join(ws, "service.codefly.yaml"), serviceConfig)
	write(t, filepath.Join(ws, "code", "go.mod"), "module myservice\n\ngo 1.26\n")
	write(t, filepath.Join(ws, "code", "main.go"), "package main\n\nfunc main() {}\n")
	return loadBuilderAt(t, ws)
}

// loadBuilderAt loads a Builder against a workspace a caller has already laid
// out, so a test can pick the service's source layout.
func loadBuilderAt(t *testing.T, ws string) (*gobuilder.Builder, context.Context) {
	t.Helper()
	svc := goservice.New(agent)
	b := gobuilder.New(svc, gobuilder.BuildConfig{
		FactoryFS:    factoryFS,
		BuilderFS:    builderFS,
		DeploymentFS: deploymentFS,
		Requirements: requirements,
	})

	ctx := context.Background()
	identity := &basev0.ServiceIdentity{
		Name: "myservice", Module: "mymodule", Version: "0.0.1",
		WorkspacePath: ws, RelativeToWorkspace: ".",
	}
	if _, err := b.Load(ctx, &builderv0.LoadRequest{Identity: identity}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	return b, ctx
}

// TestBuildEmitsRecipePlan drives the Build RPC with an output directory and
// asserts the agent renders a self-contained recipe there and returns a
// DockerBuildPlan that re-verifies against the on-disk tree.
func TestBuildEmitsRecipePlan(t *testing.T) {
	b, ctx := loadedBuilder(t)
	client := grpcBuilderClient(t, b)
	capabilities, err := client.BuildCapabilities(ctx, &builderv0.BuildCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !capabilities.GetBuildxSelection() {
		t.Fatal("recipe producer must support caller-owned Buildx selection")
	}
	out := filepath.Join(t.TempDir(), "recipe")

	resp, err := client.Build(ctx, &builderv0.BuildRequest{
		OutputDirectory: out,
		BuildContext: &builderv0.BuildContext{
			Kind: &builderv0.BuildContext_DockerBuildContext{
				DockerBuildContext: &builderv0.DockerBuildContext{DockerRepository: "registry.example.com", BuildxBuilder: "recipe-only-no-such-builder", Cache: &builderv0.BuildCacheOptions{Backend: "registry", Scope: "test/service", Imports: []string{"registry.example.com/cache"}}},
			},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if resp.GetState().GetState() != builderv0.BuildStatus_SUCCESS {
		t.Fatalf("build state = %v, message=%q", resp.GetState().GetState(), resp.GetState().GetMessage())
	}

	plan := resp.GetResult().GetDockerBuildPlan()
	if plan == nil {
		t.Fatalf("expected a DockerBuildPlan, got %T", resp.GetResult().GetKind())
	}
	if err := services.VerifyDockerBuildPlan(out, plan); err != nil {
		t.Fatalf("plan does not verify against its tree: %v", err)
	}

	if len(plan.GetRecipes()) != 1 {
		t.Fatalf("expected 1 recipe, got %d", len(plan.GetRecipes()))
	}
	recipe := plan.GetRecipes()[0]
	if recipe.GetDockerfile() != "builder/Dockerfile" {
		t.Errorf("dockerfile = %q", recipe.GetDockerfile())
	}
	if recipe.GetContext() != "." {
		t.Errorf("context = %q", recipe.GetContext())
	}
	if recipe.GetContextRoot() != builderv0.RecipeContextRoot_RECIPE_CONTEXT_ROOT_OUTPUT {
		t.Fatalf("context root = %v; rewritten sources must build from output_directory", recipe.GetContextRoot())
	}
	if plan.GetContractVersion() != services.DockerBuildRecipeContextContractVersion {
		t.Fatalf("context-root recipe requires v4, got %q", plan.GetContractVersion())
	}
	if got := recipe.GetPlatforms(); len(got) != 2 || got[0] != "linux/amd64" || got[1] != "linux/arm64" {
		t.Errorf("platforms = %v", got)
	}
	if !strings.Contains(recipe.GetImage(), "myservice") {
		t.Errorf("image = %q, want it to reference the service", recipe.GetImage())
	}

	for _, rel := range []string{"builder/Dockerfile", "builder/dockerignore", "code/go.mod", "code/main.go"} {
		if _, err := os.Stat(filepath.Join(out, rel)); err != nil {
			t.Errorf("expected %s in recipe tree: %v", rel, err)
		}
	}

	dockerfile, err := os.ReadFile(filepath.Join(out, "builder", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	// Dependabot must see the exact versioned images that the recipe uses.
	// Template variables in FROM lines leave the updater with no dependencies.
	source, err := builderFS.ReadFile("templates/builder/Dockerfile.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	from := regexp.MustCompile(`(?m)^FROM (?:--platform=\S+ )?(\S+)`)
	images := from.FindAllStringSubmatch(string(source), -1)
	if len(images) != 2 {
		t.Fatalf("expected two base images, got %v", images)
	}
	literal := regexp.MustCompile(`^(golang|alpine):[0-9][a-zA-Z0-9_.-]*$`)
	for _, image := range images {
		if !literal.MatchString(image[1]) {
			t.Errorf("base image %q is not a literal version Dependabot can update", image[1])
		}
		if !strings.Contains(string(dockerfile), image[0]+"\n") && !strings.Contains(string(dockerfile), image[0]+" AS ") {
			t.Errorf("rendered Dockerfile does not preserve base image %q", image[1])
		}
	}
	// The recipe declares two architectures, so the Dockerfile must build for
	// the target platform rather than a hardcoded GOARCH.
	if !strings.Contains(string(dockerfile), "TARGETARCH") {
		t.Errorf("rendered Dockerfile is not multi-arch aware:\n%s", dockerfile)
	}
}

// TestBuildRecipeSkipsSymlinks proves a symlink in the source tree is left out
// of the recipe context — the recipe inventory rejects symlinks, so copying one
// through would break plan generation.
func TestBuildRecipeSkipsSymlinks(t *testing.T) {
	b, ctx := loadedBuilder(t)
	src := b.SourceLocation
	if err := os.Symlink("go.mod", filepath.Join(src, "link.mod")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
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
	if _, err := os.Lstat(filepath.Join(out, "code", "link.mod")); !os.IsNotExist(err) {
		t.Errorf("symlink leaked into recipe tree: %v", err)
	}
}

// TestBuildRecipeAtSourceRootExcludesItsOwnOutput drives the layout the CLI
// hands a service declaring source-dir ".": the sources are rooted at the
// service directory, which is also where the recipe is written. The build must
// emit each source file once instead of copying its own growing output back
// into the context until the path outruns the filesystem's name limit.
func TestBuildRecipeAtSourceRootExcludesItsOwnOutput(t *testing.T) {
	ws := t.TempDir()
	write(t, filepath.Join(ws, "service.codefly.yaml"), serviceConfig+"spec:\n  source-dir: \".\"\n")
	write(t, filepath.Join(ws, "go.mod"), "module myservice\n\ngo 1.26\n")
	write(t, filepath.Join(ws, "main.go"), "package main\n\nfunc main() {}\n")

	b, ctx := loadBuilderAt(t, ws)
	if b.SourceLocation != ws {
		t.Fatalf("source location = %q, want the service root %q", b.SourceLocation, ws)
	}
	out := filepath.Join(ws, "builder")

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
	plan := resp.GetResult().GetDockerBuildPlan()
	if plan == nil {
		t.Fatalf("expected a plan, got state %v message %q",
			resp.GetState().GetState(), resp.GetState().GetMessage())
	}
	if err := services.VerifyDockerBuildPlan(out, plan); err != nil {
		t.Fatalf("plan does not verify against its tree: %v", err)
	}

	var files []string
	if err := filepath.WalkDir(filepath.Join(out, "code"), func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(filepath.Join(out, "code"), p)
		if err != nil {
			return err
		}
		files = append(files, rel)
		return nil
	}); err != nil {
		t.Fatalf("walk recipe context: %v", err)
	}
	slices.Sort(files)
	want := []string{"go.mod", "main.go", "service.codefly.yaml"}
	if !slices.Equal(files, want) {
		t.Errorf("recipe context = %v, want each source once: %v", files, want)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func grpcBuilderClient(t *testing.T, b *gobuilder.Builder, options ...grpc.ServerOption) *services.BuilderAgent {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(options...)
	builderv0.RegisterBuilderServer(server, b)
	go func() {
		if err := server.Serve(listener); err != nil {
			t.Errorf("serve builder: %v", err)
		}
	}()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	})
	return services.NewBuilderAgentClient(conn)
}

func TestBuildCapabilitiesBeforeLoad(t *testing.T) {
	b := gobuilder.New(goservice.New(agent), gobuilder.BuildConfig{})
	client := grpcBuilderClient(t, b)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	response, err := client.BuildCapabilities(ctx, &builderv0.BuildCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !response.GetBuildxSelection() {
		t.Fatal("Buildx selection must be supported before Load")
	}
}

func TestBuildxRecipeOverGRPC(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprintf("cache=%t", cached), func(t *testing.T) {
			b, _ := loadedBuilder(t)
			t.Setenv("PATH", t.TempDir())
			for _, executable := range []string{"docker", "buildx"} {
				if _, err := exec.LookPath(executable); err == nil {
					t.Fatalf("%s unexpectedly on PATH", executable)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			out := filepath.Join(t.TempDir(), "recipe")
			docker := &builderv0.DockerBuildContext{DockerRepository: "registry.example.com", BuildxBuilder: "caller-owned-builder"}
			if cached {
				docker.Cache = &builderv0.BuildCacheOptions{Backend: "registry", Scope: "test/myservice", Imports: []string{"registry.example.com/cache"}, Exports: []string{"registry.example.com/cache"}}
			}
			request := &builderv0.BuildRequest{OutputDirectory: out, BuildContext: &builderv0.BuildContext{Kind: &builderv0.BuildContext_DockerBuildContext{DockerBuildContext: docker}}}
			calls := make(chan string, 2)
			client := grpcBuilderClient(t, b, grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
				calls <- info.FullMethod
				if info.FullMethod == builderv0.Builder_BuildCapabilities_FullMethodName {
					if _, err := os.Stat(out); !os.IsNotExist(err) {
						t.Errorf("capability negotiation prepared recipe output: %v", err)
					}
				}
				if build, ok := req.(*builderv0.BuildRequest); ok && !proto.Equal(build, request) {
					t.Errorf("Build request changed in transit: %v", build)
				}
				return handler(ctx, req)
			}))
			response, err := client.Build(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			if response.GetState().GetState() != builderv0.BuildStatus_SUCCESS {
				t.Fatalf("Build failed: %v", response.GetState())
			}
			for _, want := range []string{builderv0.Builder_BuildCapabilities_FullMethodName, builderv0.Builder_Build_FullMethodName} {
				select {
				case got := <-calls:
					if got != want {
						t.Errorf("RPC = %q, want %q", got, want)
					}
				default:
					t.Fatalf("missing RPC %s", want)
				}
			}
			plan := response.GetResult().GetDockerBuildPlan()
			if plan == nil {
				t.Fatal("missing recipe plan")
			}
			if err := services.VerifyDockerBuildPlan(out, plan); err != nil {
				t.Fatal(err)
			}
			if response.GetBuildxBuilder() != "" || response.GetCacheContractVersion() != "" {
				t.Fatal("recipe must leave execution acknowledgement to the caller")
			}
		})
	}
}

func TestBuildRejectsMissingOrRelativeDestinationOverGRPC(t *testing.T) {
	for _, selection := range []string{"", "explicit", "cache-selected"} {
		for _, destination := range []string{"", "relative"} {
			t.Run(fmt.Sprintf("builder=%s/destination=%s", selection, destination), func(t *testing.T) {
				b, _ := loadedBuilder(t)
				t.Setenv("PATH", t.TempDir())
				original := filepath.Join(b.Location, "builder", "Dockerfile")
				write(t, original, "original recipe")
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				docker := &builderv0.DockerBuildContext{DockerRepository: "registry.example.com", BuildxBuilder: selection}
				if selection == "cache-selected" {
					docker.Cache = &builderv0.BuildCacheOptions{Backend: "registry", Scope: "test/myservice", Exports: []string{"registry.example.com/cache"}}
				}
				request := &builderv0.BuildRequest{OutputDirectory: destination, BuildContext: &builderv0.BuildContext{Kind: &builderv0.BuildContext_DockerBuildContext{DockerBuildContext: docker}}}
				response, err := grpcBuilderClient(t, b).Build(ctx, request)
				if err != nil {
					if !strings.Contains(err.Error(), "output_directory") {
						t.Fatalf("unexpected error: %v", err)
					}
				} else if response.GetState().GetState() != builderv0.BuildStatus_ERROR || !strings.Contains(response.GetState().GetMessage(), "output_directory") {
					t.Fatalf("expected destination rejection, got %v", response)
				}
				contents, err := os.ReadFile(original)
				if err != nil {
					t.Fatal(err)
				}
				if string(contents) != "original recipe" {
					t.Fatal("rejected request overwrote existing recipe")
				}
				entries, err := os.ReadDir(filepath.Dir(original))
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 1 {
					t.Fatal("rejected request prepared builder files")
				}
			})
		}
	}
}

// emittedPlan drives Build the way the CLI does and returns the recipe plan the
// agent hands back — the only description of its images the agent owns.
func emittedPlan(t *testing.T) (*gobuilder.Builder, context.Context, *builderv0.DockerBuildPlan) {
	t.Helper()
	b, ctx := loadedBuilder(t)
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
	plan := resp.GetResult().GetDockerBuildPlan()
	if plan == nil {
		t.Fatalf("expected a plan, got state %v message %q", resp.GetState().GetState(), resp.GetState().GetMessage())
	}
	return b, ctx, plan
}

// resolvedFromPlan stands in for the digests a caller reads back from the buildx
// run it drives; this agent only emits the recipe, so a test never has real ones.
func resolvedFromPlan(t *testing.T, plan *builderv0.DockerBuildPlan) []sbom.ResolvedImage {
	t.Helper()
	var resolved []sbom.ResolvedImage
	for _, recipe := range plan.GetRecipes() {
		for i, platform := range recipe.GetPlatforms() {
			resolved = append(resolved, sbom.ResolvedImage{
				Recipe:   recipe.GetName(),
				Platform: platform,
				Digest:   fmt.Sprintf("sha256:%064d", i),
				Source:   sbom.SourceRegistry,
			})
		}
	}
	return resolved
}

// TestImageSBOMRequiresCallerSuppliedSubjects drives the SBOM RPC under image
// scope with no subjects. This agent emits a recipe and never runs buildx, so it
// holds no digest to inventory: the honest answer is a precondition failure, not
// an unsupported operation and not a no-image claim, because the service really
// does ship an image.
func TestImageSBOMRequiresCallerSuppliedSubjects(t *testing.T) {
	b, _ := loadedBuilder(t)
	client := grpcBuilderClient(t, b)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	resp, err := client.SBOM(ctx, &builderv0.SBOMRequest{Scope: builderv0.SBOMScope_SBOM_SCOPE_IMAGE})
	if err != nil {
		t.Fatalf("SBOM: %v", err)
	}
	if state := resp.GetState().GetState(); state != builderv0.SBOMStatus_ERROR {
		t.Fatalf("state = %s, want ERROR: %s", state, resp.GetState().GetMessage())
	}
	// An image failure reported without image scope reads as a source inventory,
	// which hides the cause behind a scope complaint.
	if scope := resp.GetScope(); scope != builderv0.SBOMScope_SBOM_SCOPE_IMAGE {
		t.Errorf("scope = %s, want image", scope)
	}
	if code := resp.GetState().GetFailure().GetCode(); code != basev0.FailureCode_FAILURE_CODE_PRECONDITION_FAILED {
		t.Errorf("failure code = %s, want precondition failed", code)
	}
	if reason := resp.GetNoImageReason(); reason != builderv0.NoImageReason_NO_IMAGE_REASON_UNSPECIFIED {
		t.Errorf("no-image reason = %s, but this service ships an image", reason)
	}
	if images := resp.GetImages(); len(images) != 0 {
		t.Errorf("failed image scope carries %d inventories", len(images))
	}
}

// TestImageSBOMCoverageDerivesFromTheEmittedRecipe checks the agent's own plan
// through the shared conformance helper: every shipped platform is its own
// subject, and the subjects-required failure never passes as coverage.
func TestImageSBOMCoverageDerivesFromTheEmittedRecipe(t *testing.T) {
	b, ctx, plan := emittedPlan(t)

	expected, err := sbom.ExpectedFromBuildPlan("myservice", plan, resolvedFromPlan(t, plan))
	if err != nil {
		t.Fatalf("ExpectedFromBuildPlan: %v", err)
	}
	if len(expected) != 2 {
		t.Fatalf("expected one subject per shipped platform, got %d", len(expected))
	}
	for i, platform := range []string{"linux/amd64", "linux/arm64"} {
		if got := expected[i].GetPlatform(); got != platform {
			t.Errorf("subject %d platform = %q, want %q", i, got, platform)
		}
		if got := expected[i].GetRole(); got != "app" {
			t.Errorf("subject %d role = %q, want the recipe name", i, got)
		}
		if got := expected[i].GetService(); got != "myservice" {
			t.Errorf("subject %d service = %q", i, got)
		}
	}

	unsatisfied, err := b.SBOM(ctx, &builderv0.SBOMRequest{Scope: builderv0.SBOMScope_SBOM_SCOPE_IMAGE})
	if err != nil {
		t.Fatalf("SBOM: %v", err)
	}
	validation := sbom.ValidateCoverage("myservice", expected, unsatisfied)
	if validation == nil {
		t.Fatal("a precondition failure passed coverage validation")
	}
	if !strings.Contains(validation.Error(), "does not build its own images") {
		t.Errorf("coverage failure does not name its cause: %v", validation)
	}
}

// TestSourceInventoryIsNotImageCoverage proves the unchanged source path still
// answers an unset scope, and that its module graph cannot stand in for evidence
// about the shipped image.
func TestSourceInventoryIsNotImageCoverage(t *testing.T) {
	b, ctx, plan := emittedPlan(t)

	resp, err := b.SBOM(ctx, &builderv0.SBOMRequest{})
	if err != nil {
		t.Fatalf("SBOM: %v", err)
	}
	if state := resp.GetState().GetState(); state != builderv0.SBOMStatus_COMPLETE {
		t.Fatalf("source SBOM state = %s: %s", state, resp.GetState().GetMessage())
	}
	if scope := resp.GetScope(); scope != builderv0.SBOMScope_SBOM_SCOPE_SOURCE {
		t.Errorf("unset scope = %s, want source", scope)
	}
	expected, err := sbom.ExpectedFromBuildPlan("myservice", plan, resolvedFromPlan(t, plan))
	if err != nil {
		t.Fatalf("ExpectedFromBuildPlan: %v", err)
	}
	if sbom.ValidateCoverage("myservice", expected, resp) == nil {
		t.Fatal("a module inventory passed as image coverage")
	}
}

// TestImageSBOMRefusesUnpinnedSubjects covers the substitution this agent cannot
// otherwise detect. A subject naming only a tag is inventoried from whatever the
// registry serves when the scan runs — for a build loaded locally and never
// pushed, that is a different image, or a previously pushed one. Nothing
// downstream catches it: a tag-derived subject carries no digest, so neither the
// shared helper's mismatch check nor ValidateCoverage has anything to compare,
// and a stale inventory would be reported as coverage for the built image.
func TestImageSBOMRefusesUnpinnedSubjects(t *testing.T) {
	b, ctx := loadedBuilder(t)
	// No scanner toolchain: a refusal must come from the missing digest, not
	// from a scan that was attempted and happened to fail.
	t.Setenv("PATH", t.TempDir())

	reference := "registry.example.com/codefly/myservice:1.0.0"
	resp, err := b.SBOM(ctx, &builderv0.SBOMRequest{
		Scope: builderv0.SBOMScope_SBOM_SCOPE_IMAGE,
		Subjects: []*builderv0.ImageSubject{{
			Reference: reference,
			Platform:  "linux/amd64",
			Role:      "app",
			Service:   "myservice",
		}},
	})
	if err != nil {
		t.Fatalf("SBOM: %v", err)
	}
	if state := resp.GetState().GetState(); state != builderv0.SBOMStatus_ERROR {
		t.Fatalf("unpinned subject state = %s, want ERROR", state)
	}
	if scope := resp.GetScope(); scope != builderv0.SBOMScope_SBOM_SCOPE_IMAGE {
		t.Errorf("scope = %s, want image", scope)
	}
	if images := resp.GetImages(); len(images) != 0 {
		t.Errorf("refused request carries %d inventories", len(images))
	}
	message := resp.GetState().GetMessage()
	if !strings.Contains(message, "immutable digest") || !strings.Contains(message, reference) {
		t.Errorf("refusal does not name the unpinned subject and why: %q", message)
	}
}

// TestImageSBOMAcceptsSubjectsPinnedByReference proves the pin check reads the
// reference as well as the digest field, so a caller that pins the reference
// itself reaches the scanner rather than being refused.
func TestImageSBOMAcceptsSubjectsPinnedByReference(t *testing.T) {
	b, ctx := loadedBuilder(t)
	t.Setenv("PATH", t.TempDir())

	resp, err := b.SBOM(ctx, &builderv0.SBOMRequest{
		Scope: builderv0.SBOMScope_SBOM_SCOPE_IMAGE,
		Subjects: []*builderv0.ImageSubject{{
			Reference: "registry.example.com/codefly/myservice@sha256:" + strings.Repeat("a", 64),
			Platform:  "linux/amd64",
			Role:      "app",
			Service:   "myservice",
		}},
	})
	if err != nil {
		t.Fatalf("SBOM: %v", err)
	}
	if message := resp.GetState().GetMessage(); strings.Contains(message, "immutable digest") {
		t.Errorf("reference-pinned subject was refused as unpinned: %q", message)
	}
}

// TestImageSBOMScanFailuresStayImageScoped proves a failed scan propagates as an
// image-scope error rather than partial or fabricated coverage. The scanner
// toolchain is removed from PATH so the failure is the same one in every
// environment, with no network lookup to depend on.
func TestImageSBOMScanFailuresStayImageScoped(t *testing.T) {
	b, ctx := loadedBuilder(t)
	t.Setenv("PATH", t.TempDir())

	digest := "sha256:" + strings.Repeat("a", 64)
	resp, err := b.SBOM(ctx, &builderv0.SBOMRequest{
		Scope: builderv0.SBOMScope_SBOM_SCOPE_IMAGE,
		Subjects: []*builderv0.ImageSubject{{
			Reference: "registry.example.com/codefly/myservice@" + digest,
			Digest:    digest,
			Platform:  "linux/amd64",
			Role:      "app",
			Service:   "myservice",
		}},
	})
	if err != nil {
		t.Fatalf("SBOM: %v", err)
	}
	if state := resp.GetState().GetState(); state != builderv0.SBOMStatus_ERROR {
		t.Fatalf("unscannable image state = %s, want ERROR", state)
	}
	if scope := resp.GetScope(); scope != builderv0.SBOMScope_SBOM_SCOPE_IMAGE {
		t.Errorf("scope = %s, want image", scope)
	}
	if images := resp.GetImages(); len(images) != 0 {
		t.Errorf("failed scan carries %d inventories", len(images))
	}
	if resp.GetState().GetMessage() == "" {
		t.Error("failed scan reports no diagnostics")
	}
}
