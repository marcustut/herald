package cli

import (
	"os"
	"testing"
)

// TestMain points the credential file at a throwaway directory for every test,
// so no test reads or writes the developer's real user config directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "herald-cli-credentials")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("HERALD_CONFIG_DIR", dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
