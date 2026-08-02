---
name: use-llm-provider-reasoning
description: Configure, review, troubleshoot, or extend reasoning controls in the llm-provider repository. Use for /v1/models reasoning capabilities, reasoning effort discovery, effort/toggle/token-budget request shapes, Codex or Claude effort mapping, OpenRouter reasoning objects, Grok capability metadata, Hermes reasoning toggles, and model_metadata overrides.
---

# Use LLM Provider Reasoning

Treat repository behavior and `README.md` as the common API contract. Use
provider documentation only to establish provider-specific capability values
or request mappings.

## Follow the workflow

1. Resolve the prefixed model through `GET /v1/models/{id}`.
2. Read `capabilities.reasoning`; do not infer controls from the model name
   unless the repository has a documented provider capability profile.
3. Select the request shape from `control` and honor `mandatory`.
4. Send only a listed effort when `supported_efforts` is non-empty.
5. Do not treat mechanical forwarding of an unknown field as provider support;
   use only the control advertised for the selected model.
6. Keep an effort stable across a cache-sensitive conversation unless the user
   explicitly wants to trade cache reuse for a different effort.
7. Verify the provider wire shape with the narrow provider or Gateway test.

## Map capability to request

| `control` | Chat Completions HTTP | Go `ChatRequest` | Responses HTTP |
|---|---|---|---|
| `effort` | `"reasoning_effort":"high"` | `ReasoningEffort: "high"` | `"reasoning":{"effort":"high"}` |
| `toggle` | `"reasoning":{"enabled":true}` | `Extra["reasoning"]` with `enabled` | Same nested object |
| `token_budget` | `"reasoning":{"max_tokens":4096}` | `Extra["reasoning"]` with `max_tokens` | Same nested object |
| `fixed` | Omit reasoning controls | Omit reasoning controls | Omit reasoning controls |

Do not send `enabled: false` when `mandatory` is true. Treat an effort control
with no enumerated `supported_efforts` as provider-defined rather than as
unsupported. `supports_max_tokens` may expose a token budget in addition to
the primary control.

## Understand provider mapping

| Route | Effort wire mapping | Capability source |
|---|---|---|
| Codex App Server | `turn/start.effort` | `model/list` |
| Anthropic | `output_config.effort` | Models API `capabilities.effort` |
| OpenAI/xAI Chat | `reasoning_effort` | Upstream metadata, config, or xAI profile |
| OpenRouter Chat | `reasoning.effort` | Models API `reasoning` object |
| OpenRouter Hermes | `reasoning.enabled` | Models API reasoning toggle metadata |

Native OpenAI-compatible `/v1/responses` routes preserve the complete
`reasoning` object. The adapted Codex and Anthropic Responses path consumes
only `reasoning.effort` because those routes advertise effort control.

For direct OpenRouter Go clients, construct the provider with
`openai.WithReasoningEffortObject()`. The Gateway does this automatically for
`type: "openrouter"`.

## Extend model capabilities

Use the common structure in `types.go`:

```json
{
  "capabilities": {
    "reasoning": {
      "supported": true,
      "control": "effort",
      "supported_efforts": ["low", "medium", "high"],
      "default_effort": "high",
      "mandatory": true
    }
  }
}
```

When upstream discovery is unavailable, add a narrowly scoped provider profile
or a `model_metadata` override. Prefer upstream metadata, then built-in
documented provider metadata, then explicit configuration. Never guess unknown
model capabilities from a family substring.

Inspect these files before editing:

- `types.go` for the common model schema and OpenRouter normalization.
- `gateway/gateway.go` for discovery enrichment and provider profiles.
- `gateway/config.go` for `model_metadata` validation.
- `providers/codex/provider.go` and `providers/anthropic/provider.go` for native
  discovery and effort mapping.
- `providers/openai/provider.go` for OpenAI-compatible and OpenRouter request
  encoding.

## Verify changes

Run the narrow affected tests first, then the full suite:

```powershell
go test ./providers/codex ./providers/anthropic ./providers/openai ./gateway
go test ./...
git diff --check
```

Keep paid or authenticated provider integration tests opt-in. If reasoning
changes affect prompt caching, also use `$use-llm-provider-caching` and run only
the matching external regression with user authorization.
