// Package render turns a event into an email subject, HTML body and text body.
// Untrusted fields are escaped by html/template.
package render

import (
	"bytes"
	"embed"
	"fmt"
	htmltemplate "html/template"
	texttemplate "text/template"
	"time"

	"github.com/marcustut/herald/internal/domain"
)

//go:embed templates/*.tmpl
var files embed.FS

var (
	htmlTmpl = htmltemplate.Must(htmltemplate.ParseFS(files, "templates/event.html.tmpl"))
	textTmpl = texttemplate.Must(texttemplate.ParseFS(files, "templates/event.txt.tmpl"))
)

type Rendered struct {
	Subject string
	HTML    string
	Text    string
}

type view struct {
	Status  string
	Project string
	Agent   string
	Host    string
	Title   string
	Body    string
	Links   []domain.Link
	Time    string
}

func Event(r domain.Event) (Rendered, error) {
	v := view{
		Status:  r.Status.String(),
		Project: string(r.ProjectID),
		Agent:   r.AgentID.String(),
		Host:    r.Context.Host(),
		Title:   r.Title,
		Body:    r.Body,
		Links:   r.Links,
		Time:    r.CreatedAt.UTC().Format(time.RFC3339),
	}
	var htmlBody, textBody bytes.Buffer
	if err := htmlTmpl.Execute(&htmlBody, v); err != nil {
		return Rendered{}, fmt.Errorf("render html: %w", err)
	}
	if err := textTmpl.Execute(&textBody, v); err != nil {
		return Rendered{}, fmt.Errorf("render text: %w", err)
	}
	return Rendered{Subject: Subject(v.Status, string(r.ProjectID), v.Agent, v.Host, v.Title), HTML: htmlBody.String(), Text: textBody.String()}, nil
}

// Subject builds the email subject line, showing which agent on which host.
func Subject(status, project, agent, host, title string) string {
	who := agent
	if host != "" {
		who += "@" + host
	}
	return fmt.Sprintf("[herald] %s · %s · %s — %s", status, project, who, title)
}
