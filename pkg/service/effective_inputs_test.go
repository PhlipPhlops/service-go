package service_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/ciinputs"
	agent "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	goservice "github.com/codefly-dev/service-go/pkg/service"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

func writeInput(t *testing.T, root, name, content string) {
	t.Helper()
	name = filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func inputService(t *testing.T) (*goservice.Service, string) {
	t.Helper()
	root := t.TempDir()
	for name, content := range map[string]string{
		"service/code/go.mod":                         "module example.com/app\n\ngo 1.27.0\n\nrequire (\n example.com/contest v0.0.0\n example.com/testlib v0.0.0\n)\nreplace example.com/contest => ../../contest\nreplace example.com/testlib => ../../testlib\n",
		"contest/go.mod":                              "module example.com/contest\n\ngo 1.27.0\n",
		"contest/lib.go":                              "package contest\nfunc Value() string { return \"ok\" }\n",
		"testlib/go.mod":                              "module example.com/testlib\n\ngo 1.27.0\n",
		"testlib/lib.go":                              "package testlib\nconst Expected = \"ok\"\n",
		"service/code/main.go":                        "package main\nimport (\n _ \"embed\"\n \"example.com/contest\"\n)\n//go:embed asset.txt\nvar asset string\nfunc main() { println(contest.Value(), asset) }\n",
		"service/code/main_test.go":                   "package main\nimport (\n \"testing\"\n \"os\"\n \"example.com/contest\"\n \"example.com/testlib\"\n)\nfunc TestValue(t *testing.T) { if contest.Value() != testlib.Expected { t.Fatal(\"value\") }; if _, err := os.ReadFile(\"testdata/fixture.txt\"); err != nil { t.Fatal(err) } }\n",
		"service/code/asset.txt":                      "asset\n",
		"service/code/testdata/fixture.txt":           "fixture-secret\n",
		"service/service.codefly.yaml":                "name: sample\n",
		"service/configurations/local/app.secret.env": "TOKEN=low-entropy-secret\n",
	} {
		writeInput(t, root, name, content)
	}
	svc := goservice.New(&resources.Agent{Name: "go", Version: "0.0.45"})
	svc.Location = filepath.Join(root, "service")
	svc.Identity = &resources.ServiceIdentity{WorkspacePath: root}
	svc.SetSourceLocation(filepath.Join(root, "service/code"))
	return svc, root
}

func inputClient(t *testing.T, svc agent.AgentServer) agent.AgentClient {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	agent.RegisterAgentServer(server, svc)
	go func() {
		if err := server.Serve(listener); err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	conn, err := grpc.NewClient("passthrough:///agent", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); server.Stop(); listener.Close() })
	return agent.NewAgentClient(conn)
}

func discoverInputs(t *testing.T, client agent.AgentClient, snapshot string) *agent.GetEffectiveInputsResponse {
	t.Helper()
	response, err := client.GetEffectiveInputs(t.Context(), &agent.GetEffectiveInputsRequest{SchemaVersion: 1, Snapshot: snapshot})
	if err != nil {
		t.Fatal(err)
	}
	if response.Snapshot != snapshot {
		t.Fatalf("snapshot = %q", response.Snapshot)
	}
	return response
}

func inputTask(t *testing.T, response *agent.GetEffectiveInputsResponse, phase agent.TaskPhase) *agent.TaskInputs {
	t.Helper()
	for _, task := range response.Tasks {
		if task.Task.Phase == phase {
			return task
		}
	}
	t.Fatalf("missing phase %v", phase)
	return nil
}

func pathInput(task *agent.TaskInputs, name string) *agent.EffectiveInput {
	for _, in := range task.Inputs {
		if in.Path && in.Name == name {
			return in
		}
	}
	return nil
}

func runNative(t *testing.T, source string, args ...string) {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = source
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go %v: %v\n%s", args, err, out)
	}
}

