// Package service defines the generic Go agent's shared state.
// Specializations (go-grpc, …) embed *Service in their own Service and
// add protocol-specific fields (endpoints, richer settings).
package service

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/codefly-dev/core/agents/services"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/languages"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	golanghelpers "github.com/codefly-dev/core/runners/golang"
)

// Settings is the generic Go agent's configuration. Specializations embed
// this inline (yaml:",inline") to inherit GoAgentSettings fields:
//
//	type Settings struct {
//	    goservice.Settings `yaml:",inline"`
//	    RestEndpoint bool `yaml:"rest-endpoint"`
//	}
type Settings struct {
	golanghelpers.GoAgentSettings `yaml:",inline"`
}

// Service carries the shared state used by Runtime, Code, Tooling, Builder.
// Specializations embed *Service to inherit the identity, logger, location,
// and source resolution.
type Service struct {
	*services.Base
	Settings *Settings

	// SourceLocation is the path to the Go sources. Read-only Code/Tooling can
	// bind it before Runtime.Load; Runtime and Builder may later reaffirm the
	// same configured root. It typically points at `<service>/code` (via
	// Settings.GoSourceDir()) but falls back to the service root when go.mod is
	// declared there.
	SourceLocation string
	sourceMu       sync.RWMutex

	// ActiveEnv is the plugin's active RunnerEnvironment — set by
	// Runtime.Init via CreateRunnerEnvironment and consumed by Code /
	// Tooling / commands so every spawn routes through the same mode
	// (native / docker / nix). Distinct from Runtime.RunnerEnvironment
	// which is the Go-specific wrapper (GoRunnerEnvironment); this is
	// the underlying interface, obtained via env.Env() on the wrapper.
	// Nil before Runtime.Init — call-sites fall back to a fresh
	// NativeEnvironment for pre-init ops (typically Code file-level).
	ActiveEnv runners.RunnerEnvironment

	// InputPlans is configured by the operation owner before serving RPCs.
	InputPlans EffectiveInputPlanner
}

func (s *Service) CurrentRunnerEnvironment() runners.RunnerEnvironment {
	s.sourceMu.RLock()
	defer s.sourceMu.RUnlock()
	return s.ActiveEnv
}

func (s *Service) SetRunnerEnvironment(env runners.RunnerEnvironment) {
	s.sourceMu.Lock()
	defer s.sourceMu.Unlock()
	s.ActiveEnv = env
}

// CurrentSourceLocation returns the source root currently shared by Code,
// Tooling, Runtime, and Builder.
func (s *Service) CurrentSourceLocation() string {
	s.sourceMu.RLock()
	defer s.sourceMu.RUnlock()
	return s.SourceLocation
}

// SetSourceLocation atomically rebinds the shared source root.
func (s *Service) SetSourceLocation(location string) {
	s.sourceMu.Lock()
	defer s.sourceMu.Unlock()
	s.SourceLocation = location
}

// ResolveSourceLocation binds read-only Code and Tooling traffic to the real
// configured project source before Runtime.Load. Core owns declaration
// hydration and attachment resolution; this language layer retains only the
// Go convention that a root-level go.mod identifies the service root.
func (s *Service) ResolveSourceLocation(ctx context.Context) (string, error) {
	s.sourceMu.Lock()
	defer s.sourceMu.Unlock()
	if s.SourceLocation != "" {
		return s.SourceLocation, nil
	}
	location, err := s.Base.ResolveSourceLocation(ctx, s.Settings, s.Settings.GoSourceDir)
	if err != nil {
		return "", err
	}
	if _, statErr := os.Stat(filepath.Join(location, "go.mod")); statErr != nil {
		root, rootErr := s.Base.ResolveSourceLocation(ctx, s.Settings, nil)
		if rootErr != nil {
			return "", rootErr
		}
		if _, rootStatErr := os.Stat(filepath.Join(root, "go.mod")); rootStatErr == nil {
			location = root
		}
	}
	s.SourceLocation = location
	return location, nil
}

// New builds a generic Go Service bound to the given agent manifest.
func New(agent *resources.Agent) *Service {
	return &Service{
		Base:     services.NewServiceBase(context.Background(), agent),
		Settings: &Settings{},
	}
}

// GetAgentInformation returns the generic Go agent advertisement.
// Specializations should override this; their overrides typically add
// protocols (HTTP/gRPC) and techniques. The generic advertisement has no
// README because README rendering depends on embed.FS which can't cross
// package boundaries — each specialization's binary embeds and renders
// its own.
func (s *Service) GetAgentInformation(_ context.Context, _ *agentv0.AgentInformationRequest) (*agentv0.AgentInformation, error) {
	return services.Advertisement{
		Backends: runners.BackendSupport{
			Local:  func() bool { return languages.HasGoRuntime(nil) },
			Nix:    true,
			Docker: true,
		},
		Toolchains:              []agentv0.Toolchain_Type{agentv0.Toolchain_GO},
		Languages:               []agentv0.Language_Type{agentv0.Language_GO},
		ReadMe:                  "Generic Go service. Specializations add protocols.",
		Validation:              ValidationCapabilities(),
		EffectiveInputsVersions: []uint32{1},
	}.Build(), nil
}

// ValidationCapabilities is the authoritative operation contract inherited by
// generic Go service plugins. It describes semantic operations; local commands,
// Mind/editor integrations, and CI all dispatch the corresponding Runtime and
// Builder RPCs.
func ValidationCapabilities() *agentv0.ValidationCapabilities {
	workspace := []agentv0.ValidationScope{
		agentv0.ValidationScope_VALIDATION_SCOPE_WORKSPACE,
	}
	operation := func(supported bool) *agentv0.ValidationOperationCapability {
		return &agentv0.ValidationOperationCapability{
			Supported: supported,
			Scopes:    append([]agentv0.ValidationScope(nil), workspace...),
		}
	}
	return &agentv0.ValidationCapabilities{
		Lint:    operation(true),
		Compile: operation(true),
		Test: &agentv0.TestValidationCapability{
			Supported: true,
			Scopes:    append([]agentv0.ValidationScope(nil), workspace...),
			Suites: []*agentv0.TestSuiteCapability{{
				Name:           "unit",
				DependencyMode: agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_NONE,
				DefaultSuite:   true,
			}},
		},
		Audit:         operation(true),
		ArtifactBuild: operation(true),
		Sbom:          operation(true),
		SourcePackage: operation(true),
		// Generic Go has no derived source to generate. Its Sync RPC is an
		// authoritative, non-mutating no-op in both normal and dry-run modes.
		Sync: operation(true),
	}
}
