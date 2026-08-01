package codex

import (
	"context"
	"io"
	"sync"

	llmprovider "github.com/snowmerak/llm-provider"
)

type turnState struct {
	ctx         context.Context
	provider    *Provider
	threadID    string
	model       string
	toolHandler llmprovider.ToolHandler

	mu           sync.Mutex
	turnID       string
	queue        []*llmprovider.ChatChunk
	done         bool
	err          error
	errDelivered bool
	wake         chan struct{}
	closed       bool
	usage        llmprovider.Usage
	pendingError error
	deltaItems   map[string]bool
	itemPhases   map[string]string
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

func (s *turnState) recv() (*llmprovider.ChatChunk, error) {
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
		s.mu.Unlock()

		select {
		case <-s.ctx.Done():
			return nil, s.ctx.Err()
		case <-s.wake:
		}
	}
}

func (s *turnState) close() error {
	s.mu.Lock()
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
}

func (s *codexStream) Recv() (*llmprovider.ChatChunk, error) { return s.state.recv() }
func (s *codexStream) Close() error                          { return s.state.close() }

var _ llmprovider.Stream = (*codexStream)(nil)
