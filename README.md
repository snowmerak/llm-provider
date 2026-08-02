# llm-provider

`llm-provider` exposes multiple LLM backends through a single Go interface and
an OpenAI-compatible HTTP gateway.

It currently supports:

- OpenAI-compatible APIs, including local servers, OpenRouter, and xAI/Grok
- Anthropic Claude through the native Messages API
- a local `codex app-server --listen stdio://` process
- model listing and model metadata lookup
- regular and SSE-streaming Chat Completions
- native or adapted Responses API requests, including SSE streaming
- embeddings through OpenAI-compatible backends
- function tool calling and tool-result round trips
- stateful Codex conversations through `conversation_id`
- provider-specific request headers, response headers, and JSON extensions
- normalized prompt-cache usage across providers

## Gateway

The Gateway creates only the providers marked as `enabled` in the configuration
file. The first segment of an external model ID is used as the provider prefix.
The prefix is removed before the request is sent to the backend.

| Gateway model ID | Backend | Model sent to the backend |
|---|---|---|
| `codex/gpt-5.6-luna` | `codex app-server` | `gpt-5.6-luna` |
| `claude/claude-sonnet-5` | Anthropic Messages API | `claude-sonnet-5` |
| `openrouter/anthropic/claude-sonnet-4` | OpenRouter | `anthropic/claude-sonnet-4` |
| `local/gpt-5.6-luna` | `http://macmini:11888/v1` | `gpt-5.6-luna` |
| `lmstudio/text-embedding-model` | `http://localhost:1234/v1` | `text-embedding-model` |
| `grok/grok-4.5` | xAI | `grok-4.5` |

See [llm-provider.example.json](./llm-provider.example.json) for a complete
configuration example.

```powershell
Copy-Item llm-provider.example.json llm-provider.json
$env:OPENROUTER_API_KEY = "..."
$env:CLAUDE_API_KEY = "..."
$env:XAI_API_KEY = "..."
go run ./cmd/llm-provider -f ./llm-provider.json
```

`-config` remains available as an alias for `-f`. The process watches the
selected configuration file with
[`fsnotify`](https://github.com/fsnotify/fsnotify). Changes are debounced and a
replacement Gateway is built and model-cache-warmed before it starts receiving
traffic. Invalid configurations are logged and ignored, leaving the current
Gateway running. In-flight requests continue on the previous Gateway until
they finish.

The containing directory is watched rather than only the file, so atomic saves
performed by editors are detected. Provider and model-cache settings reload at
runtime. A changed `listen` address is logged but requires a process restart;
the `-listen` command-line override continues to take precedence on every
reload.

### Model listing and metadata

List every model exposed by the enabled providers:

```bash
curl http://127.0.0.1:8080/v1/models
```

The Gateway discovers model lists concurrently during startup and serves model
list and detail requests from an in-memory cache. It refreshes the cache every
15 minutes by default, so those endpoints do not wait for backend network
calls. The interval can be set from 5 to 30 minutes, and each refresh has a
configurable timeout:

```json
{
  "model_cache_refresh_interval": "15m",
  "model_cache_refresh_timeout": "10s"
}
```

If discovery fails for a provider without a static model list, that provider's
models are omitted from the cache while models from healthy providers remain
available. A later successful refresh adds the provider back automatically.

When a provider has a `models` array, that array is the authoritative allowlist.
The Gateway still attempts model discovery to enrich allowlisted entries with
upstream metadata, but a discovery failure does not make a static allowlist
unavailable.

When `models` is omitted, an OpenAI-compatible provider calls `GET /models`, an
Anthropic provider calls the Claude Models API, and a Codex provider calls the
App Server `model/list` method.

The Gateway normalizes the following upstream context-window fields to the
`context_length` extension in its OpenAI-compatible model object:

- `context_length`
- `max_input_tokens`
- `max_model_len`
- `context_window`
- `context_window_tokens`
- `max_context_length`

If the backend does not provide a context length, the field is omitted. A
single prefixed model can be retrieved using the OpenAI-style model endpoint:

```bash
curl http://127.0.0.1:8080/v1/models/local/Agents-A1-4bit
```

Example response:

```json
{
  "id": "local/Agents-A1-4bit",
  "object": "model",
  "created": 1785572356,
  "owned_by": "omlx",
  "context_length": 262144
}
```

Providers can override limits for individual backend model IDs. Explicit
configuration takes precedence over discovered metadata:

```json
{
  "models": ["claude-sonnet-5"],
  "model_metadata": {
    "claude-sonnet-5": {
      "context_length": 1000000,
      "max_output_tokens": 128000
    }
  }
}
```

Claude `max_input_tokens` and `max_tokens` are normalized to `context_length`
and `max_output_tokens`. Its RFC 3339 `created_at` value is converted to the
Unix timestamp used by the OpenAI-compatible model object. Codex `model/list`
currently has no context-window field; after the first turn, the Gateway learns
the effective value from `thread/tokenUsage/updated` and includes it in later
model responses unless configuration overrides it.

### Chat Completions

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "codex/gpt-5.6-luna",
    "messages": [
      {"role": "system", "content": "Always answer concisely."},
      {"role": "user", "content": "Describe this repository in one sentence."}
    ]
  }'
