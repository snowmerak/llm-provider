package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	llmprovider "github.com/snowmerak/llm-provider"
)

func TestDelegatedParallelToolsAreCoalescedAndMatched(t *testing.T) {
	state := newTurnState(context.Background(), nil, "thread_parallel", "model", nil)
	state.setTurnID("turn_parallel")
	for index, id := range []string{"call_a", "call_b"} {
		ok := state.addDelegatedTool(llmprovider.ToolCall{
			ID: id, Type: llmprovider.ToolTypeFunction,
			Function: llmprovider.FunctionCall{Name: "lookup", Arguments: `{}`},
		}, json.RawMessage([]byte{byte('1' + index)}))
		if !ok {
			t.Fatalf("tool %s was not delegated", id)
		}
	}
	stream := &codexStream{state: state, ctx: context.Background()}
	for index, id := range []string{"call_a", "call_b"} {
		chunk, err := stream.Recv()
		if err != nil || chunk.Choices[0].Delta.ToolCalls[0].ID != id ||
			chunk.Choices[0].Delta.ToolCalls[0].Index != index {
			t.Fatalf("chunk %d = %#v, err = %v", index, chunk, err)
		}
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("pause error = %v", err)
	}

	pending, results, err := state.resumeWithToolResults([]llmprovider.Message{
		{Role: llmprovider.RoleTool, ToolCallID: "call_b", Content: "B"},
		{Role: llmprovider.RoleTool, ToolCallID: "call_a", Content: "A"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || len(results) != 2 || results[0].Content != "A" || results[1].Content != "B" {
		t.Fatalf("pending = %#v, results = %#v", pending, results)
	}
}
