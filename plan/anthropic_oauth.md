# Implementation Plan: Anthropic Browser OAuth Authentication (Claude Subscription)

Enable `llm-router` to authenticate with Anthropic using a **Claude Pro / Team / Max user subscription** via a browser login flow (OAuth 2.0 PKCE) instead of requiring a pay-per-token Anthropic API key.

---

## User Review Required

> [!WARNING]
> **Anthropic Terms of Service & Policy Notice:**
> Anthropic's OAuth flow (`claude.ai/oauth/authorize` with client ID `9d1c250a-e61b-44d9-88ed-5944d1962f5e`) is officially provisioned for the **Claude Code CLI** (`@anthropic-ai/claude-code`). While community proxies successfully use this flow to leverage Claude Pro/Max subscriptions, Anthropic's consumer terms discourage unauthorized third-party tool usage. The implementation will faithfully implement the PKCE flow used by Claude Code, but you should be aware of this policy context.

> [!IMPORTANT]
> **Usage Caps on Subscriptions:**
> Unlike pay-as-you-go API keys (which are billed per million tokens with high RPM), user subscriptions are bound by interactive rolling caps (e.g., ~45 messages every 5 hours for Claude Pro, higher for Max/Team). When the subscription cap is reached, Anthropic returns rate-limit errors until the window resets.

---

## Open Questions

> [!NOTE]
> Preferred login interaction methods supported:
> 1. **Web Dashboard One-Click Login**: A "Connect Claude Subscription" button in the embedded web UI (`http://localhost:8080`) that opens your browser, listens for the OAuth callback, and saves credentials.
> 2. **CLI Login Command**: A terminal command such as `./bin/llm-router auth login anthropic` that opens the browser or prints a login link with an authorization code prompt.
> 3. **Automatic Claude Code Import**: If you already ran `claude login` with Claude Code, `llm-router` can automatically discover and import your existing credentials from your macOS environment.

---

## Proposed Architecture & Flow

```
                         BROWSER AUTHENTICATION FLOW
┌──────────────┐         1. Generate PKCE & State          ┌────────────────┐
│  LLM Router  │ ────────────────────────────────────────> │ User's Browser │
│ (Port 8080)  │         2. Open claude.ai/oauth/authorize └───────┬────────┘
└──────┬───────┘                                                   │
       │                                                    3. User logs in &
       │                                                       clicks "Approve"
       │         4. Redirect to http://localhost:8080/callback     │
       │ <─────────────────────────────────────────────────────────┘
       │
       │         5. Exchange Code + Verifier for Tokens
       ▼ ────────────────────────────────────────────────────────> ┌────────────────┐
                                                                   │ Anthropic Auth │
       │ <──────────────────────────────────────────────────────── │   claude.ai    │
       │         6. Stores access_token & refresh_token            └────────────────┘
       │            in ~/.llm-router/anthropic_credentials.json
       │
       │ 7. Upstream requests to api.anthropic.com use Bearer <access_token>
```

---

## Proposed Changes

### Component 1: OAuth & Credential Management (`pkg/auth/anthropic`)

Create a dedicated OAuth package to handle PKCE generation, browser launching, token exchange, file-based credential storage, and background token refresh.

#### [NEW] `pkg/auth/anthropic/oauth.go`
- Implements RFC 7636 PKCE (`code_verifier` and SHA-256 `code_challenge`).
- Generates authorization URL pointing to `https://claude.ai/oauth/authorize`.
- Implements token exchange against Anthropic's OAuth token endpoint (`grant_type=authorization_code`).
- Implements token refresh against the token endpoint (`grant_type=refresh_token`).

#### [NEW] `pkg/auth/anthropic/store.go`
- Manages secure persistence of credentials (`access_token`, `refresh_token`, `expires_at`, `account_email`) in `~/.llm-router/anthropic_credentials.json` with restricted file permissions (`0600`).
- Provides automatic token retrieval and lazy refresh: if `access_token` is within 5 minutes of expiration, it transparently refreshes the token using `refresh_token` before returning it.
- Includes discovery fallback to check if Claude Code credentials already exist on the machine.

---

### Component 2: Configuration & Provider Integration

#### [MODIFY] `pkg/config/config.go`
- Extend `ProviderConfig` with:
  ```yaml
  providers:
    anthropic:
      type: anthropic
      auth_mode: oauth  # "api_key" (default) or "oauth"
      credentials_file: "~/.llm-router/anthropic_credentials.json"
  ```
- Support environment variable fallback (e.g. `ANTHROPIC_AUTH_MODE=oauth`).

#### [MODIFY] `pkg/providers/anthropic/client.go`
- Support supplying dynamic OAuth tokens to Anthropic requests.
- When `auth_mode == "oauth"`, dynamically retrieve the valid token from the token store and send:
  ```http
  Authorization: Bearer <oauth_access_token>
  anthropic-version: 2023-06-01
  anthropic-beta: prompt-caching-2024-07-31,tools-2024-04-04
  ```
- If an upstream `401 Unauthorized` is returned due to token expiration, trigger immediate refresh and retry the request once.

---

### Component 3: Web Dashboard & Local Callback Handler

#### [MODIFY] `pkg/server/server.go`
- Register OAuth callback endpoints:
  - `GET /api/auth/anthropic/login`: Generates PKCE parameters, saves state, and redirects the browser to Anthropic's authorization page.
  - `GET /api/auth/anthropic/callback`: Receives the authorization code from Anthropic, performs token exchange, writes credentials to disk, and redirects back to `/` with a success message.
  - `GET /api/auth/anthropic/status`: Returns current auth status (logged in vs not logged in, account email, token expiration).
  - `POST /api/auth/anthropic/logout`: Clears saved tokens.

#### [MODIFY] `pkg/server/web/ui.go`
- Add a visual **"Claude Subscription Status"** card in the dashboard:
  - Displays connection state: `Disconnected` / `Connected as user@domain.com`.
  - "Connect Claude Account" button that initiates the browser login flow.
  - "Disconnect" button to clear stored subscription tokens.

---

### Component 4: CLI Auth Command (`cmd/router/main.go`)

#### [MODIFY] `cmd/router/main.go`
- Add command-line support for authenticating directly in the terminal:
  - `./bin/llm-router auth login anthropic`: Starts a temporary local HTTP server, opens the browser, captures the tokens, and exits with a confirmation.
  - `./bin/llm-router auth status`: Shows current OAuth subscription connection details.

---

## Verification Plan

### Automated Tests
- **PKCE & Crypto tests**: Verify SHA-256 base64url challenge and verifier compliance.
- **Store & Refresh tests**: Unit tests verifying token serialization, file permissions (`0600`), and simulated token refresh flow.
- **Client Auth Header tests**: Unit test ensuring `Authorization: Bearer <token>` is properly attached to requests when `auth_mode: oauth`.

```bash
go test -v ./pkg/auth/anthropic/...
go test -v ./pkg/providers/anthropic/...
go test -v ./pkg/config/...
```

### Manual Verification
1. **Initiate Browser Login**:
   - Start the server: `./bin/llm-router`
   - Open `http://localhost:8080` in the browser.
   - Click "Connect Claude Account".
   - Confirm Anthropic OAuth login page opens at `claude.ai`.
   - Log in, approve the connection, and confirm redirect back to dashboard showing `Connected (Claude Pro/Team)`.
2. **End-to-End Chat & Tool Calling**:
   - Send a test prompt to `/v1/messages` using `curl` without passing an API key.
   - Confirm response is streamed back successfully from Anthropic.
   - Run Claude Code or an agent client pointing to `http://localhost:8080` and verify multi-turn tool calling operates under the subscription quota.
