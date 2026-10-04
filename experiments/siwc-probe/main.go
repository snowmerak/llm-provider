// siwc-probe checks the documented ChatGPT plan OAuth flow without changing
// Codex credentials or Q settings. Tokens live only in this process.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/snowmerak/llm-provider/providers/openai"
)

const issuer = "https://auth.openai.com"
const resource = "https://api.openai.com/v1"

type registration struct {
	HostID   string `json:"ext_agent_host_id"`
	ClientID string `json:"client_id,omitempty"`
	Subject  string `json:"subject,omitempty"`
}

type tokens struct {
	Access          string `json:"access_token"`
	Refresh         string `json:"refresh_token"`
	ID              string `json:"id_token"`
	Type            string `json:"token_type"`
	Scope           string `json:"scope"`
	EarliestRefresh int64  `json:"earliest_refresh_at"`
}

type callback struct{ Code, ClientID, Error string }

func main() {
	model := flag.String("model", "", "account catalog model slug; default prefers Luna")
	noOpen := flag.Bool("no-open", false, "print the URL without opening the system browser")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 12*time.Minute)
	defer cancel()
	if err := run(ctx, *model, *noOpen); err != nil {
		fmt.Fprintln(os.Stderr, "SIWC probe:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, model string, noOpen bool) error {
	client := &http.Client{Timeout: 90 * time.Second}
	ctx = oidc.ClientContext(ctx, client)
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return errors.New("OpenID discovery failed")
	}
	var endpoints struct {
		Authorization string `json:"authorization_endpoint"`
		Token         string `json:"token_endpoint"`
		Revocation    string `json:"revocation_endpoint"`
	}
	if err := provider.Claims(&endpoints); err != nil {
		return err
	}
	for _, endpoint := range []string{endpoints.Authorization, endpoints.Token, endpoints.Revocation} {
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Scheme != "https" || parsed.Host != "auth.openai.com" {
			return errors.New("discovery returned an unexpected endpoint")
		}
	}
	fmt.Println("PASS: OpenID discovery")
	reg, path, err := loadRegistration()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	state, nonce, verifier := randomValue(), randomValue(), randomValue()
	redirect := "http://" + listener.Addr().String() + "/auth/callback"
	result := make(chan callback, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", callbackHandler(state, reg.ClientID, result))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()
	query := url.Values{
		"client_id": {reg.ClientID}, "ext_agent_host_id": {reg.HostID},
		"response_type": {"code"}, "redirect_uri": {redirect},
		"scope":    {"openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"},
		"resource": {resource}, "state": {state}, "nonce": {nonce},
		"code_challenge_method": {"S256"},
	}
	if reg.ClientID == "" {
		query.Set("client_id", "dynamic_agent_client")
		query.Set("agent_name_hint", "llm-provider")
	}
	digest := sha256.Sum256([]byte(verifier))
	query.Set("code_challenge", base64.RawURLEncoding.EncodeToString(digest[:]))
	authorizationURL := endpoints.Authorization + "?" + query.Encode()
	fmt.Println("Continue with ChatGPT:", authorizationURL)
	fmt.Println("Waiting for your browser sign-in and consent (up to 12 minutes).")
	if !noOpen {
		if err := openBrowser(authorizationURL); err != nil {
			fmt.Println("Open the URL above in your system browser.")
		}
	}
	var returned callback
	select {
	case returned = <-result:
	case <-ctx.Done():
		return errors.New("browser sign-in did not complete before cancellation or timeout")
	}
	if returned.Error != "" {
		return fmt.Errorf("authorization failed (%s)", returned.Error)
	}
	reg.ClientID = returned.ClientID
	// Keep the issued registration even if exchange or ID validation fails.
	if err := saveRegistration(path, reg); err != nil {
		return err
	}
	value, err := tokenRequest(ctx, client, endpoints.Token, url.Values{
		"grant_type": {"authorization_code"}, "client_id": {reg.ClientID},
		"code": {returned.Code}, "code_verifier": {verifier},
		"redirect_uri": {redirect}, "resource": {resource},
	})
	if err != nil {
		return err
	}
	// Revoke this probe's renewable session even if identity or inference fails.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if value.Refresh == "" {
			return
		}
		form := url.Values{"token": {value.Refresh}, "token_type_hint": {"refresh_token"}, "client_id": {reg.ClientID}}
		request, err := http.NewRequestWithContext(cleanup, http.MethodPost, endpoints.Revocation, strings.NewReader(form.Encode()))
		if err != nil {
			return
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, err := client.Do(request)
		if err == nil {
			defer response.Body.Close()
			if response.StatusCode == http.StatusOK {
				fmt.Println("PASS: probe session revoked; no tokens saved")
				return
			}
		}
		fmt.Fprintln(os.Stderr, "Remote session revocation was not confirmed. Disconnect llm-provider in ChatGPT Settings if needed.")
	}()
	idToken, err := provider.Verifier(&oidc.Config{ClientID: reg.ClientID}).Verify(ctx, value.ID)
	if err != nil {
		return errors.New("ID token signature, issuer, audience, or expiry validation failed")
	}
	var identity struct {
		Nonce string `json:"nonce"`
	}
	if err := idToken.Claims(&identity); err != nil {
		return errors.New("invalid ID token claims")
	}
	if subtle.ConstantTimeCompare([]byte(identity.Nonce), []byte(nonce)) != 1 || idToken.Subject == "" {
		return errors.New("ID token nonce or subject validation failed")
	}
	if reg.Subject != "" && reg.Subject != idToken.Subject {
		return errors.New("returning account identity changed")
	}
	if !slices.Contains(strings.Fields(value.Scope), "chatgpt.tokens.use.direct") {
		return errors.New("ChatGPT plan usage scope was not granted")
	}
	reg.Subject = idToken.Subject
	if err := saveRegistration(path, reg); err != nil {
		return err
	}
	fmt.Println("PASS: verified OAuth identity and ChatGPT plan usage scope")
	model, err = selectModel(ctx, client, value.Access, model)
	if err != nil {
		return err
	}
	fmt.Println("PASS: account-specific model catalog; selected", model)
	// Exercise llm-provider's existing HTTP/SSE transport with the OAuth token.
	input := []any{map[string]any{"role": "user", "content": "Reply exactly SIWC_OK."}}
	output, err := infer(ctx, client, value.Access, model, input)
	if err != nil {
		return err
	}
	if responseText(output) != "SIWC_OK" {
		return errors.New("first inference did not return the requested smoke-test marker")
	}
	fmt.Println("PASS: llm-provider Responses stream reached response.completed")
	for _, item := range output {
		input = append(input, json.RawMessage(item))
	}
	input = append(input, map[string]any{"role": "user", "content": "Reply exactly SIWC_HISTORY_OK."})
	secondOutput, err := infer(ctx, client, value.Access, model, input)
	if err != nil {
		return err
	}
	if responseText(secondOutput) != "SIWC_HISTORY_OK" {
		return errors.New("second inference did not return the requested smoke-test marker")
	}
	fmt.Println("PASS: second turn with locally replayed Responses history")
	if value.Refresh != "" && value.EarliestRefresh <= time.Now().Unix() {
		renewed, err := tokenRequest(ctx, client, endpoints.Token, url.Values{"grant_type": {"refresh_token"}, "client_id": {reg.ClientID}, "refresh_token": {value.Refresh}, "resource": {resource}})
		if err != nil {
			return err
		}
		value = renewed
		if _, err := selectModel(ctx, client, value.Access, model); err != nil {
			return err
		}
		fmt.Println("PASS: token rotation and renewed access")
	} else {
		fmt.Println("SKIP: refresh is not yet permitted or no refresh token was issued")
	}
	return nil
}