func TestEffectiveInputsRPCNativeBoundaries(t *testing.T) {
	t.Setenv("GOFLAGS", "")
	svc, root := inputService(t)
	client := inputClient(t, svc)
	runNative(t, svc.CurrentSourceLocation(), "build", "-o", filepath.Join(t.TempDir(), "app"), "./...")
	runNative(t, svc.CurrentSourceLocation(), "test", "./...")
	before := discoverInputs(t, client, "reference")
	production := inputTask(t, before, agent.TaskPhase_TASK_PHASE_COMPILE)
	unit := inputTask(t, before, agent.TaskPhase_TASK_PHASE_TEST)
	for _, name := range []string{"service/code/main.go", "contest/lib.go", "contest/go.mod", "service/code/asset.txt", "service/code/go.mod"} {
		if pathInput(production, name) == nil {
			t.Errorf("production omitted %s: %v", name, production)
		}
	}
	for _, name := range []string{"service/code/main_test.go", "testlib/lib.go", "service/code/testdata/fixture.txt"} {
		if pathInput(production, name) != nil {
			t.Errorf("production consumes test-only %s", name)
		}
		if pathInput(unit, name) == nil {
			t.Errorf("unit omitted %s", name)
		}
	}
	writeInput(t, root, "service/code/main_test.go", "package main\nimport \"testing\"\nfunc TestReplacement(t *testing.T) {}\n")
	runNative(t, svc.CurrentSourceLocation(), "test", "./...")
	after := discoverInputs(t, client, "candidate")
	if !proto.Equal(production, inputTask(t, after, agent.TaskPhase_TASK_PHASE_COMPILE)) {
		t.Fatal("test-only edit changed observed production inputs")
	}
	afterUnit := inputTask(t, after, agent.TaskPhase_TASK_PHASE_TEST)
	if proto.Equal(unit, afterUnit) || pathInput(afterUnit, "testlib/lib.go") != nil {
		t.Fatal("test dependency removal was not independently discovered")
	}
	required, err := ciinputs.Required(goservice.ValidationCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	evaluate := func(response *agent.GetEffectiveInputsResponse) []ciinputs.Task {
		tasks, err := ciinputs.Evaluate(response, &agent.GetEffectiveInputsRequest{SchemaVersion: 1, Snapshot: response.Snapshot}, required)
		if err != nil {
			t.Fatal(err)
		}
		for _, task := range tasks {
			if !task.Conservative || task.CacheEligible || task.Identity != "" {
				t.Fatal("trace alone authorized exclusion/reuse")
			}
		}
		return tasks
	}
	if changed := ciinputs.Changed(evaluate(before), evaluate(after)); len(changed) != len(required) {
		t.Fatalf("incomplete tasks lost from selection: %v", changed)
	}
	info, err := client.GetAgentInformation(t.Context(), &agent.AgentInformationRequest{})
	if err != nil || len(info.GetEffectiveInputsVersions()) != 1 || info.EffectiveInputsVersions[0] != 1 {
		t.Fatalf("advertisement: %v %v", info, err)
	}
	if !proto.Equal(info.Validation, goservice.ValidationCapabilities()) {
		t.Fatal("suite semantics changed")
	}
	if err := os.Rename(filepath.Join(root, "contest/lib.go"), filepath.Join(root, "contest/renamed.go")); err != nil {
		t.Fatal(err)
	}
	renamed := inputTask(t, discoverInputs(t, client, "renamed"), agent.TaskPhase_TASK_PHASE_COMPILE)
	if pathInput(renamed, "contest/lib.go") != nil || pathInput(renamed, "contest/renamed.go") == nil {
		t.Fatal("rename not discovered")
	}
	writeInput(t, root, "contest/extra.go", "package contest\nconst NewDependency = 1\n")
	added := inputTask(t, discoverInputs(t, client, "added"), agent.TaskPhase_TASK_PHASE_COMPILE)
	if pathInput(added, "contest/extra.go") == nil {
		t.Fatal("new file not discovered")
	}
	if err := os.Remove(filepath.Join(root, "contest/extra.go")); err != nil {
		t.Fatal(err)
	}
	deleted := inputTask(t, discoverInputs(t, client, "deleted"), agent.TaskPhase_TASK_PHASE_COMPILE)
	if pathInput(deleted, "contest/extra.go") != nil {
		t.Fatal("deleted file retained")
	}
}

func TestEffectiveInputsSensitiveAndHistoricalRPC(t *testing.T) {
	svc, root := inputService(t)
	client := inputClient(t, svc)
	response := discoverInputs(t, client, "current")
	for _, secret := range []string{"low-entropy-secret", "fixture-secret"} {
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(secret+"\n")))
		if strings.Contains(response.String(), secret) || strings.Contains(response.String(), digest) {
			t.Fatal("secret or plain secret hash leaked")
		}
	}
	for _, task := range response.Tasks {
		if in := pathInput(task, "service/configurations/local/app.secret.env"); in != nil && (!in.Sensitive || in.Identity != nil) {
			t.Fatal("configuration identity is not protected")
		}
	}
	protected, err := ciinputs.Protect([]byte(strings.Repeat("k", 32)), "tests/v1", []byte("TOKEN=low-entropy-secret\n"))
	if err != nil {
		t.Fatal(err)
	}
	in := &agent.EffectiveInput{Kind: agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_CONFIGURATION, Owner: "workspace", Name: "service/configurations/local/app.secret.env", Path: true, Mode: 0o100644, Sensitive: true, Identity: protected}
	req := &agent.GetEffectiveInputsRequest{SchemaVersion: 1, Snapshot: "protected", Context: []*agent.EffectiveInput{in}}
	resolved, err := client.GetEffectiveInputs(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := pathInput(inputTask(t, resolved, agent.TaskPhase_TASK_PHASE_COMPILE), in.Name); !proto.Equal(got, in) {
		t.Fatal("protected context was not retained")
	}
	in.Identity = &agent.EffectiveIdentity{Kind: agent.EffectiveIdentityKind_EFFECTIVE_IDENTITY_KIND_SHA256, Digest: strings.Repeat("0", 64)}
	if _, err := client.GetEffectiveInputs(t.Context(), req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("plain secret hash accepted: %v", err)
	}
	for _, req := range []*agent.GetEffectiveInputsRequest{{SchemaVersion: 2, Snapshot: "bad"}, {SchemaVersion: 1}, {SchemaVersion: 1, Snapshot: "old", Revision: "unavailable"}} {
		got, err := client.GetEffectiveInputs(t.Context(), req)
		if req.Revision == "" {
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("invalid request: %v", err)
			}
			continue
		}
		if err != nil || len(got.Tasks) != len(response.Tasks) {
			t.Fatalf("historical inventory: %v %v", got, err)
		}
		for _, task := range got.Tasks {
			if task.Complete {
				t.Fatal("historical completeness")
			}
			for _, in := range task.Inputs {
				if in.Path {
					t.Fatal("historical request read current files")
				}
			}
		}
	}
	writeInput(t, root, "service/code/main.go", "invalid go source")
	broken := discoverInputs(t, client, "broken")
	if len(broken.Tasks) != len(response.Tasks) {
		t.Fatal("failed native discovery dropped required tasks")
	}
}

