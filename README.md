# llm-provider

여러 LLM backend를 하나의 Go 인터페이스와 OpenAI-compatible HTTP endpoint로 제공하는 프로젝트입니다.

- OpenAI-compatible backend와 OpenRouter, xAI/Grok
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

`prompt_cache_key`, `prompt_cache_options`, `cache_control`처럼 공통 타입에 없는 JSON 필드는 수정하지 않고 선택한 OpenAI-compatible backend로 전달됩니다.

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "X-OpenRouter-Cache: true" \
  -H "X-OpenRouter-Cache-TTL: 600" \
  -d '{
    "model": "openrouter/openai/gpt-5.6",
    "prompt_cache_key": "tenant-a:agent-v3",
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

OpenRouter의 `X-OpenRouter-Cache-Status`, `X-OpenRouter-Cache-Age`, `X-OpenRouter-Cache-TTL`, `X-Generation-Id` 응답 header도 Gateway 응답으로 전달합니다. `cached_tokens`와 `cache_write_tokens`는 `usage.prompt_tokens_details`에 보존됩니다.

Codex App Server는 Gateway가 HTTP 캐시 header를 주입하는 backend가 아닙니다. Codex의 모델 통신과 캐시는 App Server가 관리합니다.

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

Codex dynamic tool은 Go API에서 `ToolHandler`를 제공할 때 App Server의 `item/tool/call`을 통해 실행됩니다. HTTP Gateway의 OpenAI-style tool call은 OpenAI-compatible backend에서는 그대로 동작하지만, Codex backend의 dynamic tool을 외부 클라이언트에 위임하는 프로토콜은 아직 제공하지 않습니다.

## 검증

```bash
go test ./...
```

실제 backend 통합 테스트:

```powershell
$env:OPENAI_COMPAT_INTEGRATION_BASE_URL = "http://macmini:11888/v1"
$env:OPENAI_COMPAT_INTEGRATION_MODEL = "gpt-5.6-luna"
go test ./providers/openai -run TestIntegration -v

$env:CODEX_APP_SERVER_INTEGRATION = "1"
$env:CODEX_APP_SERVER_CHAT_INTEGRATION = "1"
$env:CODEX_APP_SERVER_INTEGRATION_MODEL = "gpt-5.6-luna"
go test ./providers/codex -run TestIntegration -v
```

구현은 [OpenAI Chat Completions API](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create), [Function calling](https://developers.openai.com/api/docs/guides/function-calling), [Codex App Server](https://developers.openai.com/codex/app-server/) 문서를 기준으로 합니다.
