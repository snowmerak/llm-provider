package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	llmprovider "github.com/snowmerak/llm-provider"
)

func TestDefaultCommandStartsCodexAppServer(t *testing.T) {
	cfg := defaultConfig()
	if cfg.command != "codex" || !slices.Equal(cfg.args, []string{"app-server", "--listen", "stdio://"}) {
		t.Fatalf("unexpected command: %q %#v", cfg.command, cfg.args)
	}
}

type fakeTransport struct {
	reads     chan []byte
	writes    chan []byte
	closed    chan struct{}
	closeOnce sync.Once
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		reads:  make(chan []byte, 32),
		writes: make(chan []byte, 32),
		closed: make(chan struct{}),
	}
}

func (t *fakeTransport) ReadMessage() ([]byte, error) {
	select {
	case message := <-t.reads:
		return message, nil
	case <-t.closed:
		return nil, io.EOF
	}
}

func (t *fakeTransport) WriteMessage(message []byte) error {
	select {
	case t.writes <- append([]byte(nil), message...):
		return nil
	case <-t.closed:
		return io.ErrClosedPipe
	}
}

func (t *fakeTransport) Close() error {
	t.closeOnce.Do(func() { close(t.closed) })
	return nil
}

func (t *fakeTransport) send(message any) {
	data, err := json.Marshal(message)
	if err != nil {
		panic(err)
	}
	t.reads <- data
}

