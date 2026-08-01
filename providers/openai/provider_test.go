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
			Messages []llmprovider.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Messages) != 3 || body.Messages[1].ToolCalls[0].ID != "call_1" || body.Messages[2].ToolCallID != "call_1" {
			t.Fatalf("messages = %#v", body.Messages)
		}
		_, _ = io.WriteString(w, `{"id":"chat_2","choices":[{"index":0,"message":{"role":"assistant","content":"sunny"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	client := llmprovider.New(New(WithBaseURL(server.URL)))
	_, err := client.Chat(context.Background(), llmprovider.ChatRequest{Messages: []llmprovider.Message{
		{Role: llmprovider.RoleUser, Content: "weather?"},
		{Role: llmprovider.RoleAssistant, ToolCalls: []llmprovider.ToolCall{{ID: "call_1", Type: llmprovider.ToolTypeFunction, Function: llmprovider.FunctionCall{Name: "get_weather", Arguments: `{}`}}}},
		{Role: llmprovider.RoleTool, ToolCallID: "call_1", Content: `{"weather":"sunny"}`},
	}})
	if err != nil {
		t.Fatal(err)
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
