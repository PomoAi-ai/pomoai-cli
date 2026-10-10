package pomoai

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type waitOpts struct {
	noWait, noDownload bool
	timeout            time.Duration
	out, outDir        string
}

// task drives one local record: submit, poll, native video phase, download.
type task struct {
	a     *app
	c     *client
	st    store
	rec   *record
	fresh bool // record created by this process (status submitting is not a crash leftover)
	start time.Time
}

var taskIDRe = regexp.MustCompile(`^tsk_[0-9a-f]{64}$`)

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (a *app) logf(format string, args ...any) { a.prog.event(fmt.Sprintf(format, args...)) }

// newTask persists the request body and the record before anything is sent.
func (a *app) newTask(s *session, c *client, kind, model, prompt, submitPath, ctype string, body []byte, protocol string) (*task, error) {
	st := newStore(s.home)
	head := []rune(prompt)
	if len(head) > 80 {
		head = head[:80]
	}
	r := &record{
		LocalID: newLocalID(), Kind: kind, Status: stSubmitting, BaseURL: c.base, KeyFP: keyFP(s.key),
		IdempotencyKey: newIdempotencyKey(), SubmitPath: submitPath, ContentType: ctype, BodySHA256: sha256Hex(body),
		Model: model, PromptHead: string(head), Protocol: protocol, CreatedAt: a.now().UTC(),
	}
	if err := writeFileAtomic(st.bodyPath(r.LocalID), body); err != nil {
		return nil, fmt.Errorf("write request body before sending: %w", err)
	}
	if err := st.save(r); err != nil {
		return nil, err
	}
	return &task{a: a, c: c, st: st, rec: r, fresh: true}, nil
}

// openTask loads a record by l_/tsk_ id, or starts an unsaved one for a bare
// task id. The key must be the one that submitted the task.
func (a *app) openTask(s *session, id string) (*task, error) {
	st := newStore(s.home)
	rec, err := st.find(id)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		if !taskIDRe.MatchString(id) {
			return nil, newErr(exitFail, "local_record_not_found", "no local record for "+id)
		}
		c, err := a.apiClient(s)
		if err != nil {
			return nil, err
		}
		rec = &record{LocalID: newLocalID(), TaskID: id, BaseURL: c.base, KeyFP: keyFP(s.key)}
		return &task{a: a, c: c, st: st, rec: rec}, nil
	}
	if rec.KeyFP != keyFP(s.key) {
		return nil, &ExitError{Exit: exitAuth, Code: "key_mismatch", LocalID: rec.LocalID, TaskID: rec.TaskID,
			Msg: "the current API key is not the one that submitted this task (fingerprint " + rec.KeyFP + "); tasks can only be queried with the submitting key"}
	}
	return &task{a: a, c: a.client(rec.BaseURL, s.key), st: st, rec: rec}, nil
}

// save persists the record once its kind is known (bare task ids wait for the first envelope).
// Once the server accepted the task, a failed save is reported as exit 5 with
// the task id so the caller resumes instead of creating again.
func (t *task) save() error {
	if t.rec.Kind == "" {
		return nil
	}
	err := t.st.save(t.rec)
	if err != nil && t.rec.TaskID != "" {
		return t.fail(&ExitError{Exit: exitUnknown, Code: "record_save_failed", Hint: "pomoai task wait " + t.rec.TaskID,
			Msg: err.Error() + " (the task was accepted by the server; do not create again)"})
	}
	return err
}

func (t *task) fail(e *ExitError) *ExitError {
	e.TaskID, e.LocalID = t.rec.TaskID, t.rec.LocalID
	if t.rec.Kind == "" { // never saved: no local id to resume with
		e.LocalID = ""
	}
	return e
}

