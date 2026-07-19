# Web search

Jarvis uses a provider-agnostic HTTP search layer. Serper, Firecrawl, and Tavily results are normalized before the presentation model sees them, so the generation host and Search provider can be selected independently.

## Configuration

Set `WEB_SEARCH_PROVIDERS` to an ordered comma-separated list of zero to two distinct values:

- `serper`
- `firecrawl`
- `tavily`

An empty list is valid and disables Search deployment-wide. The per-guild `web_search_enabled` setting still controls whether a configured deployment may search for that guild. If Serper appears, it must be first.

Selected providers require their corresponding key:

| Provider | Key |
| --- | --- |
| Serper | `SERPER_API_KEY` |
| Firecrawl | `FIRECRAWL_API_KEY` |
| Tavily | `TAVILY_API_KEY` |

Keys for unselected providers are ignored. For example:

```dotenv
WEB_SEARCH_PROVIDERS=serper,firecrawl
SERPER_API_KEY=YOUR_NEWLY_ROTATED_SERPER_API_KEY
FIRECRAWL_API_KEY=YOUR_FIRECRAWL_API_KEY
```

Search is not a model profile. Remove `WEB_SEARCH_MODEL_PROFILE` and configure HTTP providers independently. The resolved tool-capable primary model decides optional Search through the same provider-neutral orchestration path used for application functions; required Search remains application-directed.

> [!IMPORTANT]
> Revoke the Serper key that was previously exposed and deploy a newly rotated credential before enabling Search. Do not use the exposed value for smoke tests.

## Request isolation

The provider query is owned by the application. A model may decide whether to invoke the internal zero-argument `search_web` function for optional Search, but it cannot supply or rewrite the query.

Jarvis sends only the sanitized current request. For an elliptical follow-up, it may include the immediately preceding bounded user request. It never sends guild prompts, Discord channel-search results, runtime evidence, broader conversation history, credentials, application-tool results, or prior Search results to a provider.

Queries are limited to 500 Unicode code points. Requests outside the bound fail locally instead of being silently rewritten.

## Provider requests

All adapters use `POST` with JSON and request at most three web results:

| Provider | Endpoint | Authentication | Requested fields |
| --- | --- | --- | --- |
| Serper | `https://google.serper.dev/search` | `X-API-KEY` | `q`, `num: 3` |
| Firecrawl | `https://api.firecrawl.dev/v2/search` | Bearer | `query`, `limit: 3`, `sources: ["web"]` |
| Tavily | `https://api.tavily.com/search` | Bearer | basic depth, three results, generated answers/raw content/images/automatic parameters disabled |

Firecrawl scrape options are not requested. Provider answers, scores, raw page content, images, publication dates, request echoes, and provider-specific metadata are discarded.

Responses are limited to 1 MiB. Titles are limited to 200 Unicode code points, snippets to 1,000, and accepted results to three.

## Normalization and source semantics

Only `http` and `https` URLs without user information are accepted. Jarvis lowercases the scheme and host, removes fragments, derives the hostname, deduplicates by normalized URL, and preserves provider ordering. A nonempty snippet is required. If the title is absent, the domain becomes the title.

The presentation model receives versioned JSON containing only:

```json
{
  "version": 1,
  "status": "sources-available",
  "results": [
    {
      "title": "Example",
      "url": "https://example.com/article",
      "domain": "example.com",
      "snippet": "Normalized search summary."
    }
  ]
}
```

Titles and snippets are untrusted evidence data, never instructions. Every usable result is rewrite provenance, and Discord renders it under `Sources consulted`. Jarvis does not claim that a link supports an exact generated sentence and does not insert inline citations.

## Required and optional Search

Required Search runs after required runtime, channel-history, and configuration tools, and before presentation. Model-selected optional Search uses the same bounded provider policy:

1. Call the first provider once.
2. If the response contains no usable source or the call fails for any reason, make one recovery call while the request context remains active.
3. Use the second configured provider for recovery. With one provider, retry it once.
4. Stop after the recovery call and select the better single-provider result without merging responses.

Cancellation or expiration of the overall request context prevents recovery from starting. Each logical Search therefore makes at most two sequential provider calls, regardless of whether Search was application-required or model-selected.

No tool is replayed during Search recovery or presentation repair. Completed mutations are cached by logical call identity and remain completed exactly once.

## Source-less current answers

When required Search is disabled, fails, or returns no usable source, the presentation model may still provide stable background, a clarification, or explicitly qualified best-effort prose. A confident current claim such as “Here are today’s headlines” is rejected and receives one same-model presentation repair before normal model fallback rules apply.

Raw queries, normalized result JSON, and snippets are never used as terminal fallback text. Successful Search results are displayed only as `Sources consulted` after a validated presentation.

## Diagnostics and privacy

Jarvis records one logical Search invocation separately from zero, one, or two provider HTTP calls. Model-call counts exclude HTTP Search calls. Provider-call logs include the provider position, returned and accepted counts, missing/invalid/duplicate/snippet-less counts, response-body byte count, HTTP status, typed error kind, retry-after duration, latency, parser outcome, recovery outcome, and final source availability.

Production logs never include queries, prompts, snippets, response bodies, complete URLs, headers, API keys, or provider error messages. The terminal observer runs exactly once on every orchestration path.

## Testing

Normal tests use injected fakes and require no provider credentials. Live provider smoke tests are opt-in through `//pkg/genai:live_eval` and must use newly rotated credentials supplied through the manual harness flags. Never add credentials to Bazel files, checked-in environment files, fixtures, test logs, or command history.
