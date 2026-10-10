# pomoai CLI 第一版实现蓝图（task/001-cli-v1）

参考实现（要移植到 Go 的部分）的绝对路径和行号：
- `/Volumes/codes/pomoai-pj/pomoai-code-next/frontend/web/src/helpers/asyncTasks.js`：重试间隔表在 40-46；信封解析在 84-104；两种错误形状解析在 108-125；提交失败分类在 140-169；提交、重放、换键循环在 188-290；查询失败分类在 297-325；失败文案在 329-365。
- `/Volumes/codes/pomoai-pj/pomoai-code-next/frontend/web/src/helpers/videoProtocols.js`：协议表在 37-77；`safeVideoResultUrl` 在 94-109；参考图校验在 111-118；请求体构造在 120-178；查询路径在 180-185；响应解析在 187-279；「不落盘签名 URL」白名单在 341-367。
- `/Volumes/codes/pomoai-pj/pomoai-code-next/frontend/web/src/pages/CreateCenter/imageTasks.js`：上限在 42-45；端点选择在 54；编码（含 Gemini inlineData 和 multipart `image[]`）在 100；Gemini 结果提取在 133-160；合并提取在 161。
- `/Volumes/codes/pomoai-pj/pomoai-code-next/frontend/web/src/pages/DrawingPlayground/paramProfiles.js:119`：`isGeminiChatImageModel`。
- `/Volumes/codes/pomoai-pj/pomoai-code-next/frontend/web/src/pages/DrawingPlayground/utils.js:1200`：`normalizeImages`（取 `data[].url`，兜底 `b64_json`）。
- `/Volumes/codes/pomoai-pj/pomoai-code-next/backend/gateway/router/edge-gateway-router.go:219-221`：`/v1/videos/:task_id/content` 只挂在 `/v1/videos` 这一组上。

---

## 0. 总体取舍

**命令解析用 cobra，不用 stdlib flag。**
- stdlib `flag` 在第一个非 flag 参数处停止解析，`pomoai task wait tsk_x --json` 里的 `--json` 会被当成位置参数。agent 经常把 flag 写在位置参数后面，这一点是硬伤。
- stdlib 方案还要手写子命令分发和帮助文本，大约 100 行。cobra 只增加 cobra 和 pflag 两个依赖（Windows 上还有 mousetrap），换来参数可以穿插、自动生成帮助、`SetFlagErrorFunc` 可统一映射成退出码 2。

**全部依赖只有两个：** `github.com/spf13/cobra`，以及用于 `login` 隐藏输入 Key 的 `golang.org/x/term`。
- HTTP、JSON、multipart、embed、TTY 检测都用 stdlib。TTY 检测用 `os.Stderr.Stat()` 加 `ModeCharDevice` 判断。
- UUID 用 `crypto/rand` 自己格式化，大约 5 行，不引 google/uuid。
- 不建接口。需要在测试里替换的只有 `send`、`sleep`、`now` 三个函数字段。

**包结构：** 根目录放一个包 `pomoai`，承载全部逻辑和 `//go:embed skills/pomoai/SKILL.md`（embed 不能引用 `..`，所以逻辑必须在根包）。`cmd/pomoai/main.go` 只有一行 `os.Exit(pomoai.Main(os.Args[1:]))`，这样 `go install .../cmd/pomoai@latest` 得到的二进制名是 `pomoai`，不是 `pomoai-cli`。

---

## 1. 目录结构与文件职责

```text
pomoai-cli/
├── go.mod                       module github.com/PomoAi-ai/pomoai-cli；依赖 cobra、x/term
├── cmd/pomoai/main.go           入口一行
├── cli.go                       cobra 命令树、全局 flag、每个命令的 RunE（只做参数组装，调下面的函数）
├── output.go                    ExitError{Code,Exit,Msg,Extra}、退出码常量、JSON 成功/失败输出、进度器（TTY 单行 / 非 TTY 逐行）、信号处理
├── config.go                    POMOAI_HOME 解析、config.json 读写（0600）、Key 与 base URL 优先级、Key 指纹
├── store.go                     本地任务记录：原子写、按 local_id/tsk_id 查找、列表、body 文件清理
├── client.go                    HTTP：Do、两种错误形状解析、信封解析、Retry-After、User-Agent、/v1/models、/v1/usage/balance
├── submit.go                    移植 classifySubmitFailure + submitTask（重试、重放、换键）
├── run.go                       续跑主流程：提交 → 轮询 tsk → 视频原生查询 → 下载；轮询间隔、查询失败分类
├── image.go                     generations/edits/Gemini 请求构造与编码；结果提取（url / b64 / fileData / inlineData）
├── video.go                     5 协议表、请求体构造、原生响应解析、safeURL
├── skill.go                     //go:embed skills/pomoai/SKILL.md；guide 输出（去掉 frontmatter）
├── submit_test.go               分类表、重放循环（换键一次、48h、Retry-After 下限与 60s 封顶）
├── video_test.go                5 协议、扁平 openai、newapi 信封与公开查询、unknown
├── client_test.go               错误形状、信封、图片结果提取、退出码映射
├── e2e_test.go                  httptest 假服务器：提交 202 → running → succeeded → 下载落盘；视频原生跟进一条
├── skills/pomoai/SKILL.md
├── .claude-plugin/plugin.json
├── .claude-plugin/marketplace.json   让 `/plugin marketplace add PomoAi-ai/pomoai-cli` 能用
├── .cursor-plugin/plugin.json
├── .goreleaser.yaml
├── .github/workflows/release.yml     tag 触发
├── .github/workflows/ci.yml          可选：PR/push 时只做 gofmt、vet、govulncheck、goreleaser check
├── install.sh
├── npm/pomoai-cli/{package.json, bin/pomoai.js, README.md}   主包模板
├── scripts/npm-publish.mjs           Node 零依赖：从 dist/artifacts.json 生成 6 个平台子包，再发布主包
├── README.md, LICENSE
└── task/001-cli-v1/
```

