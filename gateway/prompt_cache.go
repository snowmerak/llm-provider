package gateway

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	llmprovider "github.com/snowmerak/llm-provider"
)

const gatewayCacheConversationPrefix = "cache_"

// preparePromptCache gives stateless HTTP providers a stable cache-affinity
// value. Codex owns ConversationID as an App Server thread ID, while native
// Anthropic caching requires explicit cache_control breakpoints and is left to
// the caller.
func preparePromptCache(route *route, request llmprovider.ChatRequest) (llmprovider.ChatRequest, string, error) {
	mechanism := promptCacheMechanism(route)
	if mechanism == "" {
		// A model group may move a conversation from a stateless cached route to
		// Codex. A Gateway cache-affinity ID is not a resumable App Server thread.
		if isCodexRoute(route) && strings.HasPrefix(request.ConversationID, gatewayCacheConversationPrefix) {
			request.ConversationID = ""
			return request, "", nil
		}
		return request, request.ConversationID, nil
	}
	if route.cacheAffinityConfigured || requestCacheAffinityConfigured(mechanism, request) {
		return request, request.ConversationID, nil
	}

	conversationID := request.ConversationID
	if conversationID == "" {
		var err error
		conversationID, err = newCacheConversationID()
		if err != nil {
			return llmprovider.ChatRequest{}, "", err
		}
	}

	// Gateway-generated IDs are cache affinity identifiers, not upstream
	// stateful-conversation handles. Do not leak them as conversation_id to
	// OpenAI-compatible backends that may reject or interpret that extension.
	if strings.HasPrefix(request.ConversationID, gatewayCacheConversationPrefix) {
		request.ConversationID = ""
	}

	switch mechanism {
	case "openai":
		request.Extra = cloneAnyMap(request.Extra)
		if _, configured := request.Extra["prompt_cache_key"]; !configured {
			request.Extra["prompt_cache_key"] = conversationID
		}
	case "openrouter":
		request.Extra = cloneAnyMap(request.Extra)
		if _, configured := request.Extra["session_id"]; !configured {
			request.Extra["session_id"] = conversationID
		}
	case "grok":
		request.Headers = request.Headers.Clone()
		if request.Headers == nil {
			request.Headers = make(http.Header)
		}
		if request.Headers.Get("X-Grok-Conv-Id") == "" {
			request.Headers.Set("X-Grok-Conv-Id", conversationID)
		}
	}
	return request, conversationID, nil
}

func providerCacheAffinityConfigured(config ProviderConfig, providerKind string) bool {
	mechanism := promptCacheMechanism(&route{providerType: config.Type, providerKind: providerKind})
	switch mechanism {
	case "openai":
		_, configured := config.Body["prompt_cache_key"]
		return configured
	case "openrouter":
		if _, configured := config.Body["session_id"]; configured {
			return true
		}
		return headerValue(config.Headers, "X-Session-Id") != ""
	case "grok":
		return headerValue(config.Headers, "X-Grok-Conv-Id") != ""
	default:
		return false
	}
}

func requestCacheAffinityConfigured(mechanism string, request llmprovider.ChatRequest) bool {
	switch mechanism {
	case "openai":
		_, configured := request.Extra["prompt_cache_key"]
		return configured
	case "openrouter":
		if _, configured := request.Extra["session_id"]; configured {
			return true
		}
		return request.Headers.Get("X-Session-Id") != ""
	case "grok":
		return request.Headers.Get("X-Grok-Conv-Id") != ""
	default:
		return false
	}
}

func headerValue(headers map[string]string, name string) string {
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			return value
		}
	}
	return ""
}

func isCodexRoute(route *route) bool {
	return routeKind(route) == "codex"
}

func promptCacheMechanism(route *route) string {
	switch routeKind(route) {
	case "grok":
		return "grok"
	case "openrouter":
		return "openrouter"
	case "openai":
		return "openai"
	default:
		return ""
	}
}

func routeKind(route *route) string {
	if route == nil {
		return ""
	}
	if route.providerKind != "" {
		return route.providerKind
	}
	if route.modelCapabilityProfile == "xai" || route.providerType == "grok" || route.providerType == "xai" {
		return "grok"
	}
	switch route.providerType {
	case "openrouter":
		return "openrouter"
	case "openai-compatible":
		return "openai"
	case "anthropic", "claude":
		return "anthropic"
	case "codex", "codex-app-server":
		return "codex"
	default:
		return ""
	}
}

func newCacheConversationID() (string, error) {
	var entropy [18]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("gateway: generate prompt-cache conversation ID: %w", err)
	}
	return gatewayCacheConversationPrefix + base64.RawURLEncoding.EncodeToString(entropy[:]), nil
}

func cloneAnyMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source)+1)
	for key, value := range source {
		result[key] = value
	}
	return result
}
