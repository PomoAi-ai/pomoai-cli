# 探索：CLI 要对接的 PomoAi 外部契约

来源：pomoai-code（`../pomoai-code-next`）工作树，2026-10-09，dev:explorer 只读整理。路径均相对 pomoai-code 仓库。

## 关键结论
- `/v1/tasks*` 只挂在 **edge 主口**；gw1/gw2 回 503 `task_service_unavailable`（Retry-After 30），企业口无此路由（NoRoute 404 `{"error":"Not Found"}`）。
- **令牌侧没有 tsk_ 任务列表**：`GET /api/user/self/async-tasks` 只认主站登录会话（UserAuth），不接受 API Key。
- 仓库无正式公开 API 域名 → `--base-url`/`POMOAI_BASE_URL` 必须显式配置，缺失 fail-fast（待用户给默认值）。
- 企业/代理密钥（企业口）第一版用不了异步任务。

## 1. `/v1/tasks`
| 方法 | 路径 | 行为 |
|---|---|---|
| POST | `/v1/tasks` | 400 `endpoint_required` |
| POST | `/v1/tasks{原生路径}[?query]` | 提交 / 同键幂等重放 |
| GET | `/v1/tasks/{task_id}` | 查询信封（恒 200） |

- task_id：`^tsk_[0-9a-f]{64}$`。
- 白名单（`backend/gateway/service/async_task_endpoints.go:72-101`，精确匹配）：
  - 图片：`/v1/images/generations`、`/v1/images/edits`（JSON 或 multipart）
  - Gemini：`^/(v1|v1beta)/models/([^/:]+):generateContent$`（信封 endpoint 为模板 `/v1beta/models/{model}:generateContent`）
  - 视频：`/v1/video/generations`、`/v1/videos`、`/api/v3/contents/generations/tasks`、`/api/v1/contents/generations/tasks`、`/v2/video_generation`
  - 其余 404 `unsupported_endpoint`。
- 凭据：`Authorization: Bearer` 或 `x-api-key`；Gemini 路径 `?key=`、`x-goog-api-key` 依次覆盖；只剥 `Bearer ` 前缀。无凭据 401 `invalid_api_key`（读 body 前）。
- Idempotency-Key：可选，≤255 字节；作用域是**用户**（同用户任意令牌同键同摘要 → 同任务）。
- 摘要：endpoint、转义 path、去 `key` 后排序 query、媒体类型、body 身份（JSON 顶层键排序；multipart 每分片 name/filename/Content-Type/sha256，boundary 不计）、`X-Group`/`anthropic-version`/`anthropic-beta`/`OpenAI-Beta`。
- 流式一律 400 `streaming_not_supported`。
- 上限：body `MAX_REQUEST_BODY_MB` 默认 128（413）；path>512B、model>128B、multipart 分片>1000、JSON 顶层成员>4096 → 400 `invalid_request`。

### 信封（`types/async_task.go:136-150`）
`id`、`object:"task"`、`endpoint`、`path`、`model`、`status`（`running|succeeded|failed`）、`created_at`/`completed_at`（Unix 秒，可 null）、`expires_at`（终态 completed_at+48h；running 为 deadline_at+48h）、`request_id`、`native_task_id`（仅视频，omitempty）、`response`（null 或 `{status_code, content_type, retry_after?, body}`，body 任意 JSON，非 JSON 包成字符串）、`error`（null 或 `{code, message}`）。
- 无 `deadline_at` 字段；running 时 `expires_at - 48h` 推出。
- running 且 now≥deadline → 对外 failed `execution_interrupted`。
- 执行时限：图片 60 分钟（deadline≈受理后 80 分钟），视频提交 5 分钟。
- HTTP：新受理 202；重放命中 running 202、终态 200，均带 `Idempotency-Replayed: true`。

### 任务层 error.code
`native_error`（原生非 2xx，含计费失败/429/无渠道，原生错误在 response.body）、`execution_interrupted`、`result_empty`、`result_too_large`、`result_store_failed`、`image_transfer_failed`（status_code 502，带 usage）、`result_unavailable`。

### 错误回包
`{"error":{"code","message"}}`；例外：DrainGuard 503 `{"error":"edge_gateway_draining","reason","request_id"}`（字符串，无 Retry-After）、NoRoute `{"error":"Not Found"}`。

| HTTP | code | 处理 |
|---|---|---|
| 400 | endpoint_required / streaming_not_supported / invalid_idempotency_key / invalid_request | 修正 |
| 401 | invalid_api_key | 不重试 |
| 403 | access_denied | 不重试 |
| 404 | unsupported_endpoint | 不重试 |
| 404 | task_not_found | 重放：过期或 meta 缺失 >3 分钟 → 换新键；查询：五种合并（无 Key/ID 非法/不存在/owner 不符/过期） |
| 409 | idempotency_conflict | 同键不同摘要 |
| 413 | request_too_large | 不重试 |
| 429 | task_capacity_exceeded | Retry-After 5，原键重试 |
| 429 | rate_limit_exceeded | 按 Retry-After，原键重试 |
| 503 | task_storage_unavailable / task_registry_unavailable / task_timeout_misconfigured | Retry-After 5，原键重试 |
| 503 | task_service_unavailable | Retry-After 30；打到 gw → 提示检查 base-url |
| 503 | task_accept_failed | **必须换新键** |
| 503 | edge_gateway_draining | 原键重试 |

