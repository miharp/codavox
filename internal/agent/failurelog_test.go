package agent

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// capture returns a logger writing to buf, at a level low enough to see
// everything the failureLog might emit.
func capture() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), &buf
}

func levels(buf *bytes.Buffer) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		for _, f := range strings.Fields(line) {
			if strings.HasPrefix(f, "level=") {
				out = append(out, strings.TrimPrefix(f, "level="))
			}
		}
	}
	return out
}

// The reported problem: a publisher outage logged one ERROR per poll forever. At
// the default 30s interval a two-hour outage is 240 polls, and 240 ERROR lines
// for a state the design calls survivable is how alerting gets muted.
func TestFailureLogCollapsesARunOfTheSameFailure(t *testing.T) {
	log, buf := capture()
	var f failureLog
	err := errors.New("polling publisher: connection refused")

	for range 240 {
		f.failed(log, "sync failed", err)
	}

	got := levels(buf)
	// 1 (ERROR) + powers of two up to 240: 2,4,8,16,32,64,128 = 7 WARN.
	if len(got) != 8 {
		t.Fatalf("240 identical failures produced %d lines, want 8:\n%s", len(got), buf.String())
	}
	if got[0] != "ERROR" {
		t.Errorf("first line is %s, want ERROR — the start of an outage is news", got[0])
	}
	for i, l := range got[1:] {
		if l != "WARN" {
			t.Errorf("repeat %d logged at %s, want WARN", i+2, l)
		}
	}
}

// Going quiet would be the wrong fix: a log that stops mentioning a problem reads
// like the problem stopped. Repeats have to keep appearing, just not per poll.
func TestFailureLogKeepsReportingDuringALongOutage(t *testing.T) {
	log, buf := capture()
	var f failureLog
	err := errors.New("connection refused")

	for range 100 {
		f.failed(log, "sync failed", err)
	}
	if !strings.Contains(buf.String(), "consecutive=64") {
		t.Errorf("a 100-poll outage never reported its length:\n%s", buf.String())
	}
}

// A publisher that goes from unreachable to refusing a revoked certificate is a
// different problem, and must not be swallowed as "more of the same".
func TestFailureLogReportsAChangedCauseAtError(t *testing.T) {
	log, buf := capture()
	var f failureLog

	for range 50 {
		f.failed(log, "sync failed", errors.New("connection refused"))
	}
	before := len(levels(buf))

	f.failed(log, "sync failed", errors.New("peer certificate is revoked"))

	got := levels(buf)
	if len(got) != before+1 {
		t.Fatalf("a changed cause logged %d lines, want 1", len(got)-before)
	}
	if got[len(got)-1] != "ERROR" {
		t.Errorf("changed cause logged at %s, want ERROR", got[len(got)-1])
	}
	if !strings.Contains(buf.String(), "revoked") {
		t.Error("the new cause is not in the output")
	}
}

// The end of an outage is news, and it is the line an operator greps for to find
// out how long it lasted.
func TestFailureLogReportsRecovery(t *testing.T) {
	log, buf := capture()
	var f failureLog

	for range 5 {
		f.failed(log, "sync failed", errors.New("connection refused"))
	}
	f.succeeded(log, "sync recovered")

	out := buf.String()
	if !strings.Contains(out, "sync recovered") {
		t.Fatalf("recovery was not logged:\n%s", out)
	}
	if !strings.Contains(out, "after_failed_attempts=5") {
		t.Errorf("recovery did not say how long the outage was:\n%s", out)
	}
}

// A healthy agent polls constantly and must stay silent, or the collapsing has
// just moved the noise rather than removed it.
func TestFailureLogIsSilentWhenNothingIsWrong(t *testing.T) {
	log, buf := capture()
	var f failureLog

	for range 100 {
		f.succeeded(log, "sync recovered")
	}
	if buf.Len() != 0 {
		t.Errorf("a healthy agent logged %d bytes:\n%s", buf.Len(), buf.String())
	}
}

// After recovery the next failure is a fresh outage, not a continuation.
func TestFailureLogResetsAfterRecovery(t *testing.T) {
	log, buf := capture()
	var f failureLog
	err := errors.New("connection refused")

	for range 10 {
		f.failed(log, "sync failed", err)
	}
	f.succeeded(log, "sync recovered")
	buf.Reset()

	f.failed(log, "sync failed", err)

	got := levels(buf)
	if len(got) != 1 || got[0] != "ERROR" {
		t.Errorf("the first failure after recovery logged %v, want one ERROR", got)
	}
}

func TestIsPowerOfTwo(t *testing.T) {
	for _, n := range []int{1, 2, 4, 8, 1024} {
		if !isPowerOfTwo(n) {
			t.Errorf("isPowerOfTwo(%d) = false", n)
		}
	}
	for _, n := range []int{0, -1, 3, 6, 100} {
		if isPowerOfTwo(n) {
			t.Errorf("isPowerOfTwo(%d) = true", n)
		}
	}
}