一共 11 个 Go 源文件加 4 个测试文件。`run.go` 预计最大，约 400 行。下载逻辑放在 `run.go` 里，不单独成文件。

---

## 2. 命令与参数

### 2.1 全局约定

**全局 flag（cobra 持久 flag）：**
- `--json`：stdout 只输出一行 JSON。
- `--base-url URL`
- `--non-interactive`：任何需要提示的地方直接 exit 2。目前只影响 `login`。

**输出分工：**
- stdout 只放结果：非 JSON 模式下每行一个文件绝对路径、任务号或表格。
- stderr 放进度、重试、诊断和续跑提示。
- 加了 `--json` 后进度仍然写 stderr。

**成功形状：** `{"ok":true, ...}`

**失败形状：**
```json
{"ok":false,"error":{"code":"...","message":"...","http_status":429,"task_id":"tsk_…","local_id":"l_…","hint":"pomoai task wait tsk_…"}}
```
- `code` 能透传服务端的就透传，否则用 CLI 自有码（见 2.4）。
- flag 解析失败时 cobra 可能还没解析到 `--json`，因此回退检查 `os.Args` 里有没有 `--json`。

**退出码：**

| 码 | 含义 | 典型触发 |
|---|---|---|
| 0 | 成功 | |
| 1 | 任务失败，或确定未受理，或网络/服务端失败且重试用尽 | `failed`、`native_error`、原生 failed/cancelled、409 `idempotency_conflict`、`task_expired`、`download_failed` |
| 2 | 用法或参数错误 | flag 错误；缺 base URL；非 loopback 的 http；本地校验失败；400 `invalid_request`/`streaming_not_supported`/`endpoint_required`；404 `unsupported_endpoint`；413；`task_service_unavailable` 用尽（base URL 指向了 gw） |
| 3 | 鉴权 | 没有 Key、401、403、`key_mismatch`（本地记录的 Key 指纹与当前 Key 不同） |
| 4 | 本地等待超时，任务仍在跑 | 到了 `--timeout`；hint 是 `pomoai task wait <id>` |
| **5（新增）** | **结果不明，禁止重新 create** | 提交 `unknown`、`replay_expired`、视频 `status:"unknown"`、查询 404 已过竞态窗 |
| 130 / 143 | SIGINT / SIGTERM | 记录已落盘，hint 给出续跑命令 |

为什么在 LinkModel 的分档上加 5：agent 对 4 的正确动作是「继续 `task wait`」，对「结果不明」的正确动作是「不要重提，最多复查一次，然后报告用户」。两者合并的话，agent 可能无限续等，或者把它当成 1 去重新 create 导致重复计费。

### 2.2 各命令

**`pomoai login [--base-url URL]`**
- 读 Key：stdin 是 TTY 时用 `term.ReadPassword` 隐藏输入；非 TTY 时读 stdin 第一行。加了 `--non-interactive` 且 stdin 是 TTY 时 exit 2，提示用管道输入。
- 先调 `/v1/usage/balance` 校验：401/403 不保存，exit 3；网络失败不保存，exit 1。
- 校验通过后原子写入 `config.json`，权限 0600，目录 0700。
- 输出：`{"ok":true,"config":"<abs path>","base_url":"…","key_fp":"9f2c1a7e5b3d4c11","plan_name":"…"}`
- 不提供 `--api-key`。`logout` 不做，删配置文件即可。

**`pomoai whoami`**
- 调 `GET /v1/usage/balance`。
- 输出：`{"ok":true,"base_url","key_source":"env|config","key_fp","plan_name","remaining","used","total","unit","unlimited":bool,"valid":bool}`。`remaining==999999` 判为 `unlimited`。
- 非 JSON 模式输出一段 `key=value` 文本。

**`pomoai models [--kind image|video]`**
- 调 `GET /v1/models`，不带 `client_version` 和 `X-Codex-Catalog`。
- 分类规则：
  - 有 5 种视频 endpoint_type 之一 → `video`，并带上协议列表。
  - 有 `image-generation` → `image`，端点为 `images`。
  - 名字满足 Gemini 判定 → `image`，端点为 `gemini`。
  - 其余不列。
- Gemini 判定：小写后去掉最后一个 `/` 之前的部分，以 `gemini` 开头、包含 `image`、不以 `imagen` 开头。
- 输出：`{"ok":true,"models":[{"id","kind":"image","endpoint":"images|gemini"},{"id","kind":"video","protocols":["ark_v3"]}]}`；非 JSON 输出三列：`ID  KIND  ENDPOINT/PROTOCOLS`。

**`pomoai image create`**
- 必填：`--model`；`--prompt` 或 `--prompt-file <path|->` 二选一。
- 参数：
  - `--image <path>`：可重复，最多 4 张，单张 ≤50MiB，合计 ≤100MiB，与前端一致。
  - `--mask <path>`：只用于 edits。
  - `--size`、`--n`、`--quality`。
  - `--aspect-ratio`、`--image-size`：只用于 Gemini。
  - `--param k=v`：可重复，透传顶层字段。JSON 体里先按 JSON 解析，失败就当字符串；multipart 里原样当字符串。
