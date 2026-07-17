<h1 align="center">Jarvis</h1>

<p align="center">
  A fast, open-source, search-grounded AI chatbot for Discord, powered by Google Vertex AI.
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

Jarvis brings grounded answers, conversation recall, and server-specific configuration directly to Discord. Run it as a single container or deploy its gateway and worker services independently.

## Features

| Capability | What it does |
| --- | --- |
| **Grounded web search** | Uses Google Search when current or explicitly researched information is needed, verifies the result, and renders supporting sources in Discord. |
| **Conversation recall** | Includes recent Discord context by default. Optional DynamoDB storage adds persistent history and model-directed search across the current channel or thread. |
| **Fast by design** | Go services, compact raw-protobuf transport, bounded context windows, and a direct request path keep the runtime small and responsive. |
| **Multi-cloud ready** | Combines Vertex AI on Google Cloud with optional DynamoDB on AWS, including keyless Google-to-AWS workload identity federation. |
| **Server customization** | Authorized administrators can manage prompts, response settings, search, history, retention, and delegated access from Discord. |
| **Accuracy and resilience** | Tracks evidence, retries unverifiable grounded answers once, exposes health checks, and degrades gracefully after request-time storage failures. |

## Quick start

You will need:

- A Discord bot token
- A Google Cloud project with Vertex AI enabled
- Google Cloud application default credentials

Pull the published image from [Docker Hub](https://hub.docker.com/r/justinswe/jarvis):

```sh
docker pull justinswe/jarvis:latest
```

Run the combined ingestor and worker image:

```sh
docker run --rm \
  --name jarvis \
  -p 8080:8080 \
  -v "$HOME/.config/gcloud/application_default_credentials.json:/credentials.json:ro" \
  -e GOOGLE_APPLICATION_CREDENTIALS=/credentials.json \
  -e PROJECT_ID=YOUR_GCP_PROJECT_ID \
  -e DISCORD_BOT_TOKEN=YOUR_DISCORD_BOT_TOKEN \
  justinswe/jarvis:latest
```

The container exposes health and readiness checks at `http://localhost:8080/healthz` and `http://localhost:8080/readyz`. The published image currently targets `linux/amd64`.

The combined image runs a small PID 1 supervisor. It starts the worker on loopback port 8081, waits for readiness, and then starts the Discord Gateway ingestor on port 8080. If either process exits, the supervisor stops the other so the container platform can replace the instance cleanly.

### Run from source

Run both services locally with Bazel:

```sh
export PROJECT_ID=YOUR_GCP_PROJECT_ID
export DISCORD_BOT_TOKEN=YOUR_DISCORD_BOT_TOKEN
bazel run //:jarvis
```

The multirun target starts the ingestor health server on port 8080 and the worker HTTP server on port 8081. Run `bazel run //:ingestor -- --help` or `bazel run //:worker -- --help` to inspect and start either service independently.

## Search and conversation recall

Jarvis makes Google Search available to the model when web search is enabled for a server. Requests for explicit research or clearly current information require grounded results; if no usable source is returned, Jarvis retries once before responding with an explicit verification caveat. Source links are preserved in Discord responses. See [Google Search grounding](docs/grounding.md) for the full accuracy policy and diagnostics.

Recent conversation context is loaded from Discord by default. When DynamoDB is enabled, Jarvis records incoming messages, uses the stored conversation as model context, and can search the current channel or thread by text, author, or time range. Search results include direct links back to Discord. See [DynamoDB storage](docs/dynamodb.md) for retention, access, and search behavior.

Within one worker instance, overlapping requests in the same Discord thread use latest-message-wins processing. A newer request cancels the active request, replaces any older pending request, and waits for cancellation to finish before generating one response from the latest available thread history. Existing context-window and rune-budget settings still apply. Separate threads remain concurrent; deployments with multiple worker replicas need external request affinity or distributed coordination to provide the same guarantee across replicas.

## Multi-cloud configuration

Vertex AI provides generation and Google Search grounding. DynamoDB can optionally provide persistent Discord history and per-server configuration:

```text
Google Cloud                                      AWS
Vertex AI <-> Jarvis worker -- identity token --> STS --> DynamoDB
```

For a keyless deployment, Jarvis retrieves a short-lived identity token from its attached Google Cloud service account and exchanges it through AWS STS `AssumeRoleWithWebIdentity`. It does not require a mounted Google service-account key or static AWS access keys. Local, ECS, and EC2 deployments can instead use the AWS SDK's normal credential chain.

The primary configuration variables are:

| Variable | Required | Purpose |
| --- | --- | --- |
| `PROJECT_ID` | Yes | Google Cloud project used by Vertex AI. |
| `DISCORD_BOT_TOKEN` | Yes | Token used for Discord Gateway and REST access. |
| `GOOGLE_APPLICATION_CREDENTIALS` | Environment-dependent | Path to application credentials for local or container execution. |
| `LOCATION` | No | Vertex AI location; defaults to `global`. |
| `DEFAULT_PROMPT` | No | Root-controlled assistant customization that may define its name and personality; empty by default. |
| `DYNAMODB_ENABLED` | No | Enables persistent history and server configuration; defaults to `false`. |
| `DYNAMODB_TABLE` | With DynamoDB | Existing DynamoDB table name; defaults to `jarvis`. |
| `AWS_REGION` | With DynamoDB | DynamoDB region resolved by the AWS SDK. |
| `AWS_ROLE_ARN` | For federation | AWS role assumed with a Google identity token. |
| `AWS_WEB_IDENTITY_AUDIENCE` | For federation | Audience placed in the Google identity token. |

Every command flag is also available as an uppercase environment variable with hyphens replaced by underscores. For example, `--message-retention-days` maps to `MESSAGE_RETENTION_DAYS`. Use `--help` to see all options.

## Architecture

Jarvis separates the stateful Discord connection from independently deployable message processing:

```text
Discord Gateway -> ingestor -> HTTP/1.1 raw protobuf -> worker
                                                       |-> Discord REST
                                                       |-> Vertex AI
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

- [Google Search grounding](docs/grounding.md)
- [DynamoDB storage, history, and multi-cloud authentication](docs/dynamodb.md)

## License

Jarvis is available under the [MIT License](LICENSE).
