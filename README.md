# autoclawpi

> Unofficial AutoClaw proxy — OpenAI-compatible API + web management panel

[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/badge/license-masantoid-blue)](LICENSE)
[![Platform](https://img.shields.io/badge/platform-Linux%20%7C%20Windows%20%7C%20macOS-333?logo=windows)]()

**autoclawpi** is a self-hosted proxy for AutoClaw that provides an OpenAI-compatible API endpoint with multi-account round-robin, auto token refresh, daily check-in, 100M token claim, and a full-featured web management panel.

---

## Features

### 🤖 OpenAI-Compatible API
- Drop-in replacement for OpenAI client — use any OpenAI library
- Model list verified real-time against upstream (auto-probed, refreshed every 10 min): `auto`, `auto-fast`, `glm-5.3`, `glm-5.3-flash`, `deepseek-v4-pro`, `deepseek-v4-flash`
- Round-robin load balancing across accounts
- Auto token refresh on 401
- Token usage tracking & cost estimation
- API key authentication
- WAF prefix stripping — handles multiple `{"message":"forbidden"}` prefixes
- WAF auto-retry — switch to next account on hard block + 500ms delay
- Token bucket rate limiter — prevents WAF rate-limit (configurable)

### 🌐 Web Management Panel
- **Dashboard** — account overview, points balance (+ token quota conversion: 1 pt = 10k tokens), API usage stats, cost tracking
- **Accounts** — OAuth login, import tokens, view details, JWT decode, health check
- **Claim 100M** — claim newbie token reward for fresh accounts
- **Check-In** — daily reward claiming for all accounts
- **Logs** — real-time API request log (auto-refresh every 3s, pauses on inactive tab) with terminal-style view + time range filter (Today/7D/30D/60D/All)
- **Settings** — password, multiple API keys (UI-managed, active for endpoint auth), round-robin strategy
- **Health** — test & refresh all account tokens
- **API Docs** — endpoint reference, curl examples, live model badges, rate limit & error docs
- **SPA** — single page application navigation (no full page reloads)
- **Responsive** — collapsible sidebar, adaptive layout

### 🔐 Security
- Web panel password protection
- API key authentication for OpenAI endpoint
- JWT token auto-decode (user info extracted from token)
- Session-based panel login with 30-day expiry

---

## Quick Start

### Prerequisites
- Go 1.26+ (for building from source)
- Linux (tested on Zorin OS 18.1), Windows 10/11, atau macOS

### Install

**Linux / macOS:**

```bash
git clone https://github.com/hirotomasato/autoclawpi.git
cd autoclawpi
go build -o ~/.local/bin/autoclawpi ./cmd/autoclawpi
```

**Windows (PowerShell):**

```powershell
git clone https://github.com/hirotomasato/autoclawpi.git
cd autoclawpi
go build -o "$env:USERPROFILE\.local\bin\autoclawpi.exe" ./cmd/autoclawpi
```

> ⚠️ **Windows**: binary harus berakhiran `.exe` agar bisa dieksekusi. Menambahkan `%USERPROFILE%\.local\bin` ke `PATH` membuat perintah `autoclawpi` bisa dipanggil dari mana saja.

### Run

**Linux / macOS:**

```bash
# Start server with password
autoclawpi serve --port 8787 --web-password yourpassword

# With API key protection
autoclawpi serve --port 8787 --web-password yourpassword --api-key sk-your-key
```

**Windows (PowerShell):**

```powershell
# Start server with password
autoclawpi.exe serve --port 8787 --web-password yourpassword

# With API key protection
autoclawpi.exe serve --port 8787 --web-password yourpassword --api-key sk-your-key
```

> 💡 Windows juga bisa menjalankan binary dari folder project langsung:
> `go build -o autoclawpi.exe ./cmd/autoclawpi` lalu `.\autoclawpi.exe serve`

Open **http://localhost:8787** in your browser.

---

## Usage

### CLI Commands

```bash
# Login (OAuth via browser with captcha)
autoclawpi login

# Account management
autoclawpi account list
autoclawpi account show 1
autoclawpi account add --access "Bearer eyJ..." --refresh "Bearer eyJ..."
autoclawpi account remove 1

# Check-in all accounts
autoclawpi checkin

# Start server
autoclawpi serve --port 8787 --web-password yourpassword --api-key sk-your-key
```

> 💡 **Windows**: ganti `autoclawpi` dengan `autoclawpi.exe` (atau `.\autoclawpi.exe` jika di folder project).

### API

```bash
# List models
curl http://localhost:8787/v1/models

# Chat completion
curl -X POST http://localhost:8787/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "auto", "messages": [{"role": "user", "content": "Hello!"}]}'

# With API key
curl -X POST http://localhost:8787/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-your-key" \
  -d '{"model": "deepseek-v4-pro", "messages": [{"role": "user", "content": "Hello!"}]}'

# Python SDK
python3 -c "
from openai import OpenAI
client = OpenAI(base_url='http://localhost:8787/v1', api_key='sk-your-key')
r = client.chat.completions.create(model='auto', messages=[{'role':'user','content':'halo'}])
print(r.choices[0].message.content)
"
```

### Web Panel Routes

| Route | Description |
|-------|-------------|
| `/` | Dashboard with stats |
| `/accounts` | Account management |
| `/accounts/{id}` | Account detail + claim 100M |
| `/accounts/login` | OAuth login |
| `/accounts/import` | Import token manually |
| `/checkin` | Daily check-in |
| `/logs` | Real-time API request logs, auto-refresh 3s (filter: `?range=today\|7d\|30d\|60d\|all`) |
| `/settings` | Configuration, API keys |
| `/health` | Account health check |
| `/docs` | API documentation |
| `/login` | Panel login |

---

## Architecture

```
autoclawpi
├── cmd/autoclawpi/       # CLI entry point
│   ├── main.go           # Command routing, server startup
│   ├── login.go          # OAuth login (captcha widget)
│   ├── account.go        # Account management commands
│   └── checkin.go        # Check-in command
├── internal/
│   ├── client/           # AutoClaw HTTP API client (inference, claim, check-in, OAuth)
│   ├── config/           # Configuration loading (rate limit, ports, etc.)
│   ├── db/               # SQLite database layer (accounts, logs, config)
│   ├── server/           # OpenAI-compatible proxy + token bucket rate limiter
│   ├── sign/             # X-Auth signature generation (md5-based)
│   ├── store/            # Credential storage (AES-GCM encrypted)
│   └── web/              # Web management panel
│       └── templates/    # HTML templates (glassmorphism dark theme)
└── go.mod
```

### Data Flow

```
Client (OpenAI SDK) → autoclawpi (port 8787)
  ├── Rate Limiter (token bucket: 1 req/1.5s, burst 3)
  ├── /v1/chat/completions → Round-robin account selection
  │   ├── Account #1 → AutoClaw API → Response → Token usage logged
  │   ├── 401 → Auto refresh token → Retry
  │   ├── 403 (WAF block) → Next account + 500ms delay
  │   └── ... (failover across all accounts)
  └── Web Panel → SQLite → Account management, logs, config
```

---

## Configuration

### Server Options

| Flag | Default | Description |
|------|---------|-------------|
| `--port` | `8787` | Listen port |
| `--host` | `127.0.0.1` | Listen host |
| `--web-password` | `""` | Panel password (required) |
| `--api-key` | `""` | API key for OpenAI endpoint |

### Config File

- Linux/macOS: `~/.autoclawpi/config.json`
- Windows: `%USERPROFILE%\.autoclawpi\config.json`

```json
{
  "rate_limit_per_sec": 0.667,
  "rate_limit_burst": 3
}
```

| Key | Default | Description |
|-----|---------|-------------|
| `rate_limit_per_sec` | `0.667` (1/1.5s) | Token refill rate per second |
| `rate_limit_burst` | `3` | Burst capacity for parallel requests |
| `host` | `127.0.0.1` | Listen host |
| `port` | `8787` | Listen port |
| `api_key` | `""` | Legacy API key via flag (still accepted). Prefer managing keys in Settings UI — stored in `config` table as `api_keys` and honored by the endpoint immediately, no restart needed |

### Rate Limiter Guide

| Config | Rate | Burst | Use Case |
|--------|------|-------|----------|
| Default | 1 req/1.5s | 3 | Coding (Claude Code/Cursor) |
| Safe | 1 req/2s | 2 | Conservative |
| Relaxed | 1 req/s | 5 | Heavy parallel tool calls |

### Environment Variables

| Variable | Description |
|----------|-------------|
| `AUTOCLAWPI_DIR` | Data directory (default: `~/.autoclawpi/`) |

---

## Storage

All data is stored in SQLite:

- Linux/macOS: `~/.autoclawpi/autoclawpi.db`
- Windows: `%USERPROFILE%\.autoclawpi\autoclawpi.db`

| Table | Description |
|-------|-------------|
| `accounts` | OAuth tokens, user info, points balance |
| `checkin_log` | Daily check-in history |
| `logs` | API request logs (tokens, cost, timestamps) |
| `config` | Key-value settings |

---

## Models

Model discovery is **real-time**: `/v1/models` probes each candidate against the upstream chat endpoint (parallel, cached 10 min, warmed at startup) — only models the upstream currently accepts are listed.

Verified live (2026-09-12):

| Model | Route ID | Status |
|-------|----------|--------|
| `auto` | `zai_auto` | ✅ routed to available model |
| `auto-fast` | `zai_auto-fast` | ✅ |
| `glm-5.3` | `zaicoding_glm-5.3` | ✅ (requires coding-plan quota) |
| `glm-5.3-flash` | `zai_glm-5.3-flash` | ✅ |
| `deepseek-v4-pro` | `tdpsk_deepseek-v4-pro-202606` | ✅ |
| `deepseek-v4-flash` | `tdpsk_deepseek-v4-flash-202605` | ✅ |
| `glm-5-turbo` | — | ❌ rejected upstream (400 非法模型) |
| `glm-5.2` | — | ❌ rejected upstream (400 非法模型) |

---

## Screenshots

```
┌─────────────────────────────────────────────┐
│  Dashboard                                   │
│  ┌──────┐ ┌──────┐ ┌──────┐                 │
│  │Accts │ │Active│ │Points│                 │
│  │  4   │ │  4   │ │ 105K │                 │
│  └──────┘ └──────┘ └──────┘                 │
│  ┌──────┐ ┌──────┐ ┌──────┐                 │
│  │ Req  │ │Tokens│ │ Cost │                 │
│  │  25  │ │ 1.5K │ │ 0.00 │                 │
│  └──────┘ └──────┘ └──────┘                 │
│  ─── Accounts ─────────────────────────      │
│  ✓ Account #5           27,181 pts           │
│  ✓ Account #6           36,200 pts           │
│  ✓ Account #7           36,200 pts           │
│  ✓ Account #9           11,940 pts           │
└─────────────────────────────────────────────┘
```

---

## Security

- **Panel access**: Password-protected (`--web-password`)
- **API access**: Optional API key (`--api-key`)
- **Token storage**: Stored in SQLite (local only)
- **Session**: Cookie-based with 30-day expiry
- **JWT decode**: Auto-extract user info from tokens

---

## Why autoclawpi?

- **Self-hosted** — full control over your data
- **OpenAI-compatible** — use any existing OpenAI tooling
- **Multi-account** — round-robin with auto failover + WAF bypass
- **Rate-limited** — token bucket prevents WAF blocks
- **Token management** — auto-refresh, never lose access
- **Claim 100M** — one-click newbie token reward
- **Monitoring** — real-time logs, cost tracking, health checks
- **Responsive** — collapsible sidebar, adaptive layout

---

## License

masantoid — see [LICENSE](LICENSE) file for details.

---

*Built by masanto*