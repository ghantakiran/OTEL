#!/usr/bin/env bash
#
# Checks that the worked examples in docs/ still match what otel-guardrail
# actually prints.
#
# Three of the four documented outputs had gone stale by the time anyone noticed,
# because prose has no test. This gives it one. A doc block opts in with a marker
# on the line before it:
#
#     <!-- verify: check --as-of 2026-08-01 guardrail/examples/waived-contract.yaml -->
#     ```
#     …the exact expected output…
#     ```
#
# Everything after `verify:` is passed to otel-guardrail verbatim, run from the
# repository root. The fenced block that follows must match its combined
# stdout+stderr exactly. Unmarked blocks are ignored, so illustrative or
# hand-written snippets stay possible — but anything marked is a promise.
#
# Creates nothing and needs no network or auth. Runs in CI on every PR.

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root" || exit 1

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

guardrail="$work/otel-guardrail"
if ! go build -o "$guardrail" ./guardrail/cmd/otel-guardrail; then
	echo "FAIL: could not build otel-guardrail"
	exit 1
fi

checked=0
failed=0

# Every marked block, as: FILE<TAB>LINE<TAB>ARGS
markers="$work/markers"
grep -rn '<!-- verify: ' docs/ --include='*.md' |
	sed -E 's/^([^:]+):([0-9]+):.*<!-- verify: (.*) -->.*$/\1\t\2\t\3/' >"$markers"

if [ ! -s "$markers" ]; then
	echo "FAIL: no '<!-- verify: ... -->' markers found in docs/ — the checker is wired to nothing"
	exit 1
fi

while IFS=$'\t' read -r doc line args; do
	checked=$((checked + 1))

	# The fenced block opens on the line after the marker; take everything up to
	# the next fence.
	expected="$work/expected"
	awk -v start="$((line + 1))" '
		NR < start { next }
		NR == start {
			if ($0 !~ /^```/) { print "MARKER_NOT_FOLLOWED_BY_FENCE" > "/dev/stderr"; exit 1 }
			next
		}
		/^```/ { exit }
		{ print }
	' "$doc" >"$expected" 2>"$work/awk.err"

	if [ -s "$work/awk.err" ]; then
		echo "FAIL $doc:$line — the verify marker is not immediately followed by a fenced block"
		failed=$((failed + 1))
		continue
	fi

	actual="$work/actual"
	# shellcheck disable=SC2086 # args are intentionally word-split into flags
	"$guardrail" $args >"$actual" 2>&1

	if diff -u "$expected" "$actual" >"$work/diff"; then
		echo "ok   $doc:$line — otel-guardrail $args"
	else
		echo "FAIL $doc:$line — otel-guardrail $args"
		echo "     the documented output is not what the command prints:"
		sed 's/^/     /' "$work/diff"
		failed=$((failed + 1))
	fi
done <"$markers"

echo
if [ "$failed" -ne 0 ]; then
	echo "$failed of $checked documented example(s) no longer match the CLI"
	exit 1
fi
echo "all $checked documented example(s) match the CLI"
