package copilot

import (
	"regexp"
	"sort"
	"strings"
)

// PROVENANCE, NOT SUPPORT — the distinction this whole file rests on.
//
// There are two different questions you can ask about a citation:
//
//	provenance   Was this trace actually fetched? Did the tool return it, or did
//	             the model produce a plausible-looking ID?
//	support      Does this trace bear out the claim it is attached to? A real
//	             trace cited for something it does not show is still wrong.
//
// This file answers the FIRST and only the first. Provenance is decidable here
// because the conversation records exactly what came back; support needs a judge
// that reads the trace and the claim together, which is #18's mechanism and #20's
// measurement. #53 tracks the gap.
//
// Saying so matters because provenance is the cheaper check and it looks like the
// expensive one. A summary whose every ID is real reads as verified, and it is
// not — it is un-hallucinated, which is a much weaker claim.

// traceIDPattern matches a trace ID as the platform's Backends render one: 32 hex
// characters, the OTLP width.
//
// Deliberately not anchored to surrounding prose. A model writes citations in
// whatever shape the sentence wants — bare, parenthesised, backticked, trailing a
// full stop — and a pattern that assumed one of those would silently miss the
// others, reporting a clean summary because it found nothing to check.
var traceIDPattern = regexp.MustCompile(`\b[0-9a-f]{32}\b`)

// Citations partitions the trace IDs a summary mentions into those the tools
// actually returned and those they did not.
//
// `cited` is every ID that appears in the summary AND in the conversation's
// evidence. `unknown` is every ID that appears in the summary and nowhere in the
// evidence — a claim resting on a trace that was never fetched.
//
// Both are sorted and de-duplicated, so a caller comparing two runs is comparing
// findings rather than orderings.
//
// A summary that cites nothing returns two empty slices. That is NOT the same as
// a summary that cites correctly, and a caller that treats them alike has built
// the check backwards: an ungrounded claim cites nothing at all, which is exactly
// what ADR 0009 says must be suppressed.
func Citations(summary string, c *Conversation) (cited, unknown []string) {
	if c == nil {
		return nil, nil
	}

	fetched := map[string]bool{}
	for _, ref := range c.Evidence() {
		if ref.TraceID != "" {
			fetched[strings.ToLower(ref.TraceID)] = true
		}
	}

	seen := map[string]bool{}
	for _, match := range traceIDPattern.FindAllString(strings.ToLower(summary), -1) {
		if seen[match] {
			continue
		}
		seen[match] = true

		if fetched[match] {
			cited = append(cited, match)
		} else {
			unknown = append(unknown, match)
		}
	}

	sort.Strings(cited)
	sort.Strings(unknown)
	return cited, unknown
}

// Grounded reports whether every trace ID a summary mentions was actually
// fetched, and that it mentions at least one.
//
// The second half is the part worth stating. "No unknown IDs" is trivially true
// of a summary that cites nothing, so a check written only on `unknown` would
// pass its way through the exact failure ADR 0009 is about — a confident answer
// resting on no evidence at all.
//
// PROVENANCE ONLY. A Grounded summary has not been shown to be correct; it has
// been shown not to have invented its citations. #53 is where support is checked.
func Grounded(summary string, c *Conversation) bool {
	cited, unknown := Citations(summary, c)
	return len(unknown) == 0 && len(cited) > 0
}
