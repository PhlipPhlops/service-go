package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/core/ciinputs"
	agent "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func (s *Service) GetEffectiveInputs(ctx context.Context, req *agent.GetEffectiveInputsRequest) (*agent.GetEffectiveInputsResponse, error) {
	return s.DiscoverEffectiveInputs(ctx, req, ValidationCapabilities())
}

// DiscoverEffectiveInputs lets specializations retain their own authoritative
// suite inventory. Additional suites remain incomplete until their consumption
// and execution requirements can be established by the specialization.
func (s *Service) DiscoverEffectiveInputs(ctx context.Context, req *agent.GetEffectiveInputsRequest, validation *agent.ValidationCapabilities) (*agent.GetEffectiveInputsResponse, error) {
	if _, err := ciinputs.Evaluate(nil, req, nil); err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid effective-input request")
	}
	keys, err := ciinputs.Required(validation)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "invalid validation inventory")
	}
	needsServices := map[string]bool{}
	for _, suite := range validation.GetTest().GetSuites() {
		needsServices[suite.Name] = suite.DependencyMode != agent.TestDependencyMode_TEST_DEPENDENCY_MODE_NONE
	}
	response := &agent.GetEffectiveInputsResponse{SchemaVersion: ciinputs.Version, Snapshot: req.Snapshot}
	for _, key := range keys {
		response.Tasks = append(response.Tasks, &agent.TaskInputs{Task: &agent.TaskKey{Phase: key.Phase, Suite: key.Suite}})
	}
	// The current agent/settings cannot attest to historical discovery semantics.
	if req.Revision != "" {
		for _, task := range response.Tasks {
			task.Inputs = []*agent.EffectiveInput{unresolved(agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_CONFIGURATION, "historical-discovery")}
		}
		return response, nil
	}
	source, err := s.ResolveSourceLocation(ctx)
	if err != nil {
		for _, task := range response.Tasks {
			task.Inputs = []*agent.EffectiveInput{unresolved(agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_SOURCE, "source-location")}
		}
		return response, nil
	}
	root := s.Location
	if s.Identity != nil && s.Identity.WorkspacePath != "" {
		root = s.Identity.WorkspacePath
	}
	if root == "" {
		root = source
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "invalid workspace location")
	}
	physicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "workspace is unavailable")
	}
	production := newInputFiles(root, physicalRoot)
	tests := newInputFiles(root, physicalRoot)
	for _, set := range []*inputFiles{production, tests} {
		for dir := source; ; dir = filepath.Dir(dir) {
			if !within(root, dir) {
				break
			}
			for _, name := range []string{"go.mod", "go.sum", "go.work", "go.work.sum"} {
				set.optional(filepath.Join(dir, name), agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_LOCKFILE, false)
			}
			for _, name := range []string{"service.codefly.yaml", "workspace.codefly.yaml"} {
				set.optional(filepath.Join(dir, name), agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_CONFIGURATION, true)
			}
			if dir == root || filepath.Dir(dir) == dir {
				break
			}
		}
		set.tree(filepath.Join(s.Location, "configurations"), agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_CONFIGURATION, true)
	}
	// This trace observes native default invocations only. Runner backends, workspace
	// package expansion and request-specific flags need an execution-bound contract.
	if s.ActiveEnv == nil && !s.Settings.WithWorkspace {
		s.listInputs(ctx, source, false, production)
		s.listInputs(ctx, source, true, tests)
	} else {
		production.add(unresolved(agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_TOOLCHAIN, "runner-discovery"))
		tests.add(unresolved(agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_TOOLCHAIN, "runner-discovery"))
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	for _, task := range response.Tasks {
		set := production
		if task.Task.Phase == agent.TaskPhase_TASK_PHASE_TEST || task.Task.Phase == agent.TaskPhase_TASK_PHASE_LINT {
			set = tests
		}
		if task.Task.Phase != agent.TaskPhase_TASK_PHASE_SYNC {
			task.Inputs = set.sorted()
		}

		task.Inputs = append(task.Inputs,
			unresolved(agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_PLUGIN, "resolved-agent"),
			unresolved(agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_TOOLCHAIN, "execution-toolchain"),
			unresolved(agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_ENVIRONMENT, "effective-runtime"),
			unresolved(agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_CONFIGURATION, "task-invocation"),
		)
		switch task.Task.Phase {
		case agent.TaskPhase_TASK_PHASE_TEST:
			task.Inputs = append(task.Inputs, unresolved(agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_FIXTURE, "dynamic-consumption"))
			if needsServices[task.Task.Suite] {
				task.Inputs = append(task.Inputs, unresolved(agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_SERVICE_IMPLEMENTATION, "suite-service-closure"))
			}
		case agent.TaskPhase_TASK_PHASE_ARTIFACT_BUILD, agent.TaskPhase_TASK_PHASE_SOURCE_PACKAGE:
			task.Inputs = append(task.Inputs, unresolved(agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_ARTIFACT, "build-context-and-prerequisites"))
		case agent.TaskPhase_TASK_PHASE_AUDIT:
			task.Inputs = append(task.Inputs, unresolved(agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_EXTERNAL, "vulnerability-database"))
		}
		for _, in := range req.Context {
			if consumesContext(task.Task, in.Kind, needsServices[task.Task.Suite]) {
				found := false
				for i, observed := range task.Inputs {
					if inputKey(observed) != inputKey(in) {
						continue
					}
					found = true
					if observed.Identity == nil && observed.Path == in.Path && observed.Mode == in.Mode && observed.Sensitive == in.Sensitive {
						task.Inputs[i] = proto.Clone(in).(*agent.EffectiveInput)
					} else if !proto.Equal(observed, in) {
						return nil, status.Error(codes.InvalidArgument, "conflicting effective-input context")
					}
					break
				}
				if !found {
					task.Inputs = append(task.Inputs, proto.Clone(in).(*agent.EffectiveInput))
				}
			}
		}
	}
	if _, err := ciinputs.Evaluate(response, req, keys); err != nil {
		return nil, status.Error(codes.InvalidArgument, "conflicting effective-input context")
	}
	return response, nil
}

