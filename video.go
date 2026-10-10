package pomoai

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type videoProtocol struct {
	ID           string
	EndpointType string
	CreatePath   string
	QueryBase    string
	Duration     int
	Resolution   string
	Ratio        string
	Size         string
}

var videoProtocols = []videoProtocol{
	{"ark_v3", "ark-video", "/api/v3/contents/generations/tasks", "/api/v3/contents/generations/tasks", 5, "720p", "16:9", ""},
	{"las_v1", "las-video", "/api/v1/contents/generations/tasks", "/api/v1/contents/generations/tasks", 5, "720p", "16:9", ""},
	{"newapi_video", "newapi-video", "/v1/video/generations", "/v1/video/generations", 5, "720p", "16:9", ""},
	{"openai_video", "openai-video", "/v1/videos", "/v1/videos", 4, "", "16:9", "1280x720"},
	{"minimax_v2", "minimax-video-v2", "/v2/video_generation", "/v2/query/video_generation", 6, "768P", "16:9", ""},
}

func protocolByID(id string) *videoProtocol {
	for i := range videoProtocols {
		if videoProtocols[i].ID == id {
			return &videoProtocols[i]
		}
	}
	return nil
}

// protocolByEndpoint matches an endpoint type or create path ("video" means openai).
func protocolByEndpoint(typeOrPath string) *videoProtocol {
	if typeOrPath == "video" {
		typeOrPath = "openai-video"
	}
	for i := range videoProtocols {
		if p := &videoProtocols[i]; p.EndpointType == typeOrPath || p.CreatePath == typeOrPath {
			return p
		}
	}
	return nil
}

// safeURL accepts only http(s) URLs without userinfo or whitespace/control
// characters, or a same-origin path starting with a single "/".
func safeURL(v string) bool {
	if v == "" || strings.IndexFunc(v, func(r rune) bool { return r <= 0x20 }) >= 0 {
		return false
	}
	if strings.HasPrefix(v, "/") {
		return !strings.HasPrefix(v, "//") && !strings.Contains(v, `\`)
	}
	u, err := url.Parse(v)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil && u.Host != ""
}

var assetRefRe = regexp.MustCompile(`^asset://[^\s/?#]+$`)

var videoRatios = []string{"16:9", "9:16", "1:1", "4:3", "3:4", "21:9", "adaptive"}

type videoOpts struct {
	model, prompt, protocol string
	duration                int
	resolution, ratio, size string
	image                   string
	params                  []string
}

// buildVideoPayload builds the native create body. Only hard limits are
// checked locally; resolution/size values are left to the server.
func buildVideoPayload(p *videoProtocol, o videoOpts) ([]byte, error) {
	params, err := parseParams(o.params)
	if err != nil {
		return nil, err
	}
	d := o.duration
	if d == 0 {
		d = p.Duration
	}
	switch {
	case d < 2 || d > 30:
		return nil, usageErr("--duration must be an integer between 2 and 30 seconds")
	case p.ID == "openai_video" && d != 4 && d != 8 && d != 12:
		return nil, usageErr("openai_video supports --duration 4, 8 or 12")
	case p.ID == "las_v1" && d < 4:
		return nil, usageErr("las_v1 needs --duration >= 4")
	case p.ID == "minimax_v2" && d > 15:
		return nil, usageErr("minimax_v2 supports --duration <= 15")
	case p.ID == "openai_video" && o.resolution != "":
		return nil, usageErr("openai_video takes --size, not --resolution")
	case p.ID != "openai_video" && o.size != "":
		return nil, usageErr("--size only applies to openai_video; use --resolution")
	}
	ref := strings.TrimSpace(o.image)
	if ref != "" && !assetRefRe.MatchString(ref) && !((strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")) && safeURL(ref)) {
		return nil, &ExitError{Exit: exitUsage, Code: "video_reference_must_be_url",
			Msg: "video --image must be an http(s) URL or asset://ID; local files are not supported"}
	}
	resolution, ratio := cmp.Or(o.resolution, p.Resolution), cmp.Or(o.ratio, p.Ratio)
	if !slices.Contains(videoRatios, ratio) {
		return nil, usageErr("--ratio must be one of %s", strings.Join(videoRatios, ", "))
	}
	var m map[string]any
	switch p.ID {
	case "openai_video":
		m = map[string]any{"model": o.model, "prompt": o.prompt, "seconds": strconv.Itoa(d), "size": cmp.Or(o.size, p.Size)}
		if ref != "" {
			m["input_reference"] = map[string]any{"image_url": ref}
		}
	case "newapi_video":
		m = map[string]any{"model": o.model, "prompt": o.prompt, "seconds": strconv.Itoa(d),
			"metadata": map[string]any{"resolution": resolution, "ratio": ratio}}
		if ref != "" {
			m["image"] = ref
		}
	default:
		content := []any{map[string]any{"type": "text", "text": o.prompt}}
		if ref != "" {
			content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{"url": ref}, "role": "first_frame"})
			if p.ID == "minimax_v2" {
				ratio = "adaptive"
			}
		}
		m = map[string]any{"model": o.model, "content": content, "duration": d, "resolution": resolution, "ratio": ratio}
	}
	for k, v := range params {
		m[k] = jsonParamValue(v)
	}
	return json.Marshal(m)
}

// videoState is a normalized native video task.
type videoState struct {
	id, status, err, url string
	progress             float64
}

var videoStatusMap = map[string]string{
	"queued": "queued", "pending": "queued", "submitted": "queued", "not_start": "queued",
	"running": "running", "in_progress": "running", "processing": "running",
	"succeeded": "completed", "success": "completed", "completed": "completed",
	"failed": "failed", "failure": "failed",
	"cancelled": "cancelled", "canceled": "cancelled",
	"unknown": "unknown",
}

// truthy mirrors JavaScript truthiness for decoded JSON values.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case bool:
		return x
	case float64:
		return x != 0
	}
	return true
}

