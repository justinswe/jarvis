# Search grounding

Jarvis makes Google Search available when the guild configuration enables web search. A deterministic current-request classifier requires grounding for explicit web requests and clearly volatile facts. It inspects only `CURRENT REQUEST`, not historical transcript text, and does not make a separate model call.

Explicit requests to search the current channel or retrieve earlier messages instead require the `search_current_channel` function and successful channel-history evidence. They do not trigger Google Search unless the request also explicitly asks for web or external research. If DynamoDB-backed channel search is disabled or fails before returning usable history, Jarvis returns a channel-history verification fallback rather than guessing. Requests that need both channel history and web research retain both requirements.

## Verification policy

Grounding enforcement starts when Search was attempted or the request policy requires grounding:

1. Jarvis accepts the response when it contains at least one valid HTTP or HTTPS web source.
2. If Search returned no usable source, Jarvis makes one verification retry with Google Search as the only tool, including when Gemini never attempted Search. Function tools are not exposed or executed again. The retry uses medium thinking and at least 2,048 output tokens. Temperature is omitted so Gemini uses its recommended provider default.
3. Jarvis accepts the retry only when it contains visible text and a valid web source. Otherwise, it discards the unverified answer and returns a short verification failure message.

Optional Search requests for which Gemini emits no search query retain the model response. When guild Search is disabled, a required-current request returns an explicit unverified fallback rather than presenting the generated claim as verified. Grounding, code-execution, and response-validation recovery share a single one-call accuracy-retry budget. Existing function-tool rounds are separate from that budget.

Up to three unique sources are appended to the Discord response in a compact footer:

```text
-# Sources: [1](https://example.com) · [2](https://example.org)
```

Discord cannot render the exact HTML/CSS Google Search suggestion supplied in `SearchEntryPoint.RenderedContent`. Jarvis logs when that content is present but intentionally uses the Discord-native numbered footer instead.

Successful non-web evidence is appended separately so it persists in Discord and DynamoDB history:

```text
-# Evidence used: runtime context · channel history · code execution
```

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

Function-tool rounds log requested, executed, succeeded, and failed counts. Stored channel searches additionally log duration, scanned and returned counts, filter-presence booleans, truncation, and incompleteness. DEBUG logs add exposed tool names, per-call timing and outcome, and grounding source domains. Logs intentionally exclude search query text, author criteria, full source URLs, function arguments, function outputs, and message content.
