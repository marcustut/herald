# Launch material

Everything needed to announce Herald's open-source release: a landing page, a
waitlist form, a launch post draft, and a demo script for developer
communities.

- [Landing page](#landing-page)
- [Waitlist form](#waitlist-form)
- [Privacy-preserving analytics](#privacy-preserving-analytics)
- [Launch post draft](#launch-post-draft)
- [Demo script](#demo-script)
- [Checklist](#checklist)

## Landing page

`site/` is a static, dependency-free page — no build step, no JavaScript, no
external fonts or CDNs:

```
site/index.html   # content: pitch, self-host vs Cloud, demo, waitlist, privacy
site/style.css    # ~200 lines, system fonts, light/dark via prefers-color-scheme
site/site_test.go # pins the acceptance criteria (links, inert form, no trackers)
```

### Publish on GitHub Pages

1. Repository **Settings → Pages**.
2. **Source: Deploy from a branch**, branch `main`, folder **`/site`**.
3. Save. The page is served at `https://marcustut.github.io/herald/`.

No workflow and no artifact upload are required — Pages renders the folder as
static files. To preview locally:

```sh
python3 -m http.server -d site 8000   # http://localhost:8000
```

The page links to docs by absolute GitHub URL, so it works both on Pages and
from any other static host.

## Waitlist form

The form in `site/index.html` is deliberately **inert**: `action=""` and no
`method`, so submitting reloads the page and no data leaves the browser. It is a
placeholder the operator wires up.

To collect addresses, choose one of:

- **Own endpoint (preferred).** Add a tiny handler to an existing service and
  point the form at it:
  ```html
  <form class="waitlist" action="https://herald.example.com/waitlist" method="post">
  ```
  It receives `email` and `consent=yes` as form fields. Store only the address,
  the consent flag, and a timestamp; never log the request body elsewhere.
- **Hosted form provider.** Paste the provider's endpoint into `action`. Prefer
  a provider that does not set cross-site cookies or resell data, and keep the
  consent checkbox required.

Whichever you pick, keep these properties, which the tests enforce by default:

- the form does not post anywhere until wired (`site/site_test.go` checks the
  action stays empty in the committed file);
- consent is an explicit, required checkbox;
- the privacy note links to `#privacy`.

When you wire the form, update the test's expectation or move the endpoint into
a template variable outside the repo; do not commit a live endpoint that leaks
an operator hostname. Use `example.com` in the committed file.

## Privacy-preserving analytics

The site ships with **no trackers**, so it satisfies the "measure visits →
docs → installation or signup without intrusive tracking" criterion by design.
To count usage, use one of these, in order of least data collected:

1. **Server access logs.** If you host the page yourself, count `GET /`, then
   count clicks to the quickstart/GitHub links from your docs host. No cookies,
   no client-side code, no personal data.
2. **Self-hosted, cookieless analytics.** Run Plausible or Umami on your own
   infrastructure with no cross-site cookies and no fingerprinting. Because it
   is first-party, it survives ad blockers and stays under your control.
3. **GitHub Pages only.** Pages provides no analytics; treat outbound clicks to
   `github.com/marcustut/herald` in your repo's traffic graph as the top of the
   funnel.

Define the funnel as: landing view → docs view → either `go install`/clone or
waitlist signup. Measure only aggregate counts, never per-person identifiers.

## Launch post draft

> **Herald — your coding agents email you when they finish**
>
> Coding agents now run for minutes or hours across many repos. When one
> finishes, you want to know — but wiring each agent to email or chat is
> fiddly, and letting an agent choose the recipient is a footgun.
>
> Herald is a small self-hosted Go gateway that fixes that. Agents `POST` one
> event over HTTPS with a Bearer token; the gateway authenticates the token,
> resolves the recipient **server-side**, redacts secret-looking strings, and
> queues the message in PostgreSQL. A separate dispatcher delivers it by SMTP or
> to an outbound webhook, with retries, backoff, a lease, and a dead state.
>
> Why self-host instead of a SaaS? Your notification content can contain repo
> names, PR links, and failing test output. Herald keeps that on your box. The
> only dependency is PostgreSQL; the binary has two Go module dependencies
> total.
>
> What it looks like:
>
> ```sh
> herald skill > ~/.claude/skills/herald-notify/SKILL.md
> herald send --status success --title "deploy finished"
> ```
>
> The skill is deliberately quiet: it fires only when you ask ("notify me when
> you're done"), sending one event at the terminal state, not a stream. There is
> also an MCP `send_event` tool and a CLI for CI and lifecycle hooks — all onto
> the same `POST /v1/events`.
>
> It is early and intentionally narrow: one gateway, one queue, email and
> webhook sinks. There is no hosted Cloud yet — a waitlist is on the landing
> page if you would rather not run PostgreSQL yourself.
>
> Try it in under ten minutes: quickstart in the repo. Feedback and issues
> welcome.

Tailor the opening line per venue (Hacker News, r/golang, Lobsters, dev.to);
keep the body the same.

## Demo script

Target **3–4 minutes**, terminal only. Rehearse the exact commands so nothing
blocks on the network live.

1. **Setup (before recording).** `docker compose up -d` is healthy; a token is
   in `$HERALD_TOKEN`; the shell is at a project root with `herald.toml`.
2. **State the problem (15s).**
   "I run coding agents in several repos. I want an email when one finishes —
   without giving the agent a mail account or a recipient."
3. **Show the single endpoint (20s).**
   ```sh
   curl -sS -X POST http://localhost:8787/v1/events \
     -H "Authorization: Bearer $HERALD_TOKEN" \
     -H "Content-Type: application/json" \
     -d '{"project":"axis","status":"success","title":"hello","agent":"me"}'
   ```
   Point out the `202` and the event id; note `/v1/reports` still works as a
   deprecated alias.
4. **Install the skill (30s).**
   ```sh
   herald skill > ~/.claude/skills/herald-notify/SKILL.md
   ```
   Read the line that says it fires only when the user asks. Then, in the agent
   harness, type: `notify me when this is done` and let the agent run
   `herald send …`.
5. **Show the delivery (30s).** Open the inbox; the message arrives with the
   links. Mention retries and the dead state shown by
   `herald delivery list --state dead`.
6. **Make the safety point (20s).** The recipient is pinned by the token, so the
   agent cannot redirect the mail; secret-looking strings are redacted at
   ingest.
7. **Close (15s).** "Self-host with Docker or NixOS. Cloud is coming; a waitlist
   is on the page. Link in the description."

Prepare a fallback: if SMTP is unavailable during the demo, show
`herald delivery list` and the webhook sink instead.

## Checklist

- [ ] Pages source set to `/site`; `https://marcustut.github.io/herald/` loads.
- [ ] `go test ./site/` passes (links, inert form, no trackers, no personal data).
- [ ] Waitlist endpoint chosen and wired, or left inert on purpose.
- [ ] Analytics method chosen (logs or self-hosted) — no third-party trackers.
- [ ] Launch post adapted per venue; demo rehearsed on a warm gateway.
