package sink

import (
	"bytes"
	"context"
	"fmt"
	"mime"
	"mime/multipart"
	"net/smtp"
	"net/textproto"
	"time"

	"github.com/marcustut/herald/internal/app"
)

// Mailer sends transactional mail (the sign-in magic link) over the same SMTP
// submission path as the email sink. It is the single mail seam: there is no
// second SMTP implementation, and it reuses smtpTransport, hostOf and
// formatFrom so STARTTLS policy and header hardening are identical.
type Mailer struct {
	cfg       EmailConfig
	transport MailTransport
	now       func() time.Time
}

// NewMailer builds a mailer from the [smtp] settings. The recipient comes from
// the message, not the config.
func NewMailer(cfg EmailConfig) *Mailer {
	return &Mailer{
		cfg:       cfg,
		transport: smtpTransport{tlsServerName: cfg.TLSServerName, allowPlaintext: isLoopbackHost(hostOf(cfg.Addr))},
		now:       time.Now,
	}
}

// WithTransport overrides the mail transport (tests).
func (m *Mailer) WithTransport(t MailTransport) *Mailer {
	m.transport = t
	return m
}

var _ app.Mailer = (*Mailer)(nil)

// SendMail renders one multipart/alternative message and submits it.
func (m *Mailer) SendMail(_ context.Context, msg app.MailMessage) error {
	built, err := m.buildMessage(msg)
	if err != nil {
		return fmt.Errorf("build message: %w", err)
	}
	var auth smtp.Auth
	if m.cfg.Username != "" {
		auth = smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, hostOf(m.cfg.Addr))
	}
	if err := m.transport.Send(m.cfg.Addr, auth, m.cfg.From, []string{msg.To}, built); err != nil {
		return fmt.Errorf("send mail: %w", err)
	}
	return nil
}

func (m *Mailer) buildMessage(msg app.MailMessage) ([]byte, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	textHeader := textproto.MIMEHeader{}
	textHeader.Set("Content-Type", "text/plain; charset=utf-8")
	textPart, err := mw.CreatePart(textHeader)
	if err != nil {
		return nil, err
	}
	if _, err := textPart.Write([]byte(msg.Text)); err != nil {
		return nil, err
	}
	htmlHeader := textproto.MIMEHeader{}
	htmlHeader.Set("Content-Type", "text/html; charset=utf-8")
	htmlPart, err := mw.CreatePart(htmlHeader)
	if err != nil {
		return nil, err
	}
	if _, err := htmlPart.Write([]byte(msg.HTML)); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	var out bytes.Buffer
	fmt.Fprintf(&out, "From: %s\r\n", formatFrom(m.cfg.From, m.cfg.FromName))
	fmt.Fprintf(&out, "To: %s\r\n", msg.To)
	fmt.Fprintf(&out, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", msg.Subject))
	fmt.Fprintf(&out, "Date: %s\r\n", m.now().Format(time.RFC1123Z))
	out.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&out, "Content-Type: multipart/alternative; boundary=%q\r\n", mw.Boundary())
	out.WriteString("\r\n")
	out.Write(body.Bytes())
	return out.Bytes(), nil
}
