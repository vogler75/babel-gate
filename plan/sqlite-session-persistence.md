# Plan: SQLite Session Persistence & Restart Continuity

## 1. Executive Summary & Feasibility

### Can we write session info to the SQLite database so existing sessions can continue across BabelGate restarts?
**Yes, absolutely.** Not only is it possible, but it is architecturally clean, robust, and directly compatible with modern LLM client workflows (such as Claude Code, Cursor, OpenAI SDK, etc.).

### Is this possible from the communication point of view?
**Yes, 100% possible.**

#### Why LLM Client Communication Allows This:
1. **Stateless Wire Protocol**:
   - In all major LLM protocols (Anthropic Messages API `/v1/messages`, OpenAI Chat Completions `/v1/chat/completions`, and Google Gemini REST `:generateContent`), communication is **stateless at the transport layer**.
   - The server does **not** need to retain conversation history across turns to answer subsequent turns.
   - LLM clients (Claude Code, OpenAI SDK, etc.) maintain the conversation context locally and send the **full conversation history** (`messages: [...]`, system prompt, tool definitions, and tool results) in every single request.
2. **Idle Connections Between Turns**:
   - When a user is reading a response or typing the next prompt, there is **no open TCP connection** between the client and BabelGate.
   - If BabelGate restarts while the client is idle, the client is completely unaware of the restart. When the user submits the next prompt, the client opens a fresh HTTP request.
3. **Session Identification**:
   - Clients like Claude Code pass session identifiers on every request via headers (e.g. `x-claude-code-session-id`, `anthropic-session-id`, `x-session-id`, or query param `?session_id=...`).
   - For clients that do not pass an explicit session header (e.g. standard curl scripts), BabelGate's `GetOrCreate` groups requests by client IP + detected client within an `idleTimeout` window (default: 30 minutes).
4. **Current Behavior vs. With SQLite Persistence**:
   - **Current Behavior (In-Memory Only)**: When BabelGate restarts, `session.Manager` loses all session records. When Claude Code sends the next turn, BabelGate treats it as an unknown session and initializes a new record with `RequestCount: 1` and 0 prior tokens. While the upstream LLM call succeeds, BabelGate's web dashboard and API lose all prior turn logs, cumulative token usage, and session start time.
   - **With SQLite Persistence**: On startup, BabelGate hydrates active sessions and recent request history from SQLite. When Claude Code sends the next turn, BabelGate resolves the existing session, appends the new request, updates cumulative token counts and generation speeds, and updates the database. The dashboard and TUI display continuous session history across restarts.

#### Edge Cases & Transport Details:
- **Restarting during an active in-flight request**: If BabelGate is restarted while a response is actively streaming, the TCP connection drops. The client receives an EOF/connection reset. Standard LLM clients handle this gracefully with retry prompts. Once BabelGate finishes restarting, retrying that turn connects cleanly to the persisted session.
- **Gemini Thought Signatures (`thoughtCache`)**: Google Gemini 2.0+ models require thought signatures on function calls (`Part.ThoughtSignature`), which BabelGate caches in `pkg/providers/google/cache.go`. Even if BabelGate restarts between a Gemini tool call and tool result, BabelGate's `pkg/providers/google/transform.go` already falls back to Google's official sentinel `"skip_thought_signature_validator"`, so tool turns continue without error.

---

## 2. Architecture & Design

### Hybrid In-Memory + Write-Through SQLite Persistence
To ensure that dashboard queries, TUI redraws (which poll session data multiple times per second), and inbound routing remain sub-microsecond fast without SQLite lock contention:
1. **Startup (Hydration)**:
   - On startup, `session.Manager` loads all active sessions where `last_active > (now - sessionTTL)` from SQLite, along with their recent requests (up to `maxRequests` per session).
   - In-memory data structures (`sessions map[string]*Session`, `order []string`, global totals) are fully populated.
2. **Runtime (Write-Through)**:
   - When a new session is created in `GetOrCreate`: persisted to SQLite table `sessions`.
   - When a request finishes in `RecordRequest`: appended to `session_requests` and aggregated counters in `sessions` are updated in SQLite.
   - When a session is deleted via dashboard/API (`DeleteSession` or `Clear`): deleted from both memory and SQLite.
3. **Retention & Expiration**:
   - On startup and during background cleanup sweeps, sessions older than `sessionTTL` (default 24h) are pruned from both memory and SQLite (`DELETE FROM sessions WHERE last_active < ?`).

---

## 3. SQLite Database Schema

Add two new tables to the existing database at `cfg.Database.Path` (`data/metrics.db`):

