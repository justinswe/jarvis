# Search grounding

Jarvis makes Google Search available when the guild configuration enables web search. The system prompt directs the model to search for explicit web requests, current public information, and factual answers that are niche, uncertain, or unsupported by the supplied Discord context. Jarvis does not make a separate classification request before generation.

## Verification policy

Grounding enforcement starts when Gemini returns at least one web search query in grounding metadata:

1. Jarvis accepts the response when it contains at least one valid HTTP or HTTPS web source.
2. If Search returned no usable source, Jarvis makes one verification retry with Google Search as the only tool. Function tools are not exposed or executed again. The retry uses temperature `1.0`, medium thinking, and at least 2,048 output tokens.
3. Jarvis accepts the retry only when it contains visible text and a valid web source. Otherwise, it discards the unverified answer and returns a short verification failure message.

Requests for which Gemini emits no search query retain the model response without forcing a second request. Blocked, empty-response, and tool-failure application fallbacks do not trigger grounding recovery.

Up to three unique sources are appended to the Discord response in a compact footer:

```text
-# Sources: [1](https://example.com) · [2](https://example.org)
```

Discord cannot render the exact HTML/CSS Google Search suggestion supplied in `SearchEntryPoint.RenderedContent`. Jarvis logs when that content is present but intentionally uses the Discord-native numbered footer instead.

## Diagnostics

Every Gemini attempt has an `attempt` value such as `initial`, `tool_followup`, `empty_response_recovery`, or `grounding_recovery`. INFO logs include duration, token usage when available, finish details, Search availability, function-tool count and mode, and these grounding fields:

- `grounding_metadata_present`
- `search_query_count`
- `grounding_chunk_count`
- `web_chunk_count`
- `grounding_support_count`
- `valid_source_count`
- `invalid_source_count`
- `duplicate_source_count`
- `search_entry_point_present`
- `grounding_outcome`

Function-tool rounds log requested, executed, succeeded, and failed counts. DEBUG logs add exposed tool names, per-call timing and outcome, and grounding source domains. Logs intentionally exclude search query text, full source URLs, function arguments, and function outputs.
