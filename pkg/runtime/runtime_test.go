package runtime_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codefly-dev/core/agents/helpers/code"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	golanghelpers "github.com/codefly-dev/core/runners/golang"
	selectioncontract "github.com/codefly-dev/core/runners/testselection"

	goruntime "github.com/codefly-dev/service-go/pkg/runtime"
	goservice "github.com/codefly-dev/service-go/pkg/service"
)

func TestRuntimeClassifiesMalformedModuleAsEnvironmentBlock(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("modle example.com/broken\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken_test.go"), []byte("package broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := goservice.New(&resources.Agent{Kind: "codefly:service", Name: "go"})
	svc.SourceLocation = dir
	runner, err := golanghelpers.NewNativeGoRunner(context.Background(), dir, ".")
	if err != nil {
		t.Fatalf("new native runner: %v", err)
	}
	runner.WithWorkspace(false)
	runtime := goruntime.New(svc)
	runtime.RunnerEnvironment = runner

	response, err := runtime.Test(context.Background(), &runtimev0.TestRequest{})
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if response.GetResult().GetState() != runtimev0.TestRunResult_ERRORED ||
		!strings.Contains(response.GetResult().GetMessage(), "env-blocked") ||
		response.GetCounts().GetTotal() != 0 || response.GetCounts().GetFailed() != 0 {
		t.Fatalf("malformed module response = state=%s message=%q counts=%+v, want ERRORED/env-blocked with zero cases", response.GetResult().GetState(), response.GetResult().GetMessage(), response.GetCounts())
	}
}

// TestRuntimeEmbedsService verifies the embedding chain:
//
//	runtime.Runtime → *service.Service → *services.Base
//
// Specializations rely on this chain to inherit Wool, Logger, Location,
// Identity, etc. via method promotion. If embedding is replaced with a
// named field this test breaks loudly.
func TestRuntimeEmbedsService(t *testing.T) {
	svc := goservice.New(&resources.Agent{Kind: "codefly:service", Name: "go"})
	rt := goruntime.New(svc)

	if rt == nil {
		t.Fatal("New returned nil")
	}
	if rt.Service != svc {
		t.Error("embedded Service is not the same pointer passed to New")
	}
	// Promoted fields from *services.Base must be reachable on *Runtime.
	// If these compile, the chain is intact.
	_ = rt.Base
	_ = rt.Settings
	_ = rt.Runtime
}

// TestRuntimeImageIsExported ensures the default runtime image is exported
// so specializations can override or reference it.
func TestRuntimeImageIsExported(t *testing.T) {
	if goruntime.RuntimeImage == nil {
		t.Fatal("RuntimeImage is nil")
	}
	if goruntime.RuntimeImage.Name == "" {
		t.Error("RuntimeImage.Name is empty")
	}
}

func TestEventHandlerRequestsCorrectLifecycleStage(t *testing.T) {
	svc := goservice.New(&resources.Agent{Kind: "codefly:service", Name: "go"})
	rt := goruntime.New(svc)

	if err := rt.EventHandler(code.Change{Path: "code/main.go", IsRelative: true}); err != nil {
		t.Fatalf("go change: %v", err)
	}
	if got := rt.Runtime.DesiredState.GetStage(); got != runtimev0.DesiredState_START {
		t.Fatalf("go change stage = %s, want START", got)
	}

	if err := rt.EventHandler(code.Change{Path: "service.codefly.yaml", IsRelative: true}); err != nil {
		t.Fatalf("service config change: %v", err)
	}
	if got := rt.Runtime.DesiredState.GetStage(); got != runtimev0.DesiredState_LOAD {
		t.Fatalf("service config change stage = %s, want LOAD", got)
	}
}

// TestInitRecordsFixtureAndOverrides pins the values a service under test
// observes. Its Start is a no-op sequencing barrier under
// TEST_DEPENDENCY_MODE_START_DEPENDENCIES and never runs at all under NONE, so a
// process only ever sees these if Init records them.
func TestInitRecordsFixtureAndOverrides(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/init\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := goservice.New(&resources.Agent{Kind: "codefly:service", Name: "go"})
	svc.SourceLocation = dir
	runner, err := golanghelpers.NewNativeGoRunner(context.Background(), dir, ".")
	if err != nil {
		t.Fatalf("new native runner: %v", err)
	}
	runner.WithWorkspace(false)
	rt := goruntime.New(svc)
	rt.RunnerEnvironment = runner

	if _, err := rt.Init(context.Background(), &runtimev0.InitRequest{
		ProposedNetworkMappings: []*basev0.NetworkMapping{{
			Endpoint:  &basev0.Endpoint{Module: "example", Service: "worker", Name: "http", Api: "http"},
			Instances: []*basev0.NetworkInstance{{Address: "http://127.0.0.1:12345", Access: resources.NewNativeNetworkAccess()}},
		}},
		Fixture:   "dev-admin",
		Overrides: map[string]string{"CODEFLY__API_CONSUMES": "billing"},
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	envs, err := rt.EnvironmentVariables.All()
	if err != nil {
		t.Fatalf("environment variables: %v", err)
	}
	if got := envValue(t, envs, "CODEFLY__ENDPOINT__EXAMPLE__WORKER__HTTP__HTTP"); got != "http://127.0.0.1:12345" {
		t.Errorf("own endpoint = %q; declared address must reach the user process", got)
	}
	if got := envValue(t, envs, "CODEFLY__FIXTURE"); got != "dev-admin" {
		t.Errorf("CODEFLY__FIXTURE = %q, want the InitRequest selection", got)
	}
	if got := envValue(t, envs, "CODEFLY__API_CONSUMES"); got != "billing" {
		t.Errorf("CODEFLY__API_CONSUMES = %q, want the InitRequest override", got)
	}

	// An agent process is reused across invocations, so Init is authoritative:
	// a second invocation selecting nothing must not keep serving the first
	// one's values.
	if _, err := rt.Init(context.Background(), &runtimev0.InitRequest{}); err != nil {
		t.Fatalf("second Init: %v", err)
	}
	envs, err = rt.EnvironmentVariables.All()
	if err != nil {
		t.Fatalf("environment variables: %v", err)
	}
	for _, key := range []string{"CODEFLY__FIXTURE", "CODEFLY__API_CONSUMES"} {
		if got := envValue(t, envs, key); got != "" {
			t.Errorf("%s = %q after an invocation carrying none", key, got)
		}
	}
}

// envValue returns the single value recorded for key, failing when a key was
// recorded twice: a process holding two entries for one variable resolves it by
// whichever the exec layer happens to keep.
func envValue(t *testing.T, envs []*resources.EnvironmentVariable, key string) string {
	t.Helper()
	var found []string
	for _, env := range envs {
		if env.Key == key {
			found = append(found, env.ValueAsString())
		}
	}
	if len(found) > 1 {
		t.Fatalf("%s recorded %d times: %v", key, len(found), found)
	}
	if len(found) == 0 {
		return ""
	}
	return found[0]
}

func TestRuntimeHonorsTypedSelectionWithStructuredResult(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/selection\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testSource := `package selection

import "testing"

func TestSelectedFailure(t *testing.T) { t.Fatal("selected") }
func TestUnselectedPass(t *testing.T) {}
`
	if err := os.WriteFile(filepath.Join(dir, "selection_test.go"), []byte(testSource), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := goservice.New(&resources.Agent{Kind: "codefly:service", Name: "go"})
	svc.SourceLocation = dir
	rt := goruntime.New(svc)
	req := &runtimev0.TestRequest{
		Selection: &runtimev0.TestSelection{Scope: &runtimev0.TestSelection_TestCase{TestCase: &runtimev0.TestCaseSelection{
			Package:       ".",
			Path:          "selection_test.go",
			QualifiedName: []string{"TestSelectedFailure"},
		}}},
		SelectionId: "go-selected-case",
	}
	resp, err := rt.Test(context.Background(), req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if err := selectioncontract.VerifyAcknowledgement(req, resp); err != nil {
		t.Fatalf("selection acknowledgement: %v", err)
	}
	if resp.GetResult().GetState() != runtimev0.TestRunResult_FAILED || resp.GetCounts().GetTotal() != 1 || resp.GetCounts().GetFailed() != 1 {
		t.Fatalf("selected result = %s counts=%+v, want only one failing case", resp.GetResult().GetState(), resp.GetCounts())
	}
}

func TestRuntimePropagatesFailFastToNativeRunner(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/failfast\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testSource := `package failfast

import "testing"

func TestFirstFailure(t *testing.T) { t.Fatal("first") }
func TestSecondFailure(t *testing.T) { t.Fatal("second") }
`
	if err := os.WriteFile(filepath.Join(dir, "failfast_test.go"), []byte(testSource), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := goservice.New(&resources.Agent{Kind: "codefly:service", Name: "go"})
	svc.SourceLocation = dir
	rt := goruntime.New(svc)
	resp, err := rt.Test(context.Background(), &runtimev0.TestRequest{
		Formula:  &runtimev0.TestFormula{Command: []string{"go", "test", "-json", "./..."}},
		FailFast: true,
	})
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.GetCounts().GetFailed() != 1 || resp.GetCounts().GetTotal() != 1 {
		t.Fatalf("fail-fast counts = %+v, want only the first failing test", resp.GetCounts())
	}
}

func TestRuntimeTestExecutesEveryInvocation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	t.Cleanup(server.Close)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/cacheproof\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testSource := fmt.Sprintf(`package cacheproof

import (
	"net/http"
	"testing"
)

func TestInvocationReachesCounter(t *testing.T) {
	response, err := http.Get(%q)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
}
`, server.URL)
	if err := os.WriteFile(filepath.Join(dir, "cacheproof_test.go"), []byte(testSource), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := goservice.New(&resources.Agent{Kind: "codefly:service", Name: "go"})
	svc.SourceLocation = dir
	rt := goruntime.New(svc)
	request := &runtimev0.TestRequest{
		Formula: &runtimev0.TestFormula{Command: []string{"go", "test", "-json", "./..."}},
	}
	for attempt := 1; attempt <= 2; attempt++ {
		response, err := rt.Test(context.Background(), request)
		if err != nil {
			t.Fatalf("Test attempt %d: %v", attempt, err)
		}
		if response.GetResult().GetState() != runtimev0.TestRunResult_PASSED || response.GetCounts().GetPassed() != 1 {
			t.Fatalf("attempt %d state = %s counts=%+v", attempt, response.GetResult().GetState(), response.GetCounts())
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("formula test binary invocation count = %d, want 2; Runtime.Test reused a successful Go cache result", got)
	}

	runner, err := golanghelpers.NewNativeGoRunner(context.Background(), dir, ".")
	if err != nil {
		t.Fatalf("new native runner: %v", err)
	}
	runner.WithWorkspace(false)
	rt.RunnerEnvironment = runner
	for attempt := 1; attempt <= 2; attempt++ {
		response, err := rt.Test(context.Background(), &runtimev0.TestRequest{})
		if err != nil {
			t.Fatalf("default Test attempt %d: %v", attempt, err)
		}
		if response.GetResult().GetState() != runtimev0.TestRunResult_PASSED || response.GetCounts().GetPassed() != 1 {
			t.Fatalf("default attempt %d state = %s counts=%+v", attempt, response.GetResult().GetState(), response.GetCounts())
		}
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("combined test binary invocation count = %d, want 4; Runtime.Test reused a successful Go cache result", got)
	}
}
