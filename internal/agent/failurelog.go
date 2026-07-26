package agent

import (
	"log/slog"
	"math/bits"
)

// failureLog collapses a run of the same failure into a few lines.
//
// A publisher outage is survivable by design: the compiler keeps serving the
// version it has and catalogs keep compiling, which is the property that makes
// polling better than a shared filesystem. Logging it at ERROR on every poll
// describes that non-event as an emergency, and at the default 30s interval it is
// 120 lines an hour per compiler — a forty-compiler fleet through a two-hour
// maintenance window produces around 9,600 of them. Anything alerting on ERROR
// fires continuously for a state the design calls fine, which is how real alerts
// get muted.
//
// Going quiet is the wrong correction, though: "still broken" is worth knowing,
// and a log that stops mentioning a problem reads like the problem stopped. So:
//
//   - the first failure is news, and logs at ERROR
//   - a failure whose cause *changed* is also news — a publisher that went from
//     unreachable to refusing a revoked certificate is a different problem — so it
//     logs at ERROR too, however deep into a streak it happens
//   - repeats back off to powers of two at WARN, turning 240 polls into 8 lines
//     while still confirming periodically that it has not resolved
//   - recovery always logs, because the end of an outage is news
type failureLog struct {
	consecutive int
	last        string
}

// failed reports err, choosing a level from how much of it is new.
func (f *failureLog) failed(log *slog.Logger, msg string, err error, attrs ...any) {
	cause := err.Error()
	changed := cause != f.last
	f.last = cause

	if changed {
		f.consecutive = 1
	} else {
		f.consecutive++
	}

	args := append([]any{"error", err}, attrs...)

	// First of a streak, or a new cause: something happened, say so loudly.
	if f.consecutive == 1 {
		log.Error(msg, args...)
		return
	}

	// Otherwise only at 2, 4, 8, 16 … so a long outage stays visible without
	// scaling with its length.
	if isPowerOfTwo(f.consecutive) {
		log.Warn(msg, append(args, "consecutive", f.consecutive)...)
	}
}

// succeeded resets the streak, reporting recovery when there was one to recover
// from. Silent otherwise, because a healthy poll is not news.
func (f *failureLog) succeeded(log *slog.Logger, msg string, attrs ...any) {
	if f.consecutive == 0 {
		return
	}
	log.Info(msg, append([]any{"after_failed_attempts", f.consecutive}, attrs...)...)
	f.consecutive = 0
	f.last = ""
}

func isPowerOfTwo(n int) bool { return n > 0 && bits.OnesCount(uint(n)) == 1 }