// drive runs the full resume flow and maps cancellation to exit 4 / 130 / 143.
func (t *task) drive(ctx context.Context, o waitOpts) error {
	t.start = t.a.now()
	t.a.prog.start(time.Now())
	defer t.a.prog.end()
	if o.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, o.timeout, errWaitTimeout)
		defer cancel()
	}
	err := t.run(ctx, o)
	if err != nil && ctx.Err() != nil {
		if serr := t.save(); serr != nil {
			t.a.logf("warning: %v", serr)
		}
		e := ctxExit(ctx, t.rec.resumeID())
		if t.rec.TaskID == "" {
			e.Hint = "pomoai task wait " + t.rec.LocalID
		}
		return t.fail(e)
	}
	return err
}

func (t *task) run(ctx context.Context, o waitOpts) error {
	var env *envelope
	if t.rec.TaskID == "" {
		if t.rec.Status == stFailed {
			e := newErr(exitFail, "submit_failed", "this task was not accepted")
			if t.rec.Error != nil {
				e.Code, e.Msg = t.rec.Error.Code, t.rec.Error.Message
			}
			return t.fail(e)
		}
		var err error
		if env, err = t.submit(ctx); err != nil {
			return err
		}
		if o.noWait {
			t.a.ok(t.output(map[string]any{"status": stRunning, "hint": "pomoai task wait " + t.rec.TaskID}), t.rec.TaskID)
			return nil
		}
	}
	env, err := t.pollTask(ctx, env, false)
	if err != nil {
		return err
	}
	if env.Status == stFailed {
		return t.taskFailed(env)
	}
	srcs, refresh, err := t.results(ctx, env, false)
	if err != nil {
		return err
	}
	return t.finish(ctx, srcs, refresh, o)
}

// results returns the downloadable outputs of a succeeded task and a function
// that fetches fresh URLs (signed URLs expire).
func (t *task) results(ctx context.Context, env *envelope, once bool) ([]result, func(context.Context) ([]result, error), error) {
	if t.rec.Kind == "video" {
		v, err := t.pollNative(ctx, env, once)
		if err != nil {
			return nil, nil, err
		}
		srcs, err := t.videoResult(v)
		refresh := func(ctx context.Context) ([]result, error) {
			v, err := t.pollNative(ctx, env, true)
			if err != nil {
				return nil, err
			}
			return t.videoResult(v)
		}
		return srcs, refresh, err
	}
	srcs, err := extractImages(env.body())
	if err != nil || len(srcs) == 0 {
		msg := "the task succeeded but its response contains no image"
		if err != nil {
			msg = err.Error()
		}
		return nil, nil, t.fail(newErr(exitFail, "result_empty", msg))
	}
	t.rec.Status = stSucceeded
	if err := t.save(); err != nil {
		return nil, nil, err
	}
	refresh := func(ctx context.Context) ([]result, error) {
		env, err := t.pollTask(ctx, nil, true)
		if err != nil {
			return nil, err
		}
		return extractImages(env.body())
	}
	return srcs, refresh, nil
}

func (t *task) output(m map[string]any) map[string]any {
	m["kind"], m["task_id"], m["local_id"], m["model"] = t.rec.Kind, t.rec.TaskID, t.rec.LocalID, t.rec.Model
	if t.rec.Kind == "video" {
		m["protocol"], m["native_task_id"] = t.rec.Protocol, t.rec.NativeTaskID
	}
	return m
}

func (t *task) send(ctx context.Context, key string) (int, []byte, time.Duration, error) {
	body, err := os.ReadFile(t.st.bodyPath(t.rec.LocalID))
	if err != nil {
		return 0, nil, 0, fmt.Errorf("read stored request body: %w", err)
	}
	if sha256Hex(body) != t.rec.BodySHA256 {
		return 0, nil, 0, errors.New("stored request body does not match its checksum")
	}
	h := http.Header{"Content-Type": {t.rec.ContentType}, "Idempotency-Key": {key}}
	resp, err := t.c.do(ctx, http.MethodPost, "/v1/tasks"+t.rec.SubmitPath, body, h)
	if err != nil {
		if ctx.Err() == nil {
			t.a.logf("submit: %v", err)
		}
		return 0, nil, 0, nil
	}
	return resp.status, resp.body, retryAfter(resp.header), nil
}

