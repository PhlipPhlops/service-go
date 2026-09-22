package builder

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
)

// go build succeeds for a library too, but its ar archive cannot be launched.
func TestPackageRejectsLibraryArchive(t *testing.T) {
	source := t.TempDir()
	mustWrite(t, filepath.Join(source, "go.mod"), "module example.test/library\n\ngo 1.27.0\n", 0600)
	mustWrite(t, filepath.Join(source, "library.go"), "package library\nconst Value = 42\n", 0600)
	destination := filepath.Join(t.TempDir(), "agent")
	err := packageGoBinary(t.Context(), source, destination, &builderv0.PackageTarget{Os: runtime.GOOS, Architecture: runtime.GOARCH})
	if err == nil || !strings.Contains(err.Error(), "package main") {
		t.Fatalf("library must not be packaged as an executable: %v", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("invalid package was installed: %v", err)
	}
}

func TestPackageDeclaredNestedMainUsesEnclosingModule(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "go.mod"), "module example.test/nested\n\ngo 1.27.0\n", 0600)
	mustWrite(t, filepath.Join(root, "library.go"), "package nested\nconst Value = 42\n", 0600)
	source := filepath.Join(root, "cmd", "agent")
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(source, "main.go"), "package main\nimport (\"fmt\"; \"example.test/nested\")\nfunc main() { fmt.Println(nested.Value) }\n", 0600)
	destination := filepath.Join(t.TempDir(), "agent")
	if err := packageGoBinary(t.Context(), source, destination, &builderv0.PackageTarget{Os: runtime.GOOS, Architecture: runtime.GOARCH}); err != nil {
		t.Fatal(err)
	}
	output, err := exec.CommandContext(t.Context(), destination).CombinedOutput()
	if err != nil || string(output) != "42\n" {
		t.Fatalf("nested executable: %v, output=%q", err, output)
	}
}
