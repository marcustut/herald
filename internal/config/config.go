// Package config loads the gateway and client configuration files.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/marcustut/herald/internal/app"
)

// Gateway is the server configuration. Secrets (SMTP password, token hashes)
// never live here.
type Gateway struct {
	Server   Server   `toml:"server"`
	SMTP     SMTP     `toml:"smtp"`
	Security Security `toml:"security"`
	Limits   Limits   `toml:"limits"`
	Auth     Auth     `toml:"auth"`
}

// Auth configures the account/control-plane surface. Accounts are part of the
// product: whenever a database is configured the control-plane API is composed.
// BaseURL is the public base under which the API is served; the magic-link
// callback path is appended to it. It is an absolute URL and may include a path
// prefix such as /api. Signup gates whether a request for an unregistered
// address provisions and mails a new account; existing accounts always receive
// links.
//
// BounceWebhook enables the provider bounce/complaint ingestion endpoint
// (POST /v1/email/events). It is shared-secret authenticated with
// HERALD_BOUNCE_WEBHOOK_SECRET, which is read from the environment so no secret
// lives in the config file. See docs/deliverability.md.
type Auth struct {
	BaseURL       string `toml:"base_url"`
	Signup        bool   `toml:"signup"`
	BounceWebhook bool   `toml:"bounce_webhook"`
}

// Security holds the outbound-delivery hardening knobs. A webhook destination
// can only ever come from the database (the request body and event content have
// no path to it); these settings constrain those URLs further.
// See docs/security.md.
type Security struct {
	// WebhookAllowHosts, when non-empty, restricts webhook destinations to
	// these hosts (case-insensitive; a port is ignored). Empty allows any host,
	// still subject to the dial-time private-network guard. It is enforced at
	// delivery time, since destinations live in the database.
	WebhookAllowHosts []string `toml:"webhook_allow_hosts"`
	// AllowPrivateNetworks permits webhook delivery to loopback, private and
	// link-local addresses (for a receiver on a trusted internal network). Off
	// by default, so a misconfigured or redirected URL cannot reach cloud
	// metadata (169.254.169.254) or intranet services.
	AllowPrivateNetworks bool `toml:"allow_private_networks"`
}

// Built-in submit limits, applied to every (tenant, project). They are
// deliberately generous for one agent but bounded, so a runaway or compromised
// client cannot fill the queue. See docs/rate-limiting.md.
const (
	DefaultRequests = 60
	DefaultWindow   = "1m"
	DefaultDaily    = 2000
	DefaultMonthly  = 20000
)

// Limits is the global submit budget, applied to every (tenant, project). Every
// field is optional: an unset field inherits the built-in default. Window is a
// Go duration string ("30s", "1m", "1h"). Queued caps outstanding deliveries
// (backpressure) and is off by default.
type Limits struct {
	Requests int    `toml:"requests"`
	Window   string `toml:"window"`
	Daily    int    `toml:"daily"`
	Monthly  int    `toml:"monthly"`
	Queued   int    `toml:"queued"`
}

type Server struct {
	Addr string `toml:"addr"`
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

func LoadGateway(path string) (Gateway, error) {
	var g Gateway
	md, err := toml.DecodeFile(path, &g)
	if err != nil {
		return Gateway{}, fmt.Errorf("decode gateway config: %w", err)
	}
	if !md.IsDefined("smtp", "from_name") {
		g.SMTP.FromName = DefaultFromName
	}
	if !md.IsDefined("auth", "signup") {
		g.Auth.Signup = true
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
	if l.Requests < 0 || l.Daily < 0 || l.Monthly < 0 || l.Queued < 0 {
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

// AppLimits is the resolved global budget the submit limiter enforces for every
// (tenant, project).
func (l Limits) AppLimits() app.Limits {
	d, _ := time.ParseDuration(l.Window)
	return app.Limits{
		Requests: l.Requests,
		Window:   d,
		Daily:    l.Daily,
		Monthly:  l.Monthly,
		Queued:   l.Queued,
	}
}

func (g Gateway) Validate() error {
	if g.Server.Addr == "" {
		return errors.New("server.addr is required")
	}
	if err := g.Limits.validate("limits"); err != nil {
		return err
	}
	if err := g.Security.validate(); err != nil {
		return err
	}
	return g.Auth.validate(g.SMTP)
}

func (s Security) validate() error {
	for _, h := range s.WebhookAllowHosts {
		if hostOnly(h) == "" {
			return errors.New("security.webhook_allow_hosts contains an empty host")
		}
	}
	return nil
}

// WebhookHostAllowed reports whether a stored webhook destination passes the
// optional allowlist. An empty allowlist permits any host; the dial-time guard
// still blocks non-public addresses. A non-http(s) or host-less URL never
// matches.
func (s Security) WebhookHostAllowed(raw string) bool {
	if len(s.WebhookAllowHosts) == 0 {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	host := hostOnly(u.Host)
	for _, allowed := range s.WebhookAllowHosts {
		if hostOnly(allowed) == host {
			return true
		}
	}
	return false
}

// validate rejects an auth section that lacks the pieces the account surface
// needs: a public base URL and a usable SMTP submission path.
func (a Auth) validate(smtp SMTP) error {
	u, err := url.Parse(a.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("auth.base_url must be an absolute http(s) URL with a host")
	}
	if smtp.Addr == "" || smtp.From == "" {
		return errors.New("accounts require [smtp] addr and from to send magic links")
	}
	return nil
}

// hostOnly lowercases a host and drops a trailing port, so allowlist entries
// compare by name alone.
func hostOnly(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if h, _, err := net.SplitHostPort(s); err == nil {
		return h
	}
	return s
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
