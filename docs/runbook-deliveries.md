# Runbook: inspecting and redriving deliveries

Herald's durable queue is the `deliveries` table in PostgreSQL. Every `Event`
fans out to one row per sink, and that row moves through a small state machine:

```
pending --claim--> inflight --success--> delivered
                       │
                       ├─ transient failure (attempts < max) --> pending (backoff)
                       ├─ permanent failure or attempts = max --> dead
                       └─ lease expires (worker died) --> pending (reclaim)
```

A row is **at-least-once**: a sink can see the same event twice if a worker
crashes after the sink accepted it but before the row was marked delivered.
Sinks receive an `Idempotency-Key` so they can collapse that. The operator tools
below never knowingly duplicate a *successful* delivery.

All commands take `--database-url` or read `HERALD_DATABASE_URL`, and run the
same automatic migrations as `serve`/`dispatch`.

## Inspect

```sh
# everything, newest first (default limit 100)
herald delivery list

# filter by state, event and/or sink
herald delivery list --state dead
herald delivery list --state pending --sink email
herald delivery list --event evt_abc123
herald delivery list --limit 0            # no limit

# full detail for one row
herald delivery show del_abc123
```

`list` prints one line per delivery:

```
ID          STATE   EVENT         SINK    ATT  NEXT ATTEMPT          LAST ERROR
del_abc123  dead    evt_abc123    email   8    2026-10-13T12:00:00Z  smtp: 550 mailbox unavailable
```

`show` adds lease, created and updated timestamps. Useful queries during an
incident:

```sh
# what is stuck right now?
herald delivery list --state dead
herald delivery list --state inflight

# is a specific event fully delivered?
herald delivery list --event evt_abc123
```

## Permanent vs transient failures

The dispatcher classifies every failure:

- **Transient** (network, 5xx, timeouts, `context`-unrelated errors): the row
  returns to `pending` with exponential backoff and keeps retrying until
  `--max-attempts` (default 8) is reached. No operator action needed.
- **Permanent** (bad recipient, 4xx from a webhook, unknown sink, missing
  event): the row goes straight to `dead`. Retrying without fixing the cause
  just fails again.
- **Suppressed** (hosted email): the address is on the platform-wide
  suppression list after a hard bounce or a complaint, so the delivery is
  refused and dead-lettered at once. Redriving will not help until the
  suppression is removed (see below).
- **Attempts exhausted**: a transient failure that kept failing is also `dead`.

Read `LAST ERROR` from `herald delivery show <id>` before redriving. A permanent
error that has not been fixed will not succeed on redrive.

### Suppressed recipients

A hard bounce or complaint webhook suppresses an address for the whole
deployment. To recover after the mailbox is fixed:

```sql
-- Inspect, then remove only the address that was genuinely fixed.
SELECT address, reason, created_at FROM suppressions ORDER BY created_at DESC;
DELETE FROM suppressions WHERE address = 'user@example.com';
```

Then redrive the affected dead letters (`herald delivery requeue --sink <id>` or
`--state dead`). See [deliverability.md](deliverability.md) for how the list is
populated and why it is global.

## Safe repair and manual redrive

1. **Find the dead letter(s).**

   ```sh
   herald delivery list --state dead
   ```

2. **Diagnose.** Read the last error and the event.

   ```sh
   herald delivery show del_abc123
   ```

3. **Fix the root cause first** — correct the sink config/credentials, bring the
   webhook back, fix the recipient, etc. Redriving a still-broken sink only puts
   the row back into the dead state.

4. **Redrive one row:**

   ```sh
   herald delivery retry del_abc123
   ```

   This makes the row `pending`, due immediately, with a **fresh attempt
   budget** (`attempts` reset to 0) and the old `last_error` cleared. The
   dispatcher picks it up on its next poll.

5. **Or redrive every dead letter, optionally scoped:**

   ```sh
   herald delivery requeue --state dead
   herald delivery requeue --state dead --event evt_abc123
   herald delivery requeue --state dead --sink email
   ```

   It is safe to run twice: the second pass reports `requeued 0`.

### What requeue will not do

- It **never revives a `delivered` row.** A sink that already accepted the event
  is not contacted again — this is what makes redrive idempotent.
- It **leaves an `inflight` row alone while its lease is held.** A worker owns
  that row and may be mid-send; redriving it would double-send. An `inflight`
  row whose lease has **expired** is reclaimed first (below) and only then can
  be redriven.
- `herald delivery retry <id>` on a protected row prints why and exits `0`:
  `del_abc123 is delivered; not requeued (already delivered or leased by a worker)`.

## Reclaiming expired leases

If a dispatcher process is killed after claiming a row, the row stays
`inflight` until its lease expires. `Claim` reclaims expired leases
automatically at the start of every poll, so the normal fix is simply to let the
dispatcher run. To force it (for example before a manual redrive, or when the
dispatchers are stopped):

```sh
herald delivery reclaim
```

Reclaim moves expired `inflight` rows back to `pending` **without consuming
another attempt**, keeping the existing attempt count and last error.

## Emergency SQL

The CLI is the supported path. If PostgreSQL is reachable but the CLI is not,
these read-only queries are safe:

```sql
-- dead letters, newest first
SELECT id, event_id, sink_id, attempts, last_error, updated_at
FROM deliveries WHERE state = 'dead' ORDER BY updated_at DESC;

-- workers that may have died (lease expired)
SELECT id, event_id, sink_id, attempts, lease_until
FROM deliveries WHERE state = 'inflight' AND lease_until <= now();

-- a stuck event and all of its sink rows
SELECT * FROM deliveries WHERE event_id = 'evt_abc123';
```

Avoid hand-written `UPDATE deliveries` unless there is no other option. The
supported transitions enforce the "never resend a delivered row" guarantee; ad
hoc SQL can bypass it. If you must, mirror `requeue` and always scope the
`WHERE` so a `delivered` row cannot match:

```sql
UPDATE deliveries
SET state = 'pending', attempts = 0, next_attempt_at = now(),
    lease_until = NULL, last_error = '', updated_at = now()
WHERE id = 'del_abc123'
  AND state IN ('dead', 'pending');   -- never 'delivered'
```

## Compatibility and deployment notes

- **No schema change.** These tools read the existing `deliveries` columns; no
  new migration is required. An older binary can keep serving against the same
  database.
- The dispatcher reclaims expired leases itself, so upgrading the dispatcher is
  what makes stuck workers recover automatically. The `herald delivery` command
  is a separate operator surface and can be run from any host with database
  access.
- `herald delivery requeue` resets the attempt budget; if your sink has a tight
  rate limit, spread a large redrive over time with `--event`/`--sink` filters
  rather than redriving everything at once.

## Verification after a repair

```sh
herald delivery requeue --state dead --sink email
sleep 5
herald delivery list --state dead --sink email   # expect no rows
herald delivery list --state pending             # rows should drain to delivered
```

If rows keep returning to `dead`, the root cause is not fixed — inspect
`LAST ERROR` again.