- 等待相关 flag 见本节末尾。
- 端点选择：
  - Gemini 名 → `/v1beta/models/{PathEscape(model)}:generateContent`，本地图转 `inlineData`；编码前估算 4/3 膨胀，超过 128MiB 则 exit 2。
  - 有 `--image` → `/v1/images/edits`，multipart。
  - 否则 → `/v1/images/generations`，JSON。
- 参数冲突直接 exit 2：Imagen 带 `--image`、Gemini 带 `--size`、非 edits 带 `--mask`。

**`pomoai video create`**
- 必填：`--model`；`--prompt` 或 `--prompt-file`。
- 参数：`--protocol ark_v3|las_v1|newapi_video|openai_video|minimax_v2`、`--duration N`、`--resolution`、`--ratio`、`--size`（只用于 openai）、`--image <http(s) URL | asset://ID>`、`--param k=v`，加等待相关 flag。
- 协议推断：没传 `--protocol` 时查 `/v1/models`，取该模型 `supported_endpoint_types` 与 5 种视频类型的交集：
  - 交集恰好 1 个 → 用它。
  - 交集为空 → exit 2，`model_not_found`。
  - 多个 → exit 2，`protocol_ambiguous`，并在 `candidates` 里列出候选。
- 默认值照搬 `videoProtocols.js:38-76`：时长 5（openai 为 4，minimax 为 6），分辨率 720p（minimax 为 768P），比例 16:9，openai 尺寸 1280x720。
- 本地只保留硬约束：时长是 2–30 的整数；las ≥4；minimax ≤15；openai 只能 4/8/12；比例在白名单内。分辨率和尺寸的白名单**不移植**，因为那是演练表单的选项，由服务端裁决。
- `--image` 是本地路径时 exit 2，`video_reference_must_be_url`。原因是 edge 会以 415 `video_input_unsupported` 拒绝 multipart 和 `data:`，v1 不做上传。

**等待相关 flag（`image create`、`video create`、`task wait` 共用）：**
- `--no-wait`（别名 `--async`）：受理后立即返回。
- `--timeout <dur>`：默认 0，即一直等到终态。
- `--no-download`：只输出 URL。
- `--out <file>` 或 `--out-dir <dir>`：默认是当前目录。

**create 和 wait 的成功输出：**
```json
{"ok":true,"kind":"image","task_id":"tsk_…","local_id":"l_…","status":"succeeded","model":"…",
 "files":[{"path":"/abs/pomoai-3f2a9c1e-1.png","bytes":1234567,"content_type":"image/png"}],"elapsed_seconds":48}
```
- 视频额外带 `"protocol"` 和 `"native_task_id"`。
- 加 `--no-download` 时用 `"urls":[...]` 代替 `files`。
- 加 `--no-wait` 时输出 `{"ok":true,"task_id","local_id","status":"running","hint":"pomoai task wait tsk_…"}`；非 JSON 的 stdout 只有任务号。

**`pomoai task get <tsk_id|local_id>`**
- 只查一次，不下载。视频且 tsk 已 succeeded 时，再查一次原生接口。
- 退出码只反映「查询本身成不成功」，任务状态看 `status` 字段。
- 输出：`{"ok":true,"task_id","local_id","kind","status","native_status","progress","expires_at","error":null|{code,message}}`
- 本地记录还没有 tsk 号（submitting/unknown）时不发网络请求，只报本地状态。

**`pomoai task wait <tsk_id|local_id>`**
- 续跑完整主流程（第 3 节）：
  - 本地状态 submitting/unknown 且距提交不到 48h → 同键重放提交。
  - running → 继续轮询。
  - 视频 → 原生查询。
  - 最后下载。
- 输出和退出码与 create 一致。
- 没有本地记录的 tsk 号也支持：用当前 Key 和 base URL 直接查。视频协议从信封的 `endpoint` 反查协议表，与 `getVideoProtocolByEndpoint` 同义。

**`pomoai task list [--limit 20]`**
- 只列本地记录，按 `created_at` 倒序。
- 输出：`{"ok":true,"tasks":[{local_id,task_id,kind,model,status,created_at,files}]}`

**`pomoai task download <id> [--out|--out-dir]`**
- 要求状态为 succeeded。
- 图片：重查 `/v1/tasks/{id}` 拿 URL（本地不存签名 URL）。
- 视频：重查原生接口拿新地址。

**`pomoai version`**
- 输出：`{"ok":true,"version","commit","date","os","arch"}`；三个值由 ldflags 注入到根包变量。

**`pomoai guide`**
- 输出内嵌的 SKILL.md 正文；`--json` 时为 `{"ok":true,"guide":"…"}`。

### 2.3 凭据与 base URL

- **Key 来源：** 环境变量 `POMOAI_API_KEY` 优先，其次 `config.json` 的 `api_key`。都没有则 exit 3，`api_key_missing`，提示设置环境变量或让用户自己运行 `pomoai login`。Key 只用 `Authorization: Bearer` 发送，原样存、原样用。
- **Base URL 优先级：** `--base-url`，然后 `POMOAI_BASE_URL`，然后 config，最后常量 `DefaultBaseURL`（暂为 `""`，等待用户提供）。
  - 处理：去空白、去尾部 `/`，再去尾部 `/v1`（用户常粘贴带 `/v1` 的地址）。
  - 为空时 exit 2，`base_url_missing`。
  - `http://` 且主机不是 localhost/127.0.0.1/::1 时 exit 2，`base_url_insecure`，避免 Key 明文传输。
- **Key 指纹：** `hex(sha256(key))[:16]`，只存在于记录和输出里。
- **请求头：** 每个请求带 `User-Agent: pomoai-cli/<version> (<os>/<arch>)`。

