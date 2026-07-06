# Jarvis

Jarvis is a fast, open source AI chatbot for Discord with built-in search. It is designed to be intuitive and easy to set up, and uses Google Vertex AI to generate responses.

## Quick start

You will need:

- A Discord bot token
- A Google Cloud project with Vertex AI enabled
- Google Cloud application credentials

Run Jarvis with Docker:

```sh
docker run --rm \
  -p 8080:8080 \
  -v "$HOME/.config/gcloud/application_default_credentials.json:/credentials.json:ro" \
  -e GOOGLE_APPLICATION_CREDENTIALS=/credentials.json \
  justinswe/jarvis \
  --project-id YOUR_GCP_PROJECT_ID \
  --discord-bot-token YOUR_DISCORD_BOT_TOKEN
```

The Docker image is available on [Docker Hub](https://hub.docker.com/r/justinswe/jarvis).

Use `--help` to see all configuration options.

## License

See [LICENSE](LICENSE).
