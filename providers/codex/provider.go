package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	llmprovider "github.com/snowmerak/llm-provider"
)

// RPCError is an error response returned by Codex App Server.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("codex: JSON-RPC error %d: %s", e.Code, e.Message)
}

type wireMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

type rpcResult struct {
	result json.RawMessage
	err    error
}

// Provider owns one Codex App Server process and supports concurrent threads.
// The process is started lazily by the first request.
type Provider struct {
	config config

	startMu   sync.Mutex
	transport transport
	started   bool
	closed    bool
	readDone  chan struct{}
	readErr   error

	mu        sync.Mutex
	pending   map[int64]chan rpcResult
	turns     map[string]*turnState
	active    map[string]*turnState
	loaded    map[string]bool
	nextID    atomic.Int64
	closeOnce sync.Once
}

func New(opts ...Option) *Provider {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	return &Provider{
		config:   cfg,
		readDone: make(chan struct{}),
		pending:  make(map[int64]chan rpcResult),
		turns:    make(map[string]*turnState),
		active:   make(map[string]*turnState),
		loaded:   make(map[string]bool),
	}
}

// ListModels discovers the picker-visible models from Codex App Server.
func (p *Provider) ListModels(ctx context.Context) ([]llmprovider.Model, error) {
	type appServerModel struct {
		ID    string `json:"id"`
		Model string `json:"model"`
	}
	models := make([]llmprovider.Model, 0)
	var cursor string
	for {
		params := map[string]any{"limit": 100, "includeHidden": false}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var response struct {
			Data       []appServerModel `json:"data"`
			NextCursor *string          `json:"nextCursor"`
		}
		if err := p.Call(ctx, "model/list", params, &response); err != nil {
			return nil, err
		}
		for _, model := range response.Data {
			id := firstNonEmpty(model.Model, model.ID)
			if id != "" {
				models = append(models, llmprovider.Model{ID: id, Object: "model", OwnedBy: "codex"})
			}
		}
		if response.NextCursor == nil || *response.NextCursor == "" {
			break
		}
		cursor = *response.NextCursor
	}
	return models, nil
}

func (p *Provider) Chat(ctx context.Context, request llmprovider.ChatRequest) (*llmprovider.ChatResponse, error) {
	stream, err := p.ChatStream(ctx, request)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	response := &llmprovider.ChatResponse{
		Object: "chat.completion",
		Model:  firstNonEmpty(request.Model, p.config.model),
		Choices: []llmprovider.Choice{{
			Index:   0,
			Message: llmprovider.Message{Role: llmprovider.RoleAssistant},
		}},
	}
	var allContent, finalContent string
	for {
		chunk, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return nil, recvErr
		}
		if response.ID == "" {
			response.ID = chunk.ID
		}
		response.ConversationID = chunk.ConversationID
		if len(chunk.Choices) > 0 {
			choice := chunk.Choices[0]
			if choice.Delta != nil {
				allContent += choice.Delta.Content
				if choice.Phase == "final_answer" {
					finalContent += choice.Delta.Content
				}
			}
			if choice.FinishReason != "" {
				response.Choices[0].FinishReason = choice.FinishReason
			}
		}
		if chunk.Usage != nil {
			response.Usage = *chunk.Usage
		}
	}
	response.Choices[0].Message.Content = allContent
	if finalContent != "" {
		response.Choices[0].Message.Content = finalContent
	}
	return response, nil
}