### 2.4 CLI 自有错误码

`usage`、`base_url_missing`、`base_url_insecure`、`api_key_missing`、`key_mismatch`、`model_not_found`、`protocol_ambiguous`、`video_reference_must_be_url`、`local_record_not_found`、`network_error`、`wait_timeout`、`interrupted`、`submit_unknown`、`replay_expired`、`video_status_unknown`、`task_query_unknown`、`task_expired`、`download_failed`。

其余码透传服务端的 `error.code`。DrainGuard 那种「error 字段是字符串」的形状，同样提取该字符串当 code。

---

## 3. 关键流程

### 3.1 本地记录

**位置：** `$POMOAI_HOME`，默认 `~/.pomoai/`，下面有：
- `config.json`（0600）
- `tasks/<local_id>.json`（0600）
- `tasks/<local_id>.body`（0600）

**为什么不用 XDG 或 `os.UserConfigDir`：** 一个目录、三平台一致，SKILL 和排障文档都只需要写一句话。

**记录格式（JSON，`v:1`）：**
```json
{"v":1,"local_id":"l_3f9a1c2b7d","kind":"image|video","status":"submitting|unknown|running|succeeded|failed|expired",
 "base_url":"https://…","key_fp":"9f2c1a7e5b3d4c11",
 "idempotency_key":"cli-<uuidv4>","rekeyed":false,"sent_before":false,
 "submit_path":"/v1/images/edits","content_type":"multipart/form-data; boundary=…","body_sha256":"…",
 "model":"…","prompt_head":"前 80 字符","protocol":"ark_v3",
 "created_at":"RFC3339","updated_at":"…",
 "task_id":"tsk_…","expires_at":"…","deadline_at":"…",
 "native_task_id":"…","native_status":"running","progress":42,
 "files":["/abs/…"],"error":{"code":"…","message":"…","http_status":0}}
```

**body 文件：**
- 内容是**实际发送的字节**，multipart 的 boundary 已经固化在里面，`content_type` 里存同一个 boundary。
- 每次发送都从这个文件读，所以重放逐字节一致，也不受源图片后来被修改的影响。
- 受理（拿到 tsk 号）或确定失败后立即删除。unknown 状态保留，用于重放；`task list` 顺手删掉超过 48h 的 body。
- 本地**不保存任何签名 URL**，与 `videoProtocols.js:337-341` 同口径。

**并发写：**
- 所有写入都是「同目录临时文件 → `f.Sync()` → `os.Rename`」，Windows 上 `os.Rename` 也会覆盖目标。
- 不加锁。两个进程同时续跑同一记录时，同键重放由服务端幂等兜底，下载是同名覆盖，最终结果是后写者胜，文件不会损坏。
- 列表和查找都是扫描 `tasks/*.json`。`ponytail:` 记录数到上千时才考虑加索引或 `task prune`。

**状态机：**
```text
(新建) --落盘(发送前)--> submitting
submitting --2xx 信封--> running（删 body）
submitting --确定失败--> failed（删 body）
submitting --结果不明 / replay_expired--> unknown（保留 body）
进程中途死亡：读到 submitting → 视为 sent_before=true，等同 unknown（照搬 sanitizeVideoJobs 的 submitting→unknown）
unknown --task wait 且 <48h--> 同键重放 --> running | failed | unknown
running --tsk succeeded（图片）--> succeeded
running --tsk succeeded（视频）--> running（写 native_task_id，进入原生阶段）--原生 completed--> succeeded
running --tsk failed / 原生 failed|cancelled--> failed
running --404 且已过 expires_at--> expired
succeeded --下载成功--> succeeded（写 files）
```

**落盘时机只有 4 处：**
1. 首次发送前：status=submitting、created_at、key、body。
2. 换键前：先写新 key 和 `rekeyed=true`、`sent_before=false`，再发送。崩溃后重启不会二次换键，也不会丢失已经发出去的 key。
3. 提交循环结束时。
4. 轮询中每次状态或进度变化时。

### 3.2 提交、重试、重放（submit.go，逐条移植 asyncTasks.js:140-290）

```go
type submitter struct {
    send  func(ctx context.Context, key string) (status int, body []byte, retryAfter time.Duration, sendErr error) // sendErr = 根本没发出（打不开 body 文件等）；网络错误返回 status 0
    sleep func(ctx context.Context, d time.Duration) error
    now   func() time.Time
    rekey func(newKey string) error // 必须先落盘再返回
}
func (s submitter) run(ctx, key string, sentBefore bool, createdAt time.Time, rekeyed bool) submitResult
// outcome: accepted | failed | unknown | stopped
```

**分类表：**

| 情形 | 动作 | 用尽后 |
|---|---|---|
| status 0、408、2xx 但不是信封、其余 5xx | network 类重试 1/2/4s，并置 `sent_before=true` | unknown |
| 503 `edge_gateway_draining` | 1/2/4s | sent_before ? unknown : failed |
| 429（两种码） | 5/10/20s，与 Retry-After 取较大值，封顶 60s | 同上 |
| 503 存储类三码 | 5s×3，与 Retry-After 取较大值 | 同上 |
| 503 `task_service_unavailable` | 30s×1 | 同上；exit 2，hint 检查 base URL |
| 503 `task_accept_failed` | 换键（整条记录生命周期最多一次） | failed |
| 404 `task_not_found`（重放） | 距 created_at 不到 48h 则换键 | unknown |
| 400 带 code、404 `unsupported_endpoint`、413 `request_too_large` | failed（确定性错误，不论 sent_before） | |
| 401/403/409/其它 4xx | sent_before ? unknown : failed | |

