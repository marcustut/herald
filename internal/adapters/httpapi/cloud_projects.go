package httpapi

import (
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"time"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// registerProjects wires the project and token self-service surface. Every
// route is session-scoped: ownership is taken from the session's tenant, never
// from the request, so one account can never address another's rows. The HTML
// page is a thin shell over the JSON routes.
func (c *Cloud) registerProjects(mux *http.ServeMux) {
	mux.HandleFunc("GET /dashboard", c.handleProjectsPage)
	mux.HandleFunc("GET /dashboard/fonts/{name}", c.handleDashboardFont)
	mux.HandleFunc("GET /v1/projects", c.handleListProjects)
	mux.HandleFunc("POST /v1/projects", c.handleCreateProject)
	mux.HandleFunc("DELETE /v1/projects/{project}", c.handleDeleteProject)
	mux.HandleFunc("GET /v1/tokens", c.handleListTokens)
	mux.HandleFunc("POST /v1/tokens", c.handleCreateScopedToken)
	mux.HandleFunc("DELETE /v1/tokens/{id}", c.handleRevokeToken)
	mux.HandleFunc("POST /v1/projects/{project}/token", c.handleCreateProjectToken)
	mux.HandleFunc("POST /v1/projects/{project}/token/rotate", c.handleRotateProjectToken)
}

type projectPayload struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type scopedTokenPayload struct {
	Projects []string `json:"projects"`
}

type projectDTO struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
}

func projectDTOFrom(p domain.Project) projectDTO {
	return projectDTO{ID: string(p.ID), Name: p.Name, CreatedAt: p.CreatedAt}
}

