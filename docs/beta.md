# Private beta: funnel, go/no-go and privacy

This document is the operating note for the hosted cloud private beta. It defines
the activation funnel we watch, the go/no-go criteria for widening the beta, the
feedback path, and exactly what telemetry is collected and why.

The cloud surface is **always on when a database is configured**: the account
surface is part of the product. A `--dev` run (no database) has no account
surface, exposes no accounts/signup/sign-in routes and emits none of the funnel
below. `[auth] signup = false` runs the surface closed: only existing accounts
can sign in.

## The funnel

Four stages, each a bounded counter (`herald_funnel_events_total{stage=...}`).
No account id, email, address or event body is ever attached to a stage.

| stage | meaning | where it is recorded |
|---|---|---|
| `account_created` | a first sign-in provisioned an account | `Identities.EnsureUser`, after the provisioning transaction commits |
| `project_created` | a project was created through the cloud surface | `Projects.CreateProject`, after the store write |
| `first_delivered` | an account's first distinct event reached a sink | the dispatcher, when the just-completed delivery is the account's first |
| `repeat_send` | a later distinct event reached a sink | the dispatcher, for every subsequent delivered event |

`herald_activation_seconds` is a histogram of **account-created →
first-delivered** for each account that activates. Its buckets run from one
minute to one week, because a beta account can take minutes or days to connect
its first agent.

`first_delivered` and `repeat_send` are per **account and distinct event**, not
per delivery and not per sink: one event delivered to two destinations is still
one activation, and a retry of an already-delivered event is not a repeat send.

## No new data collection

The funnel needs no schema of its own. Every stage is derived from rows Herald
already keeps:

- `account_created` — `accounts.created_at` (and the counter above).
- `project_created` — `projects.created_at` (and the counter above).
- `first_delivered` / `repeat_send` — the delivered rows in `deliveries` joined
  to their `events`, grouped by tenant.
- activation time — first delivered minus `accounts.created_at`.

`postgres.Funnel` answers the per-account question with a single read-only query;
it writes nothing and stores no additional identifier. The dispatcher consults it
on a successful delivery only. There is deliberately **no migration** for the
funnel.

## Queries for the beta review

Conversion between stages, over a window:

```promql
# Accounts created, projects created, activations and repeat senders in 30 days.
sum by (stage) (increase(herald_funnel_events_total[30d]))

# Signup -> project creation, and signup -> first delivery.
sum(increase(herald_funnel_events_total{stage="project_created"}[30d]))
  / sum(increase(herald_funnel_events_total{stage="account_created"}[30d]))
sum(increase(herald_funnel_events_total{stage="first_delivered"}[30d]))
  / sum(increase(herald_funnel_events_total{stage="account_created"}[30d]))
```

Time to first notification (the number that matters most for onboarding):

```promql
# Median and p90 account-created -> first-delivered setup time.
histogram_quantile(0.5, sum by (le) (rate(herald_activation_seconds_bucket[30d])))
histogram_quantile(0.9, sum by (le) (rate(herald_activation_seconds_bucket[30d])))
```

Retention, as the share of activated accounts that send again:

```promql
sum(increase(herald_funnel_events_total{stage="repeat_send"}[30d]))
  / sum(increase(herald_funnel_events_total{stage="first_delivered"}[30d]))
```

Because the counters are process-local, scrape a single API process for the beta
review (or sum across replicas if several run).

## Go/no-go criteria

Widen the beta only when **all** of the following hold over a four-week window,
against a cohort of at least 20 invited accounts:

1. **Signup works end to end.** At least 80% of invited accounts that sign in go
   on to create a project.
2. **Activation is fast enough.** Median account-created → first-delivered is
   under one hour, and the p90 is under 24 hours.
3. **Delivery is trustworthy.** The `delivered` outcome is at least 99% of
   delivery attempts, with zero unresolved hard bounces caused by Herald's own
   configuration, and no suppression-list growth from our sends.
4. **It is worth repeating.** At least 40% of activated accounts deliver a
   repeat send within the window.
5. **Quality bar.** No Sev-1 incident (data loss, cross-tenant exposure, or
   deliverability damage) and no more than one Sev-2, both resolved.
6. **Feedback is net positive.** At least half of the accounts that reply report
   they would keep using Herald; blocking themes are triaged before widening.

If a criterion fails, keep the beta at its current size and fix the specific
funnel gap before the next review. Record the decision and the numbers in the
issue for the beta.

## Feedback capture

There is no third-party form or analytics dependency. Feedback and invite
requests go to the same monitored address that is linked on the landing page:

- Invite request — the **Request a beta invite** button (`mailto:`).
- Feedback — the **Send feedback** link (`mailto:`).

Ask respondents to include what they tried, what they expected, and what
happened. Do not paste event bodies, tokens or addresses into the email if they
are sensitive.

## Privacy note

**Collected.** Bounded stage counts (the four counters above), a duration
histogram for setup time, and the metrics and logs Herald already emits
(`docs/observability.md`). The funnel query reads opaque account and tenant ids
and timestamps that already exist for the product to work; it records no new
field.

**Not collected.** Email addresses, destination addresses, event titles, bodies,
links or context; raw tokens or secrets; anything from a third-party analytics or
tracking script. The landing page loads no third-party resources and sets no
tracking cookies.

**Why.** The beta needs to know whether invited people reach a first
notification and come back, and how long that takes, so we can fix the funnel.
Counts and durations answer that without identifying anyone. Account deletion
(`DELETE /v1/account`) cascades the account's rows away, so a deleted
account disappears from the funnel too.
