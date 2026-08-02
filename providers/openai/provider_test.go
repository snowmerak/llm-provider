package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	llmprovider "github.com/snowmerak/llm-provider"
)

func TestChatWithTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/chat/completions" {
			t.Errorf("path = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("authorization = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["stream"] != false || body["tool_choice"] != "auto" || body["parallel_tool_calls"] != true {
			t.Errorf("tool options = %#v", body)
		}
		if body["reasoning_effort"] != "high" {
			t.Errorf("reasoning effort = %#v", body["reasoning_effort"])
		}
		tools, ok := body["tools"].([]any)
		if !ok || len(tools) != 1 {
			t.Fatalf("tools = %#v", body["tools"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat_1","model":"test","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Seoul\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
	}))
	defer server.Close()

	strict, parallel := true, true
	client := llmprovider.New(New(WithBaseURL(server.URL+"/v1"), WithAPIKey("secret")))
	response, err := client.Chat(context.Background(), llmprovider.ChatRequest{
		Model:    "test",
		Messages: []llmprovider.Message{{Role: llmprovider.RoleUser, Content: "weather?"}},
		Tools: []llmprovider.Tool{{Type: llmprovider.ToolTypeFunction, Function: llmprovider.FunctionDefinition{
			Name: "get_weather", Description: "Get weather", Strict: &strict,
			Parameters: map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []string{"city"}, "additionalProperties": false},
		}}},
		ToolChoice:        llmprovider.ToolChoiceAuto,
		ParallelToolCalls: &parallel,
		ReasoningEffort:   "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	call := response.Choices[0].Message.ToolCalls[0]
	if call.ID != "call_1" || call.Function.Name != "get_weather" || response.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("response = %#v", response)
	}
}

func TestChatToolResultMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages       []llmprovider.Message `json:"messages"`
			ConversationID string                `json:"conversation_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Messages) != 3 || body.Messages[1].ToolCalls[0].ID != "call_1" || body.Messages[2].ToolCallID != "call_1" {
			t.Fatalf("messages = %#v", body.Messages)
		}
		if body.ConversationID != "thread_1" {
			t.Fatalf("conversation id = %q", body.ConversationID)
		}
		_, _ = io.WriteString(w, `{"id":"chat_2","choices":[{"index":0,"message":{"role":"assistant","content":"sunny"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	client := llmprovider.New(New(WithBaseURL(server.URL)))
	_, err := client.Chat(context.Background(), llmprovider.ChatRequest{ConversationID: "thread_1", Messages: []llmprovider.Message{
		{Role: llmprovider.RoleUser, Content: "weather?"},
		{Role: llmprovider.RoleAssistant, ToolCalls: []llmprovider.ToolCall{{ID: "call_1", Type: llmprovider.ToolTypeFunction, Function: llmprovider.FunctionCall{Name: "get_weather", Arguments: `{}`}}}},
		{Role: llmprovider.RoleTool, ToolCallID: "call_1", Content: `{"weather":"sunny"}`},
	}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestChatPreservesPromptCacheExtensions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["prompt_cache_key"] != "request-key" {
			t.Fatalf("prompt cache key = %#v", body["prompt_cache_key"])
		}
		cacheControl, ok := body["cache_control"].(map[string]any)
		if !ok || cacheControl["type"] != "ephemeral" || cacheControl["ttl"] != "1h" {
			t.Fatalf("cache control = %#v", body["cache_control"])
		}
		messages := body["messages"].([]any)
		content := messages[0].(map[string]any)["content"].([]any)
		breakpoint := content[0].(map[string]any)["prompt_cache_breakpoint"].(map[string]any)
		if breakpoint["mode"] != "explicit" {
			t.Fatalf("content breakpoint = %#v", content)
		}
		_, _ = io.WriteString(w, `{"id":"chat_cache","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1200,"completion_tokens":1,"total_tokens":1201,"prompt_tokens_details":{"cached_tokens":1000,"cache_write_tokens":50}}}`)
	}))
	defer server.Close()

	provider := New(
		WithBaseURL(server.URL),
		WithBodyField("prompt_cache_key", "default-key"),
		WithBodyField("cache_control", map[string]any{"type": "ephemeral", "ttl": "1h"}),
	)
	response, err := provider.Chat(context.Background(), llmprovider.ChatRequest{
		Messages: []llmprovider.Message{{
			Role: llmprovider.RoleSystem,
			ContentParts: []llmprovider.MessageContentPart{{
				"type": "text", "text": "stable instructions",
				"prompt_cache_breakpoint": map[string]any{"mode": "explicit"},
			}},
		}},
		Extra: map[string]any{"prompt_cache_key": "request-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Usage.PromptDetails == nil || response.Usage.PromptDetails.CachedTokens != 1000 ||
		response.Usage.PromptDetails.CacheWriteTokens != 50 {
		t.Fatalf("cache usage = %#v", response.Usage)
	}
}

func TestChatStreamToolCallFragments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"chat_1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\"}}]}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"chat_1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"Seoul\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()

	client := llmprovider.New(New(WithBaseURL(server.URL)))
	stream, err := client.ChatStream(context.Background(), llmprovider.ChatRequest{Messages: []llmprovider.Message{{Role: llmprovider.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	first, err := stream.Recv()
	if err != nil || first.Choices[0].Delta.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("first = %#v, err = %v", first, err)
	}
	second, err := stream.Recv()
	if err != nil || second.Choices[0].Delta.ToolCalls[0].Function.Arguments != `"Seoul"}` {
		t.Fatalf("second = %#v, err = %v", second, err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("final error = %v", err)
	}
}

func TestAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"bad request","type":"invalid_request_error","code":"bad"}}`)
	}))
	defer server.Close()

	client := llmprovider.New(New(WithBaseURL(server.URL)))
	_, err := client.Chat(context.Background(), llmprovider.ChatRequest{Messages: []llmprovider.Message{{Role: llmprovider.RoleUser, Content: "hi"}}})
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.StatusCode != http.StatusBadRequest {
		t.Fatalf("error = %T %v", err, err)
	}
}
