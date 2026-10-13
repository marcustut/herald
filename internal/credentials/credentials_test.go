package credentials

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathUsesConfigDirEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(DirEnv, dir)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, FileName); path != want {
		t.Fatalf("Path() = %q, want %q", path, want)
	}
}

func TestSaveCreatesPrivateFileAndLoadsBack(t *testing.T) {
	t.Setenv(DirEnv, filepath.Join(t.TempDir(), "herald"))
	path, err := Save("herald_axis_secret")
	if err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("credential file mode = %o, want 0600", perm)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Fatalf("credential dir mode = %o, want 0700", perm)
	}

	got, err := Token()
	if err != nil {
		t.Fatal(err)
	}
	if got != "herald_axis_secret" {
		t.Fatalf("Token() = %q", got)
	}
}

// A file left world-readable by an earlier tool must be tightened on rewrite.
func TestSaveForcesExistingFileMode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(DirEnv, dir)
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte("HERALD_TOKEN=old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Save("herald_axis_new"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %o, want 0600", perm)
	}
}

func TestTokenMissingFileIsNotAnError(t *testing.T) {
	t.Setenv(DirEnv, t.TempDir())
	got, err := Token()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("Token() = %q, want empty", got)
	}
}

func TestParseIgnoresCommentsAndOtherKeys(t *testing.T) {
	content := strings.Join([]string{
		"# a comment",
		"",
		"HERALD_GATEWAY=https://gw.example.com",
		`HERALD_TOKEN="herald_axis_quoted"`,
		"nonsense",
	}, "\n")
	if got := parse(content, TokenKey); got != "herald_axis_quoted" {
		t.Fatalf("parse = %q", got)
	}
}
