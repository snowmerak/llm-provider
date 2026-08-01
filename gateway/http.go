package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	llmprovider "github.com/snowmerak/llm-provider"
)

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", g.handleModels)
	mux.HandleFunc("GET /v1/models/{id...}", g.handleModel)
	mux.HandleFunc("POST /v1/chat/completions", g.handleChatCompletions)
	return mux
}

func (g *Gateway) handleModels(writer http.ResponseWriter, request *http.Request) {
	models, err := g.Models(request.Context())
	if err != nil {
		writeError(writer, http.StatusBadGateway, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"object": "list", "data": models})
}

func (g *Gateway) handleModel(writer http.ResponseWriter, request *http.Request) {
	model, err := g.Model(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, http.StatusNotFound, err)
		return
	}
	writeJSON(writer, http.StatusOK, model)
}

func (g *Gateway) handleChatCompletions(writer http.ResponseWriter, request *http.Request) {
	chatRequest, stream, err := decodeChatRequest(request.Body)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	route, backendModel, err := g.resolve(chatRequest.Model)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err)
		return
	}
	externalModel := chatRequest.Model
	chatRequest.Model = backendModel
	chatRequest.Headers = selectedHeaders(request.Header, route.forwardHeaders)
	if stream {
		g.streamChat(writer, request.Context(), route, externalModel, chatRequest)
		return
	}
	response, err := route.provider.Chat(request.Context(), chatRequest)
	if err != nil {
		writeError(writer, http.StatusBadGateway, err)
		return
	}
	response.Model = externalModel
	copySelectedHeaders(writer.Header(), response.Headers, route.forwardResponseHeaders)
	writeJSON(writer, http.StatusOK, response)
}

func (g *Gateway) streamChat(writer http.ResponseWriter, ctx context.Context, route *route, externalModel string, request llmprovider.ChatRequest) {
	stream, err := route.provider.ChatStream(ctx, request)
	if err != nil {
		writeError(writer, http.StatusBadGateway, err)
		return
	}
	defer stream.Close()
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeError(writer, http.StatusInternalServerError, errors.New("streaming is unsupported by the HTTP server"))
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	if headerer, ok := stream.(llmprovider.ResponseHeaderer); ok {
		copySelectedHeaders(writer.Header(), headerer.ResponseHeaders(), route.forwardResponseHeaders)
	}
	writer.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(writer)
	for {
		chunk, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			_, _ = io.WriteString(writer, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
		if recvErr != nil {
			data, _ := json.Marshal(errorEnvelope(recvErr))
			_, _ = fmt.Fprintf(writer, "data: %s\n\n", data)
			flusher.Flush()
			return
		}
		chunk.Model = externalModel
		_, _ = io.WriteString(writer, "data: ")
		if err := encoder.Encode(chunk); err != nil {
			return
		}
		_, _ = io.WriteString(writer, "\n")
		flusher.Flush()
	}
}

func decodeChatRequest(reader io.Reader) (llmprovider.ChatRequest, bool, error) {
	data, err := io.ReadAll(io.LimitReader(reader, 16<<20))
	if err != nil {
		return llmprovider.ChatRequest{}, false, fmt.Errorf("read request: %w", err)
	}
	var wire struct {
		llmprovider.ChatRequest
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return llmprovider.ChatRequest{}, false, fmt.Errorf("decode request: %w", err)
	}
	if wire.Model == "" {
		return llmprovider.ChatRequest{}, false, errors.New("model is required")
	}
	var extra map[string]any
	if err := json.Unmarshal(data, &extra); err != nil {
		return llmprovider.ChatRequest{}, false, fmt.Errorf("decode request extensions: %w", err)
	}
	for _, key := range []string{
		"model", "messages", "temperature", "top_p", "max_tokens", "max_completion_tokens",
		"stop", "tools", "tool_choice", "parallel_tool_calls", "stream", "conversation_id",
	} {
		delete(extra, key)
	}
	wire.Extra = extra
	return wire.ChatRequest, wire.Stream, nil
}

func selectedHeaders(source http.Header, allowed map[string]struct{}) http.Header {
	result := make(http.Header)
	copySelectedHeaders(result, source, allowed)
	return result
}

func copySelectedHeaders(target, source http.Header, allowed map[string]struct{}) {
	for key, values := range source {
		canonical := http.CanonicalHeaderKey(key)
		if _, ok := allowed[canonical]; !ok {
			continue
		}
		target.Del(canonical)
		for _, value := range values {
			target.Add(canonical, value)
		}
	}
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, err error) {
	writeJSON(writer, status, errorEnvelope(err))
}

func errorEnvelope(err error) map[string]any {
	return map[string]any{"error": map[string]any{
		"message": err.Error(), "type": "invalid_request_error",
		"code": strings.ToLower(strings.ReplaceAll(http.StatusText(http.StatusBadRequest), " ", "_")),
	}}
}