func jsString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func errorText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if m, ok := v.(map[string]any); ok {
		for _, k := range []string{"message", "code"} {
			if truthy(m[k]) {
				return jsString(m[k])
			}
		}
	}
	return ""
}

var leadingFloat = regexp.MustCompile(`^\s*[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?`)

// parseVideoResponse normalizes the 5 native create/query shapes (phase is
// "create" or "query"). Status is one of queued/running/completed/failed/
// cancelled/unknown; url is set only for a completed task without error.
func parseVideoResponse(protocol, phase string, data []byte, expectedID string) (*videoState, error) {
	var payload map[string]any
	if json.Unmarshal(data, &payload) != nil || payload == nil {
		return nil, errors.New("the video API returned an invalid response")
	}
	// Submission uncertainty uses one public envelope for all protocols.
	uncertain := payload["status"] == "unknown" && truthy(payload["id"])
	// new-api creates return code/data; its public queries return a video object.
	_, hasCode := payload["code"]
	publicQuery := protocol == "newapi_video" && phase == "query" && payload["object"] == "video" && !hasCode
	newapiEnvelope := protocol == "newapi_video" && !publicQuery
	switch strings.ToLower(jsString(payload["status"])) {
	case "failed", "failure", "cancelled", "canceled", "completed", "succeeded":
	default:
		if !uncertain && truthy(payload["error"]) {
			return nil, errors.New(cmp.Or(errorText(payload["error"]), "video request failed"))
		}
	}
	if !uncertain && newapiEnvelope && payload["code"] != "success" {
		return nil, errors.New(cmp.Or(jsString(payload["message"]), errorText(payload["error"]), "new-api video request failed"))
	}
	var task map[string]any
	switch {
	case uncertain:
		task = payload
	case newapiEnvelope:
		task, _ = payload["data"].(map[string]any)
	case protocol == "minimax_v2" && phase != "create":
		task, _ = payload["task"].(map[string]any)
	default:
		task = payload
	}
	if task == nil {
		return nil, errors.New("the video response has no task data")
	}
	var idv any
	switch {
	case uncertain:
		idv = payload["id"]
	case newapiEnvelope || (protocol == "minimax_v2" && phase == "create"):
		idv = task["task_id"]
	default:
		idv = task["id"]
	}
	id, _ := idv.(string)
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("the video response has no public task id; keep the idempotency key to verify, do not create again")
	}
	if expectedID != "" && id != expectedID {
		return nil, errors.New("the query returned a different task id than requested")
	}
	raw := task["status"]
	if !truthy(raw) && phase == "create" {
		raw = "queued"
	}
	status, ok := videoStatusMap[strings.ToLower(jsString(raw))]
	if !ok {
		return nil, fmt.Errorf("unrecognized video task status: %q", jsString(raw))
	}
	v := &videoState{id: id, status: status, err: cmp.Or(errorText(task["error"]), errorText(task["fail_reason"]))}
	if s := leadingFloat.FindString(jsString(task["progress"])); s != "" {
		f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
		v.progress = min(100, max(0, f))
	}
	content, _ := task["content"].(map[string]any)
	var res any
	for _, c := range []any{task["download_url"], task["result_url"], content["video_url"], content["url"]} {
		if truthy(c) {
			res = c
			break
		}
	}
	if u, _ := res.(string); status == "completed" && v.err == "" && safeURL(u) {
		v.url = u
	}
	return v, nil
}
