package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCallbackBindsStateAndRegistration(t *testing.T) {
	for _, test := range []struct {
		name, saved, query string
		status             int
	}{
		{"new registration", "", "state=expected&code=code&client_id=oaiapp_new", 200},
		{"returning registration", "oaiapp_saved", "state=expected&code=code", 200},
		{"wrong state", "", "state=wrong&code=code&client_id=oaiapp_new", 400},
		{"missing issued ID", "", "state=expected&code=code", 400},
		{"bootstrap ID", "", "state=expected&code=code&client_id=dynamic_agent_client", 400},
		{"changed registration", "oaiapp_saved", "state=expected&code=code&client_id=oaiapp_other", 400},
		{"declined consent", "", "state=expected&error=access_denied", 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := make(chan callback, 1)
			handler := callbackHandler("expected", test.saved, result)
			response := httptest.NewRecorder()
			handler(response, httptest.NewRequest(http.MethodGet, "/auth/callback?"+test.query, nil))
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d", response.Code, test.status)
			}
			if test.status != 200 {
				if len(result) != 0 {
					t.Fatal("invalid callback was accepted")
				}
				return
			}
			accepted := <-result
			if test.saved != "" && accepted.ClientID != test.saved {
				t.Fatal("returning client ID was not retained")
			}
			if strings.Contains(response.Body.String(), "code=") {
				t.Fatal("callback leaked the authorization code")
			}
			replay := httptest.NewRecorder()
			handler(replay, httptest.NewRequest(http.MethodGet, "/auth/callback?"+test.query, nil))
			if replay.Code != http.StatusConflict {
				t.Fatal("callback replay was accepted")
			}
		})
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (fn transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestInferenceRequiresCompletedTerminalEvent(t *testing.T) {
	for _, test := range []struct {
		name, stream string
		success      bool
	}{
		{"completed", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\"}]}}\n\n", true},
		{"interrupted after delta", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n", false},
		{"failure after delta", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\ndata: {\"type\":\"response.failed\"}\n\n", false},
		{"incomplete", "data: {\"type\":\"response.incomplete\"}\n\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != resource+"/responses" || r.Header.Get("Authorization") != "Bearer test-oauth-token" {
					t.Fatal("unexpected request target or credential")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body["store"] != false || body["stream"] != true {
					t.Fatal("ChatGPT plan request contract was not enforced")
				}
				if _, ok := body["input"].([]any); !ok {
					t.Fatal("input is not an array")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(test.stream))}, nil
			})}
			output, err := infer(context.Background(), client, "test-oauth-token", "test-model", []any{map[string]any{"role": "user", "content": "hello"}})
			if (err == nil) != test.success {
				t.Fatalf("success = %v, want %v; error = %v", err == nil, test.success, err)
			}
			if test.success && len(output) != 1 {
				t.Fatal("completed native output was not preserved for history replay")
			}
		})
	}
}

func TestRefreshUsesIssuedClientAndRetainsGrant(t *testing.T) {
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		form, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatal(err)
		}
		if form.Get("client_id") != "oaiapp_issued" || form.Get("resource") != resource || form.Has("scope") {
			t.Fatal("refresh changed the client or granted scope")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"new-access","refresh_token":"new-refresh","token_type":"Bearer"}`))}, nil
	})}
	value, err := tokenRequest(context.Background(), client, issuer+"/api/accounts/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {"oaiapp_issued"}, "refresh_token": {"old-refresh"}, "resource": {resource}})
	if err != nil || value.Refresh != "new-refresh" {
		t.Fatal("rotated refresh token was not retained")
	}
}