func consumesContext(task *agent.TaskKey, kind agent.EffectiveInputKind, needsServices bool) bool {
	switch kind {
	case agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_SERVICE_IMPLEMENTATION:
		return task.Phase == agent.TaskPhase_TASK_PHASE_TEST && needsServices
	case agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_FIXTURE:
		return task.Phase == agent.TaskPhase_TASK_PHASE_TEST
	case agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_ARTIFACT, agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_VALIDATION:
		return task.Phase == agent.TaskPhase_TASK_PHASE_ARTIFACT_BUILD || task.Phase == agent.TaskPhase_TASK_PHASE_SOURCE_PACKAGE
	default:
		return task.Phase != agent.TaskPhase_TASK_PHASE_SYNC || kind == agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_PLUGIN || kind == agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_TOOLCHAIN
	}
}

func unresolved(kind agent.EffectiveInputKind, name string) *agent.EffectiveInput {
	return &agent.EffectiveInput{Kind: kind, Owner: "go-discovery", Name: name}
}

type listedModule struct {
	Path, Version, Sum, Dir, GoMod string
	Replace                        *listedModule
}

type listedPackage struct {
	Dir               string
	Standard, DepOnly bool
	Module            *listedModule

	GoFiles, CgoFiles                  []string
	CFiles, CXXFiles, MFiles           []string
	HFiles, FFiles, SFiles             []string
	SwigFiles, SwigCXXFiles, SysoFiles []string
	EmbedFiles                         []string
	TestGoFiles, XTestGoFiles          []string
	TestEmbedFiles, XTestEmbedFiles    []string
}

func (s *Service) listInputs(ctx context.Context, source string, test bool, files *inputFiles) {
	args := []string{"list", "-mod=readonly", "-deps", "-json"}
	if test {
		args = append(args, "-test")
	}
	args = append(args, "./...")
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = source
	cmd.Env = append(os.Environ(), "GOWORK=off")
	output, err := cmd.Output()
	if err != nil {
		files.add(unresolved(agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_SOURCE, "native-discovery"))
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var pkg listedPackage
		if err := decoder.Decode(&pkg); err != nil {
			if err != io.EOF {
				files.add(unresolved(agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_SOURCE, "native-discovery"))
			}
			break
		}
		if pkg.Standard {
			continue
		}
		if mod := pkg.Module; mod != nil {
			if mod.Replace != nil {
				mod = mod.Replace
			}
			if mod.Version != "" {
				in := &agent.EffectiveInput{Kind: agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_LIBRARY, Owner: "go-modules", Name: mod.Path}
				if mod.Sum != "" {
					in.Identity = &agent.EffectiveIdentity{Kind: agent.EffectiveIdentityKind_EFFECTIVE_IDENTITY_KIND_VERSIONED, Namespace: "go-module-sum/v1", Digest: mod.Version + ":" + mod.Sum}
				}
				files.add(in)
				continue
			}
			files.optional(mod.GoMod, agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_LOCKFILE, false)
			files.optional(filepath.Join(mod.Dir, "go.sum"), agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_LOCKFILE, false)
		}
		groups := [][]string{pkg.GoFiles, pkg.CgoFiles, pkg.CFiles, pkg.CXXFiles, pkg.MFiles, pkg.HFiles, pkg.FFiles, pkg.SFiles, pkg.SwigFiles, pkg.SwigCXXFiles, pkg.SysoFiles}
		if test && !pkg.DepOnly {
			groups = append(groups, pkg.TestGoFiles, pkg.XTestGoFiles)
			files.tree(filepath.Join(pkg.Dir, "testdata"), agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_FIXTURE, true)
		}
		embeds := append([]string(nil), pkg.EmbedFiles...)
		if test && !pkg.DepOnly {
			embeds = append(embeds, pkg.TestEmbedFiles...)
			embeds = append(embeds, pkg.XTestEmbedFiles...)
		}
		for _, name := range embeds {
			files.file(filepath.Join(pkg.Dir, name), agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_SOURCE, true)
		}
		for _, group := range groups {
			for _, name := range group {
				if !filepath.IsAbs(name) {
					name = filepath.Join(pkg.Dir, name)
				}
				files.file(name, agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_SOURCE, false)
			}
		}
	}
}