前端重试表（`frontend/web/src/helpers/asyncTasks.js:40-46,140-169`）：网络 1/2/4s；draining 1/2/4s；429 5/10/20s 与 Retry-After 取大、≤60s；存储类 503 5s×3；task_service_unavailable 30s×1。

### 重放与换键
- 幂等窗口 = deadline_at + 48h；过窗同键 → 新任务再计费。距本地提交满 48h 禁止同键重放（asyncTasks.js:188-229）。
- 换键只允许：503 `task_accept_failed`；或重放 404 `task_not_found` 且距提交 <48h。每次提交最多换一次。

### 轮询与保留
- 间隔：图片 ≥2s，视频 ≥10s。创造中心：图片首查 2s→3s，30s 不变 6s、2 分钟后 15s；视频首查 5s→10s。
- **查询凭据须与提交逐字相同**（owner = 规范化凭据哈希）；同用户其他令牌查询也 404。
- 限流：每节点每 Key 6000/60s（提交+查询共用，带不带 `sk-` 两个桶）。
- 保留 48h；Base64 图片签名 URL 48h（略早于 expires_at 失效），查询不重签。
- 404 分流（asyncTasks.js:305-317）：已过 expires_at → 过期；距提交 <3 分钟 → 继续查；否则「待确认」。

### 令牌侧列表替代
- 原生视频列表（TokenAuth，按用户，**只列渠道 61 PomoVideo**，MiniMax 近 7 天）：`GET /v1/videos?after=&limit=&order=`（≤100，`{object:"list",data,has_more,first_id,last_id}`）、`GET /api/v3/contents/generations/tasks?page_num=&page_size=&filter.*`（`{items,total}`）、`GET /v2/query/video_generation`。
- `GET /api/log/token` 消费日志（按 request_id 对账）。
- 结论：`task list` 第一版只列 CLI 本地落盘记录。

## 2. 视频（5 协议）
流程：POST `/v1/tasks{createPath}` → 轮询 tsk 至 succeeded（视频只要原生创建 2xx 就 succeeded）→ 原生号：`native_task_id`，空则从 response.body 回退 → 轮询原生查询（前端 4s）。

| 协议 | endpoint_type | createPath | 主要字段 | 原生号 | queryPath |
|---|---|---|---|---|---|
| ark_v3 | ark-video | `/api/v3/contents/generations/tasks` | `model, content:[{type:text,text},{type:image_url,image_url:{url},role:first_frame}], duration, resolution, ratio` | `id` | `/api/v3/contents/generations/tasks/{id}` |
| las_v1 | las-video | `/api/v1/contents/generations/tasks` | 同 ark；时长≥4，可 4k | `id` | `/api/v1/contents/generations/tasks/{id}` |
| newapi_video | newapi-video | `/v1/video/generations` | `model, prompt, seconds:"5", metadata:{resolution,ratio}, image?` | `data.task_id`（后端不填 native_task_id） | `/v1/video/generations/{id}` |
| openai_video | openai-video | `/v1/videos` | `model, prompt, seconds:"4|8|12", size:"1280x720", input_reference?:{image_url}` | `id` | `/v1/videos/{id}` |
| minimax_v2 | minimax-video-v2 | `/v2/video_generation` | 同 ark content；时长≤15；带参考图 ratio=adaptive | `task_id` | `/v2/query/video_generation/{id}` |

来源：`packages/shared/src/constants/videoEndpoints.js:3-29`、`frontend/web/src/helpers/videoProtocols.js:37-178`、`backend/gateway/service/video_protocol_create_response.go:11-23`。

