package llmprovider

import (
	"encoding/json"
	"testing"
)

func TestChatChunkPreservesChoicePhaseOnWire(t *testing.T) {
	chunk := ChatChunk{Choices: []Choice{{
		Index: 0, Phase: "commentary",
		Delta: &Message{Role: RoleAssistant, Content: "working"},
	}}}
	data, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ChatChunk
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Choices) != 1 || decoded.Choices[0].Phase != "commentary" ||
		decoded.Choices[0].Delta == nil || decoded.Choices[0].Delta.Content != "working" {
		t.Fatalf("chunk = %#v, wire = %s", decoded, data)
	}
}

func TestModelNormalizesContextLengthAliases(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int64
	}{
		{name: "canonical", body: `{"id":"a","context_length":1000}`, want: 1000},
		{name: "anthropic", body: `{"id":"a","max_input_tokens":1500}`, want: 1500},
		{name: "vllm", body: `{"id":"a","max_model_len":2000}`, want: 2000},
		{name: "context window", body: `{"id":"a","context_window":3000}`, want: 3000},
		{name: "token suffix", body: `{"id":"a","context_window_tokens":4000}`, want: 4000},
		{name: "string", body: `{"id":"a","max_context_length":"5000"}`, want: 5000},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var model Model
			if err := json.Unmarshal([]byte(test.body), &model); err != nil {
				t.Fatal(err)
			}
			if model.ContextLength != test.want {
				t.Fatalf("context length = %d, want %d", model.ContextLength, test.want)
			}
			data, err := json.Marshal(model)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			if err := json.Unmarshal(data, &wire); err != nil {
				t.Fatal(err)
			}
			if wire["context_length"] != float64(test.want) {
				t.Fatalf("wire model = %s", data)
			}
		})
	}
}

func TestModelNormalizesMaxOutputTokenAliases(t *testing.T) {
	var model Model
	if err := json.Unmarshal([]byte(`{"id":"a","max_tokens":128000}`), &model); err != nil {
		t.Fatal(err)
	}
	if model.MaxOutputTokens != 128000 {
		t.Fatalf("max output tokens = %d", model.MaxOutputTokens)
	}
	data, err := json.Marshal(model)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["max_output_tokens"] != float64(128000) {
		t.Fatalf("wire model = %s", data)
	}
}

func TestModelPreservesReasoningEffortMetadata(t *testing.T) {
	var model Model
	if err := json.Unmarshal([]byte(`{"id":"a","capabilities":{"reasoning":{"supported":true,"control":"effort","supported_efforts":["low","medium","high"],"default_effort":"medium"}}}`), &model); err != nil {
		t.Fatal(err)
	}
	reasoning := model.Capabilities.Reasoning
	if reasoning.DefaultEffort != "medium" || len(reasoning.SupportedEfforts) != 3 ||
		reasoning.SupportedEfforts[2] != "high" {
		t.Fatalf("model = %#v", model)
	}
	data, err := json.Marshal(model)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) || string(data) == "" {
		t.Fatalf("model JSON = %s", data)
	}
}

func TestModelNormalizesOpenRouterReasoningMetadata(t *testing.T) {
	var model Model
	if err := json.Unmarshal([]byte(`{"id":"a","reasoning":{"supported_efforts":["high","medium","low","minimal"],"default_effort":"medium","mandatory":true}}`), &model); err != nil {
		t.Fatal(err)
	}
	reasoning := model.Capabilities.Reasoning
	if reasoning.Control != ReasoningControlEffort || reasoning.DefaultEffort != "medium" ||
		len(reasoning.SupportedEfforts) != 4 || reasoning.SupportedEfforts[3] != "minimal" {
		t.Fatalf("model = %#v", model)
	}
}

func TestModelNormalizesOpenRouterReasoningToggle(t *testing.T) {
	var model Model
	if err := json.Unmarshal([]byte(`{"id":"hermes","reasoning":{"default_enabled":false,"mandatory":false}}`), &model); err != nil {
		t.Fatal(err)
	}
	reasoning := model.Capabilities.Reasoning
	if reasoning.Control != ReasoningControlToggle || reasoning.DefaultEnabled == nil || *reasoning.DefaultEnabled {
		t.Fatalf("model = %#v", model)
	}
}

func TestModelNormalizesOpenRouterReasoningTokenBudget(t *testing.T) {
	var model Model
	if err := json.Unmarshal([]byte(`{"id":"budget","reasoning":{"supports_max_tokens":true,"default_enabled":true}}`), &model); err != nil {
		t.Fatal(err)
	}
	reasoning := model.Capabilities.Reasoning
	if reasoning.Control != ReasoningControlTokenBudget || !reasoning.SupportsMaxTokens ||
		reasoning.DefaultEnabled == nil || !*reasoning.DefaultEnabled {
		t.Fatalf("model = %#v", model)
	}
}
