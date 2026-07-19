<h1 align="center">Jarvis</h1>

<p align="center">
  A fast, open-source AI chatbot for Discord with provider-neutral model hosting and web search.
</p>

<p align="center">
  <img alt="Publish passing" src="https://img.shields.io/badge/publish-passing-brightgreen">
  <a href="https://hub.docker.com/r/justinswe/jarvis"><img alt="Docker pulls" src="https://img.shields.io/docker/pulls/justinswe/jarvis"></a>
  <a href="https://hub.docker.com/r/justinswe/jarvis/tags"><img alt="Docker image version" src="https://img.shields.io/docker/v/justinswe/jarvis?sort=semver"></a>
  <a href="https://github.com/justinswe/jarvis/blob/main/LICENSE"><img alt="MIT license" src="https://img.shields.io/github/license/justinswe/jarvis"></a>
</p>

<p align="center">
  <a href="#quick-start">Quick start</a> ·
  <a href="#features">Features</a> ·
  <a href="#architecture">Architecture</a> ·
  <a href="https://hub.docker.com/r/justinswe/jarvis">Docker Hub</a>
</p>

Jarvis brings sourced current answers, conversation recall, and server-specific configuration directly to Discord. Run it as a single container or deploy its gateway and worker services independently.

## Features

| Capability | What it does |
| --- | --- |
| **Provider-agnostic web search** | Uses Serper, Firecrawl, or Tavily and supplies only normalized results to the presentation model. |
| **Conversation recall** | Includes recent Discord context by default. Optional DynamoDB storage adds persistent history and model-directed search across the current channel or thread. |
| **Fast by design** | Go services, compact raw-protobuf transport, bounded context windows, and a direct request path keep the runtime small and responsive. |
| **Provider-neutral models** | Hosts generation on Google AI, Vertex AI, OpenRouter, or NVIDIA hosted NIM with named profiles, confirmed capabilities, and retryable failover. |
| **Server customization** | Authorized administrators can manage prompts, response settings, search, history, retention, and delegated access from Discord. |
| **Accuracy and resilience** | Tracks source availability, permits one bounded recovery call for Search, exposes health checks, and qualifies source-less current answers. |

## Quick start

The simplest deployment is the combined Docker image. It starts both the Discord Gateway ingestor and the model worker in one container.

You will need a Discord bot token. The Vertex example below also needs:

- A Google Cloud project with Vertex AI enabled
- Google application default credentials (ADC)