```

Send the `conversation_id` from a Codex response with the next request to
continue the same App Server thread. Configure `ephemeral: false` if the thread
must be reopenable after the App Server restarts.

For OpenAI-compatible providers, Chat response JSON fields and SSE chunk fields
are preserved except that the backend model ID is replaced with its Gateway ID.
This retains compatible extensions such as `service_tier`,
`system_fingerprint`, annotations, refusal data, and log probabilities.

### Responses

```bash
curl http://127.0.0.1:8080/v1/responses \
  -H "Content-Type: application/json" \
  -d '{
    "model": "codex/gpt-5.6-luna",
    "instructions": "Always answer concisely.",
    "input": "Describe this repository in one sentence."
  }'
```

OpenAI-compatible providers receive `/responses` requests and SSE events
natively, with unknown fields preserved. Providers that expose only Chat are
adapted for the common text/message/function-tool subset. The lifecycle APIs
for stored or background Responses are intentionally not implemented yet.

### Embeddings

```bash
curl http://127.0.0.1:8080/v1/embeddings \
  -H "Content-Type: application/json" \
  -d '{
    "model": "lmstudio/text-embedding-model",
    "input": ["first document", "second document"],
    "encoding_format": "float"
  }'
```

There is no separate embedding-model configuration. Configure the server as a
normal OpenAI-compatible provider:

```json
{
  "id": "lmstudio",
  "type": "openai-compatible",
  "prefix": "lmstudio",
  "enabled": true,
  "base_url": "http://localhost:1234/v1"
}
```

If that backend returns an embedding model from `GET /models`, the normal model
cache exposes it with the provider prefix and `/v1/embeddings` routes it back
to that backend. The Gateway removes `lmstudio/` before the upstream call and
restores it in the response. Use a distinct prefix for each backend URL; for
example, the existing `local` provider can continue to point at
`http://macmini:11888/v1`. Codex App Server and Anthropic currently do not
expose an embeddings capability through this package, so selecting those
prefixes returns an explicit unsupported-provider error.

## Caching

The repository has three distinct cache layers:

| Layer | Owner | What is cached | How to observe it |
|---|---|---|---|
| Model catalog | Gateway | Results used by `GET /v1/models` and model detail | Models appear immediately without a backend call |
| Prompt cache | Selected model provider | Reusable input-prefix computation | `usage.prompt_tokens_details.cached_tokens` and `cache_write_tokens` |
| Response cache | OpenRouter | A complete response for an identical request | `X-OpenRouter-Cache-Status`, `Age`, and `TTL` |

The Gateway does not store generated Chat/Responses bodies or embedding
vectors. Prompt caching is provider-managed: the Gateway preserves request
extensions and normalizes the usage data needed to measure it.

### Prompt-cache checklist

1. Put stable system/developer instructions, examples, tools, schemas, and
   reusable documents first.
2. Put the current user input, timestamps, IDs, and other changing data after
   the reusable prefix.
3. Reuse a stable key for requests with the same prefix. Include a prompt or
   schema version in the key, such as `tenant-a:support-agent:v3`.
4. Warm the cache once, then append the next turn without rewriting the earlier
   prefix.
5. Inspect `cached_tokens` and `cache_write_tokens`; latency alone does not
   prove a cache hit.

