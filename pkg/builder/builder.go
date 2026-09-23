// Package builder implements the generic Go Builder gRPC service.
// Specializations embed *Builder to inherit Load / Init / Sync / Create /
// Build / Deploy. Because //go:embed cannot reach outside the .go file's
// directory, the caller (binary main.go) provides the three template FS
// trees (factory, builder, deployment) at construction time.
package builder

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/codefly-dev/core/agents/communicate"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/agents/services/audit"
	"github.com/codefly-dev/core/agents/services/sbom"
	"github.com/codefly-dev/core/builders"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/companion"
	golanghelpers "github.com/codefly-dev/core/runners/golang"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"

	goservice "github.com/codefly-dev/service-go/pkg/service"
)

// Setting names for communicate prompts.
const (
	HotReload                 = golanghelpers.SettingHotReload
	DebugSymbols              = golanghelpers.SettingDebugSymbols
	RaceConditionDetectionRun = golanghelpers.SettingRaceConditionDetectionRun
)

// BuildConfig provides the embedded template trees plus the file
// requirements descriptor. Specializations construct this struct with
// their own //go:embed directives in their main.go.
type BuildConfig struct {
	FactoryFS     embed.FS // templates/factory — service scaffolding
	BuilderFS     embed.FS // templates/builder — Dockerfile generation
	DeploymentFS  embed.FS // templates/deployment — k8s manifests
	Requirements  *builders.Dependencies
	GoVersion     string
	AlpineVersion string
}

// Builder is the generic Go builder server. Embedded by specializations.
type Builder struct {
	services.BuilderServer
	*goservice.Service

	cfg           BuildConfig
	cacheLocation string
	answers       map[string]*agentv0.Answer
}

// New builds a generic Go Builder. Caller provides template FS + deps.
func New(svc *goservice.Service, cfg BuildConfig) *Builder {
	return &Builder{Service: svc, cfg: cfg}
}

func (s *Builder) Load(ctx context.Context, req *builderv0.LoadRequest) (*builderv0.LoadResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	if err := s.Builder.Load(ctx, req.Identity, s.Settings); err != nil {
		return nil, err
	}
	if err := s.Settings.Validate(); err != nil {
		return s.Builder.LoadErrorf(err, "invalid Go settings")
	}

	s.Service.SetSourceLocation(s.Local("%s", s.Settings.GoSourceDir()))
	s.cacheLocation = s.Local(".cache")
	if s.cfg.Requirements != nil {
		s.cfg.Requirements.Localize(s.Location)
	}

	if req.CreationMode != nil {
		s.Builder.CreationMode = req.CreationMode
		gs, err := templates.ApplyTemplateFrom(ctx, shared.Embed(s.cfg.FactoryFS), "templates/factory/GETTING_STARTED.md", s.Information)
		if err != nil {
			return s.Builder.LoadError(err)
		}
		s.Builder.GettingStarted = gs
		return s.Builder.LoadResponse()
	}

	s.Endpoints, _ = s.Base.Service.LoadEndpoints(ctx)
	return s.Builder.LoadResponse()
}

func (s *Builder) Init(ctx context.Context, req *builderv0.InitRequest) (*builderv0.InitResponse, error) {
	defer s.Wool.Catch()
	s.Builder.LogInitRequest(req)
	ctx = s.Wool.Inject(ctx)
	s.DependencyEndpoints = req.DependenciesEndpoints
	return s.Builder.InitResponse()
}

func (s *Builder) Update(ctx context.Context, _ *builderv0.UpdateRequest) (*builderv0.UpdateResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	return &builderv0.UpdateResponse{}, nil
}

// Sync is a no-op on the generic layer — go has no protos to regenerate.
// Specializations (go-grpc) override.
func (s *Builder) Sync(ctx context.Context, _ *builderv0.SyncRequest) (*builderv0.SyncResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	return s.Builder.SyncResponse()
}

func (s *Builder) BuildCapabilities(context.Context, *builderv0.BuildCapabilitiesRequest) (*builderv0.BuildCapabilitiesResponse, error) {
	return &builderv0.BuildCapabilitiesResponse{BuildxSelection: true}, nil
}

