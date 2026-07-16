# Jarvis

Jarvis is a fast, open source AI chatbot for Discord with built-in search. It is designed to be intuitive and easy to set up, and uses Google Vertex AI to generate responses.

## Quick start

You will need:

- A Discord bot token
- A Google Cloud project with Vertex AI enabled
- Google Cloud application credentials

Run both services locally with Bazel:

```sh
export PROJECT_ID=YOUR_GCP_PROJECT_ID
export DISCORD_BOT_TOKEN=YOUR_DISCORD_BOT_TOKEN
bazel run //:jarvis
```

The multirun target starts the ingestor health server on port 8080 and the worker HTTP server on port 8081. Use `bazel run //:ingestor -- --help` and `bazel run //:worker -- --help` to inspect or start either service independently.

Use `--help` to see all configuration options.

## Architecture

Jarvis consists of two independent services:

```text
Discord Gateway -> ingestor -> HTTP/1.1 raw protobuf -> worker
                                                       |-> Discord REST
                                                       |-> Vertex AI
                                                       `-> DynamoDB (optional)
```

The ingestor is the only service that opens a stateful Discord Gateway connection. It normalizes each message into the versioned protobuf contract in `api/jarvis/discord/v1/worker.proto` and synchronously posts it to the configured `WORKER_URL`. It does not start, stop, monitor, or otherwise manage a worker process.

The worker exposes `POST /v1/messages:process` over HTTP/1.1. The request body is an `IngestMessageRequest` serialized as raw protobuf with content type `application/x-protobuf`. The worker returns `204 No Content` only after processing completes. This matches Pub/Sub push payload unwrapping: a future publisher can place the same bytes on a topic and configure a no-wrapper push subscription without changing the worker handler.

The processing endpoint intentionally has no application-level authentication. The combined image binds it to loopback. A standalone worker must be deployed behind an internal VPC or another trusted network boundary and must not be exposed directly to the public internet. The direct HTTP publisher makes one synchronous attempt because processing has Discord side effects; retries and durable deduplication are deferred until Pub/Sub is introduced.

The ingestor and worker have separate one-process images. A combined image is also available for single-container deployments. Build and load them locally with:

```sh
bazel run //:ingestor_image_load
bazel run //:worker_image_load
bazel run //:image_load
```

Run the combined image with shared configuration supplied through environment variables:

```sh
docker run --rm \
  -p 8080:8080 \
  -v "$HOME/.config/gcloud/application_default_credentials.json:/credentials.json:ro" \
  -e GOOGLE_APPLICATION_CREDENTIALS=/credentials.json \
  -e PROJECT_ID=YOUR_GCP_PROJECT_ID \
  -e DISCORD_BOT_TOKEN=YOUR_DISCORD_BOT_TOKEN \
  justinswe/jarvis
```

The combined image runs a small PID 1 supervisor. It starts the worker on loopback port 8081, waits for `/readyz`, and then starts the ingestor on `$PORT`. The supervisor only owns process lifecycle and signal forwarding; the ingestor remains independent and communicates with the worker through the same HTTP/1.1 protobuf endpoint used by isolated deployments. If either process exits, the supervisor stops the other and exits so the container platform can replace the instance.

The existing publish workflow publishes the combined image as `justinswe/jarvis`. Publication of the isolated `justinswe/jarvis-ingestor` and `justinswe/jarvis-worker` images is intentionally deferred. In a deployed isolated configuration, set `WORKER_URL` to the worker's complete processing endpoint. Both services use `$PORT` for their own HTTP server and expose `/healthz` and `/readyz`. Every command flag is also available as an uppercase environment variable with hyphens replaced by underscores; for example, `--worker-request-timeout` maps to `WORKER_REQUEST_TIMEOUT`.

Server behavior is resolved for every targeted request through the configuration provider in `internal/config`. DynamoDB integration is disabled by default, in which case the worker uses validated hard-coded settings and Discord REST history. Enable it with `DYNAMODB_ENABLED=true`; the worker then records every valid incoming MessageCreate event, reads conversation history from DynamoDB, and allows authorized server administrators to manage per-server settings through model tools. Data failures are fail-open: message processing continues with defaults or partial context. See [docs/dynamodb.md](docs/dynamodb.md) for the table contract, flags, access model, and provisioning requirements.

Google Search grounding is model-directed and strictly verified once Search emits a query. See [docs/grounding.md](docs/grounding.md) for retry behavior, Discord source rendering, and diagnostic fields.

## Development

Format protobuf definitions and run the complete test suite with Bazel:

```sh
bazel run //:buf_format
bazel test //...
```

## License

See [LICENSE](LICENSE).
