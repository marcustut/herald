// Package config loads the gateway and client configuration files.
package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

// Gateway is the server configuration. Secrets (SMTP password, token hashes)
// never live here.
type Gateway struct {
	Server   Server             `toml:"server"`
	SMTP     SMTP               `toml:"smtp"`
	Projects map[string]Project `toml:"projects"`
	Sinks    map[string]Sink    `toml:"sinks"`
}

type Server struct {
	Addr   string `toml:"addr"`
	Tenant string `toml:"tenant"`
}

// SMTP holds non-secret delivery settings; username and password come from the
// HERALD_SMTP_USERNAME / HERALD_SMTP_PASSWORD environment variables.
type SMTP struct {
	Addr string `toml:"addr"`
	From string `toml:"from"`
}

type Project struct {
	Sinks []string `toml:"sinks"`
}

type Sink struct {
	Kind      string `toml:"kind"`
	Recipient string `toml:"recipient"`
	URL       string `toml:"url"`
}

func LoadGateway(path string) (Gateway, error) {
	var g Gateway
	if _, err := toml.DecodeFile(path, &g); err != nil {
		return Gateway{}, fmt.Errorf("decode gateway config: %w", err)
	}
	if err := g.Validate(); err != nil {
		return Gateway{}, err
	}
	return g, nil
}

func (g Gateway) Validate() error {
	if g.Server.Addr == "" {
		return errors.New("server.addr is required")
	}
	if g.Server.Tenant == "" {
		return errors.New("server.tenant is required")
	}
	if len(g.Projects) == 0 {
		return errors.New("at least one project is required")
	}
	for slug, p := range g.Projects {
		if slug == "" {
			return errors.New("project slug must not be empty")
		}
		if len(p.Sinks) == 0 {
			return fmt.Errorf("project %q has no sinks", slug)
		}
		for _, id := range p.Sinks {
			sink, ok := g.Sinks[id]
			if !ok {
				return fmt.Errorf("project %q references unknown sink %q", slug, id)
			}
			if err := sink.Validate(); err != nil {
				return fmt.Errorf("sink %q: %w", id, err)
			}
		}
	}
	return nil
}

func (s Sink) Validate() error {
	switch s.Kind {
	case "email":
		if s.Recipient == "" {
			return errors.New("email sink requires recipient")
		}
	case "webhook":
		if s.URL == "" {
			return errors.New("webhook sink requires url")
		}
	default:
		return fmt.Errorf("unknown sink kind %q", s.Kind)
	}
	return nil
}

// Client is the per-project configuration committed to a repository. It holds
// no secrets; the token comes from HERALD_TOKEN.
type Client struct {
	Project string `toml:"project"`
	Gateway string `toml:"gateway"`
}

func LoadClient(path string) (Client, error) {
	var c Client
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return Client{}, fmt.Errorf("decode client config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return Client{}, err
	}
	return c, nil
}

func (c Client) Validate() error {
	if c.Project == "" {
		return errors.New("project is required")
	}
	if c.Gateway == "" {
		return errors.New("gateway is required")
	}
	return nil
}

// ClientFromEnv fills a client config from environment variables, used when no
// herald.toml is present.
func ClientFromEnv(project, gateway string) Client {
	return Client{Project: project, Gateway: gateway}
}

// Token reads the bearer token from the environment.
func Token() string { return os.Getenv("HERALD_TOKEN") }
