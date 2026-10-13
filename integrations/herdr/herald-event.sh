#!/bin/sh
# Herald lifecycle plugin for herdr.
#
# herdr invokes this on every pane.agent_status_changed event. We notify Herald
# only when an agent pane transitions to `blocked` (needs a human) or `done`
# (finished), and derive an occurrence id from the pane plus the live herdr
# sequence so duplicate deliveries and restarts do not double-send.
#
# Testability: herdr puts its own binary path in HERDR_BIN_PATH and we send via
# $HERALD_BIN (default `herald`); tests stub both.
#
# Failure isolation: this hook NEVER exits non-zero and never hangs, so a broken
# config or an unreachable gateway cannot wedge or slow down herdr.
set -eu

log() { printf 'herald-event: %s\n' "$*" >&2; }

event_json=${HERDR_PLUGIN_EVENT_JSON:-}
[ -n "$event_json" ] || exit 0

# Extract the fields we need. herdr emits compact JSON with these keys once each,
# so a targeted sed is enough and keeps the plugin dependency-free.
status=$(printf '%s' "$event_json" | sed -n 's/.*"agent_status":"\([a-z]*\)".*/\1/p')
pane_id=$(printf '%s' "$event_json" | sed -n 's/.*"pane_id":"\([^"]*\)".*/\1/p')
workspace_id=$(printf '%s' "$event_json" | sed -n 's/.*"workspace_id":"\([^"]*\)".*/\1/p')
agent=$(printf '%s' "$event_json" | sed -n 's/.*"agent":"\([^"]*\)".*/\1/p')

# Only transitions that are worth waking a human for. idle/working/unknown are
# routine state churn and would otherwise spam the gateway.
case "$status" in
	done | blocked) ;;
	*) exit 0 ;;
esac
[ -n "$pane_id" ] || { log "event has no pane_id; skipping"; exit 0; }

# The wire event carries no sequence (verified against herdr 0.9.3), so read the
# live one from herdr. done is an idle transition that completed work
# (completion_seq); blocked is an ordinary state change (state_change_seq).
herdr_bin=${HERDR_BIN_PATH:-herdr}
info=$("$herdr_bin" agent get "$pane_id" 2>/dev/null || true)
name=$(printf '%s' "$info" | sed -n 's/.*"name":"\([^"]*\)".*/\1/p')
cwd=$(printf '%s' "$info" | sed -n 's/.*"cwd":"\([^"]*\)".*/\1/p')
seq=
case "$status" in
	done) seq=$(printf '%s' "$info" | sed -n 's/.*"completion_seq":\([0-9][0-9]*\).*/\1/p') ;;
	blocked) seq=$(printf '%s' "$info" | sed -n 's/.*"state_change_seq":\([0-9][0-9]*\).*/\1/p') ;;
esac
if [ -z "$seq" ]; then
	seq=$(printf '%s' "$info" | sed -n 's/.*"state_change_seq":\([0-9][0-9]*\).*/\1/p')
fi
if [ -z "$seq" ]; then
	# shortcut: herdr did not answer agent get, so fall back to a hash of the
	# event. Two genuine transitions with byte-identical payloads would collapse;
	# upgrade by persisting the last key per pane in HERDR_PLUGIN_STATE_DIR.
	seq=$(printf '%s' "$event_json" | cksum | tr ' ' '-')
fi
occurrence="herdr:${pane_id}:${status}:${seq}"

# Agent id must be a herald slug (a-z0-9 and inner hyphens, <=32 chars).
agent=$(printf '%s' "$agent" | tr 'A-Z' 'a-z' | tr -c 'a-z0-9-' '-' | cut -c1-32 | sed 's/^-*//; s/-*$//')
[ -n "$agent" ] || agent=herdr

# Config and scoped token live in the plugin config dir: the plugin cannot read
# the pane's environment. `herald.env` is dotenv-style and is sourced.
config_dir=${HERDR_PLUGIN_CONFIG_DIR:-}
[ -n "$config_dir" ] || { log "HERDR_PLUGIN_CONFIG_DIR is unset; not sending"; exit 0; }
env_file=$config_dir/herald.env
[ -f "$env_file" ] || { log "missing $env_file; not sending"; exit 0; }
set -a
# shellcheck disable=SC1090
. "$env_file" || { set +a; log "cannot read $env_file; not sending"; exit 0; }
set +a
if [ -z "${HERALD_GATEWAY:-}" ] || [ -z "${HERALD_PROJECT:-}" ] || [ -z "${HERALD_TOKEN:-}" ]; then
	log "$env_file must set HERALD_GATEWAY, HERALD_PROJECT and HERALD_TOKEN; not sending"
	exit 0
fi

case "$status" in
	blocked)
		herald_status=blocked
		title="Agent blocked: ${name:-$agent}"
		;;
	done)
		herald_status=success
		title="Agent done: ${name:-$agent}"
		;;
esac
body="herdr ${status} in ${workspace_id:-?}/${pane_id}"
if [ -n "$cwd" ]; then
	body="$body at $cwd"
fi

herald_bin=${HERALD_BIN:-herald}
send() {
	if command -v timeout >/dev/null 2>&1; then
		timeout 10 "$herald_bin" send "$@"
	else
		"$herald_bin" send "$@"
	fi
}
if out=$(send --status "$herald_status" --title "$title" --body "$body" \
	--agent "$agent" --occurrence-id "$occurrence" 2>&1); then
	log "sent $occurrence -> $out"
else
	log "send failed for $occurrence: $out"
fi
exit 0
