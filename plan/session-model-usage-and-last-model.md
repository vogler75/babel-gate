# Plan: Session Model Usage Breakdown & Last-Used Model Tracking

## 1. Overview & Problem Statement

### The Problem in TUI
When a client (such as Claude Code) switches models for a single turn (e.g., calling `anthropic/claude-3-7-sonnet` for one request before returning to `google/gemini-3.8-flash`), the TUI sessions pane permanently displays `claude-3-7-sonnet` as the session's active model. Even when all subsequent calls use Gemini and the log stream correctly shows Gemini, the TUI never updates back.

### Root Cause
1. **Deduplicated Append-Only Model Slice (`pkg/session/manager.go:301-313`):**
   `s.Models` only appends a model name if it does not already exist in the slice. Re-using an existing model does not update the slice order or move the model to the end.
2. **TUI Last Element Assumption (`pkg/tui/tui.go:636-639`):**
   The TUI assumes the most recent model is at `sess.Models[len(sess.Models)-1]`. Once a second model is added, `sess.Models[1]` remains at the tail forever, even if model 0 is used for all subsequent requests.

### The Dashboard Requirement
In the Web Dashboard (`/` and `/api/sessions`):
1. **Display Usage Percentage (%):** Show how much each model was used in the session (both by request count and token volume).
2. **Identify Last Call:** Clearly mark which model was used in the most recent request.

---

## 2. Architecture & Data Model Changes

### 2.1. `pkg/session/manager.go`

Define `ModelUsage` to capture cumulative statistics per model within a session:

```go
// ModelUsage tracks aggregate usage for a specific model within a session.
type ModelUsage struct {
    Model        string  `json:"model"`
    RequestCount int     `json:"request_count"`
    InputTokens  int     `json:"input_tokens"`
    OutputTokens int     `json:"output_tokens"`
    TotalTokens  int     `json:"total_tokens"`
    PercentReq   float64 `json:"percent_req"` // Percentage of total session requests (0-100)
    PercentTok   float64 `json:"percent_tok"` // Percentage of total session tokens (0-100)
    LastUsed     time.Time `json:"last_used"`
}
```

Update `Session` struct:
```go
type Session struct {
    ID                     string                 `json:"id"`
    Client                 string                 `json:"client"`
    ClientIP               string                 `json:"client_ip,omitempty"`
    UserAgent              string                 `json:"user_agent,omitempty"`
    CreatedAt              time.Time              `json:"created_at"`
    LastActive             time.Time              `json:"last_active"`
    LastModel              string                 `json:"last_model,omitempty"` // Model used in the most recent request
    RequestCount           int                    `json:"request_count"`
    ContextTokens          int                    `json:"context_tokens"`
    ContextTokensEstimated bool                   `json:"context_tokens_estimated,omitempty"`
    InputTokens            int                    `json:"input_tokens"`
    OutputTokens           int                    `json:"output_tokens"`
    TotalTokens            int                    `json:"total_tokens"`
    TokensPerSecond        float64                `json:"tokens_per_second"`
    GenerationDurationMs   int64                  `json:"-"`
    MeasuredOutputTokens   int                    `json:"-"`
    Models                 []string               `json:"models"`               // Unique model names in discovery order
    ModelStats             map[string]*ModelUsage `json:"model_stats,omitempty"` // Per-model aggregate usage
    RecentRequests         []RequestRecord        `json:"recent_requests,omitempty"`
}
```

#### Updating `RecordRequest`:
```go
// In RecordRequest(sessionID string, rec RequestRecord):
s.LastModel = rec.Model

if s.ModelStats == nil {
    s.ModelStats = make(map[string]*ModelUsage)
}
stat, exists := s.ModelStats[rec.Model]
if !exists {
    stat = &ModelUsage{Model: rec.Model}
    s.ModelStats[rec.Model] = stat
}
stat.RequestCount++
stat.InputTokens += rec.InputTokens
stat.OutputTokens += rec.OutputTokens
stat.TotalTokens += rec.TotalTokens
stat.LastUsed = rec.Timestamp

// Recalculate percentages across all models in this session
if s.RequestCount > 0 {
    for _, st := range s.ModelStats {
        st.PercentReq = (float64(st.RequestCount) / float64(s.RequestCount)) * 100.0
        if s.TotalTokens > 0 {
            st.PercentTok = (float64(st.TotalTokens) / float64(s.TotalTokens)) * 100.0
        }
    }
}
```

---

## 3. SQLite Persistence & Schema Migration

### 3.1. Schema Migration (`pkg/metrics/store.go`)
To support existing databases without breaking or requiring a wipe, add non-destructive column additions to the database initialization:

```sql
-- Ensure new columns exist on existing sessions tables:
ALTER TABLE sessions ADD COLUMN last_model TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN model_stats TEXT NOT NULL DEFAULT '{}';
```

*(Executed with safe inspection via `PRAGMA table_info(sessions)` or error suppression for `duplicate column name`.)*