func (p *Provider) ChatStream(ctx context.Context, request llmprovider.ChatRequest) (llmprovider.Stream, error) {
	lastUser := lastUserMessage(request.Messages)
	if lastUser < 0 {
		return nil, errors.New("codex: at least one user message is required")
	}
	if lastUser != len(request.Messages)-1 {
		return nil, errors.New("codex: the last message must have the user role")
	}
	if err := p.ensureStarted(ctx); err != nil {
		return nil, err
	}

	threadID, isNew, err := p.prepareThread(ctx, request)
	if err != nil {
		return nil, err
	}
	if isNew {
		if err := p.injectHistory(ctx, threadID, request.Messages[:lastUser]); err != nil {
			return nil, err
		}
	}

	state := newTurnState(ctx, p, threadID, firstNonEmpty(request.Model, p.config.model), request.ToolHandler)
	p.mu.Lock()
	if previous := p.active[threadID]; previous != nil {
		p.mu.Unlock()
		return nil, fmt.Errorf("codex: thread %s already has an active turn", threadID)
	}
	p.active[threadID] = state
	p.mu.Unlock()

	params := map[string]any{
		"threadId": threadID,
		"input":    []map[string]any{{"type": "text", "text": request.Messages[lastUser].Content}},
	}
	if model := firstNonEmpty(request.Model, p.config.model); model != "" {
		params["model"] = model
	}
	if cwd := firstNonEmpty(request.WorkingDirectory, p.config.cwd); cwd != "" {
		params["cwd"] = cwd
	}
	if request.ReasoningEffort != "" {
		params["effort"] = request.ReasoningEffort
	}
	if request.OutputSchema != nil {
		params["outputSchema"] = request.OutputSchema
	}

	var response struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := p.Call(ctx, "turn/start", params, &response); err != nil {
		p.removeTurn(state)
		state.finish(err)
		return nil, err
	}
	if response.Turn.ID == "" {
		err := errors.New("codex: turn/start returned an empty turn ID")
		p.removeTurn(state)
		state.finish(err)
		return nil, err
	}
	state.setTurnID(response.Turn.ID)
	p.mu.Lock()
	p.turns[response.Turn.ID] = state
	p.mu.Unlock()
	return &codexStream{state: state}, nil
}

func (p *Provider) prepareThread(ctx context.Context, request llmprovider.ChatRequest) (string, bool, error) {
	dynamicTools, includeTools, err := p.dynamicTools(request)
	if err != nil {
		return "", false, err
	}
	if request.ConversationID != "" {
		p.mu.Lock()
		loaded := p.loaded[request.ConversationID]
		p.mu.Unlock()
		if !loaded || includeTools {
			params := map[string]any{"threadId": request.ConversationID}
			if includeTools {
				params["dynamicTools"] = dynamicTools
			}
			var response threadResponse
			if err := p.Call(ctx, "thread/resume", params, &response); err != nil {
				return "", false, err
			}
			if response.Thread.ID == "" {
				return "", false, errors.New("codex: thread/resume returned an empty thread ID")
			}
			p.mu.Lock()
			p.loaded[response.Thread.ID] = true
			p.mu.Unlock()
		}
		return request.ConversationID, false, nil
	}

	params := map[string]any{
		"approvalPolicy": string(p.config.approvalPolicy),
		"sandbox":        string(p.config.sandbox),
		"ephemeral":      p.config.ephemeral,
	}
	if p.config.baseInstructions != "" {
		params["baseInstructions"] = p.config.baseInstructions
	}
	if model := firstNonEmpty(request.Model, p.config.model); model != "" {
		params["model"] = model
	}
	if cwd := firstNonEmpty(request.WorkingDirectory, p.config.cwd); cwd != "" {
		params["cwd"] = cwd
	}
	if instructions := developerInstructions(request.Messages); instructions != "" {
		params["developerInstructions"] = instructions
	}
	if p.config.serviceName != "" {
		params["serviceName"] = p.config.serviceName
	}
	if includeTools {
		params["dynamicTools"] = dynamicTools
	}
	var response threadResponse
	if err := p.Call(ctx, "thread/start", params, &response); err != nil {
		return "", false, err
	}
	if response.Thread.ID == "" {
		return "", false, errors.New("codex: thread/start returned an empty thread ID")
	}
	p.mu.Lock()
	p.loaded[response.Thread.ID] = true
	p.mu.Unlock()
	return response.Thread.ID, true, nil
}

