// Package grounding turns citation PROVENANCE into citation SUPPORT.
//
// P1 could already prove that every trace a summary cites was actually fetched.
// It could not prove the trace shows what the claim says it shows — and those are
// different questions:
//
//	provenance   Was this trace fetched? Decidable from the conversation, which
//	             records exactly what came back. That is copilot.Citations.
//	support      Does this trace bear out the claim attached to it? A real trace
//	             cited for something it does not show is still wrong.
//
// Provenance is the cheaper check and it looks like the expensive one: a summary
// whose every ID is real reads as verified when it is only un-hallucinated. This
// package closes that gap (#53), and ADR 0009 is what requires it — an ungrounded
// hypothesis is suppressed or flagged, never emitted as fact.
package grounding

import (
	"regexp"
	"strings"
)

// Claim is one assertion from a summary, with the traces it rests on.
//
// A claim is a SENTENCE. That is a heuristic and it is worth naming as one: the
// better long-term shape is for the model to emit claims structurally, each with
// its own citation list, so nothing has to be inferred from punctuation. Sentence
// splitting is what makes this checkable today without changing how summaries are
// generated, and the seam below does not depend on it — swapping in structured
// claims changes this file and nothing else.
type Claim struct {
	// Text is the sentence, as the model wrote it.
	Text string
	// CitedTraceIDs are the trace IDs appearing in it, in order, de-duplicated.
	CitedTraceIDs []string
}

// Uncited reports a claim that rests on no trace at all.
//
// This is the case ADR 0009 is most concerned with — a confident sentence with
// nothing behind it — and it is why an uncited sentence is still a Claim rather
// than something Claims() filters out. Dropping it here would make the check pass
// by ignoring exactly what it exists to catch.
func (c Claim) Uncited() bool { return len(c.CitedTraceIDs) == 0 }

// traceIDPattern matches a trace ID as this platform's Backends render one: 32
// hex characters, the OTLP width. Same shape copilot.Citations looks for.
var traceIDPattern = regexp.MustCompile(`\b[0-9a-f]{32}\b`)

// sentenceEnd matches a full stop, question mark or exclamation that genuinely
// ends a sentence: one followed by whitespace and an upper-case letter, or by the
// end of the text.
//
// THE LOOKAHEAD IS LOAD-BEARING. This domain is full of full stops that end
// nothing — `service.name`, `otel.platform.config_version`, `sha256:abc.`,
// version numbers, `e.g.` — and a naive split on "." would cut an attribute name
// in half and judge each half as a separate claim. Requiring whitespace plus a
// capital is not perfect, but it is wrong in the safe direction: it merges two
// sentences rather than inventing a claim that nobody wrote.
var sentenceEnd = regexp.MustCompile(`[.!?](?:\s+(?:[A-Z])|\s*$)`)

// Claims breaks a summary into its claims.
//
// One claim per sentence, because a paragraph in which two sentences are solid
// and a third is invented deserves three verdicts, not one. A single verdict for
// the whole summary would leave an operator unable to tell which part to trust.
func Claims(summary string) []Claim {
	var out []Claim

	for _, sentence := range split(summary) {
		text := strings.TrimSpace(sentence)
		if text == "" || strings.Trim(text, ".!? \t\n") == "" {
			continue
		}
		out = append(out, Claim{Text: text, CitedTraceIDs: traceIDs(text)})
	}
	return out
}

// split cuts the text at sentence ends, keeping the terminator with the sentence
// it ends.
func split(text string) []string {
	locs := sentenceEnd.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		return []string{text}
	}

	var out []string
	start := 0
	for _, loc := range locs {
		// loc[0] is the punctuation itself; keep it, and leave the following
		// whitespace and capital for the next sentence.
		end := loc[0] + 1
		out = append(out, text[start:end])
		start = end
	}
	if start < len(text) {
		out = append(out, text[start:])
	}
	return out
}

// traceIDs pulls the cited IDs out of one sentence, lower-cased and
// de-duplicated. Case is not meaningful in hex, and the same trace named twice is
// one citation.
func traceIDs(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, id := range traceIDPattern.FindAllString(strings.ToLower(text), -1) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
