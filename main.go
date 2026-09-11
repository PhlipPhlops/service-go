// Binary service-go is the generic Go agent entry point.
//
// All logic lives under ./pkg so specializations (go-grpc, …) can import
// and compose the reusable pieces:
//
//	github.com/codefly-dev/service-go/pkg/service   — shared Service/Settings
//	github.com/codefly-dev/service-go/pkg/runtime   — Runtime gRPC server
//	github.com/codefly-dev/service-go/pkg/code      — Code gRPC server
//	github.com/codefly-dev/service-go/pkg/tooling   — Tooling gRPC server
//	github.com/codefly-dev/service-go/pkg/builder   — Builder gRPC server
//
// Templates are embedded here (at the binary root) and passed to
// pkg/builder — //go:embed cannot reach up from a subpackage.
package main

import (
	"embed"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/builders"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/toolbox/lang"

	gobuilder "github.com/codefly-dev/service-go/pkg/builder"
	gocode "github.com/codefly-dev/service-go/pkg/code"
	goruntime "github.com/codefly-dev/service-go/pkg/runtime"
	goservice "github.com/codefly-dev/service-go/pkg/service"
	gotooling "github.com/codefly-dev/service-go/pkg/tooling"
)

// Agent version loaded from agent.codefly.yaml.
var agent = shared.Must(resources.LoadFromFs[resources.Agent](shared.Embed(infoFS)))

// File dependencies watched for change detection during build.
var requirements = builders.NewDependencies(agent.Name,
	builders.NewDependency("service.codefly.yaml"),
	builders.NewDependency("code").WithPathSelect(shared.NewSelect("*.go")),
)

func main() {
	svc := goservice.New(agent)
	code := gocode.New(svc)
	rt := goruntime.New(svc)
	tooling := gotooling.New(code, rt)
	agents.Serve(agents.PluginRegistration{
		Agent:   svc,
		Runtime: rt,
		Builder: gobuilder.New(svc, gobuilder.BuildConfig{
			FactoryFS:    factoryFS,
			BuilderFS:    builderFS,
			DeploymentFS: deploymentFS,
			Requirements: requirements,
		}),
		Code:    code,
		Tooling: tooling,
		Toolbox: lang.NewToolboxFromTooling(agent.Name, agent.Version, tooling),
	})
}

//go:embed agent.codefly.yaml
var infoFS embed.FS

//go:embed templates/factory
var factoryFS embed.FS

//go:embed templates/builder
var builderFS embed.FS

//go:embed templates/deployment
var deploymentFS embed.FS