For OpenAI GPT-5.6 and later, a cacheable prefix must be at least 1,024 tokens.
Use a stable `prompt_cache_key` and place an explicit
`prompt_cache_breakpoint` after reusable content when the suffix changes.
Setting `prompt_cache_options.mode` to `explicit` disables the implicit latest
message breakpoint, avoiding cache writes for a changing suffix. See the
[OpenAI prompt caching guide](https://developers.openai.com/api/docs/guides/prompt-caching)
for current provider semantics.

### Provider recipes

| Provider | Reuse mechanism | Keep stable | Hit signal |
|---|---|---|---|
| OpenAI-compatible GPT-5.6+ | `prompt_cache_key`, `prompt_cache_options`, `prompt_cache_breakpoint` | Prefix, cache key, tools and schema order | `cached_tokens`, `cache_write_tokens` |
| Claude | `cache_control` on a stable content block | Marked block and preceding content | Normalized cached/write tokens |
| Codex App Server | Provider-managed; retain `conversation_id` | Thread, history, tool calls and results | Normalized cached/write tokens |
| Grok | Stable `X-Grok-Conv-Id` | Header, prefix, system message and tools | `cached_tokens` |
| OpenRouter prompt cache | Stable `session_id` or `X-Session-Id` | Session key and prompt prefix | `cached_tokens` |
| OpenRouter response cache | `X-OpenRouter-Cache: true` | The complete request | `MISS`, then `HIT` response header |

Use request-scoped cache options when a provider handles mixed endpoint types.
Provider-level `headers` and `body` values are broad defaults and may be
inappropriate for `/embeddings` or backends that do not support the same cache
extensions.

Repository agents can follow the
[`use-llm-provider-caching`](./.agents/skills/use-llm-provider-caching/SKILL.md)
workflow when adding cache configuration, diagnosing misses, or running the
paid/external regression tasks.

### Forwarded cache fields

The following request headers are in the Gateway's default forwarding
allowlist. They are forwarded only to the selected HTTP provider:

- `X-OpenRouter-Cache`
- `X-OpenRouter-Cache-TTL`
- `X-OpenRouter-Cache-Clear`
- `X-Session-Id`
- `X-Grok-Conv-Id`
- `Anthropic-Beta`

Register additional names in a provider's `forward_headers` array.
`Authorization`, `Content-Type`, `Accept`, and `Host` are not copied from the
Gateway request; backend credentials and protocol headers are managed by the
provider configuration.

Unknown JSON request fields such as `prompt_cache_key`,
`prompt_cache_options`, `cache_control`, and `session_id` are preserved and
forwarded to the selected OpenAI-compatible backend. A provider's `body` object
can supply default JSON fields for every request. Request fields override
defaults with the same name.

```json
{
  "headers": {
    "X-OpenRouter-Cache": "true",
    "X-OpenRouter-Cache-TTL": "600"
  },
  "body": {
    "cache_control": {"type": "ephemeral", "ttl": "1h"}
  }
}
```

Configure body defaults only when the selected backend supports them. For
example, OpenRouter's top-level `cache_control` can affect provider selection.

Supported cache paths include:

- OpenAI GPT-5.6+: `prompt_cache_key`, `prompt_cache_options`, and content-block
  `prompt_cache_breakpoint`
- Anthropic Claude: top-level or content-block `cache_control`;
  `cache_read_input_tokens` and `cache_creation_input_tokens` are normalized to
  common cache usage fields
- OpenRouter provider prompt cache: `session_id`, `X-Session-Id`, and
  top-level or content-block `cache_control`
- OpenRouter response cache: `X-OpenRouter-Cache`,
  `X-OpenRouter-Cache-TTL`, and `X-OpenRouter-Cache-Clear`
- Grok: a stable `X-Grok-Conv-Id` plus stable system messages, tool definitions,
  and conversation prefixes
- Other automatic prompt-cache backends: stable system messages, tool
  definitions, and conversation prefixes

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "X-OpenRouter-Cache: true" \
  -H "X-OpenRouter-Cache-TTL: 600" \
  -d '{
    "model": "openrouter/openai/gpt-5.6",
    "prompt_cache_key": "tenant-a:agent-v3",
    "prompt_cache_options": {"mode": "explicit", "ttl": "30m"},
    "session_id": "conversation-123",
    "messages": [{
      "role": "system",
      "content": [{
        "type": "text",
        "text": "Long, stable instructions...",
        "prompt_cache_breakpoint": {"mode": "explicit"}
      }]
    }, {
      "role": "user",
      "content": "Hello"
    }]
  }'
```

The Gateway also forwards these selected response headers:

- `X-OpenRouter-Cache-Status`
- `X-OpenRouter-Cache-Age`
- `X-OpenRouter-Cache-TTL`
- `X-Generation-Id`
- `Request-Id`

`cached_tokens` and `cache_write_tokens` are preserved under
`usage.prompt_tokens_details`.

Codex App Server does not receive Gateway HTTP cache headers. Codex manages its
own model transport and caching. App Server `cachedInputTokens` and
`cacheWriteInputTokens` are converted to the OpenAI-compatible `cached_tokens`
and `cache_write_tokens` fields.

## Go provider API

### OpenAI-compatible providers

```go
provider := openai.New(
    openai.WithBaseURL("http://macmini:11888/v1"),
    openai.WithAPIKey("optional-for-local-server"),
)
client := llmprovider.New(provider)
defer client.Close()