// Build emits a recipe for the CLI to build and publish.
func (s *Builder) Build(ctx context.Context, req *builderv0.BuildRequest) (*builderv0.BuildResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	if req.GetOutputDirectory() == "" {
		return s.Builder.BuildError(fmt.Errorf("BuildRequest.output_directory is required for image recipes"))
	}
	return s.buildRecipe(ctx, req, req.GetOutputDirectory())
}

// buildRecipe renders the Dockerfile, dockerignore, and Go source into the
// caller-owned output directory and returns a single-image DockerBuildPlan. The
// image becomes a durable, reproducible recipe the CLI rebuilds with buildx, so
// a consumer never needs the agent toolchain.
func (s *Builder) buildRecipe(ctx context.Context, req *builderv0.BuildRequest, outputDir string) (*builderv0.BuildResponse, error) {
	dockerRequest, err := s.Builder.DockerBuildRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	image := s.Builder.DockerImage(dockerRequest)

	docker := golanghelpers.DockerTemplating{
		Components:    s.cfg.Requirements.All(),
		GoVersion:     s.cfg.GoVersion,
		AlpineVersion: s.cfg.AlpineVersion,
	}
	err = s.Builder.Templates(ctx, docker,
		services.WithBuilder(s.cfg.BuilderFS).WithDestination("%s", filepath.Join(outputDir, "builder")))
	if err != nil {
		return s.Builder.BuildError(err)
	}

	codeContext := filepath.Join(outputDir, "code")
	if err = copyGoContext(s.SourceLocation, codeContext, outputDir); err != nil {
		return s.Builder.BuildError(err)
	}
	// A module that replaces another by filesystem path resolves that path
	// against its own directory, which the context is: carry the replacement in
	// beside it so the builder stage resolves what the host resolves.
	if err = carryLocalReplacements(s.SourceLocation, codeContext, outputDir); err != nil {
		return s.Builder.BuildError(err)
	}

	recipe := &builderv0.DockerBuildRecipe{
		Name:         "app",
		Dockerfile:   "builder/Dockerfile",
		Context:      ".",
		Dockerignore: "builder/dockerignore",
		Image:        image.FullName(),
		Platforms:    []string{"linux/amd64", "linux/arm64"},
	}
	plan, err := services.BuildDockerBuildPlan(outputDir, []*builderv0.DockerBuildRecipe{recipe})
	if err != nil {
		return s.Builder.BuildError(err)
	}
	s.Builder.WithBuildPlan(plan)
	return s.Builder.BuildResponse()
}

// copyGoContext copies the Go source tree at src into dst, preserving file
// modes. Symlinks and other irregular files are skipped: the recipe inventory
// rejects symlinks outright, so a copied symlink would fail plan generation.
//
// output is the recipe directory the walk must never descend into. A service
// with source-dir "." has its sources rooted at the service directory, where
// the CLI also puts the recipe, so the walk would otherwise reach its own
// output and copy the growing recipe into itself until the path outruns the
// filesystem's name limit. Directory identity, not a path prefix, decides: the
// caller-selected recipe directory is skipped, no directory that merely shares
// its name is.
func copyGoContext(src, dst, output string) error {
	outputInfo, err := os.Stat(output)
	if err != nil {
		return err
	}
	return filepath.WalkDir(src, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if os.SameFile(info, outputInfo) {
				return filepath.SkipDir
			}
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		return copyFile(p, target)
	})
}

func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Audit is inherited by every Go specialization. Scanner ownership belongs in
// this language base so a new Go plugin gets a typed, fail-closed security RPC
// without reimplementing process invocation or result mapping.
func (s *Builder) Audit(ctx context.Context, req *builderv0.AuditRequest) (*builderv0.AuditResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	result, err := audit.Golang(ctx, s.Service.SourceLocation, req.GetIncludeOutdated())
	if err != nil {
		return s.Builder.AuditError(err)
	}
	return s.Builder.AuditResponse(req, result.Findings, result.Outdated, result.Tool, result.Language)
}