func (p *Provider) dynamicTools(request llmprovider.ChatRequest) ([]map[string]any, bool, error) {
	if len(request.Tools) == 0 {
		return nil, false, nil
	}
	if !p.config.experimentalAPI {
		return nil, false, errors.New("codex: dynamic tools require the experimental API capability")
	}
	switch choice := request.ToolChoice.(type) {
	case nil:
	case llmprovider.ToolChoiceMode:
		if choice == llmprovider.ToolChoiceNone {
			return []map[string]any{}, true, nil
		}
		if choice != "" && choice != llmprovider.ToolChoiceAuto {
			return nil, false, fmt.Errorf("codex: tool choice %q is not supported; use auto or none", choice)
		}
	case string:
		if choice == string(llmprovider.ToolChoiceNone) {
			return []map[string]any{}, true, nil
		}
		if choice != "" && choice != string(llmprovider.ToolChoiceAuto) {
			return nil, false, fmt.Errorf("codex: tool choice %q is not supported; use auto or none", choice)
		}
	default:
		return nil, false, errors.New("codex: named or structured tool choice is not supported")
	}
	if request.ToolHandler == nil {
		return nil, false, errors.New("codex: ToolHandler is required when dynamic tools are provided")
	}
	tools := make([]map[string]any, 0, len(request.Tools))
	for _, tool := range request.Tools {
		if tool.Type != llmprovider.ToolTypeFunction {
			return nil, false, fmt.Errorf("codex: unsupported tool type %q", tool.Type)
		}
		if tool.Function.Name == "" {
			return nil, false, errors.New("codex: tool function name is required")
		}
		description := tool.Function.Description
		if description == "" {
			description = tool.Function.Name
		}
		inputSchema := any(tool.Function.Parameters)
		if inputSchema == nil {
			inputSchema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		tools = append(tools, map[string]any{
			"type": "function", "name": tool.Function.Name,
			"description": description, "inputSchema": inputSchema,
		})
	}
	return tools, true, nil
}

type threadResponse struct {
	Thread struct {
		ID string `json:"id"`
	} `json:"thread"`
}

func (p *Provider) injectHistory(ctx context.Context, threadID string, messages []llmprovider.Message) error {
	items := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		if message.Role == llmprovider.RoleSystem || message.Role == llmprovider.RoleDeveloper {
			continue
		}
		if message.Role == llmprovider.RoleTool {
			items = append(items, map[string]any{
				"type": "function_call_output", "call_id": message.ToolCallID, "output": message.Content,
			})
			continue
		}
		role := string(message.Role)
		contentType := "input_text"
		if message.Role == llmprovider.RoleAssistant {
			contentType = "output_text"
		} else if message.Role != llmprovider.RoleUser {
			role = "user"
		}
		if message.Content != "" {
			items = append(items, map[string]any{
				"type":    "message",
				"role":    role,
				"content": []map[string]any{{"type": contentType, "text": message.Content}},
			})
		}
		for _, call := range message.ToolCalls {
			items = append(items, map[string]any{
				"type": "function_call", "call_id": call.ID,
				"name": call.Function.Name, "arguments": call.Function.Arguments,
			})
		}
	}
	if len(items) == 0 {
		return nil
	}
	return p.Call(ctx, "thread/inject_items", map[string]any{"threadId": threadID, "items": items}, nil)
}

// Call invokes any App Server JSON-RPC method. A nil result discards the
// result. This is the escape hatch for APIs beyond the common chat surface.
func (p *Provider) Call(ctx context.Context, method string, params, result any) error {
	if err := p.ensureStarted(ctx); err != nil {
		return err
	}
	raw, err := p.callStarted(ctx, method, params)
	if err != nil {
		return err
	}
	if result != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, result); err != nil {
			return fmt.Errorf("codex: decode %s result: %w", method, err)
		}
	}
	return nil
}