func callbackHandler(state, savedClient string, result chan<- callback) http.HandlerFunc {
	var mu sync.Mutex
	consumed := false
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		query := r.URL.Query()
		if r.Method != http.MethodGet || subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(state)) != 1 {
			http.Error(w, "Invalid sign-in callback.", http.StatusBadRequest)
			return
		}
		clientID := query.Get("client_id")
		if savedClient != "" {
			if clientID != "" && clientID != savedClient {
				http.Error(w, "Client registration changed.", http.StatusBadRequest)
				return
			}
			clientID = savedClient
		}
		if query.Get("error") == "" && (query.Get("code") == "" || clientID == "" || clientID == "dynamic_agent_client") {
			http.Error(w, "Incomplete sign-in callback.", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if consumed {
			http.Error(w, "Sign-in callback already consumed.", http.StatusConflict)
			return
		}
		consumed = true
		result <- callback{Code: query.Get("code"), ClientID: clientID, Error: query.Get("error")}
		_, _ = io.WriteString(w, "Sign-in callback received. You can close this tab and return to Codex.")
	}
}

func tokenRequest(ctx context.Context, client *http.Client, endpoint string, form url.Values) (tokens, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokens{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		return tokens{}, errors.New("OAuth token endpoint request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return tokens{}, fmt.Errorf("OAuth token endpoint HTTP %d", response.StatusCode)
	}
	var value tokens
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&value); err != nil {
		return tokens{}, errors.New("invalid OAuth token response")
	}
	if value.Access == "" || !strings.EqualFold(value.Type, "Bearer") {
		return tokens{}, errors.New("OAuth token response has no Bearer credential")
	}
	return value, nil
}