另外两条：
- **每次发送前**都检查：`sent_before && now-created_at ≥ 48h` → unknown，`replay_expired`，不发送。
- 换键限制记录在 `rekeyed` 字段里，跨进程有效，比前端「单次调用内最多一次」更严。

**退出码映射：**
- accepted → 继续 3.3。
- failed → 401/403 给 3；确定性校验错误给 2；其余给 1。
- unknown → 5，hint 为 `pomoai task wait <local_id>`，并提示「不要重新 create，可能重复计费；到使用日志核对」。

**发送细节：**
- 请求为 `POST {base}/v1/tasks{submit_path}`，带 `Authorization`、`Content-Type`、`Idempotency-Key`。
- body 用 `os.Open(body文件)` 流式发送。
- Transport 设 `ResponseHeaderTimeout: 90s`，不设整体超时，因为 body 最大 128MiB。

### 3.3 轮询 tsk（run.go，移植 asyncTasks.js:297-325）

- 请求 `GET {base}/v1/tasks/{id}`，每次 30s 超时，凭据必须与提交时逐字相同。开始前先比对 `key_fp`，不一致 exit 3。
- **间隔：**
  - 图片：首查 2s，然后 3s；超过 30s 后 6s；超过 2 分钟后 15s。
  - 视频：首查 5s，然后 10s。
  - 集中在一个函数 `pollDelay(kind, phase, elapsed)` 里实现。
- **查询失败：**

  | 情形 | 处理 |
  |---|---|
  | 404 且已过 expires_at | exit 1，`task_expired` |
  | 404 且距 created_at 不到 3 分钟 | 继续查 |
  | 404 其他情况 | exit 5，`task_query_unknown` |
  | 401/403 | exit 3 |
  | 429 | 5s 起指数退避，封顶 60s |
  | 503 `task_service_unavailable` | 30s 后再查 |
  | 其它 5xx | 5s 后再查 |
  | 网络错误 | 3s 起指数退避 |
  | 其它 | exit 1 |

- **终态：**
  - failed → 按 `describeTaskFailure`（asyncTasks.js:342-365）生成 message，exit 1。`native_error` 时从 `response.body` 提取原生错误文案。
  - succeeded → 图片进入下载；视频进入 3.4。
- 本地 `--timeout` 到期时，先落盘再 exit 4。

### 3.4 视频原生阶段

1. 用 `parseVideoResponse(protocol, response.body, phase=create)` 解析 succeeded 信封里的创建回包。
   - 原生号优先取 `native_task_id`，其次从 body 提取：newapi 取 `data.task_id`，minimax 取 `task_id`，其余取 `id`。
   - 两者都没有时 exit 5：「上游已受理但拿不到任务号，勿重复生成」。
2. 轮询 `GET {base}{queryPath}/{原生号}`，首查 5s，之后 10s。原生查询按 user_id 判归属，同用户的其他 Key 也能查，但统一用同一个 Key。
3. 解析时完全移植 `videoProtocols.js:187-279`：
   - 状态映射表照搬。
   - 实体位置：minimax 在 `task.*`；newapi 的创建和信封响应在 `code/data`；newapi 公开查询（`object:"video"` 且没有 `code`）走扁平形状；其余直接取对象本身。
   - 进度用 `ParseFloat`，兼容 `"42%"`；只有 openai 和 newapi 才有进度。
   - 地址取 `download_url` / `result_url` / `content.video_url` / `content.url`。
4. 原生查询失败复用 3.3 的分类，404 竞态窗从 tsk 的 `completed_at` 起算。
5. failed/cancelled → exit 1，带 `error`/`fail_reason`。

**`status:"unknown"` 的处理（建议）：** 前端遇到它会停止自动查询。CLI 改为「有界继续查」，因为 GET 不计费、没有副作用：
- unknown 状态下最多继续查 10 分钟。
- 期间转成确定状态就正常继续。
- 仍是 unknown 就落盘为 `unknown`，exit 5，`video_status_unknown`，message 为「上游是否已创建不确定；不要重新 create；稍后 `pomoai task wait <id>` 只会复查，不会重提」。
- 对记录已有 tsk 号的情况，`task wait` 只做查询，不会重新提交。

### 3.5 下载

**选「下载前重查原生接口」，不选 `/v1/videos/{id}/content`。**
- `/content` 只注册在 `/v1/videos` 路由组上（edge-gateway-router.go:219-221），对 ark/las/minimax/newapi 的原生号是否可用没有核实。
- 重查对 5 种协议都成立；签名地址有效期是 min(600s, 剩余保留期)，重查后立刻开始下载，时间足够。
- 下载返回 403/410（签名过期）时再重查一次并重试，最多 1 次。
- 开放问题 Q9：后端如果确认 `/content` 对所有协议通用，可以改成它作为兜底，跟随 302 即可。

**图片结果提取：**
- Images 形状：`response.body.data[].url`，兜底 `b64_json`。
- Gemini 形状：`candidates[].content.parts[]`，跳过 `thought`；取 `fileData.fileUri` 或 `file_data.file_uri`，兜底 `inlineData` 或 `inline_data`；mime 限 png/jpeg/webp/gif/avif。
- 照搬 imageTasks.js:133-170 和 utils.js:1200。

**URL 安全（移植 safeVideoResultUrl）：**
- 只接受 http(s)，且 URL 里不能带 userinfo。
- 以 `/` 开头的相对地址按 base URL 解析，并**带上** Authorization。
- 绝对地址只有当 host 等于 base URL 的 host 时才带 Authorization，否则不带。
- Go 的 http.Client 跨 host 跳转时会自动去掉 Authorization，跟随 COS 302 是安全的。