// SBOM is inherited by every Go specialization. Source scope inventories the
// exact GOWORK-disabled module graph selected by go.mod/go.sum. Image scope
// inventories a shipped image instead — its OS packages included, which a
// module graph says nothing about. Go images are built by whoever executes the
// emitted recipe, so the digests to scan arrive as caller-supplied subjects;
// asked for image scope with none, the shared helper reports the precondition
// failure rather than passing the module graph off as image coverage.
func (s *Builder) SBOM(ctx context.Context, req *builderv0.SBOMRequest) (*builderv0.SBOMResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	if req.GetScope() == builderv0.SBOMScope_SBOM_SCOPE_IMAGE {
		if subject := unpinnedImageSubject(req.GetSubjects()); subject != nil {
			return s.Builder.SBOMImageError(fmt.Errorf("image subject %q names no immutable digest: a tag resolves to whatever the registry serves when the scan runs, which need not be the image this build produced; supply the digest the build resolved", subject.GetReference()))
		}
		return s.Builder.SBOMImages(ctx, req.GetSubjects())
	}
	result, err := sbom.Golang(ctx, s.Service.SourceLocation)
	if err != nil {
		return s.Builder.SBOMError(err)
	}
	return s.Builder.SBOMResponse(result.Bom, result.Tool, result.Language, result.SHA256)
}

// unpinnedImageSubject returns the first subject that names no immutable image.
// Evidence is only ever compared against a digest the caller already holds, so a
// subject carrying nothing but a tag would be inventoried from whatever the
// registry serves at scan time and reported as coverage for the built image
// without anything detecting the substitution.
func unpinnedImageSubject(subjects []*builderv0.ImageSubject) *builderv0.ImageSubject {
	for _, subject := range subjects {
		if strings.HasPrefix(subject.GetDigest(), "sha256:") {
			continue
		}
		if _, reference, pinned := strings.Cut(subject.GetReference(), "@"); pinned && strings.HasPrefix(reference, "sha256:") {
			continue
		}
		return subject
	}
	return nil
}

