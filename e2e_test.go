package pomoai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKey = "sk-test-key"

var testTaskID = "tsk_" + strings.Repeat("ab", 32)

// fakeAPI is a scripted PomoAi edge: handlers by "METHOD path", plus a log of requests.
type fakeAPI struct {
	mu       sync.Mutex
	t        *testing.T
	srv      *httptest.Server
	handlers map[string]func(w http.ResponseWriter, r *http.Request, body []byte)
	calls    []string
	idemKeys []string
	bodies   [][]byte
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{t: t, handlers: map[string]func(http.ResponseWriter, *http.Request, []byte){}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.Path
		f.mu.Lock()
		f.calls = append(f.calls, key)
		if r.Method == http.MethodPost {
			f.idemKeys = append(f.idemKeys, r.Header.Get("Idempotency-Key"))
			f.bodies = append(f.bodies, body)
		}
		h := f.handlers[key]
		f.mu.Unlock()
		if !strings.HasPrefix(r.URL.Path, "/files/") && r.Header.Get("Authorization") != "Bearer "+testKey {
			t.Errorf("%s: missing Authorization", key)
		}
		if h == nil {
			t.Errorf("unexpected request %s", key)
			w.WriteHeader(500)
			return
		}
		h(w, r, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func envelopeJSON(status, endpoint string, extra map[string]any) map[string]any {
	now := time.Now().Unix()
	m := map[string]any{"id": testTaskID, "object": "task", "endpoint": endpoint, "model": "m", "status": status,
		"created_at": now, "expires_at": now + 49*3600, "completed_at": nil, "response": nil, "error": nil}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// testApp wires env vars, a temp home and an instant sleep.
func testApp(t *testing.T, f *fakeAPI) (*app, string) {
	home := t.TempDir()
	t.Setenv("POMOAI_HOME", home)
	t.Setenv("POMOAI_API_KEY", testKey)
	t.Setenv("POMOAI_BASE_URL", f.srv.URL+"/v1/")
	a := newApp(strings.NewReader(""), nil, nil)
	a.sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	return a, home
}

type cliResult struct {
	OK      bool
	TaskID  string `json:"task_id"`
	LocalID string `json:"local_id"`
	Status  string
	Files   []fileOut
	Error   struct{ Code, Message string }
}

func parseOut(t *testing.T, out string) cliResult {
	t.Helper()
	var r cliResult
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("stdout is not one JSON line: %q", out)
	}
	return r
}

var pngBytes = []byte("\x89PNG\r\n\x1a\nfake-image")

// imageHandlers serves the result from a second host (like COS): the API key
// must not be sent there.
func imageHandlers(f *fakeAPI, submit func(w http.ResponseWriter, n int)) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			f.t.Errorf("Authorization sent to a foreign host")
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(pngBytes)
	}))
	f.t.Cleanup(cdn.Close)
	posts, gets := 0, 0
	f.handlers["POST /v1/tasks/v1/images/generations"] = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		posts++
		submit(w, posts)
	}
	f.handlers["GET /v1/tasks/"+testTaskID] = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		if gets++; gets == 1 {
			writeJSONStatus(w, 200, envelopeJSON("running", imagesGenPath, nil))
			return
		}
		writeJSONStatus(w, 200, envelopeJSON("succeeded", imagesGenPath, map[string]any{
			"completed_at": time.Now().Unix(),
			"response":     map[string]any{"status_code": 200, "body": map[string]any{"data": []any{map[string]any{"url": cdn.URL + "/a.png"}}}},
		}))
	}
}

func TestE2EImageAcceptedPollDownload(t *testing.T) {
	f := newFakeAPI(t)
	imageHandlers(f, func(w http.ResponseWriter, _ int) {
		writeJSONStatus(w, 202, envelopeJSON("running", imagesGenPath, nil))
	})
	a, home := testApp(t, f)
	outDir := t.TempDir()
	code, out, stderr := runCLI(t, a, "image", "create", "--model", "gpt-image-1", "--prompt", "a fox", "--json", "--out-dir", outDir)
	r := parseOut(t, out)
	if code != 0 || !r.OK || r.Status != "succeeded" || len(r.Files) != 1 {
		t.Fatalf("code=%d out=%s stderr=%s", code, out, stderr)
	}
	want := filepath.Join(outDir, "pomoai-abababab-1.png")
	if got, _ := os.ReadFile(want); r.Files[0].Path != want || !bytes.Equal(got, pngBytes) {
		t.Fatalf("file %s: %+v", want, r.Files)
	}
	if strings.Contains(stderr, testKey) || strings.Contains(out, testKey) {
		t.Fatal("API key leaked into output")
	}
	rec, err := newStore(home).find(r.LocalID)
	if err != nil || rec == nil || rec.Status != stSucceeded || rec.TaskID != testTaskID {
		t.Fatalf("record: %+v %v", rec, err)
	}
	if _, err := os.Stat(newStore(home).bodyPath(r.LocalID)); !os.IsNotExist(err) {
		t.Fatal("request body should be deleted after acceptance")
	}
	if data, _ := os.ReadFile(newStore(home).path(r.LocalID)); bytes.Contains(data, []byte(testKey)) || bytes.Contains(data, []byte("/a.png")) {
		t.Fatal("record must not hold the key or result URLs")
	}
}

