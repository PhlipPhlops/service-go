package service

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/codefly-dev/core/ciinputs"
	agent "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	golanghelpers "github.com/codefly-dev/core/runners/golang"
)

// EffectiveInputPlan is owned by the operation implementation. Complete attests
// to consumption beyond native imports, including dynamic inputs and runtime
// dependencies. Inputs must include resolved execution identities for reuse.
type EffectiveInputPlan struct {
	Invocation      *GoInputInvocation
	Inputs          []*agent.EffectiveInput
	RuntimeServices []string
	Complete        bool
}

type EffectiveInputPlanner func(context.Context, *agent.GetEffectiveInputsRequest) (map[ciinputs.Key]EffectiveInputPlan, error)

// GoInputInvocation binds native discovery to the same source, target, runner
// and environment used by Build. Workspace defaults require the runner's owned
// package expansion; an explicit Target removes that ambiguity.
type GoInputInvocation struct {
	Runner      runners.RunnerEnvironment
	Source      string
	Target      string
	Environment []*resources.EnvironmentVariable
	Workspace   bool
	Tests       bool
	buildRunner *golanghelpers.GoRunnerEnvironment
}

func (s *Service) GoInputInvocation(ctx context.Context, target string, tests bool) (*GoInputInvocation, error) {
	source, err := s.ResolveSourceLocation(ctx)
	if err != nil {
		return nil, err
	}
	env := s.CurrentRunnerEnvironment()
	if env == nil {
		env, err = runners.NewNativeEnvironment(ctx, source)
		if err != nil {
			return nil, err
		}
	}
	variables, err := s.EnvironmentVariables.All()
	if err != nil {
		return nil, err
	}
	return &GoInputInvocation{Runner: env, Source: source, Target: target, Environment: variables, Workspace: s.Settings.WithWorkspace, Tests: tests}, nil
}

func (s *Service) GoBuildInvocation(ctx context.Context, target string, runner *golanghelpers.GoRunnerEnvironment) (*GoInputInvocation, error) {
	if runner == nil {
		return nil, fmt.Errorf("build invocation requires an initialized Go runner")
	}
	inv, err := s.GoInputInvocation(ctx, target, false)
	if err != nil {
		return nil, err
	}
	inv.Runner, inv.buildRunner = runner.Env(), runner
	return inv, nil
}

func (inv *GoInputInvocation) Build(ctx context.Context) (string, error) {
	if inv.buildRunner == nil {
		return "", fmt.Errorf("build invocation requires an initialized Go runner")
	}
	return golanghelpers.RunGoBuild(ctx, inv.buildRunner, inv.Source, inv.Environment, golanghelpers.BuildOptions{Target: inv.Target})
}

func (inv *GoInputInvocation) list(ctx context.Context) ([]byte, error) {
	args := []string{"list", "-deps", "-json"}
	if !inv.Workspace {
		if _, err := os.Stat(filepath.Join(inv.Source, "vendor", "modules.txt")); os.IsNotExist(err) {
			args = append(args, "-mod=readonly")
		}
	}
	if inv.Tests {
		args = append(args, "-test")
	}
	target := inv.Target
	if target == "" {
		target = "./..."
	}
	args = append(args, target)
	proc, err := inv.Runner.NewProcess("go", args...)
	if err != nil {
		return nil, err
	}
	proc.WithDir(inv.Source)
	proc.WithEnvironmentVariables(ctx, inv.Environment...)
	if !inv.Workspace {
		proc.WithEnvironmentVariables(ctx, resources.Env("GOWORK", "off"))
	}
	proc.WithOutput(io.Discard)
	stdout, err := proc.StdoutPipe()
	if err != nil {
		return nil, err
	}
	defer stdout.Close()
	if err = proc.Start(ctx); err != nil {
		return nil, err
	}
	output, readErr := io.ReadAll(stdout)
	waitErr := proc.Wait(ctx)
	if readErr != nil {
		return nil, readErr
	}
	return output, waitErr
}

// GenericGoInputPlans is registered alongside the generic no-op Builder.Sync.
// Specializations with generation must supply their own Sync plan.
func GenericGoInputPlans(ctx context.Context, _ *agent.GetEffectiveInputsRequest) (map[ciinputs.Key]EffectiveInputPlan, error) {
	name, err := os.Executable()
	if err != nil {
		return nil, err
	}
	identity, err := fileIdentity(ctx, name)
	if err != nil {
		return nil, err
	}
	return map[ciinputs.Key]EffectiveInputPlan{
		{Phase: agent.TaskPhase_TASK_PHASE_SYNC}: {
			Complete: true,
			Inputs: []*agent.EffectiveInput{
				{Kind: agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_PLUGIN, Owner: "go-sync", Name: "executable", Identity: identity},
				{Kind: agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_TOOLCHAIN, Owner: "go-sync", Name: "embedded-go-runtime", Identity: identity},
			},
		},
	}, nil
}
