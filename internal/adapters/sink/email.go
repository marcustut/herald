package sink

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
	"github.com/marcustut/herald/internal/render"
)

// MailTransport sends a raw RFC 5322 message. It matches smtp.SendMail so the
// real implementation is a thin wrapper and tests can substitute a fake.
type MailTransport interface {
	Send(addr string, a smtp.Auth, from string, to []string, msg []byte) error
}

// smtpTransport submits over SMTP. When the server offers STARTTLS it upgrades
// and verifies the certificate against tlsServerName — needed when dialling
// loopback while the certificate is issued for the public hostname.
//
// allowPlaintext is true only for loopback targets. Every other host must offer
// STARTTLS: without it the transport refuses before any mailbox or credential is
// sent, so a downgrade to cleartext auth is never silent.
type smtpTransport struct {
	tlsServerName  string
	allowPlaintext bool
}

func (t smtpTransport) Send(addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
	c, err := smtp.Dial(addr)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	if ok, _ := c.Extension("STARTTLS"); ok {
		name := t.tlsServerName
		if name == "" {
			name = hostOf(addr)
		}
		if err := c.StartTLS(&tls.Config{ServerName: name, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	} else if !t.allowPlaintext {
		return errors.New("smtp: refusing plaintext session to non-loopback host (server does not offer STARTTLS)")
	}
	if auth != nil {
		if ok, _ := c.Extension("AUTH"); !ok {
			return errors.New("smtp: server does not support AUTH")
		}
		if err := c.Auth(auth); err != nil {
			return err
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

type EmailConfig struct {
	Addr          string
	From          string
	FromName      string
	Recipient     string
	Username      string
	Password      string
	TLSServerName string
}

// Email renders a event to multipart/alternative and submits it over SMTP.
type Email struct {
	sinkID    domain.SinkID
	cfg       EmailConfig
	transport MailTransport
	now       func() time.Time
}

func NewEmail(id domain.SinkID, cfg EmailConfig) *Email {
	return &Email{
		sinkID:    id,
		cfg:       cfg,
		transport: smtpTransport{tlsServerName: cfg.TLSServerName, allowPlaintext: isLoopbackHost(hostOf(cfg.Addr))},
		now:       time.Now,
	}
}

// WithTransport overrides the mail transport (tests).
func (e *Email) WithTransport(t MailTransport) *Email {
	e.transport = t
	return e
}

func (e *Email) ID() domain.SinkID { return e.sinkID }

var _ app.Sink = (*Email)(nil)

func (e *Email) Deliver(_ context.Context, r domain.Event) error {
	msg, err := e.buildMessage(r)
	if err != nil {
		return app.Permanent(fmt.Errorf("build message: %w", err))
	}
	var auth smtp.Auth
	if e.cfg.Username != "" {
		auth = smtp.PlainAuth("", e.cfg.Username, e.cfg.Password, hostOf(e.cfg.Addr))
	}
	if err := e.transport.Send(e.cfg.Addr, auth, e.cfg.From, []string{e.cfg.Recipient}, msg); err != nil {
		return fmt.Errorf("send mail: %w", e.classifySMTP(err))
	}
	return nil
}

// classifySMTP marks a permanent SMTP 5xx reply as a bounce: the recipient was
// hard-rejected (unknown mailbox, blocked sender), so retrying the same address
// is pointless. Every other failure — a 4xx, a network error, a TLS refusal —
// stays retryable.
//
// It also strips the recipient address from the error text before it reaches a
// log line or a stored last_error, while preserving the underlying error so
// errors.Is/As (bounce, context cancellation) still work.
func (e *Email) classifySMTP(err error) error {
	if redacted := redactAddress(err.Error(), e.cfg.Recipient); redacted != err.Error() {
		err = redactedError{err: err, msg: redacted}
	}
	var protoErr *textproto.Error
	if errors.As(err, &protoErr) && protoErr.Code >= 500 {
		return app.Bounce(err)
	}
	return err
}

// redactedError keeps the original error chain (errors.Is/As still match) while
// exposing a message with a recipient address removed.
type redactedError struct {
	err error
	msg string
}

func (e redactedError) Error() string { return e.msg }
func (e redactedError) Unwrap() error { return e.err }

// redactAddress removes a recipient address from a message so a log or stored
// last_error never carries the mailbox. It is the same marker domain.Redact
// uses for secrets.
func redactAddress(msg, address string) string {
	if address == "" {
		return msg
	}
	return strings.ReplaceAll(msg, address, domain.RedactionMarker)
}

func (e *Email) buildMessage(r domain.Event) ([]byte, error) {
	rendered, err := render.Event(r)
	if err != nil {
		return nil, err
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	textHeader := textproto.MIMEHeader{}
	textHeader.Set("Content-Type", "text/plain; charset=utf-8")
	textPart, err := mw.CreatePart(textHeader)
	if err != nil {
		return nil, err
	}
	if _, err := textPart.Write([]byte(rendered.Text)); err != nil {
		return nil, err
	}
	htmlHeader := textproto.MIMEHeader{}
	htmlHeader.Set("Content-Type", "text/html; charset=utf-8")
	htmlPart, err := mw.CreatePart(htmlHeader)
	if err != nil {
		return nil, err
	}
	if _, err := htmlPart.Write([]byte(rendered.HTML)); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	var msg bytes.Buffer
	fmt.Fprintf(&msg, "From: %s\r\n", formatFrom(e.cfg.From, e.cfg.FromName))
	fmt.Fprintf(&msg, "To: %s\r\n", e.cfg.Recipient)
	fmt.Fprintf(&msg, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", rendered.Subject))
	fmt.Fprintf(&msg, "Date: %s\r\n", e.now().Format(time.RFC1123Z))
	fmt.Fprintf(&msg, "Idempotency-Key: %s\r\n", r.IdempotencyKey)
	msg.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&msg, "Content-Type: multipart/alternative; boundary=%q\r\n", mw.Boundary())
	msg.WriteString("\r\n")
	msg.Write(body.Bytes())
	return msg.Bytes(), nil
}

// hostOf splits the host from a host:port address, handling bracketed IPv6
// literals, and returns the input unchanged when there is no port.
func hostOf(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// isLoopbackHost reports whether a mail host is loopback, where a submission
// listener (e.g. Stalwart on 127.0.0.1:587) legitimately speaks plaintext.
// Every other host requires STARTTLS.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// formatFrom renders the From header. An empty display name yields the bare
// address; otherwise the name is trimmed of CR/LF (header injection), left
// unquoted when it is a valid RFC 5322 phrase, quoted when it contains
// specials, and RFC 2047 encoded when it contains non-ASCII.
func formatFrom(from, name string) string {
	name = strings.TrimSpace(strings.NewReplacer("\r", "", "\n", "").Replace(name))
	switch {
	case name == "":
		return from
	case !isASCII(name):
		return mime.QEncoding.Encode("utf-8", name) + " <" + from + ">"
	case isPhrase(name):
		return name + " <" + from + ">"
	default:
		escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(name)
		return `"` + escaped + `" <` + from + `>`
	}
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// isPhrase reports whether s is a non-empty run of atext characters and spaces,
// which is a valid RFC 5322 phrase without quoting.
func isPhrase(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r == ' ' {
			continue
		}
		if !isAtext(r) {
			return false
		}
	}
	return true
}

func isAtext(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("!#$%&'*+-/=?^_`{|}~", r)
}
