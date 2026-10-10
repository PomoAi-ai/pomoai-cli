package pomoai

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Injected by -ldflags at release time.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

type app struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	json           bool
	baseURLFlag    string
	nonInteractive bool

	prog  *progress
	hc    *http.Client
	sleep func(ctx context.Context, d time.Duration) error
	now   func() time.Time
}

func newApp(stdin io.Reader, stdout, stderr io.Writer) *app {
	return &app{
		stdin: stdin, stdout: stdout, stderr: stderr,
		hc: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
		}},
		sleep: sleepCtx,
		now:   time.Now,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Main runs the CLI and returns the process exit code.
func Main(args []string) int {
	ctx, stop := signalContext()
	defer stop()
	return newApp(os.Stdin, os.Stdout, os.Stderr).main(ctx, args)
}

func (a *app) main(ctx context.Context, args []string) int {
	a.prog = newProgress(a.stderr)
	root := a.commands()
	root.SetArgs(args)
	root.SetIn(a.stdin)
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return exitOK
	}
	var ee *ExitError
	if !errors.As(err, &ee) { // cobra parse/argument errors
		ee = newErr(exitUsage, "usage", err.Error())
	}
	if slices.Contains(args, "--json") { // flag parsing may have failed before --json was seen
		a.json = true
	}
	a.printError(ee)
	return ee.Exit
}

func (a *app) printError(e *ExitError) {
	if a.json {
		writeJSON(a.stdout, map[string]any{"ok": false, "error": e.payload()})
		return
	}
	fmt.Fprintf(a.stderr, "pomoai: error: %s: %s\n", e.Code, e.Msg)
	if e.Hint != "" {
		fmt.Fprintf(a.stderr, "pomoai: hint: %s\n", e.Hint)
	}
}

// ok prints a success result: one JSON line, or text lines in plain mode.
func (a *app) ok(m map[string]any, text ...string) {
	a.prog.end() // finish the live status line before stdout output
	if a.json {
		m["ok"] = true
		writeJSON(a.stdout, m)
		return
	}
	for _, line := range text {
		fmt.Fprintln(a.stdout, line)
	}
}

// wrap converts any non-ExitError from a command into exit 1.
func wrap(fn func(cmd *cobra.Command, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		err := fn(cmd, args)
		var ee *ExitError
		if err != nil && !errors.As(err, &ee) {
			return newErr(exitFail, "error", err.Error())
		}
		return err
	}
}

func (a *app) commands() *cobra.Command {
	root := &cobra.Command{
		Use:           "pomoai",
		Short:         "Generate images and videos through the PomoAi async task API",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageErr("%v", err) })
	pf := root.PersistentFlags()
	pf.BoolVar(&a.json, "json", false, "print exactly one JSON line on stdout")
	pf.StringVar(&a.baseURLFlag, "base-url", "", "API base URL (default "+DefaultBaseURL+"), overrides POMOAI_BASE_URL and config")
	pf.BoolVar(&a.nonInteractive, "non-interactive", false, "never prompt; fail with exit 2 instead")

	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		RunE: wrap(func(*cobra.Command, []string) error {
			a.ok(map[string]any{"version": version, "commit": commit, "date": date, "os": runtime.GOOS, "arch": runtime.GOARCH},
				fmt.Sprintf("pomoai %s (%s %s) %s/%s", version, commit, date, runtime.GOOS, runtime.GOARCH))
			return nil
		}),
	})
	root.AddCommand(&cobra.Command{
		Use:   "guide",
		Short: "Print the usage guide for AI agents",
		Args:  cobra.NoArgs,
		RunE: wrap(func(*cobra.Command, []string) error {
			g := guideText()
			a.ok(map[string]any{"guide": g}, g)
			return nil
		}),
	})
	image := &cobra.Command{Use: "image", Short: "Create images"}
	image.AddCommand(a.imageCreateCmd())
	video := &cobra.Command{Use: "video", Short: "Create videos"}
	video.AddCommand(a.videoCreateCmd())
	root.AddCommand(a.loginCmd(), a.whoamiCmd(), a.modelsCmd(), image, video, a.taskCmd())
	return root
}

// session holds what most commands need: home dir, config and the API key.
type session struct {
	home      string
	cfg       config
	key       string
	keySource string
}

