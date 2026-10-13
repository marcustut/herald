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
	mux.HandleFunc("GET /cloud/projects", c.handleProjectsPage)
	mux.HandleFunc("GET /cloud/v1/projects", c.handleListProjects)
	mux.HandleFunc("POST /cloud/v1/projects", c.handleCreateProject)
	mux.HandleFunc("DELETE /cloud/v1/projects/{project}", c.handleDeleteProject)
	mux.HandleFunc("GET /cloud/v1/tokens", c.handleListTokens)
	mux.HandleFunc("POST /cloud/v1/tokens", c.handleCreateScopedToken)
	mux.HandleFunc("DELETE /cloud/v1/tokens/{id}", c.handleRevokeToken)
	mux.HandleFunc("POST /cloud/v1/projects/{project}/token", c.handleCreateProjectToken)
	mux.HandleFunc("POST /cloud/v1/projects/{project}/token/rotate", c.handleRotateProjectToken)
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
	if _, ok := c.session(w, r); !ok {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'")
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
*{box-sizing:border-box}
body{font-family:system-ui,sans-serif;max-width:52rem;margin:2rem auto;padding:0 1rem;color:#111}
table{border-collapse:collapse;width:100%;margin:1rem 0}
th,td{text-align:left;border-bottom:1px solid #ddd;padding:.4rem .6rem;font-size:.9rem;vertical-align:top}
code,pre{font-family:ui-monospace,monospace}
pre{background:#f5f5f5;padding:.75rem;overflow:auto}
.row{display:flex;gap:.5rem;flex-wrap:wrap;margin:.5rem 0;align-items:center}
.revoked{color:#999;text-decoration:line-through}
#notice{min-height:1.2rem;color:#b00}
button{cursor:pointer;font:inherit;padding:.3rem .6rem;border:1px solid #bbb;border-radius:4px;background:#fff}
button:disabled{opacity:.5;cursor:not-allowed}
label{font-size:.85rem;color:#333}
input,select{font:inherit;padding:.3rem;border:1px solid #bbb;border-radius:4px;max-width:100%;background:#fff}
:focus-visible{outline:2px solid #06c;outline-offset:2px}
.skip{position:absolute;left:-999px}
.skip:focus{left:1rem;top:1rem;background:#fff;padding:.5rem;border:1px solid #06c;z-index:10}
.tablewrap{overflow-x:auto;-webkit-overflow-scrolling:touch}
caption{text-align:left;font-size:.85rem;color:#555;padding:.25rem 0}
.pager{display:flex;gap:.5rem;align-items:center;flex-wrap:wrap;margin:.25rem 0 .75rem}
.state-failure,.state-blocked,.state-dead{color:#b00}
.state-success,.state-delivered{color:#070}
.state-pending{color:#a60}
.state-inflight{color:#06c}
h2{margin-top:2rem}
@media (max-width:600px){
  body{margin:1rem auto}
  th,td{white-space:nowrap}
  h1{font-size:1.5rem}
}
</style>
</head>
<body>
<a class="skip" href="#main">Skip to content</a>
<h1 id="main">Herald dashboard</h1>
<p id="notice"></p>
<div id="csrfboot" hidden>
  <p>Paste the CSRF token returned by sign-in to enable changes:</p>
  <div class="row"><input id="csrfinput" placeholder="CSRF token"><button id="csrfsave" type="button">Save</button></div>
</div>
<h2>New project</h2>
<form id="new-project" class="row">
  <input name="id" placeholder="project-slug" pattern="[a-z0-9-]+" required>
  <input name="name" placeholder="Display name" required>
  <button type="submit">Create</button>
</form>
<h2>Projects</h2>
<table><thead><tr><th>Project</th><th>Name</th><th>Created</th><th></th></tr></thead>
<tbody id="projects"></tbody></table>
<h2>Tokens</h2>
<p>Create a project token from the project row, or a multi-project token below. A secret is shown once; copy it before leaving.</p>
<form id="new-scoped" class="row">
  <input name="projects" placeholder="project-a,project-b" required>
  <button type="submit">Create multi-project token</button>
</form>
<table><thead><tr><th>Id</th><th>Kind</th><th>Projects</th><th>Created</th><th>Last used</th><th></th></tr></thead>
<tbody id="tokens"></tbody></table>
<pre id="raw" hidden></pre>

<h2>Recent events</h2>
<form id="events-filter" class="row" aria-label="Filter events">
  <label for="ev-project">Project</label>
  <select id="ev-project" name="project"><option value="">All projects</option></select>
  <label for="ev-status">Status</label>
  <select id="ev-status" name="status">
    <option value="">Any</option>
    <option value="success">success</option>
    <option value="failure">failure</option>
    <option value="blocked">blocked</option>
  </select>
  <label for="ev-since">From</label>
  <input id="ev-since" name="since" type="date">
  <label for="ev-until">To</label>
  <input id="ev-until" name="until" type="date">
  <button type="submit">Apply filters</button>
</form>
<div class="tablewrap">
<table><caption>Your tenant's recent events</caption>
<thead><tr><th>When</th><th>Project</th><th>Status</th><th>Title</th><th>Agent</th></tr></thead>
<tbody id="events"></tbody></table>
</div>
<div class="pager">
  <button id="events-prev" type="button">Previous</button>
  <span id="events-page" role="status" aria-live="polite"></span>
  <button id="events-next" type="button">Next</button>
</div>

<h2>Delivery attempts</h2>
<form id="deliveries-filter" class="row" aria-label="Filter delivery attempts">
  <label for="del-project">Project</label>
  <select id="del-project" name="project"><option value="">All projects</option></select>
  <label for="del-state">State</label>
  <select id="del-state" name="state">
    <option value="">Any</option>
    <option value="pending">pending</option>
    <option value="inflight">inflight</option>
    <option value="delivered">delivered</option>
    <option value="dead">dead</option>
  </select>
  <label for="del-since">From</label>
  <input id="del-since" name="since" type="date">
  <label for="del-until">To</label>
  <input id="del-until" name="until" type="date">
  <button type="submit">Apply filters</button>
</form>
<div class="tablewrap">
<table><caption>Your tenant's delivery attempts, newest first. Last error is redacted.</caption>
<thead><tr><th>When</th><th>Event</th><th>Sink</th><th>State</th><th>Attempts</th><th>Last error</th></tr></thead>
<tbody id="deliveries"></tbody></table>
</div>
<div class="pager">
  <button id="deliveries-prev" type="button">Previous</button>
  <span id="deliveries-page" role="status" aria-live="polite"></span>
  <button id="deliveries-next" type="button">Next</button>
</div>

<h2>Destinations</h2>
<div class="tablewrap">
<table><caption>Destinations across your projects</caption>
<thead><tr><th>Project</th><th>Kind</th><th>Target</th><th>Status</th></tr></thead>
<tbody id="destinations"></tbody></table>
</div>
<script>
(function () {
  var KEY = "herald_csrf";
  var csrf = sessionStorage.getItem(KEY);
  var m = location.hash.match(/csrf=([^&]+)/);
  if (m) {
    csrf = decodeURIComponent(m[1]);
    sessionStorage.setItem(KEY, csrf);
    history.replaceState(null, "", location.pathname);
  }
  function notice(msg) { document.getElementById("notice").textContent = msg || ""; }
  function refreshCsrfUi() { document.getElementById("csrfboot").hidden = !!csrf; }
  document.getElementById("csrfsave").addEventListener("click", function () {
    csrf = document.getElementById("csrfinput").value.trim();
    if (csrf) { sessionStorage.setItem(KEY, csrf); refreshCsrfUi(); notice(""); }
  });
  refreshCsrfUi();
  function api(method, path, body) {
    return fetch(path, {
      method: method,
      headers: {"Content-Type": "application/json", "X-CSRF-Token": csrf || ""},
      body: body ? JSON.stringify(body) : undefined
    }).then(function (r) {
      if (r.status === 401) { location.reload(); throw new Error("not signed in"); }
      if (r.status === 204) { return null; }
      return r.json().then(function (data) {
        if (!r.ok) { throw new Error((data.error && data.error.message) || ("HTTP " + r.status)); }
        return data;
      });
    });
  }
  function td(text, cls) {
    var el = document.createElement("td");
    el.textContent = text;
    if (cls) { el.className = cls; }
    return el;
  }
  function button(label, fn) {
    var b = document.createElement("button");
    b.type = "button";
    b.textContent = label;
    b.addEventListener("click", function () { fn().catch(function (e) { notice(e.message); }); });
    return b;
  }
  function showToken(data) {
    var pre = document.getElementById("raw");
    pre.hidden = false;
    pre.textContent = "New " + data.kind + " token (shown once):\n" + data.token;
    notice("");
  }
  function reload() { return load(); }
  function renderProjects(projects) {
    var body = document.getElementById("projects");
    body.textContent = "";
    projects.forEach(function (p) {
      var tr = document.createElement("tr");
      tr.appendChild(td(p.id));
      tr.appendChild(td(p.name));
      tr.appendChild(td(new Date(p.createdAt).toLocaleString()));
      var actions = document.createElement("td");
      actions.appendChild(button("Create token", function () {
        return api("POST", "/cloud/v1/projects/" + encodeURIComponent(p.id) + "/token").then(showToken);
      }));
      actions.appendChild(button("Rotate", function () {
        return api("POST", "/cloud/v1/projects/" + encodeURIComponent(p.id) + "/token/rotate").then(showToken).then(reload);
      }));
      actions.appendChild(button("Delete", function () {
        if (!confirm("Delete project " + p.id + "?")) { return Promise.resolve(); }
        return api("DELETE", "/cloud/v1/projects/" + encodeURIComponent(p.id)).then(reload);
      }));
      tr.appendChild(actions);
      body.appendChild(tr);
    });
  }
  function renderTokens(tokens) {
    var body = document.getElementById("tokens");
    body.textContent = "";
    tokens.forEach(function (t) {
      var tr = document.createElement("tr");
      if (t.revokedAt) { tr.className = "revoked"; }
      tr.appendChild(td(t.id.slice(0, 12) + "\u2026"));
      tr.appendChild(td(t.kind));
      tr.appendChild(td((t.projects || []).join(", ")));
      tr.appendChild(td(new Date(t.createdAt).toLocaleString()));
      tr.appendChild(td(t.lastUsedAt ? new Date(t.lastUsedAt).toLocaleString() : "never"));
      var actions = document.createElement("td");
      if (!t.revokedAt) {
        actions.appendChild(button("Revoke", function () {
          return api("DELETE", "/cloud/v1/tokens/" + encodeURIComponent(t.id)).then(reload);
        }));
      }
      tr.appendChild(actions);
      body.appendChild(tr);
    });
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
      var sel = document.getElementById(id);
      while (sel.options.length > 1) { sel.remove(1); }
      projects.forEach(function (p) {
        var o = document.createElement("option");
        o.value = p.id;
        o.textContent = p.id;
        sel.appendChild(o);
      });
    });
  }
  function renderEvents(events) {
    var body = document.getElementById("events");
    body.textContent = "";
    events.forEach(function (e) {
      var tr = document.createElement("tr");
      tr.appendChild(td(new Date(e.createdAt).toLocaleString()));
      tr.appendChild(td(e.project));
      tr.appendChild(td(e.status, "state-" + e.status));
      tr.appendChild(td(e.title));
      tr.appendChild(td(e.agent));
      body.appendChild(tr);
    });
  }
  function loadEvents(reset) {
    if (reset) { eventsOffset = 0; }
    var params = {
      project: document.getElementById("ev-project").value,
      status: document.getElementById("ev-status").value,
      since: document.getElementById("ev-since").value,
      until: document.getElementById("ev-until").value,
      limit: PAGE,
      offset: eventsOffset
    };
    return api("GET", "/cloud/v1/events" + qs(params)).then(function (d) {
      var events = d.events || [];
      renderEvents(events);
      eventsHasMore = !!(d.page && d.page.hasMore);
      document.getElementById("events-prev").disabled = eventsOffset <= 0;
      document.getElementById("events-next").disabled = !eventsHasMore;
      document.getElementById("events-page").textContent = events.length + " events at offset " + eventsOffset;
    });
  }
  function renderDeliveries(rows) {
    var body = document.getElementById("deliveries");
    body.textContent = "";
    rows.forEach(function (d) {
      var tr = document.createElement("tr");
      tr.appendChild(td(new Date(d.updatedAt).toLocaleString()));
      tr.appendChild(td(d.eventId));
      tr.appendChild(td(d.sink));
      tr.appendChild(td(d.state, "state-" + d.state));
      tr.appendChild(td(String(d.attempts)));
      var err = td(d.lastError || "\u2014");
      if (d.lastError) { err.title = d.lastError; }
      tr.appendChild(err);
      body.appendChild(tr);
    });
  }
  function loadDeliveries(reset) {
    if (reset) { deliveriesOffset = 0; }
    var params = {
      project: document.getElementById("del-project").value,
      state: document.getElementById("del-state").value,
      since: document.getElementById("del-since").value,
      until: document.getElementById("del-until").value,
      limit: PAGE,
      offset: deliveriesOffset
    };
    return api("GET", "/cloud/v1/deliveries" + qs(params)).then(function (d) {
      var rows = d.deliveries || [];
      renderDeliveries(rows);
      deliveriesHasMore = !!(d.page && d.page.hasMore);
      document.getElementById("deliveries-prev").disabled = deliveriesOffset <= 0;
      document.getElementById("deliveries-next").disabled = !deliveriesHasMore;
      document.getElementById("deliveries-page").textContent = rows.length + " attempts at offset " + deliveriesOffset;
    });
  }
  function renderDestinations(list) {
    var body = document.getElementById("destinations");
    body.textContent = "";
    list.forEach(function (d) {
      var tr = document.createElement("tr");
      tr.appendChild(td(d.project));
      tr.appendChild(td(d.kind));
      tr.appendChild(td(d.target));
      tr.appendChild(td(d.verified ? "verified" : (d.enabled ? "unverified" : "disabled")));
      body.appendChild(tr);
    });
  }
  function loadDestinations(projects) {
    if (!projects.length) { renderDestinations([]); return Promise.resolve(); }
    return Promise.all(projects.map(function (p) {
      return api("GET", "/cloud/v1/projects/" + encodeURIComponent(p.id) + "/destinations")
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
  function load() {
    return Promise.all([
      api("GET", "/cloud/v1/projects").then(function (d) {
        var projects = d.projects || [];
        renderProjects(projects);
        fillProjectSelects(projects);
        return loadDestinations(projects);
      }),
      api("GET", "/cloud/v1/tokens").then(function (d) { renderTokens(d.tokens || []); }),
      loadEvents(true),
      loadDeliveries(true)
    ]).catch(function (e) { notice(e.message); });
  }
  document.getElementById("new-project").addEventListener("submit", function (ev) {
    ev.preventDefault();
    var f = ev.target;
    api("POST", "/cloud/v1/projects", {id: f.id.value, name: f.name.value}).then(function () {
      f.reset();
      return reload();
    }).catch(function (e) { notice(e.message); });
  });
  document.getElementById("new-scoped").addEventListener("submit", function (ev) {
    ev.preventDefault();
    var f = ev.target;
    var projects = f.projects.value.split(",").map(function (s) { return s.trim(); }).filter(Boolean);
    api("POST", "/cloud/v1/tokens", {projects: projects}).then(showToken).catch(function (e) { notice(e.message); });
  });
  document.getElementById("events-filter").addEventListener("submit", function (ev) {
    ev.preventDefault();
    loadEvents(true).catch(function (e) { notice(e.message); });
  });
  document.getElementById("events-prev").addEventListener("click", function () {
    eventsOffset = Math.max(0, eventsOffset - PAGE);
    loadEvents(false).catch(function (e) { notice(e.message); });
  });
  document.getElementById("events-next").addEventListener("click", function () {
    if (eventsHasMore) { eventsOffset += PAGE; }
    loadEvents(false).catch(function (e) { notice(e.message); });
  });
  document.getElementById("deliveries-filter").addEventListener("submit", function (ev) {
    ev.preventDefault();
    loadDeliveries(true).catch(function (e) { notice(e.message); });
  });
  document.getElementById("deliveries-prev").addEventListener("click", function () {
    deliveriesOffset = Math.max(0, deliveriesOffset - PAGE);
    loadDeliveries(false).catch(function (e) { notice(e.message); });
  });
  document.getElementById("deliveries-next").addEventListener("click", function () {
    if (deliveriesHasMore) { deliveriesOffset += PAGE; }
    loadDeliveries(false).catch(function (e) { notice(e.message); });
  });
  load();
})();
</script>
</body>
</html>`))