func (t *task) submit(ctx context.Context) (*envelope, error) {
	r := t.rec
	// A record this process did not just create may have been killed mid-send
	// (its status is submitting or unknown): treat every resume as ambiguous.
	sentBefore := r.SentBefore || !t.fresh
	if r.Status != stSubmitting {
		r.Status = stSubmitting // a crash from here on must look like "sent"
		if err := t.st.save(r); err != nil {
			return nil, err
		}
	}
	t.a.logf("local=%s submitting endpoint=%s model=%s resume: pomoai task wait %s", r.LocalID, r.SubmitPath, r.Model, r.LocalID)
	res := submitter{
		send:  t.send,
		sleep: t.a.sleep,
		now:   t.a.now,
		rekey: func(k string) error {
			r.IdempotencyKey, r.Rekeyed, r.SentBefore = k, true, false // status stays submitting
			return t.st.save(r)
		},
		log: t.a.logf,
	}.run(ctx, r.IdempotencyKey, sentBefore, r.CreatedAt, r.Rekeyed)
	r.IdempotencyKey, r.SentBefore = res.key, res.sentBefore
	switch res.outcome {
	case "accepted":
		r.Status, r.Error = stRunning, nil
		t.applyEnvelope(res.env)
		t.a.logf("accepted task=%s resume: pomoai task wait %s", r.TaskID, r.TaskID)
		if err := t.save(); err != nil {
			return nil, err
		}
		t.st.removeBody(r.LocalID)
		return res.env, nil
	case "failed":
		r.Status = stFailed
		r.Error = &recordError{Code: res.code, Message: res.message, HTTPStatus: res.status}
		t.st.removeBody(r.LocalID)
		if err := t.st.save(r); err != nil {
			return nil, err
		}
		code := res.code
		if code == "" {
			code = "submit_failed"
		}
		e := &ExitError{Exit: exitForHTTP(res.status, res.code), Code: code, Msg: res.message, HTTPStatus: res.status, Hint: hintFor(res.status, res.code)}
		return nil, t.fail(e)
	case "unknown":
		code, hint := "submit_unknown", "pomoai task wait "+r.LocalID
		if res.code == "replay_expired" {
			code = res.code
		}
		r.Status = stUnknown
		r.Error = &recordError{Code: code, Message: res.message, HTTPStatus: res.status}
		if err := t.st.save(r); err != nil {
			return nil, err
		}
		return nil, t.fail(&ExitError{Exit: exitUnknown, Code: code, Msg: res.message, HTTPStatus: res.status, Hint: hint})
	}
	r.Status = stUnknown // stopped; drive() saves and maps the cancellation
	return nil, ctx.Err()
}

// applyEnvelope copies envelope fields into the record (and fills kind/model
// for bare task ids).
func (t *task) applyEnvelope(e *envelope) {
	r := t.rec
	r.TaskID = e.ID
	if r.Model == "" {
		r.Model = e.Model
	}
	if r.Kind == "" {
		switch p := protocolByEndpoint(e.Endpoint); {
		case p != nil:
			r.Kind, r.Protocol = "video", p.ID
		case e.Endpoint == imagesGenPath || e.Endpoint == imagesEditPath || strings.HasSuffix(e.Endpoint, ":generateContent"):
			r.Kind = "image"
		}
		if r.Kind != "" {
			r.CreatedAt = unixTime(e.CreatedAt)
		}
	}
	r.ExpiresAt = unixTime(e.ExpiresAt)
	r.DeadlineAt = time.Time{}
	if e.Status == stRunning && !r.ExpiresAt.IsZero() {
		r.DeadlineAt = r.ExpiresAt.Add(-retention)
	}
	r.CompletedAt = unixTime(e.CompletedAt)
	if e.NativeTaskID != "" {
		r.NativeTaskID = e.NativeTaskID
	}
}