// Notify sends a JSON-RPC notification after ensuring App Server is ready.
func (p *Provider) Notify(ctx context.Context, method string, params any) error {
	if err := p.ensureStarted(ctx); err != nil {
		return err
	}
	return p.notifyStarted(method, params)
}

func (p *Provider) ensureStarted(ctx context.Context) error {
	p.startMu.Lock()
	defer p.startMu.Unlock()
	if p.closed {
		return errors.New("codex: provider is closed")
	}
	if p.started {
		return nil
	}
	factory := p.config.transportFactoryForTest
	if factory == nil {
		factory = func() (transport, error) { return startProcessTransport(p.config) }
	}
	transport, err := factory()
	if err != nil {
		return err
	}
	p.transport = transport
	p.started = true
	go p.readLoop()

	params := map[string]any{"clientInfo": map[string]string{
		"name": p.config.clientName, "title": p.config.clientTitle, "version": p.config.clientVersion,
	}}
	if p.config.experimentalAPI {
		params["capabilities"] = map[string]any{"experimentalApi": true}
	}
	if _, err := p.callStarted(ctx, "initialize", params); err != nil {
		_ = p.transport.Close()
		return err
	}
	if err := p.notifyStarted("initialized", map[string]any{}); err != nil {
		_ = p.transport.Close()
		return err
	}
	return nil
}

func (p *Provider) callStarted(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := p.nextID.Add(1)
	message, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return nil, fmt.Errorf("codex: encode %s request: %w", method, err)
	}
	response := make(chan rpcResult, 1)
	p.mu.Lock()
	p.pending[id] = response
	p.mu.Unlock()
	if err := p.transport.WriteMessage(message); err != nil {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return nil, fmt.Errorf("codex: write %s request: %w", method, err)
	}
	select {
	case value := <-response:
		return value.result, value.err
	case <-ctx.Done():
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return nil, ctx.Err()
	case <-p.readDone:
		return nil, p.connectionError()
	}
}

func (p *Provider) notifyStarted(method string, params any) error {
	message, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err != nil {
		return fmt.Errorf("codex: encode %s notification: %w", method, err)
	}
	if err := p.transport.WriteMessage(message); err != nil {
		return fmt.Errorf("codex: write %s notification: %w", method, err)
	}
	return nil
}

func (p *Provider) readLoop() {
	for {
		data, err := p.transport.ReadMessage()
		if err != nil {
			p.failConnection(err)
			return
		}
		var message wireMessage
		if err := json.Unmarshal(data, &message); err != nil {
			p.failConnection(fmt.Errorf("decode server message: %w", err))
			return
		}
		if len(message.ID) > 0 && message.Method != "" {
			go p.handleServerRequest(message)
			continue
		}
		if len(message.ID) > 0 {
			var id int64
			if err := json.Unmarshal(message.ID, &id); err != nil {
				continue
			}
			p.mu.Lock()
			pending := p.pending[id]
			delete(p.pending, id)
			p.mu.Unlock()
			if pending != nil {
				if message.Error != nil {
					pending <- rpcResult{err: message.Error}
				} else {
					pending <- rpcResult{result: message.Result}
				}
			}
			continue
		}
		if message.Method != "" {
			p.handleNotification(message.Method, message.Params)
		}
	}
}

func (p *Provider) handleServerRequest(message wireMessage) {
	if message.Method == "item/tool/call" {
		p.handleDynamicToolCall(message)
		return
	}
	var result any
	var handlerErr error
	if p.config.requestHandler == nil {
		handlerErr = fmt.Errorf("no handler for server request %s", message.Method)
	} else {
		result, handlerErr = p.config.requestHandler(message.Method, message.Params)
	}
	p.respondToServer(message.ID, result, handlerErr)
}

