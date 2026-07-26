#!/usr/bin/env bash
#
# Proposes a fleet rollout as a pull request, idempotently.
#
# Runs after `otel-guardrail compile-fleet` has recompiled the Fleet in the working
# tree. Reads that command's JSON report and keeps exactly one pull request in step
# with what the Contracts now compile to:
#
#   compiled tree changed, no rollout PR open        -> push the branch, open one
#   compiled tree changed, rollout PR open, stale    -> update branch and PR body
#   compiled tree changed, PR already proposes this  -> touch nothing
#   compiled tree already correct, rollout PR open   -> comment and close it
#   compiled tree already correct, no PR             -> do nothing
#
# The point of all that is that a cron which force-pushes a branch and opens a fresh
# pull request every morning is spam, spam gets muted, and a muted rollout is worse
# than none: nobody reads the diff that was the whole review gate (ADR 0006).
#
# Nothing here merges anything. A rollout reaches the fleet when a human approves
# the pull request and existing GitOps tooling applies the merged tree.
#
# This lives in a script rather than inline in the workflow so it can be run against
# a throwaway git repository with `gh` stubbed — see gitops-rollout-pr_test.sh.
#
# Environment:
#   REPORT   Path to the JSON report from `compile-fleet --format json`. Required.
#   FLEET    The Fleet directory. Default: fleet.
#   BRANCH   Branch the rollout is proposed on. Default: gitops/fleet-rollout.
#   BASE     Branch the pull request targets. Default: the checked-out branch.
#   LABEL    Label that identifies the rollout PR. Default: fleet-rollout.
#   GH_TOKEN Token `gh` authenticates with (set by the workflow).
#   GH_REPO  Repository to act on (set by the workflow).

set -euo pipefail

REPORT="${REPORT:?REPORT must be the path to the JSON rollout report}"
FLEET="${FLEET:-fleet}"
BRANCH="${BRANCH:-gitops/fleet-rollout}"
LABEL="${LABEL:-fleet-rollout}"

# MARKER identifies a pull request this job wrote. The LABEL is how the PR is found
# — a search is cheap and exact — and the marker is the second key that proves what
# was found is ours before we overwrite it. Without it, a human labelling their own
# PR `fleet-rollout` would have its body replaced by tomorrow's rollout.
MARKER="<!-- otel-guardrail:fleet-rollout -->"

# The commit author. A rollout commit is written by a job, not by whoever last
# touched a Contract, and the history should say so.
AUTHOR_NAME="otel-guardrail"
AUTHOR_EMAIL="otel-guardrail@users.noreply.github.com"

main() {
	local base pr body_file title

	base="${BASE:-$(git rev-parse --abbrev-ref HEAD)}"
	# A detached HEAD has no branch name to open a pull request against, and
	# guessing one would target the wrong branch. Say so instead.
	if [ "$base" = "HEAD" ]; then
		echo "::error::the checkout is on a detached HEAD, so there is no base branch to propose the rollout against. Set BASE to the branch this should target." >&2
		return 1
	fi

	ensure_label
	pr="$(find_rollout_pr)"

	# `compile-fleet` writes only files that changed and prunes only files no
	# Contract accounts for, so git's own answer is the whole test for "is there
	# anything to roll out".
	if [ -z "$(git status --porcelain -- "$FLEET")" ]; then
		all_clear "$pr"
		return 0
	fi

	commit_rollout "$base"

	if fleet_already_proposed; then
		echo "the '${BRANCH}' branch already holds this compiled tree — not pushing"
	else
		git push --force origin "HEAD:refs/heads/${BRANCH}"
		echo "pushed the compiled tree to '${BRANCH}'"
	fi

	body_file="$(mktemp)"
	build_body "$base" >"$body_file"
	title="$(build_title)"
	summarise_run "$body_file"

	if [ -z "$pr" ]; then
		gh pr create --base "$base" --head "$BRANCH" --title "$title" \
			--body-file "$body_file" --label "$LABEL"
		echo "opened a rollout pull request"
		return 0
	fi

	# The whole reason this job is not spam: when today's rollout proposes exactly
	# what the open pull request already proposes, there is no news, so nothing is
	# written and nobody is notified.
	if [ "$(pr_body "$pr")" = "$(cat "$body_file")" ]; then
		echo "pull request #${pr} already proposes exactly this — left untouched"
		return 0
	fi

	gh pr edit "$pr" --title "$title" --body-file "$body_file"
	echo "updated pull request #${pr}"
}

