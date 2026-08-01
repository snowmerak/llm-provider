package llmprovider

import (
	"context"
	"encoding/json"
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
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
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

// ToolHandler executes a Codex dynamic function call. OpenAI-compatible
// providers return tool calls to the caller instead of invoking this handler.
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

	// ConversationID continues a stateful provider conversation. The
	// OpenAI-compatible provider ignores it. The Codex provider treats it as a
	// Codex thread ID.
	ConversationID string `json:"-"`
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
	ConversationID string          `json:"-"`
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
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
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
	ConversationID string          `json:"-"`
	Raw            json.RawMessage `json:"-"`
}

// Stream returns chunks in order. Recv returns io.EOF after the final chunk.
type Stream interface {
	Recv() (*ChatChunk, error)
	Close() error
}

// Provider is the minimal contract implemented by LLM backends.
type Provider interface {
	Chat(context.Context, ChatRequest) (*ChatResponse, error)
	ChatStream(context.Context, ChatRequest) (Stream, error)
	Close() error
}