response, err := client.Chat(ctx, llmprovider.ChatRequest{
    Model: "gpt-5.6-luna",
    Messages: []llmprovider.Message{
        {Role: llmprovider.RoleSystem, Content: "Always answer concisely."},
        {Role: llmprovider.RoleUser, Content: "Hello!"},
    },
    Headers: http.Header{
        "X-Grok-Conv-Id": {"conversation-123"},
    },
})
```

### Codex App Server

```go
provider := codex.New(
    codex.WithModel("gpt-5.6-luna"),
    codex.WithWorkingDirectory("."),
    codex.WithBaseInstructions("You are a concise coding assistant."),
)
client := llmprovider.New(provider)
defer client.Close()
```

System and developer messages become `developerInstructions` on a new Codex
thread. Earlier user, assistant, and tool messages are inserted through
`thread/inject_items`.

Use `WithMinimal` when the caller needs Codex transport and thread continuity
without the optional Codex agent prompt. It explicitly clears the built-in base
instructions, disables skill, app, collaboration, permission, and environment
instructions, disables the main optional agent features, and starts the thread
without a default execution environment. Caller-supplied dynamic tools remain
available. Starting without a default execution environment requires the
experimental API capability, which this provider enables by default.

The preset also sets `project_doc_max_bytes` to zero, disables personality,
shell and unified-exec features, and sets `web_search` to `disabled`. These are
thread-scoped config defaults and can be restored through
`WithThreadStartParams` or `thread_start.config`.

```go
provider := codex.New(
    codex.WithModel("gpt-5.6-sol"),
    codex.WithMinimal(),
    codex.WithThreadStartParams(map[string]any{
        "config": map[string]any{
            "mcp_servers.openaiDeveloperDocs.enabled": false,
        },
    }),
)
```

`WithThreadStartParams` is the escape hatch for additional App Server
`thread/start` fields. Its nested `config` object is merged with the minimal
defaults, so an explicit value can restore one part of the prompt. Request
model, working directory, system/developer messages, and dynamic tools take
precedence over provider defaults. Top-level names inside `thread_start` use
the App Server's native camelCase spelling.

The equivalent Gateway configuration is:

```json
{
  "codex": {
    "model": "gpt-5.6-sol",
    "minimal": true,
    "thread_start": {
      "config": {
        "mcp_servers.openaiDeveloperDocs.enabled": false
      }
    }
  }
}
```

Keep the returned `conversation_id` for follow-up turns. Minimal mode changes
the stable prompt prefix but does not replace Codex's provider-managed prompt
cache or the Gateway's normalized cached-token accounting.

Codex dynamic tools operate in one of two modes:

- With a `ToolHandler`, the provider handles App Server `item/tool/call` events
  inside the same Codex turn.
- Without a `ToolHandler`, the provider delegates the callback to the caller as
  OpenAI-compatible `tool_calls` with `finish_reason: "tool_calls"`.

The following example calls the Gateway through the OpenAI provider and
completes a delegated Codex tool round trip:

```go
client := llmprovider.New(openai.New(
    openai.WithBaseURL("http://127.0.0.1:8080/v1"),
))

request := llmprovider.ChatRequest{
    Model: "codex/gpt-5.6-luna",
    Messages: []llmprovider.Message{{
        Role:    llmprovider.RoleUser,
        Content: "Call lookup_value before answering.",
    }},
    Tools: []llmprovider.Tool{{
        Type: llmprovider.ToolTypeFunction,
        Function: llmprovider.FunctionDefinition{
            Name:       "lookup_value",
            Parameters: map[string]any{"type": "object"},
        },
    }},
    ToolChoice: llmprovider.ToolChoiceAuto,
}

first, err := client.Chat(ctx, request)
call := first.Choices[0].Message.ToolCalls[0]

