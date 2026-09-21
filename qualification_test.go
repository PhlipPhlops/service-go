package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/contract"
	"github.com/codefly-dev/core/agents/manager"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/recoveryscope"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// TestSelfBootstrapAdvertisesTheRuntimeContract walks the path a host with an
// empty CODEFLY_HOME takes to qualify this agent: it runs the declared
// bootstrap command to produce the executable, installs it, then spawns it and
// reads the advertisement over the authenticated agent connection.
func TestSelfBootstrapAdvertisesTheRuntimeContract(t *testing.T) {
	var manifest struct {
		Source struct {
			Directory string   `yaml:"directory"`
			Agent     string   `yaml:"agent"`
			Bootstrap []string `yaml:"bootstrap"`
		} `yaml:"source"`
	}
	readPolicyYAML(t, "agent.codefly.yaml", &manifest)
	if manifest.Source.Agent != "self" {
		t.Fatalf("source.agent = %q, want self", manifest.Source.Agent)
	}
	if len(manifest.Source.Bootstrap) == 0 {
		t.Fatal("a self source agent owns its bootstrap command")
	}

	home := t.TempDir()
	t.Setenv(resources.CodeflyHomeEnv, home)
	installed, err := agent.Path(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}

	bootstrap := exec.Command(manifest.Source.Bootstrap[0], manifest.Source.Bootstrap[1:]...)
	bootstrap.Dir = manifest.Source.Directory
	bootstrap.Env = append(os.Environ(),
		"GOWORK=off",
		resources.CodeflyHomeEnv+"="+home,
		"CODEFLY_AGENT_OUTPUT="+installed,
		"CODEFLY_AGENT_OS="+runtime.GOOS,
		"CODEFLY_AGENT_ARCH="+runtime.GOARCH,
	)
	if out, err := bootstrap.CombinedOutput(); err != nil {
		t.Fatalf("bootstrap %v: %v\n%s", manifest.Source.Bootstrap, err, out)
	}
	info, err := os.Lstat(installed)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		t.Fatalf("bootstrap emitted %s (%d bytes), want a nonempty regular executable", info.Mode(), info.Size())
	}
	if err := os.Chmod(installed, 0o755); err != nil {
		t.Fatal(err)
	}

	scope, namespace := strings.Repeat("a", 64), strings.Repeat("b", 64)
	conn, err := manager.Load(t.Context(), agent,
		manager.WithoutSandbox(),
		manager.WithoutPrincipal(),
		manager.WithEnv(recoveryscope.EnvironmentVariable+"="+recoveryscope.Marker(os.Getpid(), scope, namespace)),
	)
	if err != nil {
		t.Fatalf("load bootstrapped agent: %v", err)
	}
	t.Cleanup(conn.Close)

	var headers metadata.MD
	advertisement, err := agentv0.NewAgentClient(conn.GRPCConn()).GetAgentInformation(t.Context(), &agentv0.AgentInformationRequest{}, grpc.Header(&headers))
	if err != nil {
		t.Fatalf("discover agent information: %v\n%s", err, conn.StderrTail())
	}
	if err := contract.Check(advertisement.GetContract(), contract.ContainerRecoveryScope); err != nil {
		t.Fatal(err)
	}
	// Exact, not "contains": a capability gained through a Core bump reaches
	// this advertisement whether or not this agent honours it, and every claim
	// here is one a host is entitled to act on.
	if capabilities := advertisement.GetContract().GetCapabilities(); !slices.Equal(capabilities, []string{contract.ContainerRecoveryScope}) {
		t.Fatalf("advertised capabilities %v", capabilities)
	}
	if acknowledgement := headers.Get(recoveryscope.Header); !slices.Equal(acknowledgement, []string{scope + ":" + namespace}) {
		t.Fatalf("container recovery acknowledgement %v", acknowledgement)
	}
}
