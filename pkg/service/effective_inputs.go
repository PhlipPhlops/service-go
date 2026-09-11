package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

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
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
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
	plans := map[ciinputs.Key]EffectiveInputPlan{}
	if s.InputPlans != nil {
		plans, err = s.InputPlans(ctx, req)
		if err != nil {
			return nil, err
		}
		inventory := map[ciinputs.Key]bool{}
		for _, key := range keys {
			inventory[key] = true
		}
		for key := range plans {
			if !inventory[key] {
				return nil, status.Error(codes.FailedPrecondition, "plan is outside validation inventory")
			}
		}
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
	production.ctx, tests.ctx = ctx, ctx
	tests.sensitive = production.sensitive
	sensitiveInputs := append([]*agent.EffectiveInput(nil), req.Context...)
	for _, plan := range plans {
		if _, err := ciinputs.Evaluate(nil, &agent.GetEffectiveInputsRequest{SchemaVersion: req.SchemaVersion, Snapshot: req.Snapshot, Context: plan.Inputs}, nil); err != nil {
			return nil, status.Error(codes.FailedPrecondition, "invalid operation input plan")
		}
		sensitiveInputs = append(sensitiveInputs, plan.Inputs...)
	}
	for _, in := range sensitiveInputs {
		if in.Path && in.Sensitive && in.Owner == "workspace" {
			name, err := filepath.EvalSymlinks(filepath.Join(physicalRoot, filepath.FromSlash(in.Name)))
			if err == nil {
				production.sensitive[name] = true
			}
		}
	}
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
	metadata := newInputFiles(root, physicalRoot)
	metadata.ctx, metadata.sensitive = ctx, production.sensitive
	metadata.inputs, metadata.content = maps.Clone(production.inputs), maps.Clone(production.content)
	metadata.incomplete = production.incomplete
	for _, entry := range []struct {
		tests bool
		files *inputFiles
	}{{false, production}, {true, tests}} {
		invocation, err := s.GoInputInvocation(ctx, "", entry.tests)
		if err != nil {
			entry.files.add(unresolved(agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_CONFIGURATION, "task-invocation"))
			continue
		}
		s.listInputs(ctx, invocation, entry.files)
	}
	planFiles := map[ciinputs.Key]*inputFiles{}
	for key, plan := range plans {
		set := newInputFiles(root, physicalRoot)
		set.ctx, set.sensitive = ctx, production.sensitive
		if plan.Invocation != nil {
			set.inputs, set.content = maps.Clone(metadata.inputs), maps.Clone(metadata.content)
			set.incomplete = metadata.incomplete
			s.listInputs(ctx, plan.Invocation, set)
			set.inputs[inputKey(unresolved(agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_ENVIRONMENT, "effective-runtime"))] = unresolved(agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_ENVIRONMENT, "effective-runtime")
		}
		planFiles[key] = set
	}
	for _, set := range append([]*inputFiles{production, tests, metadata}, slices.Collect(maps.Values(planFiles))...) {
		if err := set.resolve(ctx); err != nil {
			return nil, status.FromContextError(err).Err()
		}
	}

	for _, task := range response.Tasks {
		set := metadata
		switch task.Task.Phase {
		case agent.TaskPhase_TASK_PHASE_COMPILE:
			set = production
		case agent.TaskPhase_TASK_PHASE_TEST, agent.TaskPhase_TASK_PHASE_LINT:
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
		key := ciinputs.Key{Phase: task.Task.Phase, Suite: task.Task.Suite}
		if plan, ok := plans[key]; ok {
			task.Inputs = planFiles[key].sorted()
			task.Inputs, err = mergeInputContext(ctx, task.Inputs, plan.Inputs, true)
			if err != nil {
				return nil, err
			}
			task.RuntimeServices = append([]string(nil), plan.RuntimeServices...)
			task.Complete = plan.Complete && !planFiles[key].incomplete
		}
		task.Inputs, err = mergeInputContext(ctx, task.Inputs, req.Context, false)
		if err != nil {
			return nil, err
		}
	}
	if _, err := ciinputs.Evaluate(response, req, keys); err != nil {
		return nil, status.Error(codes.InvalidArgument, "conflicting effective-input context")
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	return response, nil
}

func mergeInputContext(ctx context.Context, observed, supplied []*agent.EffectiveInput, includeNew bool) ([]*agent.EffectiveInput, error) {
	positions := make(map[string]int, len(observed)+len(supplied))
	for i, in := range observed {
		positions[inputKey(in)] = i
	}
	for _, in := range supplied {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		if in == nil {
			return nil, status.Error(codes.InvalidArgument, "invalid effective input")
		}
		key := inputKey(in)
		if i, found := positions[key]; found {
			prior := observed[i]
			if prior.Identity == nil && prior.Path == in.Path && prior.Mode == in.Mode && prior.Sensitive == in.Sensitive {
				observed[i] = proto.Clone(in).(*agent.EffectiveInput)
			} else if !proto.Equal(prior, in) {
				return nil, status.Error(codes.InvalidArgument, "conflicting effective-input context")
			}
		} else if includeNew {
			positions[key] = len(observed)
			observed = append(observed, proto.Clone(in).(*agent.EffectiveInput))
		}
	}
	return observed, nil
}

func unresolved(kind agent.EffectiveInputKind, name string) *agent.EffectiveInput {
	return &agent.EffectiveInput{Kind: kind, Owner: "go-discovery", Name: name, Sensitive: kind == agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_ENVIRONMENT}
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

func (s *Service) listInputs(ctx context.Context, invocation *GoInputInvocation, files *inputFiles) {
	if invocation.Workspace && invocation.Target == "" {
		files.add(unresolved(agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_CONFIGURATION, "workspace-package-expansion"))
		return
	}
	selection, _ := json.Marshal(struct {
		Target           string
		Workspace, Tests bool
	}{invocation.Target, invocation.Workspace, invocation.Tests})
	files.add(&agent.EffectiveInput{Kind: agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_CONFIGURATION, Owner: "go-invocation", Name: "selection", Identity: contentIdentity(selection)})
	output, err := invocation.list(ctx)
	if err != nil {
		files.add(unresolved(agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_SOURCE, "native-discovery"))
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		if ctx.Err() != nil {
			return
		}
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
			if mod.Version != "" && !within(files.root, pkg.Dir) {
				in := &agent.EffectiveInput{Kind: agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_LIBRARY, Owner: "go-modules", Name: mod.Path}
				if mod.Sum != "" {
					in.Identity = &agent.EffectiveIdentity{Kind: agent.EffectiveIdentityKind_EFFECTIVE_IDENTITY_KIND_VERSIONED, Namespace: "go-module-sum/v1", Digest: mod.Version + ":" + mod.Sum}
				}
				files.add(in)
				continue
			}
			files.optional(mod.GoMod, agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_LOCKFILE, false)
			if mod.Dir != "" {
				files.optional(filepath.Join(mod.Dir, "go.sum"), agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_LOCKFILE, false)
			}
		}
		groups := [][]string{pkg.GoFiles, pkg.CgoFiles, pkg.CFiles, pkg.CXXFiles, pkg.MFiles, pkg.HFiles, pkg.FFiles, pkg.SFiles, pkg.SwigFiles, pkg.SwigCXXFiles, pkg.SysoFiles}
		if invocation.Tests && !pkg.DepOnly {
			groups = append(groups, pkg.TestGoFiles, pkg.XTestGoFiles)
			files.tree(filepath.Join(pkg.Dir, "testdata"), agent.EffectiveInputKind_EFFECTIVE_INPUT_KIND_FIXTURE, true)
		}
		embeds := append([]string(nil), pkg.EmbedFiles...)
		if invocation.Tests && !pkg.DepOnly {
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
	ctx        context.Context
	root       string
	alias      string
	inputs     map[string]*agent.EffectiveInput
	sensitive  map[string]bool
	incomplete bool
	content    map[string]string
}

func newInputFiles(alias, root string) *inputFiles {
	return &inputFiles{ctx: context.Background(), root: root, alias: alias, inputs: map[string]*agent.EffectiveInput{}, sensitive: map[string]bool{}, content: map[string]string{}}
}
func inputKey(in *agent.EffectiveInput) string {
	return in.Kind.String() + "\x00" + in.Owner + "\x00" + in.Name
}
func (f *inputFiles) add(in *agent.EffectiveInput) {
	if in.Owner == "go-discovery" && in.Identity == nil {
		f.incomplete = true
	}
	f.inputs[inputKey(in)] = in
}
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
		if canceled := f.ctx.Err(); canceled != nil {
			return canceled
		}
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
	if f.ctx.Err() != nil {
		return
	}
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
	if !utf8.ValidString(rel) || strings.ContainsAny(filepath.ToSlash(rel), "\\\x00\r\n") {
		f.add(unresolved(kind, "unrepresentable-filesystem-path"))
		return
	}
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
		if sensitive {
			f.sensitive[name] = true
		}
		f.content[inputKey(in)] = name
		f.add(in)
	}
}

// Classification precedes hashing so aliases cannot downgrade a sensitive target.
func (f *inputFiles) resolve(ctx context.Context) error {
	for key, name := range f.content {
		if err := ctx.Err(); err != nil {
			return err
		}
		in := f.inputs[key]
		in.Sensitive = in.Sensitive || f.sensitive[name] || strings.HasSuffix(name, ".env") || strings.Contains(filepath.ToSlash(name), "/configurations/")
		if in.Sensitive {
			in.Identity = nil
			continue
		}
		identity, err := fileIdentity(ctx, name)
		if err != nil {
			f.add(unresolved(in.Kind, "filesystem-discovery"))
			continue
		}
		in.Identity = identity
	}
	return ctx.Err()
}

func contentIdentity(content []byte) *agent.EffectiveIdentity {
	digest := sha256.Sum256(content)
	return &agent.EffectiveIdentity{Kind: agent.EffectiveIdentityKind_EFFECTIVE_IDENTITY_KIND_SHA256, Digest: hex.EncodeToString(digest[:])}
}

type inputReader struct {
	context.Context
	io.Reader
}

func (r inputReader) Read(p []byte) (int, error) {
	if err := r.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}
func fileIdentity(ctx context.Context, name string) (*agent.EffectiveIdentity, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err = io.Copy(digest, inputReader{ctx, file}); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return &agent.EffectiveIdentity{Kind: agent.EffectiveIdentityKind_EFFECTIVE_IDENTITY_KIND_SHA256, Digest: hex.EncodeToString(digest.Sum(nil))}, nil
}
