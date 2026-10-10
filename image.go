package pomoai

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	mib                = 1 << 20
	maxRefImages       = 4
	maxRefBytes        = 50 * mib
	maxRefTotalBytes   = 100 * mib
	maxRequestBodySize = 128 * mib
	imagesGenPath      = "/v1/images/generations"
	imagesEditPath     = "/v1/images/edits"
)

// isGeminiImageModel: Gemini chat-image models (generateContent), not Imagen.
func isGeminiImageModel(model string) bool {
	name := strings.ToLower(model)
	return !strings.HasPrefix(name, "imagen") && strings.Contains(name, "gemini") && strings.Contains(name, "image")
}

func isImagenModel(model string) bool { return strings.HasPrefix(strings.ToLower(model), "imagen") }

type imageOpts struct {
	model, prompt          string
	images                 []string
	mask                   string
	size, quality          string
	n                      int
	aspectRatio, imageSize string
	params                 []string
}

// parseParams parses repeated --param k=v. JSON bodies take the value as JSON
// when it parses, else as a string.
func parseParams(list []string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range list {
		k, v, ok := strings.Cut(p, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, usageErr("--param must be key=value, got %q", p)
		}
		out[strings.TrimSpace(k)] = v
	}
	return out, nil
}

func jsonParamValue(v string) any {
	var x any
	if json.Unmarshal([]byte(v), &x) == nil {
		return x
	}
	return v
}

type refFile struct {
	name, mime string
	data       []byte
}

func readRefs(paths []string) ([]refFile, error) {
	if len(paths) > maxRefImages {
		return nil, usageErr("at most %d reference images", maxRefImages)
	}
	var out []refFile
	total := 0
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return nil, usageErr("reference image: %v", err)
		}
		if st.Size() > maxRefBytes {
			return nil, usageErr("reference image %s is larger than 50 MiB", p)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, usageErr("reference image: %v", err)
		}
		total += len(data)
		if total > maxRefTotalBytes {
			return nil, usageErr("reference images exceed 100 MiB in total")
		}
		out = append(out, refFile{name: filepath.Base(p), mime: fileMime(p, data), data: data})
	}
	return out, nil
}

func fileMime(name string, data []byte) string {
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); t != "" {
		mt, _, _ := mime.ParseMediaType(t)
		return mt
	}
	mt, _, _ := mime.ParseMediaType(http.DetectContentType(data))
	return mt
}

// buildImageRequest returns the native path, content type and the exact body
// bytes to store and send.
func buildImageRequest(o imageOpts) (path, ctype string, body []byte, err error) {
	params, err := parseParams(o.params)
	if err != nil {
		return "", "", nil, err
	}
	gemini := isGeminiImageModel(o.model)
	switch {
	case isImagenModel(o.model) && len(o.images) > 0:
		return "", "", nil, usageErr("Imagen models do not support reference images")
	case gemini && (o.size != "" || o.n != 0 || o.quality != ""):
		return "", "", nil, usageErr("Gemini image models take --aspect-ratio / --image-size, not --size/--n/--quality")
	case !gemini && (o.aspectRatio != "" || o.imageSize != ""):
		return "", "", nil, usageErr("--aspect-ratio / --image-size only apply to Gemini image models")
	case o.mask != "" && (gemini || len(o.images) == 0):
		return "", "", nil, usageErr("--mask only applies to edits (non-Gemini model with --image)")
	}
	refs, err := readRefs(o.images)
	if err != nil {
		return "", "", nil, err
	}

	if gemini {
		parts := []any{map[string]any{"text": o.prompt}}
		estimate := len(o.prompt) + 4096
		for _, r := range refs {
			estimate += 4*((len(r.data)+2)/3) + 1024
			parts = append(parts, map[string]any{"inlineData": map[string]any{"mimeType": r.mime, "data": base64.StdEncoding.EncodeToString(r.data)}})
		}
		if estimate > maxRequestBodySize {
			return "", "", nil, usageErr("Gemini reference images exceed 128 MiB after base64 encoding; use smaller images")
		}
		gen := map[string]any{"responseModalities": []string{"TEXT", "IMAGE"}}
		ic := map[string]any{}
		if o.aspectRatio != "" {
			ic["aspectRatio"] = o.aspectRatio
		}
		if o.imageSize != "" {
			ic["imageSize"] = o.imageSize
		}
		if len(ic) > 0 {
			gen["imageConfig"] = ic
		}
		m := map[string]any{"contents": []any{map[string]any{"role": "user", "parts": parts}}, "generationConfig": gen}
		for k, v := range params {
			m[k] = jsonParamValue(v)
		}
		body, err = json.Marshal(m)
		return "/v1beta/models/" + url.PathEscape(o.model) + ":generateContent", "application/json", body, err
	}

	if len(refs) == 0 {
		m := map[string]any{"model": o.model, "prompt": o.prompt}
		if o.size != "" {
			m["size"] = o.size
		}
		if o.quality != "" {
			m["quality"] = o.quality
		}
		if o.n != 0 {
			m["n"] = o.n
		}
		for k, v := range params {
			m[k] = jsonParamValue(v)
		}
		body, err = json.Marshal(m)
		return imagesGenPath, "application/json", body, err
	}

	// edits: multipart, every field as a string, files as image[] with their names.
	fields := map[string]string{"model": o.model, "prompt": o.prompt}
	if o.size != "" {
		fields["size"] = o.size
	}
	if o.quality != "" {
		fields["quality"] = o.quality
	}
	if o.n != 0 {
		fields["n"] = strconv.Itoa(o.n)
	}
	for k, v := range params {
		fields[k] = v
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := w.WriteField(k, fields[k]); err != nil {
			return "", "", nil, err
		}
	}
	files := refs
	if o.mask != "" {
		m, err := readRefs([]string{o.mask})
		if err != nil {
			return "", "", nil, err
		}
		files = append(files, m[0])
	}
	for i, r := range files {
		field := "image[]"
		if o.mask != "" && i == len(files)-1 {
			field = "mask"
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, field, strings.NewReplacer(`"`, "_", "\r", "_", "\n", "_").Replace(r.name)))
		h.Set("Content-Type", r.mime)
		pw, err := w.CreatePart(h)
		if err != nil {
			return "", "", nil, err
		}
		pw.Write(r.data)
	}
	if err := w.Close(); err != nil {
		return "", "", nil, err
	}
	return imagesEditPath, w.FormDataContentType(), buf.Bytes(), nil
}

