package builder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
)

// Resolve the declared package with Go itself. Cross builds must mount the
// enclosing module, not just a nested main directory with no go.mod or imports.
func resolvePackageSource(ctx context.Context, source string, target *builderv0.PackageTarget) (root, entry string, err error) {
	command := exec.CommandContext(ctx, "go", "list", "-mod=readonly", "-json", ".")
	command.Dir = source
	command.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=1", "GOOS="+target.GetOs(), "GOARCH="+target.GetArchitecture())
	var stderr bytes.Buffer
	command.Stderr = &stderr
	payload, err := command.Output()
	if err != nil {
		return "", "", fmt.Errorf("resolve Go package source: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var pkg struct {
		Name   string
		Dir    string
		Module *struct{ Dir string }
	}
	if err := json.Unmarshal(payload, &pkg); err != nil {
		return "", "", fmt.Errorf("decode Go package source: %w", err)
	}
	if pkg.Name != "main" {
		return "", "", fmt.Errorf("Go executable packaging requires package main, got %q; select the main package source directory", pkg.Name)
	}
	if pkg.Module == nil || !filepath.IsAbs(pkg.Module.Dir) || !filepath.IsAbs(pkg.Dir) {
		return "", "", fmt.Errorf("Go executable packaging requires a module-owned source directory")
	}
	root, err = filepath.EvalSymlinks(pkg.Module.Dir)
	if err != nil {
		return "", "", err
	}
	directory, err := filepath.EvalSymlinks(pkg.Dir)
	if err != nil {
		return "", "", err
	}
	relative, err := filepath.Rel(root, directory)
	if err != nil || !filepath.IsLocal(relative) {
		return "", "", fmt.Errorf("Go main package must stay inside its module root")
	}
	if relative == "." {
		return root, ".", nil
	}
	return root, "./" + filepath.ToSlash(relative), nil
}
