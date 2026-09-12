package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/services"
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

// TestBuildRejectsMissingOutputDirectory proves the agent never executes an
// image build. A BuildRequest without an output directory is refused outright
// instead of falling back to an in-agent docker build, and the refusal lands
// before any preparation, so nothing is rendered into the service tree.
func TestBuildRejectsMissingOutputDirectory(t *testing.T) {
	b, _ := loadedBuilder(t)
	client := grpcBuilderClient(t, b)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	response, err := client.Build(ctx, &builderv0.BuildRequest{
		BuildContext: &builderv0.BuildContext{
			Kind: &builderv0.BuildContext_DockerBuildContext{
				DockerBuildContext: &builderv0.DockerBuildContext{DockerRepository: "registry.example.com"},
			},
		},
	})
	if err == nil {
		t.Fatalf("expected a refusal, got %v", response)
	}
	if !strings.Contains(err.Error(), "output_directory") {
		t.Errorf("refusal must name the missing field, got %v", err)
	}
	// The in-process path used to render builder/Dockerfile into the service
	// directory before building, so an untouched tree is the evidence that the
	// request was refused ahead of any preparation.
	if _, statErr := os.Stat(filepath.Join(b.Location, "builder")); !os.IsNotExist(statErr) {
		t.Errorf("refused build prepared the service tree: %v", statErr)
	}
}