func selectModel(ctx context.Context, client *http.Client, token, selected string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, resource+"/models", nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		return "", errors.New("model catalog request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("model catalog HTTP %d", response.StatusCode)
	}
	var catalog struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&catalog); err != nil {
		return "", errors.New("invalid model catalog")
	}
	var choices []string
	for _, model := range catalog.Models {
		if model.Visibility == "list" && model.Slug != "" {
			choices = append(choices, model.Slug)
		}
	}
	if selected != "" {
		if !slices.Contains(choices, selected) {
			return "", errors.New("selected model is absent from the account catalog")
		}
		return selected, nil
	}
	for _, choice := range choices {
		if strings.Contains(choice, "luna") {
			return choice, nil
		}
	}
	if len(choices) == 0 {
		return "", errors.New("account catalog has no visible models")
	}
	return choices[0], nil
}

func infer(ctx context.Context, client *http.Client, token, model string, input []any) ([]json.RawMessage, error) {
	provider := openai.New(openai.WithBaseURL(resource), openai.WithAPIKey(token), openai.WithHTTPClient(client))
	defer provider.Close()
	body, err := json.Marshal(map[string]any{"model": model, "input": input, "store": false, "stream": true})
	if err != nil {
		return nil, err
	}
	stream, err := provider.CreateResponseStream(ctx, body, nil)
	if err != nil {
		return nil, fmt.Errorf("Responses request failed: %w", err)
	}
	defer stream.Close()
	for {
		event, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, errors.New("stream ended without response.completed")
			}
			return nil, err
		}
		var value struct {
			Type     string `json:"type"`
			Response struct {
				Status string            `json:"status"`
				Output []json.RawMessage `json:"output"`
			} `json:"response"`
		}
		if err := json.Unmarshal(event.Data, &value); err != nil {
			return nil, errors.New("invalid Responses event")
		}
		switch value.Type {
		case "response.completed":
			if value.Response.Status != "completed" {
				return nil, errors.New("terminal response did not complete")
			}
			return value.Response.Output, nil
		case "response.failed", "response.incomplete", "error":
			return nil, fmt.Errorf("Responses terminal event: %s", value.Type)
		}
	}
}

func randomValue() string {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(value[:])
}

func responseText(output []json.RawMessage) string {
	var text strings.Builder
	for _, raw := range output {
		var item struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(raw, &item) != nil || item.Type != "message" {
			continue
		}
		for _, part := range item.Content {
			if part.Type == "output_text" {
				text.WriteString(part.Text)
			}
		}
	}
	return strings.TrimSpace(text.String())
}

func loadRegistration() (registration, string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return registration{}, "", err
	}
	path := filepath.Join(configDir, "llm-provider", "siwc-probe", "registration.json")
	data, err := os.ReadFile(path)
	if err == nil {
		var reg registration
		if err := json.Unmarshal(data, &reg); err != nil {
			return reg, path, err
		}
		if !strings.HasPrefix(reg.HostID, "urn:uuid:") {
			return reg, path, errors.New("invalid persisted probe host ID")
		}
		return reg, path, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return registration{}, path, err
	}
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return registration{}, path, err
	}
	entropy[6] = (entropy[6] & 0x0f) | 0x40
	entropy[8] = (entropy[8] & 0x3f) | 0x80
	reg := registration{HostID: fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", entropy[:4], entropy[4:6], entropy[6:8], entropy[8:10], entropy[10:])}
	return reg, path, saveRegistration(path, reg)
}

func saveRegistration(path string, reg registration) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	// Only host/client identifiers and the verified subject are persisted.
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func openBrowser(target string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", target).Start()
	case "darwin":
		return exec.Command("open", target).Start()
	default:
		return exec.Command("xdg-open", target).Start()
	}
}
