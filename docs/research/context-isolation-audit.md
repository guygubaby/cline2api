# Context isolation audit

Date: 2026-09-01
Scope: Claude Code -> `cline2api` -> Cline Chat Completions -> `deepseek/deepseek-v4-flash`
Original audit method: local session evidence, repository source/tests, and first-party Anthropic/Cline documentation. The first audit pass changed no production code.

> Remediation update: the isolation fixes recommended below were implemented after the original audit. Production remains exposed until the updated binary is deployed and downstream keys are rotated.

## Executive conclusion

The incident is real: a new local Claude Code session whose human input was only `hi` received approximately 9,511 output tokens of an unrelated Whisper/offline-flight task in the model's thinking, then visibly continued that task. This establishes **unintended foreign-context-like output**. It does not establish who originally supplied that context, so it is not yet possible to claim conclusively that it belonged to another customer.

The local `--continue`/resume explanation is strongly ruled out for this incident. The exact unrelated text does not exist in another local WLSA session, project instruction, or Claude memory that was searched. The affected session is a normal, non-sidechain session with a new UUID, and the local `cc` wrapper runs `claude --dangerously-skip-permissions`, not `claude --continue`.

The exact server-side root cause remains unverified. The strongest remaining boundary is Cline/DeepSeek upstream request state or cache isolation. The proxy also has a concrete isolation defect that could enable such contamination: it calls a millisecond timestamp a unique task/session ID and sends it in both undocumented `session_id` and Cline's `X-Task-ID`. Under concurrency these identifiers collide.

Treat this as a **high-severity privacy incident** until the weak identifier is repaired and the upstream is cleared or disproved by traceable isolation tests.

## Confirmed evidence

### 1. The unrelated context arrived in the model response

Local artifact (kept private because it contains the leaked text):

`~/.claude/projects/-Users-bryceloskie-c-wlsa/d0541aef-f71c-4f44-8894-769ca9ef9f2c.jsonl`

- Lines 4-8 contain startup `/model` events followed by the human message `hi`.
- Lines 12-13 are a local `/skills` command and `No changes`; they add no task prompt.
- Line 14 is the assistant response. It is `isSidechain: false`, uses `deepseek/deepseek-v4-flash`, and contains 38,847 characters / 9,511 output tokens of unrelated task material in a thinking block.
- The recorded usage reports `cache_read_input_tokens: 0`. That makes an ordinary, correctly reported cache hit less likely, but cannot rule out undisclosed Cline/DeepSeek state or cache behavior behind the custom gateway.
- Line 15 visibly continues that task with the plane-related statement seen in the supplied screenshot/transcript.
- Searches for two distinctive phrases across the other local Claude project histories, WLSA tree, and local memory/instruction locations found no other source; the only match was this returned assistant block.

This proves the content was present in what the gateway/model returned. It does not, by itself, identify whether Cline, DeepSeek, or another layer inserted it.

### 2. Official APIs are documented as history-in-request, not hidden conversation state

