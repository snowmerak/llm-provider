# llm-provider

`llm-provider` exposes multiple LLM backends through a single Go interface and
an OpenAI-compatible HTTP gateway.

It currently supports:

- OpenAI-compatible APIs, including local servers, OpenRouter, and xAI/Grok
- Anthropic Claude through the native Messages API
- a local `codex app-server --listen stdio://` process
- model listing and model metadata lookup
- regular and SSE-streaming Chat Completions
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
| `grok/grok-4.5` | xAI | `grok-4.5` |

See [llm-provider.example.json](./llm-provider.example.json) for a complete
configuration example.

```powershell
Copy-Item llm-provider.example.json llm-provider.json
$env:OPENROUTER_API_KEY = "..."
$env:CLAUDE_API_KEY = "..."
$env:XAI_API_KEY = "..."
go run ./cmd/llm-provider -config ./llm-provider.json
```

### Model listing and metadata

List every model exposed by the enabled providers:

```bash
curl http://127.0.0.1:8080/v1/models
```

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

## Caching and provider metadata

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
- [OpenAI Models API](https://platform.openai.com/docs/api-reference/models)
- [OpenAI prompt caching](https://developers.openai.com/api/docs/guides/prompt-caching)
- [OpenAI function calling](https://developers.openai.com/api/docs/guides/function-calling)
- [Codex App Server](https://developers.openai.com/codex/app-server/)
- [Claude Messages API](https://platform.claude.com/docs/en/api/messages/create)
- [Claude prompt caching](https://platform.claude.com/docs/en/build-with-claude/prompt-caching)
- [OpenRouter prompt caching](https://openrouter.ai/docs/guides/best-practices/prompt-caching)
- [OpenRouter response caching](https://openrouter.ai/docs/guides/features/response-caching)
- [xAI prompt caching](https://docs.x.ai/developers/advanced-api-usage/prompt-caching)