func (a *app) session() (*session, error) {
	home, err := pomoaiHome()
	if err != nil {
		return nil, err
	}
	cfg, err := loadConfig(home)
	if err != nil {
		return nil, err
	}
	key, src, err := resolveKey(cfg)
	if err != nil {
		return nil, err
	}
	return &session{home: home, cfg: cfg, key: key, keySource: src}, nil
}

func (a *app) client(base, key string) *client { return newClient(base, key, a.hc) }

// apiClient resolves the base URL for a session and builds a client.
func (a *app) apiClient(s *session) (*client, error) {
	base, err := resolveBaseURL(a.baseURLFlag, s.cfg)
	if err != nil {
		return nil, err
	}
	return a.client(base, s.key), nil
}

func (a *app) readKey() (string, error) {
	if f, ok := a.stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		if a.nonInteractive {
			return "", usageErr("stdin is a terminal; pipe the key instead: printf %%s \"$KEY\" | pomoai login --non-interactive")
		}
		fmt.Fprint(a.stderr, "PomoAi API key: ")
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(a.stderr)
		return strings.TrimSpace(string(b)), err
	}
	line, err := bufio.NewReader(a.stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func (a *app) loginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Save an API key (read from the terminal or stdin) after verifying it",
		Args:  cobra.NoArgs,
		RunE: wrap(func(cmd *cobra.Command, _ []string) error {
			home, err := pomoaiHome()
			if err != nil {
				return err
			}
			cfg, err := loadConfig(home)
			if err != nil {
				return err
			}
			base, err := resolveBaseURL(a.baseURLFlag, cfg)
			if err != nil {
				return err
			}
			key, err := a.readKey()
			if err != nil {
				return err
			}
			if key == "" {
				return usageErr("empty API key")
			}
			b, err := a.client(base, key).balance(cmd.Context())
			if err != nil {
				return err // 401/403 -> exit 3, network -> exit 1; nothing saved
			}
			cfg.BaseURL, cfg.APIKey = base, key
			if err := saveConfig(home, cfg); err != nil {
				return err
			}
			path := configPath(home)
			a.ok(map[string]any{"config": path, "base_url": base, "key_fp": keyFP(key), "plan_name": b.PlanName},
				fmt.Sprintf("logged in: base_url=%s key_fp=%s plan=%s config=%s", base, keyFP(key), b.PlanName, path))
			return nil
		}),
	}
}

func (a *app) whoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show balance and the active key",
		Args:  cobra.NoArgs,
		RunE: wrap(func(cmd *cobra.Command, _ []string) error {
			s, err := a.session()
			if err != nil {
				return err
			}
			c, err := a.apiClient(s)
			if err != nil {
				return err
			}
			b, err := c.balance(cmd.Context())
			if err != nil {
				return err
			}
			unlimited := b.Remaining == 999999
			a.ok(map[string]any{"base_url": c.base, "key_source": s.keySource, "key_fp": keyFP(s.key), "plan_name": b.PlanName,
				"remaining": b.Remaining, "used": b.Used, "total": b.Total, "unit": b.Unit, "unlimited": unlimited, "valid": b.IsValid},
				"base_url="+c.base, "key_source="+s.keySource, "key_fp="+keyFP(s.key), "plan_name="+b.PlanName,
				fmt.Sprintf("remaining=%v", b.Remaining), fmt.Sprintf("used=%v", b.Used), fmt.Sprintf("total=%v", b.Total),
				"unit="+b.Unit, fmt.Sprintf("unlimited=%v", unlimited), fmt.Sprintf("valid=%v", b.IsValid))
			return nil
		}),
	}
}

func (a *app) modelsCmd() *cobra.Command {
	var kind string
	cmd := &cobra.Command{
		Use:   "models",
		Short: "List image and video models available to this key",
		Args:  cobra.NoArgs,
		RunE: wrap(func(cmd *cobra.Command, _ []string) error {
			if kind != "" && kind != "image" && kind != "video" {
				return usageErr("--kind must be image or video")
			}
			s, err := a.session()
			if err != nil {
				return err
			}
			c, err := a.apiClient(s)
			if err != nil {
				return err
			}
			list, err := c.models(cmd.Context())
			if err != nil {
				return err
			}
			out := []map[string]any{}
			var lines []string
			for _, m := range list {
				k, ep, protos := classifyModel(m)
				if k == "" || (kind != "" && k != kind) {
					continue
				}
				item := map[string]any{"id": m.ID, "kind": k}
				col := ep
				if k == "video" {
					item["protocols"] = protos
					col = strings.Join(protos, ",")
				} else {
					item["endpoint"] = ep
				}
				out = append(out, item)
				lines = append(lines, fmt.Sprintf("%s\t%s\t%s", m.ID, k, col))
			}
			a.ok(map[string]any{"models": out}, lines...)
			return nil
		}),
	}
	cmd.Flags().StringVar(&kind, "kind", "", "filter: image or video")
	return cmd
}

