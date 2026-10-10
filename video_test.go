package pomoai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseVideoResponse(t *testing.T) {
	cases := []struct {
		name, proto, phase, body string
		id, status, url, err     string
		progress                 float64
	}{
		{"ark create", "ark_v3", "create", `{"id":"cgt-1"}`, "cgt-1", "queued", "", "", 0},
		{"ark done", "ark_v3", "query", `{"id":"cgt-1","status":"succeeded","content":{"video_url":"https://cos/v.mp4"}}`, "cgt-1", "completed", "https://cos/v.mp4", "", 0},
		{"las failed", "las_v1", "query", `{"id":"l-1","status":"failed","error":{"code":"x","message":"bad prompt"}}`, "l-1", "failed", "", "bad prompt", 0},
		{"minimax create", "minimax_v2", "create", `{"task_id":"mm-1","status":"queued"}`, "mm-1", "queued", "", "", 0},
		{"minimax query", "minimax_v2", "query", `{"task":{"id":"mm-1","status":"succeeded","content":{"url":"https://cos/m.mp4"}}}`, "mm-1", "completed", "https://cos/m.mp4", "", 0},
		{"openai flat", "openai_video", "query", `{"id":"video_1","object":"video","status":"in_progress","progress":42}`, "video_1", "running", "", "", 42},
		{"openai done", "openai_video", "query", `{"id":"video_1","status":"completed","download_url":"/v1/videos/video_1/content"}`, "video_1", "completed", "/v1/videos/video_1/content", "", 0},
		{"newapi create", "newapi_video", "create", `{"code":"success","data":{"task_id":"na-1","status":"SUBMITTED"}}`, "na-1", "queued", "", "", 0},
		{"newapi envelope", "newapi_video", "query", `{"code":"success","data":{"task_id":"na-1","status":"IN_PROGRESS","progress":"30%"}}`, "na-1", "running", "", "", 30},
		{"newapi public", "newapi_video", "query", `{"id":"na-1","object":"video","status":"completed","download_url":"https://cos/n.mp4"}`, "na-1", "completed", "https://cos/n.mp4", "", 0},
		{"newapi failure", "newapi_video", "query", `{"code":"success","data":{"task_id":"na-1","status":"FAILURE","fail_reason":"nsfw"}}`, "na-1", "failed", "", "nsfw", 0},
		{"unknown", "minimax_v2", "query", `{"id":"mm-1","status":"unknown"}`, "mm-1", "unknown", "", "", 0},
		{"unsafe url", "ark_v3", "query", `{"id":"c","status":"succeeded","content":{"video_url":"https://u:p@cos/v.mp4"}}`, "c", "completed", "", "", 0},
	}
	for _, c := range cases {
		v, err := parseVideoResponse(c.proto, c.phase, []byte(c.body), "")
		if err != nil || v.id != c.id || v.status != c.status || v.url != c.url || v.err != c.err || v.progress != c.progress {
			t.Errorf("%s: %+v %v", c.name, v, err)
		}
	}
	for name, body := range map[string]string{
		"newapi not success": `{"code":"error","message":"no channel"}`,
		"error mid-flight":   `{"id":"c","status":"running","error":"boom"}`,
		"missing id":         `{"status":"running"}`,
		"bad status":         `{"id":"c","status":"weird"}`,
	} {
		proto := "ark_v3"
		if name == "newapi not success" {
			proto = "newapi_video"
		}
		if _, err := parseVideoResponse(proto, "query", []byte(body), ""); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := parseVideoResponse("ark_v3", "query", []byte(`{"id":"other","status":"running"}`), "cgt-1"); err == nil {
		t.Error("mismatched id accepted")
	}
}

func TestBuildVideoPayload(t *testing.T) {
	body, err := buildVideoPayload(protocolByID("minimax_v2"), videoOpts{model: "m", prompt: "p", image: "https://x/a.png"})
	var m map[string]any
	if err != nil || json.Unmarshal(body, &m) != nil || m["ratio"] != "adaptive" || m["duration"] != 6.0 || m["resolution"] != "768P" {
		t.Fatalf("minimax: %s %v", body, err)
	}
	if _, err := buildVideoPayload(protocolByID("ark_v3"), videoOpts{model: "m", prompt: "p", image: "./a.png"}); err == nil || err.(*ExitError).Code != "video_reference_must_be_url" {
		t.Fatalf("local reference: %v", err)
	}
	if _, err := buildVideoPayload(protocolByID("openai_video"), videoOpts{model: "m", prompt: "p", duration: 5}); err == nil {
		t.Fatal("openai 5s accepted")
	}
}

const arkCreatePath = "/api/v3/contents/generations/tasks"

// arkFake serves an ark video whose tsk succeeded with native id cgt-1;
// native answers the n-th native query (1-based).
func arkFake(t *testing.T, native func(n int) map[string]any) *fakeAPI {
	f := newFakeAPI(t)
	f.handlers["GET /v1/models"] = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeJSONStatus(w, 200, map[string]any{"success": true, "object": "list", "data": []any{
			map[string]any{"id": "seedance", "supported_endpoint_types": []string{"ark-video"}}}})
	}
	f.handlers["POST /v1/tasks"+arkCreatePath] = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeJSONStatus(w, 202, envelopeJSON("running", arkCreatePath, nil))
	}
	f.handlers["GET /v1/tasks/"+testTaskID] = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeJSONStatus(w, 200, envelopeJSON("succeeded", arkCreatePath, map[string]any{
			"completed_at": time.Now().Unix(), "native_task_id": "cgt-1",
			"response": map[string]any{"status_code": 200, "body": map[string]any{"id": "cgt-1"}}}))
	}
	queries := 0
	f.handlers["GET "+arkCreatePath+"/cgt-1"] = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		queries++
		writeJSONStatus(w, 200, native(queries))
	}
	return f
}

