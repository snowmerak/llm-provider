# llm-provider

여러 LLM backend를 하나의 Go 인터페이스로 사용하는 클라이언트입니다.

- OpenAI-compatible `POST /chat/completions`
- 로컬 `codex app-server --listen stdio://` JSON-RPC
- 일반 응답과 스트리밍
- function tool calling
- Codex thread ID와 대화 컨텍스트 유지

## OpenAI-compatible API

OpenAI 구현은 `providers/openai` 패키지에 있습니다. 기본 설정은 `OPENAI_BASE_URL`(기본값 `https://api.openai.com/v1`)과 `OPENAI_API_KEY`를 읽습니다.

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
})
```

`ChatRequest.Extra`로 provider 전용 필드를 추가할 수 있습니다. `model`, `messages`, `stream`, `tools`처럼 공통 타입으로 정의된 필드가 `Extra`보다 우선합니다.

## Tool calling

공통 `Tool`, `ToolCall`, `ToolResult` 타입을 사용합니다.

```go
request := llmprovider.ChatRequest{
    Model: "gpt-5.6-luna",
    Messages: []llmprovider.Message{
        {Role: llmprovider.RoleUser, Content: "서울 날씨를 알려줘."},
    },
    Tools: []llmprovider.Tool{{
        Type: llmprovider.ToolTypeFunction,
        Function: llmprovider.FunctionDefinition{
            Name:        "get_weather",
            Description: "현재 날씨를 조회한다.",
            Parameters: map[string]any{
                "type": "object",
                "properties": map[string]any{
                    "city": map[string]any{"type": "string"},
                },
                "required":             []string{"city"},
                "additionalProperties": false,
            },
        },
    }},
    ToolChoice: llmprovider.ToolChoiceAuto,
}
```

OpenAI-compatible provider는 모델의 tool call을 응답으로 반환합니다. 애플리케이션에서 함수를 실행하고 assistant 메시지와 `RoleTool` 결과 메시지를 추가해 다음 요청을 보내면 됩니다.

Codex provider는 App Server의 `dynamicTools`와 `item/tool/call` 흐름을 사용합니다. `ToolHandler`를 지정하면 호출과 결과 전송을 provider가 자동 처리합니다.

```go
request.ToolHandler = func(ctx context.Context, call llmprovider.ToolCall) (llmprovider.ToolResult, error) {
    output, err := executeTool(call.Function.Name, call.Function.Arguments)
    if err != nil {
        return llmprovider.ToolResult{Content: err.Error(), IsError: true}, nil
    }
    return llmprovider.ToolResult{Content: output}, nil
}
```

Codex dynamic tools는 App Server experimental API가 필요하며 기본으로 활성화됩니다. Codex에서는 `ToolChoiceAuto`와 `ToolChoiceNone`을 지원합니다.

## Codex App Server

Codex provider는 HTTP endpoint를 사용하지 않습니다. 첫 호출 때 로컬에서 `codex app-server --listen stdio://`를 실행하고 initialize handshake를 자동 수행합니다.

```go
provider := codex.New(
    codex.WithModel("gpt-5.6-luna"),
    codex.WithWorkingDirectory("."),
    codex.WithBaseInstructions("You are a concise coding assistant."),
)
client := llmprovider.New(provider)
defer client.Close()
```

### 시스템 프롬프트와 컨텍스트

- `WithBaseInstructions`는 새 thread의 Codex base instructions를 교체합니다.
- system/developer 메시지는 thread의 `developerInstructions`로 추가됩니다.
- 새 대화의 이전 user/assistant/tool 메시지는 `thread/inject_items`로 주입됩니다.
- 응답의 `ConversationID`를 다음 요청에 전달하면 같은 thread의 누적 컨텍스트를 이어갑니다.

```go
first, err := client.Chat(ctx, llmprovider.ChatRequest{
    Messages: []llmprovider.Message{
        {Role: llmprovider.RoleSystem, Content: "항상 한국어로 답해."},
        {Role: llmprovider.RoleUser, Content: "작업 상태를 ALPHA로 기억해."},
    },
})

followUp, err := client.Chat(ctx, llmprovider.ChatRequest{
    ConversationID: first.ConversationID,
    Messages: []llmprovider.Message{
        {Role: llmprovider.RoleUser, Content: "작업 상태를 BETA로 바꿔."},
    },
})
```

기본값은 `sandbox=read-only`, `approvalPolicy=never`, `ephemeral=true`입니다. App Server 재시작 뒤에도 thread를 재개하려면 `codex.WithEphemeral(false)`를 사용해야 합니다.

## 검증

```bash
go test ./...
```

실제 통합 테스트는 환경 변수로 보호됩니다.

```bash
# OpenAI-compatible API만 macmini endpoint로 테스트
OPENAI_COMPAT_INTEGRATION_BASE_URL=http://macmini:11888/v1 \
OPENAI_COMPAT_INTEGRATION_MODEL=gpt-5.6-luna \
go test ./providers/openai -run TestIntegration -v

# 실제 로컬 codex app-server와 gpt-5.6-luna 테스트
CODEX_APP_SERVER_INTEGRATION_MODEL=gpt-5.6-luna \
CODEX_APP_SERVER_CHAT_INTEGRATION=1 \
CODEX_APP_SERVER_TOOL_INTEGRATION=1 \
CODEX_APP_SERVER_CONTEXT_INTEGRATION=1 \
go test ./providers/codex -run 'TestIntegration(Chat|DynamicTool|SystemPromptAndContext)$' -v
```

구현은 [OpenAI Chat Completions API](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create), [Function calling 가이드](https://developers.openai.com/api/docs/guides/function-calling), [Codex App Server 문서](https://developers.openai.com/codex/app-server/)를 기준으로 합니다.
