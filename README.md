# BabelGate 🗼

<p align="center">
  <b>A blazing-fast, zero-dependency, any-to-any LLM gateway & real-time translation proxy written in Go.</b><br>
  Route requests from any LLM client or agent (<b>Claude Code</b>, OpenAI SDK, Gemini SDK, Cursor) to any upstream provider with full SSE streaming, bidirectional tool calling, model aliasing, and session telemetry.
</p>

<p align="center">
  <a href="https://golang.org/"><img src="https://img.shields.io/badge/Go-1.24+-00ADD8?style=flat&logo=go" alt="Go Version"></a>
  <a href="#running-tests"><img src="https://img.shields.io/badge/tests-passing-brightgreen?style=flat" alt="Tests"></a>
  <a href="#key-features"><img src="https://img.shields.io/badge/dependencies-zero%20external-success?style=flat" alt="Zero Dependencies"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue.svg?style=flat" alt="License"></a>
  <a href="#quickstart"><img src="https://img.shields.io/badge/status-production--ready-orange?style=flat" alt="Status"></a>
</p>

---

## Table of Contents

- [Overview](#overview)
- [Key Features](#key-features)
- [Protocol Support Matrix](#protocol-support-matrix)
- [Architecture](#architecture)
- [Quickstart](#quickstart)
- [Popular Recipes & Client Integrations](#popular-recipes--client-integrations)
  - [1. Claude Code with Google Gemini](#1-claude-code-with-google-gemini)
  - [2. Claude Code with GitHub Copilot](#2-claude-code-with-github-copilot)
  - [3. Claude Code with Local / Enterprise LLMs (Ollama, vLLM, DeepSeek)](#3-claude-code-with-local--enterprise-llms-ollama-vllm-deepseek)
  - [4. OpenAI SDK (Python & Node) to Claude or Gemini](#4-openai-sdk-python--node-to-claude-or-gemini)
  - [5. Google GenAI SDK / cURL to OpenAI or Claude](#5-google-genai-sdk--curl-to-openai-or-claude)
- [GitHub Copilot Authentication](#github-copilot-authentication)
- [Web Dashboard & Playground](#web-dashboard--playground)
- [Configuration Reference](#configuration-reference)
- [Unified API Endpoints](#unified-api-endpoints)
- [Development & Testing](#development--testing)

---

## Overview

Just as the mythical Tower of Babel was the intersection of all human languages, **BabelGate** serves as the universal real-time translator for AI model protocols:
- **Claude Code** expects the Anthropic Messages API (`/v1/messages`) with strict SSE framing and tool definitions.
- **OpenAI clients** expect the Chat Completions API (`/v1/chat/completions`).
- **Google GenAI clients** expect the Gemini REST API (`/v1beta/models/...`).

**BabelGate** bridges this divide. It accepts incoming requests in **any** supported protocol, converts them into a normalized canonical structure, and routes them out to **any** upstream provider—translating streaming events, tool/function calls, and token telemetry in real time.

---

## Key Features

- 🔄 **True Any-to-Any Translation**:
  Seamless bidirectional translation of system prompts, multi-turn messages, multimodal content (images/base64), function/tool declarations, tool invocations, and tool results across Anthropic, OpenAI, and Google Gemini.
- ⚡ **Real-Time Streaming SSE Engine**:
  Ultra-low latency streaming transformation (e.g. Gemini `streamGenerateContent` chunks → Anthropic `message_start`, `content_block_delta`, `message_delta`, `message_stop`).
- 🤖 **Turnkey Claude Code Support**:
  Run Claude Code against Google Gemini 2.5, GitHub Copilot, or local models without modifying Claude Code's binary.
- 🔑 **GitHub Copilot RFC 8628 Integration**:
  Built-in OAuth Device Flow (`./bin/babelgate -copilot-login` or web UI at `/setup`) and zero-config discovery of existing VS Code / JetBrains / GitHub CLI credentials.
- 📊 **Built-in Session & Token Telemetry**:
  Automatically identifies active clients (Claude Code, OpenAI SDK, Web Playground), tracks input/output/total token usage, duration, and error rates per session.
- 🖥️ **Embedded Dark Web Dashboard**:
  Inspect connected providers, browsable unified model catalog, active sessions, and an interactive streaming playground at `http://localhost:8080/`.
- 🔀 **Flexible Routing & Failover Chains**:
  Route by provider prefix (`google/gemini-2.5-pro`), transparent alias (`claude-3-7-sonnet` → `copilot/gpt-4o`), or automatic multi-provider fallback.
- 📦 **Zero Runtime Dependencies**:
  Pure Go standard library core with minimal YAML parsing. Compiles to a single static binary.

---

## Protocol Support Matrix

| Inbound Client Protocol | Upstream: Anthropic | Upstream: Google Gemini | Upstream: OpenAI | Upstream: GitHub Copilot | Upstream: OpenAI-Compatible (Ollama, vLLM, Corporate) |
|---|:---:|:---:|:---:|:---:|:---:|
| **Anthropic Messages** (`/v1/messages`) | ✅ Direct | ✅ Full Translation | ✅ Full Translation | ✅ Full Translation | ✅ Full Translation |
| **OpenAI Chat** (`/v1/chat/completions`) | ✅ Full Translation | ✅ Full Translation | ✅ Direct | ✅ Full Translation | ✅ Direct |
| **Google Gemini REST** (`/v1beta/...`) | ✅ Full Translation | ✅ Direct | ✅ Full Translation | ✅ Full Translation | ✅ Full Translation |
| **Bidirectional Tool Calling** | ✅ | ✅ | ✅ | ✅ | ✅ |
| **Server-Sent Events (SSE)** | ✅ | ✅ | ✅ | ✅ | ✅ |
| **Multimodal / Vision** | ✅ | ✅ | ✅ | ✅ | ✅ |

---

## Architecture

```
                       INBOUND CLIENTS & AGENTS
      ┌──────────────────────┬──────────────────────┬──────────────────────┐
      │     Claude Code      │      OpenAI SDK      │    Google SDK /      │
      │  (Anthropic Client)  │   (OpenAI Client)    │   Gemini Client      │
      └──────────┬───────────┴──────────┬───────────┴──────────┬───────────┘
                 │                      │                      │
                 ▼                      ▼                      ▼
           /v1/messages        /v1/chat/completions    /v1beta/models/...
                 │                      │                      │
      ┌──────────┴──────────────────────┴──────────────────────┴───────────┐
      │                      CANONICAL PROTOCOL LAYER                      │
      │  • Normalized Messages, Multimodal Parts, Tools & Invocations      │
      │  • Unified Streaming SSE Event Bus                                 │
      │  • Session & Token Usage Aggregator                                │
      └─────────────────────────────────┬──────────────────────────────────┘
                                        │
                                        ▼
      ┌────────────────────────────────────────────────────────────────────┐
      │                     ROUTING & ALIAS ENGINE                         │
      │  • Model Alias Mapping (e.g. claude-3-7-sonnet -> gemini-2.5-pro)  │
      │  • Provider Dispatch, Priority Ordering & Failover Fallbacks       │
      └───────┬──────────────┬──────────────┬──────────────┬───────────────┘
              │              │              │              │
              ▼              ▼              ▼              ▼
      ┌──────────────┐┌──────────────┐┌──────────────┐┌────────────────────┐
      │  Anthropic   ││ Google Gemini││    OpenAI    ││ GitHub Copilot /   │
      │ (Claude 3.7) ││ (Gemini 2.5) ││(GPT-4o, o1)  ││ Local (Ollama/vLLM)│
      └──────────────┘└──────────────┘└──────────────┘└────────────────────┘
```

---

## Quickstart

### 1. Build from Source

**macOS / Linux:**
```bash
git clone https://github.com/vogler75/babel-gate.git
cd babel-gate
make build
```

**Windows (PowerShell / Command Prompt):**
```powershell
git clone https://github.com/vogler75/babel-gate.git
cd babel-gate
go build -o bin\babelgate.exe .\cmd\router
```

This generates the standalone binary in `./bin/babelgate` (`bin\babelgate.exe` on Windows, with `./bin/llm-router` symlinked on Unix).

### 2. Run with Zero-Config

`babelgate` automatically detects API keys from your environment variables:

**macOS / Linux:**
```bash
export GEMINI_API_KEY="AIzaSy..."
export ANTHROPIC_API_KEY="sk-ant-..."
export OPENAI_API_KEY="sk-..."

./bin/babelgate
```

**Windows (PowerShell):**
```powershell
$env:GEMINI_API_KEY="AIzaSy..."
$env:ANTHROPIC_API_KEY="sk-ant-..."
$env:OPENAI_API_KEY="sk-..."

.\bin\babelgate.exe
```

> [!TIP]
> You can also create a `.env` file in the root directory. `babelgate` loads it automatically at startup!

### 3. Verify the Gateway

Open **[`http://localhost:8080/`](http://localhost:8080/)** in your browser to explore the **Web Dashboard**, inspect connected models, and test live streaming responses.

Or query the unified health endpoint:
```bash
curl http://localhost:8080/api/status
```

---

## Popular Recipes & Client Integrations

### 1. Claude Code with Google Gemini

Route Claude Code to Google's high-context Gemini models:

1. Create or edit `config.yaml`:
   ```yaml
   providers:
     google:
       type: google
       api_key: "${GEMINI_API_KEY}"

   routing:
     routes:
       "claude-3-7-sonnet": "google/gemini-2.5-pro"
       "claude-3-5-sonnet": "google/gemini-2.5-flash"
   ```

2. Launch `babelgate`:
   ```bash
   ./bin/babelgate -config config.yaml
   ```

3. Start Claude Code pointing to BabelGate:
   ```bash
   export ANTHROPIC_BASE_URL="http://localhost:8080"
   export ANTHROPIC_API_KEY="dummy-key"
   claude
   ```

   Alternatively, configure the required environment settings in your `.claude/settings.json` (or `~/.claude/settings.json`):
   ```json
   {
     "env": {
       "ANTHROPIC_BASE_URL": "http://localhost:8080/",
       "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS": "1",
       "CLAUDE_CODE_ENABLE_TELEMETRY": "1",
       "CLAUDE_CODE_USE_BEDROCK": "0",
       "DISABLE_PROMPT_CACHING": "0",
       "CLAUDE_CODE_DISABLE_1M_CONTEXT": "0",
       "CLAUDE_CODE_BLOCKING_LIMIT_OVERRIDE": "200000",
       "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE": "50",
       "CLAUDE_CODE_EFFORT_LEVEL": "medium",
       "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"
     }
   }
   ```

Claude Code executes its full agentic loop (terminal execution, file viewing, codebase search) through Gemini 2.5 Pro!

---

### 2. Claude Code with GitHub Copilot

Leverage your GitHub Copilot subscription inside Claude Code:

1. Authenticate Copilot (see [GitHub Copilot Authentication](#github-copilot-authentication)):
   ```bash
   ./bin/babelgate -copilot-login
   ```

2. Add routing in `config.yaml`:
   ```yaml
   providers:
     copilot:
       type: copilot

   routing:
     routes:
       "claude-3-7-sonnet": "copilot/gpt-4o"
       "claude-3-5-sonnet": "copilot/claude-3.5-sonnet"
   ```

3. Run Claude Code:
   ```bash
   export ANTHROPIC_BASE_URL="http://localhost:8080"
   export ANTHROPIC_API_KEY="dummy"
   claude
   ```

---

### 3. Claude Code with Local / Enterprise LLMs (Ollama, vLLM, DeepSeek)

Connect Claude Code or any Anthropic agent to local or enterprise OpenAI-compatible endpoints:

```yaml
providers:
  local:
    type: openai
    base_url: "http://localhost:11434/v1"   # Ollama, vLLM, LocalAI, or Enterprise Gateway
    api_key: "ollama"

routing:
  routes:
    "claude-3-7-sonnet": "local/qwen2.5-coder:32b"
```

---

### 4. OpenAI SDK (Python & Node) to Claude or Gemini

Point any standard OpenAI SDK client directly to `babelgate`:

#### Python
```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="dummy-key"
)

# Stream from Anthropic Claude using the OpenAI SDK
stream = client.chat.completions.create(
    model="anthropic/claude-3-7-sonnet-20250219",
    messages=[{"role": "user", "content": "Explain raft consensus in 3 bullet points."}],
    stream=True,
)

for chunk in stream:
    if chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="", flush=True)
```

#### Node / TypeScript
```typescript
import OpenAI from "openai";

const openai = new OpenAI({
  baseURL: "http://localhost:8080/v1",
  apiKey: "dummy-key",
});

const completion = await openai.chat.completions.create({
  model: "google/gemini-2.5-pro",
  messages: [{ role: "user", content: "Hello Gemini from OpenAI SDK!" }],
});

console.log(completion.choices[0].message.content);
```

---

### 5. Google GenAI SDK / cURL to OpenAI or Claude

Use standard Google Gemini REST endpoints to query OpenAI or Anthropic models:

```bash
curl -X POST "http://localhost:8080/v1beta/models/openai/gpt-4o:generateContent" \
  -H "Content-Type: application/json" \
  -d '{
    "contents": [{
      "role": "user",
      "parts": [{"text": "Hello GPT-4o via Gemini syntax!"}]
    }]
  }'
```

---

## GitHub Copilot Authentication

GitHub Copilot can be authenticated through three convenient methods:

1. **Interactive CLI Device Code Flow (RFC 8628)**:
   ```bash
   ./bin/babelgate -copilot-login
   ```
   Follow the displayed URL and 8-character code to authorize. Credentials are securely stored in `~/.config/github-copilot/hosts.json`.

2. **Web Browser Setup UI**:
   Visit **`http://localhost:8080/setup`**, click **"Start Device Registration"**, and complete authorization in your browser.

3. **Zero-Config Auto-Discovery**:
   If you are already logged in via VS Code, JetBrains, or the GitHub CLI (`gh auth login`), `babelgate` automatically reads your tokens from:
   - `~/.config/github-copilot/apps.json`
   - `~/.config/github-copilot/hosts.json`
   - `~/.config/gh/hosts.yml`

4. **Environment Variable**:
   ```bash
   export GITHUB_TOKEN="ghp_..."
   # or
   export COPILOT_API_KEY="ghu_..."
   ```

---

## Web Dashboard & Playground

`babelgate` includes an embedded dark-themed web console available at **`http://localhost:8080/`**:

- **Active Provider Status**: Real-time health, priority levels, and endpoint configurations.
- **Unified Model Catalog**: Interactive table of all upstream and aliased models.
- **Streaming Prompt Playground**: Test any connected model with live token streaming and duration metrics directly in your browser.
- **Live Session Telemetry**: View incoming clients (e.g. Claude Code, SDKs), request counts, input/output token usage, duration, and error logs.
- **Copilot Device Setup**: One-click device authorization at `/setup`.

---

## Configuration Reference

A complete template is available in [`config.example.yaml`](config.example.yaml):

```yaml
server:
  port: 8080               # Port to listen on (or use $PORT / -port flag)
  api_key: ""              # Optional: require clients to provide this key (Bearer / x-api-key)
  timeout_seconds: 120     # Upstream request timeout
  cors_origins: ["*"]      # Allowed CORS origins

# Upstream LLM Providers
# Supports environment variable expansion (${VAR} or $VAR)
providers:
  google:
    type: google           # "google", "anthropic", "openai", "copilot"
    api_key: "${GEMINI_API_KEY}"
    priority: 1            # Priority for default selection (lower = higher priority)

  anthropic:
    type: anthropic
    api_key: "${ANTHROPIC_API_KEY}"
    priority: 2

  openai:
    type: openai
    api_key: "${OPENAI_API_KEY}"
    priority: 3

  copilot:
    type: copilot

# Routing, Aliases, and Fallbacks
routing:
  # Exact model name aliases
  routes:
    "claude-3-7-sonnet": "google/gemini-2.5-pro"
    "gpt-4o": "anthropic/claude-3-7-sonnet-20250219"

  # Default model if requested model is not found
  default: "google/gemini-2.5-flash"

  # Automatic fallback chains if primary model fails or is rate-limited
  fallbacks:
    "google/gemini-2.5-pro":
      - "anthropic/claude-3-7-sonnet-20250219"
      - "openai/gpt-4o"
```

---

## Unified API Endpoints

| Endpoint | Methods | Protocol / Format | Description |
|---|:---:|---|---|
| `/` | `GET` | HTML / Web | Embedded Web Dashboard & Playground |
| `/setup` | `GET` | HTML / Web | GitHub Copilot Device Code Setup |
| `/v1/messages` | `POST` | Anthropic Messages | Claude Code & Anthropic SDK entrypoint |
| `/v1/chat/completions` | `POST` | OpenAI Chat Completions | OpenAI SDK, Cursor, OpenWebUI entrypoint |
| `/v1beta/models/{model}:generateContent` | `POST` | Google Gemini REST | Google GenAI unary completions |
| `/v1beta/models/{model}:streamGenerateContent` | `POST` | Google Gemini REST (SSE) | Google GenAI streaming completions |
| `/v1/models` | `GET` | OpenAI or Anthropic format | Auto-detects client format or responds with OpenAI models |
| `/v1beta/models` | `GET` | Google Gemini format | Lists all models in Google Gemini format |
| `/api/status` | `GET` | JSON | Health check, active providers & routes |
| `/api/models` | `GET` | JSON | Catalog of all active models and aliases |
| `/api/sessions` | `GET`, `DELETE` | JSON | Active client sessions & token usage telemetry |
| `/api/auth/copilot/device-code` | `POST` | JSON | Start GitHub Copilot RFC 8628 OAuth |
| `/api/auth/copilot/poll` | `POST` | JSON | Poll GitHub Copilot OAuth status |

---

## Development & Testing

```bash
# Build the binary
make build

# Run all unit and integration tests
make test

# Run with example configuration
make run

# Clean build artifacts
make clean
```

### Project Structure

```
babelgate/
├── cmd/
│   └── router/         # Application entry point & CLI flags
├── pkg/
│   ├── canonical/      # Normalized data structures & streaming event bus
│   ├── config/         # YAML config parsing & environment variable expansion
│   ├── providers/      # Upstream drivers (Anthropic, Copilot, Google, OpenAI)
│   ├── router/         # Model catalog, alias routing & fallback engine
│   ├── server/         # Inbound HTTP protocol handlers & SSE multiplexers
│   └── session/        # Live session tracking, client fingerprinting & metrics
├── Makefile            # Build and test shortcuts
├── config.example.yaml # Annotated configuration template
└── README.md
```

---

## License

This project is licensed under the [MIT License](LICENSE).
