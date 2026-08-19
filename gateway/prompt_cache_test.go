package gateway

import (
	"net/http"
	"strings"
	"testing"

	llmprovider "github.com/snowmerak/llm-provider"
)

func TestPreparePromptCacheMapsConversationAffinity(t *testing.T) {
	tests := []struct {
		name       string
		route      route
		extraField string
		header     string
	}{
		{name: "OpenAI compatible", route: route{providerType: "openai-compatible"}, extraField: "prompt_cache_key"},
		{name: "OpenRouter", route: route{providerType: "openrouter"}, extraField: "session_id"},
		{name: "Grok", route: route{providerType: "grok"}, header: "X-Grok-Conv-Id"},
		{name: "xAI compatible", route: route{providerType: "openai-compatible", modelCapabilityProfile: "xai"}, header: "X-Grok-Conv-Id"},
		{name: "explicit Grok kind", route: route{providerType: "openai-compatible", providerKind: "grok"}, header: "X-Grok-Conv-Id"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prepared, conversationID, err := preparePromptCache(&test.route, llmprovider.ChatRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(conversationID, gatewayCacheConversationPrefix) {
				t.Fatalf("conversation ID = %q", conversationID)
			}
			if test.extraField != "" && prepared.Extra[test.extraField] != conversationID {
				t.Fatalf("extra = %#v", prepared.Extra)
			}
			if test.header != "" && prepared.Headers.Get(test.header) != conversationID {
				t.Fatalf("headers = %#v", prepared.Headers)
			}

			prepared, secondID, err := preparePromptCache(&test.route, llmprovider.ChatRequest{ConversationID: conversationID})
			if err != nil {
				t.Fatal(err)
			}
			if secondID != conversationID || prepared.ConversationID != "" {
				t.Fatalf("second request ID = %q, upstream conversation ID = %q", secondID, prepared.ConversationID)
			}
			if test.extraField != "" && prepared.Extra[test.extraField] != conversationID {
				t.Fatalf("second extra = %#v", prepared.Extra)
			}
			if test.header != "" && prepared.Headers.Get(test.header) != conversationID {
				t.Fatalf("second headers = %#v", prepared.Headers)
			}
		})
	}
}

func TestPreparePromptCachePreservesExplicitSettings(t *testing.T) {
	extra := map[string]any{"prompt_cache_key": "explicit"}
	request := llmprovider.ChatRequest{ConversationID: "provider-thread", Extra: extra}
	prepared, conversationID, err := preparePromptCache(&route{providerType: "openai-compatible"}, request)
	if err != nil {
		t.Fatal(err)
	}
	if conversationID != "provider-thread" || prepared.ConversationID != "provider-thread" ||
		prepared.Extra["prompt_cache_key"] != "explicit" {
		t.Fatalf("prepared = %#v, conversation ID = %q", prepared, conversationID)
	}
	headers := http.Header{"X-Grok-Conv-Id": []string{"explicit-grok"}}
	prepared, _, err = preparePromptCache(&route{providerType: "grok"}, llmprovider.ChatRequest{
		ConversationID: "provider-thread", Headers: headers,
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Headers.Get("X-Grok-Conv-Id") != "explicit-grok" {
		t.Fatalf("headers = %#v", prepared.Headers)
	}
}

func TestPreparePromptCacheDoesNotOverrideExplicitAffinity(t *testing.T) {
	tests := []struct {
		name    string
		route   route
		request llmprovider.ChatRequest
	}{
		{name: "request OpenAI key", route: route{providerType: "openai-compatible"}, request: llmprovider.ChatRequest{Extra: map[string]any{"prompt_cache_key": "explicit"}}},
		{name: "request OpenRouter session", route: route{providerType: "openrouter"}, request: llmprovider.ChatRequest{Extra: map[string]any{"session_id": "explicit"}}},
		{name: "request Grok header", route: route{providerType: "grok"}, request: llmprovider.ChatRequest{Headers: http.Header{"X-Grok-Conv-Id": []string{"explicit"}}}},
		{name: "provider default", route: route{providerType: "openai-compatible", cacheAffinityConfigured: true}, request: llmprovider.ChatRequest{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prepared, conversationID, err := preparePromptCache(&test.route, test.request)
			if err != nil {
				t.Fatal(err)
			}
			if conversationID != "" || prepared.ConversationID != "" {
				t.Fatalf("prepared = %#v, conversation ID = %q", prepared, conversationID)
			}
		})
	}
}

func TestPreparePromptCacheLeavesClaudeAndCodexAlone(t *testing.T) {
	for _, providerType := range []string{"anthropic", "claude", "codex", "codex-app-server"} {
		request := llmprovider.ChatRequest{ConversationID: "existing"}
		prepared, conversationID, err := preparePromptCache(&route{providerType: providerType}, request)
		if err != nil {
			t.Fatal(err)
		}
		if conversationID != "existing" || prepared.ConversationID != "existing" ||
			prepared.Extra != nil || prepared.Headers != nil {
			t.Fatalf("%s prepared = %#v, conversation ID = %q", providerType, prepared, conversationID)
		}
	}

	generated, err := newCacheConversationID()
	if err != nil {
		t.Fatal(err)
	}
	for _, providerType := range []string{"codex", "codex-app-server"} {
		prepared, conversationID, err := preparePromptCache(&route{providerType: providerType}, llmprovider.ChatRequest{ConversationID: generated})
		if err != nil {
			t.Fatal(err)
		}
		if conversationID != "" || prepared.ConversationID != "" {
			t.Fatalf("%s treated cache affinity as a Codex thread: %#v", providerType, prepared)
		}
	}
}
