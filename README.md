<div align="center">

# Cline2API

Cline API 反向代理 · 多账号轮询 · 双协议兼容 · 桌面端

[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Platform](https://img.shields.io/badge/Platform-Windows%20%7C%20macOS%20%7C%20Linux-blue)](#构建)

**🌐 English: [English README](README.en.md)**

</div>

---

## 简介

Cline2API 是 Cline API 的反向代理服务，支持多账号轮询、OpenAI 和 Anthropic Messages API 双协议、API Key 鉴权，内置中英文管理后台（自动跟随浏览器语言，可手动切换）。账号、模型、请求日志、渠道、配置、管理员、会话与 API Key 均使用 PostgreSQL；Docker Compose 会一并启动数据库。

**开发语言**：Go（后端 + 代理 + 桌面壳），React 19 / TypeScript（管理后台前端，构建后内嵌于二进制）。

## 功能

- **双协议兼容**：同时支持 `/v1/chat/completions`（OpenAI）和 `/v1/messages`（Anthropic Messages API）
- **多账号轮询**：自动在多个 Cline 账号间切换负载（`round_robin` / `fill` / `random` / `least_latency` 策略）
- **中英文管理后台**：浏览器访问 `/admin/` 管理账号、API Key、模型配置、请求头、代理设置；自动跟随浏览器语言，侧栏可手动切换
- **动态模型同步**：启动时自动拉取 Cline 官方推荐模型接口（免费/订阅模型），模型变化时弹窗提示，也可在后台手动「从 Cline 同步模型」
- **第三方渠道管理**：接入 OpenAI Chat Completions 或 Anthropic Messages 兼容上游，同步/映射模型，并在支持同一公开模型的渠道间负载均衡
- **自定义模型**：后台可手动添加/删除模型 ID，并自由选择默认模型（未设置时自动回退到第一个免费模型）
- **API Key 鉴权**：保护代理端点，使用 `sk-{uuid}` 格式；每根密钥可设置到期时间、按百万 Token 计的累计额度、可调用模型及模型别名，也可查看详情、编辑和吊销。密钥只在创建时显示一次
- **System Prompt 覆盖**：项目目录下放 `override.md` 则自动替换系统提示词
- **账号导入/导出**：支持 OAuth 登录、手动 Token、批量文件导入，以及跨设备导出
- **Token 自动续期**：每分钟巡检一次，并在账号 Token 即将过期前 5 分钟主动刷新；请求前与 401 响应时仍会兜底续期
- **请求日志**：记录每次请求的 token 用量、耗时、TPS 等指标
- **桌面端**：单文件跨平台桌面应用（Wails v2）；运行时需连接 PostgreSQL

## 快速开始

### 方式一：桌面端

从 [Releases](https://github.com/luawei1/cline2api/releases) 下载对应平台的可执行文件。运行前需设置 `CLINE_DATABASE_URL`、`CLINE_ADMIN_EMAIL` 和 `CLINE_ADMIN_PASSWORD` 并准备 PostgreSQL；Docker Compose 会自动配置这些服务。

> Windows 提示 SmartScreen「已保护你的电脑」是**未购买代码签名证书的正常现象**，
> 点击「更多信息 → 仍要运行」即可，不影响使用。

| 平台 | 文件 | 说明 |
|------|------|------|
| Windows x64 | `cline-proxy-desktop.exe` | Win10/11 自带 WebView2 |
| macOS Apple Silicon | `cline-proxy-desktop-darwin-arm64` | 需 Xcode CLT |
| macOS Intel | `cline-proxy-desktop-darwin-amd64` | 需 Xcode CLT |
| Linux x64 | `cline-proxy-desktop-linux-amd64` | 需 GTK3 + WebKit2GTK |

### 方式二：命令行

```bash
cd frontend && pnpm install --frozen-lockfile && pnpm build && cd ..
go build -o cline-proxy .
./cline-proxy              # 默认端口 3457
./cline-proxy -port 8080   # 指定端口
```

启动前需设置 `CLINE_DATABASE_URL`、`CLINE_ADMIN_EMAIL` 和首次初始化用的 `CLINE_ADMIN_PASSWORD`。启动后访问 http://127.0.0.1:3457/admin/ 进入管理后台。

### 方式三：Docker

```bash
cp .env.example .env  # 填写 CLINE_DB_PASSWORD 和 CLINE_ADMIN_PASSWORD
docker compose up -d      # 构建并启动
docker compose logs -f     # 查看日志
docker compose down        # 停止
```

容器监听 `0.0.0.0:3457`，Compose 映射为主机端口 `4000`。管理后台需要邮箱和密码登录；对公网部署时请配置 HTTPS。

## 使用指南

### 1. 添加 Cline 账号

在管理后台 **账号管理 → 导入账号**：

- **OAuth 浏览器登录**：点击按钮启动设备授权流程，在系统浏览器中完成登录（支持已登录 Cline 的浏览器）
- **手动输入 Token**：输入已有账号的 refreshToken
- **批量文件导入**：上传 JSON 文件或粘贴文本（每行一个 token，或 JSON 数组 `[{refreshToken, email}]`）

### 2. 配置客户端

```
Base URL: http://127.0.0.1:3457/v1
API Key:  <设置 → API 密钥中生成的 sk-{uuid}>
Model:    z-ai/glm-5.3-flash
```

兼容 OpenAI 和 Anthropic 两种 API 格式。

### 第三方渠道

管理后台的 **渠道管理** 页面可以添加渠道名称、协议、Base URL 和 API Key。保存后可从上游 `/models` 同步模型，也可以手动添加上游 Model ID，并为每个模型设置公开 Model ID。多个启用渠道映射到同一个公开 Model ID 时，可按轮询、填满、随机或最低延迟策略选择；系统用 EWMA 学习各模型渠道的首事件延迟，异常慢于已知替代渠道时会短暂冷却。429、认证失败、网络错误和 5xx 同样会触发冷却，并在响应尚未开始时最多切换一次其他渠道。渠道默认禁止访问回环、私网、链路本地和云元数据地址；只有可信内网 Provider 才应显式开启“私网访问”。

设置页还可以选择 Anthropic thinking 请求的默认 `low` / `medium` / `high` effort；请求显式提供 effort 时始终以请求为准，`thinking.type=disabled` 不会再被转换成高推理模式。请求日志分别展示上游首事件、thinking 首字、可见正文首字与缓存命中状态。

对 DeepSeek V4 和 GLM 5.3 的非流式轻量辅助请求（不超过 256 输出 token、无工具、未显式要求 thinking/reasoning），代理会关闭 thinking，避免标题或摘要任务把全部输出预算消耗在 reasoning。大任务、客户端流式请求和显式推理请求不受影响。

自定义渠道可选择 OpenAI Chat Completions (`/chat/completions`)、OpenAI Responses (`/responses`) 或 Anthropic Messages (`/messages`) 作为上游协议。上游响应会归一化，因此下游仍可使用 Chat Completions、Responses 或 Anthropic Messages。

如果某个 Responses 上游要求 `stream: true`，可在该渠道开启“Responses 上游强制流式”。下游非流式请求会在上游流式完成后汇总返回；其他渠道不受影响。

### API 协议兼容

| 协议 | 标准端点 | 已支持的核心调用 |
|------|----------|------------------|
| OpenAI Chat Completions | `POST /v1/chat/completions` | 标准 Chat Completion / Chunk、文本、多模态图片、函数工具、并行工具调用、`stream_options.include_usage` |
| OpenAI Responses | `POST /v1/responses` | `input` / `instructions`、多模态图片、`reasoning.effort`、`text.format`、自定义函数与 `function_call_output`、标准 Responses SSE 生命周期 |
| Anthropic Messages | `POST /v1/messages` | system/content blocks、base64/URL 图片、`stop_sequences`、`output_config`、客户端工具、多个 `tool_result`、标准 Messages SSE 生命周期 |
| Anthropic Token Count | `POST /v1/messages/count_tokens` | 返回标准 `{ "input_tokens": number }` 结构（本地近似估算） |

鉴权同时接受 OpenAI 的 `Authorization: Bearer <key>` 和 Anthropic 的 `x-api-key: <key>`；Anthropic SDK 可照常发送 `anthropic-version`、`anthropic-beta` 请求头。

API Key 的模型候选来自当前 `/v1/models` 对外可见列表。不选择模型或点击“全部模型（all）”表示不限制模型，该密钥的 `/v1/models` 自动跟随全局模型展示配置；逐个勾选则使用固定白名单。设置别名后，该密钥的模型列表展示别名；客户端用别名或原模型 ID 调用时都会路由到原模型。额度按累计总 Token 计算（1M = 1,000,000 Token）；优先使用上游返回的 usage，缺少 usage 的已完成请求按输入量估算。详情中的计量请求仅统计有实际或估算用量的调用。额度到达后拒绝新的生成请求，已经开始的并发请求可能使最终用量略超额度。旧版密钥保留原有不限模型、不限额度行为，编辑后可设置限制。

> 上游实际是 Chat Completions，因此无法可靠模拟需要厂商服务端状态或托管执行环境的功能。OpenAI 的 `background`、`previous_response_id`、`conversation`、托管工具，以及 Anthropic 的服务端工具、容器/Skill 等请求会返回标准错误，不会静默丢弃。Responses 可通过在下一次 `input` 中回传之前的 output items 实现无状态多轮调用。

Responses 流会把上游 `reasoning_content` 转换为标准 reasoning item 与 reasoning summary delta，并在后续 input 中恢复 reasoning history。代理会在提交客户端 SSE 前检查流初始化；遇到可恢复的 429、5xx、空流、首事件超时或提前 EOF 时最多切换一个账号重试一次。只有收到 `[DONE]` 或明确 `finish_reason` 才会标记完成；`length` / `content_filter` 返回 `response.incomplete`，无终止事件的 EOF 返回 `response.failed`。

Chat Completions 对外只返回 OpenAI 标准字段；上游专用的 `reasoning_content`、计费字段与 provider metadata 不会泄漏。需要流式 reasoning 的客户端应使用 Responses。设置 `stream_options: {"include_usage": true}` 时，普通 chunk 的 `usage` 为 `null`，并在 `[DONE]` 前发送 `choices: []` 的最终 usage chunk。Chat 与 Responses 共享 30 秒首事件超时、模型级短暂冷却和最多一次换号重试。

也可以请求虚拟模型 `free`：远程模型同步成功后，代理只使用上游当前仍在架的免费模型，并优先尝试 `z-ai/glm-5.3-flash`、`deepseek/deepseek-v4-flash`、`cline-free/longcat-2.0`；其余新免费模型按上游顺序补入。离线时回退到这三个内置模型。每个模型最多尝试 2 个未冷却账号，避免免费池故障时产生无界重试；请求日志记录最终实际模型。

多用户隔离：每次发往 Cline 的上游尝试都生成独立的 128 位安全随机会话 ID，并同时用于 `X-Task-ID` 和 body `session_id`；401 原请求重放保持同一 ID，新的尝试绝不复用。Zen 压缩状态、客户端缓存键与 user 标识按下游 API Key 的不可逆租户摘要隔离；代理请求必须提供有效 API Key。审计日志只记录随机 request/task ID、遮罩后的 Key 标识和带进程随机密钥的 HMAC-SHA256，不记录提示词或响应正文。不同人员或应用必须使用不同 API Key；账号池仍由实例全局共享，敏感多租户场景还应使用独立实例/账号池。

现代 Codex 自定义 Provider 使用 Responses 协议。仓库内的 `codex-models.json` 提供 DeepSeek 与 GLM 的 1M 上下文、reasoning、shell 和 apply_patch 元数据，可避免 Codex 的 unknown-model 临时错误。示例 `~/.codex/config.toml`：

```bash
curl http://127.0.0.1:3458/codex-models.json -o "$HOME/.codex/cline2api-models.json"
```

```toml
model = "deepseek/deepseek-v4-flash"
model_provider = "cline2api"
model_catalog_json = "/absolute/path/to/.codex/cline2api-models.json"

[model_providers.cline2api]
name = "Cline2API"
base_url = "http://127.0.0.1:3458/v1"
env_key = "CLINE2API_API_KEY"
wire_api = "responses"
```

Codex 发送的 function、namespace 与 custom/apply_patch 工具会转换到 Cline Chat 上游；Cline 无法执行的 hosted web search 会被忽略。API Key 请放入 `CLINE2API_API_KEY` 环境变量，不要写进配置文件。

DeepSeek 的客户端非流式请求会在 Cline 上游使用 SSE，再聚合为客户端所需的非流式格式。聚合保留正文、并行工具调用、推理字段、usage 与结束原因；若只有推理而没有正文或工具调用，最多换一个账号重试一次，不会把隐藏推理冒充最终答案。

Usage 元数据按各协议的标准字段返回：OpenAI Chat 使用 `prompt_tokens` / `completion_tokens` / `total_tokens`，Responses 使用 `input_tokens` / `output_tokens` 及 cache/reasoning details，Anthropic 使用独立的 input、cache read、cache creation、output 与 thinking counters。由于 Cline 上游只在流结束时给出真实 usage，Anthropic `message_start` 的 input 是本地预估值，最终 `message_delta` 会返回真实明细；这可兼顾 Claude Code session log 的上下文显示与实时 TTFT。

Anthropic 流式转换会把 Cline/DeepSeek 的 `reasoning_content` 立即输出为标准 `thinking` block，并在后续工具调用历史中恢复为上游要求的 reasoning。reasoning、正文和工具调用全部为空时，会按策略尝试最多 3 个不同账号，并对返回空结果的“账号 + 模型”短暂冷却。全部失败后，只有估算输入接近模型上下文上限时才返回 `400 invalid_request_error` 并提示 `/compact`；短上下文返回 `529 overloaded_error`、`Retry-After` 和 `upstream_empty_response` 日志错误码，提示稍后重试或切换模型/渠道。相同请求的 SHA-256 指纹仍会短时熔断，避免重试风暴且不保存会话内容。

流式输出还会检测与 SSE 分块无关的周期性复读，以及模型对当前 system prompt 的大段复述；命中后分别以 `repetitive_output` 或 `prompt_echo` 中止流。system prompt 防泄漏检测只在单次请求内使用不可逆片段哈希，正常输出不增加首字缓冲。

`GET /v1/models` 在收到 `anthropic-version` 请求头时返回 Anthropic Models 标准结构，包括 `max_input_tokens` 与 `max_tokens`；OpenAI 请求仍保持其标准的基础 Model 对象，不添加非标准 context 字段。

### 3. 账号导出/导入（跨设备迁移）

- **导出**：账号管理页面点击「导出」按钮，下载 `cline-accounts-export.json`
- **导入**：在另一台设备上用「从文件导入」上传该文件
- 导出格式与批量导入格式完全兼容

### 4. System Prompt 覆盖

在 exe 同目录下创建 `override.md`，内容将替换所有客户端请求的系统提示词。

### 5. 监听地址与访问设置（局域网 / 多网卡）

默认只监听 `127.0.0.1`（仅本机可访问）。管理后台 **访问设置** 区可：

- **监听地址下拉选择**：`127.0.0.1`（仅本机）/ `0.0.0.0`（所有网卡）/ 本机检测到的 IP，保存后自动重启监听立即生效，选择会自动检测并展示本机 IP 列表
- **管理员登录**：邮箱与密码登录，24 小时数据库会话；支持多个管理员、修改密码与退出。首次部署通过 `CLINE_ADMIN_EMAIL`、`CLINE_ADMIN_PASSWORD` 初始化

命令行也可指定监听地址（优先级：环境变量 > 后台设置 > `127.0.0.1`）：

```bash
# CLI：监听所有网卡（局域网设备可通过本机 IP 访问）
./cline-proxy -host 0.0.0.0

# 指定某个网卡的 IP
./cline-proxy -host 192.168.1.100

# 环境变量方式（桌面端同样支持）
CLINE_PROXY_HOST=0.0.0.0 ./cline-proxy
```

管理后台与代理接口始终要求凭据。监听非回环地址时，请按部署网络设置防火墙及 HTTPS。

## 构建

### 桌面端（单文件跨平台）

Wails 依赖各平台原生 WebView，需在目标系统上构建：

```bash
# Windows（当前机器）
./desktop/build.sh

# macOS
xcode-select --install
./desktop/build.sh

# Linux
sudo apt install libgtk-3-dev libwebkit2gtk-4.1-dev
./desktop/build.sh
```

### CI 自动构建

推送 `v*` 标签触发 GitHub Actions 三平台自动构建并发布 Release：

```bash
git tag v1.0.0
git push origin v1.0.0
```

### 发布 zip（网盘分发推荐）

裸 exe 上传网盘后浏览器容易拦截，打包成 zip 可显著降低拦截率：

```bash
./desktop/build.sh && ./desktop/dist.sh
# 产物：desktop/dist/ccline2api-windows-amd64.zip
```

## 旧数据迁移

首次连接新的 PostgreSQL 数据库时，程序会从以下旧文件一次性导入数据。查找顺序为：

1. 可执行文件所在目录
2. 当前工作目录
3. 用户主目录 `~/.cline2api/`

| 文件 | 说明 |
|------|------|
| `.cline-accounts.json` | 账号池、自定义模型、默认模型、监听地址及旧版明文 API Key |
| `.cline-request-logs.json` | 请求日志 |
| `.cline-zen.json` | OpenCode Zen 配置、代理与压缩设置 |
| `.cline-providers.json` | 第三方渠道、API Key 与模型映射 |
| `.cline-config.json` | 代理轮询策略与上游请求头 |
| `.cline-proxy.json` | Cline/WorkOS 出口代理池与选择策略 |
| `.cline-credentials.json` | OAuth 凭据 |
| `override.md` | System Prompt 覆盖（可选，继续作为文件使用，不导入数据库）|

导入后 PostgreSQL 是上述 JSON 数据的运行时数据源。每项数据只导入一次；数据库已有对应记录时，不会再用旧文件覆盖。缺失、空文件或 JSON `null` 按默认值初始化；旧文件内容无效时启动会报错，修复文件后可重试。升级前请备份旧文件；导入后的文件可作为备份保留，但后续后台修改不会写回文件。`override.md` 仍可直接编辑。

> ⚠️ 账号、OAuth 凭据和第三方渠道文件包含密钥，不要放入发布包或提交到 Git。

Docker Compose 将旧文件挂载供首次导入，持续运行的数据保存在 `auth-db` 数据卷。首次部署预先创建不存在的挂载文件；已有文件请保留原内容：

```bash
touch .cline-accounts.json .cline-request-logs.json .cline-zen.json .cline-credentials.json override.md
chmod 600 .cline-accounts.json .cline-request-logs.json .cline-zen.json .cline-credentials.json
```

启动前通过 `.env` 设置数据库密码和初始管理员密码；Docker Compose 会自动读取该文件：

```bash
cp .env.example .env
chmod 600 .env
# 编辑 .env，填写 CLINE_DB_PASSWORD（URL 安全随机值）和 CLINE_ADMIN_PASSWORD（至少 12 位）
docker compose up -d --build
```

Docker 构建默认优先使用阿里云 Go 模块镜像，失败后切换七牛 `goproxy.cn`，并输出具体下载 URL。`GOPROXY` 使用 `|` 分隔，使超时或 HTTP 错误也能触发备用镜像；保留 Go 模块校验。需要切换为其他镜像时，可覆盖构建参数：

```bash
docker compose --progress=plain build --build-arg GOPROXY='https://mirrors.tencent.com/go/|https://goproxy.cn' cline-proxy
docker compose up -d
```

旧版管理密码哈希会迁移到 `CLINE_ADMIN_EMAIL` 指定的管理员（默认 `admin@local.test`）；已有明文 API Key 会以哈希形式导入数据库，旧凭据随后从账号文件移除。初始化后修改 `.env` 不会重置密码，请在后台修改。不要提交 `.env`。非 Docker 运行也需要设置 `CLINE_DATABASE_URL` 并连接 PostgreSQL；HTTPS 反向代理后将 `CLINE_ADMIN_SECURE_COOKIE=true`。

管理后台可单独配置 Cline 出口代理池。它只作用于 `*.cline.bot` 与 `*.workos.com`，不会把第三方 Provider 流量误送到 Cline 代理；代理密码不会回显到浏览器。导入后配置保存在 PostgreSQL。

## 可用模型

**默认动态同步**：程序启动时会自动从 Cline 官方推荐模型接口拉取最新模型（免费 / cline-pass / 推荐模型），
模型列表变化时管理后台会弹窗提示，也可在「设置 → 模型配置」点击「从 Cline 同步模型」手动刷新。

- 同步成功后，后台模型列表以**远程模型**为主（内置硬编码模型仅作为离线 fallback）
- 远程模型直接可用；后台「设置 → 模型配置」可添加/删除**自定义模型**（带 ✕ 删除按钮的是自定义项）
- 默认模型可在「设置 → 常规设置」下拉中设置；未设置时自动回退到第一个免费模型

> 内置 fallback 模型（离线/同步失败时兜底）：
> `z-ai/glm-5.3-flash`、`cline-free/longcat-2.0`、`cline-pass/glm-5.2`、`cline-pass/deepseek-v4-flash`、`cline-pass/qwen3.7-max`、`deepseek/deepseek-v4-flash`、`poolside/laguna-s-2.1:free`

## 项目结构

```
├── main.go              CLI 入口（go build .）
├── desktop_main.go      桌面端入口（go build -tags desktop）
├── proxy.go             HTTP 服务、API 路由、协议转换、SSE
├── admin.go             管理后台 REST API
├── admin_frontend.go    内嵌 React 构建产物
├── frontend/            React 管理后台源码与 dist 构建产物
├── auth.go              WorkOS OAuth + Token 刷新
├── pool.go              账号池管理、多位置数据查找
├── request_logs.go      请求日志
├── desktop/             桌面端构建脚本、文档、图标生成器
├── Dockerfile           Docker 构建
├── docker-compose.yml   Docker Compose
└── .github/workflows/   CI 三平台自动构建
```

## 技术栈

- [Go 1.25](https://go.dev) — 后端、代理、桌面壳
- [Wails v2](https://wails.io) — 跨平台桌面 WebView（单文件，非 Electron）
- [WebView2](https://developer.microsoft.com/microsoft-edge/webview2/) / [WebKit](https://webkit.org) / [WebKitGTK](https://webkitgtk.org) — 各平台原生 WebView

## 许可证

[MIT License](LICENSE) © 2026 [luawei1](https://github.com/luawei1)
