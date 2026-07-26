#!/usr/bin/env bash
#
# Files the daily Waiver-expiry report as a GitHub issue, idempotently.
#
# Reads the JSON produced by `otel-guardrail waivers --format json` and keeps
# exactly one tracking issue in step with it:
#
#   Waivers need attention, no open tracking issue   -> open one
#   Waivers need attention, tracking issue is open   -> edit it in place
#   Waivers need attention, issue already says this  -> touch nothing
#   Nothing needs attention, tracking issue is open  -> comment and close it
#   Nothing needs attention, no tracking issue       -> do nothing
#
# The point of all that is that a cron which opens a fresh issue every morning
# is spam, spam gets muted, and a muted alert is the same as no alert at all.
#
# This lives in a script rather than inline in the workflow so it can be run
# against a captured report with `gh` stubbed — see waiver-expiry-issue_test.sh.
#
# Environment:
#   REPORT   Path to the JSON expiry report. Required.
#   LABEL    Label that identifies the tracking issue. Default: waiver-expiry.
#   GH_TOKEN Token `gh` authenticates with (set by the workflow).
#   GH_REPO  Repository to act on (set by the workflow).

set -euo pipefail

REPORT="${REPORT:?REPORT must be the path to the JSON expiry report}"
LABEL="${LABEL:-waiver-expiry}"

# MARKER identifies an issue this job wrote. The LABEL is how the issue is found
# — a search is cheap and exact — and the marker is the second key that proves
# what was found is ours before we overwrite it. Without it, a human labelling an
# unrelated issue `waiver-expiry` would have their issue silently replaced by
# tomorrow's report.
MARKER="<!-- otel-guardrail:waiver-expiry -->"

main() {
	local expired expiring total issue current_body body_file title

	expired="$(jq '.expired | length' "$REPORT")"
	expiring="$(jq '.expiring | length' "$REPORT")"
	total=$((expired + expiring))

	ensure_label

	issue="$(find_tracking_issue)"
	current_body=""
	if [ -n "$issue" ]; then
		# Strip CR so a body round-tripped through the API still compares equal to
		# the one we generate; otherwise every run would look like a change.
		current_body="$(gh issue view "$issue" --json body --jq .body | tr -d '\r')"
		if ! printf '%s' "$current_body" | grep -qF -- "$MARKER"; then
			echo "::error::issue #${issue} carries the '${LABEL}' label but was not filed by this job (no marker in its body). Refusing to overwrite someone else's issue — remove the label from it, or close it." >&2
			return 1
		fi
	fi

	if [ "$total" -eq 0 ]; then
		all_clear "$issue"
		return 0
	fi

	body_file="$(mktemp)"
	build_body >"$body_file"
	title="$(build_title "$expired" "$expiring")"
	summarise_run "$body_file"

	if [ -z "$issue" ]; then
		gh issue create --title "$title" --body-file "$body_file" --label "$LABEL"
		echo "opened a tracking issue: ${total} Waiver(s) need attention"
		return 0
	fi

	# The whole reason this job is not spam: when today's report says exactly what
	# yesterday's issue already says, there is no news, so nothing is written and
	# nobody is notified.
	if [ "$current_body" = "$(cat "$body_file")" ]; then
		echo "issue #${issue} already says exactly this — left untouched"
		return 0
	fi

	gh issue edit "$issue" --title "$title" --body-file "$body_file"
	echo "updated issue #${issue}: ${total} Waiver(s) need attention"
}

# all_clear retires the tracking issue once no Waiver needs attention. Closing is
# a state change worth one notification; leaving a stale "action required" issue
# open would train people to ignore it.
all_clear() {
	local issue="$1" window
	window="$(jq -r '.within_days' "$REPORT")"

	summarise_all_clear

	if [ -z "$issue" ]; then
		echo "no Waiver needs attention and no tracking issue is open — nothing to do"
		return 0
	fi

	gh issue close "$issue" --reason completed \
		--comment "No Waiver has expired and none expires within ${window} days, as of $(jq -r '.as_of' "$REPORT"). Closing; this issue reopens as a new one if that changes."
	echo "closed issue #${issue}: nothing needs attention"
}

# ensure_label makes the tracking label exist. It is the job's only handle on its
# own issue, so it is created here rather than assumed — a fresh clone of this
# repository must be able to run the cron without a manual setup step.
ensure_label() {
	gh label create "$LABEL" \
		--description "Tracks Waivers that have expired or are about to; filed by .github/workflows/waiver-expiry.yml" \
		--color FBCA04 \
		--force >/dev/null
}

