// Package runtime implements the generic Go Runtime gRPC service.
// Specializations embed *Runtime (Go struct embedding) to inherit the
// full lifecycle and override only what their layer adds — typically
// Load (additional endpoint wiring) and Init (REST/gRPC env vars).
// Test, Lint, Build are reused as-is.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/codefly-dev/core/agents/helpers/code"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/builders"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/llmout"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	golanghelpers "github.com/codefly-dev/core/runners/golang"
	selectioncontract "github.com/codefly-dev/core/runners/testselection"

	goservice "github.com/codefly-dev/service-go/pkg/service"
)

// RuntimeImage is the default runtime Docker image. Specializations can
// override by reassigning before Init if their layer needs a different base.
var RuntimeImage = &resources.DockerImage{Name: "codeflydev/go", Tag: "0.0.10"}

// Runtime is the generic Go runtime server. Embedded by specializations
// (go-grpc, …) to inherit the services.Base chain via *goservice.Service
// and the full lifecycle methods.
type Runtime struct {
	services.RuntimeServer
	*goservice.Service

	// RunnerEnvironment is exported so specializations can reach it for
	// extra env wiring or port bindings. Nil before Init.
	RunnerEnvironment *golanghelpers.GoRunnerEnvironment

	cacheLocation string
	runner        runners.Proc
	// runnerCancel distinguishes an intentional Stop/hot-reload replacement
	// from a user process that exited unexpectedly.
	runnerCancel context.CancelFunc
	testProc     runners.Proc
}

// New builds a generic Go Runtime bound to the shared Service.
func New(svc *goservice.Service) *Runtime {
	return &Runtime{Service: svc}
}

func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	err := s.Base.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return s.Runtime.LoadErrorf(err, "loading base")
	}

	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	if req.DisableCatch {
		s.Wool.DisableCatch()
	}
	if err = s.Settings.Validate(); err != nil {
		return s.Runtime.LoadErrorf(err, "invalid Go settings")
	}

	s.Runtime.SetEnvironment(req.Environment)

	// Prefer configured source dir (default: code/).
	// Fall back to service root if source dir has no go.mod (arbitrary Go project).
	sourceLocation, err := s.LocalDirCreate(ctx, "%s", s.Settings.GoSourceDir())
	if err != nil {
		return s.Runtime.LoadErrorf(err, "creating source location")
	}
	if _, statErr := os.Stat(path.Join(sourceLocation, "go.mod")); statErr != nil {
		if _, rootErr := os.Stat(path.Join(s.Location, "go.mod")); rootErr == nil {
			sourceLocation = s.Location
		}
	}
	s.Service.SetSourceLocation(sourceLocation)

	s.cacheLocation, err = s.LocalDirCreate(ctx, ".cache")
	if err != nil {
		return s.Runtime.LoadErrorf(err, "creating cache location")
	}

	// Optional: load endpoints if service has any (e.g. HTTP health). No gRPC required.
	s.Endpoints, _ = s.Base.Service.LoadEndpoints(ctx)
	// Leave GrpcEndpoint/RestEndpoint unset — go has no gRPC

	return s.Runtime.LoadResponse()
}

func (s *Runtime) SetRuntimeContext(_ context.Context, runtimeContext *basev0.RuntimeContext) error {
	s.Runtime.RuntimeContext = golanghelpers.SetGoRuntimeContext(runtimeContext)
	return nil
}