// pollDelay: images 2s first, then 3s, 6s after 30s, 15s after 2 minutes;
// videos 5s first, then 10s.
func pollDelay(kind string, first bool, elapsed time.Duration) time.Duration {
	switch {
	case kind == "video" && first:
		return 5 * time.Second
	case kind == "video":
		return 10 * time.Second
	case first:
		return 2 * time.Second
	case elapsed >= 2*time.Minute:
		return 15 * time.Second
	case elapsed >= 30*time.Second:
		return 6 * time.Second
	}
	return 3 * time.Second
}

const replayGrace = 3 * time.Minute

type queryDecision struct {
	action string // expired | retry | unknown | stop
	delay  time.Duration
}

// classifyQuery decides what to do with a failed GET; failures counts
// consecutive failures including this one.
func classifyQuery(status int, code string, created, expires, now time.Time, failures int) queryDecision {
	backoff := func(d time.Duration) time.Duration {
		for i := 1; i < failures && d < maxBackoff; i++ {
			d *= 2
		}
		return min(d, maxBackoff)
	}
	switch {
	case status == 404 && !expires.IsZero() && !now.Before(expires):
		return queryDecision{action: "expired"}
	case status == 404 && !created.IsZero() && now.Sub(created) < replayGrace:
		return queryDecision{action: "retry"}
	case status == 404:
		return queryDecision{action: "unknown"}
	case status == 401 || status == 403:
		return queryDecision{action: "stop"}
	case status == 429:
		return queryDecision{action: "retry", delay: backoff(5 * time.Second)}
	case status == 503 && code == "task_service_unavailable":
		return queryDecision{action: "retry", delay: 30 * time.Second}
	case status >= 500:
		return queryDecision{action: "retry", delay: 5 * time.Second}
	case status == 0:
		return queryDecision{action: "retry", delay: backoff(3 * time.Second)}
	}
	return queryDecision{action: "stop"}
}

// get is one GET with a 30s timeout; status 0 means no response.
func (t *task) get(ctx context.Context, path string) (int, []byte) {
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := t.c.do(qctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return 0, nil
	}
	return resp.status, resp.body
}

// queryFailed handles a failed query; nil error means retry after the returned delay.
func (t *task) queryFailed(status int, body []byte, created time.Time, failures int, once bool) (time.Duration, error) {
	code, msg := errorInfo(status, body)
	d := classifyQuery(status, code, created, t.rec.ExpiresAt, t.a.now(), failures)
	if once && d.action == "retry" {
		d.action = "stop"
	}
	switch d.action {
	case "expired":
		t.rec.Status = stExpired
		if err := t.save(); err != nil {
			return 0, err
		}
		return 0, t.fail(newErr(exitFail, "task_expired", "the task is past its 48h retention; its result was deleted"))
	case "unknown":
		return 0, t.fail(&ExitError{Exit: exitUnknown, Code: "task_query_unknown", HTTPStatus: status,
			Msg:  "cannot find this task: it may have expired, the submitting key may have been reset, or it was never accepted; check the usage log",
			Hint: "pomoai task wait " + t.rec.resumeID()})
	case "stop":
		if status == 0 {
			return 0, t.fail(newErr(exitFail, "network_error", msg))
		}
		return 0, t.fail(apiErr(status, body))
	}
	t.a.logf("query failed (%s), retrying", describeStatus(status, code))
	return d.delay, nil
}

