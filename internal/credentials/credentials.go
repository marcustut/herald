// Package credentials stores herald bearer tokens in a per-user file, so a shell
// or agent session can find them without hand-exporting HERALD_TOKEN. Tokens are
// keyed by (gateway, project) with a default fallback, so several projects and
// gateways coexist. The file is written 0600 and is the fallback between the
// process environment and herald.toml: process env > credential file > herald.toml.
package credentials

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// DirEnv overrides the directory that holds the credential file. It is the
// single knob tests and unusual setups need; an unset value uses the OS user
// config dir.
const DirEnv = "HERALD_CONFIG_DIR"

// TokenKey is the variable name the legacy credential file used, matching the
// environment variable it stood in for.
const TokenKey = "HERALD_TOKEN"

// FileName is the credential file inside the config directory.
const FileName = "credentials"

// header is written above the TOML so a reader knows the file is secret and what
// precedence it follows.
const header = "# Herald credentials. Keep this file private (chmod 0600).\n" +
	"# Precedence: HERALD_TOKEN env > the entry for the active gateway+project > default_token.\n"

// File is the on-disk credential document. Credentials holds one [[credential]]
// table per (gateway, project); DefaultToken is the fallback when none matches.
type File struct {
	DefaultToken string       `toml:"default_token,omitempty"`
	Credentials  []Credential `toml:"credential"`
}

// Credential is one gateway+project entry.
type Credential struct {
	Gateway string `toml:"gateway"`
	Project string `toml:"project"`
	Token   string `toml:"token"`
}

// Dir resolves the directory holding the credential file: $HERALD_CONFIG_DIR
// when set, else the OS user config dir (on Linux, ~/.config/herald).
func Dir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv(DirEnv)); dir != "" {
		return dir, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	return filepath.Join(base, "herald"), nil
}

// Path is the credential file path.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// Load reads the credential file. A missing file is not an error: it returns an
// empty File so callers can treat it as "not configured".
func Load() (File, error) {
	path, err := Path()
	if err != nil {
		return File{}, err
	}
	return loadFrom(path)
}

// Lookup returns the token for the active gateway+project, falling back to
// default_token when no entry matches. Gateway is compared with trailing
// slashes trimmed. A missing file yields ("", nil).
func Lookup(gateway, project string) (string, error) {
	f, err := Load()
	if err != nil {
		return "", err
	}
	want := normalizeGateway(gateway)
	for _, c := range f.Credentials {
		if normalizeGateway(c.Gateway) == want && c.Project == project {
			return c.Token, nil
		}
	}
	return f.DefaultToken, nil
}

// Save upserts one credential and returns the file path. An empty project sets
// default_token instead of an entry. Other entries are preserved.
func Save(gateway, project, token string) (string, error) {
	path, err := Path()
	if err != nil {
		return "", err
	}
	return path, saveTo(path, gateway, project, token)
}

func saveTo(path, gateway, project, token string) error {
	f, err := loadFrom(path)
	if err != nil {
		return err
	}
	if project == "" {
		f.DefaultToken = token
	} else {
		f.upsert(normalizeGateway(gateway), project, token)
	}
	return writeAtomic(path, f)
}

func (f *File) upsert(gateway, project, token string) {
	for i := range f.Credentials {
		if normalizeGateway(f.Credentials[i].Gateway) == gateway && f.Credentials[i].Project == project {
			f.Credentials[i] = Credential{Gateway: gateway, Project: project, Token: token}
			return
		}
	}
	f.Credentials = append(f.Credentials, Credential{Gateway: gateway, Project: project, Token: token})
}

func loadFrom(path string) (File, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return File{}, nil
	}
	if err != nil {
		return File{}, fmt.Errorf("read credentials: %w", err)
	}
	var f File
	_, decErr := toml.Decode(string(data), &f)
	if decErr == nil && (f.DefaultToken != "" || len(f.Credentials) > 0) {
		return f, nil
	}
	// Backward compat: the old format was a single HERALD_TOKEN=<value> line,
	// which is not valid TOML (or parses to an unused key). Read it as the
	// default token; the next Save rewrites the file as TOML.
	if legacy := parse(string(data), TokenKey); legacy != "" {
		return File{DefaultToken: legacy}, nil
	}
	if decErr != nil {
		return File{}, fmt.Errorf("parse credentials: %w", decErr)
	}
	return f, nil
}

func normalizeGateway(gateway string) string {
	return strings.TrimRight(strings.TrimSpace(gateway), "/")
}

// writeAtomic writes f to path via a same-directory temp file, so a crash mid
// write never truncates existing credentials. A new dir is 0700 and the file 0600.
func writeAtomic(path string, f File) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create credential dir: %w", err)
	}

	var buf bytes.Buffer
	buf.WriteString(header)
	if err := toml.NewEncoder(&buf).Encode(f); err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}

	tmp, err := os.CreateTemp(dir, FileName+".*")
	if err != nil {
		return fmt.Errorf("create credentials: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return fmt.Errorf("write credentials: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("secure credentials: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	return nil
}

// parse reads a KEY=VALUE line from an env-style file, ignoring blank lines and
// # comments and stripping one layer of optional surrounding quotes. It only
// exists to read the legacy credential format.
func parse(content, key string) string {
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		return strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return ""
}
