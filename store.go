package pomoai

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Record statuses.
const (
	stSubmitting = "submitting"
	stUnknown    = "unknown"
	stRunning    = "running"
	stSucceeded  = "succeeded"
	stFailed     = "failed"
	stExpired    = "expired"
)

type recordError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// record is the local task state in $POMOAI_HOME/tasks/<local_id>.json.
// It never holds the API key or any signed result URL.
type record struct {
	V              int          `json:"v"`
	LocalID        string       `json:"local_id"`
	Kind           string       `json:"kind"`
	Status         string       `json:"status"`
	BaseURL        string       `json:"base_url"`
	KeyFP          string       `json:"key_fp"`
	IdempotencyKey string       `json:"idempotency_key,omitempty"`
	Rekeyed        bool         `json:"rekeyed"`
	SentBefore     bool         `json:"sent_before"`
	SubmitPath     string       `json:"submit_path,omitempty"`
	ContentType    string       `json:"content_type,omitempty"`
	BodySHA256     string       `json:"body_sha256,omitempty"`
	Model          string       `json:"model,omitempty"`
	PromptHead     string       `json:"prompt_head,omitempty"`
	Protocol       string       `json:"protocol,omitempty"`
	CreatedAt      time.Time    `json:"created_at,omitzero"`
	UpdatedAt      time.Time    `json:"updated_at,omitzero"`
	TaskID         string       `json:"task_id,omitempty"`
	ExpiresAt      time.Time    `json:"expires_at,omitzero"`
	DeadlineAt     time.Time    `json:"deadline_at,omitzero"`
	CompletedAt    time.Time    `json:"completed_at,omitzero"`
	NativeTaskID   string       `json:"native_task_id,omitempty"`
	NativeStatus   string       `json:"native_status,omitempty"`
	Progress       float64      `json:"progress,omitempty"`
	Files          []string     `json:"files,omitempty"`
	Error          *recordError `json:"error,omitempty"`
}

// resumeID is what `pomoai task wait` should be given.
func (r *record) resumeID() string {
	if r.TaskID != "" {
		return r.TaskID
	}
	return r.LocalID
}

type store struct{ dir string }

func newStore(home string) store { return store{dir: filepath.Join(home, "tasks")} }

func (s store) path(localID string) string     { return filepath.Join(s.dir, localID+".json") }
func (s store) bodyPath(localID string) string { return filepath.Join(s.dir, localID+".body") }

func (s store) save(r *record) error {
	r.V = 1
	r.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.path(r.LocalID), data); err != nil {
		return fmt.Errorf("save task record %s: %w", r.LocalID, err)
	}
	return nil
}

func (s store) removeBody(localID string) {
	_ = os.Remove(s.bodyPath(localID))
}

func (s store) read(path string) (*record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r record
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("corrupt task record %s: %w", path, err)
	}
	return &r, nil
}

// find looks a record up by local id (l_…) or task id (tsk_…); nil if absent.
func (s store) find(id string) (*record, error) {
	if strings.HasPrefix(id, "l_") && !strings.ContainsAny(id, `/\.`) {
		r, err := s.read(s.path(id))
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return r, err
	}
	all, err := s.list()
	if err != nil {
		return nil, err
	}
	for _, r := range all {
		if r.TaskID == id {
			return r, nil
		}
	}
	return nil, nil
}

// list returns all records, newest first.
// ponytail: full directory scan; add an index or `task prune` when records reach thousands.
func (s store) list() ([]*record, error) {
	paths, err := filepath.Glob(filepath.Join(s.dir, "l_*.json"))
	if err != nil {
		return nil, err
	}
	out := make([]*record, 0, len(paths))
	for _, p := range paths {
		r, err := s.read(p)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newLocalID() string { return "l_" + randomHex(5) }

func newIdempotencyKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // variant
	h := hex.EncodeToString(b)
	return "cli-" + h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

var extByType = map[string]string{
	"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif", "image/avif": ".avif", "video/mp4": ".mp4",
}

var urlExtRe = regexp.MustCompile(`^\.[A-Za-z0-9]{2,5}$`)

func extFor(contentType, urlPath string) string {
	mt, _, _ := mime.ParseMediaType(contentType)
	if e, ok := extByType[mt]; ok {
		return e
	}
	if e := path.Ext(urlPath); urlExtRe.MatchString(e) {
		return strings.ToLower(e)
	}
	return ".bin"
}

// writePart streams into <path>.part and renames over path (overwrites).
func writePart(p string, r io.Reader) (int64, error) {
	part := p + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, r)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(part, p)
	}
	if err != nil {
		os.Remove(part)
	}
	return n, err
}