func (o *waitOpts) bind(cmd *cobra.Command, create bool) {
	f := cmd.Flags()
	if create {
		f.BoolVar(&o.noWait, "no-wait", false, "return right after the task is accepted")
		f.BoolVar(&o.noWait, "async", false, "alias of --no-wait")
	}
	f.DurationVar(&o.timeout, "timeout", 0, "stop waiting locally after this long (exit 4); 0 = wait for the result")
	f.BoolVar(&o.noDownload, "no-download", false, "print result URLs instead of downloading")
	f.StringVar(&o.out, "out", "", "output file (multiple outputs get -1, -2 … before the extension)")
	f.StringVar(&o.outDir, "out-dir", "", "output directory (default: current directory)")
}

func (o *waitOpts) check() error {
	if o.out != "" && o.outDir != "" {
		return usageErr("use either --out or --out-dir, not both")
	}
	if o.timeout < 0 {
		return usageErr("--timeout must not be negative")
	}
	return nil
}

func (a *app) readPrompt(prompt, file string) (string, error) {
	if (prompt == "") == (file == "") {
		return "", usageErr("exactly one of --prompt or --prompt-file is required")
	}
	if file != "" {
		var data []byte
		var err error
		if file == "-" {
			data, err = io.ReadAll(a.stdin)
		} else {
			data, err = os.ReadFile(file)
		}
		if err != nil {
			return "", usageErr("--prompt-file: %v", err)
		}
		prompt = string(data)
	}
	if prompt = strings.TrimSpace(prompt); prompt == "" {
		return "", usageErr("prompt is empty")
	}
	return prompt, nil
}

func (a *app) imageCreateCmd() *cobra.Command {
	var o imageOpts
	var promptFile string
	var w waitOpts
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Generate or edit images (blocks until done unless --no-wait)",
		Args:  cobra.NoArgs,
		RunE: wrap(func(cmd *cobra.Command, _ []string) error {
			if o.model == "" {
				return usageErr("--model is required")
			}
			var err error
			if o.prompt, err = a.readPrompt(o.prompt, promptFile); err != nil {
				return err
			}
			if err := w.check(); err != nil {
				return err
			}
			path, ct, body, err := buildImageRequest(o)
			if err != nil {
				return err
			}
			s, err := a.session()
			if err != nil {
				return err
			}
			c, err := a.apiClient(s)
			if err != nil {
				return err
			}
			t, err := a.newTask(s, c, "image", o.model, o.prompt, path, ct, body, "")
			if err != nil {
				return err
			}
			return t.drive(cmd.Context(), w)
		}),
	}
	f := cmd.Flags()
	f.StringVar(&o.model, "model", "", "model id (see `pomoai models --kind image`)")
	f.StringVar(&o.prompt, "prompt", "", "prompt text")
	f.StringVar(&promptFile, "prompt-file", "", "read the prompt from a file, or - for stdin")
	f.StringArrayVar(&o.images, "image", nil, "reference image file (repeatable, max 4, each <= 50 MiB)")
	f.StringVar(&o.mask, "mask", "", "mask image for edits")
	f.StringVar(&o.size, "size", "", "image size, e.g. 1024x1024")
	f.IntVar(&o.n, "n", 0, "number of images")
	f.StringVar(&o.quality, "quality", "", "quality, e.g. low|medium|high|auto")
	f.StringVar(&o.aspectRatio, "aspect-ratio", "", "Gemini only: aspect ratio, e.g. 16:9")
	f.StringVar(&o.imageSize, "image-size", "", "Gemini only: image size, e.g. 2K")
	f.StringArrayVar(&o.params, "param", nil, "extra top-level request field key=value (repeatable)")
	w.bind(cmd, true)
	return cmd
}

