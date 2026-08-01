---
name: use-llm-provider-caching
description: Configure, review, troubleshoot, or verify caching in the llm-provider repository. Use for model-list cache settings, provider prompt caching, OpenRouter response caching, cache keys and breakpoints, cached-token accounting, stable conversation prefixes, or the external cache hit-rate regression tasks.
---

# Use LLM Provider Caching

Use repository behavior as the source of truth and provider documentation only
for provider-specific semantics. Keep cache changes scoped to the selected
provider and endpoint.

## Identify the cache layer

Choose the layer before editing configuration or requests:

- Model catalog cache: Gateway-owned cache for `GET /v1/models` and model
  detail. Configure `model_cache_refresh_interval` and
  `model_cache_refresh_timeout`. Do not treat it as an inference cache.
- Prompt cache: Provider-owned reuse of an exact, stable input prefix. The
  Gateway forwards cache fields and normalizes usage accounting.
- Response cache: OpenRouter-only replay of an identical request, controlled
  by `X-OpenRouter-Cache*` headers.

The Gateway does not cache generated Chat/Responses bodies or embedding
vectors itself.

## Follow the workflow

1. Inspect `llm-provider.json` or the supplied config and identify the provider
   `type`, `prefix`, and target endpoint.
2. Read the caching section in `README.md` and the matching case in
   `tests/cache_hitrate_test.go` before changing request shapes.
3. Put stable instructions, examples, tools, schemas, and reusable context at
   the beginning. Put user-specific input, timestamps, nonce values, and other
   changing data after the reusable prefix.
4. Reuse a stable cache or conversation key for requests that share the same
   prefix. Version the key when the stable prompt or tool schema changes; for
   example, `tenant-a:support-agent:v3`.
5. Send a warm-up request, then a probe request that retains the same prefix
   and appends only the next conversation turn.
6. Verify the result from normalized usage or response-cache headers. Do not
   claim a cache hit from latency alone.

## Select the provider recipe

| Provider | Request/config mechanism | Preserve between warm-up and probe | Verify |
|---|---|---|---|
| OpenAI-compatible GPT-5.6+ | `prompt_cache_key`, `prompt_cache_options`, content-block `prompt_cache_breakpoint` | Exact prefix, key, tool order, schemas | `usage.prompt_tokens_details.cached_tokens` and `cache_write_tokens` |
| Anthropic | `cache_control` on a stable system/message block | Marked content and everything before it | Normalized `cached_tokens` and `cache_write_tokens` |
| Codex App Server | Provider-managed transport cache; no Gateway cache headers | Latest `conversation_id`, prior messages, tool calls/results | Normalized cached/write token fields; thread continuity |
| Grok | Stable `X-Grok-Conv-Id` | Header, system message, tools, conversation prefix | `cached_tokens` |
| OpenRouter prompt cache | Stable `session_id` or `X-Session-Id`; provider cache fields when supported | Session key and prompt prefix | `cached_tokens` |
| OpenRouter response cache | `X-OpenRouter-Cache: true` and optional TTL | Entire request must be identical | First `X-OpenRouter-Cache-Status: MISS`, then `HIT` |

For GPT-5.6+ explicit caching, place
`prompt_cache_breakpoint: {"mode":"explicit"}` at the end of the stable
content and set `prompt_cache_options.mode` to `explicit` when only marked
prefixes should be eligible. Keep the exact field set conditional on backend
support; OpenAI-compatible does not imply that every server implements OpenAI
cache extensions.

Prefer request-scoped cache fields when one provider serves mixed endpoint
types. Provider `headers` and `body` are defaults applied broadly by the HTTP
provider, so endpoint-specific cache options can be inappropriate for
`/embeddings` or a backend that does not understand them.

## Verify and diagnose

Run the smallest external regression that matches the provider:

```powershell
task cache:codex
task cache:claude
task cache:grok
task cache:openrouter
task cache:openai-compatible
```

Use `task cache:all` only when the user accepts external API usage and cost.
Override the minimum ratio with `CACHE_MIN_HIT_RATE` or the provider-specific
variable documented in `README.md`.

When a probe reports zero cached tokens, check in this order:

1. Confirm the cacheable prefix meets the backend's minimum size.
2. Diff the rendered prefix, including content-block order, tools, schemas,
   images, and image detail.
3. Confirm the cache key is stable but not overloaded across unrelated
   prefixes.
4. Move changing values after the explicit breakpoint.
5. Confirm the backend accepts the chosen extension fields and exposes cache
   usage.
6. For OpenRouter response caching, confirm the second request is byte-shape
   equivalent and inspect `X-OpenRouter-Cache-Status`.
7. For Codex, confirm the caller retained the latest `conversation_id` and
   completed delegated tool results on the same thread.

Preserve the existing opt-in guards in `tests/cache_hitrate_test.go`; cache
regressions can call paid or local external backends and must remain skipped by
the default `go test ./...` run.
