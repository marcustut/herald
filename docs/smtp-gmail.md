# SMTP credentials (Gmail) — so Herald can email you

Herald's email sink sends through an **SMTP submission server**. Self-hosters
are both the sender and the recipient: Herald sends *from* an account you
control *to* a pinned mailbox (usually the same address). This doc gets you
working SMTP credentials from a Gmail account in a few minutes.

If you already have an SMTP server (your own Stalwart/Postfix, or a
transactional provider), skip to [Other providers](#other-providers).

## What Gmail requires

Gmail will **not** accept your normal Google password over SMTP, and the old
"Less secure app access" switch was removed in 2022. You must:

1. turn on **2-Step Verification**, then
2. create an **App Password** and use that as the SMTP password.

An App Password is a 16-character code that only works for apps like Herald.
You can revoke it any time without changing your Google password.

## Get an App Password (≈3 min)

1. Sign in and open **<https://myaccount.google.com/security>**.
2. Under **How you sign in to Google**, enable **2-Step Verification** if it is
   off. App Passwords are unavailable until it is on.
3. Open **<https://myaccount.google.com/apppasswords>** (or search "App
   passwords" in the account settings).
4. Enter a name such as `Herald`, then **Create**.
5. Google shows a **16-character password** like `abcd efgh ijkl mnop`. Copy it
   and remove the spaces → `abcdefghijklmnop`. **This is shown once.**

> Google Workspace accounts: an administrator can disable App Passwords. If the
> option is missing, use an [alternative provider](#other-providers) or ask your
> admin to enable it.

## Point Herald at Gmail

`gateway.toml` (non-secret settings):

```toml
[smtp]
addr = "smtp.gmail.com:587"   # STARTTLS submission
from = "you@gmail.com"        # must be the account, or a verified "Send mail as" alias
```

Then add an email destination `you@gmail.com` (send to yourself, or any address
you own) for the project over the API or `/dashboard` — destinations are not in
`gateway.toml`.

Credentials come from the **environment**, never the config file:

```sh
export HERALD_SMTP_USERNAME=you@gmail.com          # the full Gmail address
export HERALD_SMTP_PASSWORD=abcdefghijklmnop       # the App Password, no spaces
```

With Docker Compose, put both in `.env` (see
[self-hosting-docker.md](self-hosting-docker.md)).

You do **not** need `[smtp] server_name` for Gmail: it is only used when you
dial an IP address and must tell TLS which certificate name to expect. Dialling
`smtp.gmail.com` directly already matches the certificate.

### Sending to yourself

Herald is single-owner: the recipient is pinned server-side and agents can never
change it. For personal use, set both `from` and `recipient` to the same Gmail
address and you will get the notification in your own inbox. To land it in a
different mailbox, set `recipient` to that address.

## Limits and etiquette

- **~500 messages/day** on consumer Gmail, **~2000/day** on Google Workspace.
  Sending to yourself counts toward the quota.
- One message per finished task, not per step (Herald emits on terminal states
  only — success, failure, blocked).
- If you expect real volume, use a transactional provider or your own server
  rather than a personal Gmail account.

## Troubleshooting

| Error | Cause / fix |
|---|---|
| `535-5.7.8 Username and Password not accepted` | You used your Google password instead of an **App Password**, or 2FA is off. Create an App Password. |
| `Application-specific password required` | Same as above — Gmail is telling you to use an App Password. |
| `535 ... Invalid login` with an App Password | Remove the spaces from the 16-character code. |
| App Passwords option missing | 2-Step Verification is off, or a Workspace admin disabled it. |
| `tls: failed to verify certificate ... x509` | You set `addr` to an IP. Use `smtp.gmail.com:587`, or set `[smtp] server_name = "smtp.gmail.com"`. |
| `421 ... too many messages` / rate limit | Daily Gmail quota reached; wait or move to a provider below. |
| Events accepted but no email | Check the dispatcher: `docker compose logs herald-dispatcher`. |

## Other providers

| Provider | Host:port | Credential | Notes |
|---|---|---|---|
| Gmail | `smtp.gmail.com:587` | App Password (2FA required) | ~500/day |
| Google Workspace | `smtp.gmail.com:587` | App Password / SMTP relay | ~2000/day; admin may restrict |
| Fastmail | `smtp.fastmail.com:587` | App Password | paid plan |
| Outlook / Microsoft 365 | `smtp.office365.com:587` | App Password or OAuth | increasingly OAuth-only |
| Proton Mail | Proton Bridge (`127.0.0.1:1025`) | Bridge password | requires Proton Bridge |
| Amazon SES | `email-smtp.<region>.amazonaws.com:587` | SMTP credentials | request production access |
| Postmark / Mailgun / Resend | provider SMTP endpoint | API key as the password | transactional, higher limits |
| Self-hosted (Stalwart / Postfix) | your host:587 | a local mailbox account | full control, no third party |

Anything that speaks SMTP submission with STARTTLS works: set `[smtp] addr` and
`from`, and supply `HERALD_SMTP_USERNAME` / `HERALD_SMTP_PASSWORD`.

## Security

- Treat the App Password like any secret: store it in a secret manager, `sops`,
  or the host `EnvironmentFile` — **never commit it**.
- Revoke it from <https://myaccount.google.com/apppasswords> if it leaks.
- Herald never logs the password and refuses to send over an unverified TLS
  session to a remote host.