// Package emits portable Go binaries and release-bound CycloneDX evidence.
// The operation is plugin-owned and local-first; agent build, CI, and future
// editor/Mind consumers all call this same typed RPC.
func (s *Builder) Package(ctx context.Context, req *builderv0.PackageRequest) (*builderv0.PackageResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	if req == nil {
		return s.Builder.PackageError(fmt.Errorf("package request is required"))
	}
	outputDirectory := filepath.Clean(req.GetOutputDirectory())
	if !filepath.IsAbs(outputDirectory) {
		return s.Builder.PackageError(fmt.Errorf("package output_directory must be absolute"))
	}
	artifactName := strings.TrimSpace(req.GetArtifactName())
	if artifactName == "" && s.Identity != nil {
		artifactName = s.Identity.Name
	}
	if artifactName == "" || artifactName == "." || filepath.Base(artifactName) != artifactName || strings.ContainsAny(artifactName, "/\\\x00") {
		return s.Builder.PackageError(fmt.Errorf("invalid package artifact_name %q", artifactName))
	}
	targets, err := normalizePackageTargets(req.GetTargets())
	if err != nil {
		return s.Builder.PackageError(err)
	}
	if err := os.MkdirAll(outputDirectory, 0o755); err != nil {
		return s.Builder.PackageError(fmt.Errorf("create package output: %w", err))
	}

	artifacts := make([]*builderv0.PackageArtifact, 0, len(targets)*2)
	for _, target := range targets {
		destinationDirectory := filepath.Join(outputDirectory, target.GetOs()+"-"+target.GetArchitecture())
		if err := os.MkdirAll(destinationDirectory, 0o755); err != nil {
			return s.Builder.PackageError(fmt.Errorf("create target output: %w", err))
		}
		destination := filepath.Join(destinationDirectory, artifactName)
		if err := packageGoBinary(ctx, s.Service.SourceLocation, destination, target); err != nil {
			return s.Builder.PackageError(err)
		}
		digest, err := packageFileSHA256(destination)
		if err != nil {
			return s.Builder.PackageError(err)
		}
		artifacts = append(artifacts, &builderv0.PackageArtifact{
			Kind:      builderv0.PackageArtifact_EXECUTABLE,
			Path:      destination,
			Target:    target,
			Sha256:    digest,
			MediaType: "application/vnd.codefly.executable",
		})
	}

	if req.GetIncludeSbom() {
		source, err := sbom.GolangWithOptions(ctx, s.Service.SourceLocation, sbom.GolangOptions{
			UseWorkspace: s.Settings.WithWorkspace,
		})
		if err != nil {
			return s.Builder.PackageError(fmt.Errorf("generate package SBOM: %w", err))
		}
		for _, executable := range append([]*builderv0.PackageArtifact(nil), artifacts...) {
			subject := req.GetSubject()
			publisher, name, version := "", artifactName, ""
			if subject != nil {
				publisher, name, version = subject.GetPublisher(), subject.GetName(), subject.GetVersion()
			}
			if name == "" {
				name = artifactName
			}
			release, err := sbom.AttachArtifact(source, sbom.Artifact{
				Publisher: publisher,
				Name:      name,
				Version:   version,
				Target:    executable.GetTarget().GetOs() + "/" + executable.GetTarget().GetArchitecture(),
				SHA256:    executable.GetSha256(),
			})
			if err != nil {
				return s.Builder.PackageError(fmt.Errorf("attach package artifact to SBOM: %w", err))
			}
			payload, err := sbom.MarshalCycloneDXJSON(release.Bom)
			if err != nil {
				return s.Builder.PackageError(fmt.Errorf("encode package SBOM: %w", err))
			}
			destination := executable.GetPath() + ".cdx.json"
			if err := writePackageFile(destination, append(payload, '\n'), 0o644); err != nil {
				return s.Builder.PackageError(fmt.Errorf("write package SBOM: %w", err))
			}
			digest, err := packageFileSHA256(destination)
			if err != nil {
				return s.Builder.PackageError(err)
			}
			artifacts = append(artifacts, &builderv0.PackageArtifact{
				Kind:      builderv0.PackageArtifact_SBOM,
				Path:      destination,
				Target:    executable.GetTarget(),
				Sha256:    digest,
				MediaType: "application/vnd.cyclonedx+json",
			})
		}
	}
	sort.Slice(artifacts, func(i, j int) bool {
		left := artifacts[i].GetTarget().GetOs() + "/" + artifacts[i].GetTarget().GetArchitecture() + "/" + artifacts[i].GetPath()
		right := artifacts[j].GetTarget().GetOs() + "/" + artifacts[j].GetTarget().GetArchitecture() + "/" + artifacts[j].GetPath()
		return left < right
	})
	return s.Builder.PackageResponse(artifacts)
}

func normalizePackageTargets(requested []*builderv0.PackageTarget) ([]*builderv0.PackageTarget, error) {
	if len(requested) == 0 {
		requested = []*builderv0.PackageTarget{{Os: runtime.GOOS, Architecture: runtime.GOARCH}}
	}
	byIdentity := make(map[string]*builderv0.PackageTarget, len(requested))
	for _, target := range requested {
		if target == nil || !validPackageTargetComponent(target.GetOs()) || !validPackageTargetComponent(target.GetArchitecture()) {
			return nil, fmt.Errorf("invalid package target %v", target)
		}
		identity := target.GetOs() + "/" + target.GetArchitecture()
		byIdentity[identity] = &builderv0.PackageTarget{Os: target.GetOs(), Architecture: target.GetArchitecture()}
	}
	identities := make([]string, 0, len(byIdentity))
	for identity := range byIdentity {
		identities = append(identities, identity)
	}
	sort.Strings(identities)
	targets := make([]*builderv0.PackageTarget, 0, len(identities))
	for _, identity := range identities {
		targets = append(targets, byIdentity[identity])
	}
	return targets, nil
}

