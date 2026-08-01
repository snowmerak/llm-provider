package codex

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	llmprovider "github.com/snowmerak/llm-provider"
)

func integrationProvider() *Provider {
	options := []Option{WithEphemeral(true), WithSandbox(SandboxReadOnly)}
	if model := os.Getenv("CODEX_APP_SERVER_INTEGRATION_MODEL"); model != "" {
		options = append(options, WithModel(model))
	}
	if instructions := os.Getenv("CODEX_APP_SERVER_BASE_INSTRUCTIONS"); instructions != "" {
		options = append(options, WithBaseInstructions(instructions))
	}
	return New(options...)
}

func TestIntegrationModelList(t *testing.T) {
	if os.Getenv("CODEX_APP_SERVER_INTEGRATION") == "" {
		t.Skip("set CODEX_APP_SERVER_INTEGRATION=1 to test an installed Codex App Server")
	}
	provider := New()
	defer provider.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var response struct {
		Data []any `json:"data"`
	}
	if err := provider.Call(ctx, "model/list", map[string]any{}, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Data) == 0 {
		t.Fatal("model/list returned no models")
	}
}

func TestIntegrationDynamicTool(t *testing.T) {
	if os.Getenv("CODEX_APP_SERVER_TOOL_INTEGRATION") == "" {
		t.Skip("set CODEX_APP_SERVER_TOOL_INTEGRATION=1 to run a real Codex dynamic tool turn")
	}
	provider := integrationProvider()
	defer provider.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var called atomic.Bool
	response, err := provider.Chat(ctx, llmprovider.ChatRequest{
		Messages: []llmprovider.Message{{Role: llmprovider.RoleUser, Content: "Call get_magic_number, then reply with the returned number."}},
		Tools: []llmprovider.Tool{{Type: llmprovider.ToolTypeFunction, Function: llmprovider.FunctionDefinition{
			Name: "get_magic_number", Description: "Returns the magic number. Always call this function when asked for the magic number.",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
		}}},
		ToolHandler: func(ctx context.Context, call llmprovider.ToolCall) (llmprovider.ToolResult, error) {
			called.Store(true)
			return llmprovider.ToolResult{Content: `{"number":42}`}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called.Load() {
		t.Fatalf("dynamic tool handler was not called; response: %q", response.Choices[0].Message.Content)
	}
	if !strings.Contains(response.Choices[0].Message.Content, "42") {
		t.Fatalf("unexpected response: %q", response.Choices[0].Message.Content)
	}
}

func TestIntegrationChat(t *testing.T) {
	if os.Getenv("CODEX_APP_SERVER_CHAT_INTEGRATION") == "" {
		t.Skip("set CODEX_APP_SERVER_CHAT_INTEGRATION=1 to run a real Codex turn")
	}
	provider := integrationProvider()
	defer provider.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	response, err := provider.Chat(ctx, llmprovider.ChatRequest{Messages: []llmprovider.Message{
		{Role: llmprovider.RoleUser, Content: "Reply with exactly PONG. Do not use tools."},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToUpper(response.Choices[0].Message.Content), "PONG") {
		t.Fatalf("unexpected response: %q", response.Choices[0].Message.Content)
	}
	if response.ConversationID == "" {
		t.Fatal("response has no conversation ID")
	}
}

func TestIntegrationSystemPromptAndContext(t *testing.T) {
	if os.Getenv("CODEX_APP_SERVER_CONTEXT_INTEGRATION") == "" {
		t.Skip("set CODEX_APP_SERVER_CONTEXT_INTEGRATION=1 to test prompt and thread context")
	}
	provider := integrationProvider()
	defer provider.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	first, err := provider.Chat(ctx, llmprovider.ChatRequest{Messages: []llmprovider.Message{
		{Role: llmprovider.RoleSystem, Content: "Every answer must include CODEX_SYSTEM_OK_517."},
		{Role: llmprovider.RoleUser, Content: "Store CODEX_CONTEXT_ALPHA_103 as the current mutable working-context value, then state it with the system token."},
	}})
	if err != nil {
		t.Fatal(err)
	}
	firstContent := first.Choices[0].Message.Content
	if !strings.Contains(firstContent, "CODEX_SYSTEM_OK_517") || !strings.Contains(firstContent, "CODEX_CONTEXT_ALPHA_103") {
		t.Fatalf("system prompt was not applied: %q", firstContent)
	}

	second, err := provider.Chat(ctx, llmprovider.ChatRequest{
		ConversationID: first.ConversationID,
		Messages:       []llmprovider.Message{{Role: llmprovider.RoleUser, Content: "Update the mutable working-context value from CODEX_CONTEXT_ALPHA_103 to CODEX_CONTEXT_BETA_842. Confirm the new value."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.Choices[0].Message.Content, "CODEX_CONTEXT_BETA_842") {
		t.Fatalf("context update was not acknowledged: %q", second.Choices[0].Message.Content)
	}

	third, err := provider.Chat(ctx, llmprovider.ChatRequest{
		ConversationID: first.ConversationID,
		Messages:       []llmprovider.Message{{Role: llmprovider.RoleUser, Content: "What is the current context value? Include the system token too."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	thirdContent := third.Choices[0].Message.Content
	if !strings.Contains(thirdContent, "CODEX_CONTEXT_BETA_842") || !strings.Contains(thirdContent, "CODEX_SYSTEM_OK_517") {
		t.Fatalf("modified thread context was not retained: %q", thirdContent)
	}
}
