# PLAN -- pomoai CLI + Agent Skill 第一版

## Status: implementing
## Task: 001
## Related: pomoai-code task/1051（异步任务接口 `/v1/tasks`、「我的异步任务」）
## Baseline Commit: N/A（新仓库）

## Goal
为 PomoAi 提供给人和 AI agent（Codex、Claude Code、Cursor 等）使用的命令行工具 `pomoai` 与配套 Agent Skill：
- 图片、视频生成全部经 `/v1/tasks` 异步任务接口提交；默认阻塞等待、进度写 stderr、结果写 stdout，可下载到本地。
- `--json` 机器可读输出、稳定退出码、幂等续跑（本地超时/中断不取消服务端任务，可 `task wait <id>` 续等）。
- 单独仓库 `../pomoai-cli`（GitHub `PomoAi-ai/pomoai-cli`），Go 编写。

## Scope（第一版）
- 命令：`login` / `whoami`、`models`、`image create`、`video create`、`task get` / `wait` / `list` / `download`、`version`。
- 发布：GitHub Release + 安装脚本、npm（`pomoai-cli`，分平台二进制子包）、Homebrew tap（`PomoAi-ai/homebrew-tap`）、skills（`npx skills add PomoAi-ai/pomoai-cli`，附 `.claude-plugin` / `.cursor-plugin` 清单）。

## Non-goals
- 对话类接口（chat）不在第一版。
- 不做 MCP server（后续可复用同一份逻辑再包一层）。

## Decisions
- 用户决策（2026-10-09）：命名 `pomoai-cli` / 命令 `pomoai`；范围＝图片+视频+任务管理；语言 Go；发布渠道＝GitHub Release+安装脚本、npm、Homebrew tap、skills 全部。
- 参考：MiniMax `mmx`（阻塞等待+`--async`、SKILL、npm）、Runway skills（stderr 进度/stdout 结果、Key 只读环境变量、本地超时不取消任务、AI 友好文档）、LinkModel（退出码分档）。

- 探索完成（1 路 dev:explorer，结论存 `explore.md`）：`/v1/tasks` 只在 edge 主口；令牌侧无 tsk_ 列表 → `task list` 只列本地记录；仓库无正式公开域名 → 默认 base URL 待用户提供；企业口密钥第一版不支持；视频原生查询仅 openai/newapi 有进度百分比。
- 顺带修复 pomoai-code `docs/api/tasks-v1.openapi.yaml` 4 处 24h 残留 → 48h（属 task/1051 的 48h 口径）。
- 设计完成（dev:architect，存 `design.md`）。开放问题按推荐默认执行、待用户确认：Q1 `DefaultBaseURL` 暂空（缺失 exit 2）；Q2 新增退出码 5「结果不明，禁止重新 create」；Q3 默认不限时；Q4 视频 `status:"unknown"` 有界复查 10 分钟后 exit 5；Q5 拒绝非 loopback `http://`；Q6 本地目录 `~/.pomoai`（`POMOAI_HOME` 覆盖）；Q7 `task get` 退出码只反映查询成败；Q8 多协议 exit 2 列 candidates；Q9 下载前重查原生接口，不依赖 `/content`；Q11 许可证待定（LICENSE 留到发布步骤）；Q12–Q17 按 design 默认。
- 实现分两段：步骤 1–6（代码与测试）先做；步骤 7（发布流水线）等用户提供域名、许可证、GitHub/npm 账号后做。
- 步骤 1–6 实现偏差（2026-10-09，dev-engineer）：
  - 移植以参考实现为准：Gemini 判定按 `isGeminiChatImageModel` 用「小写全名包含 gemini」，不做 design 的「去 `/` 前缀 + 前缀匹配」；只认 `gemini` 子串，不搬整张 gemini 族表（族表含 `imagen-4`，会把 `google/imagen-4…` 误判为 chat 出图）。
  - `models` 分类先判 Gemini 名再看 `image-generation`，与 `image create` 的端点选择一致（design 顺序相反）。
  - `b64_json` 结果用 `http.DetectContentType` 定类型（参考实现写死 image/png，webp/jpeg 输出会拿错扩展名）。
  - Authorization 只在 scheme 与 host 都等于 base URL 时附带（design 只比 host），防同 host 降级到 http。
  - 提交中途被取消（status 0）时置 `sent_before=true`（照 design §3.6；参考实现 stopped 时不改 ambiguous）。
  - 视频原生 `completed` 但无地址且无错误时继续轮询（服务端转存未完成），不当成功也不当失败。
  - 新增 CLI 自有码：`video_failed`/`video_cancelled`、`native_error`（原生 200 解析失败）、`task_not_succeeded`、`result_empty`、`unsupported_task`、`invalid_response`、`http_error`（无服务端 code）、`submit_failed`。
  - 额外参数冲突 exit 2：Gemini 拒 `--size/--n/--quality`，非 Gemini 拒 `--aspect-ratio/--image-size`；视频 openai 拒 `--resolution`、非 openai 拒 `--size`。
  - 查询 200 非信封按 502 处理（5s 后重查）；无本地记录的 `tsk_` 在首次查到信封后才落盘，首查 404 直接 exit 5（没有本地 created_at，不进 3 分钟竞态窗）。
  - multipart 分片 Content-Type 按扩展名/嗅探填写（与浏览器 File.type 同义），普通字段按键名排序；body 落盘后逐字节重放。
  - TTY 判定用 `term.IsTerminal`（Windows 也成立），不用 `ModeCharDevice`。CLI 文案统一英文。
  - `golang.org/x/term` 固定 v0.45.0（v0.46.0 要求 go1.26）；go.mod `go 1.25.0`。
  - SKILL.md 安装段暂只写 `go install`，brew/npm/install.sh 在步骤 7 发布后补。