// result is one downloadable output: a URL, or inline bytes.
type result struct {
	url  string
	data []byte
	mime string
}

var geminiImageMime = regexp.MustCompile(`(?i)^image/(png|jpe?g|webp|gif|avif)$`)

// extractImages reads both native shapes: Images data[] (url, b64_json) and
// Gemini candidates[].content.parts[] (fileData uri, inlineData).
func extractImages(body json.RawMessage) ([]result, error) {
	var b struct {
		Data []struct {
			URL     string `json:"url"`
			B64JSON string `json:"b64_json"`
		} `json:"data"`
		Candidates []struct {
			Content struct {
				Parts []map[string]any `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if json.Unmarshal(body, &b) != nil {
		return nil, nil
	}
	var out []result
	for _, d := range b.Data {
		switch {
		case d.URL != "":
			out = append(out, result{url: d.URL})
		case d.B64JSON != "":
			data, err := base64.StdEncoding.DecodeString(d.B64JSON)
			if err != nil {
				return nil, fmt.Errorf("invalid b64_json: %w", err)
			}
			out = append(out, result{data: data, mime: http.DetectContentType(data)})
		}
	}
	str := func(m map[string]any, keys ...string) string {
		for _, k := range keys {
			if s, _ := m[k].(string); s != "" {
				return s
			}
		}
		return ""
	}
	for _, c := range b.Candidates {
		for _, p := range c.Content.Parts {
			if t, _ := p["thought"].(bool); t {
				continue
			}
			file, _ := p["fileData"].(map[string]any)
			if file == nil {
				file, _ = p["file_data"].(map[string]any)
			}
			inline, _ := p["inlineData"].(map[string]any)
			if inline == nil {
				inline, _ = p["inline_data"].(map[string]any)
			}
			mt := str(file, "mimeType", "mime_type")
			if mt == "" {
				mt = str(inline, "mimeType", "mime_type")
			}
			if !geminiImageMime.MatchString(mt) {
				continue
			}
			if uri := str(file, "fileUri", "file_uri"); strings.HasPrefix(strings.ToLower(uri), "http://") || strings.HasPrefix(strings.ToLower(uri), "https://") {
				out = append(out, result{url: uri})
			} else if d := str(inline, "data"); d != "" {
				data, err := base64.StdEncoding.DecodeString(d)
				if err != nil {
					return nil, fmt.Errorf("invalid inlineData: %w", err)
				}
				out = append(out, result{data: data, mime: mt})
			}
		}
	}
	return out, nil
}