func TestE2EImageAcceptFailedRekeysOnce(t *testing.T) {
	f := newFakeAPI(t)
	imageHandlers(f, func(w http.ResponseWriter, n int) {
		if n == 1 {
			writeJSONStatus(w, 503, map[string]any{"error": map[string]any{"code": "task_accept_failed", "message": "x"}})
			return
		}
		writeJSONStatus(w, 202, envelopeJSON("running", imagesGenPath, nil))
	})
	a, home := testApp(t, f)
	code, out, stderr := runCLI(t, a, "image", "create", "--model", "gpt-image-1", "--prompt", "a fox", "--json", "--out-dir", t.TempDir())
	r := parseOut(t, out)
	if code != 0 || !r.OK {
		t.Fatalf("code=%d out=%s stderr=%s", code, out, stderr)
	}
	if len(f.idemKeys) != 2 || f.idemKeys[0] == f.idemKeys[1] || f.idemKeys[0] == "" || !bytes.Equal(f.bodies[0], f.bodies[1]) {
		t.Fatalf("want two sends with different keys and identical bodies: %v", f.idemKeys)
	}
	rec, _ := newStore(home).find(r.LocalID)
	if rec == nil || !rec.Rekeyed || rec.IdempotencyKey != f.idemKeys[1] {
		t.Fatalf("record: %+v", rec)
	}
}

func TestE2EMissingKey(t *testing.T) {
	t.Setenv("POMOAI_HOME", t.TempDir())
	t.Setenv("POMOAI_BASE_URL", "")
	t.Setenv("POMOAI_API_KEY", "")
	code, out, _ := runCLI(t, nil, "image", "create", "--model", "m", "--prompt", "p", "--json")
	if r := parseOut(t, out); code != exitAuth || r.Error.Code != "api_key_missing" {
		t.Fatalf("code=%d out=%s", code, out)
	}
}

// A record killed mid-send can be left as unknown + sent_before=false after a
// rekey; resuming it must stay ambiguous: exhausted retries end in exit 5 and
// the body is kept for a later replay.
func TestE2EResumeUnknownRecordStaysAmbiguous(t *testing.T) {
	f := newFakeAPI(t)
	f.handlers["POST /v1/tasks"+imagesGenPath] = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeJSONStatus(w, 429, map[string]any{"error": map[string]any{"code": "task_capacity_exceeded", "message": "busy"}})
	}
	a, home := testApp(t, f)
	st := newStore(home)
	body := []byte(`{"model":"m","prompt":"p"}`)
	rec := &record{LocalID: "l_0123456789", Kind: "image", Status: stUnknown, BaseURL: f.srv.URL, KeyFP: keyFP(testKey),
		IdempotencyKey: "cli-k2", Rekeyed: true, SubmitPath: imagesGenPath, ContentType: "application/json",
		BodySHA256: sha256Hex(body), Model: "m", CreatedAt: time.Now().UTC()}
	if err := writeFileAtomic(st.bodyPath(rec.LocalID), body); err != nil {
		t.Fatal(err)
	}
	if err := st.save(rec); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := runCLI(t, a, "task", "wait", rec.LocalID, "--json")
	if r := parseOut(t, out); code != exitUnknown || r.Error.Code != "submit_unknown" {
		t.Fatalf("code=%d out=%s stderr=%s", code, out, stderr)
	}
	if _, err := os.Stat(st.bodyPath(rec.LocalID)); err != nil {
		t.Fatalf("body must be kept for replay: %v", err)
	}
	for _, k := range f.idemKeys {
		if k != "cli-k2" {
			t.Fatalf("resume must reuse the stored key, sent %v", f.idemKeys)
		}
	}
	if got, _ := st.find(rec.LocalID); got.Status != stUnknown || !got.SentBefore {
		t.Fatalf("record: %+v", got)
	}
}
