// Package credentials stores the herald bearer token in a per-user file, so a
// shell or agent session can find it without hand-exporting HERALD_TOKEN. The
// file is written 0600 and is the fallback between the process environment and
// herald.toml: process env > credential file > herald.toml.
package credentials

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DirEnv overrides the directory that holds the credential file. It is the
// single knob tests and unusual setups need; an unset value uses the OS user
// config dir.
const DirEnv = "HERALD_CONFIG_DIR"

// TokenKey is the variable name the credential file uses, matching the
// environment variable it stands in for.
const TokenKey = "HERALD_TOKEN"

// FileName is the credential file inside the config directory.
const FileName = "credentials"

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

// Token returns HERALD_TOKEN from the credential file. A missing file is not an
// error: it returns ("", nil) so callers can treat it as "not configured".
func Token() (string, error) {
	path, err := Path()
	if err != nil {
		return "", err
	}
	return tokenFrom(path)
}

func tokenFrom(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read credentials: %w", err)
	}
	return parse(string(data), TokenKey), nil
}

// Save writes token to the credential file, creating the directory 0700 and
// the file 0600. It replaces the file, so no stale value survives.
func Save(token string) (string, error) {
	path, err := Path()
	if err != nil {
		return "", err
	}
	return path, saveTo(path, token)
}

func saveTo(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create credential dir: %w", err)
	}
	content := "# herald credentials. Keep this file private (chmod 0600).\n" +
		TokenKey + "=" + token + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	// WriteFile only applies the mode when it creates the file, so an existing
	// file keeps its old permissions; force them.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure credentials: %w", err)
	}
	return nil
}

// parse reads a KEY=VALUE line from an env-style file, ignoring blank lines and
// # comments and stripping one layer of optional surrounding quotes.
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