Pull the published image from [Docker Hub](https://hub.docker.com/r/justinswe/jarvis):

```sh
docker pull justinswe/jarvis:latest
```

Create a local `jarvis.env` file:

```dotenv
DISCORD_BOT_TOKEN=YOUR_DISCORD_BOT_TOKEN
PROJECT_ID=YOUR_GCP_PROJECT_ID
MODEL_PROFILE=primary=vertex:YOUR_TOOL_CAPABLE_VERTEX_MODEL_ID
PRIMARY_MODEL_PROFILE=primary
```

Keep this file out of source control. For local Docker, create ADC with `gcloud auth application-default login`, then run:

```sh
docker run --rm \
  --name jarvis \
  -p 8080:8080 \
  --env-file ./jarvis.env \
  -v "$HOME/.config/gcloud/application_default_credentials.json:/credentials.json:ro" \
  -e GOOGLE_APPLICATION_CREDENTIALS=/credentials.json \
  justinswe/jarvis:latest
```

Jarvis has no implicit model. The selected primary must advertise both tools and tool choice; it performs provider-neutral function orchestration and then a tools-disabled presentation pass. Web search is deployment-wide and remains unavailable until `WEB_SEARCH_PROVIDERS` and the selected provider keys are configured. On Google Cloud, prefer the service's attached identity instead of mounting a credential file.

The container exposes health and readiness checks at `http://localhost:8080/healthz` and `http://localhost:8080/readyz`. The published image currently targets `linux/amd64`.

The combined image runs a small PID 1 supervisor. It starts the worker on loopback port 8081, waits for readiness, and then starts the Discord Gateway ingestor on port 8080. If either process exits, the supervisor stops the other so the container platform can replace the instance cleanly.

### Configure primary and fallback models

Every model is declared as `name=provider:model-id`. For an OpenRouter primary and Vertex presentation fallback, set:

```sh
worker \
  --model-profile=primary=openrouter:YOUR_OPENROUTER_MODEL_ID \
  --model-profile=fallback=vertex:YOUR_VERTEX_MODEL_ID \
  --primary-model-profile=primary \
  --fallback-model-profile=fallback \
  --openrouter-api-key=YOUR_OPENROUTER_API_KEY \
  --project-id=YOUR_GCP_PROJECT_ID
```

For a Vertex primary and OpenRouter presentation fallback, reverse the profile providers:

```sh
worker \
  --model-profile=primary=vertex:YOUR_VERTEX_MODEL_ID \
  --model-profile=fallback=openrouter:YOUR_OPENROUTER_MODEL_ID \
  --primary-model-profile=primary \
  --fallback-model-profile=fallback \
  --project-id=YOUR_GCP_PROJECT_ID \
  --openrouter-api-key=YOUR_OPENROUTER_API_KEY
```

The primary model must advertise both `tools` and `tool_choice` during startup capability probing. The fallback receives normalized application evidence but never function schemas, so it may be a presentation-only model; it does not take over an interrupted tool phase. To run without fallback, omit `--fallback-model-profile` and its profile. Choose a tool-capable model for both profiles if either one may later be selected as primary.

#### Use Google AI Studio models

[Google AI Studio](https://aistudio.google.com/apikey) supplies the `GOOGLE_AI_API_KEY` credential. Jarvis passes that key explicitly to the Gemini Developer API through the official [Google Gen AI SDK for Go](https://pkg.go.dev/google.golang.org/genai). Google AI profiles do not use Vertex `PROJECT_ID`, `LOCATION`, ADC, or service-account credentials.

Google AI profiles require an explicit Gemini 3-or-newer model ID. Startup validates the canonical model metadata and rejects Gemini 2.5, tuned or opaque names, malformed names, and moving aliases that cannot prove their version, such as `gemini-flash-latest`. Prefer a stable, version-bearing ID such as `gemini-3.1-flash-lite` over a moving `latest` alias; see Google's [model naming guidance](https://ai.google.dev/gemini-api/docs/models).

The following pairings are supported:

```sh
# Google AI primary, OpenRouter fallback
worker \
  --model-profile=primary=google-ai:gemini-3.1-flash-lite \
  --model-profile=fallback=openrouter:YOUR_OPENROUTER_MODEL_ID \
  --primary-model-profile=primary \
  --fallback-model-profile=fallback \
  --google-ai-api-key=YOUR_GOOGLE_AI_API_KEY \
  --openrouter-api-key=YOUR_OPENROUTER_API_KEY

# Google AI primary, Vertex fallback
worker \
  --model-profile=primary=google-ai:gemini-3.1-flash-lite \
  --model-profile=fallback=vertex:YOUR_VERTEX_MODEL_ID \
  --primary-model-profile=primary \
  --fallback-model-profile=fallback \
  --google-ai-api-key=YOUR_GOOGLE_AI_API_KEY \
  --project-id=YOUR_GCP_PROJECT_ID

# Vertex primary, Google AI fallback
worker \
  --model-profile=primary=vertex:YOUR_VERTEX_MODEL_ID \
  --model-profile=fallback=google-ai:gemini-3.1-flash-lite \
  --primary-model-profile=primary \
  --fallback-model-profile=fallback \
  --project-id=YOUR_GCP_PROJECT_ID \
  --google-ai-api-key=YOUR_GOOGLE_AI_API_KEY

# OpenRouter primary, Google AI fallback
worker \
  --model-profile=primary=openrouter:YOUR_OPENROUTER_MODEL_ID \
  --model-profile=fallback=google-ai:gemini-3.1-flash-lite \
  --primary-model-profile=primary \
  --fallback-model-profile=fallback \
  --openrouter-api-key=YOUR_OPENROUTER_API_KEY \
  --google-ai-api-key=YOUR_GOOGLE_AI_API_KEY
```

Create a dedicated key in AI Studio, restrict it to the Gemini API and, where applicable, the worker's egress IPs, and store it only in the worker's server-side secret manager. Rotate keys by deploying a replacement before revoking the old key; revoke a suspected exposed key immediately after the replacement is active. Never commit or expose the key to a Discord or browser client. Google's [API-key guidance](https://ai.google.dev/gemini-api/docs/api-key) covers creation, restriction, storage, rotation, revocation, and leak response.

`--model-profile` is comma-capable and repeatable. The equivalent environment configuration for the first example is:

```dotenv
MODEL_PROFILE=primary=openrouter:YOUR_OPENROUTER_MODEL_ID,fallback=vertex:YOUR_VERTEX_MODEL_ID
PRIMARY_MODEL_PROFILE=primary
FALLBACK_MODEL_PROFILE=fallback
OPENROUTER_API_KEY=YOUR_OPENROUTER_API_KEY
PROJECT_ID=YOUR_GCP_PROJECT_ID
```

The resolved primary handles runtime, configuration, reaction, channel-history, and optional Search-decision tools through the same provider-neutral host contract whether it uses Google AI, Vertex, or OpenRouter. Completed results are converted to portable application records before presentation or cross-provider fallback. Search provider configuration remains independent from model routing.

### Run from source

Run both services locally with Bazel:

```sh
export PROJECT_ID=YOUR_GCP_PROJECT_ID
export DISCORD_BOT_TOKEN=YOUR_DISCORD_BOT_TOKEN
export MODEL_PROFILE=primary=vertex:YOUR_TOOL_CAPABLE_VERTEX_MODEL_ID
export PRIMARY_MODEL_PROFILE=primary
bazel run //:jarvis
```

The multirun target starts the ingestor health server on port 8080 and the worker HTTP server on port 8081. Run `bazel run //:ingestor -- --help` or `bazel run //:worker -- --help` to inspect and start either service independently.

## Search and conversation recall

Jarvis searches through Serper, Firecrawl, or Tavily when web search is enabled for a server. Serper must be first whenever it is configured. Explicit research, current facts, and actionable price, availability, safety, or recommendation questions require one logical Search invocation. Every required or model-selected Search calls the first provider and may make one recovery call after any error or a response without usable sources. Recovery uses the second configured provider, or retries the only provider once, and never starts after the request is canceled or its deadline expires.

The provider receives only the sanitized current request plus bounded previous-request context for an elliptical follow-up. It never receives guild prompts, Discord channel results, runtime evidence, credentials, conversation history, or tool output. The presentation model receives versioned JSON containing only status and validated result records. Every displayed link is labeled `Sources consulted`. When no usable URL is available, confident current claims are rejected and repaired into explicitly qualified prose. See [Web search](docs/web-search.md) for request limits, source semantics, recovery rules, diagnostics, migration, and key rotation.

Recent conversation context is loaded from Discord by default. When DynamoDB is enabled, Jarvis records incoming messages, uses the stored conversation as model context, and can search the current channel or thread by text, author, or time range. Search results include direct links back to Discord. See [DynamoDB storage](docs/dynamodb.md) for retention, access, and search behavior.

Within one worker instance, overlapping requests in the same Discord thread use latest-message-wins processing. A newer request cancels the active request, replaces any older pending request, and waits for cancellation to finish before generating one response from the latest available thread history. Existing context-window and rune-budget settings still apply. Separate threads remain concurrent; deployments with multiple worker replicas need external request affinity or distributed coordination to provide the same guarantee across replicas.

## Configuration

Explicit model profiles may host generation on Google AI, Vertex AI, OpenRouter, or NVIDIA hosted NIM. Web-search providers are configured independently from model profiles. DynamoDB can optionally provide persistent Discord history and per-server profile selection:

```text
Google Cloud                                      AWS
Vertex AI <-> Jarvis worker -- identity token --> STS --> DynamoDB
```

For a keyless deployment, Jarvis retrieves a short-lived identity token from its attached Google Cloud service account and exchanges it through AWS STS `AssumeRoleWithWebIdentity`. It does not require a mounted Google service-account key or static AWS access keys. Local, ECS, and EC2 deployments can instead use the AWS SDK's normal credential chain.

The primary configuration variables are:

| Variable | Required | Purpose |
| --- | --- | --- |
| `PROJECT_ID` | With Vertex | Google Cloud project used by Vertex AI. |
| `DISCORD_BOT_TOKEN` | Yes | Token used for Discord Gateway and REST access. |
| `GOOGLE_APPLICATION_CREDENTIALS` | Environment-dependent | Path to application credentials for local or container execution. |
| `GOOGLE_AI_API_KEY` | With Google AI | Restricted Google AI Studio credential used only for `google-ai` profiles. Jarvis does not use `GEMINI_API_KEY` or SDK key auto-discovery. |
| `LOCATION` | No | Vertex AI location; defaults to `global`. |
| `DEFAULT_PROMPT` | No | Root-controlled assistant customization that may define its name and personality; empty by default. |
| `MODEL_PROFILE` | Yes | Comma-separated `name=provider:model-id` declarations. The command flag is also repeatable. Providers are `google-ai`, `vertex`, `openrouter`, and `nvidia-nim`. |
| `PRIMARY_MODEL_PROFILE` | Yes | Default primary profile name. It must confirm tools and tool choice. |
| `FALLBACK_MODEL_PROFILE` | No | Default fallback profile name; empty disables fallback. |
| `WEB_SEARCH_PROVIDERS` | No | Ordered comma-separated list of zero to two distinct providers: `serper`, `firecrawl`, or `tavily`. Serper must be first. Empty disables Search globally. |
| `SERPER_API_KEY` | When Serper is selected | Serper credential; ignored when Serper is unselected. |
| `FIRECRAWL_API_KEY` | When Firecrawl is selected | Firecrawl credential; ignored when Firecrawl is unselected. |
| `TAVILY_API_KEY` | When Tavily is selected | Tavily credential; ignored when Tavily is unselected. |
| `OPENROUTER_API_KEY` | With OpenRouter | API key used only for OpenRouter generation. |
| `NVIDIA_API_KEY` | With NVIDIA NIM | Bearer key for hosted `integrate.api.nvidia.com`; self-hosted NIM endpoints are not configured here. |
| `DYNAMODB_ENABLED` | No | Enables persistent history and server configuration; defaults to `false`. |
| `DYNAMODB_TABLE` | With DynamoDB | Existing DynamoDB table name; defaults to `jarvis`. |
| `AWS_REGION` | With DynamoDB | DynamoDB region resolved by the AWS SDK. |
| `AWS_ROLE_ARN` | For federation | AWS role assumed with a Google identity token. |
| `AWS_WEB_IDENTITY_AUDIENCE` | For federation | Audience placed in the Google identity token. |

Every non-repeatable command flag is also available as an uppercase environment variable with hyphens replaced by underscores. For example, `--message-retention-days` maps to `MESSAGE_RETENTION_DAYS`. Use `--help` to see all options.

### Breaking model and Search migration

Remove `MODEL_PROVIDER`, `OPENROUTER_MODEL`, `TOOL_MODEL_PROFILE`, and `TEXT_ONLY_MODEL_PROFILE`. Define every model through `MODEL_PROFILE`, select one tool-capable `PRIMARY_MODEL_PROFILE`, and optionally select a presentation `FALLBACK_MODEL_PROFILE`. There is no built-in Gemini model ID or dedicated Vertex tool route.

Remove `WEB_SEARCH_MODEL_PROFILE`; Search no longer uses any model profile. Set `WEB_SEARCH_PROVIDERS` and keys only for selected providers. If a Serper key was exposed during earlier setup or review, revoke it and deploy a newly rotated value before enabling Search. Never reuse the exposed key in a smoke test or deployment.

The model profile syntax is:

```text
name=provider:model-id
```

For example, an OpenRouter primary with a Vertex fallback and Serper-first web search is:

```sh
worker \
  --model-profile=chat=openrouter:YOUR_OPENROUTER_MODEL_ID \
  --model-profile=fallback=vertex:YOUR_VERTEX_MODEL_ID \
  --primary-model-profile=chat \
  --fallback-model-profile=fallback \
  --openrouter-api-key=YOUR_OPENROUTER_API_KEY \
  --project-id=YOUR_GCP_PROJECT_ID \
  --web-search-providers=serper,tavily \
  --serper-api-key=YOUR_NEWLY_ROTATED_SERPER_API_KEY \
  --tavily-api-key=YOUR_TAVILY_API_KEY
```

Credentials remain provider-wide: configure `GOOGLE_AI_API_KEY`, `OPENROUTER_API_KEY`, `NVIDIA_API_KEY`, and/or Vertex `PROJECT_ID` plus ADC for every provider referenced by the profiles. A Google AI key is ignored when no `google-ai` profile is selected. Removing all `google-ai` profiles and the key restores the previous deployment configuration without a persisted-settings migration.

## Architecture

Jarvis separates the stateful Discord connection from independently deployable message processing:

```text
Discord Gateway -> ingestor -> HTTP/1.1 raw protobuf -> worker
                                                       |-> Discord REST
                                                       |-> Google AI Gemini Developer API (generation and tools)
                                                       |-> Vertex AI (generation and tools)
                                                       |-> OpenRouter (generation and tools)
                                                       |-> NVIDIA hosted NIM (generation and confirmed tools)
                                                       |-> Serper / Firecrawl / Tavily (Search)
                                                       `-> DynamoDB (optional)
```

The ingestor owns the Discord Gateway connection and normalizes each message into the versioned protobuf contract at `api/jarvis/discord/v1/worker.proto`. It synchronously sends those bytes to the configured `WORKER_URL`; it does not manage the worker process.

The worker exposes `POST /v1/messages:process` with the `application/x-protobuf` content type and returns `204 No Content` after processing completes. The contract is compatible with an unwrapped Pub/Sub push payload, allowing the transport to change without changing the worker handler.

Build and load the combined or individual service images locally:

```sh
bazel run //:image_load
bazel run //:ingestor_image_load
bazel run //:worker_image_load
```

Only the combined `justinswe/jarvis` image is currently published. Publication of `justinswe/jarvis-ingestor` and `justinswe/jarvis-worker` is intentionally deferred.

> [!WARNING]
> The worker endpoint has no application-level authentication. The combined image binds it to loopback. A standalone worker must remain behind an internal VPC or another trusted network boundary and must not be exposed directly to the public internet.

Direct HTTP publishing makes one synchronous attempt because processing has Discord side effects. Durable retries and deduplication are deferred until Pub/Sub is introduced.

## Development

Format protobuf definitions and run the complete test suite with Bazel:

```sh
bazel run //:buf_format
bazel test //...
```

Additional documentation:

- [Web search providers, sources, and recovery](docs/web-search.md)
- [DynamoDB storage, history, and multi-cloud authentication](docs/dynamodb.md)

## License

Jarvis is available under the [MIT License](LICENSE).