# all_clear retires the rollout pull request once the compiled tree on the base
# branch already matches the Contracts. Leaving a stale "rollout pending" PR open
# would train people to ignore it, and the next real change would arrive inside
# something everybody had stopped reading.
all_clear() {
	local pr="$1"

	summarise_all_clear

	if [ -z "$pr" ]; then
		echo "the compiled configuration already matches the Telemetry Contracts and no rollout pull request is open — nothing to do"
		return 0
	fi

	gh pr close "$pr" --delete-branch --comment \
		"The compiled collector configuration on the base branch now matches the Telemetry Contracts, so there is nothing left to roll out. Closing; a fresh pull request opens by itself the next time they diverge."
	echo "closed pull request #${pr}: nothing left to roll out"
}

# commit_rollout puts the recompiled tree on the rollout branch. The branch is
# rebuilt from the base branch every run rather than accumulated onto, so the diff
# a reviewer reads is always "base versus what the Contracts compile to today" and
# never a pile of superseded rollouts.
commit_rollout() {
	local base="$1"

	git switch -C "$BRANCH" >/dev/null 2>&1
	git add -A -- "$FLEET"
	git -c "user.name=${AUTHOR_NAME}" -c "user.email=${AUTHOR_EMAIL}" \
		commit --quiet -m "chore(fleet): recompile collector configuration from the Telemetry Contracts

Generated by .github/workflows/gitops-rollout.yml from ${base}.
Do not edit the compiled files; change a Telemetry Contract or a Pipeline Profile."
}

# fleet_already_proposed reports whether the rollout branch on the remote already
# holds exactly this compiled tree. Only the Fleet paths are compared: the branch
# being based on an older commit of the base branch is not news worth a force-push
# and a notification, whereas a different compiled tree is.
fleet_already_proposed() {
	git fetch --quiet origin "$BRANCH" 2>/dev/null || return 1
	git diff --quiet FETCH_HEAD HEAD -- "$FLEET"
}

# ensure_label makes the tracking label exist. It is the job's only handle on its
# own pull request, so it is created here rather than assumed — a fresh clone of
# this repository must be able to run the cron without a manual setup step.
ensure_label() {
	gh label create "$LABEL" \
		--description "Tracks the pending fleet rollout; filed by .github/workflows/gitops-rollout.yml" \
		--color 1D76DB \
		--force >/dev/null
}

# find_rollout_pr prints the number of the one open rollout pull request, or
# nothing. Two of them means the job cannot tell which to update, and picking one
# arbitrarily would leave the other rotting — so it stops and says so.
find_rollout_pr() {
	local numbers count body
	numbers="$(gh pr list --label "$LABEL" --state open --limit 100 --json number --jq '.[].number')"
	count="$(printf '%s' "$numbers" | grep -c . || true)"

	if [ "$count" -gt 1 ]; then
		echo "::error::${count} open pull requests carry the '${LABEL}' label; the job cannot tell which one to update. Close all but one." >&2
		return 1
	fi
	if [ "$count" -eq 0 ]; then
		return 0
	fi

	body="$(pr_body "$numbers")"
	if ! printf '%s' "$body" | grep -qF -- "$MARKER"; then
		echo "::error::pull request #${numbers} carries the '${LABEL}' label but was not filed by this job (no marker in its body). Refusing to overwrite someone else's pull request — remove the label from it, or close it." >&2
		return 1
	fi
	printf '%s' "$numbers"
}

# pr_body is a pull request's body with CR stripped, so a body round-tripped
# through the API still compares equal to the one we generate; otherwise every run
# would look like a change.
pr_body() {
	gh pr view "$1" --json body --jq .body | tr -d '\r'
}

build_title() {
	local compiled failed
	compiled="$(jq '.compiled | length' "$REPORT")"
	failed="$(jq '.not_compiled | length' "$REPORT")"

	if [ "$failed" -eq 0 ]; then
		echo "Fleet rollout: ${compiled} service(s) recompiled"
	else
		echo "Fleet rollout: ${compiled} service(s) recompiled, ${failed} Contract(s) did not compile"
	fi
}