func (p *Provider) handleDynamicToolCall(message wireMessage) {
	var params struct {
		Arguments json.RawMessage `json:"arguments"`
		CallID    string          `json:"callId"`
		ThreadID  string          `json:"threadId"`
		Tool      string          `json:"tool"`
		TurnID    string          `json:"turnId"`
	}
	if err := json.Unmarshal(message.Params, &params); err != nil {
		p.respondToServer(message.ID, nil, fmt.Errorf("invalid dynamic tool call: %w", err))
		return
	}
	p.mu.Lock()
	state := p.turns[params.TurnID]
	if state == nil {
		state = p.active[params.ThreadID]
	}
	p.mu.Unlock()
	result := llmprovider.ToolResult{IsError: true, Content: "no handler for dynamic tool call"}
	if state != nil && state.toolHandler != nil {
		call := llmprovider.ToolCall{
			ID: params.CallID, Type: llmprovider.ToolTypeFunction,
			Function: llmprovider.FunctionCall{Name: params.Tool, Arguments: string(params.Arguments)},
		}
		value, err := state.toolHandler(state.ctx, call)
		if err != nil {
			result = llmprovider.ToolResult{IsError: true, Content: err.Error()}
		} else {
			result = value
		}
	}
	p.respondToServer(message.ID, map[string]any{
		"success":      !result.IsError,
		"contentItems": []map[string]any{{"type": "inputText", "text": result.Content}},
	}, nil)
}

func (p *Provider) respondToServer(id json.RawMessage, result any, responseErr error) {
	response := map[string]any{"id": json.RawMessage(id)}
	if responseErr != nil {
		response["error"] = map[string]any{"code": -32601, "message": responseErr.Error()}
	} else {
		response["result"] = result
	}
	data, err := json.Marshal(response)
	if err == nil {
		_ = p.transport.WriteMessage(data)
	}
}

func (p *Provider) handleNotification(method string, params json.RawMessage) {
	var ids struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
	}
	_ = json.Unmarshal(params, &ids)
	p.mu.Lock()
	state := p.turns[ids.TurnID]
	if state == nil {
		state = p.active[ids.ThreadID]
	}
	p.mu.Unlock()
	if state == nil {
		return
	}
	if ids.TurnID != "" {
		state.setTurnIDIfEmpty(ids.TurnID)
	}

	switch method {
	case "item/agentMessage/delta":
		var event struct {
			Delta  string `json:"delta"`
			ItemID string `json:"itemId"`
		}
		if json.Unmarshal(params, &event) == nil {
			state.markDelta(event.ItemID)
			phase := state.itemPhase(event.ItemID)
			state.enqueue(&llmprovider.ChatChunk{
				ID: state.id(), Object: "chat.completion.chunk", Model: state.model,
				ConversationID: state.threadID,
				Choices:        []llmprovider.Choice{{Index: 0, Delta: &llmprovider.Message{Role: llmprovider.RoleAssistant, Content: event.Delta}, Phase: phase}},
			})
		}
	case "item/started":
		var event struct {
			Item struct {
				ID    string `json:"id"`
				Type  string `json:"type"`
				Phase string `json:"phase"`
			} `json:"item"`
		}
		if json.Unmarshal(params, &event) == nil && event.Item.Type == "agentMessage" {
			state.setItemPhase(event.Item.ID, event.Item.Phase)
		}
	case "item/completed":
		var event struct {
			Item struct {
				ID    string `json:"id"`
				Type  string `json:"type"`
				Text  string `json:"text"`
				Phase string `json:"phase"`
			} `json:"item"`
		}
		if json.Unmarshal(params, &event) == nil && event.Item.Type == "agentMessage" && !state.hasDelta(event.Item.ID) && event.Item.Text != "" {
			state.setItemPhase(event.Item.ID, event.Item.Phase)
			state.enqueue(&llmprovider.ChatChunk{
				ID: state.id(), Object: "chat.completion.chunk", Model: state.model,
				ConversationID: state.threadID,
				Choices:        []llmprovider.Choice{{Index: 0, Delta: &llmprovider.Message{Role: llmprovider.RoleAssistant, Content: event.Item.Text}, Phase: event.Item.Phase}},
			})
		}
	case "thread/tokenUsage/updated":
		var event struct {
			TokenUsage struct {
				Last struct {
					InputTokens  int `json:"inputTokens"`
					OutputTokens int `json:"outputTokens"`
					TotalTokens  int `json:"totalTokens"`
				} `json:"last"`
			} `json:"tokenUsage"`
		}
		if json.Unmarshal(params, &event) == nil {
			state.setUsage(llmprovider.Usage{PromptTokens: event.TokenUsage.Last.InputTokens, CompletionTokens: event.TokenUsage.Last.OutputTokens, TotalTokens: event.TokenUsage.Last.TotalTokens})
		}
	case "error":
		var event struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(params, &event) == nil && event.Error.Message != "" {
			state.setTurnError(errors.New("codex: " + event.Error.Message))
		}
	case "turn/completed":
		var event struct {
			Turn struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		}
		if json.Unmarshal(params, &event) != nil {
			return
		}
		finishReason := "stop"
		var turnErr error
		switch event.Turn.Status {
		case "interrupted":
			finishReason = "cancelled"
		case "failed":
			if event.Turn.Error != nil && event.Turn.Error.Message != "" {
				turnErr = errors.New("codex: " + event.Turn.Error.Message)
			} else {
				turnErr = state.turnError()
			}
			if turnErr == nil {
				turnErr = errors.New("codex: turn failed")
			}
		}
		if turnErr == nil {
			usage := state.usageValue()
			state.enqueue(&llmprovider.ChatChunk{
				ID: state.id(), Object: "chat.completion.chunk", Model: state.model,
				ConversationID: state.threadID, Usage: &usage,
				Choices: []llmprovider.Choice{{Index: 0, Delta: &llmprovider.Message{}, FinishReason: finishReason}},
			})
		}
		p.removeTurn(state)
		state.finish(turnErr)
	}
}

