# Agents Proxy

Agents Proxy is a configurable LLM API proxy service. It provides a single entry point for clients and forwards requests to configured upstream providers such as Anthropic, OpenAI, Google/Gemini, or compatible third-party APIs.

The service is designed for managing multiple routes, models, providers, API endpoints, credentials, fallback targets, logs, and request testing from one place.

## Features

- **Unified LLM endpoint**: Send requests through `/llm/<route-id>/v1/messages`.
- **Multi-provider support**: Use Anthropic, OpenAI, Google/Gemini, or compatible API providers.
- **Model mapping**: Map client model names to provider-specific upstream model IDs.
- **Provider failover**: Configure multiple ordered targets for a route and automatically try the next target when one fails.
- **API type selection**: Configure `api_type` at the route and model-mapping level.
- **Streaming responses**: Supports SSE streaming responses.
- **Tool calling**: Supports tool-use requests and responses.
- **Web management UI**: Manage routes, providers, users, tokens, logs, app settings, and request tests from the built-in UI.
- **Hot reload**: Configuration changes can be loaded without restarting the service.
- **Authentication**: Supports API key authentication with `x-api-key` and `Authorization: Bearer` headers.
- **Logging**: Provides application logs and per-request LLM call logs.
- **HTTP proxy support**: Supports global proxy settings and provider-level proxy settings.

## Quick Start

### 1. Create `config.yaml`

Create a `config.yaml` file with at least one credential, one route, and one provider.

```yaml
app:
  level: info
  auth: true
  listen: "0.0.0.0"
  port: "8082"

users:
  - name: admin
    token: your_token

tokens:
  - id: claude-code
    token: your_token

routes:
  claude-code:
    api_type: anthropic
    targets:
      - name: primary
        enable: true
        models:
          - match_model: "*"
            provider: anthropic
            model_id: claude-sonnet-4-20250514
            api_name: default
            api_type: anthropic

providers:
  anthropic:
    enable: true
    models:
      - model_id: claude-sonnet-4-20250514
    apis:
      - name: default
        api_type: anthropic
        base_url: https://api.anthropic.com/v1
        api_key: your_api_key
```

If a model mapping does not define `api_type`, it uses the route's `api_type` by default.

### 2. Start the service

```bash
go run main.go
```

Use a custom config path:

```bash
go run main.go /path/to/config.yaml
```

### 3. Send a request

```bash
curl -X POST http://localhost:8082/llm/claude-code/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: your_token" \
  -d '{
    "model": "claude-sonnet-4-20250514",
    "max_tokens": 1024,
    "messages": [
      {"role": "user", "content": "Hello"}
    ]
  }'
```

### 4. Open the web UI

Open the management dashboard in a browser:

```text
http://localhost:8082/
```

After logging in, you can manage routes, providers, users, tokens, app settings, logs, and test requests.

![admin ui](./docs/ui-1.png)

## LLM API

| Route | Description |
|-------|-------------|
| `/llm/<route-id>/v1/messages` | Send an LLM request to the configured route |
| `/llm/<route-id>/v1/messages/count_tokens` | Count tokens for a request |

## Management API

| Route | Method | Description |
|-------|--------|-------------|
| `/api/config` | GET | Get the current configuration |
| `/api/config` | POST | Update the configuration |
| `/api/routes` | GET | List routes |
| `/api/providers` | GET | List providers |
| `/api/logs/llm` | GET | Query LLM call logs |
| `/api/logs/app` | GET | Query application logs |

## Configuration Reference

### `app`

| Field | Description |
|-------|-------------|
| `level` | Application log level: `debug`, `info`, `warn`, or `error` |
| `auth` | Enables or disables authentication |
| `listen` | Listen address |
| `port` | Listen port |

### `users` and `tokens`

Use `users` and `tokens` to configure credentials for clients that access Agents Proxy.

Supported request headers:

```text
x-api-key: your_token
Authorization: Bearer your_token
```

If `app.auth` is `false`, requests do not require authentication.

### `routes`

Routes define client-facing API entries and model forwarding rules.

| Field | Description |
|-------|-------------|
| `api_type` | Client-facing API type: `anthropic`, `openai`, or `gemini` |
| `targets` | Ordered target groups used for forwarding and failover |
| `targets[].name` | Target group name |
| `targets[].enable` | Enables or disables the target group |
| `targets[].models[].match_model` | Client model matching rule; supports exact match, prefix wildcard, and `*` |
| `targets[].models[].provider` | Provider used for the matched model |
| `targets[].models[].model_id` | Upstream model ID sent to the provider |
| `targets[].models[].api_name` | Provider API endpoint name |
| `targets[].models[].api_type` | Provider API type; defaults to the route `api_type` when omitted |

### `providers`

Providers define upstream services, models, API endpoints, and optional proxy settings.

| Field | Description |
|-------|-------------|
| `enable` | Enables or disables the provider |
| `proxy` | Optional provider-specific HTTP proxy URL |
| `models` | Models available for this provider |
| `apis[].name` | API endpoint name |
| `apis[].api_type` | API endpoint type: `anthropic`, `openai`, or `gemini` |
| `apis[].base_url` | API endpoint base URL |
| `apis[].api_key` | API key for the endpoint |

Within the same provider, the combination of `api_name` and `api_type` must be unique.

## Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `PORT` | No | `8082` | Service port; can be overridden by `app.port` in `config.yaml` |
| `LOG_LEVEL` | No | `info` | Log level; can be overridden by `app.level` in `config.yaml` |
| `PROXY_URL` | No | - | Global HTTP proxy URL |

A `.env` file can be used for these environment variables. API keys and upstream base URLs should be configured in `config.yaml`.

## Build

```bash
go build -o agents-proxy .
```

Run the built binary:

```bash
./agents-proxy
```

Run with a custom config path:

```bash
./agents-proxy /path/to/config.yaml
```

## Docker Deployment

Build an image:

```bash
docker build -t agents-proxy .
```

Run a container:

```bash
docker run -d \
  -p 8082:8082 \
  -v /etc/localtime:/etc/localtime:ro \
  -v /etc/timezone:/etc/timezone:ro \
  -e "TZ=Asia/Shanghai" \
  -v $(pwd)/config.yaml:/app/config.yaml \
  -v $(pwd)/logs:/app/logs \
  --name agents-proxy \
  agents-proxy
```

Or use the published image:

```bash
docker run -d \
  -p 8082:8082 \
  -v /etc/localtime:/etc/localtime:ro \
  -v /etc/timezone:/etc/timezone:ro \
  -e "TZ=Asia/Shanghai" \
  -v $(pwd)/config.yaml:/app/config.yaml \
  -v $(pwd)/logs:/app/logs \
  --name agents-proxy \
  ghcr.io/mark0725/go-agents-proxy:latest
```

Make sure the host config file and log directory exist before starting the container:

```text
$(pwd)/config.yaml
$(pwd)/logs
```

## Logs

- Application log: `logs/app.log`
- LLM call logs: `logs/llm-YYYY-MM-DD.jsonl`

Logs can also be viewed from the web management UI.

## License

MIT