原生查询结果（`service/video_protocol_response.go:20-75,140-172`；`videoProtocols.js:207-279`）：
| 协议 | 状态 | 进度 | 地址 |
|---|---|---|---|
| openai | `status`: queued/in_progress/completed/failed | `progress` int 0–100 | `download_url`（+`download_url_expires_at`、`expires_at`） |
| ark/las | `status`: queued/running/succeeded/failed/cancelled | 无 | `content.video_url` |
| minimax | `{task:{...}}`，`task.status` 同 ark | 无 | `task.content.url` |
| newapi | `data.status`: NOT_START/SUBMITTED/QUEUED/IN_PROGRESS/SUCCESS/FAILURE | `data.progress` "N%" | `data.result_url`，失败 `data.fail_reason` |
- 非 61 渠道旧任务可能回扁平 `{id, object:"video", status, progress, download_url...}`，两形状都要兼容。
- 生成完成但转存未完成 → 对外「处理中」；ready 才给地址。
- 下载：COS 预签名 URL，有效期 min(600s, 剩余保留)；或 `GET /v1/videos/{id}/content` → 302（跟随跳转）。拒绝码 409 video_not_ready / 502 video_delivery_failed / 410 video_expired / 404 / 401。
- 原生查询按 user_id 判归属（同用户其他令牌可查），与 `/v1/tasks` 不同。
- 创建回 202 `status:"unknown"` 时 native_task_id 仍写入；前端判「待确认」停自动查询。
- edge 视频预检拒绝 multipart 和 `data:` 内联素材（415 `video_input_unsupported`），参考图只能 http(s)（Pomo v2 图片槽例外）。

## 3. 图片
| 场景 | 路径 | 请求体 |
|---|---|---|
| 文生图 | `/v1/tasks/v1/images/generations` | JSON `{model, prompt, size, n, quality, output_format, background, moderation, output_compression...}`，不写 stream |
| 参考图编辑 | `/v1/tasks/v1/images/edits` | multipart：普通字段 String(value)；每张 `image[]` 带原文件名；可选 `mask` |
| Gemini | `/v1/tasks/v1beta/models/{model}:generateContent` | `{contents:[{role:user,parts:[{text},{inlineData:{mimeType,data}}]}], generationConfig:{responseModalities:["TEXT","IMAGE"], imageConfig?:{aspectRatio,imageSize}}}` |
- Gemini 判定：gemini 族、名含 `image`、不以 `imagen` 开头；Imagen 不支持参考图。
- 参考图：前端限 4 张、单张 ≤50MiB、合计 ≤100MiB；服务端硬上限只有 body 128MiB。
- 结果：Images `response.body.data[].url`；Gemini `candidates[].content.parts[].fileData.fileUri`（兼容 `file_data.file_uri`，跳过 thought）；残留 `b64_json`/`inlineData` 兜底解码。
- 来源：`frontend/web/src/pages/CreateCenter/imageTasks.js:42-168`、`frontend/web/src/pages/DrawingPlayground/utils.js:1200-1211,1468-1558`、`paramProfiles.js:119-235`。

## 4. models 与 whoami
- `GET /v1/models`（edge 反代中心 TokenAuth）：`{"success":true,"object":"list","data":[{id, object:"model", created, owned_by, supported_endpoint_types:[...]}]}`。**不要带 `client_version` query 或 `X-Codex-Catalog: 1`**（会返回 Codex 目录格式）。
- `supported_endpoint_types`：`image-generation`（图片）、`gemini`（结合名含 image）、`ark-video`/`las-video`/`newapi-video`/`openai-video`/`minimax-video-v2`（视频，直接映射协议）。
- whoami：`GET /v1/usage/balance`（经 edge）→ `{remaining, used, total, unit:"USD|CNY|Tokens", planName(令牌名), isValid}`，无限额度 999999。`/v1/dashboard/billing/*` 在 edge 404，不用。

## 5. 基址
- 无正式公开域名（Services 页 SSR 兜底 `https://api.pomoai.com/v1`，OpenAPI 示例 `https://api.example.com`）。
- 基址必须到 edge 主口；打到 gw → 503 `task_service_unavailable`，提示切换 API 地址。

## 6. 其他限制
| 项 | 值 |
|---|---|
| 单任务回包捕获上限 | `ASYNC_TASK_CAPTURE_MAX_MB` 默认 128 → `result_too_large` |
| 中心代理面限流 | `/v1/models`、`/v1/usage/balance`、原生视频查询按客户端 IP 540/180s |
| drain | 提交回字符串 503；查询不受影响 |

## CLI 设计注意的坑
1. API Key 原样存、原样用；本地任务记录存 key 指纹，`task get` 必须用同一 key。
2. 查询 404 不等于失败：过期 / 换 key / 竞态（<3 分钟继续查）/ 待确认。
3. Idempotency-Key 由 CLI 生成并落盘；换键只允许两种情况；满 48h 禁同键重放；重放请求体须逐字节一致（JSON 原文、multipart 文件名/类型/内容）。
4. 两种错误形状都要解析。
5. 视频 succeeded 只表示上游已创建，需按原生号轮询原生查询；newapi 回退 `response.body.data.task_id`；`status:"unknown"` 情况的处理需设计决定。
6. 视频下载地址约 10 分钟过期：下载前重查或用 `/v1/videos/{id}/content` 跟随 302。
7. ark/las/minimax 无进度百分比，进度显示要支持「无进度」。
8. `task list` 只能列本地记录。
9. `/v1/models` 不带 Codex 目录参数。
10. whoami 用 `/v1/usage/balance`。
