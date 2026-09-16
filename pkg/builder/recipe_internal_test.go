package builder

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCopyGoContextPreservesModes copies a tree with an executable file and a
// nested directory and asserts contents, layout, and permission bits survive —
// the recipe digest covers file mode, so a dropped executable bit would change
// what the CLI builds.
func TestCopyGoContextPreservesModes(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "go.mod"), "module x\n", 0o644)
	mustWrite(t, filepath.Join(src, "scripts", "build.sh"), "#!/bin/sh\n", 0o755)

	output := t.TempDir()
	dst := filepath.Join(output, "code")
	if err := copyGoContext(src, dst, output); err != nil {
		t.Fatalf("copyGoContext: %v", err)
	}

	for rel, wantMode := range map[string]os.FileMode{
		"go.mod":           0o644,
		"scripts/build.sh": 0o755,
	} {
		info, err := os.Stat(filepath.Join(dst, rel))
		if err != nil {
			t.Fatalf("stat %s: %v", rel, err)
		}
		if info.Mode().Perm() != wantMode {
			t.Errorf("%s mode = %o, want %o", rel, info.Mode().Perm(), wantMode)
		}
	}
}

// TestCopyGoContextSkipsOutputDirectory reproduces the source-dir "." layout,
// where the recipe is written inside the tree being copied. The walk must leave
// its own output alone: copying it would nest the recipe inside itself until the
// path outruns the filesystem's name limit.
func TestCopyGoContextSkipsOutputDirectory(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "go.mod"), "module x\n", 0o644)
	mustWrite(t, filepath.Join(src, "internal", "app.go"), "package internal\n", 0o644)

	output := filepath.Join(src, "builder")
	mustWrite(t, filepath.Join(output, "builder", "Dockerfile"), "FROM scratch\n", 0o644)
	dst := filepath.Join(output, "code")

	if err := copyGoContext(src, dst, output); err != nil {
		t.Fatalf("copyGoContext: %v", err)
	}

	for _, rel := range []string{"go.mod", "internal/app.go"} {
		if _, err := os.Stat(filepath.Join(dst, rel)); err != nil {
			t.Errorf("expected %s in the recipe context: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "builder")); !os.IsNotExist(err) {
		t.Errorf("recipe output copied into its own context: %v", err)
	}
}

// TestCopyGoContextCopiesSourceSharingTheOutputName proves the skip is decided
// by directory identity rather than by name: a source directory that happens to
// be called builder is part of the service and must reach the recipe, or the
// inventory would silently ship an image missing code the service builds.
func TestCopyGoContextCopiesSourceSharingTheOutputName(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "builder", "builder.go"), "package builder\n", 0o644)

	output := t.TempDir()
	dst := filepath.Join(output, "code")
	if err := copyGoContext(src, dst, output); err != nil {
		t.Fatalf("copyGoContext: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dst, "builder", "builder.go")); err != nil {
		t.Errorf("source package named like the recipe output was dropped: %v", err)
	}
}

func mustWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
