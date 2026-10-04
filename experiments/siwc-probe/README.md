# Sign in with ChatGPT feasibility probe

This isolated Go module tests the official open-source ChatGPT plan OAuth flow.
It does not implement a production provider or modify Codex credentials or Q
configuration. It uses a maintained OIDC verifier for signature, issuer,
audience, and expiry checks, and validates the original nonce and callback state.

Run from this directory:

```powershell
go test ./...
go run .
```

Complete **Continue with ChatGPT** in the system browser. The tool waits for a
loopback callback for up to 12 minutes. Use `-no-open` to open the printed link
yourself, or `-model <catalog-slug>` to choose an account-visible model. The
default prefers a visible Luna model, then the first visible catalog entry.

The probe checks OAuth consent, the account-specific model catalog, a short
Responses stream through completion using llm-provider's OpenAI transport, a
second turn with locally replayed history, and token refresh when permitted.
These two inference requests consume a small amount of the authorized ChatGPT
plan usage. It revokes its renewable session when the probe finishes. Only a
stable host ID and verified client/account registration are saved under the OS
user configuration directory in `llm-provider/siwc-probe/registration.json`;
access, refresh, and ID tokens remain in memory. The client registration can
remain visible in ChatGPT Settings after its renewable session is revoked.

This probe covers one registration and text inference. Production support still
needs a protected account store, atomic credential rotation, refresh ownership
across processes, disconnect/account-switching behavior, a Responses-to-Chat
adapter, supported tool encoding, and Gateway/Studio integration.

Official references:

- https://developers.openai.com/siwc/token-sharing-open-source/sign-in
- https://developers.openai.com/siwc/token-sharing-open-source/models-and-inference
- https://developers.openai.com/siwc/token-sharing-open-source/profiles-and-sessions
- https://developers.openai.com/siwc/token-sharing-open-source/preview-limitations

## Integration findings (2026-10-04)

The current llm-provider OpenAI transport can already send an OAuth bearer token
and consume native Responses SSE. A dedicated `chatgpt` provider should own
account-specific model discovery, renewable credentials, and the plan-specific
request contract instead of treating a rotating OAuth token as a permanent API
key. The existing Gateway `ResponsesProvider` interface provides both streaming
and non-streaming methods. For a non-streaming caller, the new provider must
consume an upstream stream through `response.completed` and return the terminal
response document; the plan route itself requires streaming.

Q already routes selected models through `client/responses.go`, replays native
Responses output, and uses the llm-provider Gateway as a supervised child
process. Important integration points are:

- `studio/frontend/src/settings/ProvidersPanel.svelte`: add ChatGPT as a provider
  and show Continue with ChatGPT, connection state, selected account, reconnect,
  disconnect, and the ChatGPT usage settings link.
- `studio/settings_providers.go`, `studio/settings_types.go`, and
  `studio/handler.go`: add account-reference configuration and login/status/
  disconnect endpoints. Tokens stay on the server, outside settings snapshots
  and settings export files. The OAuth callback should use its own loopback
  listener so its scheme, host, and path remain stable across Studio launches.
- `providerhost/model_api_modes.go`: recognize the new provider as a native
  Responses route. The Q dependency on llm-provider must be updated after the
  provider is available; a temporary module replacement can verify local work.
- `client/responses.go`: currently forwards system-role messages, flat function
  tools, optional sampling fields, and output token limits. The new provider
  must normalize permitted instruction messages and tool encodings, and handle
  unsupported fields explicitly. This belongs at the provider boundary and does
  not require another Q agent loop.
- Q supervises Gateway in a separate process. Give token refresh one owner, or
  serialize rotation across processes; do not let Studio and Gateway refresh
  the same rotating credential independently.

Existing focused tests passed for llm-provider `providers/openai` and `gateway`,
and Q `client`, `providerhost`, and `studio`. The isolated probe's callback and
terminal-stream tests and `go vet` also passed. These tests establish existing
local contracts and the probe's handling of failure cases.

### Live result

The user completed browser sign-in on 2026-10-04. The probe exited successfully
with these observed results:

- OpenID discovery succeeded.
- The ID token and ChatGPT plan usage scope were verified.
- The account-specific model catalog was retrieved; `gpt-5.6-luna` was selected.
- A Responses stream using llm-provider's existing OpenAI transport reached
  `response.completed`.
- A second turn with locally replayed native Responses history also completed.
- Token refresh was skipped because the probe's refresh eligibility condition
  was not satisfied. Live token rotation remains unverified.
- The probe's renewable session was revoked successfully; no tokens were saved.

The running probe was compiled before the exact output-marker assertions were
added, so this run verifies completed inference and history replay, not exact
marker text. Tool-call round trips, token rotation, and production Gateway/
Studio integration still require separate live verification.