request.ConversationID = first.ConversationID
request.ToolChoice = llmprovider.ToolChoiceNone
request.Messages = append(
    request.Messages,
    first.Choices[0].Message,
    llmprovider.Message{
        Role:       llmprovider.RoleTool,
        ToolCallID: call.ID,
        Content:    `{"value":"tool result"}`,
    },
)
final, err := client.Chat(ctx, request)
```

The Gateway retains delegated tool callbacks. When the caller sends the same
`conversation_id` with matching tool results, the Gateway completes the pending
App Server callbacks so the same Codex thread and turn can continue. Multiple
callbacks are matched by call ID.

If the Gateway or App Server restarts and loses a pending callback, the provider
reconstructs a new Codex thread from the external OpenAI message history. Only
this fallback may return a new `conversation_id`; callers should always retain
the most recently returned value.

### Anthropic Claude

An `anthropic` provider converts common chat requests to the native Claude
Messages API. System and developer messages become the top-level `system`
field, function tools are translated to `tool_use` and `tool_result`, and native
SSE streaming is supported.

```json
{
  "id": "claude",
  "type": "anthropic",
  "prefix": "claude",
  "enabled": true,
  "api_key_env": "CLAUDE_API_KEY",
  "models": ["claude-sonnet-5"]
}
```

Claude Sonnet 5 uses adaptive thinking by default and rejects non-default
`temperature` and `top_p` values. For explicit prompt caching, add
`cache_control: {"type":"ephemeral","ttl":"5m"}` to a system or message
content block.

## Verification

Run local tests that do not require external API access:

```bash
go test ./...
```

### Cache regression tests

External cache hit-rate tests live in `tests/cache_hitrate_test.go`. They are
skipped by the default `go test ./...` run because they can incur API costs. Use
[Task](https://taskfile.dev/) to run one provider or the complete suite
explicitly:

```powershell
task test
task cache:codex
task cache:claude
task cache:grok
task cache:openrouter
task cache:openai-compatible
task cache:all
```

The default pass condition is `cached_tokens / prompt_tokens >= 0.50`. Override
the threshold globally or for an individual provider:

```powershell
task cache:all CACHE_MIN_HIT_RATE=0.80

$env:CACHE_MIN_HIT_RATE_CODEX = "0.90"
task cache:codex
```

`task regression` runs local tests and `go vet` before executing every external
cache regression sequentially. Set `GATEWAY_CONFIG_PATH` to use a different
configuration file.

### Real-backend integration tests

```powershell
$env:OPENAI_COMPAT_INTEGRATION_BASE_URL = "http://macmini:11888/v1"
$env:OPENAI_COMPAT_INTEGRATION_MODEL = "gpt-5.6-luna"
go test ./providers/openai -run TestIntegration -v

$env:CODEX_APP_SERVER_INTEGRATION = "1"
$env:CODEX_APP_SERVER_CHAT_INTEGRATION = "1"
$env:CODEX_APP_SERVER_INTEGRATION_MODEL = "gpt-5.6-luna"
go test ./providers/codex -run TestIntegration -v

$env:CODEX_APP_SERVER_PROMPT_BASELINE_INTEGRATION = "1"
go test ./providers/codex -run TestIntegrationPromptBaseline -v -count=1

$env:ANTHROPIC_MODEL_LIST_INTEGRATION = "1"
go test ./providers/anthropic -run TestIntegrationListModels -v

$env:GATEWAY_MODEL_LIST_INTEGRATION = "1"
$env:GATEWAY_OPENAI_COMPAT_CHAT_INTEGRATION = "1"
$env:GATEWAY_CODEX_TOOL_INTEGRATION = "1"
go test ./gateway -run TestIntegration -v

$env:GATEWAY_GROK_INTEGRATION = "1"
$env:GROK_INTEGRATION_MODEL = "grok-4.5"
go test ./gateway -run TestIntegrationGrokFromConfigThroughOpenAIProvider -v

$env:GATEWAY_OPENROUTER_INTEGRATION = "1"
$env:OPENROUTER_INTEGRATION_MODEL = "openai/gpt-4.1-mini"
go test ./gateway -run TestIntegrationOpenRouterFromConfigThroughOpenAIProvider -v
```

## References

- [OpenAI Chat Completions API](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create)
- [OpenAI Responses API](https://developers.openai.com/api/reference/resources/responses/methods/create)
- [OpenAI Embeddings API](https://developers.openai.com/api/reference/resources/embeddings/methods/create)
- [OpenAI Models API](https://platform.openai.com/docs/api-reference/models)
- [OpenAI prompt caching](https://developers.openai.com/api/docs/guides/prompt-caching)
- [OpenAI function calling](https://developers.openai.com/api/docs/guides/function-calling)
- [Codex App Server](https://developers.openai.com/codex/app-server/)
- [Claude Messages API](https://platform.claude.com/docs/en/api/messages/create)
- [Claude prompt caching](https://platform.claude.com/docs/en/build-with-claude/prompt-caching)
- [OpenRouter prompt caching](https://openrouter.ai/docs/guides/best-practices/prompt-caching)
- [OpenRouter response caching](https://openrouter.ai/docs/guides/features/response-caching)
- [xAI prompt caching](https://docs.x.ai/developers/advanced-api-usage/prompt-caching)
