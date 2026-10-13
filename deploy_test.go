package herald_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcustut/herald/internal/config"
)

// The Docker/Compose deployment is only as good as the files it ships. These
// tests parse the real example gateway config with the same loader the binary
// uses, and render the real Compose file with docker, so drift breaks CI.

func repoFile(t *testing.T, name string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Join(wd, name)
}

func TestDockerExampleGatewayConfigLoads(t *testing.T) {
	cfg, err := config.LoadGateway(repoFile(t, "gateway.docker.example.toml"))
	if err != nil {
		t.Fatalf("LoadGateway(gateway.docker.example.toml): %v", err)
	}
	// The container must bind all interfaces or the published port is unreachable.
	if want := "0.0.0.0:8787"; cfg.Server.Addr != want {
		t.Errorf("server.addr = %q, want %q", cfg.Server.Addr, want)
	}
	if len(cfg.Projects) == 0 {
		t.Fatal("no projects configured")
	}
	for slug, p := range cfg.Projects {
		for _, id := range p.Sinks {
			if _, ok := cfg.Sinks[id]; !ok {
				t.Errorf("project %q references undefined sink %q", slug, id)
			}
		}
	}
}

func TestExampleGatewayConfigLoads(t *testing.T) {
	if _, err := config.LoadGateway(repoFile(t, "gateway.example.toml")); err != nil {
		t.Fatalf("LoadGateway(gateway.example.toml): %v", err)
	}
}

func requireCompose(t *testing.T) string {
	t.Helper()
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker not installed")
	}
	if out, err := exec.Command(docker, "compose", "version").CombinedOutput(); err != nil {
		t.Skipf("docker compose unavailable: %v: %s", err, out)
	}
	return docker
}

// emptyEnvFile returns a path to an empty --env-file, so the tests do not pick
// up a developer's local .env (which is gitignored and may exist).
func emptyEnvFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "empty.env")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write empty env file: %v", err)
	}
	return path
}

func TestComposeConfigRendersValidServices(t *testing.T) {
	docker := requireCompose(t)

	cmd := exec.Command(docker, "compose",
		"--env-file", emptyEnvFile(t),
		"-f", repoFile(t, "docker-compose.yml"),
		"config", "--format", "json")
	cmd.Dir = filepath.Dir(repoFile(t, "docker-compose.yml"))
	// Required secrets, as a user would provide in .env. Compose must refuse to
	// start without these, so the test supplies them explicitly.
	cmd.Env = append(environWithout("HERALD_"),
		"HERALD_DATABASE_URL=postgres://herald:pw@postgres:5432/herald?sslmode=disable",
		"HERALD_POSTGRES_PASSWORD=pw",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose config: %v\n%s", err, out)
	}

	var rendered struct {
		Services map[string]struct {
			Build struct {
				Context string `json:"context"`
			} `json:"build"`
			Image     string   `json:"image"`
			Command   []string `json:"command"`
			DependsOn map[string]struct {
				Condition string `json:"condition"`
			} `json:"depends_on"`
			Volumes []struct {
				Source string `json:"source"`
			} `json:"volumes"`
			Healthcheck struct {
				Test []string `json:"test"`
			} `json:"healthcheck"`
		} `json:"services"`
		Volumes map[string]any `json:"volumes"`
	}
	if err := json.Unmarshal(out, &rendered); err != nil {
		t.Fatalf("unmarshal compose config: %v\n%s", err, out)
	}

	for _, name := range []string{"postgres", "herald", "herald-dispatcher"} {
		if _, ok := rendered.Services[name]; !ok {
			t.Errorf("service %q missing from rendered compose config", name)
		}
	}

	api := rendered.Services["herald"]
	if api.Build.Context == "" {
		t.Error("herald service has no build context")
	}
	if got := strings.Join(api.Command, " "); !strings.Contains(got, "serve") {
		t.Errorf("herald command = %q, want it to run `serve`", got)
	}
	if len(api.Healthcheck.Test) == 0 {
		t.Error("herald service has no healthcheck")
	} else if !strings.Contains(strings.Join(api.Healthcheck.Test, " "), "/healthz") {
		t.Errorf("herald healthcheck does not hit /healthz: %v", api.Healthcheck.Test)
	}

	disp := rendered.Services["herald-dispatcher"]
	if got := strings.Join(disp.Command, " "); !strings.Contains(got, "dispatch") {
		t.Errorf("dispatcher command = %q, want it to run `dispatch`", got)
	}
	if cond := disp.DependsOn["postgres"].Condition; cond != "service_healthy" {
		t.Errorf("dispatcher depends_on postgres condition = %q, want service_healthy", cond)
	}

	pg := rendered.Services["postgres"]
	if len(pg.Healthcheck.Test) == 0 {
		t.Error("postgres service has no healthcheck")
	}
	var pgVolume string
	for _, v := range pg.Volumes {
		pgVolume = v.Source
	}
	if pgVolume != "herald-db" {
		t.Errorf("postgres volume source = %q, want named volume herald-db", pgVolume)
	}
	if _, ok := rendered.Volumes["herald-db"]; !ok {
		t.Error("named volume herald-db is not declared")
	}
}

func TestComposeRequiresSecrets(t *testing.T) {
	docker := requireCompose(t)

	// With no .env and no secrets in the environment, `config` must fail rather
	// than silently fall back to insecure defaults.
	cmd := exec.Command(docker, "compose",
		"--env-file", emptyEnvFile(t),
		"-f", repoFile(t, "docker-compose.yml"),
		"config")
	cmd.Dir = filepath.Dir(repoFile(t, "docker-compose.yml"))
	cmd.Env = environWithout("HERALD_")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("docker compose config succeeded without secrets; want failure\n%s", out)
	}
}

// environWithout returns os.Environ() minus variables with the given prefix.
func environWithout(prefix string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, prefix) {
			env = append(env, kv)
		}
	}
	return env
}
