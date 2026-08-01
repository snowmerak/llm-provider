package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	llmprovider "github.com/snowmerak/llm-provider"
)

const delegatedToolSettleDelay = 15 * time.Millisecond

type pendingDelegatedTool struct {
	call      llmprovider.ToolCall
	requestID json.RawMessage
}

type turnState struct {
	ctx         context.Context
	provider    *Provider
	threadID    string
	model       string
	toolHandler llmprovider.ToolHandler

	mu               sync.Mutex
	turnID           string
	queue            []*llmprovider.ChatChunk
	done             bool
	err              error
	errDelivered     bool
	wake             chan struct{}
	closed           bool
	usage            llmprovider.Usage
	pendingError     error
	deltaItems       map[string]bool
	itemPhases       map[string]string
	delegated        []pendingDelegatedTool
	delegatedVersion uint64
	awaitingTools    bool
}

// addDelegatedTool parks an App Server callback so an OpenAI-compatible
// caller can execute it without interrupting the Codex turn.
func (s *turnState) addDelegatedTool(call llmprovider.ToolCall, requestID json.RawMessage) bool {
	s.mu.Lock()
	if s.done || s.closed {
		s.mu.Unlock()
		return false
	}
	call.Index = len(s.delegated)
	s.delegated = append(s.delegated, pendingDelegatedTool{
		call: call, requestID: append(json.RawMessage(nil), requestID...),
	})
	s.delegatedVersion++
	s.awaitingTools = true
	usage := s.usage
	chunk := &llmprovider.ChatChunk{
		ID: s.turnID, Object: "chat.completion.chunk", Model: s.model,
		ConversationID: s.threadID, Usage: &usage,
		Choices: []llmprovider.Choice{{
			Index: 0,
			Delta: &llmprovider.Message{
				Role: llmprovider.RoleAssistant, ToolCalls: []llmprovider.ToolCall{call},
			},
			FinishReason: "tool_calls",
		}},
	}
	s.queue = append(s.queue, chunk)
	s.mu.Unlock()
	s.signal()
	return true
}

func (s *turnState) delegatedTools() []llmprovider.ToolCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]llmprovider.ToolCall, 0, len(s.delegated))
	for _, pending := range s.delegated {
		result = append(result, pending.call)
	}
	return result
}

func (s *turnState) isAwaitingTools() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.awaitingTools && !s.done
}

func (s *turnState) resumeWithToolResults(messages []llmprovider.Message) ([]pendingDelegatedTool, []llmprovider.Message, error) {
	results := make(map[string]llmprovider.Message)
	for _, message := range messages {
		if message.Role == llmprovider.RoleTool && message.ToolCallID != "" {
			results[message.ToolCallID] = message
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done || !s.awaitingTools || len(s.delegated) == 0 {
		return nil, nil, errors.New("codex: thread is not waiting for delegated tool results")
	}
	pending := append([]pendingDelegatedTool(nil), s.delegated...)
	orderedResults := make([]llmprovider.Message, 0, len(pending))
	for _, item := range pending {
		result, ok := results[item.call.ID]
		if !ok {
			return nil, nil, fmt.Errorf("codex: missing result for delegated tool call %q", item.call.ID)
		}
		orderedResults = append(orderedResults, result)
	}
	s.delegated = nil
	s.awaitingTools = false
	return pending, orderedResults, nil
}

func newTurnState(ctx context.Context, provider *Provider, threadID, model string, toolHandler llmprovider.ToolHandler) *turnState {
	return &turnState{
		ctx: ctx, provider: provider, threadID: threadID, model: model, toolHandler: toolHandler,
		wake: make(chan struct{}, 1), deltaItems: make(map[string]bool), itemPhases: make(map[string]string),
	}
}

func (s *turnState) setTurnID(id string) {
	s.mu.Lock()
	s.turnID = id
	s.mu.Unlock()
}

func (s *turnState) setTurnIDIfEmpty(id string) {
	s.mu.Lock()
	if s.turnID == "" {
		s.turnID = id
	}
	s.mu.Unlock()
}

func (s *turnState) id() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnID
}

func (s *turnState) enqueue(chunk *llmprovider.ChatChunk) {
	s.mu.Lock()
	if !s.done && !s.closed {
		s.queue = append(s.queue, chunk)
	}
	s.mu.Unlock()
	s.signal()
}

func (s *turnState) finish(err error) {
	s.mu.Lock()
	if !s.done {
		s.done = true
		s.err = err
	}
	s.mu.Unlock()
	s.signal()
}

func (s *turnState) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *turnState) recv(ctx context.Context) (*llmprovider.ChatChunk, error) {
	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			chunk := s.queue[0]
			s.queue = s.queue[1:]
			s.mu.Unlock()
			return chunk, nil
		}
		if s.done || s.closed {
			if s.err != nil && !s.errDelivered {
				s.errDelivered = true
				err := s.err
				s.mu.Unlock()
				return nil, err
			}
			s.mu.Unlock()
			return nil, io.EOF
		}
		if s.awaitingTools {
			version := s.delegatedVersion
			s.mu.Unlock()
			timer := time.NewTimer(delegatedToolSettleDelay)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return nil, ctx.Err()
			case <-s.wake:
				if !timer.Stop() {
					<-timer.C
				}
				continue
			case <-timer.C:
				s.mu.Lock()
				settled := s.awaitingTools && version == s.delegatedVersion && len(s.queue) == 0
				s.mu.Unlock()
				if settled {
					return nil, io.EOF
				}
				continue
			}
		}
		s.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.wake:
		}
	}
}

func (s *turnState) close() error {
	s.mu.Lock()
	if s.awaitingTools && !s.done {
		s.mu.Unlock()
		return nil
	}
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	done := s.done
	threadID, turnID := s.threadID, s.turnID
	s.mu.Unlock()
	s.signal()
	if !done {
		go s.provider.interrupt(threadID, turnID)
		s.provider.removeTurn(s)
	}
	return nil
}

func (s *turnState) markDelta(itemID string) {
	s.mu.Lock()
	s.deltaItems[itemID] = true
	s.mu.Unlock()
}

func (s *turnState) hasDelta(itemID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deltaItems[itemID]
}

func (s *turnState) setItemPhase(itemID, phase string) {
	s.mu.Lock()
	s.itemPhases[itemID] = phase
	s.mu.Unlock()
}

func (s *turnState) itemPhase(itemID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.itemPhases[itemID]
}

func (s *turnState) setUsage(usage llmprovider.Usage) {
	s.mu.Lock()
	s.usage = usage
	s.mu.Unlock()
}

func (s *turnState) usageValue() llmprovider.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

func (s *turnState) setTurnError(err error) {
	s.mu.Lock()
	s.pendingError = err
	s.mu.Unlock()
}

func (s *turnState) turnError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingError
}

type codexStream struct {
	state *turnState
	ctx   context.Context
}

func (s *codexStream) Recv() (*llmprovider.ChatChunk, error) { return s.state.recv(s.ctx) }
func (s *codexStream) Close() error                          { return s.state.close() }

var _ llmprovider.Stream = (*codexStream)(nil)
