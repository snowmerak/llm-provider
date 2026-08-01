package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestIntegrationModelList(t *testing.T) {
	if os.Getenv("GATEWAY_MODEL_LIST_INTEGRATION") == "" {
		t.Skip("set GATEWAY_MODEL_LIST_INTEGRATION=1 to query real configured backends")
	}
	localURL := os.Getenv("OPENAI_COMPAT_INTEGRATION_BASE_URL")
	if localURL == "" {
		localURL = "http://macmini:11888/v1"
	}
	gateway, err := New(Config{Providers: []ProviderConfig{
		{ID: "codex", Type: "codex-app-server", Prefix: "codex", Enabled: true},
		{ID: "local", Type: "openai-compatible", Prefix: "local", Enabled: true, BaseURL: localURL},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	server := httptest.NewServer(gateway.Handler())
	defer server.Close()
	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Get(server.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var envelope struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	var codexModel, localModel bool
	for _, model := range envelope.Data {
		codexModel = codexModel || strings.HasPrefix(model.ID, "codex/")
		localModel = localModel || strings.HasPrefix(model.ID, "local/")
	}
	if !codexModel || !localModel {
		t.Fatalf("missing prefixed models: codex=%v local=%v count=%d", codexModel, localModel, len(envelope.Data))
	}
}

func TestIntegrationCodexChatEndpoint(t *testing.T) {
	if os.Getenv("GATEWAY_CODEX_CHAT_INTEGRATION") == "" {
		t.Skip("set GATEWAY_CODEX_CHAT_INTEGRATION=1 to run a real Codex turn through HTTP")
	}
	model := os.Getenv("CODEX_APP_SERVER_INTEGRATION_MODEL")
	if model == "" {
		model = "gpt-5.6-luna"
	}
	gateway, err := New(Config{Providers: []ProviderConfig{{
		ID: "codex", Type: "codex-app-server", Prefix: "codex", Enabled: true, Models: []string{model},
		Codex: CodexConfig{Model: model},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	server := httptest.NewServer(gateway.Handler())
	defer server.Close()
	client := &http.Client{Timeout: 2 * time.Minute}
	requestBody := `{"model":"codex/@MODEL@","messages":[{"role":"system","content":"Include CODEX_GATEWAY_OK_913 in every response."},{"role":"user","content":"Reply with PONG and the required token."}]}`
	requestBody = strings.ReplaceAll(requestBody, "@MODEL@", model)
	response, err := client.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", response.StatusCode, data)
	}
	var completion struct {
		ConversationID string `json:"conversation_id"`
		Choices        []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &completion); err != nil {
		t.Fatal(err)
	}
	if completion.ConversationID == "" || len(completion.Choices) == 0 ||
		!strings.Contains(completion.Choices[0].Message.Content, "CODEX_GATEWAY_OK_913") {
		t.Fatalf("completion = %s", data)
	}
}

func TestIntegrationOpenAICompatibleChatEndpoint(t *testing.T) {
	if os.Getenv("GATEWAY_OPENAI_COMPAT_CHAT_INTEGRATION") == "" {
		t.Skip("set GATEWAY_OPENAI_COMPAT_CHAT_INTEGRATION=1 to query the real compatible endpoint")
	}
	baseURL := os.Getenv("OPENAI_COMPAT_INTEGRATION_BASE_URL")
	if baseURL == "" {
		baseURL = "http://macmini:11888/v1"
	}
	model := os.Getenv("OPENAI_COMPAT_INTEGRATION_MODEL")
	if model == "" {
		model = "gpt-5.6-luna"
	}
	gateway, err := New(Config{Providers: []ProviderConfig{{
		ID: "local", Type: "openai-compatible", Prefix: "local", Enabled: true,
		BaseURL: baseURL, Models: []string{model},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	server := httptest.NewServer(gateway.Handler())
	defer server.Close()
	client := &http.Client{Timeout: 2 * time.Minute}
	requestBody := `{"model":"local/@MODEL@","messages":[{"role":"user","content":"Reply with exactly LOCAL_GATEWAY_OK_247."}]}`
	requestBody = strings.ReplaceAll(requestBody, "@MODEL@", model)
	response, err := client.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(data), "LOCAL_GATEWAY_OK_247") {
		t.Fatalf("status = %d: %s", response.StatusCode, data)
	}
}