func (a *app) taskCmd() *cobra.Command {
	root := &cobra.Command{Use: "task", Short: "Inspect, resume and download tasks"}
	open := func(id string) (*task, error) {
		s, err := a.session()
		if err != nil {
			return nil, err
		}
		t, err := a.openTask(s, id)
		if err == nil {
			t.start = a.now()
		}
		return t, err
	}

	get := &cobra.Command{
		Use:   "get <tsk_id|local_id>",
		Short: "Query a task once (exit code reflects the query, not the task status)",
		Args:  cobra.ExactArgs(1),
		RunE: wrap(func(cmd *cobra.Command, args []string) error {
			t, err := open(args[0])
			if err != nil {
				return err
			}
			r := t.rec
			out := map[string]any{"task_id": r.TaskID, "local_id": r.LocalID, "kind": r.Kind}
			if r.TaskID == "" { // not accepted yet: report the local state only
				out["status"], out["error"] = r.Status, r.Error
				a.ok(out, r.LocalID+"\t"+r.Status)
				return nil
			}
			env, err := t.pollTask(cmd.Context(), nil, true)
			if err != nil {
				return err
			}
			status, native, progress := env.Status, "", 0.0
			switch {
			case env.Status == stFailed:
				r.Status = stFailed
				r.Error = &recordError{Code: env.errCode(), Message: describeFailure(env)}
			case env.Status == stSucceeded && r.Kind == "video":
				v, err := t.pollNative(cmd.Context(), env, true)
				if err != nil {
					return err
				}
				native, progress = v.status, v.progress
				r.NativeStatus, r.Progress = v.status, v.progress
				switch {
				case v.status == "completed" && v.url != "":
					status = stSucceeded
				case v.status == "completed" && v.err == "":
					return t.videoUncertain(v) // completed without a usable URL: exit 5
				case v.status == "completed", v.status == "failed", v.status == "cancelled":
					status = stFailed
					r.Error = &recordError{Code: "video_" + strings.Replace(v.status, "completed", "failed", 1), Message: v.err}
				case v.status == "unknown":
					status = stUnknown
				default:
					status = stRunning
				}
				r.Status = status
			case env.Status == stSucceeded:
				r.Status = stSucceeded
			}
			if err := t.save(); err != nil {
				return err
			}
			out["kind"], out["status"], out["native_status"], out["progress"], out["error"] = r.Kind, status, native, progress, nil
			if status == stFailed {
				out["error"] = r.Error
			}
			out["expires_at"] = nil
			if !r.ExpiresAt.IsZero() {
				out["expires_at"] = r.ExpiresAt.Format(time.RFC3339)
			}
			a.ok(out, r.TaskID+"\t"+status)
			return nil
		}),
	}

	var w waitOpts
	wait := &cobra.Command{
		Use:   "wait <tsk_id|local_id>",
		Short: "Resume a task: replay an unconfirmed submit, poll, download",
		Args:  cobra.ExactArgs(1),
		RunE: wrap(func(cmd *cobra.Command, args []string) error {
			if err := w.check(); err != nil {
				return err
			}
			t, err := open(args[0])
			if err != nil {
				return err
			}
			return t.drive(cmd.Context(), w)
		}),
	}
	w.bind(wait, false)

	var limit int
	list := &cobra.Command{
		Use:   "list",
		Short: "List local task records, newest first",
		Args:  cobra.NoArgs,
		RunE: wrap(func(*cobra.Command, []string) error {
			home, err := pomoaiHome()
			if err != nil {
				return err
			}
			st := newStore(home)
			recs, err := st.list()
			if err != nil {
				return err
			}
			tasks := []map[string]any{}
			var lines []string
			for i, r := range recs {
				if a.now().Sub(r.CreatedAt) >= retention { // past the replay window: the body is useless
					st.removeBody(r.LocalID)
				}
				if limit > 0 && i >= limit {
					continue
				}
				tasks = append(tasks, map[string]any{"local_id": r.LocalID, "task_id": r.TaskID, "kind": r.Kind, "model": r.Model,
					"status": r.Status, "created_at": r.CreatedAt.Format(time.RFC3339), "files": r.Files})
				lines = append(lines, strings.Join([]string{r.LocalID, r.TaskID, r.Kind, r.Status, r.CreatedAt.Local().Format(time.DateTime), r.Model}, "\t"))
			}
			a.ok(map[string]any{"tasks": tasks}, lines...)
			return nil
		}),
	}
	list.Flags().IntVar(&limit, "limit", 20, "maximum number of records (0 = all)")

	var dw waitOpts
	download := &cobra.Command{
		Use:   "download <tsk_id|local_id>",
		Short: "Download the result of a succeeded task again",
		Args:  cobra.ExactArgs(1),
		RunE: wrap(func(cmd *cobra.Command, args []string) error {
			if err := dw.check(); err != nil {
				return err
			}
			t, err := open(args[0])
			if err != nil {
				return err
			}
			if t.rec.TaskID == "" {
				return t.fail(newErr(exitFail, "task_not_succeeded", "the task was not accepted (status "+t.rec.Status+")"))
			}
			env, err := t.pollTask(cmd.Context(), nil, true)
			if err != nil {
				return err
			}
			if env.Status != stSucceeded {
				return t.fail(newErr(exitFail, "task_not_succeeded", "task status is "+env.Status))
			}
			srcs, refresh, err := t.results(cmd.Context(), env, true)
			if err != nil {
				return err
			}
			return t.finish(cmd.Context(), srcs, refresh, dw)
		}),
	}
	download.Flags().StringVar(&dw.out, "out", "", "output file")
	download.Flags().StringVar(&dw.outDir, "out-dir", "", "output directory")

	root.AddCommand(get, wait, list, download)
	return root
}

