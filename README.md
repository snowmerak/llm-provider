# llm-provider

여러 LLM backend를 하나의 Go 인터페이스와 OpenAI-compatible HTTP endpoint로 제공하는 프로젝트입니다.

- OpenAI-compatible backend와 OpenRouter, xAI/Grok, Anthropic Claude
- 로컬 `codex app-server --listen stdio://`
- `GET /v1/models`와 `POST /v1/chat/completions`
- 일반 응답, SSE streaming, function tool calling
- Codex thread ID를 이용한 상태 유지
- Provider별 요청 header와 body extension 전달

## Gateway

Gateway는 설정에서 활성화한 Provider만 생성합니다. 외부 모델 ID의 첫 번째 경로를 Provider prefix로 사용합니다.

| 외부 모델 ID | 실제 backend | backend에 전달되는 모델 |
|---|---|---|
| `codex/gpt-5.6-luna` | `codex app-server` | `gpt-5.6-luna` |
| `claude/claude-sonnet-5` | Anthropic Messages API | `claude-sonnet-5` |
| `openrouter/anthropic/claude-sonnet-4` | OpenRouter | `anthropic/claude-sonnet-4` |
| `local/gpt-5.6-luna` | `http://macmini:11888/v1` | `gpt-5.6-luna` |
| `grok/grok-4.5` | xAI | `grok-4.5` |

설정 예시는 [llm-provider.example.json](./llm-provider.example.json)에 있습니다.

```powershell
Copy-Item llm-provider.example.json llm-provider.json
$env:OPENROUTER_API_KEY = "..."
go run ./cmd/llm-provider -config ./llm-provider.json
```

### 모델 목록

```bash
curl http://127.0.0.1:8080/v1/models
```

`models`가 Provider 설정에 있으면 그 목록을 allowlist로 사용합니다. 생략하면 OpenAI-compatible Provider는 `GET /models`를, Codex Provider는 App Server의 `model/list`를 호출하여 실제 모델을 조회합니다.

Upstream 모델 목록에 `context_length`, `max_model_len`, `context_window`,
`context_window_tokens`, `max_context_length`가 있으면 Gateway는 이를
`context_length`로 정규화해 반환합니다. 단일 모델 정보도 같은 OpenAI 호환
모델 객체로 조회할 수 있습니다.

```bash
curl http://127.0.0.1:8080/v1/models/local/Agents-A1-4bit
```

### Chat Completions

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "codex/gpt-5.6-luna",
    "messages": [
      {"role": "system", "content": "항상 한국어로 답해."},
      {"role": "user", "content": "이 저장소를 한 문장으로 설명해줘."}
    ]
  }'
```

Codex 응답의 `conversation_id`를 다음 요청에 다시 보내면 같은 App Server thread를 이어갑니다. `ephemeral: false`로 설정해야 App Server 재시작 뒤에도 thread를 다시 열 수 있습니다.

## Cache와 요청 header

다음 요청 header는 기본 allowlist에 포함되어 선택한 HTTP Provider에만 전달됩니다.

- `X-OpenRouter-Cache`
- `X-OpenRouter-Cache-TTL`
- `X-OpenRouter-Cache-Clear`
- `X-Session-Id`
- `X-Grok-Conv-Id`

추가 header는 Provider의 `forward_headers`에 이름을 등록합니다. `Authorization`, `Content-Type`, `Accept`, `Host`는 전달하지 않고 backend 설정에서 관리합니다.

`prompt_cache_key`, `prompt_cache_options`, `cache_control`, `session_id`처럼 공통 타입에 없는 JSON 필드는 수정하지 않고 선택한 OpenAI-compatible backend로 전달됩니다. Provider 설정의 `body`에는 모든 요청에 적용할 기본 JSON 필드를 넣을 수 있으며, 요청에 같은 필드가 있으면 요청 값이 우선합니다.

```json
{
  "headers": {"X-OpenRouter-Cache": "true", "X-OpenRouter-Cache-TTL": "600"},
  "body": {"cache_control": {"type": "ephemeral", "ttl": "1h"}}
}
```

`body` 기본값은 해당 backend가 필드를 지원할 때만 설정합니다. 예를 들어 OpenRouter의 top-level `cache_control`은 provider 선택에 영향을 줄 수 있습니다.

사용할 수 있는 캐시 경로는 다음과 같습니다.

- OpenAI GPT-5.6+: `prompt_cache_key`, `prompt_cache_options`, content block의 `prompt_cache_breakpoint`
- Anthropic Claude: top-level 또는 content block의 `cache_control`; `cache_read_input_tokens`와 `cache_creation_input_tokens`는 공통 cache usage로 변환
- OpenRouter provider prompt cache: `session_id` 또는 `X-Session-Id`, top-level/개별 block의 `cache_control`
- OpenRouter response cache: `X-OpenRouter-Cache`, `X-OpenRouter-Cache-TTL`, `X-OpenRouter-Cache-Clear`
- Grok: 요청별로 안정적인 `X-Grok-Conv-Id`를 보내고 system/tool 정의와 대화 prefix를 동일하게 유지. cache read는 `usage.prompt_tokens_details.cached_tokens`로 확인
- 기타 자동 prompt cache backend: system/tool 정의와 대화 prefix를 동일하게 유지

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
    }, {"role": "user", "content": "Hello"}]
  }'
```

OpenRouter의 `X-OpenRouter-Cache-Status`, `X-OpenRouter-Cache-Age`, `X-OpenRouter-Cache-TTL`, `X-Generation-Id` 응답 header도 Gateway 응답으로 전달합니다. `cached_tokens`와 `cache_write_tokens`는 `usage.prompt_tokens_details`에 보존됩니다.