func (s *Runtime) CreateRunnerEnvironment(ctx context.Context) error {
	s.Wool.Trace("creating runner environment", wool.DirField(s.Identity.WorkspacePath))

	cfg := golanghelpers.RunnerConfig{
		RuntimeImage:   RuntimeImage,
		WorkspacePath:  s.Identity.WorkspacePath,
		RelativeSource: s.Identity.RelativeToWorkspace,
		UniqueName:     s.UniqueWithWorkspace(),
		CacheLocation:  s.cacheLocation,
		Settings: &golanghelpers.GoAgentSettings{
			HotReload:                 s.Settings.HotReload,
			DebugSymbols:              s.Settings.DebugSymbols,
			RaceConditionDetectionRun: s.Settings.RaceConditionDetectionRun,
			WithCGO:                   s.Settings.WithCGO,
			WithWorkspace:             s.Settings.WithWorkspace,
			SourceDir:                 s.Settings.SourceDir,
		},
	}

	env, err := golanghelpers.CreateRunner(ctx, s.Runtime.RuntimeContext, cfg)
	if err != nil {
		return err
	}

	allEnvs, err := s.EnvironmentVariables.All()
	if err != nil {
		return s.Wool.Wrapf(err, "cannot get environment variables")
	}
	env.WithEnvironmentVariables(ctx, allEnvs...)

	s.RunnerEnvironment = env
	// Expose the underlying RunnerEnvironment on the shared Service so
	// Code / Tooling / commands route spawns through the same mode —
	// without reaching into the Go-specific wrapper.
	s.Service.SetRunnerEnvironment(env.Env())
	return nil
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.LogInitRequest(req)

	err := s.SetRuntimeContext(ctx, req.RuntimeContext)
	if err != nil {
		return s.Runtime.InitErrorf(err, "cannot set runtime context")
	}

	s.Wool.Forwardf("starting execution environment in %s mode", s.Runtime.RuntimeContext.Kind)
	s.EnvironmentVariables.SetRuntimeContext(s.Runtime.RuntimeContext)
	s.NetworkMappings = req.ProposedNetworkMappings

	// Init reaches the service in every test dependency mode; the Start that
	// reaches a service under test is often a policy barrier carrying nothing.
	s.Runtime.SetFixtureFromInit(req.GetFixture())
	s.Runtime.SetOverridesFromInit(req.GetOverrides())

	// Service's own configuration: configurations/<env>/*.env (incl. *.secret.env)
	// → the service's own configured values injected into its environment. Without
	// this a service never receives its own config (e.g. secrets via
	// CODEFLY__SERVICE_SECRET_CONFIGURATION__...). Mirror python-fastapi.
	err = s.EnvironmentVariables.AddConfigurations(ctx, req.Configuration)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	err = s.EnvironmentVariables.AddConfigurations(ctx, req.WorkspaceConfigurations...)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	confs := resources.FilterConfigurations(req.DependenciesConfigurations, s.Runtime.RuntimeContext)
	s.Wool.Trace("adding configurations", wool.Field("configurations", resources.MakeManyConfigurationSummary(confs)))
	err = s.EnvironmentVariables.AddConfigurations(ctx, confs...)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	// Plain Go services may declare HTTP or other endpoints without a protocol
	// specialization. Carry their own resolved endpoints just as we carry the
	// dependency endpoints at Start; the SDK must not guess a bind address.
	err = s.EnvironmentVariables.AddEndpoints(ctx, req.ProposedNetworkMappings, resources.NetworkAccessFromRuntimeContext(s.Runtime.RuntimeContext))
	if err != nil {
		return s.Runtime.InitErrorf(err, "adding service endpoints")
	}

	if s.RunnerEnvironment == nil {
		err = s.CreateRunnerEnvironment(ctx)
		if err != nil {
			return s.Runtime.InitErrorf(err, "cannot create runner environment")
		}
	}

	err = s.RunnerEnvironment.Init(ctx)
	if err != nil {
		s.Wool.Error("cannot init the go runner", wool.ErrField(err))
		return s.Runtime.InitError(err)
	}

	s.Wool.Trace("runner init done")

	// A watcher is runtime state, not Init-request state. SetupWatcher detaches
	// it from the RPC context and Stop owns its lifetime. Re-create it on every
	// Init so changed settings (including source-dir/hot-reload) take effect.
	s.Base.StopWatcher()
	if s.Settings.HotReload {
		watchSource, relErr := filepath.Rel(s.Location, s.Service.SourceLocation)
		if relErr != nil {
			return s.Runtime.InitErrorf(relErr, "resolving source directory for hot reload")
		}
		dependencies := builders.NewDependencies("go-runtime",
			builders.NewDependency("service.codefly.yaml"),
			builders.NewDependency(watchSource).WithPathSelect(shared.NewSelect("*.go")),
		)
		if err = s.SetupWatcher(ctx, services.NewWatchConfiguration(dependencies), s.EventHandler); err != nil {
			// Hot reload changes Start semantics: compile errors are allowed while
			// waiting for a later edit. If the watcher cannot start, failing Init is
			// safer than reporting a dead service as successfully started.
			return s.Runtime.InitErrorf(err, "setting up hot reload")
		}
		s.Watcher.Resume()
	}
	return s.Runtime.InitResponse()
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Info("Building go binary")

	if s.runner != nil {
		if s.runnerCancel != nil {
			s.runnerCancel()
			s.runnerCancel = nil
		}
		if err := s.runner.Stop(ctx); err != nil {
			return s.Runtime.StartError(err)
		}
		s.runner = nil // don't keep a stopped runner if Start bails before re-assigning
	}

	err := s.RunnerEnvironment.BuildBinary(ctx)
	if err != nil {
		if s.Settings.HotReload && s.Watcher != nil {
			s.Wool.Info("compile error, waiting for hot-reload")
			return s.Runtime.StartResponse()
		}
		return s.Runtime.StartError(err)
	}

	err = s.EnvironmentVariables.AddEndpoints(ctx, req.DependenciesNetworkMappings, resources.NetworkAccessFromRuntimeContext(s.Runtime.RuntimeContext))
	if err != nil {
		return s.Runtime.StartError(err)
	}
	s.Runtime.SetFixtureFromStart(req.GetFixture())
	s.Runtime.SetOverridesFromStart(req.GetOverrides())

	// The service must outlive the Start RPC, but intentional teardown must be
	// visible to the supervisor before the process is stopped.
	runningContext, runnerCancel := context.WithCancel(s.Wool.Inject(context.Background()))
	s.runnerCancel = runnerCancel
	superviseStarted := false
	defer func() {
		if !superviseStarted {
			runnerCancel()
			s.runnerCancel = nil
		}
	}()

	proc, err := s.RunnerEnvironment.Runner()
	if err != nil {
		return s.Runtime.StartErrorf(err, "getting runner")
	}
	startEnvs, err := s.EnvironmentVariables.All()
	if err != nil {
		return s.Runtime.StartErrorf(err, "getting environment variables")
	}
	proc.WithEnvironmentVariables(ctx, startEnvs...)
	proc.WithOutput(s.Logger)

	s.runner = proc
	err = s.runner.Start(runningContext)
	if err != nil {
		s.runner = nil
		return s.Runtime.StartErrorf(err, "starting runner")
	}
	superviseStarted = true
	go func(p runners.Proc) {
		err := p.Wait(runningContext)
		if runningContext.Err() != nil {
			return
		}
		if err != nil {
			s.Wool.Error("user binary exited unexpectedly", wool.ErrField(err))
		} else {
			s.Wool.Error("user binary exited unexpectedly (clean exit, context not cancelled)")
		}
		s.Runtime.MarkRunnerExited(err)
	}(proc)
	s.Wool.Trace("runner started successfully")
	return s.Runtime.StartResponse()
}

