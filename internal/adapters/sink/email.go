package sink

import (
	"bytes"
	"context"
	"fmt"
	"mime"
	"mime/multipart"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
	"github.com/marcustut/herald/internal/render"
)

// MailTransport sends a raw RFC 5322 message. It matches smtp.SendMail so the
// real implementation is a one-line wrapper and tests can substitute a fake.
type MailTransport interface {
	Send(addr string, a smtp.Auth, from string, to []string, msg []byte) error
}

type smtpTransport struct{}

func (smtpTransport) Send(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
	return smtp.SendMail(addr, a, from, to, msg)
}

type EmailConfig struct {
	Addr      string
	From      string
	Recipient string
	Username  string
	Password  string
}

// Email renders a report to multipart/alternative and submits it over SMTP.
type Email struct {
	sinkID    domain.SinkID
	cfg       EmailConfig
	transport MailTransport
	now       func() time.Time
}

func NewEmail(id domain.SinkID, cfg EmailConfig) *Email {
	return &Email{sinkID: id, cfg: cfg, transport: smtpTransport{}, now: time.Now}
}

// WithTransport overrides the mail transport (tests).
func (e *Email) WithTransport(t MailTransport) *Email {
	e.transport = t
	return e
}

func (e *Email) ID() domain.SinkID { return e.sinkID }

var _ app.Sink = (*Email)(nil)

func (e *Email) Deliver(_ context.Context, r domain.Report) error {
	msg, err := e.buildMessage(r)
	if err != nil {
		return app.Permanent(fmt.Errorf("build message: %w", err))
	}
	var auth smtp.Auth
	if e.cfg.Username != "" {
		auth = smtp.PlainAuth("", e.cfg.Username, e.cfg.Password, hostOf(e.cfg.Addr))
	}
	if err := e.transport.Send(e.cfg.Addr, auth, e.cfg.From, []string{e.cfg.Recipient}, msg); err != nil {
		return fmt.Errorf("send mail: %w", err)
	}
	return nil
}

func (e *Email) buildMessage(r domain.Report) ([]byte, error) {
	rendered, err := render.Report(r)
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
	fmt.Fprintf(&msg, "From: %s\r\n", e.cfg.From)
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

func hostOf(addr string) string {
	if i := strings.LastIndexByte(addr, ':'); i >= 0 {
		return addr[:i]
	}
	return addr
}