Codex App Server는 Gateway가 HTTP 캐시 header를 주입하는 backend가 아닙니다. Codex의 모델 통신과 캐시는 App Server가 관리합니다. App Server의 `cachedInputTokens`와 `cacheWriteInputTokens`는 각각 OpenAI 형식의 `cached_tokens`와 `cache_write_tokens`로 변환합니다.

## Go Provider API

### OpenAI-compatible

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
        {Role: llmprovider.RoleSystem, Content: "항상 간결하게 답해."},
        {Role: llmprovider.RoleUser, Content: "안녕!"},
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

System/developer message는 새 Codex thread의 `developerInstructions`로 전달됩니다. 이전 user/assistant/tool message는 `thread/inject_items`로 주입합니다.

Codex dynamic tool은 두 가지 방식으로 동작합니다.

- `ToolHandler`가 있으면 App Server의 `item/tool/call`을 받아 같은 Codex turn 안에서 실행합니다.
- `ToolHandler`가 없으면 App Server callback을 OpenAI 형식의 `tool_calls`와 `finish_reason: "tool_calls"`로 호출자에게 위임합니다.

Gateway를 OpenAI Provider로 호출하는 왕복 예시:

```go
client := llmprovider.New(openai.New(
    openai.WithBaseURL("http://127.0.0.1:8080/v1"),
))

request := llmprovider.ChatRequest{
    Model: "codex/gpt-5.6-luna",
    Messages: []llmprovider.Message{{
        Role: llmprovider.RoleUser,
        Content: "lookup_value를 호출해줘.",
    }},
    Tools: []llmprovider.Tool{{
        Type: llmprovider.ToolTypeFunction,
        Function: llmprovider.FunctionDefinition{
            Name: "lookup_value",
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
        Role: llmprovider.RoleTool,
        ToolCallID: call.ID,
        Content: `{"value":"tool result"}`,
    },
)
final, err := client.Chat(ctx, request)
```

Delegated tool callback은 Gateway 안에 보관됩니다. 호출자가 같은 `conversation_id`와 tool 결과를 보내면 보관 중인 App Server callback에 결과를 전달하므로 같은 Codex thread와 turn이 계속됩니다. 여러 callback이 함께 도착하면 call ID로 결과를 매칭합니다.

Gateway/App Server가 재시작되어 보관 중인 callback을 잃은 경우에는 외부 OpenAI message history를 기준으로 새 Codex thread를 구성하는 fallback을 사용합니다. 이 경우에만 새 `conversation_id`가 반환될 수 있으므로 이후에는 가장 최근 값을 사용해야 합니다.

### Anthropic Claude

`type: "anthropic"` provider는 공통 Chat 요청을 Claude Messages API로 변환합니다. system/developer message는 top-level `system`으로, function tool은 `tool_use`/`tool_result`로 변환하며 SSE streaming도 지원합니다.

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

Claude Sonnet 5는 adaptive thinking을 기본 사용하며 비기본 `temperature`와 `top_p`를 허용하지 않습니다. 명시적인 prompt cache는 system 또는 message content block에 `cache_control: {"type":"ephemeral","ttl":"5m"}`을 넣어 사용합니다.

## 검증

```bash
go test ./...
```

### 캐시 회귀 테스트

외부 API 비용이 드는 캐시 적중률 테스트는 `tests/cache_hitrate_test.go`에 있으며 기본 `go test ./...`에서는 skip됩니다. [Task](https://taskfile.dev/)로 provider별 또는 전체 테스트를 명시적으로 실행합니다.

```powershell
task test
task cache:codex
task cache:claude
task cache:grok
task cache:openrouter
task cache:openai-compatible
task cache:all
```

기본 통과 기준은 `cached_tokens / prompt_tokens >= 0.50`입니다. 전체 또는 provider별 기준을 조정할 수 있습니다.

```powershell
task cache:all CACHE_MIN_HIT_RATE=0.80

$env:CACHE_MIN_HIT_RATE_CODEX = "0.90"
task cache:codex
```

`task regression`은 로컬 테스트와 `go vet`을 실행한 뒤 실제 provider 캐시 회귀를 순차 실행합니다. 다른 설정 파일을 사용하려면 `GATEWAY_CONFIG_PATH`를 지정합니다.

실제 backend 통합 테스트:

```powershell
$env:OPENAI_COMPAT_INTEGRATION_BASE_URL = "http://macmini:11888/v1"
$env:OPENAI_COMPAT_INTEGRATION_MODEL = "gpt-5.6-luna"
go test ./providers/openai -run TestIntegration -v

$env:CODEX_APP_SERVER_INTEGRATION = "1"
$env:CODEX_APP_SERVER_CHAT_INTEGRATION = "1"
$env:CODEX_APP_SERVER_INTEGRATION_MODEL = "gpt-5.6-luna"
go test ./providers/codex -run TestIntegration -v

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

구현은 [OpenAI Chat Completions API](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create), [Prompt caching](https://developers.openai.com/api/docs/guides/prompt-caching), [Function calling](https://developers.openai.com/api/docs/guides/function-calling), [Codex App Server](https://developers.openai.com/codex/app-server/), [Claude Messages API](https://platform.claude.com/docs/en/api/messages/create), [Claude prompt caching](https://platform.claude.com/docs/en/build-with-claude/prompt-caching), [OpenRouter prompt caching](https://openrouter.ai/docs/guides/best-practices/prompt-caching), [OpenRouter response caching](https://openrouter.ai/docs/guides/features/response-caching), [xAI prompt caching](https://docs.x.ai/developers/advanced-api-usage/prompt-caching) 문서를 기준으로 합니다.