// inferProtocol picks the model's single video protocol from /v1/models.
func inferProtocol(ctx context.Context, c *client, model string) (*videoProtocol, error) {
	list, err := c.models(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range list {
		if m.ID != model {
			continue
		}
		_, _, protos := classifyModel(m)
		switch len(protos) {
		case 0:
		case 1:
			return protocolByID(protos[0]), nil
		default:
			return nil, &ExitError{Exit: exitUsage, Code: "protocol_ambiguous", Extra: map[string]any{"candidates": protos},
				Msg: "model " + model + " supports several video protocols; pick one with --protocol: " + strings.Join(protos, ", ")}
		}
	}
	return nil, &ExitError{Exit: exitUsage, Code: "model_not_found", Msg: "no video model " + model + " for this key",
		Hint: "see `pomoai models --kind video`"}
}

func (a *app) videoCreateCmd() *cobra.Command {
	var o videoOpts
	var promptFile string
	var w waitOpts
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a video (blocks until done unless --no-wait)",
		Args:  cobra.NoArgs,
		RunE: wrap(func(cmd *cobra.Command, _ []string) error {
			if o.model == "" {
				return usageErr("--model is required")
			}
			var err error
			if o.prompt, err = a.readPrompt(o.prompt, promptFile); err != nil {
				return err
			}
			if err := w.check(); err != nil {
				return err
			}
			var p *videoProtocol
			if o.protocol != "" {
				if p = protocolByID(o.protocol); p == nil {
					return usageErr("unknown --protocol %q (ark_v3|las_v1|newapi_video|openai_video|minimax_v2)", o.protocol)
				}
			}
			s, err := a.session()
			if err != nil {
				return err
			}
			c, err := a.apiClient(s)
			if err != nil {
				return err
			}
			if p == nil {
				if p, err = inferProtocol(cmd.Context(), c, o.model); err != nil {
					return err
				}
			}
			body, err := buildVideoPayload(p, o)
			if err != nil {
				return err
			}
			t, err := a.newTask(s, c, "video", o.model, o.prompt, p.CreatePath, "application/json", body, p.ID)
			if err != nil {
				return err
			}
			return t.drive(cmd.Context(), w)
		}),
	}
	f := cmd.Flags()
	f.StringVar(&o.model, "model", "", "model id (see `pomoai models --kind video`)")
	f.StringVar(&o.prompt, "prompt", "", "prompt text")
	f.StringVar(&promptFile, "prompt-file", "", "read the prompt from a file, or - for stdin")
	f.StringVar(&o.protocol, "protocol", "", "ark_v3|las_v1|newapi_video|openai_video|minimax_v2 (default: from /v1/models)")
	f.IntVar(&o.duration, "duration", 0, "seconds (default: 5; openai_video 4; minimax_v2 6)")
	f.StringVar(&o.resolution, "resolution", "", "e.g. 720p (minimax_v2: 768P)")
	f.StringVar(&o.ratio, "ratio", "", "16:9|9:16|1:1|4:3|3:4|21:9|adaptive (default 16:9)")
	f.StringVar(&o.size, "size", "", "openai_video only, e.g. 1280x720")
	f.StringVar(&o.image, "image", "", "reference image: http(s) URL or asset://ID")
	f.StringArrayVar(&o.params, "param", nil, "extra top-level request field key=value (repeatable)")
	w.bind(cmd, true)
	return cmd
}
