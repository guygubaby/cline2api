# `cline2api-workers` implementation analysis

Research date: 2026-08-28

## Scope and snapshots

This report compares:

- `guygubaby/cline2api-workers` `main` at commit [`f2bf874862035d38fcae4f41da6ce4115dfc61d7`](https://github.com/guygubaby/cline2api-workers/tree/f2bf874862035d38fcae4f41da6ce4115dfc61d7).
- This Go project at commit [`63d45ef35f594057a1669a98715a526aa530225d`](https://github.com/guygubaby/cline2api/tree/63d45ef35f594057a1669a98715a526aa530225d).

The question is whether the Workers implementation fixes, avoids, or merely provides ideas for the intermittent failure observed with `deepseek/deepseek-v4-flash`, non-streaming requests, `max_tokens=64`, and high reasoning effort:

```text
API 500: {"error":"empty response content","success":false}
```

## Bottom line

The Workers project contains a useful **workaround**, but it does not solve the underlying small-output-budget/high-reasoning mismatch.

For every non-streaming DeepSeek request, it forces the Cline upstream request to use SSE, aggregates the stream back into a non-streaming response, retries up to three times if no visible `content` is produced, and finally copies reasoning text into `content` as a last-resort fallback. The repository describes this specifically as its fix for non-streaming `500 empty response content`. See the [README explanation](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/README.md#L221-L230), [forced-stream branch](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L373-L407), and [aggregation/retry/fallback implementation](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L419-L539).

However, it still defaults `reasoning_effort` to `high` and preserves `max_tokens=64`. If the output budget is consumed by reasoning, forcing SSE changes the transport but not the budget. The code then masks that situation through retries or by exposing reasoning as answer content. It can also still return a fully empty last response after retries if the upstream provided neither content nor reasoning.

The Go project's current targeted handling is cleaner for the observed request shape: it no longer injects a default reasoning effort, and it sends `thinking: {"type":"disabled"}` only for small, non-streaming, tool-free DeepSeek V4 auxiliary requests when the client did not explicitly choose reasoning/thinking. See [`buildUpstreamBody`](https://github.com/guygubaby/cline2api/blob/63d45ef35f594057a1669a98715a526aa530225d/proxy.go#L537-L598). The Workers pattern is still valuable as a **bounded fallback** if upstream empty responses continue after that targeted fix.

## How the Workers proxy is built

### Routes and upstream

The Worker is a single JavaScript module with these public routes:

- `GET /v1/health`
- `GET /v1/models`
- `POST /v1/chat/completions`
- `POST /v1/messages`

The router has no OpenAI Responses endpoint and no Anthropic token-count endpoint. All generation requests ultimately go to `https://api.cline.bot/api/v1/chat/completions`; Anthropic is translated to/from this OpenAI-style upstream. See the [base URL and route table](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L22-L95).

Authentication has two layers:

1. Clients authenticate to the Worker with `Authorization: Bearer ...` or `x-api-key`; if `API_KEY` is absent, the code uses a built-in default key. See [`getApiKey`](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L782-L793).
2. The Worker exchanges a Cline refresh token at `/auth/refresh`, caches the access token, and calls the upstream with `Authorization: Bearer workos:<access-token>` plus fixed Cline-client fingerprint headers. See [refresh/caching](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L101-L166) and [upstream headers/fetch](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L210-L253).

### Account selection, throttling, and retries

`CLINE_REFRESH_TOKEN` may contain one token per line. The code creates an in-memory account array with per-account access-token expiry and cooldown state. It also maintains process-global `currentAccount` and `currentToken` variables. See [account state](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L24-L30) and [pool parsing](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L101-L120).

All upstream request starts are placed behind a global promise queue with an 800 ms minimum gap. The source says this avoids free-channel concurrency producing empty responses. It then treats `429` and a `5xx` containing `empty response content` as account cooldown/switch signals, with bounded retries. See [queue](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L255-L272) and [cooldown/retry](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L274-L347).

There are important limitations:

- A `pickAccount` round-robin function exists, but the actual `getAccessToken` path iterates from the first account each time and never calls it. The advertised round-robin behavior is therefore not implemented by the inspected code path. Compare [`pickAccount`](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L169-L180) with [`getAccessToken`](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L182-L208).
- A rotated refresh token is assigned only to the in-memory account object. On the next pool parse, it differs from the unchanged environment secret, which causes the pool to be rebuilt from the old token. This is an inference from the [change detection](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L101-L120) and [refresh-token assignment](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L151-L165). There is no KV/D1/durable-object persistence in the Worker.
- `currentAccount` is global. The queue protects the upstream `fetch` until response headers arrive, but response-body aggregation occurs afterward. Account-specific cooldown applied during aggregation can therefore be associated with mutable global state rather than a response-bound account. This is an inference from [queued fetch](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L295-L300) and [later empty-content cooldown](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L423-L473).

The Go project has more durable account behavior: rotated refresh tokens, cooldowns, usage counters, and the pool index are stored in `.cline-accounts.json`; model-specific cooldown and configurable selection strategies are implemented in [`pool.go`](https://github.com/guygubaby/cline2api/blob/63d45ef35f594057a1669a98715a526aa530225d/pool.go#L132-L147) and [`pickAccountForModel`](https://github.com/guygubaby/cline2api/blob/63d45ef35f594057a1669a98715a526aa530225d/pool.go#L157-L212).

## Request and response compatibility

### OpenAI

The Workers project implements only Chat Completions. It copies basic messages and several optional Chat Completions fields, including tools, while defaulting `max_tokens` to `128000` and reasoning effort to `high`. See [`handleChat`](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L353-L417).

For a client-streaming request, its response adapter unwraps an optional upstream `data` envelope and forwards Chat Completions SSE chunks. See [`streamResponse`](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L633-L688). However, before returning that response, `clineFetchWithRetry` clones it and awaits the entire clone as text in order to inspect the body for retry signals. For a long SSE response, this can delay real client forwarding until upstream generation is complete and can buffer the tee'd original body, defeating real-time TTFT behavior. See the [pre-read before the streaming early return](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L295-L339).

It has no `/v1/responses` route, Responses request conversion, or Responses SSE lifecycle. The Go project does expose `/v1/responses` in its [route registration](https://github.com/guygubaby/cline2api/blob/63d45ef35f594057a1669a98715a526aa530225d/proxy.go#L429-L458).

### Anthropic

Anthropic support in the Workers project is only a shallow compatibility layer:

- `system` arrays and non-string message content are serialized wholesale with `JSON.stringify`, rather than semantically converting text, image, `tool_use`, and `tool_result` content blocks.
- Client tools are mapped to OpenAI function tools, but `tool_choice`, multiple tool results, stop sequences, output configuration, cache controls, images/documents, and thinking blocks are not handled.
- The non-streaming converter returns one text block and does not convert OpenAI tool calls into Anthropic `tool_use` blocks.

These behaviors are visible in [`handleAnthropic`](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L545-L617) and [`openAItoAnthropic`](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L750-L765).

Its Anthropic streaming output is not a standard current Messages stream. It omits `message_start`, `content_block_start`, and `content_block_stop`; always uses content index `0`; never emits a tool-use block containing tool ID/name; always ends with `end_turn`; and reports zero output tokens. It also applies `JSON.stringify` to OpenAI's already-string `function.arguments` fragment before placing it in `partial_json`, which double-encodes tool arguments. See [`streamResponseAnthropic`](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L690-L748).

Anthropic's official stream lifecycle is `message_start`, then for each indexed block `content_block_start` → deltas → `content_block_stop`, followed by `message_delta` and `message_stop`. Tool inputs are unparsed JSON string fragments in `input_json_delta.partial_json`, while the starting `tool_use.input` is `{}`. See [Anthropic's official streaming documentation](https://platform.claude.com/docs/en/build-with-claude/streaming). The Go project now accumulates OpenAI tool argument fragments and emits an Anthropic tool block with the required start/delta/stop sequence in [`handleAnthropicStream`](https://github.com/guygubaby/cline2api/blob/63d45ef35f594057a1669a98715a526aa530225d/proxy.go#L1861-L2090).

Consequently, the Workers repository is not a source to copy for the goal of “both standard OpenAI and standard latest Anthropic APIs.” Its empty-response mitigation is the useful part; its Anthropic conversion is materially behind the Go implementation.

## Exact empty-response behavior

For OpenAI requests, the Worker creates this upstream shape:

```js
{
  max_tokens: params.max_tokens || params.max_completion_tokens || 128000,
  reasoning_effort: params.reasoning_effort || params.reasoningEffort || "high",
  // ...
}
```

For Anthropic, it also hardcodes `reasoning_effort: "high"`. See the [OpenAI request builder](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L373-L384) and [Anthropic request builder](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L575-L584).

For any client request with `stream=false` whose upstream model starts with `deepseek/`, it sets upstream `stream=true`. It then:

1. Aggregates `delta.content` and `delta.reasoning`.
2. Accepts the response only when visible content is non-empty.
3. If only reasoning exists, cools the current account for 30 seconds and retries.
4. If retries are exhausted, returns the last response; the aggregator has copied reasoning into `content` and marked `reasoning_used_as_content`.

The implementation is in [`nonStreamWithContentCheck`](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L419-L476) and [`streamToNonStream`](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L478-L539).

This likely avoids the exact upstream **non-streaming HTTP 500** when the same upstream accepts the streamed form. It does not guarantee a semantically valid answer:

- `max_tokens=64` and `reasoning_effort=high` remain unchanged.
- Returning hidden reasoning as visible answer content changes response semantics.
- If neither content nor reasoning is present, the final return may still contain an empty message.
- The aggregator ignores `tool_calls`, so using this forced-stream path for non-streaming DeepSeek tool requests can discard tool calls.
- Empty output caused by the request shape is treated as an account problem, so rotating accounts may add latency without changing the outcome.

There are no automated tests or captured upstream fixtures in the inspected repository tree, so the README's explanation that this is specifically a free-channel non-streaming limit is a project-author claim, not independently demonstrated by repository evidence.

## Override prompts, errors, logging, and deployment

The Workers project has no `override.md` feature. It directly passes OpenAI messages and inserts an Anthropic `system` message when supplied by the client. Therefore it cannot emit `override.md is empty`; it avoids that log only because the feature does not exist. See [OpenAI message forwarding](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L373-L387) and [Anthropic system handling](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L564-L580). The Go project now treats a missing or empty override as disabled and logs only a genuine read failure or an active non-empty override; see [`loadOverrideContent`](https://github.com/guygubaby/cline2api/blob/63d45ef35f594057a1669a98715a526aa530225d/proxy.go#L1203-L1216).

Workers logging consists mainly of `console.log` lines for account switching and retries. It has no persisted per-request token, cache, TTFT, throughput, or error history. Its Anthropic endpoint also returns the same generic OpenAI-shaped error wrapper used by the OpenAI endpoint. See [retry logs](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L317-L333) and [Anthropic error handling](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/worker.js#L594-L617). By contrast, the Go project persists request/account/protocol/usage/duration/TTFT/TPS/status/error fields in [`request_logs.go`](https://github.com/guygubaby/cline2api/blob/63d45ef35f594057a1669a98715a526aa530225d/request_logs.go#L20-L50).

Deployment is intentionally stateless and simple: the Cloudflare entry is `worker.js`, secrets are environment bindings, and no storage binding is declared in [`wrangler.toml`](https://github.com/guygubaby/cline2api-workers/blob/f2bf874862035d38fcae4f41da6ce4115dfc61d7/wrangler.toml). The same repository carries a largely duplicated Vercel Edge entry. This is easy to deploy, but process-global caches/cooldowns disappear across isolates or cold starts, and refresh-token rotation is not durably stored. The Go deployment instead bind-mounts account state, request logs, and optional override data; see its [`docker-compose.yml`](https://github.com/guygubaby/cline2api/blob/63d45ef35f594057a1669a98715a526aa530225d/docker-compose.yml#L1-L24).

## Ideas worth borrowing, without copying the implementation wholesale

1. **Use forced upstream streaming as a narrow fallback.** If a tool-free, non-streaming DeepSeek request still receives `empty response content` after the current small-request thinking fix, retry once with upstream SSE and aggregate it back to a non-streaming response.
2. **Validate visible output before returning success.** An upstream HTTP 200 with neither visible text nor tool calls should not automatically be logged as complete.
3. **Make retries bounded and response-bound.** Carry the selected account alongside each response; retry another eligible account only for account-specific signals such as `429`, authentication failure, or a verified account-specific empty response.
4. **Preserve the full response during aggregation.** Any Go fallback must accumulate text, reasoning, all tool-call IDs/names/argument fragments, finish reason, usage, and errors. The Workers aggregator is insufficient because it drops tool calls.
5. **Do not globally serialize or pre-read all streams without evidence.** A global 800 ms queue reduces throughput, while awaiting `resp.clone().text()` before returning an SSE response destroys real-time forwarding. If concurrency is proven to trigger empty responses, scope the limiter by account/model, preserve response ownership through complete consumption, and detect errors incrementally without consuming the stream first.
6. **Do not copy the default `reasoning_effort=high`.** It recreates the observed `max_tokens=64` risk. The current Go behavior—honor explicit client settings, otherwise disable thinking only for the narrow auxiliary-request shape—is the better primary mitigation.

## Recommendation

Keep the current Go fix as the first-line solution and observe production logs after deployment. If empty failures remain, use the Workers repository only as inspiration for a second-line path:

```text
normal non-stream request
  -> upstream empty-content error
  -> one forced-stream retry (tool-free requests first)
  -> aggregate and validate content/usage/finish reason
  -> return standard client-protocol response
```

Do not port the Worker proxy wholesale. It is smaller and deploys easily, but its current protocol coverage, Anthropic streaming, tool handling, account persistence, and observability are all weaker than this Go project.