type inputFiles struct {
	root   string
	alias  string
	inputs map[string]*agent.EffectiveInput
}

func newInputFiles(alias, root string) *inputFiles {
	return &inputFiles{root: root, alias: alias, inputs: map[string]*agent.EffectiveInput{}}
}
func inputKey(in *agent.EffectiveInput) string {
	return in.Kind.String() + "\x00" + in.Owner + "\x00" + in.Name
}
func (f *inputFiles) add(in *agent.EffectiveInput) { f.inputs[inputKey(in)] = in }
func (f *inputFiles) sorted() []*agent.EffectiveInput {
	keys := make([]string, 0, len(f.inputs))
	for key := range f.inputs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]*agent.EffectiveInput, 0, len(keys))
	for _, key := range keys {
		result = append(result, proto.Clone(f.inputs[key]).(*agent.EffectiveInput))
	}
	return result
}
func within(root, name string) bool {
	rel, err := filepath.Rel(root, name)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}
func (f *inputFiles) optional(name string, kind agent.EffectiveInputKind, sensitive bool) {
	if name == "" {
		return
	}
	if _, err := os.Lstat(name); os.IsNotExist(err) {
		return
	}
	f.file(name, kind, sensitive)
}
func (f *inputFiles) tree(dir string, kind agent.EffectiveInputKind, sensitive bool) {
	err := filepath.WalkDir(dir, func(name string, entry fs.DirEntry, err error) error {
		if os.IsNotExist(err) && name == dir {
			return nil
		}
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			f.file(name, kind, sensitive)
		}
		return nil
	})
	if err != nil {
		f.add(unresolved(kind, "filesystem-discovery"))
	}
}
func (f *inputFiles) file(name string, kind agent.EffectiveInputKind, sensitive bool) {
	f.follow(name, kind, sensitive, map[string]bool{})
}

func (f *inputFiles) follow(name string, kind agent.EffectiveInputKind, sensitive bool, visiting map[string]bool) {
	name = filepath.Clean(name)
	if within(f.alias, name) {
		rel, _ := filepath.Rel(f.alias, name)
		name = filepath.Join(f.root, rel)
	}
	if !within(f.root, name) {
		f.add(unresolved(kind, "external-filesystem"))
		return
	}
	rel, _ := filepath.Rel(f.root, name)
	// Inspect path components as well as the leaf: directory links consume both
	// their link text and the target file, and must not escape the workspace.
	current := f.root
	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			f.add(unresolved(kind, "filesystem-discovery"))
			return
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(current)
			if err != nil {
				f.add(unresolved(kind, "filesystem-discovery"))
				return
			}
			linkRel, _ := filepath.Rel(f.root, current)
			in := &agent.EffectiveInput{Kind: kind, Owner: "workspace", Name: filepath.ToSlash(linkRel), Path: true, Mode: 0o120000, Sensitive: sensitive}
			if !sensitive {
				in.Identity = contentIdentity([]byte(link))
			}
			key := inputKey(in)
			if visiting[key] {
				f.add(unresolved(kind, "symlink-cycle"))
				return
			}
			visiting[key] = true
			f.add(in)
			target := link
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(current), target)
			}
			f.follow(filepath.Join(append([]string{target}, parts[i+1:]...)...), kind, sensitive, visiting)
			return
		}
		if i != len(parts)-1 {
			continue
		}
		if !info.Mode().IsRegular() {
			f.add(unresolved(kind, "non-regular-input"))
			return
		}
		in := &agent.EffectiveInput{Kind: kind, Owner: "workspace", Name: filepath.ToSlash(rel), Path: true, Mode: 0o100644, Sensitive: sensitive}
		if info.Mode()&0o111 != 0 {
			in.Mode = 0o100755
		}
		if !sensitive {
			content, err := os.ReadFile(name)
			if err != nil {
				f.add(unresolved(kind, "filesystem-discovery"))
				return
			}
			in.Identity = contentIdentity(content)
		}
		f.add(in)
	}
}
func contentIdentity(content []byte) *agent.EffectiveIdentity {
	digest := sha256.Sum256(content)
	return &agent.EffectiveIdentity{Kind: agent.EffectiveIdentityKind_EFFECTIVE_IDENTITY_KIND_SHA256, Digest: hex.EncodeToString(digest[:])}
}