- 审查修复（2026-10-09，dev-engineer）：
  - 续跑提交一律视为结果不明：非本进程新建的记录 `sentBefore=true`，进入提交循环前先落盘 `status=submitting`；换键回调保持 submitting、只重置 `sent_before`。SIGKILL 后不会再出现「unknown + sent_before=false」被判 failed 删 body 的二次计费路径（代价：续跑时确定性拒绝也报 exit 5 而非 exit 1）。
  - 受理后先落盘再删 body，并先在 stderr 打 `accepted task=…`；受理后任何记录落盘失败统一 exit 5 `record_save_failed`，带 task_id 与续跑 hint。
  - 每个 client 自带 `CheckRedirect`：跳转目标 scheme 或 host（含端口）不等于 base URL 时删除 Authorization（Go 默认只比 hostname）。
  - 原生 `completed` 但无可用地址（含 `safeURL` 拒绝）与 `unknown` 同口径：等待路径有界复查 10 分钟，之后 exit 5 `video_result_unavailable`；`task get`/`task download` 一次查询直接 exit 5。
  - exit 4/5/130/143 的 `error.hint` 统一为纯 `pomoai task wait <id>` 命令（说明文字移到 message）；SKILL 续跑说明改为「用 error.hint；没有时用 error.task_id，再没有用 error.local_id」。
  - 可选项：`c.do` 读响应加 256 MiB 上限。
  - `run.go` 超 800 行，`extFor`/`writePart` 移到 `store.go`（本地文件辅助函数）。
- 用户决策（2026-10-09，步骤 7 前）：
  - 默认 base URL = `https://www.pomoai.ai`（推翻 Q1 的空值）。覆盖顺序不变：`--base-url` > `POMOAI_BASE_URL` > `pomoai login` 保存值 > 默认；本地测试用 `POMOAI_BASE_URL=http://localhost:PORT`（http 仍只允许 localhost/loopback）。默认非空后 `base_url_missing` 不可达，删除该分支与错误码，`resolveBaseURL` 末尾直接 `normalizeBaseURL(DefaultBaseURL)`；原测试改为只测 `api_key_missing`。
  - 许可证 MIT，`Copyright (c) 2026 PomoAi`（Q11 定案）；npm 与 plugin 清单 `license: "MIT"`。
  - GitHub 仓 `PomoAi-ai/pomoai-cli`、`PomoAi-ai/homebrew-tap` 已建（公开）。npm 改用无 scope 包名（`@pomoai` org 不确定能否注册）：主包 `pomoai-cli`（bin `pomoai`），平台子包 `pomoai-cli-{darwin,linux,win32}-{x64,arm64}`；2026-10-10 registry 查询 7 个名字均未占用。
  - homebrew-tap 推送改走 SSH deploy key（2026-10-10）：org 原来 `deploy_keys_enabled_for_repositories: false`，gh 开成 true 后建了只授权 homebrew-tap 的 read-write deploy key，私钥存 `HOMEBREW_TAP_KEY` secret；`.goreleaser.yaml` 用 `repository.git.{url,private_key}` 替代 `token`。npm `NPM_TOKEN` 未配时 release workflow 的 npm 步骤跳过（job env `HAS_NPM_TOKEN` 判断）。
