package pomoai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"
)

// Exit codes. 5 means "outcome unknown: do not create again".
const (
	exitOK      = 0
	exitFail    = 1
	exitUsage   = 2
	exitAuth    = 3
	exitTimeout = 4
	exitUnknown = 5
)

// ExitError is the only error type commands return; Main maps it to the
// process exit code and the {"ok":false,...} output.
type ExitError struct {
	Exit       int
	Code       string
	Msg        string
	HTTPStatus int
	TaskID     string
	LocalID    string
	Hint       string
	Extra      map[string]any
}

func (e *ExitError) Error() string { return e.Code + ": " + e.Msg }

func newErr(exit int, code, msg string) *ExitError {
	return &ExitError{Exit: exit, Code: code, Msg: msg}
}

func usageErr(format string, args ...any) *ExitError {
	return newErr(exitUsage, "usage", fmt.Sprintf(format, args...))
}

// exitForHTTP maps a definite server rejection to an exit code.
func exitForHTTP(status int, code string) int {
	switch {
	case status == 401 || status == 403:
		return exitAuth
	case status == 400 && code != "",
		status == 404 && (code == "unsupported_endpoint" || code == "Not Found"),
		status == 413,
		status == 503 && code == "task_service_unavailable":
		return exitUsage
	}
	return exitFail
}

// hintFor adds the "this base URL has no task API" hint where it applies.
func hintFor(status int, code string) string {
	if (status == 503 && code == "task_service_unavailable") || (status == 404 && code == "Not Found") {
		return "this base URL does not serve the task API (/v1/tasks is only on edge nodes); check --base-url / POMOAI_BASE_URL"
	}
	return ""
}

func (e *ExitError) payload() map[string]any {
	m := map[string]any{"code": e.Code, "message": e.Msg}
	for k, v := range e.Extra {
		m[k] = v
	}
	if e.HTTPStatus != 0 {
		m["http_status"] = e.HTTPStatus
	}
	if e.TaskID != "" {
		m["task_id"] = e.TaskID
	}
	if e.LocalID != "" {
		m["local_id"] = e.LocalID
	}
	if e.Hint != "" {
		m["hint"] = e.Hint
	}
	return m
}

func writeJSON(w io.Writer, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	_, _ = w.Write(buf.Bytes())
}

// signalCause marks a context cancelled by SIGINT/SIGTERM.
type signalCause struct{ sig os.Signal }

func (s signalCause) Error() string { return "interrupted by " + s.sig.String() }

var errWaitTimeout = errors.New("local wait timeout reached")

func signalContext() (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case s := <-ch:
			cancel(signalCause{s})
		case <-ctx.Done():
		}
	}()
	return ctx, func() { signal.Stop(ch); cancel(nil) }
}

// ctxExit converts a cancelled context into the exit error for its cause.
// resumeID is what `pomoai task wait` should be given.
func ctxExit(ctx context.Context, resumeID string) *ExitError {
	hint := ""
	if resumeID != "" {
		hint = "pomoai task wait " + resumeID
	}
	var sc signalCause
	switch cause := context.Cause(ctx); {
	case errors.Is(cause, errWaitTimeout):
		return &ExitError{Exit: exitTimeout, Code: "wait_timeout", Msg: "local --timeout reached; the task continues on the server", Hint: hint}
	case errors.As(cause, &sc):
		exit := 130
		if sc.sig == syscall.SIGTERM {
			exit = 143
		}
		return &ExitError{Exit: exit, Code: "interrupted", Msg: "interrupted; task continues on server: " + hint, Hint: hint}
	}
	return &ExitError{Exit: exitFail, Code: "interrupted", Msg: context.Cause(ctx).Error(), Hint: hint}
}

// progState is what the live status line shows.
type progState struct {
	task, kind, status, model string
	native                    string
	pct                       float64
}

// progress writes to stderr. On a TTY it redraws one status line (spinner,
// 1s refresh); otherwise it prints "pomoai: …" lines on change plus a
// heartbeat every 30s so agent hosts do not treat the process as hung.
type progress struct {
	mu       sync.Mutex
	w        io.Writer
	tty      bool
	st       progState
	since    time.Time
	lastLine time.Time
	frame    int
	drawn    bool
	stop     chan struct{}
	done     chan struct{}
}

func newProgress(w io.Writer) *progress {
	f, ok := w.(*os.File)
	return &progress{w: w, tty: ok && term.IsTerminal(int(f.Fd())) && os.Getenv("TERM") != "dumb"}
}

func shortTaskID(id string) string {
	if len(id) > 16 {
		return id[:8] + "…" + id[len(id)-4:]
	}
	return id
}

func (p *progress) line() string {
	el := time.Since(p.since).Truncate(time.Second)
	if p.tty {
		s := p.st.status
		if p.st.pct > 0 {
			s += fmt.Sprintf(" %g%%", p.st.pct)
		}
		return fmt.Sprintf("%c %s  %s  %s  %02d:%02d  %s", `|/-\`[p.frame%4], shortTaskID(p.st.task), p.st.kind, s,
			int(el.Minutes()), int(el.Seconds())%60, p.st.model)
	}
	s := "task=" + p.st.task
	if p.st.native != "" {
		s += " native=" + p.st.native
	}
	s += " status=" + p.st.status
	if p.st.pct > 0 {
		s += fmt.Sprintf(" progress=%g%%", p.st.pct)
	}
	return s + " elapsed=" + el.String()
}

func (p *progress) draw() {
	fmt.Fprint(p.w, "\r\033[K"+p.line())
	p.drawn = true
}

func (p *progress) clear() {
	if p.drawn {
		fmt.Fprint(p.w, "\r\033[K")
		p.drawn = false
	}
}

// event prints a permanent line.
func (p *progress) event(msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clear()
	fmt.Fprintln(p.w, "pomoai: "+msg)
	p.lastLine = time.Now()
	if p.tty && p.st.status != "" && p.stop != nil {
		p.draw()
	}
}

// set updates the live status.
func (p *progress) set(st progState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if st == p.st {
		return
	}
	p.st = st
	if p.tty {
		p.draw()
		return
	}
	fmt.Fprintln(p.w, "pomoai: "+p.line())
	p.lastLine = time.Now()
}

func (p *progress) start(since time.Time) {
	p.since, p.stop, p.done = since, make(chan struct{}), make(chan struct{})
	go func() {
		defer close(p.done)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-tick.C:
			}
			p.mu.Lock()
			switch {
			case p.st.status == "":
			case p.tty:
				p.frame++
				p.draw()
			case time.Since(p.lastLine) >= 30*time.Second:
				fmt.Fprintln(p.w, "pomoai: "+p.line())
				p.lastLine = time.Now()
			}
			p.mu.Unlock()
		}
	}()
}

func (p *progress) end() {
	if p.stop == nil {
		return
	}
	close(p.stop)
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.drawn {
		fmt.Fprintln(p.w)
		p.drawn = false
	}
	p.stop, p.st = nil, progState{}
}
