
<h1 align="center">Switchboard Go</h1>



THIS IS A FORK

I am attempting to get this to work for other services as well for people who have multiple providers


Switchboard Go is a small local proxy for the OpenCode Go API.

It gives OpenAI-compatible and Anthropic Messages-compatible tools one stable
local endpoint and automatically cycles through your upstream OpenCode Go API
keys when one is exhausted.

Most users should run it on their own computer:

```text
OpenAI/Anthropic-compatible app -> http://127.0.0.1:8080/v1 -> OpenCode Go
```

## Why use it?

- One local `/v1/*` endpoint for OpenAI-compatible and Anthropic Messages
  requests
- One proxy API key for your tools
- Multiple upstream OpenCode Go keys behind the scenes
- Automatic failover when an upstream key is exhausted
- Automatic retry of exhausted keys after a configurable cooldown, so a
  replenished account recovers without a restart or manual reset
- **Multi-provider failover**: configure multiple upstream providers
  (OpenCode Go, DeepSeek, Xiaomi, OpenAI, etc.) and Switchboard Go will
  automatically fall through to the next provider when one is quota-exhausted
- Optional YAML config, Docker, admin status, and SMTP alerts

## Install

Download a binary from GitHub Releases:

```text
https://github.com/ArsalanDotMe/switchboard-go/releases
```

## Quick start

```bash
export PROXY_API_KEY="replace-with-a-long-random-local-key"
export OPENCODE_GO_API_KEYS="sk-first,sk-second,sk-third"
export LISTEN_ADDR="127.0.0.1:8080"

switchboard-go
```

## Use it from an OpenAI-compatible client

Use the proxy key, not your upstream OpenCode Go key:

```bash
export OPENAI_BASE_URL="http://127.0.0.1:8080/v1"
export OPENAI_API_KEY="$PROXY_API_KEY"
```

Example request:

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $PROXY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "glm-5.1",
    "messages": [{"role": "user", "content": "Say hello"}],
    "max_tokens": 100
  }'
```

## Use it from an Anthropic Messages-compatible client

Anthropic-style clients should use the same base URL and proxy key. Switchboard
Go authenticates clients with the proxy key, then forwards upstream with the
current OpenCode Go key in `x-api-key`:

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8080"
export ANTHROPIC_API_KEY="$PROXY_API_KEY"
```

Example request:

```bash
curl http://127.0.0.1:8080/v1/messages \
  -H "x-api-key: $PROXY_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "minimax-m3",
    "max_tokens": 100,
    "messages": [{"role": "user", "content": "Say hello"}]
  }'
```

For opencode and Pi Coding Agent examples, see
[docs/agent-config.md](docs/agent-config.md).

## Common settings

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `PROXY_API_KEY` | Yes | | Key clients use to access Switchboard Go. |
| `OPENCODE_GO_API_KEYS` | Yes | | Comma-separated upstream OpenCode Go API keys. |
| `LISTEN_ADDR` | No | `:8080` | Use `127.0.0.1:8080` for local-only access. |
| `UPSTREAM_BASE_URL` | No | `https://opencode.ai/zen/go/v1` | OpenCode Go upstream base URL. |
| `RETRY_EXHAUSTED_AFTER` | No | `5m` | Cooldown before an exhausted key is retried automatically. `0` disables it. |

YAML config is also supported. See
[docs/configuration.md](docs/configuration.md).

## Multi-provider failover

When you have subscriptions with multiple providers, Switchboard Go can
automatically fall through when one runs out of quota. Configure via YAML:

```yaml
providers:
  - name: opencode-go
    base_url: "https://opencode.ai/zen/go/v1"
    api_keys: ["sk-go-1", "sk-go-2"]
    priority: 0          # tried first
  - name: deepseek
    base_url: "https://api.deepseek.com/v1"
    api_keys: ["sk-ds-1"]
    priority: 1          # fallback after opencode-go is exhausted
  - name: xiaomi
    base_url: "https://api.xiaomi.com/v1"
    api_keys: ["sk-xm-1"]
    priority: 2          # last resort
```

Providers are tried in priority order (lowest first). Within each provider,
keys cycle with the same cooldown-and-retry logic as single-provider mode.
A local 429 with `Retry-After` is returned only when **all** providers are
exhausted.

Without a `providers` block, Switchboard Go falls back to legacy single-provider
mode (env vars `OPENCODE_GO_API_KEYS` + `UPSTREAM_BASE_URL`).

### ChatGPT Plus (OAuth)

You can use a ChatGPT Plus subscription as a fallback provider without an API
key. Switchboard Go will authenticate via browser OAuth and auto-refresh the
token:

```yaml
providers:
  - name: opencode-go
    base_url: "https://opencode.ai/zen/go/v1"
    api_keys: ["sk-go-1"]
    priority: 0
  - name: chatgpt-plus
    auth_type: oauth       # browser OAuth PKCE flow
    priority: 1
```

Before first use, authenticate once:

```bash
switchboard-go oauth-login chatgpt-plus
```

This opens your browser to the ChatGPT consent screen, exchanges the code for
tokens, and saves them to `~/.config/switchboard-go/chatgpt-plus-oauth.json`.
The access token auto-refreshes and the proxy will fall through to ChatGPT
when your other providers run out of quota.

## Admin endpoints

Use `Authorization: Bearer $PROXY_API_KEY`:

- `GET /admin/status` — per-provider, per-key status
- `POST /admin/validate-keys` — validate all keys across all providers
- `POST /admin/reset-key` — un-exhaust a single key by provider + index
- `POST /admin/reset-all-keys` — un-exhaust every key across all providers

Health checks:

- `GET /healthz`
- `GET /readyz`

See [docs/admin-api.md](docs/admin-api.md).

## More docs

- [Configuration](docs/configuration.md)
- [Agent/client setup](docs/agent-config.md)
- [Admin API](docs/admin-api.md)
- [Docker](docs/docker.md)
- [systemd deployment](docs/deployment.md)
- [SMTP notifications](docs/smtp.md)
- [Operations and security](docs/operations.md)

## Development

```bash
go test ./...
gofmt -w .
go build ./...
```

## License

MIT. See [LICENSE](LICENSE).
