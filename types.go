package llmprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Role identifies the author of a chat message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is the common text-message subset supported by all providers.
type Message struct {
	Role Role `json:"role"`
	// Content is the common text form. ContentParts preserves structured
	// OpenAI-compatible content blocks, including provider cache breakpoints.
	// When ContentParts is non-empty it is encoded as the content field instead
	// of Content.
	Content      string               `json:"-"`
	ContentParts []MessageContentPart `json:"-"`
	Name         string               `json:"name,omitempty"`
	ToolCallID   string               `json:"tool_call_id,omitempty"`
	ToolCalls    []ToolCall           `json:"tool_calls,omitempty"`
}

// MessageContentPart retains provider-specific multimodal and cache-control
// fields without narrowing the OpenAI-compatible content block schema.
type MessageContentPart map[string]any

func (m Message) MarshalJSON() ([]byte, error) {
	type wireMessage struct {
		Role       Role       `json:"role"`
		Content    any        `json:"content,omitempty"`
		Name       string     `json:"name,omitempty"`
		ToolCallID string     `json:"tool_call_id,omitempty"`
		ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	}
	var content any
	if len(m.ContentParts) > 0 {
		content = m.ContentParts
	} else if m.Content != "" {
		content = m.Content
	}
	return json.Marshal(wireMessage{
		Role: m.Role, Content: content, Name: m.Name,
		ToolCallID: m.ToolCallID, ToolCalls: m.ToolCalls,
	})
}

func (m *Message) UnmarshalJSON(data []byte) error {
	type wireMessage struct {
		Role       Role            `json:"role"`
		Content    json.RawMessage `json:"content"`
		Name       string          `json:"name,omitempty"`
		ToolCallID string          `json:"tool_call_id,omitempty"`
		ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	}
	var wire wireMessage
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	m.Role, m.Name, m.ToolCallID, m.ToolCalls = wire.Role, wire.Name, wire.ToolCallID, wire.ToolCalls
	m.Content, m.ContentParts = "", nil
	if len(wire.Content) == 0 || string(wire.Content) == "null" {
		return nil
	}
	if wire.Content[0] == '"' {
		return json.Unmarshal(wire.Content, &m.Content)
	}
	if err := json.Unmarshal(wire.Content, &m.ContentParts); err != nil {
		return fmt.Errorf("message content must be a string or content-block array: %w", err)
	}
	return nil
}

// TextContent returns the text represented by either Content or supported
// text content blocks. It is used by providers such as Codex whose protocol
// accepts text at the common chat boundary.
func (m Message) TextContent() string {
	if len(m.ContentParts) == 0 {
		return m.Content
	}
	parts := make([]string, 0, len(m.ContentParts))
	for _, part := range m.ContentParts {
		typeName, _ := part["type"].(string)
		if typeName != "text" && typeName != "input_text" && typeName != "output_text" {
			continue
		}
		if value, ok := part["text"].(string); ok {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, "")
}

type ToolType string

const ToolTypeFunction ToolType = "function"

// FunctionDefinition describes a function with JSON Schema parameters.
type FunctionDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Strict      *bool          `json:"strict,omitempty"`
}

