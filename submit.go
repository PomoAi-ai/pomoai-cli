package pomoai

import (
	"context"
	"fmt"
	"time"
)

// Same-key retry delays per failure class before a task is registered.
var submitRetryDelays = map[string][]time.Duration{
	"network":   {1 * time.Second, 2 * time.Second, 4 * time.Second},
	"draining":  {1 * time.Second, 2 * time.Second, 4 * time.Second},
	"throttled": {5 * time.Second, 10 * time.Second, 20 * time.Second},
	"storage":   {5 * time.Second, 5 * time.Second, 5 * time.Second},
	"service":   {30 * time.Second},
}

const maxBackoff = 60 * time.Second

var storageCodes = map[string]bool{
	"task_storage_unavailable":   true,
	"task_registry_unavailable":  true,
	"task_timeout_misconfigured": true,
}

type submitDecision struct {
	action         string // retry | rekey | failed | unknown
	class          string // retry class
	exhausted      string // failed | unknown, once retries run out
	replayNotFound bool
}

// classifySubmit decides what to do with a failed submit. sentBefore means this
// idempotency key had an earlier send with an unknown result: the server checks
// auth before idempotency, so a pre-registration rejection does not prove the
// earlier send was not accepted.
func classifySubmit(status int, code string, sentBefore bool) submitDecision {
	definite := "failed"
	if sentBefore {
		definite = "unknown"
	}
	switch {
	case status == 0 || status == 408 || (status >= 200 && status < 300):
		return submitDecision{action: "retry", class: "network", exhausted: "unknown"}
	case status == 503 && code == "task_accept_failed":
		return submitDecision{action: "rekey"}
	case status == 404 && code == "task_not_found":
		return submitDecision{action: "rekey", replayNotFound: true}
	case status == 503 && code == draining:
		return submitDecision{action: "retry", class: "draining", exhausted: definite}
	case status == 429:
		return submitDecision{action: "retry", class: "throttled", exhausted: definite}
	case status == 503 && storageCodes[code]:
		return submitDecision{action: "retry", class: "storage", exhausted: definite}
	case status == 503 && code == "task_service_unavailable":
		return submitDecision{action: "retry", class: "service", exhausted: definite}
	case status >= 500: // proxy 502/504, non-task 500: may have been accepted
		return submitDecision{action: "retry", class: "network", exhausted: "unknown"}
	case (status == 400 && code != "") ||
		(status == 404 && code == "unsupported_endpoint") ||
		(status == 413 && code == "request_too_large"):
		return submitDecision{action: "failed"} // deterministic, regardless of sentBefore
	}
	return submitDecision{action: definite}
}

type submitResult struct {
	outcome    string // accepted | failed | unknown | stopped
	key        string
	sentBefore bool
	status     int
	code       string
	message    string
	env        *envelope
}

type submitter struct {
	// send posts the stored body once. err != nil means it was not sent at all;
	// a network error is status 0 with err == nil.
	send  func(ctx context.Context, key string) (status int, body []byte, retryAfter time.Duration, err error)
	sleep func(ctx context.Context, d time.Duration) error
	now   func() time.Time
	rekey func(newKey string) error // must persist before returning
	log   func(format string, args ...any)
}

const (
	notAccepted   = " (task not accepted, not billed)"
	unknownHint   = ". The earlier submit has an unknown result: `pomoai task wait` replays it with the same idempotency key (the server returns the original task if it was accepted); do not create again, or check the usage log."
	replayExpired = "the original submit is older than the 48h safe replay window; it is not replayed to avoid a duplicate task and charge. Its result is unknown: check the usage log."
)

// run submits until accepted, definitely failed, or unknown. The key is
// changed at most once per record (rekeyed persists across processes), and
// only on 503 task_accept_failed or a replay 404 task_not_found < 48h.
func (s submitter) run(ctx context.Context, key string, sentBefore bool, createdAt time.Time, rekeyed bool) submitResult {
	ambiguous := sentBefore
	counts := map[string]int{}
	done := func(outcome string, r submitResult) submitResult {
		r.outcome, r.key, r.sentBefore = outcome, key, ambiguous
		return r
	}
	for {
		if ctx.Err() != nil {
			return done("stopped", submitResult{})
		}
		// Checked before every send: retries can cross the window too.
		if ambiguous && s.now().Sub(createdAt) >= retention {
			return done("unknown", submitResult{code: "replay_expired", message: replayExpired})
		}
		status, body, ra, err := s.send(ctx, key)
		if err != nil {
			if ambiguous {
				return done("unknown", submitResult{code: "submit_unknown", message: err.Error() + unknownHint})
			}
			return done("failed", submitResult{code: "submit_failed", message: err.Error()})
		}
		if ctx.Err() != nil {
			if status == 0 { // cancelled mid-flight: may have been accepted
				ambiguous = true
			}
			return done("stopped", submitResult{})
		}
		if status >= 200 && status < 300 {
			if env, ok := parseEnvelope(body); ok {
				return done("accepted", submitResult{status: status, env: env})
			}
		}
		code, msg := errorInfo(status, body)
		d := classifySubmit(status, code, ambiguous)
		switch d.action {
		case "rekey":
			if !rekeyed && (!d.replayNotFound || s.now().Sub(createdAt) < retention) {
				newKey := newIdempotencyKey()
				if err := s.rekey(newKey); err != nil {
					return done("failed", submitResult{code: "submit_failed", message: "persist new idempotency key: " + err.Error()})
				}
				s.log("%s: submitting again with a new idempotency key", describeStatus(status, code))
				rekeyed, key, ambiguous = true, newKey, false
				counts = map[string]int{}
				continue
			}
			if d.replayNotFound {
				return done("unknown", submitResult{status: status, code: code, message: "the server cannot find the earlier submit (it may have expired)" + unknownHint})
			}
			return done("failed", submitResult{status: status, code: code, message: "the task was registered but could not start (not billed); try again later"})
		case "retry":
			delays := submitRetryDelays[d.class]
			n := counts[d.class]
			if d.class == "network" {
				ambiguous = true
			}
			if n < len(delays) {
				counts[d.class] = n + 1
				wait := delays[n]
				if d.class != "network" && d.class != "service" {
					wait = max(wait, min(ra, maxBackoff))
				}
				s.log("retry %d/%d in %s (%s, same idempotency key)", n+1, len(delays), wait, describeStatus(status, code))
				if s.sleep(ctx, wait) != nil {
					return done("stopped", submitResult{})
				}
				continue
			}
			if code == "task_service_unavailable" {
				msg = "this base URL does not serve the task API (it is only on edge nodes); switch the base URL and retry"
			}
			if d.exhausted == "unknown" {
				return done("unknown", submitResult{status: status, code: code, message: msg + unknownHint})
			}
			return done("failed", submitResult{status: status, code: code, message: msg + notAccepted})
		case "unknown":
			return done("unknown", submitResult{status: status, code: code, message: msg + unknownHint})
		}
		return done("failed", submitResult{status: status, code: code, message: msg + notAccepted})
	}
}

func describeStatus(status int, code string) string {
	if status == 0 {
		return "network error"
	}
	if code == "" {
		return fmt.Sprintf("HTTP %d", status)
	}
	return fmt.Sprintf("%d %s", status, code)
}