// pollTask polls GET /v1/tasks/{id} until a terminal envelope. env is the
// accepted envelope from submit (nil when resuming). once = a single query.
func (t *task) pollTask(ctx context.Context, env *envelope, once bool) (*envelope, error) {
	start := t.a.now()
	var delay time.Duration
	if env != nil {
		if env.Status != stRunning {
			return env, nil
		}
		delay = pollDelay(t.rec.Kind, true, 0)
	}
	last := ""
	for failures := 0; ; {
		if delay > 0 {
			if err := t.a.sleep(ctx, delay); err != nil {
				return nil, err
			}
		}
		status, body := t.get(ctx, "/v1/tasks/"+url.PathEscape(t.rec.TaskID))
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if status == http.StatusOK {
			if e, ok := parseEnvelope(body); ok {
				failures = 0
				t.applyEnvelope(e)
				if t.rec.Kind == "" {
					return nil, t.fail(newErr(exitFail, "unsupported_task", "task endpoint "+e.Endpoint+" is not an image or video endpoint"))
				}
				t.a.prog.set(progState{task: e.ID, kind: t.rec.Kind, status: e.Status, model: t.rec.Model})
				if e.Status != last {
					last = e.Status
					if err := t.save(); err != nil {
						return nil, err
					}
				}
				if e.Status != stRunning || once {
					return e, nil
				}
				delay = pollDelay(t.rec.Kind, false, t.a.now().Sub(start))
				continue
			}
			status = http.StatusBadGateway // 200 without an envelope: treat as a proxy error
		}
		failures++
		d, err := t.queryFailed(status, body, t.rec.CreatedAt, failures, once)
		if err != nil {
			return nil, err
		}
		if delay = d; delay == 0 {
			delay = pollDelay(t.rec.Kind, false, t.a.now().Sub(start))
		}
	}
}

func (t *task) elapsed() time.Duration { return t.a.now().Sub(t.start).Truncate(time.Second) }

func (t *task) taskFailed(env *envelope) error {
	msg := describeFailure(env)
	code := env.errCode()
	if code == "" {
		code = "task_failed"
	}
	t.rec.Status = stFailed
	t.rec.Error = &recordError{Code: code, Message: msg}
	if env.Response != nil {
		t.rec.Error.HTTPStatus = env.Response.StatusCode
	}
	if err := t.save(); err != nil {
		return err
	}
	return t.fail(newErr(exitFail, code, msg))
}

type fileOut struct {
	Path        string `json:"path"`
	Bytes       int64  `json:"bytes"`
	ContentType string `json:"content_type"`
}

var errURLExpired = errors.New("download URL expired")

