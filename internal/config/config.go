// Package config loads the gateway and client configuration files.
package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// Gateway is the server configuration. Secrets (SMTP password, token hashes)
// never live here.
type Gateway struct {
	Server   Server             `toml:"server"`
	SMTP     SMTP               `toml:"smtp"`
	Limits   Limits             `toml:"limits"`
	Projects map[string]Project `toml:"projects"`
	Sinks    map[string]Sink    `toml:"sinks"`
}

// Built-in submit limits, applied to every project that does not override them.
// They are deliberately generous for one agent but bounded, so a runaway or
// compromised client cannot fill the queue. See docs/rate-limiting.md.
const (
	DefaultRequests = 60
	DefaultWindow   = "1m"
	DefaultDaily    = 2000
	DefaultMonthly  = 20000
)

// Limits is one project's submit budget. Every field is optional: an unset
// field inherits the global [limits] defaults. Window is a Go duration string
// ("30s", "1m", "1h").
type Limits struct {
	Requests int    `toml:"requests"`
	Window   string `toml:"window"`
	Daily    int    `toml:"daily"`
	Monthly  int    `toml:"monthly"`
}

type Server struct {
	Addr   string `toml:"addr"`
	Tenant string `toml:"tenant"`
}

// DefaultFromName is the display name used for the From header when the config
// file omits [smtp] from_name.
const DefaultFromName = "Herald"

// SMTP holds non-secret delivery settings; username and password come from the
// HERALD_SMTP_USERNAME / HERALD_SMTP_PASSWORD environment variables. ServerName
// is the TLS name to verify against when the SMTP host is dialled by IP.
// FromName is the display name shown by mail clients; an explicit empty value
// disables it and sends the bare address.
type SMTP struct {
	Addr       string `toml:"addr"`
	From       string `toml:"from"`
	FromName   string `toml:"from_name"`
	ServerName string `toml:"server_name"`
}

type Project struct {
	Sinks []string `toml:"sinks"`
	// Limits, when present, overrides the global [limits] for this project.
	Limits *Limits `toml:"limits"`
}

type Sink struct {
	Kind      string `toml:"kind"`
	Recipient string `toml:"recipient"`
	URL       string `toml:"url"`
}

func LoadGateway(path string) (Gateway, error) {
	var g Gateway
	md, err := toml.DecodeFile(path, &g)
	if err != nil {
		return Gateway{}, fmt.Errorf("decode gateway config: %w", err)
	}
	if !md.IsDefined("smtp", "from_name") {
		g.SMTP.FromName = DefaultFromName
	}
	g.Limits = g.Limits.withDefaults()
	if err := g.Validate(); err != nil {
		return Gateway{}, err
	}
	return g, nil
}

// withDefaults fills every unset field with its built-in default. A zero value
// means "unset"; there is no way to ask for zero requests, set a very high
// number instead.
func (l Limits) withDefaults() Limits {
	if l.Requests == 0 {
		l.Requests = DefaultRequests
	}
	if l.Window == "" {
		l.Window = DefaultWindow
	}
	if l.Daily == 0 {
		l.Daily = DefaultDaily
	}
	if l.Monthly == 0 {
		l.Monthly = DefaultMonthly
	}
	return l
}

func (l Limits) validate(scope string) error {
	if l.Requests < 0 || l.Daily < 0 || l.Monthly < 0 {
		return fmt.Errorf("%s: limits must not be negative", scope)
	}
	if l.Window != "" {
		d, err := time.ParseDuration(l.Window)
		if err != nil {
			return fmt.Errorf("%s: invalid window %q: %w", scope, l.Window, err)
		}
		if d <= 0 {
			return fmt.Errorf("%s: window must be positive", scope)
		}
	}
	return nil
}

// EffectiveLimits resolves the global [limits] and each project's override into
// the per-project budget the submit limiter enforces.
func (g Gateway) EffectiveLimits() map[domain.ProjectID]app.Limits {
	out := make(map[domain.ProjectID]app.Limits, len(g.Projects))
	for slug, p := range g.Projects {
		l := g.Limits.withDefaults()
		if p.Limits != nil {
			l = l.merge(*p.Limits)
		}
		d, _ := time.ParseDuration(l.Window)
		out[domain.ProjectID(slug)] = app.Limits{
			Requests: l.Requests,
			Window:   d,
			Daily:    l.Daily,
			Monthly:  l.Monthly,
		}
	}
	return out
}

// merge applies the non-zero override fields on top of base.
func (l Limits) merge(over Limits) Limits {
	if over.Requests != 0 {
		l.Requests = over.Requests
	}
	if over.Window != "" {
		l.Window = over.Window
	}
	if over.Daily != 0 {
		l.Daily = over.Daily
	}
	if over.Monthly != 0 {
		l.Monthly = over.Monthly
	}
	return l
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
	if err := g.Limits.validate("limits"); err != nil {
		return err
	}
	for slug, p := range g.Projects {
		if slug == "" {
			return errors.New("project slug must not be empty")
		}
		if p.Limits != nil {
			if err := p.Limits.validate("project " + slug + " limits"); err != nil {
				return err
			}
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