func validPackageTargetComponent(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

func packageGoBinary(ctx context.Context, source, destination string, target *builderv0.PackageTarget) error {
	moduleRoot, entry, err := resolvePackageSource(ctx, source, target)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".codefly-go-package-*")
	if err != nil {
		return fmt.Errorf("prepare package output: %w", err)
	}
	temporaryPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	defer os.Remove(temporaryPath)
	if target.GetOs() == runtime.GOOS && target.GetArchitecture() == runtime.GOARCH {
		if err := packageNativeGoBinary(ctx, moduleRoot, entry, temporaryPath, target); err != nil {
			return err
		}
	} else if err := packageCrossGoBinary(ctx, moduleRoot, entry, temporaryPath, target); err != nil {
		return err
	}
	if err := os.Chmod(temporaryPath, 0o755); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return err
	}
	return nil
}

func packageNativeGoBinary(ctx context.Context, source, entry, destination string, target *builderv0.PackageTarget) error {
	command := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", destination, entry)
	command.Dir = source
	command.Env = append(os.Environ(),
		"CGO_ENABLED=1",
		"GOOS="+target.GetOs(),
		"GOARCH="+target.GetArchitecture(),
		"GOWORK=off",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go package %s/%s with native CGO toolchain: %w\n%s", target.GetOs(), target.GetArchitecture(), err, strings.TrimSpace(string(output)))
	}
	return nil
}

type crossCGOToolchain struct {
	cc  string
	cxx string
}

func crossCGOToolchainFor(identity string) (crossCGOToolchain, bool) {
	switch identity {
	case "darwin/amd64":
		return crossCGOToolchain{cc: "o64-clang", cxx: "o64-clang++"}, true
	case "darwin/arm64":
		return crossCGOToolchain{cc: "oa64-clang", cxx: "oa64-clang++"}, true
	case "linux/amd64":
		return crossCGOToolchain{cc: "x86_64-linux-gnu-gcc", cxx: "x86_64-linux-gnu-g++"}, true
	case "linux/arm64":
		return crossCGOToolchain{cc: "aarch64-linux-gnu-gcc", cxx: "aarch64-linux-gnu-g++"}, true
	default:
		return crossCGOToolchain{}, false
	}
}

func packageCrossGoBinary(ctx context.Context, source, entry, destination string, target *builderv0.PackageTarget) (resultErr error) {
	identity := target.GetOs() + "/" + target.GetArchitecture()
	toolchain, supported := crossCGOToolchainFor(identity)
	if !supported {
		return fmt.Errorf("go package %s: no CGO cross toolchain; supported cross targets are darwin/amd64, darwin/arm64, linux/amd64, linux/arm64", identity)
	}
	goToolchain, err := resolvePackageGoToolchain(ctx, source)
	if err != nil {
		return err
	}

	runner, err := companion.NewCompanionRunner(ctx, companion.CompanionOpts{
		Name:      fmt.Sprintf("go-package-%s-%s-%d", target.GetOs(), target.GetArchitecture(), time.Now().UnixNano()),
		SourceDir: source,
		Image: &resources.DockerImage{
			Name: "ghcr.io/goreleaser/goreleaser-cross",
			Tag:  "v1.27.0",
		},
		PreferredBackend: companion.BackendDocker,
	})
	if err != nil {
		return fmt.Errorf("go package %s with CGO cross toolchain: %w", identity, err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if shutdownErr := runner.Shutdown(shutdownCtx); shutdownErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("shutdown go package %s toolchain: %w", identity, shutdownErr))
		}
	}()

	// ARCHITECTURE: goreleaser-cross is a one-shot tool image whose default
	// entrypoint invokes goreleaser. Package needs the image's real Go and C/C++
	// toolchains behind Codefly's typed process runner, so it explicitly clears
	// that entrypoint and keeps the container alive for Docker exec.
	runner.WithEntrypoint()
	runner.WithMount(filepath.Dir(destination), filepath.Dir(destination))
	runner.WithWorkDir(source)
	runner.WithUser(fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()))
	runner.WithPause()
	if err := runner.Init(ctx); err != nil {
		return fmt.Errorf("initialize go package %s CGO toolchain: %w", identity, err)
	}

	process, err := runner.NewProcess("go", "build", "-trimpath", "-o", destination, entry)
	if err != nil {
		return fmt.Errorf("create go package %s process: %w", identity, err)
	}
	process.WithEnvironmentVariables(ctx,
		resources.Env("CGO_ENABLED", "1"),
		resources.Env("GOOS", target.GetOs()),
		resources.Env("GOARCH", target.GetArchitecture()),
		resources.Env("CC", toolchain.cc),
		resources.Env("CXX", toolchain.cxx),
		resources.Env("GOWORK", "off"),
		resources.Env("GOTOOLCHAIN", goToolchain),
		resources.Env("HOME", "/tmp/codefly-home"),
		resources.Env("GOCACHE", "/tmp/codefly-go-build"),
		resources.Env("GOMODCACHE", "/tmp/codefly-go-mod"),
	)
	var output bytes.Buffer
	process.WithOutput(&output)
	if err := process.Run(ctx); err != nil {
		return fmt.Errorf("go package %s with CGO cross toolchain: %w\n%s", identity, err, strings.TrimSpace(output.String()))
	}
	return nil
}

func packageFileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func writePackageFile(destination string, payload []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".codefly-package-evidence-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, destination)
}

// Deploy renders k8s manifests and applies them.
func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	return golanghelpers.DeployGoKubernetes(ctx, s.Base.Builder, req, s.EnvironmentVariables, s.cfg.DeploymentFS)
}

// Options are the default communicate questions for `codefly add service`.
func (s *Builder) Options() []*agentv0.Question {
	return []*agentv0.Question{
		communicate.NewConfirm(&agentv0.Message{Name: HotReload, Message: "Code hot-reload?", Description: "Restart service when code changes"}, true),
		communicate.NewConfirm(&agentv0.Message{Name: DebugSymbols, Message: "Start with debug symbols?", Description: "Build with debug symbols for stack debugging"}, false),
		communicate.NewConfirm(&agentv0.Message{Name: RaceConditionDetectionRun, Message: "Start with race condition detection?", Description: "Build with -race"}, false),
	}
}

// CreateConfiguration is the template context passed to factory templates.
type CreateConfiguration struct {
	*services.Information
	Envs []string
}

func (s *Builder) Create(ctx context.Context, req *builderv0.CreateRequest) (*builderv0.CreateResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	if s.Builder.CreationMode != nil && s.Builder.CreationMode.Communicate && s.answers != nil {
		var err error
		s.Settings.HotReload, err = communicate.Confirm(s.answers, HotReload)
		if err != nil {
			return s.Builder.CreateError(err)
		}
		s.Settings.DebugSymbols, err = communicate.Confirm(s.answers, DebugSymbols)
		if err != nil {
			return s.Builder.CreateError(err)
		}
		s.Settings.RaceConditionDetectionRun, err = communicate.Confirm(s.answers, RaceConditionDetectionRun)
		if err != nil {
			return s.Builder.CreateError(err)
		}
	} else {
		options := s.Options()
		var err error
		s.Settings.HotReload, err = communicate.GetDefaultConfirm(options, HotReload)
		if err != nil {
			return s.Builder.CreateError(err)
		}
		s.Settings.DebugSymbols, err = communicate.GetDefaultConfirm(options, DebugSymbols)
		if err != nil {
			return s.Builder.CreateError(err)
		}
		s.Settings.RaceConditionDetectionRun, err = communicate.GetDefaultConfirm(options, RaceConditionDetectionRun)
		if err != nil {
			return s.Builder.CreateError(err)
		}
	}

	create := CreateConfiguration{Information: s.Information, Envs: []string{}}
	ignore := shared.NewIgnore("go.work*", "service.generation.codefly.yaml")

	if err := s.Templates(ctx, create, services.WithFactory(s.cfg.FactoryFS).WithPathSelect(ignore)); err != nil {
		return s.Builder.CreateError(err)
	}
	return s.Builder.CreateResponse(ctx, s.Settings)
}

func (s *Builder) Communicate(stream builderv0.Builder_CommunicateServer) error {
	asker := communicate.NewQuestionAsker(stream)
	answers, err := asker.RunSequence(s.Options())
	if err != nil {
		return err
	}
	s.answers = answers
	return nil
}