type Tool struct {
	Type     ToolType           `json:"type"`
	Function FunctionDefinition `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolCall represents both a complete call and a streamed call fragment.
// Index identifies fragments belonging to the same streamed call.
type ToolCall struct {
	Index    int          `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     ToolType     `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

type ToolChoiceMode string

const (
	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceRequired ToolChoiceMode = "required"
)

// ToolResult is returned by a Codex ToolHandler. Content may be plain text or
// JSON encoded as text. IsError tells the model that the call failed.
type ToolResult struct {
	Content string
	IsError bool
}

// ToolHandler executes a Codex dynamic function call in-process. When it is
// nil, the Codex provider delegates dynamic calls to the caller using the
// OpenAI tool_calls response shape.
type ToolHandler func(context.Context, ToolCall) (ToolResult, error)

// ChatRequest describes a chat completion. Extra fields are sent only by the
// OpenAI-compatible provider and cannot override the typed fields.
type ChatRequest struct {
	Model               string    `json:"model,omitempty"`
	Messages            []Message `json:"messages"`
	Temperature         *float64  `json:"temperature,omitempty"`
	TopP                *float64  `json:"top_p,omitempty"`
	MaxTokens           *int      `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int      `json:"max_completion_tokens,omitempty"`
	Stop                []string  `json:"stop,omitempty"`
	Tools               []Tool    `json:"tools,omitempty"`
	// ToolChoice accepts ToolChoiceMode or an OpenAI-compatible named-tool
	// object such as {"type":"function","function":{"name":"get_weather"}}.
	ToolChoice        any            `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool          `json:"parallel_tool_calls,omitempty"`
	Extra             map[string]any `json:"-"`
	// Headers are request-scoped transport headers. HTTP providers forward
	// them after applying provider defaults. Authorization, Content-Type, and
	// Accept remain provider-managed and cannot be overridden here.
	Headers http.Header `json:"-"`

	// ConversationID continues a stateful provider conversation. The
	// OpenAI-compatible provider forwards it as an extension field so gateways
	// can route stateful backends. The Codex provider treats it as a thread ID.
	ConversationID string `json:"conversation_id,omitempty"`
	// WorkingDirectory, ReasoningEffort, and OutputSchema are optional Codex
	// turn overrides. Other providers may ignore them.
	WorkingDirectory string      `json:"-"`
	ReasoningEffort  string      `json:"-"`
	OutputSchema     any         `json:"-"`
	ToolHandler      ToolHandler `json:"-"`
}

// NamedToolChoice returns an OpenAI-compatible choice that forces one
// function tool.
func NamedToolChoice(name string) map[string]any {
	return map[string]any{"type": "function", "function": map[string]string{"name": name}}
}

type ChatResponse struct {
	ID             string          `json:"id"`
	Object         string          `json:"object,omitempty"`
	Created        int64           `json:"created,omitempty"`
	Model          string          `json:"model,omitempty"`
	Choices        []Choice        `json:"choices"`
	Usage          Usage           `json:"usage,omitempty"`
	ConversationID string          `json:"conversation_id,omitempty"`
	Headers        http.Header     `json:"-"`
	Raw            json.RawMessage `json:"-"`
}

type Choice struct {
	Index        int      `json:"index"`
	Message      Message  `json:"message"`
	FinishReason string   `json:"finish_reason,omitempty"`
	Delta        *Message `json:"delta,omitempty"`
	// Phase is provider metadata. Codex uses "commentary" and
	// "final_answer"; OpenAI-compatible responses normally leave it empty.
	Phase string `json:"-"`
}

type Usage struct {
	PromptTokens     int           `json:"prompt_tokens,omitempty"`
	CompletionTokens int           `json:"completion_tokens,omitempty"`
	TotalTokens      int           `json:"total_tokens,omitempty"`
	PromptDetails    *TokenDetails `json:"prompt_tokens_details,omitempty"`
}

// TokenDetails normalizes prompt-cache accounting exposed by compatible
// providers while retaining the OpenAI field names on the wire.
type TokenDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

// ChatChunk uses the OpenAI chat.completion.chunk shape and also carries the
// provider conversation ID when one exists.
type ChatChunk struct {
	ID             string          `json:"id"`
	Object         string          `json:"object,omitempty"`
	Created        int64           `json:"created,omitempty"`
	Model          string          `json:"model,omitempty"`
	Choices        []Choice        `json:"choices"`
	Usage          *Usage          `json:"usage,omitempty"`
	ConversationID string          `json:"conversation_id,omitempty"`
	Raw            json.RawMessage `json:"-"`
}

// Model is the common OpenAI-compatible model-list representation.
type Model struct {
	ID            string `json:"id"`
	Object        string `json:"object,omitempty"`
	Created       int64  `json:"created,omitempty"`
	OwnedBy       string `json:"owned_by,omitempty"`
	ContextLength int64  `json:"context_length,omitempty"`
}

// UnmarshalJSON accepts the context-window field names commonly exposed by
// OpenAI-compatible servers and normalizes them to context_length on output.
func (m *Model) UnmarshalJSON(data []byte) error {
	type model Model
	var decoded model
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*m = Model(decoded)
	if m.ContextLength > 0 {
		return nil
	}
	m.ContextLength = 0
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, name := range []string{"max_model_len", "context_window", "context_window_tokens", "max_context_length"} {
		if value := positiveJSONInt(fields[name]); value > 0 {
			m.ContextLength = value
			break
		}
	}
	return nil
}

func positiveJSONInt(data json.RawMessage) int64 {
	if len(data) == 0 {
		return 0
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err == nil {
		value, _ := number.Int64()
		return value
	}
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		value, _ := strconv.ParseInt(text, 10, 64)
		return value
	}
	return 0
}

// ModelLister is an optional provider capability used by routing gateways.
type ModelLister interface {
	ListModels(context.Context) ([]Model, error)
}

// Stream returns chunks in order. Recv returns io.EOF after the final chunk.
type Stream interface {
	Recv() (*ChatChunk, error)
	Close() error
}

// ResponseHeaderer is an optional stream capability for HTTP response
// metadata such as provider cache status.
type ResponseHeaderer interface {
	ResponseHeaders() http.Header
}

// Provider is the minimal contract implemented by LLM backends.
type Provider interface {
	Chat(context.Context, ChatRequest) (*ChatResponse, error)
	ChatStream(context.Context, ChatRequest) (Stream, error)
	Close() error
}
