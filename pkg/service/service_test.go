package service_test

import (
	"context"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/codefly-dev/core/resources"

	goservice "github.com/codefly-dev/service-go/pkg/service"
)

// TestNew verifies a generic Go Service can be constructed and carries
// a non-nil Base + Settings. Foundation for every specialization.
func TestNew(t *testing.T) {
	svc := goservice.New(&resources.Agent{
		Kind:      "codefly:service",
		Publisher: "codefly.dev",
		Name:      "go",
		Version:   "0.0.1",
	})
	if svc == nil {
		t.Fatal("New returned nil")
	}
	if svc.Base == nil {
		t.Fatal("Service.Base is nil — services.Base embedding broken")
	}
	if svc.Settings == nil {
		t.Fatal("Service.Settings is nil")
	}
	if svc.SourceLocation != "" {
		t.Fatalf("SourceLocation should be empty before Load, got %q", svc.SourceLocation)
	}
}

// TestSettingsYAMLInline proves the inline-embed pattern specializations
// rely on. If it ever breaks, every specialization's YAML fixtures break
// silently.
func TestSettingsYAMLInline(t *testing.T) {
	type grpcSettings struct {
		goservice.Settings `yaml:",inline"`
		RestEndpoint       bool `yaml:"rest-endpoint"`
	}

	src := []byte(`
hot-reload: true
debug-symbols: false
rest-endpoint: true
`)

	var s grpcSettings
	if err := yaml.Unmarshal(src, &s); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	if !s.HotReload {
		t.Error("inherited HotReload not populated")
	}
	if !s.RestEndpoint {
		t.Error("local RestEndpoint not populated")
	}
}

// TestGetAgentInformationGeneric locks the generic advertisement contract.
func TestGetAgentInformationGeneric(t *testing.T) {
	svc := goservice.New(&resources.Agent{Kind: "codefly:service", Name: "go"})
	info, err := svc.GetAgentInformation(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetAgentInformation: %v", err)
	}
	if len(info.Languages) != 1 || info.Languages[0].Type.String() != "GO" {
		t.Errorf("expected single GO language, got %+v", info.Languages)
	}
	if len(info.Protocols) != 0 {
		t.Errorf("generic go should advertise no protocols, got %d", len(info.Protocols))
	}
	if versions := info.GetEffectiveInputsVersions(); len(versions) != 1 || versions[0] != 1 {
		t.Errorf("expected effective-input discovery schema v1, got versions %v", versions)
	}
}
