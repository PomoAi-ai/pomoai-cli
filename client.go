package pomoai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type client struct {
	base string // normalized, no trailing slash, no /v1
	key  string
	hc   *http.Client
}

// maxResponseBytes caps buffered API responses (task results are capped at 128 MiB server side).
const maxResponseBytes = 256 << 20

type response struct {
	status int
	body   []byte
	header http.Header
}

func userAgent() string {
	return fmt.Sprintf("pomoai-cli/%s (%s/%s)", version, runtime.GOOS, runtime.GOARCH)
}

func newClient(base, key string, hc *http.Client) *client {
	c := &client{base: base, key: key}
	h := *hc
	// Go keeps Authorization on redirects to the same hostname even across
	// scheme or port changes; drop it unless the target is the base origin.
	h.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if !c.sameOrigin(req.URL) {
			req.Header.Del("Authorization")
		}
		return nil
	}
	c.hc = &h
	return c
}

// sameOrigin: same scheme and host (including port) as the base URL.
func (c *client) sameOrigin(u *url.URL) bool {
	base, err := url.Parse(c.base)
	return err == nil && strings.EqualFold(base.Scheme, u.Scheme) && strings.EqualFold(base.Host, u.Host)
}

// open sends a request and returns the raw response. Paths starting with "/"
// are resolved against the base URL. The API key is attached only for the
// base origin, also across redirects.
func (c *client) open(ctx context.Context, method, target string, body []byte, hdr http.Header) (*http.Response, error) {
	if strings.HasPrefix(target, "/") {
		target = c.base + target
	}
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", userAgent())
	if c.sameOrigin(u) {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	return c.hc.Do(req)
}

// do is open + read the whole body. A returned error means no HTTP response
// (network error, timeout, cancellation): treat it as status 0.
func (c *client) do(ctx context.Context, method, target string, body []byte, hdr http.Header) (*response, error) {
	resp, err := c.open(ctx, method, target, body, hdr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("%s %s: response larger than %d MiB", method, target, maxResponseBytes>>20)
	}
	return &response{status: resp.StatusCode, body: data, header: resp.Header}, nil
}

// getJSON is a one-shot GET with a 30s timeout, mapped to ExitErrors.
func (c *client) getJSON(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := c.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return newErr(exitFail, "network_error", err.Error())
	}
	if resp.status != http.StatusOK {
		return apiErr(resp.status, resp.body)
	}
	if err := json.Unmarshal(resp.body, out); err != nil {
		return newErr(exitFail, "invalid_response", fmt.Sprintf("GET %s: %v", path, err))
	}
	return nil
}

func retryAfter(h http.Header) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After")))
	if err != nil || n < 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

const draining = "edge_gateway_draining"

// errorInfo parses both error shapes: {"error":{"code","message"}} and
// {"error":"string", ...} (DrainGuard 503, NoRoute 404).
func errorInfo(status int, data []byte) (code, msg string) {
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	switch e := m["error"].(type) {
	case string:
		code = e
		switch s, _ := m["message"].(string); {
		case e == draining:
			msg = "edge node is draining"
			if r, _ := m["reason"].(string); r != "" {
				msg += " (" + r + ")"
			}
			msg += ", retry later"
		case s != "":
			msg = s
		default:
			msg = e
		}
		return code, msg
	case map[string]any:
		code, _ = e["code"].(string)
		msg, _ = e["message"].(string)
	}
	if msg == "" {
		msg, _ = m["message"].(string)
	}
	if msg == "" {
		if status != 0 {
			msg = fmt.Sprintf("HTTP %d: request failed", status)
		} else {
			msg = "network error or timeout: no response"
		}
	}
	return code, msg
}

func errorCode(data []byte) string {
	code, _ := errorInfo(0, data)
	return code
}

// apiErr turns a non-success HTTP response into an ExitError.
func apiErr(status int, data []byte) *ExitError {
	code, msg := errorInfo(status, data)
	e := &ExitError{Exit: exitForHTTP(status, code), Code: code, Msg: msg, HTTPStatus: status, Hint: hintFor(status, code)}
	if e.Code == "" {
		e.Code = "http_error"
	}
	return e
}