```sql
-- Persistent sessions
CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    client TEXT NOT NULL,
    client_ip TEXT NOT NULL DEFAULT '',
    user_agent TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    last_active TEXT NOT NULL,
    request_count INTEGER NOT NULL DEFAULT 0,
    context_tokens INTEGER NOT NULL DEFAULT 0,
    context_tokens_estimated INTEGER NOT NULL DEFAULT 0,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens INTEGER NOT NULL DEFAULT 0,
    tokens_per_second REAL NOT NULL DEFAULT 0.0,
    generation_duration_ms INTEGER NOT NULL DEFAULT 0,
    measured_output_tokens INTEGER NOT NULL DEFAULT 0,
    models TEXT NOT NULL DEFAULT '[]' -- JSON array of string model names
);

CREATE INDEX IF NOT EXISTS idx_sessions_last_active ON sessions(last_active);
CREATE INDEX IF NOT EXISTS idx_sessions_client_lookup ON sessions(client_ip, client, last_active);

-- Persistent request records per session
CREATE TABLE IF NOT EXISTS session_requests (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    timestamp TEXT NOT NULL,
    provider TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL,
    stream INTEGER NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    generation_duration_ms INTEGER NOT NULL DEFAULT 0,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    input_tokens_estimated INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens INTEGER NOT NULL DEFAULT 0,
    tokens_per_second REAL NOT NULL DEFAULT 0.0,
    status TEXT NOT NULL DEFAULT 'success',
    error_message TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_session_requests_session ON session_requests(session_id, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_session_requests_timestamp ON session_requests(timestamp);
```

---

## 4. Go Interface & Decoupling

To prevent circular dependencies between `pkg/session` and `pkg/metrics`:

### In `pkg/session/manager.go`:
```go
// SessionStore defines the interface for persisting and restoring sessions across restarts.
type SessionStore interface {
    SaveSession(s *Session) error
    SaveRequest(sessionID string, rec RequestRecord) error
    LoadActiveSessions(since time.Time, maxSessions int, maxRequestsPerSession int) ([]*Session, error)
    DeleteSession(sessionID string) error
    ClearSessions() error
    PurgeOldSessions(cutoff time.Time) (int64, error)
}
```

### In `pkg/metrics/store.go` (or `pkg/metrics/sessions.go`):
Implement `SessionStore` on `*metrics.Store`. Since `metrics.Store` already manages the SQLite connection handle, WAL configuration, and retention logic, it naturally satisfies both `MetricsRecorder` and `SessionStore`.

### In `pkg/server/server.go`:
```go
metricsStore, err := metrics.NewStore(cfg.Database.Path, cfg.Database.RetentionDays)
if err != nil {
    log.Printf("Warning: failed to initialize SQLite metrics store at %s: %v", cfg.Database.Path, err)
} else {
    sessions.SetMetricsRecorder(metricsStore)
    if err := sessions.SetSessionStore(metricsStore); err != nil {
        log.Printf("Warning: failed to restore sessions from SQLite: %v", err)
    }
}
```

---

## 5. Implementation Steps

1. **Phase 1: SQLite Storage Layer (`pkg/metrics/`)**
   - In `pkg/metrics/store.go`:
     - Update schema initialization to create `sessions` and `session_requests` tables and indexes.
     - Add `SaveSession(s *session.Session) error` with SQLite UPSERT (`INSERT INTO sessions (...) VALUES (...) ON CONFLICT(id) DO UPDATE SET ...`).
     - Add `SaveRequest(sessionID string, rec session.RequestRecord) error`.
     - Add `LoadActiveSessions(since time.Time, maxSessions, maxRequests int) ([]*session.Session, error)` to load unexpired sessions with their recent requests and parsed models JSON.
     - Add `DeleteSession(sessionID string) error`, `ClearSessions() error`, and `PurgeOldSessions(cutoff time.Time) (int64, error)`.

2. **Phase 2: Session Manager Hydration & Write-Through (`pkg/session/`)**
   - In `pkg/session/manager.go`:
     - Add `sessionStore SessionStore` field to `Manager`.
     - Implement `SetSessionStore(store SessionStore) error` which hydrates `m.sessions`, `m.order`, and initializes `m.totalReqs`, `m.totalInTok`, `m.totalOutTok` from SQLite.
     - In `GetOrCreate`: save new sessions via `m.sessionStore.SaveSession(newSess)`.
     - In `RecordRequest`: save request via `m.sessionStore.SaveRequest(sessionID, rec)` and updated session via `m.sessionStore.SaveSession(s)`.
     - In `DeleteSession`: call `m.sessionStore.DeleteSession(sessionID)`.
     - In `Clear`: call `m.sessionStore.ClearSessions()`.
     - In `cleanupOldSessionsLocked`: call `m.sessionStore.PurgeOldSessions(cutoff)`.

3. **Phase 3: Server Wiring (`pkg/server/server.go`)**
   - Connect `metricsStore` to `sessions.SetSessionStore(metricsStore)` in `NewServer`.

4. **Phase 4: Tests & Verification**
   - Add unit tests in `pkg/metrics/store_test.go` verifying SQL upsert, request insertion, retrieval, JSON unmarshaling, and cascade deletion.
   - Add unit tests in `pkg/session/manager_test.go` simulating a restart:
     1. Create Store and Manager #1; record requests.
     2. Create Manager #2 attached to the same Store.
     3. Verify all sessions, token counts, request records, and models are restored.
     4. Record a new request and verify counters increment seamlessly from previous values.
   - Verify `make test` passes.
