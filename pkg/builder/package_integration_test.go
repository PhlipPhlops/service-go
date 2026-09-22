package builder

import (
	"context"
	"debug/buildinfo"
	"debug/elf"
	"debug/macho"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/runners/recoveryscope"
)

// This fixture requires both the current Go language version and a real C
// compiler. A pure-Go binary or metadata-only package test misses both failures.
func TestPackageRealCGO(t *testing.T) {
	if os.Getenv("SERVICE_GO_PACKAGE_TESTS") != "required" {
		t.Skip("set SERVICE_GO_PACKAGE_TESTS=required for real native/cross packaging")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "docker", "info").CombinedOutput(); err != nil {
		t.Fatalf("packaging requires Docker: %v: %s", err, output)
	}
	root := t.TempDir()
	scope, err := dockerrun.NewContainerRecoveryScope(root, root, t.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(recoveryscope.EnvironmentVariable, os.Getenv(recoveryscope.EnvironmentVariable))
	if err := dockerrun.SetContainerRecoveryScope(scope); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"go.mod":            "module example.com/cgo-package-regression\n\ngo 1.27.0\n",
		"value.go":          "package regression\nconst Value = 42\n",
		"cmd/agent/main.go": "package main\n/*\nstatic int answer(void) { return 42; }\n*/\nimport \"C\"\nimport (\"fmt\"; regression \"example.com/cgo-package-regression\")\nfunc main() { if int(C.answer()) != regression.Value { panic(\"wrong C result\") }; fmt.Println(regression.Value) }\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(source, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	targets := []*builderv0.PackageTarget{
		{Os: runtime.GOOS, Architecture: runtime.GOARCH},
		{Os: "linux", Architecture: "amd64"},
		{Os: "darwin", Architecture: "arm64"},
	}
	seen := map[string]bool{}
	for _, target := range targets {
		identity := target.GetOs() + "/" + target.GetArchitecture()
		if seen[identity] {
			continue
		}
		seen[identity] = true
		t.Run(identity, func(t *testing.T) {
			destination := filepath.Join(root, strings.ReplaceAll(identity, "/", "-"))
			if err := packageGoBinary(ctx, filepath.Join(source, "cmd", "agent"), destination, target); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(destination)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm()&0111 == 0 {
				t.Fatal("package is not executable")
			}
			build, err := buildinfo.ReadFile(destination)
			if err != nil {
				t.Fatal(err)
			}
			settings := map[string]string{}
			for _, setting := range build.Settings {
				settings[setting.Key] = setting.Value
			}
			for key, want := range map[string]string{"CGO_ENABLED": "1", "GOOS": target.GetOs(), "GOARCH": target.GetArchitecture()} {
				if settings[key] != want {
					t.Errorf("%s=%q, want %q", key, settings[key], want)
				}
			}
			switch identity {
			case "linux/amd64":
				binary, err := elf.Open(destination)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := binary.Close(); err != nil {
						t.Error(err)
					}
				})
				if binary.Machine != elf.EM_X86_64 {
					t.Fatalf("unexpected ELF machine %s", binary.Machine)
				}
			case "darwin/arm64":
				binary, err := macho.Open(destination)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := binary.Close(); err != nil {
						t.Error(err)
					}
				})
				if binary.Cpu != macho.CpuArm64 {
					t.Fatalf("unexpected Mach-O CPU %s", binary.Cpu)
				}
			}
			if target.GetOs() == runtime.GOOS && target.GetArchitecture() == runtime.GOARCH {
				output, err := exec.CommandContext(ctx, destination).CombinedOutput()
				if err != nil || string(output) != "42\n" {
					t.Fatalf("native C call: %v, output %q", err, output)
				}
			}
		})
	}
}