// nativeErrorMessage extracts readable text from a native error body.
func nativeErrorMessage(body json.RawMessage) string {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	switch b := v.(type) {
	case string:
		s := strings.TrimSpace(b)
		if r := []rune(s); len(r) > 300 {
			s = string(r[:300])
		}
		return s
	case map[string]any:
		if e, ok := b["error"].(string); ok {
			_, msg := errorInfo(503, body)
			if e == draining {
				return msg
			}
			if s, _ := b["message"].(string); s != "" {
				return s
			}
			return e
		}
		if e, ok := b["error"].(map[string]any); ok {
			if s, _ := e["message"].(string); s != "" {
				return s
			}
		}
		for _, k := range []string{"message", "msg"} {
			if s, _ := b[k].(string); s != "" {
				return s
			}
		}
	}
	return ""
}

type envelope struct {
	ID           string  `json:"id"`
	Endpoint     string  `json:"endpoint"`
	Model        string  `json:"model"`
	Status       string  `json:"status"`
	CreatedAt    float64 `json:"created_at"`
	CompletedAt  float64 `json:"completed_at"`
	ExpiresAt    float64 `json:"expires_at"`
	NativeTaskID string  `json:"native_task_id"`
	Response     *struct {
		StatusCode int             `json:"status_code"`
		Body       json.RawMessage `json:"body"`
	} `json:"response"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

const retention = 48 * time.Hour

func parseEnvelope(data []byte) (*envelope, bool) {
	var e envelope
	if json.Unmarshal(data, &e) != nil || strings.TrimSpace(e.ID) == "" {
		return nil, false
	}
	switch e.Status {
	case "running", "succeeded", "failed":
		e.NativeTaskID = strings.TrimSpace(e.NativeTaskID)
		return &e, true
	}
	return nil, false
}

func unixTime(sec float64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(sec), 0).UTC()
}

func (e *envelope) body() json.RawMessage {
	if e.Response == nil {
		return nil
	}
	return e.Response.Body
}

func (e *envelope) errCode() string {
	if e.Error == nil {
		return ""
	}
	return e.Error.Code
}

const checkBilling = "; check the usage log to see whether it was billed"

// describeFailure renders a failed envelope as a message.
func describeFailure(e *envelope) string {
	code, msg := e.errCode(), ""
	if e.Error != nil {
		msg = e.Error.Message
	}
	native := nativeErrorMessage(e.body())
	switch code {
	case "native_error":
		if native != "" {
			return native
		}
		if msg != "" {
			return msg
		}
		if e.Response != nil && e.Response.StatusCode != 0 {
			return fmt.Sprintf("upstream returned an error (HTTP %d)", e.Response.StatusCode)
		}
		return "upstream returned an error"
	case "image_transfer_failed":
		s := "image was generated but transferring it failed"
		if native != "" {
			s += ": " + native
		}
		return s + checkBilling
	case "execution_interrupted":
		return "task execution was interrupted (execution time limit exceeded or node failure)" + checkBilling
	case "result_unavailable":
		return "result is no longer readable (past the 48h retention or lost)"
	case "result_empty", "result_too_large", "result_store_failed":
		return "task ran but its result could not be stored or read (" + code + ")" + checkBilling
	}
	if msg != "" {
		return msg
	}
	if code != "" {
		return code
	}
	return "task failed"
}

type modelInfo struct {
	ID    string   `json:"id"`
	Types []string `json:"supported_endpoint_types"`
}

// models lists /v1/models (no client_version / X-Codex-Catalog: those switch
// the response to the Codex catalog format).
func (c *client) models(ctx context.Context) ([]modelInfo, error) {
	var out struct {
		Data []modelInfo `json:"data"`
	}
	if err := c.getJSON(ctx, "/v1/models", &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

type balance struct {
	Remaining float64 `json:"remaining"`
	Used      float64 `json:"used"`
	Total     float64 `json:"total"`
	Unit      string  `json:"unit"`
	PlanName  string  `json:"planName"`
	IsValid   bool    `json:"isValid"`
}

func (c *client) balance(ctx context.Context) (*balance, error) {
	var b balance
	if err := c.getJSON(ctx, "/v1/usage/balance", &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// classifyModel: video if it has a video endpoint type, image if it has
// image-generation or is a Gemini chat-image model, otherwise not listed.
func classifyModel(m modelInfo) (kind, endpoint string, protocols []string) {
	for _, t := range m.Types {
		if p := protocolByEndpoint(t); p != nil {
			protocols = append(protocols, p.ID)
		}
	}
	switch {
	case len(protocols) > 0:
		return "video", "", protocols
	case isGeminiImageModel(m.ID):
		return "image", "gemini", nil
	}
	for _, t := range m.Types {
		if t == "image-generation" {
			return "image", "images", nil
		}
	}
	return "", "", nil
}
