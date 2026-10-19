# pomoai-cli

[中文说明](README.zh-CN.md)

`pomoai` is a command-line tool for people and AI agents (Claude Code, Codex,
Cursor, …) to generate images and videos through the PomoAi async task API
(`/v1/tasks`). It submits a job, waits for it, and saves the result locally.
Progress goes to stderr, the result to stdout, and `--json` prints exactly one
JSON line.

## Install

```sh
npm i -g https://github.com/PomoAi-ai/pomoai-cli/releases/latest/download/pomoai-cli.tgz   # any OS incl. Windows, Node 18+
brew install PomoAi-ai/tap/pomoai                 # macOS / Linux (Homebrew)
curl -fsSL https://raw.githubusercontent.com/PomoAi-ai/pomoai-cli/main/install.sh | sh
go install github.com/PomoAi-ai/pomoai-cli/cmd/pomoai@latest   # from source, Go 1.25+
```

`install.sh` installs to `~/.local/bin` (override with `POMOAI_INSTALL_DIR`),
verifies the SHA-256 checksum, and takes `POMOAI_VERSION=0.1.0` to pin a
version. The npm command installs a tarball from the GitHub Release (all
platforms bundled, no npm registry account involved). Without Node on
Windows, download the zip from
[Releases](https://github.com/PomoAi-ai/pomoai-cli/releases) and put
`pomoai.exe` on your `PATH`.

Agent skill only (Claude Code, Codex, Cursor, …):

```sh
npx skills add PomoAi-ai/pomoai-cli
```

In Claude Code you can also install it as a plugin:
`/plugin marketplace add PomoAi-ai/pomoai-cli`, then `/plugin install pomoai@pomoai`.

## Quick start

1. Create an API key at <https://www.pomoai.ai/console/token>.
2. Log in once (the key is verified, then saved to `~/.pomoai/config.json`), or
   export `POMOAI_API_KEY` instead:

   ```sh
   pomoai login
   pomoai whoami                       # balance + which key is active
   ```

3. Pick a model and create:

   ```sh
   pomoai models --kind image          # copy a model id from the list
   pomoai image create --model <id> --prompt "a red fox in snow" --out-dir ./out

   pomoai models --kind video
   pomoai video create --model <id> --prompt "sunrise over the sea" --duration 5
   ```

Every command has examples: `pomoai <command> --help`, e.g.
`pomoai image create --help`.

## Commands

| Command | Purpose |
|---|---|
| `pomoai login` | Read an API key (hidden prompt, or first line of stdin), verify it, save it |
| `pomoai whoami` | Balance, plan name, and which key source is active |
| `pomoai models [--kind image\|video]` | Image models (`images` / `gemini` endpoint) and video models (protocols) |
| `pomoai image create` | Text-to-image, edits with up to 4 local `--image` files, Gemini image models |
| `pomoai video create` | Video via one of `ark_v3`, `las_v1`, `newapi_video`, `openai_video`, `minimax_v2` (inferred from `/v1/models` when `--protocol` is omitted) |
| `pomoai task get <id>` | One status check (exit code reflects the query, not the task) |
| `pomoai task wait <id>` | Resume: replay an unconfirmed submit with the same idempotency key, poll, download |
| `pomoai task list` | Local task records, newest first |
| `pomoai task download <id>` | Download the result of a succeeded task again |
| `pomoai version` / `pomoai guide` | Version info / the AI-agent usage guide |

`<id>` is a server task id (`tsk_…`) or a local record id (`l_…`).

Common flags for `create` and `task wait`: `--json`, `--timeout 9m` (stop
waiting locally, exit 4; the task keeps running), `--no-wait`, `--no-download`,
`--out <file>`, `--out-dir <dir>`, `--param key=value` (extra request fields).

Local `--timeout` and Ctrl-C never cancel the server task; resume it with
`pomoai task wait <id>`.

## Environment

| Variable | Meaning |
|---|---|
| `POMOAI_API_KEY` | API key; takes precedence over the key saved by `pomoai login` |
| `POMOAI_BASE_URL` | API base URL, default `https://www.pomoai.ai`. Order: `--base-url` > `POMOAI_BASE_URL` > the URL saved by `pomoai login` > default. A trailing `/v1` is accepted. Plain `http://` is only allowed for localhost / loopback |
| `POMOAI_HOME` | Local data directory (default `~/.pomoai`) |

Local testing against a gateway on your machine:

```sh
POMOAI_BASE_URL=http://localhost:3000 pomoai whoami
```

A task can only be queried with the exact key that submitted it. If the key is
changed or reset while a task runs, `task wait` fails with `key_mismatch`.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | Task failed, or definitely not accepted, or network/server errors after retries |
| 2 | Usage or parameter error (including an invalid or insecure base URL) |
| 3 | Authentication: no key, 401/403, `key_mismatch` |
| 4 | Local `--timeout` reached; the task is still running — `pomoai task wait <id>` |
| 5 | Outcome unknown — do **not** create again (it may bill twice); `task wait` at most once, check the usage log |
| 130 / 143 | Interrupted by SIGINT / SIGTERM; the record is saved — `pomoai task wait <id>` |

On failure, `--json` prints `{"ok":false,"error":{"code","message","hint",…}}`.

## Local directory

```text
~/.pomoai/                  (or $POMOAI_HOME; mode 0700)
├── config.json             base URL and API key saved by `pomoai login` (0600)
└── tasks/
    ├── l_<id>.json         task record: ids, status, key fingerprint, files (0600)
    └── l_<id>.body         exact request bytes, kept only until the task is accepted (0600)
```

Records never contain the API key or signed result URLs. Results are kept on
the server for 48 hours. Deleting `config.json` logs you out.

## AI agents

`skills/pomoai/SKILL.md` is an Agent Skill describing how an agent should use
the CLI; `pomoai guide` prints the same text.

## License

MIT, see [LICENSE](LICENSE).