# build_body renders the rollout as the pull-request body.
#
# It is built from the JSON report and from git, never from the CLI's prose, so
# rewording the terminal output does not rewrite an open pull request. And it is
# deterministic — no dates, no run numbers — so an unchanged rollout produces a
# byte-identical body and therefore no edit and no notification.
build_body() {
	local base="$1"

	printf '%s\n' "$MARKER"
	cat <<-'EOF'
		<!--
		  Filed and kept up to date by .github/workflows/gitops-rollout.yml.
		  The `fleet-rollout` label is how that job finds this pull request again —
		  remove it and tomorrow's run opens a duplicate. Do not edit this body by
		  hand; it is overwritten whenever the compiled tree changes.
		-->
	EOF
	echo

	cat <<-EOF
		The Control Plane recompiled the Fleet's Telemetry Contracts and the result no
		longer matches what is committed on \`${base}\`. Merging this **is** the rollout:
		existing GitOps tooling applies the merged tree to the Agents and the Gateway,
		so there is no push protocol and no control-plane server ([ADR 0006](../blob/${base}/docs/adr/0006-gitops-config-distribution.md)).

		Review this as configuration, because that is what it is. The compiled files are
		generated and deterministic, so every line in the diff is a real change to what
		the fleet will run.
	EOF
	echo

	echo "## What changes on the fleet"
	echo
	changed_files "$base"
	echo

	jq -r '
	  def row: "| `\(.service_name)` | \(.tier) | `\(.pipeline_profile)` | \(.signals | join(", ")) | `\(.collector_config)` |";
	  if (.compiled | length) == 0 then empty else
	    "## Services this rollout covers",
	    "",
	    "| Service | Service Tier | Pipeline Profile | Signals | Collector configuration |",
	    "| --- | --- | --- | --- | --- |",
	    (.compiled[] | row),
	    ""
	  end
	' "$REPORT"

	jq -r '
	  def cell: (. // "") | gsub("\\s+"; " ") | gsub("\\|"; "\\|");
	  def row: "| `\(.service_name)` | \(.tier) | `\(.telemetry_contract)` | \(.reason | cell) |";
	  if (.not_compiled | length) == 0 then empty else
	    "## Did not compile — these services are NOT in this rollout",
	    "",
	    "Each keeps whatever collector configuration it last compiled to, so nothing regresses; but each is also frozen at that configuration until its Telemetry Contract or its tier'"'"'s Pipeline Profile is fixed. The same list is recorded in `rollout-manifest.yaml`, which is why this absence is reviewable at all.",
	    "",
	    "| Service | Service Tier | Telemetry Contract | Why not |",
	    "| --- | --- | --- | --- |",
	    (.not_compiled[] | row),
	    ""
	  end
	' "$REPORT"

	cat <<-EOF
		---

		Reproduce this exactly with \`otel-guardrail compile-fleet ${FLEET}\`; compiling is
		deterministic, so it will produce these bytes. See
		[\`docs/gitops-distribution.md\`](../blob/${base}/docs/gitops-distribution.md) for
		what this loop does and how to run it by hand.
	EOF
}

# changed_files is the rollout as a table of paths, taken from git rather than from
# the report: what a reviewer is being asked to approve is the diff, and only git
# knows which files this commit actually adds, changes or deletes.
changed_files() {
	local base="$1"

	echo "| Change | File |"
	echo "| --- | --- |"
	git diff --name-status "$base" HEAD -- "$FLEET" | while IFS=$'\t' read -r status file; do
		case "$status" in
		A) echo "| added | \`${file}\` |" ;;
		D) echo "| removed — no Telemetry Contract accounts for it | \`${file}\` |" ;;
		*) echo "| changed | \`${file}\` |" ;;
		esac
	done
}

# summarise_run puts the same rollout on the workflow run's summary page, so the
# run itself is readable without opening the pull request.
summarise_run() {
	[ -n "${GITHUB_STEP_SUMMARY:-}" ] || return 0
	cat "$1" >>"$GITHUB_STEP_SUMMARY"
}

summarise_all_clear() {
	[ -n "${GITHUB_STEP_SUMMARY:-}" ] || return 0
	printf 'The compiled collector configuration already matches the Telemetry Contracts. Nothing to roll out.\n' \
		>>"$GITHUB_STEP_SUMMARY"
}

main "$@"
