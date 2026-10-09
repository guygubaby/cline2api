<div align="center">

# Cline2API

Cline API reverse proxy · multi-account rotation · dual protocol · desktop app

[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Platform](https://img.shields.io/badge/Platform-Windows%20%7C%20macOS%20%7C%20Linux-blue)](#build)

**🌏 中文: [中文 README](README.md)**

</div>

---

## Introduction

Cline2API is a reverse proxy for the Cline API with multi-account rotation, dual protocol support (OpenAI + Anthropic Messages API), API key authentication, and a bilingual admin panel (English/Chinese, auto-detected from your browser language with a manual toggle in the sidebar). Accounts, models, request logs, providers, settings, administrators, sessions, and API keys use PostgreSQL; Docker Compose starts it alongside the proxy.

**Built with**: Go (backend + proxy + desktop shell), React 19 / TypeScript (embedded admin frontend).

## Features

- **Dual protocol**: serves both `/v1/chat/completions` (OpenAI) and `/v1/messages` (Anthropic Messages API)
- **Multi-account rotation**: load-balances across Cline accounts (`round_robin` / `fill` / `random` / `least_latency`)
- **Bilingual admin panel**: `/admin/` manages accounts, API keys, models, headers and proxy settings; auto-follows your browser language, manually switchable in the sidebar
- **Dynamic model sync**: fetches the official Cline recommended-models API on startup (free / cline-pass / recommended); a popup notifies you when the model list changes, and you can also click "Sync Models from Cline" in the panel anytime
- **Custom provider management**: connect OpenAI Chat Completions or Anthropic Messages-compatible upstreams, sync/map their models, and load-balance channels that serve the same public model
- **Custom models**: add/remove model IDs manually and pick a default model (falls back to the first free model automatically)
- **API key auth**: protects proxy endpoints with `sk-{uuid}` keys; set expiry, lifetime token quotas in millions, allowed models, and per-key model aliases. Inspect details, edit, or revoke each key. Plaintext is shown only once
- **System Prompt override**: place an `override.md` next to the executable to replace the system prompt for all requests
- **Account import/export**: OAuth login, manual tokens, batch file import, and cross-device export
- **Automatic token renewal**: checks every minute and refreshes account tokens 5 minutes before expiry, with request-time and 401 retry fallbacks
- **Request logs**: per-request token usage, latency, TPS, and more
- **Desktop app**: single-file cross-platform app (Wails v2); requires a PostgreSQL connection at runtime

## Quick Start

### Option 1: Desktop app

Download the executable for your platform from [Releases](https://github.com/luawei1/cline2api/releases). Configure `CLINE_DATABASE_URL`, `CLINE_ADMIN_EMAIL`, and `CLINE_ADMIN_PASSWORD` and provide PostgreSQL before running it. Docker Compose configures these services automatically.

> On Windows, the SmartScreen "Windows protected your PC" warning is normal because no code-signing certificate is purchased. Click "More info → Run anyway".

| Platform | File | Notes |
|----------|------|-------|
| Windows x64 | `cline-proxy-desktop.exe` | WebView2 built into Win10/11 |
| macOS Apple Silicon | `cline-proxy-desktop-darwin-arm64` | Requires Xcode CLT |
| macOS Intel | `cline-proxy-desktop-darwin-amd64` | Requires Xcode CLT |
| Linux x64 | `cline-proxy-desktop-linux-amd64` | Requires GTK3 + WebKit2GTK |

### Option 2: Command line

```bash
cd frontend && pnpm install --frozen-lockfile && pnpm build && cd ..
go build -o cline-proxy .
./cline-proxy              # default port 3457
./cline-proxy -port 8080   # custom port
```

Set `CLINE_DATABASE_URL`, `CLINE_ADMIN_EMAIL`, and `CLINE_ADMIN_PASSWORD` for first-time setup before starting. Then open http://127.0.0.1:3457/admin/ for the admin panel.

### Option 3: Docker

```bash
cp .env.example .env  # Set CLINE_DB_PASSWORD and CLINE_ADMIN_PASSWORD
docker compose up -d      # build and start
docker compose logs -f    # view logs
docker compose down       # stop
```

The container listens on `0.0.0.0:3457`, mapped to host port `4000` by Compose. The admin panel requires email and password login; configure HTTPS for public deployments.

## Usage Guide

### 1. Add a Cline account

In the admin panel, go to **Import**:

- **OAuth browser login**: starts the device-authorization flow; complete login in your system browser (works with an already-logged-in Cline browser)
- **Manual token**: paste an existing refreshToken
- **Batch import**: upload a JSON file or paste text (one token per line, or a JSON array `[{refreshToken, email}]`)

### 2. Configure your client

```
Base URL: http://127.0.0.1:3457/v1
API Key:  <sk-{uuid} generated in Settings → API Keys>
Model:    <model from the synced list, e.g. stealth/ox-alpha>
```

Both OpenAI and Anthropic API formats are supported.

### Custom providers

The **Providers** page in the admin panel accepts a provider name, protocol, Base URL, and API key. You can sync models from the upstream `/models` endpoint or add an upstream model ID manually, then assign each one a public model ID. When multiple enabled providers map to the same public model ID, the proxy can use round-robin, fill, random, or least-latency selection. It learns first-event latency per model/channel with an EWMA and briefly cools a channel when it is anomalously slower than a known alternative. A 429, authentication failure, network error, or 5xx also triggers cooldown, and the proxy switches to at most one alternate channel before a response begins. Loopback, private, link-local, and cloud-metadata addresses are blocked by default; enable private-network access only for an upstream on a trusted internal network.

The Settings page also lets you choose the default `low`, `medium`, or `high` effort for Anthropic thinking requests. An explicit request effort always wins, and `thinking.type=disabled` is no longer converted into high reasoning. Request logs show upstream first event, thinking first token, visible first token, and cache-hit state separately.

For non-streaming lightweight auxiliary requests to DeepSeek V4 and GLM 5.3 (at most 256 output tokens, no tools, and no explicit thinking/reasoning request), the proxy disables thinking so title or summary tasks do not spend their entire output budget on reasoning. Large tasks, client-streamed requests, and explicit reasoning requests are unchanged.

Custom providers can use OpenAI Chat Completions (`/chat/completions`), OpenAI Responses (`/responses`), or Anthropic Messages (`/messages`) upstream. Responses are normalized internally, so downstream clients may continue using Chat Completions, Responses, or Anthropic Messages.

If a Responses upstream requires `stream: true`, enable "Force streaming for Responses upstream" on that provider. Non-streaming client requests are aggregated after the upstream stream completes; other providers are unaffected.

### API protocol compatibility

| Protocol | Standard endpoint | Supported core calls |
|----------|-------------------|----------------------|
| OpenAI Chat Completions | `POST /v1/chat/completions` | Standard Chat Completion / Chunk objects, text, multimodal images, function tools, parallel tool calls, `stream_options.include_usage` |
| OpenAI Responses | `POST /v1/responses` | `input` / `instructions`, multimodal images, `reasoning.effort`, `text.format`, custom functions and `function_call_output`, standard Responses SSE lifecycle |
| Anthropic Messages | `POST /v1/messages` | System/content blocks, base64/URL images, `stop_sequences`, `output_config`, client tools, multiple `tool_result` blocks, standard Messages SSE lifecycle |
| Anthropic Token Count | `POST /v1/messages/count_tokens` | Standard `{ "input_tokens": number }` shape using a local approximation |

Authentication accepts both OpenAI's `Authorization: Bearer <key>` and Anthropic's `x-api-key: <key>`. Anthropic SDKs may send the usual `anthropic-version` and `anthropic-beta` headers.

Per-key model choices come from the current public `/v1/models` list. Leave models unselected or click "All models (all)" for unrestricted model access; the key's `/v1/models` list automatically follows global visibility settings. Selecting individual models creates a fixed allowlist. When an alias is set, that key's model list shows the alias; both the alias and original model ID route to the original model. Quotas count lifetime total tokens (1M = 1,000,000 tokens), using upstream usage when available and estimated input for completed requests without usage. The metered-request count includes only calls with actual or estimated usage. New generation requests are rejected once the quota is reached; concurrent requests already in flight may take the final total slightly over the limit. Legacy keys remain unrestricted until edited.

> The upstream is a Chat Completions service, so features requiring vendor-side state or hosted execution cannot be faithfully emulated. OpenAI `background`, `previous_response_id`, `conversation`, and hosted tools, plus Anthropic server tools, containers, and Skills, return a standard error instead of being silently dropped. For stateless multi-turn Responses calls, send previous output items back in the next `input`.

Responses streaming maps upstream `reasoning_content` to standard reasoning items and reasoning-summary deltas, then restores reasoning history from subsequent input items. Before committing client SSE, the proxy checks stream initialization and retries at most once with another account after a recoverable 429, 5xx, empty stream, first-event timeout, or early EOF. Completion requires `[DONE]` or an explicit `finish_reason`; `length` / `content_filter` produce `response.incomplete`, while EOF without a terminal event produces `response.failed`.

Chat Completions exposes only standard OpenAI fields; upstream-only `reasoning_content`, billing fields, and provider metadata are removed. Clients that need streamed reasoning should use Responses. With `stream_options: {"include_usage": true}`, ordinary chunks carry `usage: null` and a final `choices: []` usage chunk is emitted immediately before `[DONE]`. Chat and Responses share a 30-second first-event timeout, short model-level account cooldown, and at most one alternate-account retry.

You may also request the virtual `free` model. After a successful catalog sync, the proxy uses only free models that the upstream still advertises, prioritizing `z-ai/glm-5.3-flash`, `deepseek/deepseek-v4-flash`, and `cline-free/longcat-2.0`, then appending newly advertised free models in upstream order. Offline mode falls back to the three built-in models. Each model is limited to two non-cooling accounts, preventing unbounded retries during a free-pool outage. Request logs store the effective model.

Multi-user isolation: every Cline upstream attempt gets an independent 128-bit cryptographically random session ID, used consistently for both `X-Task-ID` and body `session_id`; a 401 replay keeps the same ID, while a new attempt never reuses it. Zen compaction state, client cache keys, and user identifiers are namespaced by a non-reversible tenant digest of the downstream API key; proxy requests require a valid API key. Audit logs contain only random request/task IDs, masked key previews, and per-process-keyed HMAC-SHA256 values, never prompt or response text. Give each person/application a distinct API key. The account pool remains global to an instance, so sensitive multi-tenant deployments should also use separate instances/account pools.

Current Codex custom providers use the Responses protocol. The included `codex-models.json` supplies 1M-context, reasoning, shell, and apply_patch metadata for DeepSeek and GLM, avoiding Codex's temporary unknown-model error. Example `~/.codex/config.toml`:

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

Codex function, namespace, and custom/apply_patch tools are translated to the Cline Chat upstream. Hosted web search is ignored because Cline cannot execute it. Put the API key in the `CLINE2API_API_KEY` environment variable rather than the config file.

For client-side non-streaming DeepSeek requests, the Cline upstream is called with SSE and then aggregated back into the requested non-streaming protocol. Aggregation preserves visible text, parallel tool calls, reasoning fields, usage, and finish reason. A reasoning-only result retries at most once with another account and is never exposed as the final answer.

Usage metadata follows each protocol's standard fields: OpenAI Chat uses `prompt_tokens` / `completion_tokens` / `total_tokens`; Responses uses `input_tokens` / `output_tokens` plus cache and reasoning details; Anthropic reports fresh input, cache read, cache creation, output, and thinking counters separately. Because the Cline upstream only reports exact usage at the end of a stream, Anthropic `message_start` carries a local input estimate while the final `message_delta` carries the exact breakdown. This preserves real-time TTFT while allowing Claude Code session logs to show context usage.

Anthropic streaming immediately maps Cline/DeepSeek `reasoning_content` to standard `thinking` blocks and restores it to the upstream-required reasoning history after tool calls. When reasoning, visible text, and tool calls are all absent, the proxy tries up to three distinct accounts according to the configured strategy and briefly cools each empty “account + model” route. If every attempt is empty, only input estimated near the model context limit gets a `400 invalid_request_error` with `/compact` guidance. Short-context failures return `529 overloaded_error`, `Retry-After`, and the `upstream_empty_response` request-log code, suggesting a later retry or another model/channel. The opaque SHA-256 request fingerprint remains briefly circuit-broken without storing conversation content.

Streaming output also detects chunk-boundary-independent repetition loops and substantial regurgitation of the current system prompt, terminating them with `repetitive_output` or `prompt_echo`. Prompt-leak detection uses only irreversible shingle hashes scoped to the live request and adds no first-token buffering to normal output.

`GET /v1/models` returns the Anthropic Models shape, including `max_input_tokens` and `max_tokens`, when the request includes `anthropic-version`. OpenAI requests keep the standard basic Model object without non-standard context fields.

### 3. Account export/import (device migration)

- **Export**: click "Export" on the Accounts page to download `cline-accounts-export.json`
- **Import**: upload that file via "Import from File" on another device
- The export format is fully compatible with the batch-import format

### 4. System Prompt override

Create `override.md` next to the executable; its content replaces the system prompt for all client requests.

### 5. Listen address & access settings (LAN / multi-NIC)

By default the proxy listens on `127.0.0.1` (local only). The Settings pages let you:

- **General**: choose a listen address (`127.0.0.1`, `0.0.0.0`, or a detected local IP); saving restarts the listener immediately
- **Admins & Security**: manage email/password administrators and 24-hour database sessions; bootstrap with `CLINE_ADMIN_EMAIL` and `CLINE_ADMIN_PASSWORD`

You can also set the listen address on the command line (priority: env var > panel setting > `127.0.0.1`):

```bash
# CLI: listen on all interfaces (LAN devices can reach it via your local IP)
./cline-proxy -host 0.0.0.0

# Bind to a specific NIC
./cline-proxy -host 192.168.1.100

# Environment variable (works for the desktop app too)
CLINE_PROXY_HOST=0.0.0.0 ./cline-proxy
```

The admin and proxy endpoints always require credentials. Configure firewall rules and HTTPS for non-loopback deployments.

## Build

### Desktop app (single-file, cross-platform)

Wails requires the native WebView of each platform — build on each target system:

```bash
# Windows (this machine)
./desktop/build.sh

# macOS
xcode-select --install
./desktop/build.sh

# Linux
sudo apt install libgtk-3-dev libwebkit2gtk-4.1-dev
./desktop/build.sh
```

### CI builds

Pushing a `v*` tag triggers GitHub Actions to build and release all three platforms:

```bash
git tag v1.0.0
git push origin v1.0.0
```

### Release zip (recommended for cloud-drive sharing)

Browsers are less likely to block a zip than a bare exe:

```bash
./desktop/build.sh && ./desktop/dist.sh
# Produces desktop/dist/ccline2api-windows-amd64.zip
```

## Legacy data migration

On first connection to a new PostgreSQL database, the proxy imports the following legacy files once. It looks in the executable directory, then the working directory, then `~/.cline2api/`.

| File | Purpose |
|------|---------|
| `.cline-accounts.json` | Account pool, custom/default models, listen address, and legacy plaintext API keys |
| `.cline-request-logs.json` | Request logs |
| `.cline-zen.json` | OpenCode Zen, proxy, and compaction settings |
| `.cline-providers.json` | Custom providers, API keys, and model mappings |
| `.cline-config.json` | Proxy rotation strategy and upstream request headers |
| `.cline-proxy.json` | Cline/WorkOS egress proxy pool and selection strategy |
| `.cline-credentials.json` | OAuth credentials |
| `override.md` | System Prompt override (optional; remains a file and is not imported) |

PostgreSQL becomes the runtime source of truth after import. Each dataset is imported only if its database row is absent, so later restarts never overwrite it from a legacy file. Missing, empty, or JSON `null` files initialize defaults. Invalid legacy data prevents startup; fix the file and retry. Back up the legacy files before upgrading. They can remain as backups after import, but admin changes no longer write to them. You can still edit `override.md` directly.

> ⚠️ Account, OAuth credential, and custom-provider files contain secrets. Never ship or commit them.

Docker Compose mounts the legacy files for first import; ongoing data lives in the `auth-db` volume. Create missing bind-mounted files before deployment, without replacing existing files:

```bash
touch .cline-accounts.json .cline-request-logs.json .cline-zen.json .cline-credentials.json override.md
chmod 600 .cline-accounts.json .cline-request-logs.json .cline-zen.json .cline-credentials.json
```

Set the database password and initial admin password in `.env` before startup; Docker Compose loads this file automatically:

```bash
cp .env.example .env
chmod 600 .env
# Edit .env: set CLINE_DB_PASSWORD to a URL-safe random value and CLINE_ADMIN_PASSWORD to at least 12 characters.
docker compose up -d --build
```

An existing admin password hash is migrated to the administrator named by `CLINE_ADMIN_EMAIL` (default `admin@local.test`). Existing plaintext API keys are imported as hashes; the legacy credentials are then removed from the account file. Changing `.env` after initialization does not reset a password; use the admin panel. Never commit `.env`. Non-Docker runs also require `CLINE_DATABASE_URL` and PostgreSQL. Set `CLINE_ADMIN_SECURE_COOKIE=true` behind an HTTPS reverse proxy.

The admin panel can configure a separate Cline egress proxy pool. It applies only to `*.cline.bot` and `*.workos.com`, so custom-provider traffic is not accidentally routed through it, and proxy passwords are never returned to the browser. After import, PostgreSQL stores this configuration.

## Available Models

**Synced dynamically by default**: on startup the proxy fetches the official Cline recommended-models endpoint (free / cline-pass / recommended). When the list changes, the admin panel shows a popup; you can also hit "Sync Models from Cline" in Settings → Models at any time.

- After a successful sync, the panel lists the **remote models** (hardcoded built-ins remain only as an offline fallback)
- Remote models are ready to use; add/remove **custom models** in Settings → Models (items with an ✕ delete button are custom)
- Set the default model in Settings → General; if unset, it falls back to the first free model

> Built-in fallback models (used offline / when sync fails):
> `z-ai/glm-5.3-flash`, `cline-free/longcat-2.0`, `cline-pass/glm-5.2`, `cline-pass/deepseek-v4-flash`, `cline-pass/qwen3.7-max`, `deepseek/deepseek-v4-flash`, `poolside/laguna-s-2.1:free`

## Project Structure

```
├── main.go              CLI entry (go build .)
├── desktop_main.go      Desktop entry (go build -tags desktop)
├── proxy.go             HTTP server, API routes, protocol conversion, SSE
├── admin.go             Admin REST API
├── admin_frontend.go    Serves the embedded React build
├── frontend/            React admin source and bundled dist/
├── models_sync.go       Cline model sync (startup + manual)
├── i18n.go              Bilingual (zh/en) messages for the admin API
├── auth.go              WorkOS OAuth + token refresh
├── pool.go              Account pool management, multi-location lookup
├── request_logs.go      Request logs
├── desktop/             Desktop build scripts, docs, icon generator
├── Dockerfile           Docker build
├── docker-compose.yml   Docker Compose
└── .github/workflows/   CI builds for all three platforms
```

## Tech Stack

- [Go 1.25](https://go.dev) — backend, proxy, desktop shell
- [Wails v2](https://wails.io) — cross-platform desktop WebView (single file, not Electron)
- [WebView2](https://developer.microsoft.com/microsoft-edge/webview2/) / [WebKit](https://webkit.org) / [WebKitGTK](https://webkitgtk.org) — native WebView per platform

## License

[MIT License](LICENSE) © 2026 [luawei1](https://github.com/luawei1)