func (s *Runtime) Build(ctx context.Context, req *runtimev0.BuildRequest) (*runtimev0.BuildResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Infof("running go build")

	invocation, err := s.Service.GoBuildInvocation(ctx, req.Target, s.RunnerEnvironment)
	if err != nil {
		return s.Runtime.BuildErrorf(err, "resolving build invocation")
	}
	output, runErr := invocation.Build(ctx)
	// Compress before the output reaches the model. On failure especially, the
	// compiler errors are the biggest and most useful payload — and because a
	// gRPC error drops the response body, the compressed errors must travel in
	// the error message.
	compressed := llmout.Compress("go", []string{"build"}, output)
	if runErr != nil {
		return s.Runtime.BuildErrorf(runErr, "build failed:\n%s", compressed)
	}
	return s.Runtime.BuildResponse(compressed)
}

func (s *Runtime) Test(ctx context.Context, req *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	started := time.Now()
	if req == nil {
		req = &runtimev0.TestRequest{}
	}
	if err := selectioncontract.ValidateRequest(req); err != nil {
		return nil, fmt.Errorf("go runtime: invalid test request: %w", err)
	}

	s.Wool.Info("running go tests",
		wool.Field("target", req.Target),
		wool.Field("filters", req.Filters),
		wool.Field("race", req.Race),
		wool.Field("coverage", req.Coverage),
		wool.Field("fail_fast", req.FailFast),
		wool.Field("timeout", req.Timeout),
		wool.Field("extra_args", req.ExtraArgs))

	// Structured formulas and authoritative selections share the core Go
	// formula runner. It owns Go package/-run grammar, disables accidental
	// parent go.work inheritance for standalone source checkouts, returns the
	// structured test tree, and lets the plugin acknowledge the exact typed
	// selection it honored.
	if req.GetFormula() != nil || req.GetSelection() != nil {
		var selectors []string
		if req.GetSelection() != nil {
			rendered, err := golanghelpers.RenderTestSelection(req.GetSelection())
			if err != nil {
				return nil, fmt.Errorf("go runtime selection: %w", err)
			}
			selectors = rendered
		} else {
			selectors = append(selectors, req.GetTarget())
			selectors = append(selectors, req.GetFilters()...)
		}
		var command []string
		if formula := req.GetFormula(); formula != nil {
			command = formula.GetCommand()
		}
		response, runErr := golanghelpers.RunFormula(ctx, s.Service.SourceLocation, command, selectors, req.GetFailFast())
		if acknowledgeErr := selectioncontract.Acknowledge(req, response); acknowledgeErr != nil {
			return response, errors.Join(runErr, fmt.Errorf("acknowledge typed test selection: %w", acknowledgeErr))
		}
		return response, runErr
	}

	testEnvs, err := s.EnvironmentVariables.All()
	if err != nil {
		return s.Runtime.TestErrorf(err, "getting environment variables")
	}

	opts := golanghelpers.TestOptions{
		Target:    req.Target,
		Verbose:   req.Verbose,
		Race:      req.Race,
		Timeout:   req.Timeout,
		Coverage:  req.Coverage,
		FailFast:  req.FailFast,
		Filters:   req.Filters,
		ExtraArgs: req.ExtraArgs,
		// Stream per-test events through the logger so the CLI TUI can
		// show real-time progress instead of waiting for the summary.
		OnEvent: func(ev golanghelpers.TestEvent) {
			switch ev.Action {
			case "run":
				if ev.Test != "" {
					s.Wool.Forwardf("RUN  %s", ev.Test)
				}
			case "pass":
				if ev.Test != "" {
					s.Wool.Forwardf("PASS %s (%.2fs)", ev.Test, ev.Elapsed)
				}
			case "fail":
				if ev.Test != "" {
					s.Wool.Forwardf("FAIL %s (%.2fs)", ev.Test, ev.Elapsed)
				}
			case "skip":
				if ev.Test != "" {
					s.Wool.Forwardf("SKIP %s", ev.Test)
				}
			}
		},
	}
	execution, runErr := golanghelpers.RunGoTests(ctx, s.RunnerEnvironment, s.Service.SourceLocation, testEnvs, opts)
	if execution == nil || execution.Structured == nil {
		if runErr == nil {
			runErr = fmt.Errorf("go test produced no execution result")
		}
		message := runErr.Error()
		if reason, detail := golanghelpers.ClassifyEnvError("", runErr); reason != "" {
			message = fmt.Sprintf("env-blocked (%s): %s", reason, detail)
		}
		return s.Runtime.TestError(errors.New(message))
	}

	s.Wool.Forwardf("Tests: %s", execution.SummaryLine())
	for _, f := range execution.Failures {
		s.Wool.Forwardf("%s", f)
	}

	response := execution.Structured.ToProtoResponse("go-test", req.GetSuite(), time.Since(started))
	if runErr != nil && response.GetCounts().GetTotal() == 0 {
		message := runErr.Error()
		if reason, detail := golanghelpers.ClassifyEnvError(execution.RawOutput, runErr); reason != "" {
			message = fmt.Sprintf("env-blocked (%s): %s", reason, detail)
		}
		response.Result = &runtimev0.TestRunResult{
			State: runtimev0.TestRunResult_ERRORED, Message: message,
		}
		response.Status = &runtimev0.TestStatus{
			State: runtimev0.TestStatus_ERROR, Message: message,
		}
	}
	return response, nil
}

