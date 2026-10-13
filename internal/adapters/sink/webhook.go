package sink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// WebhookTimeout bounds one webhook request.
const WebhookTimeout = 10 * time.Second

// errBlockedAddress marks a destination that resolved only to non-public
// addresses. It is a policy refusal, so delivery is permanent, not retried.
var errBlockedAddress = errors.New("webhook target is not a public address")

// Webhook POSTs a JSON representation of the event to a destination URL. The
// URL comes from the admin's gateway config; a webhook can never be pointed at
// a request-supplied or event-supplied address.
type Webhook struct {
	sinkID domain.SinkID
	url    string
	client *http.Client
}

func NewWebhook(id domain.SinkID, url string, client *http.Client) *Webhook {
	if client == nil {
		// Deny by construction: an unconfigured client is hardened against SSRF.
		client = NewHTTPClient(WebhookTimeout, false)
	}
	return &Webhook{sinkID: id, url: url, client: client}
}

// NewHTTPClient returns an *http.Client for outbound webhooks. It never follows
// redirects (a 3xx is reported, not chased) and dials through a guard that
// refuses loopback, private, link-local and cloud-metadata addresses unless
// allowPrivate is set. Environment proxies are ignored so a proxy cannot be used
// to reach an address the guard would reject.
func NewHTTPClient(timeout time.Duration, allowPrivate bool) *http.Client {
	if timeout <= 0 {
		timeout = WebhookTimeout
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         guardedDialContext(allowPrivate),
			ForceAttemptHTTP2:   true,
			MaxIdleConns:        100,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// guardedDialContext resolves the host itself and dials a checked IP, so a DNS
// record cannot rebind between the check and the connection.
func guardedDialContext(allowPrivate bool) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ip := range ips {
			if !allowPrivate && blockedIP(ip) {
				lastErr = fmt.Errorf("%w: %s (%s)", errBlockedAddress, ip, host)
				continue
			}
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("webhook: no addresses for %q", host)
		}
		return nil, lastErr
	}
}

// cgnatRange is RFC 6598 shared address space (100.64.0.0/10). It is not
// covered by net.IP.IsPrivate, yet some clouds host metadata there (for example
// 100.100.100.200 on Alibaba Cloud).
var cgnatRange = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// blockedIP reports whether ip is outside the public internet: loopback,
// RFC1918/ULA private, link-local (which includes the 169.254.169.254 metadata
// service), carrier-grade NAT, multicast, or unspecified.
func blockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() ||
		cgnatRange.Contains(ip) || !ip.IsGlobalUnicast()
}

func (w *Webhook) ID() domain.SinkID { return w.sinkID }

type webhookPayload struct {
	ID             string            `json:"id"`
	Status         string            `json:"status"`
	Project        string            `json:"project"`
	Agent          string            `json:"agent"`
	Host           string            `json:"host,omitempty"`
	Title          string            `json:"title"`
	Body           string            `json:"body,omitempty"`
	Links          []webhookLink     `json:"links,omitempty"`
	Context        map[string]string `json:"context,omitempty"`
	IdempotencyKey string            `json:"idempotencyKey"`
	CreatedAt      string            `json:"createdAt"`
}

type webhookLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

func (w *Webhook) Deliver(ctx context.Context, r domain.Event) error {
	// Reject a non-http(s) or hostless URL up front (defense in depth: the
	// config loader already validates). This keeps file:, gopher: and
	// protocol-relative targets out permanently rather than as retries.
	if u, err := url.Parse(w.url); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return app.Permanent(fmt.Errorf("invalid webhook url %q", w.url))
	}
	payload := webhookPayload{
		ID:             string(r.ID),
		Status:         r.Status.String(),
		Project:        string(r.ProjectID),
		Agent:          r.AgentID.String(),
		Host:           r.Context.Host(),
		Title:          r.Title,
		Body:           r.Body,
		Context:        r.Context,
		IdempotencyKey: string(r.IdempotencyKey),
		CreatedAt:      r.CreatedAt.UTC().Format(time.RFC3339),
	}
	for _, l := range r.Links {
		payload.Links = append(payload.Links, webhookLink{Label: l.Label, URL: l.URL})
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return app.Permanent(fmt.Errorf("encode webhook payload: %w", err))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return app.Permanent(fmt.Errorf("build webhook request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", string(r.IdempotencyKey))

	resp, err := w.client.Do(req)
	if err != nil {
		if errors.Is(err, errBlockedAddress) {
			return app.Permanent(fmt.Errorf("webhook refused: %w", err))
		}
		return fmt.Errorf("webhook request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return app.Permanent(fmt.Errorf("webhook status %d (redirects are not followed)", resp.StatusCode))
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("webhook status %d", resp.StatusCode)
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return app.Permanent(fmt.Errorf("webhook status %d", resp.StatusCode))
	default:
		return fmt.Errorf("webhook status %d", resp.StatusCode)
	}
}

var _ app.Sink = (*Webhook)(nil)
