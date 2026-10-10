package pomoai

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// runCLI runs the CLI in-process and returns exit code, stdout, stderr.
func runCLI(t *testing.T, a *app, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	if a == nil {
		a = newApp(strings.NewReader(""), nil, nil)
	}
	a.stdout, a.stderr = &out, &errb
	code := a.main(context.Background(), args)
	return code, out.String(), errb.String()
}

func TestExitCodeMapping(t *testing.T) {
	cases := []struct {
		status int
		code   string
		want   int
	}{
		{401, "invalid_api_key", exitAuth},
		{403, "access_denied", exitAuth},
		{400, "invalid_request", exitUsage},
		{400, "", exitFail},
		{404, "unsupported_endpoint", exitUsage},
		{404, "Not Found", exitUsage},
		{404, "task_not_found", exitFail},
		{413, "request_too_large", exitUsage},
		{503, "task_service_unavailable", exitUsage},
		{409, "idempotency_conflict", exitFail},
		{503, "edge_gateway_draining", exitFail},
	}
	for _, c := range cases {
		if got := exitForHTTP(c.status, c.code); got != c.want {
			t.Errorf("exitForHTTP(%d,%q)=%d want %d", c.status, c.code, got, c.want)
		}
	}

	code, out, _ := runCLI(t, nil, "version", "--bogus", "--json")
	var res struct {
		OK    bool
		Error struct{ Code string }
	}
	if code != exitUsage || json.Unmarshal([]byte(out), &res) != nil || res.OK || res.Error.Code != "usage" {
		t.Fatalf("bad flag: code=%d out=%q", code, out)
	}
	if code, out, _ := runCLI(t, nil, "guide"); code != 0 || strings.HasPrefix(out, "---") || !strings.Contains(out, "Exit codes") {
		t.Fatalf("guide: code=%d out=%.60q", code, out)
	}
}

func TestErrorShapes(t *testing.T) {
	cases := []struct {
		status          int
		body, code, msg string
	}{
		{429, `{"error":{"code":"task_capacity_exceeded","message":"busy"}}`, "task_capacity_exceeded", "busy"},
		{503, `{"error":"edge_gateway_draining","reason":"deploy","request_id":"r"}`, "edge_gateway_draining", "edge node is draining (deploy), retry later"},
		{404, `{"error":"Not Found"}`, "Not Found", "Not Found"},
		{502, `<html>bad gateway</html>`, "", "HTTP 502: request failed"},
		{0, ``, "", "network error or timeout: no response"},
	}
	for _, c := range cases {
		code, msg := errorInfo(c.status, []byte(c.body))
		if code != c.code || msg != c.msg {
			t.Errorf("%s: got (%q,%q) want (%q,%q)", c.body, code, msg, c.code, c.msg)
		}
	}
	if e := apiErr(404, []byte(`{"error":"Not Found"}`)); e.Exit != exitUsage || e.Hint == "" {
		t.Errorf("NoRoute 404 should be exit 2 with base-url hint: %+v", e)
	}
}

func TestParseEnvelope(t *testing.T) {
	ok := `{"id":"tsk_1","object":"task","status":"failed","expires_at":1700000000,"created_at":null,
	  "response":{"status_code":429,"body":{"error":{"message":"quota exceeded"}}},"error":{"code":"native_error","message":"x"}}`
	e, good := parseEnvelope([]byte(ok))
	if !good || e.errCode() != "native_error" || describeFailure(e) != "quota exceeded" || unixTime(e.ExpiresAt).IsZero() {
		t.Fatalf("envelope: %+v %v", e, good)
	}
	for _, bad := range []string{`{"id":"","status":"running"}`, `{"id":"tsk_1","status":"queued"}`, `[]`, `oops`} {
		if _, good := parseEnvelope([]byte(bad)); good {
			t.Errorf("%s accepted as envelope", bad)
		}
	}
}

func TestClassifyModel(t *testing.T) {
	cases := []struct {
		id, kind, ep string
		types        []string
		protos       int
	}{
		{"gpt-image-1", "image", "images", []string{"image-generation"}, 0},
		{"gemini-2.5-flash-image", "image", "gemini", []string{"gemini"}, 0},
		{"imagen-4.0-generate", "image", "images", []string{"image-generation"}, 0},
		{"sora-2", "video", "", []string{"openai-video"}, 1},
		{"seedance", "video", "", []string{"ark-video", "newapi-video"}, 2},
		{"gpt-4o", "", "", []string{"openai"}, 0},
	}
	for _, c := range cases {
		k, ep, p := classifyModel(modelInfo{ID: c.id, Types: c.types})
		if k != c.kind || ep != c.ep || len(p) != c.protos {
			t.Errorf("%s: got %s/%s/%v", c.id, k, ep, p)
		}
	}
}

func TestExtractImages(t *testing.T) {
	body := `{"data":[{"url":"https://cdn/x.png"},{"b64_json":"iVBORw0KGgo="}],
	  "candidates":[{"content":{"parts":[
	    {"text":"hi"},
	    {"thought":true,"inlineData":{"mimeType":"image/png","data":"AAAA"}},
	    {"fileData":{"mimeType":"image/jpeg","fileUri":"https://cdn/y.jpg"}},
	    {"inline_data":{"mime_type":"image/webp","data":"AAAA"}},
	    {"fileData":{"mimeType":"application/pdf","fileUri":"https://cdn/z.pdf"}}]}}]}`
	got, err := extractImages([]byte(body))
	if err != nil || len(got) != 4 || got[0].url != "https://cdn/x.png" || got[1].mime != "image/png" ||
		got[2].url != "https://cdn/y.jpg" || got[3].mime != "image/webp" || len(got[3].data) != 3 {
		t.Fatalf("%+v %v", got, err)
	}
}

// Go keeps Authorization on a redirect to the same hostname even when the
// scheme or port changes; the client must drop it off the base origin.
func TestRedirectDropsKeyOffOrigin(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("Authorization leaked to %s", r.Host)
		}
	}))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("base origin request lacks Authorization")
		}
		http.Redirect(w, r, plain.URL+"/x", http.StatusFound)
	}))
	defer secure.Close()
	a := newApp(strings.NewReader(""), nil, nil)
	a.hc = secure.Client()
	resp, err := a.client(secure.URL, "k").do(context.Background(), http.MethodGet, "/start", nil, nil)
	if err != nil || resp.status != http.StatusOK {
		t.Fatalf("%v %+v", err, resp)
	}
}
