package copilot_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ghantakiran/OTEL/copilot"
)

// A query that names no service is refused rather than answered fleet-wide. The
// alternative — treating an empty service as "everything" — would put the whole
// fleet's telemetry into one prompt, which is both a context problem and an
// injection surface.
func TestAQueryThatNamesNoServiceIsRefused(t *testing.T) {
	err := copilot.TraceQuery{Since: time.Now().Add(-time.Hour)}.Validate()

	if !errors.Is(err, copilot.ErrNoService) {
		t.Fatalf("a query naming no service should be refused, got %v", err)
	}
}

func TestAWindowThatEndsBeforeItStartsIsRefused(t *testing.T) {
	now := time.Now()
	q := copilot.TraceQuery{
		Service: copilot.ServiceIdentity{Name: "checkout-api"},
		Since:   now,
		Until:   now.Add(-time.Hour),
	}

	if err := q.Validate(); err == nil {
		t.Fatal("a backwards window should be refused")
	}
}

// An open-ended window means "until now", and every adapter has to resolve it the
// same way — otherwise two Backends answer the same question over two different
// windows and nothing says so.
func TestAnOpenEndedWindowResolvesToNow(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	since := now.Add(-time.Hour)

	gotSince, gotUntil := copilot.TraceQuery{
		Service: copilot.ServiceIdentity{Name: "checkout-api"},
		Since:   since,
	}.Window(now)

	if !gotSince.Equal(since) {
		t.Errorf("since = %v, want %v", gotSince, since)
	}
	if !gotUntil.Equal(now) {
		t.Errorf("an open window should end now (%v), got %v", now, gotUntil)
	}
}

// A tool result becomes model context, so an unbounded result set is an unbounded
// prompt. A query that sets no limit still has one.
func TestAQueryWithNoLimitStillHasOne(t *testing.T) {
	if got := (copilot.TraceQuery{}).EffectiveLimit(); got != copilot.DefaultLimit {
		t.Errorf("EffectiveLimit() = %d, want the default %d", got, copilot.DefaultLimit)
	}
}
