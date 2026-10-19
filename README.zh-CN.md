# pomoai-cli

[English](README.md)

`pomoai` 是 PomoAi 的命令行工具，人和 AI agent（Claude Code、Codex、Cursor 等）都能用它通过 PomoAi 异步任务接口（`/v1/tasks`）生成图片和视频：提交任务、等待完成、把结果下载到本地。

- 进度输出到 stderr，结果输出到 stdout。
- 加 `--json` 时 stdout 只输出一行 JSON，方便脚本和 agent 解析。
- 按退出码区分「失败」「超时」「结果不明」，中断后可以用 `pomoai task wait` 接着等，不会重复扣费。

## 安装

```sh
npm i -g https://github.com/PomoAi-ai/pomoai-cli/releases/latest/download/pomoai-cli.tgz   # 任意系统，含 Windows，需要 Node 18+
brew install PomoAi-ai/tap/pomoai                 # macOS / Linux（Homebrew）
curl -fsSL https://raw.githubusercontent.com/PomoAi-ai/pomoai-cli/main/install.sh | sh
go install github.com/PomoAi-ai/pomoai-cli/cmd/pomoai@latest   # 从源码安装，需要 Go 1.25+
```

- `install.sh` 默认装到 `~/.local/bin`，可用 `POMOAI_INSTALL_DIR` 改目录，用 `POMOAI_VERSION=v0.1.0` 指定版本；下载后会校验 SHA-256。
- npm 安装的是 GitHub Release 上的安装包（内含全部平台），不经过 npm 官方仓库。
- Windows 没装 Node 时：从 [Releases](https://github.com/PomoAi-ai/pomoai-cli/releases) 下载 zip，把 `pomoai.exe` 放进 `PATH`。

只给 AI agent 装 Skill（Claude Code、Codex、Cursor 等）：

```sh
npx skills add PomoAi-ai/pomoai-cli
```

Claude Code 也可以作为插件安装：先 `/plugin marketplace add PomoAi-ai/pomoai-cli`，再 `/plugin install pomoai@pomoai`。

## 快速开始

1. 在 <https://www.pomoai.ai/console/token> 创建 API Key。
2. 登录一次。Key 会先向服务端验证，再保存到 `~/.pomoai/config.json`；也可以不登录，改为设置环境变量 `POMOAI_API_KEY`：

   ```sh
   pomoai login
   pomoai whoami                       # 查看余额和当前生效的 Key 来源
   ```

3. 选模型，开始生成：

   ```sh
   pomoai models --kind image          # 从列表里复制模型 ID
   pomoai image create --model <模型ID> --prompt "雪地里的一只红狐狸" --out-dir ./out

   pomoai models --kind video
   pomoai video create --model <模型ID> --prompt "海上日出" --duration 5
   ```

每个命令都带示例：`pomoai <命令> --help`，例如 `pomoai image create --help`。

## 常用示例

```sh
# 文生图，指定尺寸，保存到 ./out
pomoai image create --model <模型ID> --prompt "水彩风格的橘猫" --size 1024x1024 --out-dir out

# 改图：最多 4 张本地图片，可选蒙版
pomoai image create --model <模型ID> --prompt "改成夜景" --image photo.png --mask mask.png

# Gemini 图片模型：用宽高比和清晰度
pomoai image create --model <Gemini模型ID> --prompt "一张海报" --aspect-ratio 16:9 --image-size 2K

# 图生视频：--image 只接受 http(s) 地址或 asset://ID，不接受本地文件
pomoai video create --model <模型ID> --prompt "猫转头看向镜头" --image https://example.com/cat.png

# 只提交不等待，稍后再取结果
pomoai video create --model <模型ID> --prompt "..." --no-wait
pomoai task wait <tsk_id>

# 提示词很长时从文件或 stdin 读
pomoai image create --model <模型ID> --prompt-file prompt.txt
```

## 命令

| 命令 | 作用 |
|---|---|
| `pomoai login` | 读取 API Key（隐藏输入，或 stdin 第一行），验证后保存 |
| `pomoai whoami` | 余额、套餐名、当前生效的 Key 来源 |
| `pomoai models [--kind image\|video]` | 列出可用的图片模型（`images` / `gemini` 端点）和视频模型（协议） |
| `pomoai image create` | 文生图；`--image` 带最多 4 张本地图做改图；支持 Gemini 图片模型 |
| `pomoai video create` | 生成视频，协议为 `ark_v3`、`las_v1`、`newapi_video`、`openai_video`、`minimax_v2` 之一；不传 `--protocol` 时根据 `/v1/models` 自动判断 |
| `pomoai task get <id>` | 查一次任务状态（退出码反映「查询」是否成功，不反映任务成败） |
| `pomoai task wait <id>` | 继续等待：未确认受理的提交用同一个幂等键重放，然后轮询并下载 |
| `pomoai task list` | 本地任务记录，最新的在前 |
| `pomoai task download <id>` | 重新下载已成功任务的结果 |
| `pomoai version` / `pomoai guide` | 版本信息 / 给 AI agent 的使用说明 |

`<id>` 可以是服务端任务 ID（`tsk_…`），也可以是本地记录 ID（`l_…`）。

`create` 和 `task wait` 的通用参数：

| 参数 | 作用 |
|---|---|
| `--json` | stdout 只输出一行 JSON |
| `--timeout 9m` | 本地最多等这么久，到点退出码 4；服务端任务继续执行 |
| `--no-wait`（`--async`） | 受理后立即返回 |
| `--no-download` | 只输出结果地址，不下载 |
| `--out <文件>` / `--out-dir <目录>` | 保存位置；多个结果时文件名自动加 `-1`、`-2` |
| `--param key=value` | 额外的请求字段，可重复 |

本地 `--timeout` 和 Ctrl-C 都不会取消服务端任务，用 `pomoai task wait <id>` 接着等即可。

## 环境变量

| 变量 | 含义 |
|---|---|
| `POMOAI_API_KEY` | API Key，优先于 `pomoai login` 保存的 Key |
| `POMOAI_BASE_URL` | API 地址，默认 `https://www.pomoai.ai`。优先级：`--base-url` > `POMOAI_BASE_URL` > `pomoai login` 保存的地址 > 默认值。末尾带 `/v1` 也可以。明文 `http://` 只允许 localhost / 本机回环地址 |
| `POMOAI_HOME` | 本地数据目录，默认 `~/.pomoai` |

连本机网关测试：

```sh
POMOAI_BASE_URL=http://localhost:3000 pomoai whoami
```

任务只能用提交它的那个 Key 查询。任务执行期间如果换了或重置了 Key，`task wait` 会报 `key_mismatch`。

## 退出码

| 退出码 | 含义 | 该怎么做 |
|---|---|---|
| 0 | 成功 | |
| 1 | 任务失败、确定未受理，或重试后仍是网络/服务端错误 | 看错误信息，修正后重新提交 |
| 2 | 用法或参数错误（包括 API 地址无效或不安全） | 修正参数 |
| 3 | 认证问题：没有 Key、401/403、`key_mismatch` | 检查 Key，或重新 `pomoai login` |
| 4 | 到了本地 `--timeout`，任务仍在执行 | `pomoai task wait <id>` |
| 5 | 结果不明 | **不要重新提交**（可能重复扣费）；最多 `task wait` 一次，再去用量日志核对 |
| 130 / 143 | 被 Ctrl-C（SIGINT）/ SIGTERM 中断，记录已保存 | `pomoai task wait <id>` |

失败时 `--json` 输出 `{"ok":false,"error":{"code","message","hint",…}}`，`hint` 里通常就是下一步该运行的命令。

## 本地目录

```text
~/.pomoai/                  （或 $POMOAI_HOME；权限 0700）
├── config.json             pomoai login 保存的 API 地址和 Key（0600）
└── tasks/
    ├── l_<id>.json         任务记录：各种 ID、状态、Key 指纹、结果文件（0600）
    └── l_<id>.body         原始请求字节，任务受理后即删除（0600）
```

- 任务记录里不保存 API Key，也不保存带签名的结果地址。
- 服务端结果保留 48 小时，过期前记得下载。
- 删除 `config.json` 即退出登录。

## 常见问题

- **装好了但提示找不到 `pomoai`**：`~/.local/bin` 不在 `PATH` 里，按 `install.sh` 最后的提示把它加进 shell 配置。
- **macOS 提示「无法验证开发者」**：用 Homebrew 安装会自动处理；手动下载的可以执行 `xattr -d com.apple.quarantine $(which pomoai)`。
- **国内下载 GitHub 很慢**：可以先手动从 Releases 下载对应平台的压缩包，解压后把 `pomoai` 放进 `PATH`。
- **视频一直没好**：视频通常要几分钟。不想一直等就加 `--no-wait` 或 `--timeout`，之后用 `pomoai task wait <id>` 取结果。

## AI agent

`skills/pomoai/SKILL.md` 是给 agent 用的 Agent Skill，说明 agent 应该如何使用这个 CLI；`pomoai guide` 输出同样的内容。

## 许可证

MIT，见 [LICENSE](LICENSE)。