Anthropic documents Messages as stateless: prior turns are supplied in the `messages` parameter for each request. Claude Code likewise says the model remembers nothing between API requests and that the client re-sends the system prompt, project context, messages, and tool results on every turn. [Anthropic Messages API](https://platform.claude.com/docs/en/api/messages/create), [Claude Code prompt caching](https://code.claude.com/docs/en/prompt-caching#how-the-cache-is-organized)

Cline's official Chat Completions reference lists `messages` as the conversation and explicitly says to include previous messages to maintain context. Its public request schema does not document a body `session_id`. [Cline Chat Completions source](https://github.com/cline/cline/blob/main/docs/api/chat-completions.mdx#L21-L60)

The proxy's Responses facade also rejects `previous_response_id` and `conversation`, requiring stateless input items instead. [responses.go](https://github.com/guygubaby/cline2api/blob/44e93a9/responses.go#L15-L32)

### 3. The proxy forwards client history but creates a weak upstream identity

For Cline requests, the proxy copies the supplied `messages` into the upstream body. It does not deliberately append a prior request's message history. However, it generates the upstream session identifier as:

```go
sessionID := fmt.Sprintf("sess_%d", time.Now().UnixMilli())
```

It then puts the same value in both body `session_id` and header `X-Task-ID`. [proxy.go](https://github.com/guygubaby/cline2api/blob/44e93a9/proxy.go#L651-L719)

Cline defines `X-Task-ID` as a **unique task identifier** used internally by the extension. [Cline authentication source](https://github.com/cline/cline/blob/main/docs/api/authentication.mdx#L109-L117)

`UnixMilli()` has only millisecond resolution and no random or monotonic component. A local 10,000-call tight-loop probe produced only three distinct values. Concurrent requests handled in the same millisecond therefore receive the same purportedly unique ID. This is a confirmed proxy defect. Cline does not publicly document whether this header participates in inference state or cache keys, so its causal role in this incident is plausible, not confirmed.

### 4. Account rotation does not intentionally merge messages

The account scheduler chooses an eligible account per request, and a bounded initialization retry can select another account. The retry sends the same request parameters; no other client's message array is read or merged. [pool.go](https://github.com/guygubaby/cline2api/blob/44e93a9/pool.go#L325-L396)

Nevertheless, every proxy user's prompts are transmitted through credentials from the same shared Cline account pool. The proxy's public API keys are authentication strings, not first-class tenant records, and requests are not pinned to an account or isolated namespace. Account rotation therefore increases the number of upstream account/cache boundaries a conversation can touch and makes incident attribution harder. This is an architectural privacy/observability risk, but it is not proof that rotation caused the returned context.

### 5. Prompt caching cannot be assumed safe behind this custom gateway

For Anthropic's own API, prompt caching is exact-prefix based; Claude Code says the cache is server-side and keyed by request prefix, model, and effort. But Claude Code explicitly states that for a custom `ANTHROPIC_BASE_URL` or LLM gateway, cache location and behavior depend on where the gateway forwards requests and how it handles caching markers. [Claude Code prompt caching](https://code.claude.com/docs/en/prompt-caching#where-the-cache-lives)

Cline's public documentation exposes cached-token accounting but does not disclose its cache key, tenant/account scope, or whether `X-Task-ID`/`session_id` participates. [Cline cached token documentation](https://github.com/cline/cline/blob/main/docs/api/chat-completions.mdx#L85-L107)

Therefore Anthropic's first-party cache isolation guarantees cannot be extended to this Cline/DeepSeek route without upstream evidence.

### 6. Local proxy state exists, but it does not explain this DeepSeek/Cline incident

The proxy keeps in-memory compaction summaries for up to 24 hours, keyed only by a client-supplied `x-opencode-session` or body `session_id`. If two Zen clients use the same value while auto-compaction is enabled, one can load the other's stored summary/recent text. [compact.go](https://github.com/guygubaby/cline2api/blob/44e93a9/compact.go#L59-L87), [compact.go](https://github.com/guygubaby/cline2api/blob/44e93a9/compact.go#L276-L413)

This is a separate real isolation risk because the key is not namespaced by authenticated client/API-key identity. It is not the likely cause here: `maybeCompact` is invoked only on Zen routes in the current handlers, while the affected model used the Cline route. Empty session IDs also do not persist compaction state.

The semantic-empty circuit stores only a SHA-256 request fingerprint and diagnostics, not prompt or response content. Request logs store usage/account diagnostics, not message bodies. Neither provides a content source for this event.

The global `override.md` feature could deliberately replace every client's system prompt if populated, but no `override.md` was present in the audited local checkout. [proxy.go](https://github.com/guygubaby/cline2api/blob/44e93a9/proxy.go#L418-L430), [proxy.go](https://github.com/guygubaby/cline2api/blob/44e93a9/proxy.go#L1863-L1870)

## Reproduction status

A bounded production canary sent 16 isolated requests with distinct random phrases:

- 9 returned HTTP 200.
- 7 returned HTTP 502.
- No response contained another canary's phrase.

The incident did not reproduce in this small sample. That does not invalidate the captured incident; a rare collision or stale upstream cache/state fault is expected to be intermittent. The high 502 rate is a separate reliability signal and makes this sample weaker, not exculpatory.

## Root-cause ranking

| Hypothesis | Assessment | Evidence |
|---|---|---|
| Cline/DeepSeek upstream returned state/cache content from a different request | Most plausible, unverified | Foreign content is in the model response; documented APIs are otherwise stateless; exact upstream cache/state implementation is undisclosed. |
| Collision of proxy-generated `X-Task-ID` / `session_id` enabled upstream association | Concrete defect; causal link unverified | `sess_<UnixMilli>` collides under ordinary concurrency and violates Cline's stated uniqueness requirement. |
| DeepSeek hallucinated or regurgitated memorized training text without a live cross-request leak | Possible, lower confidence | A model can invent unrelated content, but the unusually long, coherent, self-correcting project prompt makes this less persuasive than upstream request-state contamination. |
| Claude Code resumed a local session | Strongly ruled out for this incident | New session UUID; no `--continue`; only `hi` before response; distinctive text absent from other searched local histories. |
| Claude Code project memory / `CLAUDE.md` supplied the task | Strongly ruled out for this incident | Distinctive text absent from searched local project/memory/instruction files and persisted session attachments. |
| Proxy Zen auto-compaction mixed summaries | Real separate risk; ruled out for this incident | Affected request used the Cline DeepSeek route; compaction is called on Zen paths only. |
| Account rotation directly concatenated histories | Unsupported by current source | Scheduler changes credentials but request history remains the caller's `messages`. |

## Recommended remediation

### Immediate containment

1. Treat `deepseek/deepseek-v4-flash` through Cline as potentially unsafe for sensitive code until an isolated rerun confirms the fix. Prefer a provider whose tenant/cache isolation is documented during the incident window.
2. Preserve the affected local JSONL, server request log, container log, account selected, and timestamps. Do not paste the leaked task text into public issues; share it only through a private security channel.
3. Ask Cline to trace the affected request/account and explain how `X-Task-ID`, body `session_id`, prompt cache, and downstream provider request IDs are scoped. Provide hashes or redacted excerpts rather than secrets/full prompts.

### Proxy fixes

1. Replace `sess_<UnixMilli>` with at least 128 bits from a cryptographically secure random source (UUID v4 or equivalent). Keep a separate stable internal request ID and a unique ID per upstream attempt. Never rely on timestamps for uniqueness.
2. Stop sending undocumented body `session_id` unless an upstream contract/test proves it is required. Continue sending a genuinely unique `X-Task-ID` because Cline documents it. Roll this out behind a compatibility test because upstream behavior is not public.
3. Namespace all server-held conversational state by an authenticated tenant/key identity plus route/model and client session. Do not key `compactStates` by a raw user-controlled header alone. If different people share one API key, issue distinct per-user keys before claiming tenant isolation.
4. Do not use account identity, model alone, or a shared prompt-cache key as a conversation key. Preserve explicit client history across account retries, but keep each upstream attempt identifier unique.

### Detection and verification

1. Add an automated concurrent isolation test: many requests, each with a random canary in the system and user messages, assert that no output contains another request's canary. Exercise normal calls, timeout retry, account rotation, Chat, Anthropic Messages, Responses, and Zen compaction.
2. Log only non-reversible identifiers: internal request ID, upstream task ID, selected account ID, retry attempt, model, and SHA-256 hashes of canonical request-history and response prefixes. Never log prompt text, API keys, access tokens, or refresh tokens.
3. Add an operator-visible security alert if two in-flight attempts ever share an upstream task ID, or if a response matches another live request's canary during synthetic monitoring.
4. Repeat the production canary after the ID fix with all requests completing successfully. A passing sample reduces risk but cannot prove Cline/DeepSeek internals; upstream confirmation remains necessary.

## Remediation implemented

The proxy now:

- generates every Cline `X-Task-ID`, proxy request ID, Anthropic message ID, and Responses object ID with at least 128 bits from `crypto/rand`;
- sends one cryptographically random per-attempt identifier consistently as Cline `X-Task-ID` and body `session_id` (revised by the follow-up finding below);
- uses a new upstream task ID for each account/model attempt while retaining one proxy request ID for correlation;
- namespaces Zen compaction state and client-supplied `prompt_cache_key`, `user`, and `safety_identifier` values by a non-reversible API-key tenant scope;
- disables cross-request shared state when no API key is configured, because no stable tenant boundary exists;
- records request-history and response-prefix HMAC-SHA256 values using a per-process random key, never prompt/response text;
- records the final upstream task ID and actual attempt count in request logs;
- includes concurrency regressions for 10,000 random task IDs, cross-key compaction state, cache-key namespacing, response-log secrecy, and 401 request replay.

The first remediation build accepted Chat Completions, Responses, and Anthropic Messages requests without body `session_id`. A post-fix 16-request concurrent canary produced 15 HTTP 200 responses and one HTTP 502; all 15 successful responses returned only their own canary. The 16 proxy request IDs and 16 final upstream task IDs were unique, including six account retries. The later production symptom led to the compatibility revision documented below.

Residual risks remain:

- callers that share one downstream API key intentionally share one tenant namespace; issue a distinct key per person/application;
- all tenants still use credentials from the global Cline account pool, so only Cline/DeepSeek can confirm their account-level cache isolation;
- the canary did not reproduce the leak and cannot prove an opaque upstream is safe;
- existing production processes and historical containers keep the old behavior until rebuilt and restarted.

## Follow-up compatibility finding (2026-09-01)

After deployment, a fresh Claude Code request containing only a greeting intermittently received unrelated system-like text. A comparison with `upstream/main` found one isolation-relevant behavioral difference: upstream sends the same per-request identifier in both `X-Task-ID` and body `session_id`, while the first remediation removed the body field. The Cline public schema still does not document `session_id`, so its precise server-side role remains unverified; however, omitting it may make the opaque upstream fall back to account-level/default request state or cache behavior.

The compatibility decision was therefore revised conservatively: the proxy now restores body `session_id`, but never restores the collision-prone timestamp generator. Each logical request gets a cryptographically random 128-bit value shared by `X-Task-ID` and body `session_id`; a transport/auth replay keeps that value, and every new request gets a different one. Integration tests assert header/body agreement, replay stability, uniqueness across requests, and per-request message isolation.

## Final assessment

There is enough evidence to say **the user observed real unintended context contamination**, not merely an ordinary Claude Code resume. There is not enough evidence to name the original owner of the context or definitively attribute the bug to this proxy versus Cline/DeepSeek. The known proxy isolation defects are now repaired locally, but production deployment and upstream tracing remain mandatory. Until Cline/DeepSeek confirms account/cache isolation and the deployed canary remains clean, the route should not be considered suitable for sensitive multi-user workloads.