# find_tracking_issue prints the number of the one open tracking issue, or
# nothing. Two of them means the job cannot tell which to update, and picking one
# arbitrarily would leave the other rotting — so it stops and says so.
find_tracking_issue() {
	local numbers count
	numbers="$(gh issue list --label "$LABEL" --state open --limit 100 --json number --jq '.[].number')"
	count="$(printf '%s' "$numbers" | grep -c . || true)"

	if [ "$count" -gt 1 ]; then
		echo "::error::${count} open issues carry the '${LABEL}' label; the job cannot tell which one to update. Close all but one." >&2
		return 1
	fi
	printf '%s' "$numbers"
}

build_title() {
	local expired="$1" expiring="$2" window
	window="$(jq -r '.within_days' "$REPORT")"
	echo "Waiver expiry: ${expired} expired, ${expiring} expiring within ${window} days"
}

# build_body renders the report as the issue body. It is built from the JSON and
# not from the CLI's prose so that rewording the terminal output never rewrites
# every open issue; and it is deterministic, so an unchanged register produces a
# byte-identical body and therefore no edit.
build_body() {
	printf '%s\n' "$MARKER"
	cat <<-'EOF'
		<!--
		  Filed and kept up to date by .github/workflows/waiver-expiry.yml.
		  The `waiver-expiry` label is how that job finds this issue again — remove
		  it and tomorrow's run opens a duplicate. Do not edit this body by hand;
		  it is overwritten whenever the register changes. Fix the register instead
		  (guardrail/waivers.yaml), and this issue closes itself.
		-->
	EOF
	echo

	jq -r '
	  def days(n): if n == 1 then "1 day" else "\(n) days" end;

	  # The expiry in the tense the day deserves.
	  def when:
	    if .days_until_expiry < 0 then "\(.expires) (\(days(0 - .days_until_expiry)) ago)"
	    elif .days_until_expiry == 0 then "\(.expires) (today — last day it holds)"
	    else "\(.expires) (in \(days(.days_until_expiry)))"
	    end;

	  # One table cell: collapse the wrapped YAML reason onto one line, and escape
	  # any pipe in it so it cannot break the table.
	  def cell: (. // "") | gsub("\\s+"; " ") | gsub("\\|"; "\\|") | ltrimstr(" ") | rtrimstr(" ");

	  def row: "| `\(.service_name)` | \(.standard) | \(when) | @\(.approved_by) | \(.reason | cell) |";
	  def table: ["| Service | Standard | Expiry | Approved by | Reason it was filed |",
	              "| --- | --- | --- | --- | --- |"] + [.[] | row] | join("\n");

	  (.expired | length) as $expired
	  | (.expiring | length) as $expiring
	  | "**\($expired + $expiring) Waiver(s) need attention** as of \(.as_of), looking \(days(.within_days)) ahead.",
	    "",
	    "A Waiver is a time-boxed, owner-approved exemption letting one service skip one Standard. It reverts on its own when it expires — so an expiry nobody acts on turns into a broken build, and an expiry nobody notices turns the escape hatch into a permanent hole ([ADR 0003](../blob/master/docs/adr/0003-phased-guardrail-enforcement.md)).",
	    "",
	    (if $expired > 0 then
	      "## Already expired — these services are blocked right now\n\nThe Standard fails their builds again already. Either fix the Telemetry Contract, or file a fresh Waiver with a fresh reason and date.\n\n\(.expired | table)\n"
	     else empty end),
	    (if $expiring > 0 then
	      "## Expiring within \(days(.within_days)) — act before the build breaks\n\nStill holding, but not for long. Fix the Telemetry Contract, or file a fresh Waiver.\n\n\(.expiring | table)\n"
	     else empty end),
	    "---",
	    "",
	    "Approvers to notify: \([.expired[], .expiring[]] | map("@" + .approved_by) | unique | join(", "))",
	    "",
	    "Waivers live in [`guardrail/waivers.yaml`](../blob/master/guardrail/waivers.yaml); see [`docs/waivers.md`](../blob/master/docs/waivers.md) for how to file one and [`docs/waiver-expiry.md`](../blob/master/docs/waiver-expiry.md) for what this job does. Reproduce this report with `otel-guardrail waivers --within \(.within_days) --as-of \(.as_of)`."
	' "$REPORT"
}

# summarise_run puts the same report on the workflow run's summary page, so the
# run itself is readable without opening the issue.
summarise_run() {
	[ -n "${GITHUB_STEP_SUMMARY:-}" ] || return 0
	cat "$1" >>"$GITHUB_STEP_SUMMARY"
}

summarise_all_clear() {
	[ -n "${GITHUB_STEP_SUMMARY:-}" ] || return 0
	printf 'No Waiver has expired and none expires within %s days, as of %s.\n' \
		"$(jq -r '.within_days' "$REPORT")" "$(jq -r '.as_of' "$REPORT")" >>"$GITHUB_STEP_SUMMARY"
}

main "$@"