**落盘：**
- 默认文件名：图片 `pomoai-<tsk 去前缀后的前 8 位>-<i>.<ext>`，视频 `pomoai-<8 位>.mp4`。
- `--out` 只有一个输出时就是原样路径，多个输出时在扩展名前插入 `-<i>`。
- 扩展名：先看 Content-Type（用 png/jpeg/webp/gif/avif/mp4 小映射表），再看 URL 路径，最后用 `.bin`。
- 先写 `<name>.part` 再 rename，同名直接覆盖（与 `curl -o` 一致）。
- 网络错误或 5xx 最多重试 3 次；仍失败 exit 1，`download_failed`，hint 为 `pomoai task download <id>`。
- 输出的 `path` 一律是绝对路径。

### 3.6 中断

- 用 `signal.NotifyContext(SIGINT, SIGTERM)`。ctx 取消后：
  1. 正在发送提交 → 标记 `sent_before=true`，状态改为 unknown。
  2. 落盘。
  3. 在 stderr 输出 `interrupted; task continues on server: pomoai task wait <id>`。
  4. 以 130 或 143 退出。
- 服务端任务**从不取消**，也没有取消接口。
- SIGKILL 无法捕获。补救办法是**首次发送前**就在 stderr 输出 local_id，受理后立刻输出 tsk 号和续跑命令；agent 的输出里总有可以续跑的号，另外还有 `task list` 兜底。

---

## 4. 进度输出

**TTY 模式**（stderr 是字符设备且 `TERM != dumb`）：单行用 `\r` 覆盖刷新，spinner 用 `|/-\`，1s 刷新一次，结束时换行。

```text
| tsk_3f2a…9c1e  image  running  00:41  gpt-image-1
/ tsk_8b11…02aa  video  in_progress 42%  03:10  sora-2      ← 有进度
- tsk_8b11…02aa  video  running  03:10  doubao-seedance…    ← 无进度：状态 + 耗时
```

**非 TTY 模式**：只在状态变化时输出一行，外加每 30s 一行心跳（避免 agent 宿主因长时间无输出把进程判为挂死）。每行固定前缀 `pomoai:`，正文为 key=value。

```text
pomoai: local=l_3f9a1c2b7d submitting endpoint=/v1/images/generations model=gpt-image-1
pomoai: retry 1/3 in 5s (429 task_capacity_exceeded, same idempotency key)
pomoai: accepted task=tsk_3f2a…  resume: pomoai task wait tsk_3f2a…
pomoai: task=tsk_3f2a… status=running elapsed=30s
pomoai: task=tsk_8b11… native=cgt-2026… status=running progress=42% elapsed=3m10s
pomoai: task=tsk_3f2a… status=succeeded elapsed=48s
pomoai: saved /abs/pomoai-3f2a9c1e-1.png (1.2 MB)
```

---

## 5. 发布流水线

### `.goreleaser.yaml`（version: 2）要点

- **builds：**
  - `main: ./cmd/pomoai`，`binary: pomoai`，`env: [CGO_ENABLED=0]`，`flags: [-trimpath]`。
  - goos 为 darwin/linux/windows，goarch 为 amd64/arm64。
  - ldflags：`-s -w -X github.com/PomoAi-ai/pomoai-cli.version={{.Version}} -X …commit={{.ShortCommit}} -X …date={{.Date}}`。
- **archives：**
  - `formats: [tar.gz]`，Windows 覆盖为 `zip`。
  - `name_template: "pomoai_{{ .Os }}_{{ .Arch }}"`，**不带版本号**，这样 install.sh 可以直接用 `releases/latest/download/...`。
  - 打包 `README.md` 和 `LICENSE`。
- **checksum：** `name_template: checksums.txt`（sha256）。
- **release：** `prerelease: auto`。
- **homebrew_casks（选它）：** GoReleaser 从 v2.10 起已弃用 `brews`，官方迁移文档推荐 casks。
  - 配置：`name: pomoai`、`binaries: [pomoai]`、`repository: {owner: PomoAi-ai, name: homebrew-tap, token: "{{ .Env.HOMEBREW_TAP_TOKEN }}"}`、`skip_upload: auto`（rc 版本不推 tap）。
  - 因为没有签名和公证，按官方文档在 post install hook 里去掉隔离属性：
    ```ruby
    if OS.mac?
      system_command "/usr/bin/xattr", args: ["-dr", "com.apple.quarantine", "#{staged_path}/pomoai"]
    end
    ```
  - 用户安装命令：`brew install --cask PomoAi-ai/tap/pomoai`。

### `.github/workflows/release.yml`

- 触发条件：`on: push: tags: ['v*']`。
- `permissions: contents: write, id-token: write`。
- 步骤：
  1. `actions/checkout`（fetch-depth 0）
  2. `actions/setup-go`（`go-version-file: go.mod`）
  3. `go vet ./...`
  4. `govulncheck ./...`（安全扫描）
  5. `goreleaser/goreleaser-action`（`release --clean`）
  6. `actions/setup-node`（Node 22，registry-url 为 npmjs）
  7. `node scripts/npm-publish.mjs`
- **不运行任何 `go test`**，符合团队 CI 规则。
- 可选的 `ci.yml`（PR/push 触发）只做 `gofmt -l`、`go vet`、`govulncheck` 和 `goreleaser check`。

### `install.sh`（POSIX sh）

- 用 `uname -s/-m` 映射成 `darwin|linux` 和 `amd64|arm64`。Windows 不支持，提示改用 npm 或 zip。
- 版本取 `POMOAI_VERSION`，默认 latest，对应 `…/releases/latest/download` 或 `…/releases/download/v$VER`。
- 在 `mktemp -d` 里下载 tarball 和 `checksums.txt`，用 `sha256sum` 或 `shasum -a 256` 比对；两个工具都没有，或比对不一致，都直接失败退出。
- 安装到 `${POMOAI_INSTALL_DIR:-$HOME/.local/bin}`（`install -m 0755`，不用 sudo），装完提示 PATH。
- 分发方式：`curl -fsSL https://raw.githubusercontent.com/PomoAi-ai/pomoai-cli/main/install.sh | sh`。

