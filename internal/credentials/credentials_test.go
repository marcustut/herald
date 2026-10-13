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

// TestSaveRoundTrip pins the TOML format and that Save/Load agree.
func TestSaveRoundTrip(t *testing.T) {
	t.Setenv(DirEnv, filepath.Join(t.TempDir(), "herald"))
	path, err := Save("https://gw.example.com", "axis", "herald_axis_secret")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "# Herald credentials.") ||
		!strings.Contains(string(data), `[[credential]]`) ||
		!strings.Contains(string(data), `project = "axis"`) {
		t.Fatalf("credential file is not the documented TOML:\n%s", data)
	}

	f, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Credentials) != 1 || f.Credentials[0].Project != "axis" || f.Credentials[0].Token != "herald_axis_secret" {
		t.Fatalf("Load() = %+v", f)
	}
	got, err := Lookup("https://gw.example.com", "axis")
	if err != nil {
		t.Fatal(err)
	}
	if got != "herald_axis_secret" {
		t.Fatalf("Lookup() = %q", got)
	}
}

// TestSaveUpsertPreservesOtherEntries is the multi-project guarantee: a second
// login must not clobber the first project's token.
func TestSaveUpsertPreservesOtherEntries(t *testing.T) {
	t.Setenv(DirEnv, t.TempDir())
	if _, err := Save("https://gw.example.com", "axis", "tok_axis"); err != nil {
		t.Fatal(err)
	}
	if _, err := Save("https://gw.example.com", "herald", "tok_herald"); err != nil {
		t.Fatal(err)
	}
	// Re-saving axis updates in place rather than appending a duplicate.
	if _, err := Save("https://gw.example.com", "axis", "tok_axis2"); err != nil {
		t.Fatal(err)
	}

	f, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Credentials) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(f.Credentials), f.Credentials)
	}
	for _, want := range []struct{ project, token string }{
		{"axis", "tok_axis2"},
		{"herald", "tok_herald"},
	} {
		got, err := Lookup("https://gw.example.com", want.project)
		if err != nil {
			t.Fatal(err)
		}
		if got != want.token {
			t.Fatalf("Lookup(%s) = %q, want %q", want.project, got, want.token)
		}
	}
}

// TestLookupPrefersProjectEntryOverDefault checks default_token is only a
// fallback for a gateway+project with no dedicated entry.
func TestLookupPrefersProjectEntryOverDefault(t *testing.T) {
	t.Setenv(DirEnv, t.TempDir())
	if _, err := Save("", "", "tok_default"); err != nil {
		t.Fatal(err)
	}
	if _, err := Save("https://gw.example.com", "axis", "tok_axis"); err != nil {
		t.Fatal(err)
	}

	if got, _ := Lookup("https://gw.example.com", "axis"); got != "tok_axis" {
		t.Fatalf("project entry = %q, want tok_axis", got)
	}
	if got, _ := Lookup("https://gw.example.com", "herald"); got != "tok_default" {
		t.Fatalf("fallback = %q, want tok_default", got)
	}
}

func TestGatewayTrailingSlashNormalization(t *testing.T) {
	t.Setenv(DirEnv, t.TempDir())
	if _, err := Save("https://gw.example.com/", "axis", "tok_axis"); err != nil {
		t.Fatal(err)
	}
	for _, gw := range []string{"https://gw.example.com", "https://gw.example.com/"} {
		got, err := Lookup(gw, "axis")
		if err != nil {
			t.Fatal(err)
		}
		if got != "tok_axis" {
			t.Fatalf("Lookup(%q) = %q, want tok_axis", gw, got)
		}
	}
	// Saving with the slash-less form must update, not duplicate.
	if _, err := Save("https://gw.example.com", "axis", "tok_axis2"); err != nil {
		t.Fatal(err)
	}
	f, _ := Load()
	if len(f.Credentials) != 1 {
		t.Fatalf("got %d entries, want 1: %+v", len(f.Credentials), f.Credentials)
	}
}

func TestSaveCreatesPrivateFileAndDir(t *testing.T) {
	t.Setenv(DirEnv, filepath.Join(t.TempDir(), "herald"))
	path, err := Save("https://gw.example.com", "axis", "herald_axis_secret")
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
}

// A file left world-readable by an earlier tool must be tightened on rewrite.
func TestSaveForcesExistingFileMode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(DirEnv, dir)
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte("HERALD_TOKEN=old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Save("https://gw.example.com", "axis", "herald_axis_new"); err != nil {
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

// TestLegacyTokenFileReadAsDefault covers the old single-line format, including
// a quoted value, and that the next Save converts the file to TOML.
func TestLegacyTokenFileReadAsDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(DirEnv, dir)
	path := filepath.Join(dir, FileName)
	legacy := "# herald credentials. Keep this file private (chmod 0600).\n" +
		"HERALD_TOKEN=herald_legacy_secret\n"
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Lookup("https://gw.example.com", "axis")
	if err != nil {
		t.Fatalf("legacy file must not error: %v", err)
	}
	if got != "herald_legacy_secret" {
		t.Fatalf("Lookup() = %q, want the legacy default", got)
	}

	if _, err := Save("https://gw.example.com", "axis", "herald_axis_new"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "HERALD_TOKEN=") || !strings.Contains(string(data), "default_token") {
		t.Fatalf("Save did not rewrite the legacy file as TOML:\n%s", data)
	}
	f, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if f.DefaultToken != "herald_legacy_secret" {
		t.Fatalf("legacy default lost on rewrite: %+v", f)
	}
}

func TestLegacyQuotedToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(DirEnv, dir)
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte("HERALD_TOKEN=\"herald_quoted\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := Lookup("", ""); err != nil || got != "herald_quoted" {
		t.Fatalf("Lookup() = %q, %v; want herald_quoted", got, err)
	}
}

// TestSaveEmptyProjectSetsDefault is how `herald login` stores a token when no
// project could be resolved.
func TestSaveEmptyProjectSetsDefault(t *testing.T) {
	t.Setenv(DirEnv, t.TempDir())
	if _, err := Save("https://gw.example.com", "", "tok_default"); err != nil {
		t.Fatal(err)
	}
	f, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Credentials) != 0 {
		t.Fatalf("empty project must not create an entry: %+v", f.Credentials)
	}
	if f.DefaultToken != "tok_default" {
		t.Fatalf("default_token = %q, want tok_default", f.DefaultToken)
	}
	if got, _ := Lookup("https://gw.example.com", "axis"); got != "tok_default" {
		t.Fatalf("Lookup() = %q, want the default", got)
	}
}

func TestLookupMissingFileIsNotAnError(t *testing.T) {
	t.Setenv(DirEnv, t.TempDir())
	got, err := Lookup("https://gw.example.com", "axis")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("Lookup() = %q, want empty", got)
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