func TestEffectiveInputsNativeBuildConstraints(t *testing.T) {
	svc, root := inputService(t)
	writeInput(t, root, "service/code/feature.go", "//go:build feature\n\npackage main\nconst Feature = true\n")
	writeInput(t, root, "service/code/platform_linux.go", "package main\nconst Linux = true\n")
	client := inputClient(t, svc)
	t.Setenv("GOFLAGS", "-tags=feature")
	t.Setenv("GOOS", "linux")
	t.Setenv("GOARCH", "amd64")
	t.Setenv("CGO_ENABLED", "0")
	selected := inputTask(t, discoverInputs(t, client, "linux-feature"), agent.TaskPhase_TASK_PHASE_COMPILE)
	for _, name := range []string{"service/code/feature.go", "service/code/platform_linux.go"} {
		if pathInput(selected, name) == nil {
			t.Fatalf("native constraint omitted %s", name)
		}
	}
	t.Setenv("GOFLAGS", "")
	t.Setenv("GOOS", "darwin")
	other := inputTask(t, discoverInputs(t, client, "darwin"), agent.TaskPhase_TASK_PHASE_COMPILE)
	for _, name := range []string{"service/code/feature.go", "service/code/platform_linux.go"} {
		if pathInput(other, name) != nil {
			t.Fatalf("native constraint retained %s", name)
		}
	}
}