- 步骤 7 实现取舍（2026-10-09，dev-engineer）：
  - cask 不写 `license`（GoReleaser homebrew_casks 无此字段）；不配 changelog，用默认。
  - govulncheck 用 `go run golang.org/x/vuln/cmd/govulncheck@latest`，不单独装 action。
  - `npm-publish.mjs` 稳定版显式 `--tag latest`、预发布 `--tag next`；tag 不符合 `vX.Y.Z[-pre]` 或 Binary 条目不是 6 个直接失败。
  - install.sh 装完执行一次 `pomoai version`，二进制跑不起来（架构错等）时安装以非零退出。
  - Cursor 清单字段按 cursor.com/docs/reference/plugins 核对（`skills: "./skills/"`）。

## Implementation Map
| # | Step | Done |
|---|------|------|
| 1 | 骨架 + version/guide + 退出码 | ✅ |
| 2 | config/client + login/whoami/models | ✅ |
| 3 | store/submit（重试、重放、换键） | ✅ |
| 4 | 图片全链路 + task get/wait/list/download | ✅ |
| 5 | 视频 5 协议 + 原生阶段 | ✅ |
| 6 | 进度/信号/心跳体验收口 | ✅ |
| 7 | 发布：goreleaser/release.yml/install.sh/npm/plugin | ✅（代码部分；CI 上 `v0.1.0-rc.1` 全流水线验证待做） |

## Validation
| Command | Required | Done |
|---------|----------|------|
| `gofmt -l .`（每步后） | 输出为空 | ✅ 空 |
| `go vet ./...`（每步后） | 通过 | ✅ |
| `go build ./...`（每步后） | 通过 | ✅ |
| `go test -count=1 ./...`（每步后，最终 17 个测试） | 全部 PASS | ✅ ok 0.17s |
| `go test -race -count=1 ./...` | 无数据竞争 | ✅ ok 1.2s |
| 手工：python 假服务器 + 编译产物，`image create ... 2>&1 \| cat`（非 TTY） | 状态变化逐行、stdout 一行 JSON | ✅ |
| 手工：`script -q /dev/null pomoai image create ...`（TTY） | 单行 spinner 刷新、结束换行后再输出路径 | ✅ |
| 手工：运行中 `kill -INT` → `task wait <l_id>` | exit 130、记录落盘、续跑成功下载 | ✅ |
| 审查修复后 `gofmt -l .` / `go vet ./...` / `go test -race -count=1 ./...`（20 个测试） | 空 / 通过 / PASS | ✅ |
| 反证：分别回退修复 1、3、4 后跑对应新测试 | 新测试失败 | ✅ 三条均 FAIL（exit 1 not billed / Authorization leaked / 500 次 sleep 仍在轮询），恢复后全绿 |
| 手工：`login`（坏 key / 好 key）、`whoami`、`--base-url http://example.com` | exit 3 不落盘 / config 0600 目录 0700 / exit 2 base_url_insecure | ✅ |
| 步骤 7：`gofmt -l .` / `go vet ./...` / `go test -race -count=1 ./...` | 空 / 通过 / PASS | ✅ ok 1.3s |
| 步骤 7：`sh -n install.sh`、`node --check` 两个 js/mjs、三份 YAML `yaml.safe_load`、四份 JSON `json.tool` | 均通过 | ✅ |
| 步骤 7：scratch 副本 + 假 `artifacts.json` + 桩 `npm` 跑 `npm-publish.mjs`（rc tag / 非 tag） | 6 子包先发、主包最后、`--tag next`、optionalDependencies 钉精确版本、exe 0755；非 tag 直接报错 | ✅ |
| 步骤 7：`go build -ldflags "-X …version/commit/date"` 到 scratch 后 `pomoai version --json` | 注入值生效 | ✅ |
| `goreleaser check` | 通过 | 未跑（本机未装 goreleaser，由 ci.yml 执行） |
| CI：推 `v0.1.0-rc.1` tag | Release 产物 7 个 + checksums；tap 不更新；npm 7 包 `next` | 待用户推 tag |
