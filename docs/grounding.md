# Search grounding

Jarvis makes Google Search available when the guild configuration enables web search. A deterministic current-request classifier requires a Search attempt for explicit web requests and clearly volatile facts. It inspects only `CURRENT REQUEST`, not quoted or historical transcript text, and does not make a separate model call. Niche or uncertain stable facts remain a model-level Search decision.

A broad first-turn recency request such as “What’s happening?” is allowed to produce one concise topic-or-region clarification without Search. If the user repeats a scope-free recency request, Jarvis keeps the Search requirement; the model is instructed to state that it is assuming general world news and proceed with Search.

Explicit requests to search the current channel or retrieve earlier messages instead require the `search_current_channel` function and successful channel-history evidence. They do not trigger Google Search unless the request also explicitly asks for web or external research. If DynamoDB-backed channel search is disabled or fails before returning usable history, Jarvis returns a channel-history verification fallback rather than guessing. Requests that need both channel history and web research retain both requirements.

## Verification policy

`AccuracyPolicy.GroundingRequired` means that Search must be attempted before a current claim may be presented. It does not mean that every source-less response is replaced by a refusal. Grounding enforcement starts when Search was attempted or the request policy requires it:

1. Jarvis accepts the response when it contains at least one valid HTTP or HTTPS web source.
2. If Search returned no usable source, Jarvis makes one verification retry with Google Search as the only tool, including when Gemini never attempted Search. Function tools are not exposed or executed again. The retry uses medium thinking and at least 2,048 output tokens. Temperature is omitted so Gemini uses its recommended provider default.
3. Jarvis accepts a retry with visible text and valid sources as grounded. If the retry has useful text but no valid source, Jarvis discards the initial unsupported answer, preserves the retry body without a leading caveat, leaves the response ungrounded, and records `web-unconfirmed` evidence status. The retry may retain useful current details but must not describe them as verified or confirmed.
4. If the Search-only retry fails or has no visible text, Jarvis returns an actionable request for a topic, region, date range, or link. If Search is disabled for a required-current request, Jarvis says so and offers stable background or help narrowing the question.

Optional Search requests for which Gemini emits no search query retain the model response. Grounding, code-execution, and response-validation recovery share a single one-call accuracy-retry budget. Existing function-tool rounds are separate from that budget, and function tools are never exposed or executed again during Search-only recovery. Runtime context, stored channel history, completed mutations, and code execution retain their strict evidence requirements.

Up to three unique sources are appended to the Discord response in a compact footer:

```text
-# Sources: [example.com](https://example.com) · [example.org](https://example.org)
```

Discord cannot render the exact HTML/CSS Google Search suggestion supplied in `SearchEntryPoint.RenderedContent`. Jarvis logs when that content is present but intentionally uses a Discord-native domain-labeled footer instead. Labels prefer publisher metadata from the grounding chunk, while Google-provided redirect URLs remain unchanged and are not resolved by Jarvis.

Successful non-web evidence is appended separately so it persists in Discord and DynamoDB history:

```text
-# Evidence used: runtime context · channel history · code execution
```

When current details could not be confirmed from usable web sources, Discord renders one fixed final footer:

```text
-# Evidence status: Current details could not be confirmed from usable web sources.
```

Metadata order is always `Sources`, `Evidence used`, then `Evidence status`. A grounded response with valid sources suppresses an unconfirmed status defensively. `Evidence status` qualifies the preceding claims as unverified; it does not count as evidence, make the response grounded, or establish recorded provenance. Only `Sources` and `Evidence used` record provenance for later conversation turns.

## Diagnostics

Every Gemini attempt has an `attempt` value such as `initial`, `tool_followup`, `empty_response_recovery`, or `grounding_recovery`. INFO logs include duration, token usage when available, finish details, Search availability, function-tool count and mode, and these grounding fields:

- `grounding_metadata_present`
- `search_query_count`
- `grounding_chunk_count`
- `web_chunk_count`
- `grounding_support_count`
- `supported_source_count`
- `valid_source_count`
- `invalid_source_count`
- `duplicate_source_count`
- `search_entry_point_present`
- `grounding_outcome`
- `search_trigger` (`none`, `explicit`, `volatile`, `implicit-volatile`, or `model-optional`)
- `search_result` (`not-used`, `grounded`, `no-sources-qualified`, `disabled`, `provider-failed`, or `empty`)
- `grounding_retry_result`
- `evidence_status` (empty or `web-unconfirmed`)
- `terminal_fallback_reason`

Function-tool rounds log requested, executed, succeeded, and failed counts. Stored channel searches additionally log duration, scanned and returned counts, filter-presence booleans, truncation, and incompleteness. DEBUG logs add exposed tool names, per-call timing and outcome, and grounding source domains. Logs intentionally exclude search query text, author criteria, full source URLs, function arguments, function outputs, and message content.

## Manual evaluation

`//pkg/genai:live_eval` is a `manual` Bazel target and is excluded from normal wildcard test runs. Its checked-in JSONL corpus contains 100 behavioral cases, including a 30-case development subset. The target uses the production `Handler.Generate` path and supports the baseline, concise, and concise-plus-few-shot Search prompts while holding the model and generation settings fixed.

Run one development pass with:

```sh
bazel test //pkg/genai:live_eval \
  --test_env=JARVIS_EVAL_PROJECT_ID=PROJECT_ID \
  --test_env=JARVIS_EVAL_PROMPT_VARIANT=concise \
  --test_env=JARVIS_EVAL_SUBSET=development
```

Set `JARVIS_EVAL_SUBSET=full` and `JARVIS_EVAL_RUNS=3` for repeated full-corpus runs. Results are written as JSONL test outputs with response text, Search requirement and attempt state, query and model-call counts, retry use, source and supported-source counts, evidence status, grounding outcome, latency, and terminal fallback reason. Status-bearing responses are classified as `qualified-answer` from the structured field rather than a response-text prefix. These evaluation artifacts may contain response text; production logs continue to exclude messages, queries, responses, and full source URLs.

## Pre-change baseline

The privacy-safe Cloud Run worker-pool logs available from July 3 through July 17, 2026 contained 124 completed requests. Attempts from canceled latest-message-wins generations were excluded from per-request query and retry calculations by joining on completed request IDs.

| Metric | Baseline |
|---|---:|
| Grounded final responses | 11 / 124 (8.9%) |
| Terminal fallbacks | 34 / 124 (27.4%) |
| Accuracy retries | 45 / 124 (36.3%) |
| Grounding-retry recovery | 10 / 40 (25.0%) |
| Average Search queries per completed request | 1.32 |
| Average model calls per completed request | 1.42 |
| p95 completed-request latency | 13.564 s |
| Provider-attempt failures in the available window | 6 |

The baseline aggregation used only existing structured counts, booleans, attempt names, durations, and request IDs. It did not retrieve message text, Search query text, response text, or source URLs.