### npm：用独立脚本，不用 GoReleaser

原因：GoReleaser 的 `npms` 是 Pro 专属，而且它的方式是 postinstall 下载归档，遇到 `--ignore-scripts` 就失效，不符合「optionalDependencies + shim」的要求。

**`scripts/npm-publish.mjs`（Node stdlib）：**
1. 版本取 `GITHUB_REF_NAME` 去掉 `v`；带 `-` 的预发布版本用 `--tag next`。
2. 读 `dist/artifacts.json`，取 `type=="Binary"` 的条目，按 `goos/goarch` 映射到 npm 命名：`windows→win32`，`amd64→x64`。
3. 生成 6 个子包 `dist/npm/@pomoai/cli-{darwin,linux,win32}-{x64,arm64}`：
   - `package.json` 为 `{name, version, os:[…], cpu:[…], files:["bin/"], license, repository}`，不声明 `bin`。
   - 二进制拷到 `bin/pomoai[.exe]` 并 chmod 0755。
4. 主包从 `npm/pomoai-cli/` 拷出，写入 version，`optionalDependencies` 把 6 个子包钉在同一精确版本。
5. 先发 6 个子包，再发主包：`npm publish --access public --provenance`。
6. 每个包发布前先 `npm view <name>@<ver> version`，已存在就跳过，保证重跑幂等。

**`npm/pomoai-cli/bin/pomoai.js`（参考 biome/esbuild）：**
- 用 `require.resolve('@pomoai/cli-${process.platform}-${process.arch}/bin/pomoai[.exe]')` 定位二进制。
- 找不到时在 stderr 说明原因（optionalDependencies 被 `--omit=optional` 跳过，或平台不受支持），并给出 install.sh 替代方案，exit 1。
- 用 `spawnSync(bin, argv, {stdio:'inherit'})` 执行，原样透传退出码；子进程被信号杀死时退出码为 128+n（SIGINT 为 130）。
- 主包写 `engines: {node: ">=18"}`。

**所需 Secrets：**

| 名称 | 用途 |
|---|---|
| `GITHUB_TOKEN` | 内置，创建 Release |
| `HOMEBREW_TAP_TOKEN` | fine-grained PAT，只授权 `PomoAi-ai/homebrew-tap` 的 Contents 读写 |
| `NPM_TOKEN` | 只用于 7 个包的**首次**发布（granular token）。之后在 npmjs 为每个包配置 Trusted Publishing（GitHub OIDC，需要 npm CLI ≥11.5.1），然后删除这个 secret |

---

## 6. Skill 与 plugin 清单

**`skills/pomoai/SKILL.md`：**
- frontmatter：
  - `name: pomoai`
  - `description:` Generate or edit images and create videos through the PomoAi API using the `pomoai` CLI; list PomoAi image/video models, check balance, or resume a PomoAi task (tsk_… / l_…).
- 正文要点（建议英文，便于公开分发）：
  1. 先运行 `pomoai version --json` 确认已安装；未安装时给出 brew/npm/install.sh 三种安装方式。
  2. 凭据只来自 `POMOAI_API_KEY` 或用户本人运行的 `pomoai login`。**不要向用户索要 Key，不要打印 Key，不要读取 `~/.pomoai/config.json`。** exit 3 时让用户自己配置。
  3. 先用 `pomoai models --json` 选模型；视频遇到 `protocol_ambiguous` 就从 `candidates` 里挑一个，用 `--protocol` 重跑。
  4. **一条阻塞命令就够：** `pomoai image create … --json --timeout <比你的工具超时短 30–60s>`。**不要自己写轮询循环。**
  5. 退出码表（2.1）以及每个退出码对应的下一步：
     - 4 → `pomoai task wait <task_id> --json --timeout …`
     - 5 → 不要重新 create；最多 `task wait` 一次，然后把情况报告给用户
     - 2 → 修正参数
     - 3 → 让用户配置 Key
  6. 命令被宿主超时杀掉时，从 stderr 里找 `resume:` 那一行，或用 `pomoai task list --json` 找回 id，再 `task wait`。
  7. 只解析 stdout 那一行 JSON；向用户汇报 `files[].path`，**不要把图片或视频文件读回上下文**。
  8. 视频参考图只能是 http(s) URL；图片参考图是本地文件，最多 4 张。
- `pomoai guide` 输出同一份正文（去掉 frontmatter），两处同源。

**`.claude-plugin/plugin.json`：**
```json
{"name":"pomoai","description":"…","version":"0.1.0","author":{"name":"PomoAi"},"homepage":"https://github.com/PomoAi-ai/pomoai-cli","repository":"https://github.com/PomoAi-ai/pomoai-cli","license":"<待定>","keywords":["image","video","pomoai","cli"]}
```
插件根目录下的 `skills/` 会被自动发现。

**`.claude-plugin/marketplace.json`：**
```json
{"name":"pomoai","owner":{"name":"PomoAi"},"plugins":[{"name":"pomoai","source":"./","description":"…"}]}
```