Updated `CREATE TABLE IF NOT EXISTS sessions`:
```sql
CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    client TEXT NOT NULL,
    client_ip TEXT NOT NULL DEFAULT '',
    user_agent TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    last_active TEXT NOT NULL,
    last_model TEXT NOT NULL DEFAULT '',
    request_count INTEGER NOT NULL DEFAULT 0,
    context_tokens INTEGER NOT NULL DEFAULT 0,
    context_tokens_estimated INTEGER NOT NULL DEFAULT 0,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens INTEGER NOT NULL DEFAULT 0,
    tokens_per_second REAL NOT NULL DEFAULT 0.0,
    generation_duration_ms INTEGER NOT NULL DEFAULT 0,
    measured_output_tokens INTEGER NOT NULL DEFAULT 0,
    models TEXT NOT NULL DEFAULT '[]',
    model_stats TEXT NOT NULL DEFAULT '{}'
);
```

### 3.2. Saving & Loading (`pkg/metrics/sessions.go`)
- **`SaveSession`**: Serialize `sess.ModelStats` to JSON and save `sess.LastModel` + `model_stats`.
- **`LoadActiveSessions`**: Read `last_model` and unmarshal `model_stats` JSON into `s.ModelStats`.

---

## 4. TUI Updates (`pkg/tui/tui.go`)

### 4.1. Reading the Active Model
Replace:
```go
model := "-"
if len(sess.Models) > 0 {
    model = sess.Models[len(sess.Models)-1]
}
```
With:
```go
model := "-"
if sess.LastModel != "" {
    model = sess.LastModel
} else if len(sess.RecentRequests) > 0 && sess.RecentRequests[0].Model != "" {
    model = sess.RecentRequests[0].Model
} else if len(sess.Models) > 0 {
    model = sess.Models[len(sess.Models)-1]
}
```

This guarantees that the model column in the TUI always reflects the exact model from the most recent request.

---

## 5. Web Dashboard UI Updates (`pkg/server/web/ui.go`)

### 5.1. Session Table: "Models Used" Column
Currently, the column renders simple pills with model names:
```html
<span class="pill anthropic">anthropic/claude-3-7-sonnet</span>
```

### Enhanced Rendering:
1. **Model Badges with Percentages & Last-Used Tag:**
   For each model in `session.model_stats` (or sorted by `total_tokens` desc):
   - Calculate or read `percent_req` (and `percent_tok`).
   - If `model === session.last_model`, append an indicator badge/dot: e.g. `(last)` or `●`.
   - Tooltip displaying:
     `{req_count} requests ({percent_req}%) · {total_tokens} tokens ({percent_tok}%) · Last used {last_used}`.
   - Example pill text:
     - `gemini-3.8-flash 90% ●` (green/active glow)
     - `claude-3-7-sonnet 10%`

2. **Visual Design:**
   - Active/last model highlighted with a subtle border or dot (`●`).
   - Percentage formatted nicely (e.g. `90%` or `90.5%`).

3. **Sub-table Request History:**
   - In the expanded "Request History for Session" view, include a small model summary bar at the top showing the breakdown:
     - e.g.: `google/gemini-3.8-flash: 18 reqs (90%), 42k tok | anthropic/claude-3-7-sonnet: 2 reqs (10%), 12k tok`.

---

## 6. Implementation Steps

1. **Phase 1: Model & Manager Updates (`pkg/session/`)**
   - Add `ModelUsage` struct.
   - Extend `Session` with `LastModel` and `ModelStats`.
   - Update `RecordRequest` to track `LastModel`, update `ModelStats`, and compute percentages.
   - Add unit tests in `manager_test.go` verifying model switching:
     - Call A (Gemini) -> LastModel is Gemini, 100%.
     - Call B (Claude) -> LastModel is Claude, 50% / 50%.
     - Call C (Gemini) -> LastModel is Gemini, 66.7% / 33.3%.

2. **Phase 2: Database Persistence (`pkg/metrics/`)**
   - Add column migration in `pkg/metrics/store.go` for `last_model` and `model_stats`.
   - Update `SaveSession` and `LoadActiveSessions` in `pkg/metrics/sessions.go`.
   - Add unit tests in `pkg/metrics/store_test.go` verifying save and reload of `LastModel` and `ModelStats`.

3. **Phase 3: TUI Fix (`pkg/tui/`)**
   - Update `pkg/tui/tui.go:getSessionsInfo` to inspect `sess.LastModel`.
   - Update `pkg/tui/tui_test.go` to test model switching behavior and ensure TUI shows the latest model.

4. **Phase 4: Dashboard Web UI (`pkg/server/web/ui.go`)**
   - Update JS session table renderer in `loadSessions()`:
     - Parse `model_stats` and `last_model`.
     - Render percentage badges with rich tooltips and `(last)` indicator.
     - Add model distribution bar in the expanded session details drawer.

5. **Phase 5: Verification & End-to-End Tests**
   - Run `go test -v ./...`.
   - Verify `server_test.go` and `web/ui_test.go`.

---

## 7. Verification Checklist

- [ ] TUI accurately updates model display when switching from Gemini -> Claude -> Gemini.
- [ ] Session `LastModel` always reflects the most recent request.
- [ ] Session `ModelStats` correctly accumulates request counts, tokens, and percentage splits.
- [ ] SQLite correctly persists and restores `LastModel` and `ModelStats` across restarts.
- [ ] Dashboard displays model percentage breakdown per session.
- [ ] Dashboard visually differentiates the last-used model.
- [ ] Backward compatibility maintained for existing databases without columns.