func TestChatAdaptsCodexProtocol(t *testing.T) {
	fake := newFakeTransport()
	provider := New(func(config *config) {
		config.transportFactoryForTest = func() (transport, error) { return fake, nil }
	})
	defer provider.Close()

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- serveChat(fake)
	}()

	client := llmprovider.NewWithProvider(provider)
	response, err := client.Chat(context.Background(), llmprovider.ChatRequest{
		Model: "test-model",
		Messages: []llmprovider.Message{
			{Role: llmprovider.RoleSystem, Content: "Be concise."},
			{Role: llmprovider.RoleUser, Content: "Earlier question"},
			{Role: llmprovider.RoleAssistant, Content: "Earlier answer"},
			{Role: llmprovider.RoleUser, Content: "Current question"},
		},
		ReasoningEffort: "medium",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := response.Choices[0].Message.Content; got != "Hello from Codex" {
		t.Fatalf("content = %q", got)
	}
	if response.ConversationID != "thread_1" || response.ID != "turn_1" {
		t.Fatalf("ids = %q, %q", response.ConversationID, response.ID)
	}
	if response.Usage.TotalTokens != 15 || response.Choices[0].FinishReason != "stop" {
		t.Fatalf("response = %#v", response)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func serveChat(transport *fakeTransport) error {
	for step := 0; step < 5; {
		select {
		case data := <-transport.writes:
			var request struct {
				ID     int64          `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if err := json.Unmarshal(data, &request); err != nil {
				return err
			}
			switch request.Method {
			case "initialize":
				transport.send(map[string]any{"id": request.ID, "result": map[string]any{"userAgent": "test"}})
				step++
			case "initialized":
				step++
			case "thread/start":
				if request.Params["sandbox"] != "read-only" || request.Params["approvalPolicy"] != "never" || request.Params["ephemeral"] != true {
					return errors.New("unexpected safe defaults")
				}
				if request.Params["developerInstructions"] != "Be concise." {
					return errors.New("system message was not mapped to developer instructions")
				}
				transport.send(map[string]any{"id": request.ID, "result": map[string]any{"thread": map[string]any{"id": "thread_1"}}})
				step++
			case "thread/inject_items":
				items, ok := request.Params["items"].([]any)
				if !ok || len(items) != 2 {
					return errors.New("chat history was not injected")
				}
				transport.send(map[string]any{"id": request.ID, "result": map[string]any{}})
				step++
			case "turn/start":
				if request.Params["effort"] != "medium" {
					return errors.New("reasoning effort was not forwarded")
				}
				transport.send(map[string]any{"id": request.ID, "result": map[string]any{"turn": map[string]any{"id": "turn_1", "status": "inProgress"}}})
				transport.send(map[string]any{"method": "item/started", "params": map[string]any{"threadId": "thread_1", "turnId": "turn_1", "item": map[string]any{"id": "item_0", "type": "agentMessage", "text": "", "phase": "commentary"}}})
				transport.send(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "thread_1", "turnId": "turn_1", "itemId": "item_0", "delta": "Working..."}})
				transport.send(map[string]any{"method": "item/started", "params": map[string]any{"threadId": "thread_1", "turnId": "turn_1", "item": map[string]any{"id": "item_1", "type": "agentMessage", "text": "", "phase": "final_answer"}}})
				transport.send(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "thread_1", "turnId": "turn_1", "itemId": "item_1", "delta": "Hello "}})
				transport.send(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "thread_1", "turnId": "turn_1", "itemId": "item_1", "delta": "from Codex"}})
				transport.send(map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"threadId": "thread_1", "turnId": "turn_1", "tokenUsage": map[string]any{"last": map[string]any{"inputTokens": 10, "outputTokens": 5, "totalTokens": 15}}}})
				transport.send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread_1", "turn": map[string]any{"id": "turn_1", "status": "completed"}}})
				step++
			default:
				return errors.New("unexpected method: " + request.Method)
			}
		case <-time.After(5 * time.Second):
			return errors.New("timed out waiting for client request")
		}
	}
	return nil
}

func TestCallReturnsRPCError(t *testing.T) {
	fake := newFakeTransport()
	provider := New(func(config *config) {
		config.transportFactoryForTest = func() (transport, error) { return fake, nil }
	})
	defer provider.Close()

	go func() {
		for data := range fake.writes {
			var request struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			_ = json.Unmarshal(data, &request)
			if request.Method == "initialize" {
				fake.send(map[string]any{"id": request.ID, "result": map[string]any{}})
			} else if request.Method == "model/list" {
				fake.send(map[string]any{"id": request.ID, "error": map[string]any{"code": -32602, "message": "bad params"}})
				return
			}
		}
	}()

	var result any
	err := provider.Call(context.Background(), "model/list", map[string]any{}, &result)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32602 {
		t.Fatalf("error = %T %v", err, err)
	}
}

func TestChatExecutesDynamicTool(t *testing.T) {
	fake := newFakeTransport()
	provider := New(WithBaseInstructions("Use actual tools."), func(config *config) {
		config.transportFactoryForTest = func() (transport, error) { return fake, nil }
	})
	defer provider.Close()
	serverErr := make(chan error, 1)
	go func() { serverErr <- serveDynamicToolChat(fake) }()

	client := llmprovider.New(provider)
	response, err := client.Chat(context.Background(), llmprovider.ChatRequest{
		Messages: []llmprovider.Message{{Role: llmprovider.RoleUser, Content: "Seoul weather?"}},
		Tools: []llmprovider.Tool{{Type: llmprovider.ToolTypeFunction, Function: llmprovider.FunctionDefinition{
			Name: "get_weather", Description: "Get weather",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
		}}},
		ToolHandler: func(ctx context.Context, call llmprovider.ToolCall) (llmprovider.ToolResult, error) {
			if call.ID != "call_1" || call.Function.Name != "get_weather" || call.Function.Arguments != `{"city":"Seoul"}` {
				return llmprovider.ToolResult{}, errors.New("unexpected tool call")
			}
			return llmprovider.ToolResult{Content: `{"weather":"sunny"}`}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := response.Choices[0].Message.Content; got != "Weather is sunny" {
		t.Fatalf("content = %q", got)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func serveDynamicToolChat(transport *fakeTransport) error {
	for {
		select {
		case data := <-transport.writes:
			var request struct {
				ID     int64          `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
				Result map[string]any `json:"result"`
			}
			if err := json.Unmarshal(data, &request); err != nil {
				return err
			}
			switch request.Method {
			case "initialize":
				caps, _ := request.Params["capabilities"].(map[string]any)
				if caps["experimentalApi"] != true {
					return errors.New("experimental API was not enabled")
				}
				transport.send(map[string]any{"id": request.ID, "result": map[string]any{}})
			case "initialized":
			case "thread/start":
				if request.Params["baseInstructions"] != "Use actual tools." {
					return errors.New("base instructions were not forwarded")
				}
				tools, ok := request.Params["dynamicTools"].([]any)
				if !ok || len(tools) != 1 {
					return errors.New("dynamic tools were not forwarded")
				}
				tool := tools[0].(map[string]any)
				if tool["name"] != "get_weather" || tool["type"] != "function" {
					return errors.New("dynamic tool has unexpected shape")
				}
				transport.send(map[string]any{"id": request.ID, "result": map[string]any{"thread": map[string]any{"id": "thread_tool"}}})
			case "turn/start":
				transport.send(map[string]any{"id": request.ID, "result": map[string]any{"turn": map[string]any{"id": "turn_tool", "status": "inProgress"}}})
				transport.send(map[string]any{"id": 900, "method": "item/tool/call", "params": map[string]any{
					"threadId": "thread_tool", "turnId": "turn_tool", "callId": "call_1",
					"tool": "get_weather", "arguments": map[string]any{"city": "Seoul"},
				}})
			case "":
				if request.ID != 900 || request.Result["success"] != true {
					return errors.New("dynamic tool response failed")
				}
				contentItems, ok := request.Result["contentItems"].([]any)
				if !ok || len(contentItems) != 1 || contentItems[0].(map[string]any)["text"] != `{"weather":"sunny"}` {
					return errors.New("unexpected dynamic tool result")
				}
				transport.send(map[string]any{"method": "item/started", "params": map[string]any{"threadId": "thread_tool", "turnId": "turn_tool", "item": map[string]any{"id": "answer", "type": "agentMessage", "phase": "final_answer"}}})
				transport.send(map[string]any{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "thread_tool", "turnId": "turn_tool", "itemId": "answer", "delta": "Weather is sunny"}})
				transport.send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread_tool", "turn": map[string]any{"id": "turn_tool", "status": "completed"}}})
				return nil
			default:
				return errors.New("unexpected method: " + request.Method)
			}
		case <-time.After(5 * time.Second):
			return errors.New("timed out waiting for dynamic tool flow")
		}
	}
}