// tokenDTO is token metadata. ID is the stored, one-way token hash: it is the
// only durable identifier there is (no separate id column), and it is not the
// secret — the raw token is never stored or returned here.
type tokenDTO struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Projects   []string   `json:"projects"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
}

func tokenDTOFrom(c app.Credential) tokenDTO {
	projects := make([]string, len(c.Projects))
	for i, p := range c.Projects {
		projects[i] = string(p)
	}
	return tokenDTO{
		ID:         c.Hash,
		Kind:       string(c.Kind),
		Projects:   projects,
		CreatedAt:  c.CreatedAt,
		LastUsedAt: timeOrNil(c.LastUsedAt),
		RevokedAt:  timeOrNil(c.RevokedAt),
	}
}

func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// handleListProjects returns the session tenant's projects.
func (c *Cloud) handleListProjects(w http.ResponseWriter, r *http.Request) {
	sess, ok := c.session(w, r)
	if !ok {
		return
	}
	projects, err := c.Projects.ListProjects(r.Context(), sess.Identity.TenantID)
	if err != nil {
		c.serverError(w, err)
		return
	}
	out := make([]projectDTO, 0, len(projects))
	for _, p := range projects {
		out = append(out, projectDTOFrom(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": out})
}

func (c *Cloud) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	sess, ok := c.session(w, r)
	if !ok {
		return
	}
	if !c.csrf(w, r, sess) {
		return
	}
	var payload projectPayload
	if !decodeJSON(w, r, &payload) {
		return
	}
	project, err := c.Projects.CreateProject(
		r.Context(), sess.Identity.TenantID, domain.ProjectID(payload.ID), payload.Name)
	if err != nil {
		if !c.projectError(w, err) {
			c.serverError(w, err)
		}
		return
	}
	c.audit(r, sess, "project.create", string(project.ID))
	writeJSON(w, http.StatusCreated, map[string]any{"project": projectDTOFrom(project)})
}

func (c *Cloud) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	sess, ok := c.session(w, r)
	if !ok {
		return
	}
	if !c.csrf(w, r, sess) {
		return
	}
	project := domain.ProjectID(r.PathValue("project"))
	if err := c.Projects.DeleteProject(r.Context(), sess.Identity.TenantID, project); err != nil {
		if !c.projectError(w, err) {
			c.serverError(w, err)
		}
		return
	}
	c.audit(r, sess, "project.delete", string(project))
	w.WriteHeader(http.StatusNoContent)
}

func (c *Cloud) handleListTokens(w http.ResponseWriter, r *http.Request) {
	sess, ok := c.session(w, r)
	if !ok {
		return
	}
	creds, err := c.Projects.ListTokens(r.Context(), sess.Identity.TenantID)
	if err != nil {
		c.serverError(w, err)
		return
	}
	out := make([]tokenDTO, 0, len(creds))
	for _, cred := range creds {
		out = append(out, tokenDTOFrom(cred))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

// handleCreateProjectToken mints a single-project token. The raw secret is
// returned exactly once and never stored or logged.
func (c *Cloud) handleCreateProjectToken(w http.ResponseWriter, r *http.Request) {
	sess, ok := c.session(w, r)
	if !ok {
		return
	}
	if !c.csrf(w, r, sess) {
		return
	}
	project := domain.ProjectID(r.PathValue("project"))
	raw, err := c.Projects.IssueProjectToken(r.Context(), sess.Identity.TenantID, project)
	if err != nil {
		if !c.projectError(w, err) {
			c.serverError(w, err)
		}
		return
	}
	c.audit(r, sess, "token.issue", string(project))
	c.writeToken(w, raw, domain.TokenProject, project)
}

// handleRotateProjectToken mints a replacement project token and revokes the
// previous one(s): a caller holding the old secret is refused from then on.
func (c *Cloud) handleRotateProjectToken(w http.ResponseWriter, r *http.Request) {
	sess, ok := c.session(w, r)
	if !ok {
		return
	}
	if !c.csrf(w, r, sess) {
		return
	}
	project := domain.ProjectID(r.PathValue("project"))
	raw, err := c.Projects.RotateProjectToken(r.Context(), sess.Identity.TenantID, project)
	if err != nil {
		if !c.projectError(w, err) {
			c.serverError(w, err)
		}
		return
	}
	c.audit(r, sess, "token.rotate", string(project))
	c.writeToken(w, raw, domain.TokenProject, project)
}

// handleCreateScopedToken mints a multi-project token. Every project must be
// owned by the session's tenant, so the token cannot widen to a foreign one.
func (c *Cloud) handleCreateScopedToken(w http.ResponseWriter, r *http.Request) {
	sess, ok := c.session(w, r)
	if !ok {
		return
	}
	if !c.csrf(w, r, sess) {
		return
	}
	var payload scopedTokenPayload
	if !decodeJSON(w, r, &payload) {
		return
	}
	projects := make([]domain.ProjectID, len(payload.Projects))
	for i, p := range payload.Projects {
		projects[i] = domain.ProjectID(p)
	}
	raw, err := c.Projects.IssueScopedToken(r.Context(), sess.Identity.TenantID, projects)
	if err != nil {
		if !c.projectError(w, err) {
			c.serverError(w, err)
		}
		return
	}
	c.audit(r, sess, "token.issue.scoped", "")
	c.writeToken(w, raw, domain.TokenScoped, projects...)
}

func (c *Cloud) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	sess, ok := c.session(w, r)
	if !ok {
		return
	}
	if !c.csrf(w, r, sess) {
		return
	}
	hash := r.PathValue("id")
	if err := c.Projects.RevokeToken(r.Context(), sess.Identity.TenantID, hash); err != nil {
		if !c.projectError(w, err) {
			c.serverError(w, err)
		}
		return
	}
	// Log the token hash, never the (already gone) raw secret.
	c.audit(r, sess, "token.revoke", "", "token", hash)
	w.WriteHeader(http.StatusNoContent)
}

// writeToken returns a freshly minted raw token once, marked no-store so a
// shared cache never keeps it.
func (c *Cloud) writeToken(w http.ResponseWriter, raw string, kind domain.TokenKind, projects ...domain.ProjectID) {
	names := make([]string, len(projects))
	for i, p := range projects {
		names[i] = string(p)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":    raw,
		"kind":     string(kind),
		"projects": names,
	})
}

// projectError maps the use case's ownership/validation errors to responses. It
// reports whether it handled the error; false means the caller answers 503.
func (c *Cloud) projectError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, domain.ErrInvalidID):
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error(), "")
		return true
	case errors.Is(err, app.ErrProjectNotFound):
		writeError(w, http.StatusNotFound, "project_not_found", "no such project", "")
		return true
	case errors.Is(err, app.ErrCredentialNotFound):
		writeError(w, http.StatusNotFound, "token_not_found", "no such token", "")
		return true
	case errors.Is(err, app.ErrInvalidInput):
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error(), "")
		return true
	default:
		return false
	}
}

// audit records one successful state-changing cloud action with the actor and
// owning tenant. It never logs a raw token.
func (c *Cloud) audit(r *http.Request, sess app.Session, action, project string, extra ...any) {
	if c.Logger == nil {
		return
	}
	attrs := []any{
		"action", action,
		"actor", string(sess.Identity.UserID),
		"account", string(sess.Identity.AccountID),
		"tenant", string(sess.Identity.TenantID),
	}
	if project != "" {
		attrs = append(attrs, "project", project)
	}
	attrs = append(attrs, extra...)
	c.Logger.InfoContext(r.Context(), "cloud mutation", attrs...)
}

// decodeJSON reads a bounded JSON body. It writes the error response itself.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if !isJSON(r.Header.Get("Content-Type")) {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json", "")
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, defaultMaxBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "could not read request body", "")
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "malformed JSON", "")
		return false
	}
	return true
}

// handleProjectsPage serves the self-contained project/token UI. It is a thin
// HTML shell: the browser fetches the JSON routes with the session cookie and
// sends the CSRF token, supplied once at sign-in, on every mutation.
func (c *Cloud) handleProjectsPage(w http.ResponseWriter, r *http.Request) {
	// Rendered even without a session so the sign-in form is reachable; the
	// page hides the dashboard sections until the JSON routes authenticate.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; font-src 'self'; connect-src 'self'")
	if err := projectsPage.Execute(w, nil); err != nil && c.Logger != nil {
		c.Logger.Error("render projects page", "error", err)
	}
}

var projectsPage = template.Must(template.New("projects").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Herald dashboard</title>
<style>
  @font-face{font-family:"Instrument Serif";src:url("/dashboard/fonts/instrument-serif-normal-400.woff2") format("woff2");font-weight:400;font-style:normal;font-display:swap}
  @font-face{font-family:"Instrument Serif";src:url("/dashboard/fonts/instrument-serif-italic-400.woff2") format("woff2");font-weight:400;font-style:italic;font-display:swap}
  @font-face{font-family:"DM Sans";src:url("/dashboard/fonts/dm-sans-normal-400.woff2") format("woff2");font-weight:400;font-display:swap}
  @font-face{font-family:"JetBrains Mono";src:url("/dashboard/fonts/jetbrains-mono-normal-400.woff2") format("woff2");font-weight:400;font-display:swap}

  :root{
    --bg:#0d0d0c; --surface:#141413; --surface-2:#1d1d1b;
    --line:#242422; --line-strong:#2a2a27;
    --fg:#edeae4; --muted:#8a8680;
    --accent:#d4a843; --accent-fg:#0d0d0c; --positive:#4ade80;
    --danger:#e5484d;
    --radius-card:.75rem; --radius-panel:1.25rem; --radius-pill:9999px;
    --font-display:"Instrument Serif"; --font-sans:"DM Sans"; --font-mono:"JetBrains Mono";
    --measure:40rem; --wide:74rem;
    --ease:cubic-bezier(.2,.6,.2,1);
  }

  *,*::before,*::after{box-sizing:border-box}
  [hidden]{display:none!important}
  html,body{margin:0;padding:0}
  body{
    background:var(--bg); color:var(--fg);
    font-family:var(--font-sans),system-ui,-apple-system,sans-serif;
    font-size:14px; line-height:1.5;
    -webkit-font-smoothing:antialiased; text-rendering:optimizeLegibility;
  }
  a{color:inherit}
  button{font:inherit}
  ::selection{background:var(--accent);color:var(--accent-fg)}
  :focus-visible{outline:2px solid var(--accent);outline-offset:2px;border-radius:4px}
  .sr-only{position:absolute;width:1px;height:1px;padding:0;margin:-1px;overflow:hidden;clip:rect(0 0 0 0);white-space:nowrap;border:0}
  .mono{font-family:var(--font-mono);font-size:12.5px;letter-spacing:-.01em}
  .muted{color:var(--muted)}
  .skip{position:absolute;left:-999px}
  .skip:focus{left:1rem;top:1rem;background:var(--surface-2);padding:.5rem .7rem;border:1px solid var(--accent);border-radius:6px;z-index:90}

  /* ---------------- shell ---------------- */
  .app{display:grid;grid-template-columns:246px minmax(0,1fr);min-height:100vh}

  .sidebar{
    position:sticky;top:0;height:100vh;overflow:auto;
    display:flex;flex-direction:column;gap:1.25rem;
    padding:1.1rem .8rem 1rem;
    border-right:1px solid var(--line);
  }
  .brand{display:flex;align-items:baseline;gap:.5rem;padding:.15rem .55rem 0}
  .brand-mark{font-family:var(--font-display);font-size:1.4rem;line-height:1;letter-spacing:.01em}
  .brand-sub{font-family:var(--font-mono);font-size:10.5px;text-transform:uppercase;letter-spacing:.14em;color:var(--muted)}
  .nav{display:flex;flex-direction:column;gap:.1rem}
  .nav-label{
    font-family:var(--font-mono);font-size:10px;text-transform:uppercase;
    letter-spacing:.14em;color:var(--muted);margin:1rem .55rem .35rem;
  }
  .nav-label:first-child{margin-top:.2rem}
  .nav-item{
    display:flex;align-items:center;gap:.55rem;
    padding:.36rem .55rem;border-radius:6px;
    color:var(--muted);text-decoration:none;font-size:13.5px;
    transition:background .12s var(--ease),color .12s var(--ease);
  }
  .nav-item:hover{background:var(--surface);color:var(--fg)}
  .nav-item[aria-current="true"]{background:var(--surface-2);color:var(--fg)}
  .nav-item[aria-current="true"]::before{
    content:"";width:3px;height:3px;border-radius:50%;background:var(--accent);
    margin-left:-.15rem;
  }
  .nav-count{margin-left:auto;font-family:var(--font-mono);font-size:11px;color:var(--muted)}

  .account{margin-top:auto;border-top:1px solid var(--line);padding-top:.85rem;display:flex;flex-direction:column;gap:.55rem}
  .account-id{display:flex;align-items:center;gap:.55rem;padding:0 .35rem;min-width:0}
  .avatar{
    width:24px;height:24px;flex:none;border-radius:50%;
    display:grid;place-items:center;font-size:11px;
    background:var(--surface-2);border:1px solid var(--line-strong);color:var(--fg);
  }
  .account-email{font-size:12.5px;color:var(--muted);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}

  /* ---------------- main ---------------- */
  .main{min-width:0;display:flex;flex-direction:column}
  .topbar{
    position:sticky;top:0;z-index:20;
    display:flex;align-items:center;gap:1rem;
    padding:.7rem 1.5rem;
    background:color-mix(in srgb,var(--bg) 88%,transparent);
    backdrop-filter:blur(8px);
    border-bottom:1px solid var(--line);
  }
  .crumbs{display:flex;align-items:center;gap:.5rem;font-size:13px;color:var(--muted);min-width:0}
  .crumb-current{color:var(--fg)}
  .crumb-sep{color:var(--line-strong)}
  .top-actions{margin-left:auto;display:flex;align-items:center;gap:.5rem}

  .content{width:100%;max-width:var(--wide);margin:0 auto;padding:1.75rem 1.5rem 4rem}
  .pagehead{margin-bottom:1.75rem}
  .eyebrow{font-family:var(--font-mono);font-size:10.5px;text-transform:uppercase;letter-spacing:.16em;color:var(--muted);margin:0 0 .5rem}
  .page-title{font-family:var(--font-display);font-weight:400;font-size:clamp(1.9rem,3vw,2.35rem);line-height:1.05;margin:0}
  .page-sub{margin:.5rem 0 0;color:var(--muted);max-width:var(--measure)}
  .notice{min-height:1.3rem;margin:.5rem 0 0;color:var(--muted)}

  .meta{display:flex;flex-wrap:wrap;gap:0;margin-top:1.4rem;border-top:1px solid var(--line);border-bottom:1px solid var(--line)}
  .meta div{flex:1 1 120px;padding:.7rem .9rem .7rem 0;display:flex;flex-direction:column;gap:.15rem}
  .meta div+div{border-left:1px solid var(--line);padding-left:.9rem}
  .meta dt{font-family:var(--font-mono);font-size:10px;text-transform:uppercase;letter-spacing:.13em;color:var(--muted);margin:0}
  .meta dd{margin:0;font-family:var(--font-display);font-size:1.35rem;line-height:1}

  /* ---------------- sections ---------------- */
  .section{margin-top:2.5rem;scroll-margin-top:4.5rem}
  .section-head{display:flex;align-items:flex-end;gap:1rem;margin-bottom:.85rem}
  .section-title{font-size:15px;font-weight:400;margin:0;letter-spacing:.005em}
  .section-sub{margin:.2rem 0 0;color:var(--muted);font-size:13px;max-width:var(--measure)}
  .section-head .section-actions{margin-left:auto;display:flex;gap:.5rem}

  .panel{background:var(--surface);border:1px solid var(--line);border-radius:var(--radius-card);overflow:hidden}
  .panel-note{padding:.65rem .95rem;border-bottom:1px solid var(--line);color:var(--muted);font-size:12.5px}

  /* forms */
  .form{display:flex;flex-wrap:wrap;align-items:flex-end;gap:.75rem;padding:.95rem;border-bottom:1px solid var(--line);background:var(--surface)}
  .field{display:flex;flex-direction:column;gap:.3rem;min-width:0}
  .field>label{font-size:11px;color:var(--muted);letter-spacing:.02em}
  input[type=text],input[type=email],input[type=date],select{
    background:var(--bg);color:var(--fg);border:1px solid var(--line);
    border-radius:6px;padding:.42rem .55rem;font-size:13px;font-family:inherit;
    transition:border-color .12s var(--ease)
  }
  input::placeholder{color:#5f5b56}
  input:hover,select:hover{border-color:var(--line-strong)}
  input:focus,select:focus{border-color:var(--accent);outline:none}
  select{appearance:none;padding-right:1.5rem;
    background-image:linear-gradient(45deg,transparent 50%,var(--muted) 50%),linear-gradient(135deg,var(--muted) 50%,transparent 50%);
    background-position:calc(100% - 14px) 55%,calc(100% - 10px) 55%;
    background-size:4px 4px,4px 4px;background-repeat:no-repeat}

  /* buttons */
  .btn{
    display:inline-flex;align-items:center;justify-content:center;gap:.4rem;
    background:var(--surface-2);color:var(--fg);border:1px solid var(--line-strong);
    border-radius:6px;padding:.42rem .7rem;font-size:12.5px;cursor:pointer;white-space:nowrap;
    transition:background .12s var(--ease),border-color .12s var(--ease),color .12s var(--ease)
  }
  .btn:hover{background:#232320;border-color:#343430}
  .btn:disabled{opacity:.45;cursor:not-allowed}
  .btn-primary{background:var(--accent);border-color:var(--accent);color:var(--accent-fg)}
  .btn-primary:hover{background:#dfb552;border-color:#dfb552}
  .btn-quiet{background:transparent;border-color:var(--line);color:var(--muted)}
  .btn-quiet:hover{background:var(--surface-2);color:var(--fg)}
  .btn-danger{background:transparent;border-color:var(--line);color:var(--muted)}
  .btn-danger:hover{border-color:var(--danger);color:var(--danger);background:transparent}
  .btn-xs{padding:.28rem .5rem;font-size:11.5px}

  /* tables */
  .table-wrap{overflow-x:auto}
  table{width:100%;border-collapse:collapse}
  caption{text-align:left}
  th{
    text-align:left;font-weight:400;font-family:var(--font-mono);
    font-size:10px;text-transform:uppercase;letter-spacing:.13em;color:var(--muted);
    padding:.6rem .95rem;border-bottom:1px solid var(--line);white-space:nowrap
  }
  td{padding:.68rem .95rem;border-bottom:1px solid var(--line);font-size:13.5px;vertical-align:middle}
  tbody tr:last-child td{border-bottom:none}
  tbody tr{transition:background .1s var(--ease)}
  tbody tr:hover td{background:var(--surface-2)}
  td.num{font-family:var(--font-mono);font-size:12.5px;color:var(--muted)}
  .cell-strong{color:var(--fg)}
  .cell-muted{color:var(--muted)}
  .tags{display:flex;flex-wrap:wrap;gap:.3rem}
  .tag{font-family:var(--font-mono);font-size:11px;color:var(--muted);border:1px solid var(--line);border-radius:var(--radius-pill);padding:.05rem .45rem}
  .row-actions{display:flex;gap:.35rem;justify-content:flex-end}
  tr.is-revoked td{color:var(--muted);text-decoration:line-through;text-decoration-color:var(--line-strong)}
  tr.is-revoked .pill{text-decoration:none}

  /* status pills */
  .pill{
    --c:var(--muted);
    display:inline-flex;align-items:center;gap:.35rem;
    font-size:11.5px;color:var(--c);
    border:1px solid color-mix(in srgb,var(--c) 34%,transparent);
    background:color-mix(in srgb,var(--c) 11%,transparent);
    border-radius:var(--radius-pill);padding:.08rem .5rem;white-space:nowrap
  }
  .pill::before{content:"";width:5px;height:5px;border-radius:50%;background:currentColor;flex:none}
  .pill-ok{--c:var(--positive)}
  .pill-warn{--c:var(--accent)}
  .pill-bad{--c:var(--danger)}
  .pill-muted{--c:var(--muted)}

  /* filters + pager */
  .filters{display:flex;flex-wrap:wrap;align-items:flex-end;gap:.6rem;padding:.8rem .95rem;border-bottom:1px solid var(--line);background:var(--surface)}
  .pager{display:flex;align-items:center;gap:.75rem;padding:.65rem .95rem;border-top:1px solid var(--line);color:var(--muted);font-size:12.5px}
  .pager .btn{margin-left:auto}
  .pager .btn+.btn{margin-left:0}

  /* secret reveal */
  .secret{
    display:flex;flex-wrap:wrap;align-items:center;gap:.75rem;
    margin:.95rem;padding:.75rem .85rem;
    border:1px dashed var(--line-strong);border-radius:var(--radius-card);
    background:var(--surface-2)
  }
  .secret-label{font-size:12.5px;color:var(--muted);flex:1 1 220px;min-width:0}
  .secret code{font-family:var(--font-mono);font-size:12.5px;color:var(--fg);background:var(--bg);border:1px solid var(--line);border-radius:6px;padding:.3rem .5rem;overflow-wrap:anywhere}

  /* empty state */
  .empty{padding:2.25rem 1rem;text-align:center;color:var(--muted)}
  .empty strong{display:block;color:var(--fg);font-weight:400;font-size:14px;margin-bottom:.25rem}
  .empty .btn{margin-top:.8rem}

  /* sign-in (secondary state) */
  .signin-panel{background:var(--surface);border:1px solid var(--line);border-radius:var(--radius-panel);padding:1.6rem;max-width:26rem}
  .signin-panel h2{font-family:var(--font-display);font-weight:400;font-size:1.4rem;margin:.1rem 0 .35rem}
  .signin-panel p{margin:0 0 1rem;color:var(--muted);font-size:13px}
  .signin-panel .form{border:0;padding:0;background:transparent;flex-direction:column;align-items:stretch;gap:.7rem}
  .signin-panel .field{width:100%}
  .signin-panel .btn{width:100%}

  /* ---------------- responsive ---------------- */
  @media (max-width:1000px){
    .content{padding:1.5rem 1.1rem 3.5rem}
    .section{margin-top:2.1rem}
  }
  @media (max-width:860px){
    .app{grid-template-columns:1fr}
    .sidebar{position:static;height:auto;border-right:none;border-bottom:1px solid var(--line);padding:.9rem .9rem .7rem;gap:.7rem}
    .nav{flex-direction:row;flex-wrap:nowrap;overflow-x:auto;-webkit-overflow-scrolling:touch;scrollbar-width:none;gap:.25rem}
    .nav::-webkit-scrollbar{display:none}
    .nav-label{display:none}
    .nav-item{font-size:12.5px;padding:.32rem .55rem;border:1px solid var(--line);background:var(--surface);white-space:nowrap;flex:none}
    .nav-item[aria-current="true"]::before{display:none}
    .account{margin-top:.35rem;border-top:0;padding-top:0;flex-direction:row;align-items:center;justify-content:space-between}
    .topbar{padding:.6rem 1.1rem}
  }
  @media (max-width:720px){
    .page-title{font-size:1.75rem}
    .meta div{flex-basis:50%}
    .meta div:nth-child(3){border-left:0;padding-left:0}
    .meta div:nth-child(n+3){border-top:1px solid var(--line)}
    .table-wrap{overflow:visible}
    table,table tbody,table tr,table td{display:block;width:100%}
    thead{display:none}
    table tr{border-bottom:1px solid var(--line);padding:.7rem .95rem}
    table tr:last-child{border-bottom:none}
    table td{border:none;padding:.18rem 0;display:flex;justify-content:space-between;gap:1rem;align-items:baseline}
    table td::before{
      content:attr(data-label);flex:none;color:var(--muted);
      font-family:var(--font-mono);font-size:10px;text-transform:uppercase;letter-spacing:.12em
    }
    table td:empty{display:none}
    .row-actions{justify-content:flex-start;margin-top:.4rem}
    td .row-actions{margin-top:.3rem}
    .form,.filters{flex-direction:column;align-items:stretch}
    .form .btn,.filters .btn{width:100%}
    .section-head{flex-direction:column;align-items:flex-start}
    .section-head .section-actions{margin-left:0}
  }
  @media (prefers-reduced-motion:reduce){
    *{transition:none!important;animation:none!important;scroll-behavior:auto!important}
  }
</style>
</head>
<body>
<a class="skip" href="#main">Skip to content</a>
<div class="app">

  <aside class="sidebar" aria-label="Primary">
    <div class="brand">
      <span class="brand-mark">Herald</span>
      <span class="brand-sub">console</span>
    </div>

    <nav class="nav nav-strip" id="section-nav" aria-label="Dashboard sections">
      <p class="nav-label">Workspace</p>
      <a class="nav-item" href="#overview" aria-current="true">Overview</a>
      <a class="nav-item" href="#projects">Projects <span class="nav-count" id="count-projects">0</span></a>
      <a class="nav-item" href="#tokens">Tokens <span class="nav-count" id="count-tokens">0</span></a>
      <a class="nav-item" href="#destinations">Destinations <span class="nav-count" id="count-destinations">0</span></a>
      <p class="nav-label">Delivery</p>
      <a class="nav-item" href="#events">Events</a>
      <a class="nav-item" href="#deliveries">Deliveries</a>
    </nav>

    <div class="account">
      <div class="account-id">
        <span class="avatar" id="account-avatar" aria-hidden="true">H</span>
        <span class="account-email" id="account-email">signed out</span>
      </div>
      <button class="btn btn-quiet" type="button" id="signout" hidden>Sign out</button>
    </div>
  </aside>

  <main class="main" id="main">
    <header class="topbar">
      <div class="crumbs">
        <span>Herald</span><span class="crumb-sep">/</span><span class="crumb-current">Dashboard</span>
      </div>
      <div class="top-actions">
        <button class="btn btn-primary" type="button" id="jump-new-project">New project</button>
      </div>
    </header>

    <div class="content">

      <section class="pagehead" id="overview" aria-labelledby="overview-title">
        <p class="eyebrow">Herald console</p>
        <h1 class="page-title" id="overview-title">Dashboard</h1>
        <p class="page-sub">Projects, tokens and destinations for your Herald workspace, plus the event stream and delivery diagnostics for everything you send.</p>
        <dl class="meta">
          <div><dt>Projects</dt><dd id="stat-projects">0</dd></div>
          <div><dt>Active tokens</dt><dd id="stat-tokens">0</dd></div>
          <div><dt>Destinations</dt><dd id="stat-destinations">0</dd></div>
          <div><dt>Recent events</dt><dd id="stat-events">0</dd></div>
        </dl>
      </section>

      <p id="notice" class="notice" role="status" aria-live="polite"></p>

      <section class="section" id="signin" hidden aria-labelledby="signin-h">
        <div class="signin-panel">
          <p class="eyebrow">Herald</p>
          <h2 id="signin-h">Sign in</h2>
          <p>Sign in with a single-use email link.</p>
          <form id="signin-form" class="form" aria-label="Sign in">
            <div class="field">
              <label for="signin-email">Email address</label>
              <input id="signin-email" name="email" type="email" placeholder="you@example.com" autocomplete="email" required>
            </div>
            <button class="btn btn-primary" type="submit">Email me a link</button>
          </form>
        </div>
      </section>

      <div id="dash" hidden>

        <div class="panel" id="csrfboot" hidden>
          <div class="panel-note">Paste the CSRF token returned by sign-in to enable changes.</div>
          <div class="form">
            <div class="field">
              <label for="csrfinput">CSRF token</label>
              <input id="csrfinput" type="text" autocomplete="off" placeholder="csrf token">
            </div>
            <button id="csrfsave" class="btn" type="button">Save</button>
          </div>
        </div>

        <section class="section" id="projects" aria-labelledby="projects-h">
          <div class="section-head">
            <div>
              <h2 class="section-title" id="projects-h">Projects</h2>
              <p class="section-sub">A project is the unit tokens and destinations are scoped to.</p>
            </div>
          </div>
          <div class="panel">
            <form id="new-project" class="form" aria-label="Create project">
              <div class="field">
                <label for="p-slug">Slug</label>
                <input id="p-slug" name="id" type="text" placeholder="new-project" pattern="[a-z0-9-]+" autocomplete="off" required>
              </div>
              <div class="field">
                <label for="p-name">Display name</label>
                <input id="p-name" name="name" type="text" placeholder="New Project" autocomplete="off" required>
              </div>
              <button class="btn btn-primary" type="submit">Create project</button>
            </form>
            <div class="table-wrap">
              <table>
                <caption class="sr-only">Projects in this workspace, with their slug, creation date and row actions.</caption>
                <thead><tr><th scope="col">Project</th><th scope="col">Slug</th><th scope="col">Created</th><th scope="col"><span class="sr-only">Actions</span></th></tr></thead>
                <tbody id="projects"></tbody>
              </table>
            </div>
          </div>
        </section>

        <section class="section" id="tokens" aria-labelledby="tokens-h">
          <div class="section-head">
            <div>
              <h2 class="section-title" id="tokens-h">Tokens</h2>
              <p class="section-sub">Bearer credentials for the API. A secret is shown once at creation; copy it before leaving.</p>
            </div>
          </div>
          <div class="panel">
            <div class="secret" id="secret" role="status" hidden>
              <span class="secret-label">New token created — copy it now, it will not be shown again.</span>
              <code id="secret-value"></code>
              <button class="btn btn-xs" type="button" id="secret-copy">Copy</button>
            </div>
            <form id="new-scoped" class="form" aria-label="Create token">
              <div class="field" style="flex:1 1 16rem">
                <label for="t-projects">Projects</label>
                <input id="t-projects" name="projects" type="text" placeholder="project-a,project-b" autocomplete="off" required>
              </div>
              <button class="btn btn-primary" type="submit">Create multi-project token</button>
            </form>
            <div class="table-wrap">
              <table>
                <caption class="sr-only">API tokens, their kind, the projects they can reach, last use and status.</caption>
                <thead><tr><th scope="col">Id</th><th scope="col">Kind</th><th scope="col">Projects</th><th scope="col">Created</th><th scope="col">Last used</th><th scope="col">Status</th><th scope="col"><span class="sr-only">Actions</span></th></tr></thead>
                <tbody id="tokens"></tbody>
              </table>
            </div>
          </div>
        </section>

        <section class="section" id="destinations" aria-labelledby="dest-h">
          <div class="section-head">
            <div>
              <h2 class="section-title" id="dest-h">Destinations</h2>
              <p class="section-sub">Where Herald delivers events. Email sinks must verify before they receive.</p>
            </div>
          </div>
          <div class="panel">
            <form id="new-destination" class="form" aria-label="Add a destination">
              <div class="field">
                <label for="dst-project">Project</label>
                <select id="dst-project" name="project" required></select>
              </div>
              <div class="field">
                <label for="dst-kind">Kind</label>
                <select id="dst-kind" name="kind">
                  <option value="email">email</option>
                  <option value="webhook">webhook</option>
                </select>
              </div>
              <div class="field" style="flex:1 1 16rem">
                <label for="dst-target">Target</label>
                <input id="dst-target" name="target" type="text" placeholder="you@example.com or https://…" autocomplete="off" required>
              </div>
              <button class="btn btn-primary" type="submit">Add destination</button>
            </form>
            <div class="panel-note">Email destinations must be verified — click the link we email — before they receive anything. Webhooks are trusted as configured.</div>
            <div class="table-wrap">
              <table>
                <caption class="sr-only">Destinations across your projects, with kind, target and verification status.</caption>
                <thead><tr><th scope="col">Project</th><th scope="col">Kind</th><th scope="col">Target</th><th scope="col">Status</th><th scope="col"><span class="sr-only">Actions</span></th></tr></thead>
                <tbody id="destinations"></tbody>
              </table>
            </div>
          </div>
        </section>

        <section class="section" id="events" aria-labelledby="events-h">
          <div class="section-head">
            <div>
              <h2 class="section-title" id="events-h">Recent events</h2>
              <p class="section-sub">Everything your agents reported, newest first. Filter by project, status or date.</p>
            </div>
          </div>
          <div class="panel">
            <form id="events-filter" class="filters" aria-label="Filter events">
              <div class="field">
                <label for="ev-project">Project</label>
                <select id="ev-project" name="project"><option value="">All projects</option></select>
              </div>
              <div class="field">
                <label for="ev-status">Status</label>
                <select id="ev-status" name="status">
                  <option value="">Any</option>
                  <option value="success">success</option>
                  <option value="failure">failure</option>
                  <option value="blocked">blocked</option>
                </select>
              </div>
              <div class="field">
                <label for="ev-since">From</label>
                <input id="ev-since" name="since" type="date">
              </div>
              <div class="field">
                <label for="ev-until">To</label>
                <input id="ev-until" name="until" type="date">
              </div>
              <button class="btn btn-quiet" type="submit">Apply filters</button>
            </form>
            <div class="table-wrap">
              <table>
                <caption class="sr-only">Your tenant's recent events, filterable by project and status.</caption>
                <thead><tr><th scope="col">When</th><th scope="col">Project</th><th scope="col">Status</th><th scope="col">Title</th><th scope="col">Agent</th></tr></thead>
                <tbody id="events"></tbody>
              </table>
            </div>
            <div class="pager">
              <span id="events-page" role="status" aria-live="polite"></span>
              <button id="events-prev" class="btn btn-xs" type="button" disabled>Previous</button>
              <button id="events-next" class="btn btn-xs" type="button" disabled>Next</button>
            </div>
          </div>
        </section>

        <section class="section" id="deliveries" aria-labelledby="del-h">
          <div class="section-head">
            <div>
              <h2 class="section-title" id="del-h">Delivery attempts</h2>
              <p class="section-sub">Per sink, per event. Dead deliveries exhausted their retries. Last error is redacted.</p>
            </div>
          </div>
          <div class="panel">
            <form id="deliveries-filter" class="filters" aria-label="Filter delivery attempts">
              <div class="field">
                <label for="del-project">Project</label>
                <select id="del-project" name="project"><option value="">All projects</option></select>
              </div>
              <div class="field">
                <label for="del-state">State</label>
                <select id="del-state" name="state">
                  <option value="">Any</option>
                  <option value="pending">pending</option>
                  <option value="inflight">inflight</option>
                  <option value="delivered">delivered</option>
                  <option value="dead">dead</option>
                </select>
              </div>
              <div class="field">
                <label for="del-since">From</label>
                <input id="del-since" name="since" type="date">
              </div>
              <div class="field">
                <label for="del-until">To</label>
                <input id="del-until" name="until" type="date">
              </div>
              <button class="btn btn-quiet" type="submit">Apply filters</button>
            </form>
            <div class="table-wrap">
              <table>
                <caption class="sr-only">Your tenant's delivery attempts, newest first, by when, event id, sink, state, attempt count and last error.</caption>
                <thead><tr><th scope="col">When</th><th scope="col">Event</th><th scope="col">Sink</th><th scope="col">State</th><th scope="col">Attempts</th><th scope="col">Last error</th></tr></thead>
                <tbody id="deliveries"></tbody>
              </table>
            </div>
            <div class="pager">
              <span id="deliveries-page" role="status" aria-live="polite"></span>
              <button id="deliveries-prev" class="btn btn-xs" type="button" disabled>Previous</button>
              <button id="deliveries-next" class="btn btn-xs" type="button" disabled>Next</button>
            </div>
          </div>
        </section>

      </div>
    </div>
  </main>
</div>

<script>
(function () {
  "use strict";
  var KEY = "herald_csrf";
  var csrf = sessionStorage.getItem(KEY);
  var m = location.hash.match(/csrf=([^&]+)/);
  if (m) {
    csrf = decodeURIComponent(m[1]);
    sessionStorage.setItem(KEY, csrf);
    history.replaceState(null, "", location.pathname);
  }
  var API = location.pathname.replace(/\/dashboard$/, "");

  var $ = function (id) { return document.getElementById(id); };
  function notice(msg) { $("notice").textContent = msg || ""; }
  function showSignin() {
    $("signin").hidden = false;
    $("dash").hidden = true;
    $("signout").hidden = true;
    $("account-email").textContent = "signed out";
  }
  function showDash() { $("signin").hidden = true; $("dash").hidden = false; }
  function refreshCsrfUi() { $("csrfboot").hidden = !!csrf; }
  function api(method, path, body) {
    return fetch(API + path, {
      method: method,
      headers: {"Content-Type": "application/json", "X-CSRF-Token": csrf || ""},
      body: body ? JSON.stringify(body) : undefined
    }).then(function (r) {
      if (r.status === 401) { showSignin(); throw new Error("not signed in"); }
      if (r.status === 204) { return null; }
      return r.text().then(function (t) {
        var data = {};
        try { data = t ? JSON.parse(t) : {}; } catch (e) { throw new Error("HTTP " + r.status); }
        if (!r.ok) { throw new Error((data.error && data.error.message) || ("HTTP " + r.status)); }
        return data;
      });
    });
  }

  var el = function (tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) { n.className = cls; }
    if (text !== undefined && text !== null) { n.textContent = text; }
    return n;
  };
  function mono(text) { return el("span", "mono", text); }
  function cell(label, cls) {
    var td = el("td", cls);
    if (label) { td.setAttribute("data-label", label); }
    return td;
  }
  function put(tr, label, cls, child) {
    var td = cell(label, cls);
    if (typeof child === "string") { td.textContent = child; }
    else if (child) { td.appendChild(child); }
    tr.appendChild(td);
  }
  function tags(names) {
    var wrap = el("div", "tags");
    names.forEach(function (n) { wrap.appendChild(el("span", "tag", n)); });
    return wrap;
  }
  function pill(state) {
    var cls = "pill pill-muted";
    if (["success", "delivered", "active", "verified"].indexOf(state) >= 0) { cls = "pill pill-ok"; }
    else if (["blocked", "pending", "inflight", "unverified"].indexOf(state) >= 0) { cls = "pill pill-warn"; }
    else if (["failure", "dead", "revoked", "disabled"].indexOf(state) >= 0) { cls = "pill pill-bad"; }
    return el("span", cls, state);
  }
  function actions(buttons) {
    var wrap = el("div", "row-actions");
    buttons.forEach(function (b) { wrap.appendChild(b); });
    return wrap;
  }
  function button(label, fn, cls) {
    var b = el("button", cls || "btn btn-xs", label);
    b.type = "button";
    b.addEventListener("click", function () { Promise.resolve(fn()).catch(function (e) { notice(e.message); }); });
    return b;
  }
  function statCount(id, n) { var node = $(id); if (node) { node.textContent = String(n); } }

  function showToken(data) {
    var box = $("secret");
    $("secret-value").textContent = data.token;
    box.hidden = false;
    notice("");
    box.scrollIntoView({block: "nearest"});
  }
  $("secret-copy").addEventListener("click", function () {
    var text = $("secret-value").textContent;
    if (navigator.clipboard) { navigator.clipboard.writeText(text).then(function () { notice("Secret copied."); }, function () {}); }
  });

  function reload() { return load(); }

  function renderProjects(projects) {
    var body = $("projects");
    body.textContent = "";
    projects.forEach(function (p) {
      var tr = el("tr");
      put(tr, "Project", null, el("span", "cell-strong", p.id));
      put(tr, "Slug", null, mono(p.id));
      put(tr, "Created", "cell-muted", new Date(p.createdAt).toLocaleString());
      var act = cell("Actions");
      act.appendChild(actions([
        button("Create token", function () {
          return api("POST", "/v1/projects/" + encodeURIComponent(p.id) + "/token").then(showToken);
        }),
        button("Rotate", function () {
          return api("POST", "/v1/projects/" + encodeURIComponent(p.id) + "/token/rotate").then(showToken).then(reload);
        }),
        button("Delete", function () {
          if (!confirm("Delete project " + p.id + "?")) { return Promise.resolve(); }
          return api("DELETE", "/v1/projects/" + encodeURIComponent(p.id)).then(reload);
        }, "btn btn-xs btn-danger")
      ]));
      tr.appendChild(act);
      body.appendChild(tr);
    });
    statCount("count-projects", projects.length);
  }

  function renderTokens(tokens) {
    var body = $("tokens");
    body.textContent = "";
    tokens.forEach(function (t) {
      var tr = el("tr");
      if (t.revokedAt) { tr.className = "is-revoked"; }
      put(tr, "Id", "mono", t.id.slice(0, 12) + "\u2026");
      put(tr, "Kind", null, mono(t.kind));
      put(tr, "Projects", null, tags(t.projects || []));
      put(tr, "Created", "cell-muted", new Date(t.createdAt).toLocaleString());
      put(tr, "Last used", "cell-muted", t.lastUsedAt ? new Date(t.lastUsedAt).toLocaleString() : "never");
      put(tr, "Status", null, pill(t.revokedAt ? "revoked" : "active"));
      var act = cell("Actions");
      if (!t.revokedAt) {
        act.appendChild(actions([button("Revoke", function () {
          return api("DELETE", "/v1/tokens/" + encodeURIComponent(t.id)).then(reload);
        }, "btn btn-xs btn-danger")]));
      }
      tr.appendChild(act);
      body.appendChild(tr);
    });
    statCount("count-tokens", tokens.filter(function (t) { return !t.revokedAt; }).length);
  }

  function emptyRow(cols, title, hint, reset) {
    var tr = el("tr");
    var td = document.createElement("td");
    td.colSpan = cols;
    var box = el("div", "empty");
    box.appendChild(el("strong", null, title));
    box.appendChild(document.createTextNode(hint));
    var row = el("div");
    row.appendChild(button("Reset filters", reset, "btn btn-quiet"));
    box.appendChild(row);
    td.appendChild(box);
    tr.appendChild(td);
    return tr;
  }

  function renderEvents(events) {
    var body = $("events");
    body.textContent = "";
    if (!events.length) {
      body.appendChild(emptyRow(5, "No events match these filters", "Try a wider time range, or reset the filters to see everything.", resetEvents));
      return;
    }
    events.forEach(function (e) {
      var tr = el("tr");
      put(tr, "When", "cell-muted", new Date(e.createdAt).toLocaleString());
      put(tr, "Project", null, el("span", "cell-strong", e.project));
      put(tr, "Status", null, pill(e.status));
      put(tr, "Title", null, e.title);
      put(tr, "Agent", null, mono(e.agent));
      body.appendChild(tr);
    });
  }

  function renderDeliveries(rows) {
    var body = $("deliveries");
    body.textContent = "";
    if (!rows.length) {
      body.appendChild(emptyRow(6, "No delivery attempts match these filters", "Try a wider time range, or reset the filters to see everything.", resetDeliveries));
      return;
    }
    rows.forEach(function (d) {
      var tr = el("tr");
      put(tr, "When", "cell-muted", new Date(d.updatedAt).toLocaleString());
      put(tr, "Event", null, mono(d.eventId));
      put(tr, "Sink", null, mono(d.sink));
      put(tr, "State", null, pill(d.state));
      put(tr, "Attempts", "num", String(d.attempts));
      var errTd = cell("Last error", "cell-muted");
      errTd.appendChild(mono(d.lastError || "\u2014"));
      if (d.lastError) { errTd.title = d.lastError; }
      tr.appendChild(errTd);
      body.appendChild(tr);
    });
  }

  function renderDestinations(list) {
    var body = $("destinations");
    body.textContent = "";
    if (!list.length) {
      body.appendChild(emptyRow(5, "No destinations yet", "Add one above to start receiving events.", function () { return Promise.resolve(); }));
      return;
    }
    list.forEach(function (d) {
      var tr = el("tr");
      put(tr, "Project", null, el("span", "cell-strong", d.project));
      put(tr, "Kind", null, mono(d.kind));
      put(tr, "Target", null, mono(d.target));
      put(tr, "Status", null, pill(d.verified ? "verified" : (d.enabled ? "unverified" : "disabled")));
      var act = cell("Actions");
      var btns = [];
      if (d.kind === "email") {
        btns.push(button("Re-send verification", function () {
          return api("POST", "/v1/projects/" + encodeURIComponent(d.project) + "/destinations/" + encodeURIComponent(d.id) + "/verify")
            .then(function () { notice("Verification link sent to " + d.target); });
        }));
      }
      btns.push(button("Delete", function () {
        if (!confirm("Delete destination " + d.target + "?")) { return Promise.resolve(); }
        return api("DELETE", "/v1/projects/" + encodeURIComponent(d.project) + "/destinations/" + encodeURIComponent(d.id)).then(reload);
      }, "btn btn-xs btn-danger"));
      act.appendChild(actions(btns));
      tr.appendChild(act);
      body.appendChild(tr);
    });
    statCount("count-destinations", list.length);
  }

  function qs(params) {
    var parts = [];
    Object.keys(params).forEach(function (k) {
      var v = params[k];
      if (v !== "" && v !== null && v !== undefined) {
        parts.push(encodeURIComponent(k) + "=" + encodeURIComponent(v));
      }
    });
    return parts.length ? "?" + parts.join("&") : "";
  }

  var eventsOffset = 0, deliveriesOffset = 0, eventsHasMore = false, deliveriesHasMore = false;
  var PAGE = 25;

  function fillProjectSelects(projects) {
    ["ev-project", "del-project"].forEach(function (id) {
      var sel = $(id);
      while (sel.options.length > 1) { sel.remove(1); }
      projects.forEach(function (p) {
        var o = document.createElement("option");
        o.value = p.id;
        o.textContent = p.id;
        sel.appendChild(o);
      });
    });
    var dst = $("dst-project");
    dst.textContent = "";
    projects.forEach(function (p) {
      var o = document.createElement("option");
      o.value = p.id;
      o.textContent = p.id;
      dst.appendChild(o);
    });
  }

  function loadEvents(reset) {
    if (reset) { eventsOffset = 0; }
    var params = {
      project: $("ev-project").value,
      status: $("ev-status").value,
      since: $("ev-since").value,
      until: $("ev-until").value,
      limit: PAGE,
      offset: eventsOffset
    };
    return api("GET", "/v1/events" + qs(params)).then(function (d) {
      var events = d.events || [];
      renderEvents(events);
      eventsHasMore = !!(d.page && d.page.hasMore);
      $("events-prev").disabled = eventsOffset <= 0;
      $("events-next").disabled = !eventsHasMore;
      $("events-page").textContent = events.length + " events at offset " + eventsOffset;
      statCount("stat-events", events.length);
    });
  }

  function loadDeliveries(reset) {
    if (reset) { deliveriesOffset = 0; }
    var params = {
      project: $("del-project").value,
      state: $("del-state").value,
      since: $("del-since").value,
      until: $("del-until").value,
      limit: PAGE,
      offset: deliveriesOffset
    };
    return api("GET", "/v1/deliveries" + qs(params)).then(function (d) {
      var rows = d.deliveries || [];
      renderDeliveries(rows);
      deliveriesHasMore = !!(d.page && d.page.hasMore);
      $("deliveries-prev").disabled = deliveriesOffset <= 0;
      $("deliveries-next").disabled = !deliveriesHasMore;
      $("deliveries-page").textContent = rows.length + " attempts at offset " + deliveriesOffset;
    });
  }

  function resetEvents() {
    $("ev-project").value = "";
    $("ev-status").value = "";
    $("ev-since").value = "";
    $("ev-until").value = "";
    return loadEvents(true);
  }
  function resetDeliveries() {
    $("del-project").value = "";
    $("del-state").value = "";
    $("del-since").value = "";
    $("del-until").value = "";
    return loadDeliveries(true);
  }

  function loadDestinations(projects) {
    if (!projects.length) { renderDestinations([]); return Promise.resolve(); }
    return Promise.all(projects.map(function (p) {
      return api("GET", "/v1/projects/" + encodeURIComponent(p.id) + "/destinations")
        .then(function (d) {
          return (d.destinations || []).map(function (x) { x.project = p.id; return x; });
        })
        .catch(function () { return []; });
    })).then(function (lists) {
      var all = [];
      lists.forEach(function (l) { all = all.concat(l); });
      renderDestinations(all);
    });
  }

  function setAccount(user) {
    if (!user) { return; }
    var email = user.email || "";
    $("account-email").textContent = email;
    $("account-avatar").textContent = (email.charAt(0) || "H").toUpperCase();
    $("signout").hidden = false;
  }

  function load() {
    return Promise.all([
      api("GET", "/v1/projects").then(function (d) {
        showDash();
        var projects = d.projects || [];
        renderProjects(projects);
        fillProjectSelects(projects);
        return loadDestinations(projects);
      }),
      api("GET", "/v1/tokens").then(function (d) { renderTokens(d.tokens || []); }),
      api("GET", "/v1/me").then(function (d) { setAccount(d.user); }),
      loadEvents(true),
      loadDeliveries(true)
    ]).catch(function (e) { notice(e.message); });
  }

  $("signin-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    var f = ev.target;
    fetch(API + "/v1/auth/magic-link", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({email: f.email.value})})
      .then(function (r) { if (!r.ok) { throw new Error("HTTP " + r.status); } notice("Check your inbox for a sign-in link."); })
      .catch(function (e) { notice(e.message); });
  });

  $("new-project").addEventListener("submit", function (ev) {
    ev.preventDefault();
    var f = ev.target;
    api("POST", "/v1/projects", {id: f.elements["id"].value, name: f.elements["name"].value}).then(function () {
      f.reset();
      return reload();
    }).catch(function (e) { notice(e.message); });
  });

  $("new-scoped").addEventListener("submit", function (ev) {
    ev.preventDefault();
    var f = ev.target;
    var projects = f.projects.value.split(",").map(function (s) { return s.trim(); }).filter(Boolean);
    api("POST", "/v1/tokens", {projects: projects}).then(showToken).catch(function (e) { notice(e.message); });
  });

  $("new-destination").addEventListener("submit", function (ev) {
    ev.preventDefault();
    var f = ev.target;
    api("POST", "/v1/projects/" + encodeURIComponent(f.project.value) + "/destinations", {kind: f.kind.value, target: f.target.value})
      .then(function () { f.target.value = ""; notice("Destination added. If it is an email, click the verification link we send."); return reload(); })
      .catch(function (e) { notice(e.message); });
  });

  $("events-filter").addEventListener("submit", function (ev) {
    ev.preventDefault();
    loadEvents(true).catch(function (e) { notice(e.message); });
  });
  $("events-prev").addEventListener("click", function () {
    eventsOffset = Math.max(0, eventsOffset - PAGE);
    loadEvents(false).catch(function (e) { notice(e.message); });
  });
  $("events-next").addEventListener("click", function () {
    if (eventsHasMore) { eventsOffset += PAGE; }
    loadEvents(false).catch(function (e) { notice(e.message); });
  });

  $("deliveries-filter").addEventListener("submit", function (ev) {
    ev.preventDefault();
    loadDeliveries(true).catch(function (e) { notice(e.message); });
  });
  $("deliveries-prev").addEventListener("click", function () {
    deliveriesOffset = Math.max(0, deliveriesOffset - PAGE);
    loadDeliveries(false).catch(function (e) { notice(e.message); });
  });
  $("deliveries-next").addEventListener("click", function () {
    if (deliveriesHasMore) { deliveriesOffset += PAGE; }
    loadDeliveries(false).catch(function (e) { notice(e.message); });
  });

  $("signout").addEventListener("click", function () {
    api("POST", "/v1/auth/signout").then(function () {
      sessionStorage.removeItem(KEY);
      csrf = "";
      showSignin();
      notice("Signed out.");
    }).catch(function (e) { notice(e.message); });
  });

  $("jump-new-project").addEventListener("click", function () {
    var n = $("p-slug");
    if (n) { n.scrollIntoView({block: "center"}); n.focus(); }
  });

  $("csrfsave").addEventListener("click", function () {
    csrf = $("csrfinput").value.trim();
    if (csrf) { sessionStorage.setItem(KEY, csrf); refreshCsrfUi(); notice(""); }
  });

  refreshCsrfUi();
  load();
})();
</script>
</body>
</html>`))