// finish downloads (or lists) the results and prints the success output.
func (t *task) finish(ctx context.Context, srcs []result, refresh func(context.Context) ([]result, error), o waitOpts) error {
	out := t.output(map[string]any{"status": stSucceeded, "elapsed_seconds": int(t.elapsed().Seconds())})
	if o.noDownload {
		var urls []string
		for _, s := range srcs {
			if s.url != "" {
				urls = append(urls, s.url)
			}
		}
		if len(urls) == len(srcs) {
			out["urls"] = urls
			t.a.ok(out, urls...)
			return nil
		} // inline results have no URL: save them as files
	}
	files, err := t.download(ctx, srcs, refresh, o)
	if err != nil {
		return err
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	t.rec.Files = paths
	if err := t.save(); err != nil {
		return err
	}
	out["files"] = files
	t.a.ok(out, paths...)
	return nil
}

func (t *task) download(ctx context.Context, srcs []result, refresh func(context.Context) ([]result, error), o waitOpts) ([]fileOut, error) {
	var files []fileOut
	refreshed := false
	for i := 0; i < len(srcs); i++ {
		f, err := t.fetch(ctx, srcs[i], func(ext string) (string, error) { return t.outPath(o, i, len(srcs), ext) })
		if errors.Is(err, errURLExpired) && !refreshed && refresh != nil {
			refreshed = true
			t.a.logf("download URL expired, fetching a fresh one")
			fresh, rerr := refresh(ctx)
			if rerr != nil {
				return nil, rerr
			}
			if len(fresh) == len(srcs) {
				srcs = fresh
				i--
				continue
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, t.fail(&ExitError{Exit: exitFail, Code: "download_failed", Msg: err.Error(), Hint: "pomoai task download " + t.rec.resumeID()})
		}
		t.a.logf("saved %s (%d bytes)", f.Path, f.Bytes)
		files = append(files, f)
	}
	return files, nil
}

func (t *task) outPath(o waitOpts, i, n int, ext string) (string, error) {
	if o.out != "" {
		p := o.out
		if n > 1 {
			e := filepath.Ext(p)
			p = fmt.Sprintf("%s-%d%s", strings.TrimSuffix(p, e), i+1, e)
		}
		return filepath.Abs(p)
	}
	short := strings.TrimPrefix(t.rec.TaskID, "tsk_")
	if len(short) > 8 {
		short = short[:8]
	}
	name := "pomoai-" + short
	if t.rec.Kind == "image" {
		name += fmt.Sprintf("-%d", i+1)
	}
	dir := o.outDir
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Abs(filepath.Join(dir, name+ext))
}

// fetch writes one result to disk; URLs get 3 retries on network errors/5xx.
func (t *task) fetch(ctx context.Context, src result, pathFor func(ext string) (string, error)) (fileOut, error) {
	if src.url == "" {
		p, err := pathFor(extFor(src.mime, ""))
		if err != nil {
			return fileOut{}, err
		}
		n, err := writePart(p, bytes.NewReader(src.data))
		mt, _, _ := mime.ParseMediaType(src.mime)
		return fileOut{Path: p, Bytes: n, ContentType: mt}, err
	}
	if !safeURL(src.url) {
		return fileOut{}, fmt.Errorf("refusing unsafe result URL")
	}
	delays := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	for attempt := 0; ; attempt++ {
		resp, err := t.c.open(ctx, http.MethodGet, src.url, nil, nil)
		if err == nil {
			switch {
			case resp.StatusCode == http.StatusOK:
				ct := resp.Header.Get("Content-Type")
				var p string
				if p, err = pathFor(extFor(ct, resp.Request.URL.Path)); err != nil {
					resp.Body.Close()
					return fileOut{}, err
				}
				n, werr := writePart(p, resp.Body)
				resp.Body.Close()
				if werr == nil {
					mt, _, _ := mime.ParseMediaType(ct)
					return fileOut{Path: p, Bytes: n, ContentType: mt}, nil
				}
				err = werr
			case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone:
				resp.Body.Close()
				return fileOut{}, errURLExpired
			case resp.StatusCode < 500:
				resp.Body.Close()
				return fileOut{}, fmt.Errorf("download: HTTP %d", resp.StatusCode)
			default:
				resp.Body.Close()
				err = fmt.Errorf("download: HTTP %d", resp.StatusCode)
			}
		}
		if ctx.Err() != nil || attempt == len(delays) {
			return fileOut{}, err
		}
		if serr := t.a.sleep(ctx, delays[attempt]); serr != nil {
			return fileOut{}, serr
		}
	}
}

const videoUnknownLimit = 10 * time.Minute

// pollNative follows a video whose tsk succeeded (= upstream accepted it) via
// the native query API until a terminal status. once = a single query.
func (t *task) pollNative(ctx context.Context, env *envelope, once bool) (*videoState, error) {
	r := t.rec
	p := protocolByID(r.Protocol)
	if p == nil {
		return nil, t.fail(newErr(exitFail, "unsupported_task", "unknown video protocol "+r.Protocol))
	}
	if r.NativeTaskID == "" {
		if v, err := parseVideoResponse(p.ID, "create", env.body(), ""); err == nil {
			r.NativeTaskID = v.id
		}
		if r.NativeTaskID == "" {
			r.Status = stUnknown
			if err := t.save(); err != nil {
				return nil, err
			}
			return nil, t.fail(&ExitError{Exit: exitUnknown, Code: "video_status_unknown",
				Msg:  "upstream accepted the video but no native task id is available; do not create again, report to the user",
				Hint: "pomoai task wait " + r.resumeID()})
		}
		if err := t.save(); err != nil {
			return nil, err
		}
	}
	queryPath := p.QueryBase + "/" + url.PathEscape(r.NativeTaskID)
	var delay time.Duration
	if !once {
		delay = pollDelay("video", true, 0)
	}
	var unknownSince time.Time
	for failures := 0; ; {
		if delay > 0 {
			if err := t.a.sleep(ctx, delay); err != nil {
				return nil, err
			}
		}
		status, body := t.get(ctx, queryPath)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		delay = pollDelay("video", false, 0)
		if status != http.StatusOK {
			failures++
			d, err := t.queryFailed(status, body, r.CompletedAt, failures, once)
			if err != nil {
				return nil, err
			}
			delay = max(delay, d)
			continue
		}
		failures = 0
		v, err := parseVideoResponse(p.ID, "query", body, r.NativeTaskID)
		if err != nil {
			return nil, t.fail(newErr(exitFail, "native_error", err.Error()))
		}
		t.a.prog.set(progState{task: r.TaskID, kind: r.Kind, status: v.status, model: r.Model, native: r.NativeTaskID, pct: v.progress})
		if v.status != r.NativeStatus || v.progress != r.Progress {
			r.NativeStatus, r.Progress = v.status, v.progress
			if err := t.save(); err != nil {
				return nil, err
			}
		}
		switch {
		case once, v.status == "failed", v.status == "cancelled", v.status == "completed" && (v.url != "" || v.err != ""):
			return v, nil
		case v.status == "unknown", v.status == "completed":
			// Unknown creation, or completed without a usable URL. GETs are free,
			// so keep checking for a bounded time, then stop without resubmitting.
			if unknownSince.IsZero() {
				unknownSince = t.a.now()
			} else if t.a.now().Sub(unknownSince) >= videoUnknownLimit {
				return nil, t.videoUncertain(v)
			}
		default:
			unknownSince = time.Time{}
		}
	}
}

// videoUncertain stops on a video whose outcome cannot be determined: exit 5,
// never resubmitted.
func (t *task) videoUncertain(v *videoState) error {
	t.rec.Status = stUnknown
	if err := t.save(); err != nil {
		return err
	}
	e := &ExitError{Exit: exitUnknown, Code: "video_status_unknown", Msg: "it is unknown whether upstream created the video; do not create again"}
	if v.status == "completed" {
		e.Code, e.Msg = "video_result_unavailable", "upstream reports the video completed but gives no usable download URL; do not create again"
	}
	e.Msg += "; `task wait` only re-checks, it never resubmits"
	e.Hint = "pomoai task wait " + t.rec.resumeID()
	return t.fail(e)
}

// videoResult turns a native state into downloadable results or a failure.
func (t *task) videoResult(v *videoState) ([]result, error) {
	r := t.rec
	switch {
	case v.status == "completed" && v.url != "":
		r.Status = stSucceeded
		return []result{{url: v.url}}, t.save()
	case v.status == "unknown" || (v.status == "completed" && v.err == ""):
		return nil, t.videoUncertain(v)
	case v.status == "failed" || v.status == "cancelled" || v.status == "completed":
		code := "video_" + v.status
		if v.status == "completed" {
			code = "video_failed"
		}
		r.Status = stFailed
		r.Error = &recordError{Code: code, Message: cmp.Or(v.err, "video generation "+v.status)}
		if err := t.save(); err != nil {
			return nil, err
		}
		return nil, t.fail(newErr(exitFail, code, r.Error.Message))
	}
	return nil, t.fail(newErr(exitFail, "task_not_succeeded", "video is still "+v.status))
}