func TestE2EVideoArk(t *testing.T) {
	var f *fakeAPI
	f = arkFake(t, func(n int) map[string]any {
		if n == 1 {
			return map[string]any{"id": "cgt-1", "status": "running"}
		}
		return map[string]any{"id": "cgt-1", "status": "succeeded", "content": map[string]any{"video_url": f.srv.URL + "/files/v.mp4"}}
	})
	f.handlers["GET /files/v.mp4"] = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Write([]byte("mp4-bytes"))
	}
	a, home := testApp(t, f)
	outDir := t.TempDir()
	code, out, stderr := runCLI(t, a, "video", "create", "--model", "seedance", "--prompt", "sunrise", "--json", "--out-dir", outDir)
	r := parseOut(t, out)
	want := filepath.Join(outDir, "pomoai-abababab.mp4")
	if code != 0 || !r.OK || len(r.Files) != 1 || r.Files[0].Path != want {
		t.Fatalf("code=%d out=%s stderr=%s", code, out, stderr)
	}
	if data, _ := os.ReadFile(want); string(data) != "mp4-bytes" {
		t.Fatalf("file content %q", data)
	}
	var sent map[string]any
	json.Unmarshal(f.bodies[0], &sent)
	if sent["ratio"] != "16:9" || sent["duration"] != 5.0 || sent["resolution"] != "720p" {
		t.Fatalf("create body: %s", f.bodies[0])
	}
	rec, _ := newStore(home).find(r.LocalID)
	if rec == nil || rec.Status != stSucceeded || rec.NativeTaskID != "cgt-1" || rec.Protocol != "ark_v3" {
		t.Fatalf("record: %+v", rec)
	}
}

// A completed video without a usable URL must end in exit 5 (bounded re-check),
// on the waiting path and on the one-shot download path.
func TestE2EVideoCompletedWithoutURLIsUnknown(t *testing.T) {
	f := arkFake(t, func(int) map[string]any {
		return map[string]any{"id": "cgt-1", "status": "succeeded", "content": map[string]any{"video_url": "https://u:p@cos/v.mp4"}}
	})
	a, _ := testApp(t, f)
	clock, sleeps := time.Now(), 0
	a.now = func() time.Time { return clock }
	a.sleep = func(ctx context.Context, d time.Duration) error {
		if sleeps++; sleeps > 500 {
			return errors.New("still polling after 500 sleeps")
		}
		clock = clock.Add(d)
		return ctx.Err()
	}
	code, out, stderr := runCLI(t, a, "video", "create", "--model", "seedance", "--prompt", "sunrise", "--json", "--out-dir", t.TempDir())
	r := parseOut(t, out)
	if code != exitUnknown || r.Error.Code != "video_result_unavailable" {
		t.Fatalf("wait path: code=%d out=%s stderr=%s", code, out, stderr)
	}
	code, out, _ = runCLI(t, a, "task", "download", testTaskID, "--json", "--out-dir", t.TempDir())
	if r := parseOut(t, out); code != exitUnknown || r.Error.Code != "video_result_unavailable" {
		t.Fatalf("download path: code=%d out=%s", code, out)
	}
}
