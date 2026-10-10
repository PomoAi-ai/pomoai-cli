package pomoai

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

type fakeResp struct {
	status int
	body   string
	ra     time.Duration
}

// fakeSubmitter replays scripted responses and records sends, sleeps and rekeys.
type fakeSubmitter struct {
	resps  []fakeResp
	keys   []string
	sleeps []time.Duration
	rekeys int
	now    time.Time
}

func (f *fakeSubmitter) submitter() submitter {
	return submitter{
		send: func(_ context.Context, key string) (int, []byte, time.Duration, error) {
			f.keys = append(f.keys, key)
			if len(f.resps) == 0 {
				return 0, nil, 0, fmt.Errorf("unexpected send #%d", len(f.keys))
			}
			r := f.resps[0]
			f.resps = f.resps[1:]
			return r.status, []byte(r.body), r.ra, nil
		},
		sleep: func(_ context.Context, d time.Duration) error { f.sleeps = append(f.sleeps, d); return nil },
		now:   func() time.Time { return f.now },
		rekey: func(string) error { f.rekeys++; return nil },
		log:   func(string, ...any) {},
	}
}

const accepted = `{"id":"tsk_1","status":"running"}`

func errBody(code string) string { return `{"error":{"code":"` + code + `","message":"m"}}` }

func TestClassifySubmit(t *testing.T) {
	cases := []struct {
		status     int
		code       string
		sentBefore bool
		action     string
		class      string
	}{
		{0, "", false, "retry", "network"},
		{408, "", false, "retry", "network"},
		{200, "", false, "retry", "network"},
		{502, "", false, "retry", "network"},
		{503, "task_accept_failed", true, "rekey", ""},
		{404, "task_not_found", true, "rekey", ""},
		{503, "edge_gateway_draining", false, "retry", "draining"},
		{429, "rate_limit_exceeded", false, "retry", "throttled"},
		{503, "task_storage_unavailable", false, "retry", "storage"},
		{503, "task_service_unavailable", false, "retry", "service"},
		{400, "invalid_request", true, "failed", ""},
		{404, "unsupported_endpoint", true, "failed", ""},
		{413, "request_too_large", true, "failed", ""},
		{401, "invalid_api_key", false, "failed", ""},
		{401, "invalid_api_key", true, "unknown", ""},
		{409, "idempotency_conflict", true, "unknown", ""},
		{404, "Not Found", false, "failed", ""},
	}
	for _, c := range cases {
		d := classifySubmit(c.status, c.code, c.sentBefore)
		if d.action != c.action || d.class != c.class {
			t.Errorf("%d %s sent=%v: got %+v", c.status, c.code, c.sentBefore, d)
		}
	}
}

func TestSubmitRekeysAtMostOncePerRecord(t *testing.T) {
	now := time.Now()
	f := &fakeSubmitter{now: now, resps: []fakeResp{{503, errBody("task_accept_failed"), 0}, {503, errBody("task_accept_failed"), 0}}}
	r := f.submitter().run(context.Background(), "k1", false, now, false)
	if r.outcome != "failed" || f.rekeys != 1 || len(f.keys) != 2 || f.keys[0] == f.keys[1] || r.key != f.keys[1] {
		t.Fatalf("first process: %+v keys=%v rekeys=%d", r, f.keys, f.rekeys)
	}
	// A later process sees rekeyed=true in the record: no second rekey.
	f = &fakeSubmitter{now: now, resps: []fakeResp{{503, errBody("task_accept_failed"), 0}}}
	r = f.submitter().run(context.Background(), "k2", false, now, true)
	if r.outcome != "failed" || f.rekeys != 0 || len(f.keys) != 1 {
		t.Fatalf("second process: %+v keys=%v", r, f.keys)
	}
}

func TestSubmitReplayNotFoundWindow(t *testing.T) {
	now := time.Now()
	f := &fakeSubmitter{now: now, resps: []fakeResp{{404, errBody("task_not_found"), 0}, {202, accepted, 0}}}
	r := f.submitter().run(context.Background(), "k", false, now.Add(-47*time.Hour), false)
	if r.outcome != "accepted" || f.rekeys != 1 {
		t.Fatalf("within 48h: %+v rekeys=%d", r, f.rekeys)
	}
	f = &fakeSubmitter{now: now, resps: []fakeResp{{404, errBody("task_not_found"), 0}}}
	r = f.submitter().run(context.Background(), "k", false, now.Add(-49*time.Hour), false)
	if r.outcome != "unknown" || f.rekeys != 0 {
		t.Fatalf("beyond 48h: %+v", r)
	}
}

func TestSubmitNetworkExhaustedIsUnknown(t *testing.T) {
	now := time.Now()
	f := &fakeSubmitter{now: now, resps: []fakeResp{{0, "", 0}, {502, "bad gateway", 0}, {0, "", 0}, {0, "", 0}}}
	r := f.submitter().run(context.Background(), "k", false, now, false)
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if r.outcome != "unknown" || !r.sentBefore || fmt.Sprint(f.sleeps) != fmt.Sprint(want) || len(f.keys) != 4 {
		t.Fatalf("%+v sleeps=%v sends=%d", r, f.sleeps, len(f.keys))
	}
	// After an ambiguous send, a 401 can no longer prove non-acceptance.
	f = &fakeSubmitter{now: now, resps: []fakeResp{{0, "", 0}, {401, errBody("invalid_api_key"), 0}}}
	if r := f.submitter().run(context.Background(), "k", false, now, false); r.outcome != "unknown" {
		t.Fatalf("401 after network error: %+v", r)
	}
}

func TestSubmitRetryAfterFloorAndCap(t *testing.T) {
	now := time.Now()
	f := &fakeSubmitter{now: now, resps: []fakeResp{
		{429, errBody("task_capacity_exceeded"), 30 * time.Second},
		{429, errBody("task_capacity_exceeded"), 600 * time.Second},
		{429, errBody("task_capacity_exceeded"), 0},
		{429, errBody("task_capacity_exceeded"), 0},
	}}
	r := f.submitter().run(context.Background(), "k", false, now, false)
	want := []time.Duration{30 * time.Second, 60 * time.Second, 20 * time.Second}
	if r.outcome != "failed" || fmt.Sprint(f.sleeps) != fmt.Sprint(want) || !strings.Contains(r.message, "not billed") {
		t.Fatalf("%+v sleeps=%v", r, f.sleeps)
	}
}

func TestSubmitReplayExpiredDoesNotSend(t *testing.T) {
	now := time.Now()
	f := &fakeSubmitter{now: now}
	r := f.submitter().run(context.Background(), "k", true, now.Add(-48*time.Hour), false)
	if r.outcome != "unknown" || r.code != "replay_expired" || len(f.keys) != 0 {
		t.Fatalf("%+v sends=%d", r, len(f.keys))
	}
}
