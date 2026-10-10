---
name: pomoai
description: Generate or edit images and create videos through the PomoAi API using the `pomoai` CLI; list PomoAi image/video models, check balance, or resume a PomoAi task (tsk_… / l_…).
---

# pomoai CLI

`pomoai` submits image and video jobs to the PomoAi async task API (`/v1/tasks`),
waits for them, and saves the results as local files. Progress goes to stderr;
the result goes to stdout.

## 1. Check the installation

```sh
pomoai version --json
```

If the command is missing, install it with one of:

```sh
brew install PomoAi-ai/tap/pomoai
npm i -g pomoai-cli
curl -fsSL https://raw.githubusercontent.com/PomoAi-ai/pomoai-cli/main/install.sh | sh
```

## 2. Credentials

- The API key comes only from the `POMOAI_API_KEY` environment variable or from
  `pomoai login`, which **the user runs themselves**.
- **Never ask the user for their key, never print a key, never read
  `~/.pomoai/config.json`.**
- The base URL defaults to `https://www.pomoai.ai`. Only override it
  (`--base-url`, `POMOAI_BASE_URL`) when the user tells you to.
- On exit code 3, tell the user to set `POMOAI_API_KEY` or run `pomoai login`.
- Tasks can only be queried with the exact key that submitted them. If the key
  changed, `key_mismatch` (exit 3) is expected.

`pomoai whoami --json` shows the balance and which key source is active.

## 3. Pick a model

```sh
pomoai models --json                 # all image and video models for this key
pomoai models --kind video --json
```

Image models report `endpoint` (`images` or `gemini`); video models report their
`protocols`. If `video create` fails with `protocol_ambiguous`, pick one of the
`candidates` and rerun with `--protocol <id>`.

## 4. Run one blocking command

Use a single command and let it wait. **Do not write your own polling loop.**
Pass `--timeout` 30–60 seconds shorter than your own tool timeout.

```sh
pomoai image create --model gpt-image-1 --prompt "a red fox in snow" \
  --size 1024x1024 --json --timeout 9m --out-dir ./out

pomoai image create --model gpt-image-1 --prompt "make it night" \
  --image ./photo.png --json --timeout 9m          # edit with up to 4 local images

pomoai video create --model doubao-seedance-2-0-260128 --prompt "sunrise over the sea" \
  --duration 5 --ratio 16:9 --json --timeout 9m
```

Useful flags:
- `--prompt-file <path|->` instead of `--prompt`.
- `--param key=value` (repeatable) passes extra top-level request fields.
- `--no-wait` returns right after the task is accepted.
- `--no-download` returns result URLs instead of files.
- `--out <file>` or `--out-dir <dir>` (default: current directory).

Image references (`--image`) are local files, at most 4, each ≤ 50 MiB.
Video references (`--image`) must be `http(s)://` URLs or `asset://ID`; local
files are rejected with `video_reference_must_be_url`.

## 5. Read the result

Parse only the single JSON line on stdout:

```json
{"ok":true,"kind":"image","task_id":"tsk_…","local_id":"l_…","status":"succeeded",
 "files":[{"path":"/abs/pomoai-3f2a9c1e-1.png","bytes":1234567,"content_type":"image/png"}]}
```

Report `files[].path` to the user. **Do not read image or video files back into
your context.**

On failure stdout is `{"ok":false,"error":{"code","message","hint",...}}`.

## 6. Exit codes and what to do next

| Code | Meaning | Next step |
|---|---|---|
| 0 | Success | Report the files |
| 1 | Task failed or was definitely not accepted | Read `error.message`; fix the prompt/parameters or report it |
| 2 | Usage or parameter error | Fix the arguments (see `error.message` / `hint`) |
| 3 | Authentication (no key, 401/403, `key_mismatch`) | Ask the user to configure the key themselves |
| 4 | Local `--timeout` reached, task still running | Resume (see below) with `--json --timeout …` |
| 5 | Outcome unknown | **Do not create again** (it may bill twice). Resume (see below) at most once, then report to the user |
| 130/143 | Interrupted | Resume (see below) |

To resume, run the command in `error.hint` (for exit 4/5/130/143 it is exactly a
`pomoai task wait <id>` command; add `--json --timeout …`). If there is no hint, run `pomoai task wait <id> --json` with `<id>` = `error.task_id`, or
`error.local_id` when `task_id` is absent (the task was not accepted yet).

## 7. Resume after your command was killed

If your host killed the command, find the last `resume:` line on stderr
(`pomoai: accepted task=tsk_… resume: pomoai task wait tsk_…`; before acceptance
it is `resume: pomoai task wait l_…`), or list local records and use `task_id`,
or `local_id` when `task_id` is empty:

```sh
pomoai task list --json
pomoai task wait <tsk_…|l_…> --json --timeout 9m
```

`task wait` never creates a second task: before acceptance it replays the same
idempotency key, after acceptance it only polls.

Other task commands:
- `pomoai task get <id> --json` — one status check, no download.
- `pomoai task download <id> --json` — download the result of a succeeded task again.

Results are kept on the server for 48 hours.
