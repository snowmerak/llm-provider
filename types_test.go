package llmprovider

import (
	"encoding/json"
	"testing"
)

func TestModelNormalizesContextLengthAliases(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int64
	}{
		{name: "canonical", body: `{"id":"a","context_length":1000}`, want: 1000},
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
