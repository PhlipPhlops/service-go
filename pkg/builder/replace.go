package builder

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"
)

// carryDirectory is where a carried filesystem replacement lands inside the
// recipe's code context. The leading underscore keeps the Go tool from walking
// the carried tree as part of the module's own packages ("./..." never descends
// into it), so a carried module contributes exactly what the replace directive
// asks for and nothing else.
const carryDirectory = "_replace"

// repositoryMarkers name the root of the repository that owns a module. A
// codefly module ships as one package from one repository, so its manifest
// marks the same boundary a checkout's .git does — and it still marks it after
// the CLI has resolved the module into its cache, where there is no .git.
var repositoryMarkers = []string{".git", "module.codefly.yaml", "workspace.codefly.yaml"}

// carryLocalReplacements makes the recipe's code context self-contained for a
// module that replaces another module by filesystem path.
//
// A `replace foo => ../../foo/code` directive is resolved relative to the main
// module's directory. The image build context is that directory alone, so the
// replacement is not in the image and the dependency download fails inside the
// builder stage with
//
//	go: foo@v0.0.0 (replaced by ../../foo/code): reading /foo/code/go.mod: no such file or directory
//
// even though the same build succeeds on the developer's machine. Two services
// sharing one Go module in a repository is a layout the recipe must build, not
// one the consumer should have to work around: copy each replacement into the
// context under _replace/ and rewrite the directive to where it landed, so the
// context the builder stage sees resolves exactly what the host resolves.
//
// Containment is the rule: a replacement must resolve inside the repository
// that owns the module. A directive pointing outside it names a path that is
// true only on one machine, and it is reported as an error naming the directive
// rather than skipped — a silently dropped replacement would build an image
// against a different dependency than the developer builds against.
//
// src is the module's own source directory, dst the recipe's code context that
// already holds a copy of it, and output the recipe directory the copy must
// never descend into. A module with no filesystem replacement is left exactly
// as it was copied, down to the bytes of its go.mod.
func carryLocalReplacements(src, dst, output string) error {
	goModPath := filepath.Join(src, "go.mod")
	data, err := os.ReadFile(goModPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	parsed, err := modfile.Parse(goModPath, data, nil)
	if err != nil {
		return fmt.Errorf("parse %s: %w", goModPath, err)
	}
	local := make([]*modfile.Replace, 0, len(parsed.Replace))
	for _, replace := range parsed.Replace {
		if replace.New.Version == "" && isFilesystemPath(replace.New.Path) {
			local = append(local, replace)
		}
	}
	if len(local) == 0 {
		return nil
	}

	moduleRoot, err := filepath.EvalSymlinks(src)
	if err != nil {
		return err
	}
	repository, err := repositoryRoot(moduleRoot)
	if err != nil {
		return fmt.Errorf("%w: %s", err, directive(local[0]))
	}

	carried := false
	for _, replace := range local {
		target := replace.New.Path
		if !filepath.IsAbs(target) {
			target = filepath.Join(moduleRoot, filepath.FromSlash(target))
		}
		resolved, err := filepath.EvalSymlinks(target)
		if err != nil {
			return fmt.Errorf("%s resolves to %s, which does not exist", directive(replace), target)
		}
		if _, err := os.Stat(filepath.Join(resolved, "go.mod")); err != nil {
			return fmt.Errorf("%s resolves to %s, which is not a Go module", directive(replace), resolved)
		}
		if within(moduleRoot, resolved) {
			// Already inside the module's own tree: the copy carried it and
			// the directive resolves unchanged in the context.
			continue
		}
		if within(resolved, moduleRoot) {
			return fmt.Errorf("%s resolves to %s, which contains the module itself: the image build context cannot include it", directive(replace), resolved)
		}
		if !within(repository, resolved) {
			return fmt.Errorf("%s resolves to %s, outside the repository %s that owns the module: the image build context cannot include it", directive(replace), resolved, repository)
		}
		relative, err := filepath.Rel(repository, resolved)
		if err != nil {
			return err
		}
		destination := filepath.Join(dst, carryDirectory, relative)
		if _, err := os.Lstat(destination); err == nil {
			return fmt.Errorf("%s would land on %s, which the module already occupies", directive(replace), filepath.Join(carryDirectory, relative))
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		if err := copyGoContext(resolved, destination, output); err != nil {
			return fmt.Errorf("carry %s into the build context: %w", directive(replace), err)
		}
		if err := parsed.AddReplace(replace.Old.Path, replace.Old.Version,
			"./"+carryDirectory+"/"+filepath.ToSlash(relative), ""); err != nil {
			return err
		}
		carried = true
	}
	if !carried {
		return nil
	}

	parsed.Cleanup()
	rewritten, err := parsed.Format()
	if err != nil {
		return err
	}
	destination := filepath.Join(dst, "go.mod")
	mode := os.FileMode(0o644)
	if info, err := os.Stat(destination); err == nil {
		mode = info.Mode().Perm()
	}
	return os.WriteFile(destination, rewritten, mode)
}

// repositoryRoot walks up from a module's directory to the repository that owns
// it. The walk stops at the first marker rather than the outermost one, so a
// module nested in a workspace of checkouts is bounded by its own checkout.
func repositoryRoot(moduleRoot string) (string, error) {
	for directory := moduleRoot; ; {
		for _, marker := range repositoryMarkers {
			if _, err := os.Lstat(filepath.Join(directory, marker)); err == nil {
				return directory, nil
			}
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf("no repository owns the module at %s, so a filesystem replacement cannot be contained", moduleRoot)
		}
		directory = parent
	}
}

// isFilesystemPath reports whether a replacement target is a directory on this
// machine rather than a module path resolved from a proxy.
func isFilesystemPath(path string) bool {
	return filepath.IsAbs(path) || strings.HasPrefix(path, "./") || strings.HasPrefix(path, "../")
}

// within reports whether path is root or sits under it.
func within(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative == "." || filepath.IsLocal(relative)
}

// directive renders a replace the way the go.mod spells it, so an error names
// the line the reader has to change.
func directive(replace *modfile.Replace) string {
	old := replace.Old.Path
	if replace.Old.Version != "" {
		old += " " + replace.Old.Version
	}
	return fmt.Sprintf("go.mod directive `replace %s => %s`", old, replace.New.Path)
}