func (s *Runtime) Lint(ctx context.Context, req *runtimev0.LintRequest) (*runtimev0.LintResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Infof("running read-only go format/import checks and go vet")

	envs, err := s.EnvironmentVariables.All()
	if err != nil {
		return s.Runtime.LintErrorf(err, "getting environment variables")
	}

	opts := golanghelpers.LintOptions{Target: req.Target}
	formatOutput, formatErr := checkGoFormatting(s.Service.SourceLocation, req.GetTarget())
	vetOutput, vetErr := golanghelpers.RunGoLint(ctx, s.RunnerEnvironment, s.Service.SourceLocation, envs, opts)
	output := strings.TrimSpace(strings.Join([]string{formatOutput, vetOutput}, "\n"))
	runErr := errors.Join(formatErr, vetErr)
	compressed := llmout.Compress("go", []string{"format/import-check", "vet"}, output)
	if runErr != nil {
		return s.Runtime.LintErrorf(runErr, "lint failed:\n%s", compressed)
	}
	return s.Runtime.LintResponse(compressed)
}

func (s *Runtime) Information(ctx context.Context, req *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	return s.Runtime.InformationResponse(ctx, req)
}

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	s.Wool.Trace("stopping service")
	// Stop accepting rebuild requests before tearing down the process.
	s.Base.StopWatcher()
	if s.testProc != nil {
		_ = s.testProc.Stop(ctx)
		s.testProc = nil
	}
	if s.runner != nil {
		if s.runnerCancel != nil {
			s.runnerCancel()
			s.runnerCancel = nil
		}
		if err := s.runner.Stop(ctx); err != nil {
			return s.Runtime.StopError(err)
		}
		s.runner = nil // released — avoid re-Stopping a dead runner on the next call
	}
	return s.Runtime.StopResponse()
}

func (s *Runtime) Destroy(ctx context.Context, req *runtimev0.DestroyRequest) (*runtimev0.DestroyResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Trace("destroying service")
	err := golanghelpers.DestroyGoRuntime(ctx, s.Runtime.RuntimeContext, RuntimeImage,
		s.cacheLocation, s.Identity.WorkspacePath,
		path.Join(s.Identity.RelativeToWorkspace, s.Settings.GoSourceDir()),
		s.UniqueWithWorkspace())
	if err != nil {
		return s.Runtime.DestroyError(err)
	}
	return s.Runtime.DestroyResponse()
}

// EventHandler translates watched configuration changes into lifecycle
// requests consumed by the orchestrator.
func (s *Runtime) EventHandler(event code.Change) error {
	s.Wool.Info("detected change", wool.Field("path", event.Path))
	if filepath.Base(event.Path) == "service.codefly.yaml" {
		s.Runtime.DesiredLoad()
		return nil
	}
	s.Runtime.DesiredStart()
	return nil
}