func (p *Provider) removeTurn(state *turnState) {
	p.mu.Lock()
	delete(p.active, state.threadID)
	if id := state.id(); id != "" {
		delete(p.turns, id)
	}
	p.mu.Unlock()
}

func (p *Provider) interrupt(threadID, turnID string) {
	if turnID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = p.Call(ctx, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID}, nil)
}

func (p *Provider) failConnection(err error) {
	p.mu.Lock()
	p.readErr = err
	for id, pending := range p.pending {
		pending <- rpcResult{err: fmt.Errorf("codex: app-server connection: %w", err)}
		delete(p.pending, id)
	}
	states := make(map[*turnState]struct{})
	for _, state := range p.active {
		states[state] = struct{}{}
	}
	p.mu.Unlock()
	for state := range states {
		state.finish(fmt.Errorf("codex: app-server connection: %w", err))
	}
	close(p.readDone)
}

func (p *Provider) connectionError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.readErr == nil {
		return errors.New("codex: app-server connection closed")
	}
	return fmt.Errorf("codex: app-server connection: %w", p.readErr)
}

func (p *Provider) Close() error {
	var closeErr error
	p.closeOnce.Do(func() {
		p.startMu.Lock()
		p.closed = true
		if p.transport != nil {
			closeErr = p.transport.Close()
		}
		p.startMu.Unlock()
	})
	return closeErr
}

func developerInstructions(messages []llmprovider.Message) string {
	var instructions []string
	for _, message := range messages {
		if message.Role == llmprovider.RoleSystem || message.Role == llmprovider.RoleDeveloper {
			instructions = append(instructions, message.Content)
		}
	}
	return strings.Join(instructions, "\n\n")
}

func lastUserMessage(messages []llmprovider.Message) int {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role == llmprovider.RoleUser {
			return index
		}
	}
	return -1
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

var _ llmprovider.Provider = (*Provider)(nil)
var _ llmprovider.ModelLister = (*Provider)(nil)