**`.cursor-plugin/plugin.json`：** 字段与上面相同，`skills` 指向 `./skills/`。Cursor 插件的字段名需要按它当前的规范核对一次，我没有核实。

**`npx skills add PomoAi-ai/pomoai-cli`：** 这个工具按 `skills/<name>/SKILL.md` 约定发现 skill，不需要额外文件。

---

## 7. 分步实现顺序

每一步都可以独立 `go build ./... && go test ./...`，测试在开发机上手工运行。

1. **骨架。** `go.mod`、`cmd/pomoai/main.go`、`cli.go`（只有 version 和 guide）、`output.go`（ExitError、JSON 输出、退出码）、`skill.go` 加一份最小 SKILL.md。测试：退出码映射。
2. **配置与客户端。** `config.go`、`client.go`，加上 `login`、`whoami`、`models`。测试：两种错误形状、非 JSON 回包、信封解析、models 分类。
3. **提交。** `store.go`、`submit.go`。测试：分类表逐行；换键只发生一次且跨进程有效；重放 404 在 48h 内外的分叉；network 用尽后为 unknown；Retry-After 下限与 60s 封顶；`replay_expired` 时不发送。
4. **图片全链路。** `image.go` 和 `run.go`（轮询、下载），加上 `image create`、`task get/wait/list/download`。测试：图片结果提取；e2e 用 httptest 跑通「202 → running → succeeded → 下载落盘」；中途 503 `task_accept_failed` 换键。
5. **视频。** `video.go` 和 `run.go` 的原生阶段，加上 `video create`。测试：5 协议、扁平 openai、newapi 信封与公开查询、unknown 解析；e2e 一条 ark。
6. **体验收口。** TTY/非 TTY 进度、信号处理、首发前输出 local_id、30s 心跳。手工在 `| cat` 和终端两种环境下核对。
7. **发布。** `.goreleaser.yaml`（本地只跑 `goreleaser check`，不在本地打包）、`release.yml`、`install.sh`、npm 模板与脚本、plugin 清单。用 `v0.1.0-rc.1` tag 在 CI 上验证整条流水线：rc 版本走 `skip_upload: auto` 不推 tap，npm 走 `--tag next`。

---

## 8. 开放问题（括号内为推荐默认）

| # | 问题 | 推荐默认 |
|---|---|---|
| Q1 | `DefaultBaseURL` 的值 | 暂为 `""`；缺失时 exit 2 `base_url_missing`。拿到域名后改常量即可 |
| Q2 | 是否接受新增的退出码 5「结果不明」 | 接受（理由见 2.1） |
| Q3 | create 和 wait 默认的 `--timeout` | 不限时；由 SKILL 要求 agent 显式传一个比工具超时更短的值 |
| Q4 | 视频 `status:"unknown"` | 有界复查 10 分钟，之后 exit 5，不重提 |
| Q5 | 拒绝非 loopback 的 `http://` base URL | 拒绝，没有逃生开关 |
| Q6 | 本地目录 | `~/.pomoai`，可用 `POMOAI_HOME` 覆盖；不用 XDG |
| Q7 | `task get` 的退出码语义 | 只反映查询成败，任务状态看 `status` 字段 |
| Q8 | 一个模型对应多个视频协议 | exit 2 并列出 `candidates`，不自动按优先级选 |
| Q9 | `/v1/videos/{id}/content` 是否对 5 种协议的原生号都可用 | 未核实，v1 不依赖它，改用重查原生接口；请后端确认 |
| Q10 | npm 作用域 `@pomoai` 是否已注册、Trusted Publishing 由谁配置 | 首发用 NPM_TOKEN，之后切到 OIDC |
| Q11 | 许可证 | MIT 或 Apache-2.0（CLI 独立仓，不沿用 new-api 的 AGPL），需要拍板，npm、brew、plugin 都会引用 |
| Q12 | macOS 签名与公证 | v1 不做；cask 用 xattr hook，curl 安装方式不会被打隔离标记 |
| Q13 | Windows 的 install.ps1 | v1 不做，Windows 用 npm 或 zip |
| Q14 | SKILL 正文语言 | 英文 |
| Q15 | 下载时同名文件 | 直接覆盖 |
| Q16 | 本地记录自动清理或 `task prune` | v1 不做；只清理超过 48h 的 body 文件 |
| Q17 | plugin.json 的 version 与 CLI 版本同步 | 手工维护 |

---

## 9. 会改变方案的风险

- **查询凭据必须与提交时逐字相同。** 用户在任务进行中换了或重置了 Key，就无法续跑，CLI 会报 `key_mismatch`（exit 3）。这一点要写进 SKILL 和 README。
- **body 文件最大约 128MiB 写在 `~/.pomoai`。** 磁盘写失败必须在发送前 fail-fast（exit 1），绝不能在没有落盘的情况下发送，否则崩溃后无法安全重放。
- **企业或代理密钥（企业口）在 v1 不可用**，因为企业口没有 `/v1/tasks` 路由，会 404 `{"error":"Not Found"}`。CLI 按 exit 2 处理并提示「该地址不提供任务接口」，与 `task_service_unavailable` 的提示合并成一条。
- **`optionalDependencies` 失效的环境**：`--omit=optional`、部分 pnpm/Yarn PnP 配置下拿不到二进制，只能靠 shim 的报错把用户引到 install.sh。
- **Gemini 判定只看模型名。** 如果后端以后出现名字里不含 `image` 的 Gemini 出图模型，需要改为依据 `/v1/models` 的端点类型判断，每次 create 会多一次请求。