func TestEffectiveInputsSymlinksAndCompilerInputs(t *testing.T) {
	t.Setenv("GOFLAGS", "")
	t.Setenv("CGO_ENABLED", "1")
	svc, root := inputService(t)
	writeInput(t, root, "service/code/native/native.go", "package native\n/*\n#include \"value.h\"\n*/\nimport \"C\"\nfunc Value() int { return int(C.value()) }\n")
	writeInput(t, root, "service/code/native/value.h", "int value(void);\n")
	writeInput(t, root, "service/code/native/value.c", "int value(void) { return 1; }\n")
	writeInput(t, root, "contest/extra.go", "package contest\nconst Extra = 1\n")
	if err := os.Rename(filepath.Join(root, "contest"), filepath.Join(root, "shared")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("shared", filepath.Join(root, "contest")); err != nil {
		t.Fatal(err)
	}
	client := inputClient(t, svc)
	runNative(t, svc.CurrentSourceLocation(), "test", "./...")
	before := inputTask(t, discoverInputs(t, client, "linked"), agent.TaskPhase_TASK_PHASE_COMPILE)
	for _, name := range []string{"contest", "shared/lib.go", "shared/extra.go", "service/code/native/native.go", "service/code/native/value.h", "service/code/native/value.c"} {
		if pathInput(before, name) == nil {
			t.Fatalf("missing compiler/link input %s", name)
		}
	}
	link := pathInput(before, "contest")
	if link.Mode != 0o120000 || link.Identity.GetDigest() != fmt.Sprintf("%x", sha256.Sum256([]byte("shared"))) {
		t.Fatal("link-text identity missing")
	}
	writeInput(t, root, "shared/lib.go", "package contest\nfunc Value() string { return \"changed\" }\n")
	after := inputTask(t, discoverInputs(t, client, "changed-library"), agent.TaskPhase_TASK_PHASE_COMPILE)
	if proto.Equal(pathInput(before, "shared/lib.go"), pathInput(after, "shared/lib.go")) {
		t.Fatal("consumed link target identity unchanged")
	}
	if err := os.Remove(filepath.Join(root, "contest")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("./shared", filepath.Join(root, "contest")); err != nil {
		t.Fatal(err)
	}
	relinked := inputTask(t, discoverInputs(t, client, "relinked"), agent.TaskPhase_TASK_PHASE_COMPILE)
	if proto.Equal(pathInput(after, "contest"), pathInput(relinked, "contest")) {
		t.Fatal("link text change lost")
	}
	t.Setenv("CGO_ENABLED", "0")
	without := inputTask(t, discoverInputs(t, client, "no-cgo"), agent.TaskPhase_TASK_PHASE_COMPILE)
	if pathInput(without, "service/code/native/value.c") != nil {
		t.Fatal("CGO-disabled trace retained C source")
	}
}

type inputSpecialization struct {
	*goservice.Service
	validation *agent.ValidationCapabilities
}

func (s *inputSpecialization) GetEffectiveInputs(ctx context.Context, req *agent.GetEffectiveInputsRequest) (*agent.GetEffectiveInputsResponse, error) {
	return s.DiscoverEffectiveInputs(ctx, req, s.validation)
}

func TestEffectiveInputsSuiteContextAndCompatibility(t *testing.T) {
	svc, _ := inputService(t)
	validation := goservice.ValidationCapabilities()
	validation.Test.Suites = []*agent.TestSuiteCapability{
		{Name: "unit", DependencyMode: agent.TestDependencyMode_TEST_DEPENDENCY_MODE_START_DEPENDENCIES},
		{Name: "pure", DependencyMode: agent.TestDependencyMode_TEST_DEPENDENCY_MODE_NONE},
	}
	client := inputClient(t, &inputSpecialization{Service: svc, validation: validation})
	req := &agent.GetEffectiveInputsRequest{SchemaVersion: 1, Snapshot: "context"}
	for _, kind := range []agent.EffectiveInputKind{
		agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_SERVICE_IMPLEMENTATION,
		agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_GENERATED_CONTRACT,
		agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_LIBRARY,
		agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_TOOLCHAIN,
		agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_PLUGIN,
		agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_FIXTURE,
		agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_ARTIFACT,
		agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_VALIDATION,
		agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_GENERATOR,
	} {
		req.Context = append(req.Context, &agent.EffectiveInput{Kind: kind, Owner: "resolved", Name: kind.String(), Identity: &agent.EffectiveIdentity{Kind: agent.EffectiveIdentityKind_EFFECTIVE_IDENTITY_KIND_VERSIONED, Namespace: "test/v1", Digest: "immutable-1"}})
	}
	before, err := client.GetEffectiveInputs(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range before.Tasks {
		for _, supplied := range req.Context {
			present := false
			for _, in := range task.Inputs {
				if proto.Equal(in, supplied) {
					present = true
				}
			}
			expected := task.Task.Phase != agent.TaskPhase_TASK_PHASE_SYNC || supplied.Kind == agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_PLUGIN || supplied.Kind == agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_TOOLCHAIN
			switch supplied.Kind {
			case agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_SERVICE_IMPLEMENTATION:
				expected = task.Task.Suite == "unit"
			case agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_FIXTURE:
				expected = task.Task.Phase == agent.TaskPhase_TASK_PHASE_TEST
			case agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_ARTIFACT, agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_VALIDATION:
				expected = task.Task.Phase == agent.TaskPhase_TASK_PHASE_ARTIFACT_BUILD || task.Task.Phase == agent.TaskPhase_TASK_PHASE_SOURCE_PACKAGE
			}
			if present != expected {
				t.Fatalf("%v / %v consumption = %v", task.Task, supplied.Kind, present)
			}
		}
	}
	req.Snapshot = "api-change"
	req.Context[0].Identity.Digest = "immutable-2"
	after, err := client.GetEffectiveInputs(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	for i, task := range before.Tasks {
		if proto.Equal(task, after.Tasks[i]) == (task.Task.Suite == "unit") {
			t.Fatalf("API change affected wrong task: %v", task.Task)
		}
	}
	legacy := inputClient(t, &agent.UnimplementedAgentServer{})
	required, err := ciinputs.Required(validation)
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range []*agent.AgentInformation{{}, {EffectiveInputsVersions: []uint32{1}}} {
		tasks, err := ciinputs.Discover(t.Context(), legacy, info, req, required)
		if err != nil || len(tasks) != len(required) {
			t.Fatalf("legacy fallback: %v %v", tasks, err)
		}
		for _, task := range tasks {
			if !task.Conservative || task.CacheEligible {
				t.Fatal("legacy reuse authorized")
			}
		}
	}
	svc.Settings.WithWorkspace = true
	unsupported := discoverInputs(t, inputClient(t, svc), "workspace")
	for _, task := range unsupported.Tasks {
		if task.Complete {
			t.Fatal("workspace discovery asserted completeness")
		}
	}
}

func TestEffectiveInputsConfigurationAndLockfileChanges(t *testing.T) {
	svc, root := inputService(t)
	client := inputClient(t, svc)
	writeInput(t, root, "service/code/go.sum", "")
	before := discoverInputs(t, client, "before-config")
	writeInput(t, root, "service/code/go.sum", "\n")
	writeInput(t, root, "service/code/testdata/fixture.txt", "different-secret\n")
	writeInput(t, root, "service/configurations/local/app.secret.env", "TOKEN=rotated-secret\n")
	after := discoverInputs(t, client, "after-config")
	beforeCompile := inputTask(t, before, agent.TaskPhase_TASK_PHASE_COMPILE)
	afterCompile := inputTask(t, after, agent.TaskPhase_TASK_PHASE_COMPILE)
	if proto.Equal(pathInput(beforeCompile, "service/code/go.sum"), pathInput(afterCompile, "service/code/go.sum")) {
		t.Fatal("lockfile edit was not observed")
	}
	required, err := ciinputs.Required(goservice.ValidationCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := ciinputs.Evaluate(after, &agent.GetEffectiveInputsRequest{SchemaVersion: 1, Snapshot: after.Snapshot}, required)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if !task.Conservative {
			t.Fatal("unresolved configuration change authorized reuse")
		}
	}
	if strings.Contains(after.String(), "rotated-secret") || strings.Contains(after.String(), "different-secret") {
		t.Fatal("changed secret leaked")
	}
	req := &agent.GetEffectiveInputsRequest{SchemaVersion: 1, Snapshot: "toolchain", Context: []*agent.EffectiveInput{{Kind: agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_TOOLCHAIN, Owner: "runner", Name: "compiler", Identity: &agent.EffectiveIdentity{Kind: agent.EffectiveIdentityKind_EFFECTIVE_IDENTITY_KIND_VERSIONED, Namespace: "compiler/v1", Digest: "immutable-1"}}}}
	first, err := client.GetEffectiveInputs(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	req.Snapshot = "new-toolchain"
	req.Context[0].Identity.Digest = "immutable-2"
	second, err := client.GetEffectiveInputs(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	for i, task := range first.Tasks {
		if proto.Equal(task, second.Tasks[i]) {
			t.Fatalf("toolchain change omitted from %v", task.Task)
		}
	}
}
