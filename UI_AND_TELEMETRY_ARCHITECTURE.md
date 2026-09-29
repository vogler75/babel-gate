# BabelGate UI & Telemetry Architecture Analysis

## Overview

BabelGate has two complementary user interfaces for monitoring and controlling the LLM gateway:

1. **TUI (Terminal User Interface)** — ANSI-based, runs in the foreground/background with live streaming logs
2. **Web Dashboard** — HTTP-served HTML/CSS/JS with REST API endpoints for metrics and session management

Both interfaces share a common **data collection layer** comprising request tracing, session management, and metrics recording. This document maps the flow of data from collection through display.

---

## 1. Data Collection Architecture

### 1.1 Request Trace (`pkg/server/trace/trace.go`)

**Purpose:** Records fine-grained latency and routing details for each HTTP request.

**Key Fields:**
```go
type RequestTrace struct {
    StartTime        time.Time      // Request start
    Method, Path     string         // HTTP method & path
    
    // Routing decisions
    RequestedModel   string         // Client-requested model (may be "smart")
    Provider         string         // Selected provider name
    Destination      string         // Upstream URL
    TargetModel      string         // Model actually sent to provider
    
    // Latency stages
    ReadDuration     time.Duration  // Time reading request body from client
    TTFT             time.Duration  // Time-to-first-token (latency before streaming starts)
    StreamDuration   time.Duration  // Elapsed time during token streaming
    UpstreamDuration time.Duration  // Total time for non-streaming calls
    
    // Token usage
    InputTokens      int            // Input tokens consumed
    OutputTokens     int            // Output tokens generated
    IsStream         bool           // Whether response was streaming
    
    // Metadata
    notes            []string       // Routing notes (e.g., smart tier, fallbacks)
    done             bool           // Whether request completed
}
```

**Lifecycle:**
- Created at request start: `trace.New(method, path)` in `pkg/server/server.go`
- Attached to context: `trace.WithTrace(ctx, tr)`
- Updated during routing: `tr.SetRoute(reqModel, provider, destination, targetModel)`
- Updated as response streams: `tr.MarkFirstToken()`, `tr.MarkStreamDone()`
- Retrieved after routing: `trace.FromContext(ctx)`

**Key Methods:**
- `ProgressInfo()` — Returns current in-flight stats for display (model, destination, has first token, elapsed time, output tokens)
- `FormatRoute()` — Human-readable route summary: `[claude-3-7-sonnet via copilot -> https://api.githubcopilot.com: 12.5k in / 850 out]`
- `FormatBreakdown()` — Timing breakdown: `(read: 15ms, ttft: 4.20s, stream: 52.10s [16.3 tok/s])`

---

### 1.2 Session Management (`pkg/session/manager.go`)

**Purpose:** Tracks client sessions, aggregates token usage, and maintains request history per session.

**Key Data Structures:**

```go
// Individual request record
type RequestRecord struct {
    ID                   string
    Timestamp            time.Time
    Protocol             string    // "anthropic", "openai", "google"
    Provider             string    // Provider that handled it
    Model                string    // Model used (with provider prefix)
    Stream               bool
    
    // Timing
    DurationMs           int64     // Total request duration
    GenerationDurationMs int64     // Time spent generating tokens
    
    // Token counts
    InputTokens          int
    OutputTokens         int
    CachedInputTokens    int       // Cached prompt tokens (if supported)
    ReasoningTokens      int       // Reasoning/thinking tokens (if supported)
    TotalTokens          int
    
    // Derived metrics
    TokensPerSecond      float64   // Output tokens / generation duration
    Status               string    // "success" or "error"
    ErrorMessage         string
}

// Session aggregate
type Session struct {
    ID                   string
    Client               string         // e.g. "Claude Code", "Web Playground"
    LastProtocol         string         // Last protocol used
    ClientIP             string
    UserAgent            string
    CreatedAt            time.Time
    LastActive           time.Time
    
    LastModel            string
    RequestCount         int
    
    // Current request's context token count
    ContextTokens        int
    ContextTokensEstimated bool
    
    // Aggregate counts
    InputTokens          int
    OutputTokens         int
    CachedInputTokens    int
    ReasoningTokens      int
    TotalTokens          int
    
    TokensPerSecond      float64   // Throughput rate
    
    Models               []string              // Models used in this session
    ModelStats           map[string]*ModelUsage // Per-model breakdown
    RecentRequests       []RequestRecord       // Last N requests (default 50)
}

// Summary across all sessions
type Summary struct {
    TotalSessions          int
    TotalRequests          int
    TotalInputTokens       int
    TotalOutputTokens      int
    TotalCachedInputTokens int
    TotalReasoningTokens   int
    TotalTokens            int
}
```

**Session Resolution:**
1. Extract session ID from headers: `x-session-id`, `anthropic-session-id`, `x-claude-code-session-id`, `?session_id`
2. Resolve client application: Detect from `x-client` header or User-Agent
3. Get or create session in `Manager.GetOrCreateWithProtocol()`

**Recording Requests:**
- Called from inbound handlers after response completes
- Tokenizes request record, updates session aggregates
- Forwards to `MetricsRecorder` (SQLite) for persistence

---

### 1.3 Smart Routing Decision (`pkg/router/smart.go`, `pkg/smart/router.go`)

**Purpose:** Classifies requests by computational complexity and routes to appropriate model tier.

**Decision Structure:**
```go
type Decision struct {
    Tier    Tier      // simple | medium | complex | reasoning
    Targets []string  // Ordered ["provider/model", ...] candidates for this tier
    Reason  string    // Explanation (e.g., "heuristic score 5 (...)")
    Sticky  bool      // Whether tier was reused from session's current turn
}
```

**Heuristic Scoring:**
- Keywords (reasoning, complex, simple) — weighted +3, +2, -1
- Instruction length (long >4000, medium >800, short <120)
- Code blocks (≥2 blocks → +1)
- Tools available → +1
- Thinking requested (high budget → +2)
- Context size (very large >400k → +2, large >120k → +1)

**Sticky Routing:**
- Once a tier is decided for a session, reuse it for tool-result follow-ups (30-minute window)
- Prevents model switching mid-agentic-loop

**Integration:**
- Called via `Engine.ApplySmart()` before dispatching request to provider
- Result stored in context as note in `RequestTrace`: `tr.AddNote(fmt.Sprintf("smart %s: %s", decision.Tier, decision.Reason))`
- Fallback chain stored in context for retries if first target fails

---

## 2. TUI Display Layer (`pkg/tui/tui.go`)

### 2.1 Architecture

**ANSI Terminal Renderer:**
- Operates in raw mode with alternate screen buffer
- 5 frames/sec render loop (200ms ticker)
- Three panes: Providers, Sessions, Live Request Logs
- CJK-aware text width handling for proper alignment

**Panes:**

| Pane | Content | Source | Interactivity |
|------|---------|--------|---|
| **Providers** | Upstream provider list with status, priority, model count, base URL | `engine.GetProviderStates()` | Space to toggle enabled/disabled, PgUp/PgDn to scroll |
| **Sessions** | Client sessions with request count, token usage, throughput, last active time, last model | `sessions.ListSessions()` + `sessions.GetSummary()` | Arrow keys to navigate |
| **Live Request Logs** | Real-time log stream with color highlighting | `logger.RingBuffer.Lines()` | Horizontal scroll (h/l/0), vertical scroll (PgUp/PgDn), c to clear |

### 2.2 Session Info Display

**Code:** `TUI.getSessionsInfo(limit int) []string` (line 616–668)

**Per-Session Rendered Line:**
```
 ▶ Claude Code   Req:42   Ctx:12.5k  Out:850   Speed:✓ 16.3 tok/s  Active:5m ago  claude-3-5-sonnet  ID:sess_abc123
```

**Fields Extracted:**
- `sess.Client` — Client application name (truncated to 18 chars)
- `sess.RequestCount` — Total requests in session
- `sess.ContextTokens` — Input tokens in latest request (prefixed with `~` if estimated)
- `sess.OutputTokens` — Total output tokens
- `sess.TokensPerSecond` — Throughput rate (formatted as "—" if 0)
- `age := time.Since(sess.LastActive)` — Time since last activity (formatted as "5m ago", "1h ago", etc.)
- `sess.LastModel` — Model used in last request (or first recent model if unavailable)

**Interaction:**
- Selection indicator (`▶`) shows which session is highlighted
- Up/Down arrows navigate within session list
- Request count, token usage update in real-time as new requests arrive

### 2.3 Live Log Colorization

**Code:** `colorizeLogLine(line string) -> string` (line 783–809)

**Highlighting Rules:**
- Status codes: 2xx → green, 4xx → yellow, 5xx → red
- Timestamp prefix (YYYY/MM/DD HH:MM) → dim gray

**Horizontal Scrolling:**
- `h` / left arrow → scroll right by 8 columns
- `l` / right arrow → scroll left
- `0` → jump to column 0
- Respects visual width for CJK characters and ANSI sequences

### 2.4 Provider Info Display

**Code:** `TUI.getSortedProvidersInfo() []string` (line 564–614)

**Per-Provider Rendered Line:**
```
 ▶ [Prio 1] OPENAI (openai) ● Enabled (45 models) https://api.openai.com
```

**Fields:**
- `state.Priority` — Provider priority (lower number = tried first)
- `state.Name` — Provider name in bold cyan
- `state.Type` — Provider type (e.g., "openai", "anthropic")
- `state.Enabled` — Status (● Enabled in green, ○ Disabled in dim gray)
- Model count from `engine.GetProviderModels(state.Name)`
- BaseURL from config

**Interaction:**
- Space bar toggles enabled/disabled status
- Status persisted to config via `engine.SetProviderEnabled()`

### 2.5 Header Status Line

**Code:** `TUI.render()` (line 376–532)

**Displayed Metrics:**
```
Status: RUNNING │ Requests: 42 │ Tokens: In: 12.5k / Out: 850 / Total: 13.35k
```

**Source:**
```go
sum := t.srv.Sessions().GetSummary()
reqCount = sum.TotalRequests
inTokens = sum.TotalInputTokens
outTokens = sum.TotalOutputTokens
```

Updated every 200ms from live summary.

---

## 3. Web Dashboard (`pkg/server/web/ui.go`)

### 3.1 Dashboard Handler Architecture

**Handler:** `DashboardHandler` struct wires together:
- `router.Engine` — Provider/model catalog
- `session.Manager` — Session state
- `metrics.Store` — SQLite hourly metrics
- `router.Catalog` — Model discovery

**REST API Endpoints:**

| Endpoint | Method | Response | Source |
|----------|--------|----------|--------|
| `/` | GET | dashboardHTML (2976-line Go string constant) | Embedded HTML/CSS/JS |
| `/api/status` | GET | Provider states & routes | `engine.GetProviderStates()`, `engine.GetRoutes()` |
| `/api/providers/:name` | PUT | Enable/disable toggle | `engine.SetProviderEnabled()` |
| `/api/routing` | GET/PUT/POST | Routing config | `engine.GetRouting()`, `engine.SetRouting()` |
| `/api/models` | GET | All available models | `catalog.ListAll()` |
| `/api/sessions` | GET/DELETE | Session list & summary | `sessions.ListSessions()`, `sessions.GetSummary()` |
| `/api/metrics/summary` | GET | Aggregated usage (date range) | `metrics.GetSummary()` |
| `/api/metrics/daily` | GET | Per-day breakdown | `metrics.GetDailyMetrics()` |
| `/api/metrics/hourly` | GET | Per-hour breakdown | `metrics.GetHourlyMetrics()` |
| `/api/metrics/speed` | GET | Token throughput by model | `metrics.GetModelSpeedMetrics()` |

### 3.2 Dashboard UI Features

**Top Stat Cards:**
- Active Sessions
- Total Requests
- Input Tokens
- Output Tokens

**Session Table:**
- Live-updating list of all sessions
- Per-session row: client name, request count, context/output tokens, throughput, last active, model used, session ID
- Delete individual sessions or clear all
- Sortable/filterable (client-side JavaScript)

**Provider Management Card:**
- Toggles to enable/disable providers
- Priority level (sortable)
- Model count per provider
- Custom base URL editing
- Reload routes button

**Routing Configuration:**
- View/edit model aliases and fallback chains
- Save changes back to YAML config
- OpenAI/Anthropic SDK config generator (for external clients)

**Analytics Tabs:**
- Daily/hourly usage breakdown (requests, tokens)
- Token throughput (tok/s) by model
- Stacked bar charts (provider breakdown)
- Date range picker, provider filter

### 3.3 Session Table Rendering (JavaScript)

**HTML structure (approximate):**
```html
<table>
  <thead>
    <tr>
      <th>Client</th>
      <th>Requests</th>
      <th>Context Tokens</th>
      <th>Output Tokens</th>
      <th>Speed</th>
      <th>Last Active</th>
      <th>Model</th>
      <th>ID</th>
      <th>Actions</th>
    </tr>
  </thead>
  <tbody id="sessionsTableBody">
    <!-- Rows populated from /api/sessions -->
  </tbody>
</table>
```

**Real-time Update Mechanism:**
- Polls `/api/sessions` every 2–5 seconds (configurable in JavaScript)
- On each poll, re-renders `<tbody>` with latest session data
- Updates header stat cards from response `summary` field

---

## 4. Data Flow Diagram

### Request Lifecycle with Tracing & Telemetry

```
┌─────────────────────────────────────────────────────────────────────────────┐
│ 1. CLIENT REQUEST ARRIVES                                                   │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  HTTP Request (e.g., POST /v1/messages)                                      │
│    ↓                                                                         │
│  pkg/server/server.go → loggingMiddleware()                                 │
│    ├─ Create RequestTrace: trace.New(method, path)                          │
│    ├─ AttachToContext: context.WithValue(..., requestTrace)                 │
│    └─ Extract Session ID + Client IP + User-Agent                           │
│       ↓                                                                      │
│       inbound/helpers.go → ResolveSession()                                 │
│         ├─ SessionStore.GetOrCreateWithProtocol(sessionID, clientIP, ...)  │
│         ├─ Detect client from headers/user-agent                            │
│         └─ Return session.Session (empty or existing)                       │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────────┐
│ 2. REQUEST ROUTING (CANONICAL TRANSFORMATION)                               │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  inbound/{protocol}.go → handleMessages() / handleCompletions() / etc.      │
│    ├─ Parse request into canonical.CanonicalRequest                        │
│    ├─ Extract requested model name                                          │
│    └─ Check if model == "smart"                                             │
│       ↓                                                                      │
│       Engine.ApplySmart(ctx, req)                                           │
│         ├─ Check if smart routing configured                                │
│         ├─ smart.Router.Decide(ctx, req) → Decision{Tier, Targets, Reason} │
│         │  └─ Heuristic score request (keywords, instruction len, context)  │
│         │  └─ Check sticky tier (same session, within 30min)                │
│         ├─ Filter targets for usable providers (not on cooldown)            │
│         ├─ Rewrite req.Model = targets[0]                                   │
│         ├─ Store remaining targets in context (fallback chain)              │
│         ├─ Add note to RequestTrace:                                        │
│         │  tr.AddNote(fmt.Sprintf("smart %s: %s", decision.Tier, reason))  │
│         └─ Return Decision (for later display)                              │
│                                                                              │
│  Engine.Execute() or Engine.Stream()                                        │
│    ├─ ResolveModel(req.Model) → (provider, targetModel, error)             │
│    ├─ Update RequestTrace:                                                  │
│    │  tr.SetRoute(req.Model, provider.Name(), endpoint, targetModel)       │
│    └─ Dispatch to provider                                                  │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────────┐
│ 3. STREAMING / RESPONSE TRACKING                                            │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  provider.Stream(ctx, req) → (reader of CanonicalEvent, error)              │
│    └─ Start upstream call (mark time in RequestTrace)                       │
│                                                                              │
│  inbound/{protocol}.go → handleStream() loop                                │
│    ├─ Iterate over canonical.Event from provider                           │
│    ├─ For first event:                                                      │
│    │  └─ tr.MarkFirstToken() [updates TTFT = now - StartTime]              │
│    ├─ Accumulate output tokens via StreamUsageTracker                       │
│    ├─ Re-serialize to client protocol (SSE events)                          │
│    └─ On stream EOF:                                                        │
│       └─ tr.MarkStreamDone() [updates StreamDuration]                       │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────────┐
│ 4. SESSION & METRICS RECORDING                                              │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  After response sent to client:                                              │
│                                                                              │
│  inbound/handlers → recordRequest(session, trace)                           │
│    ├─ Extract final values from RequestTrace:                              │
│    │  ├─ tr.TargetModel, tr.Provider                                       │
│    │  ├─ tr.InputTokens, tr.OutputTokens                                   │
│    │  ├─ tr.TTFT, tr.StreamDuration                                        │
│    │  └─ tr.IsStream                                                        │
│    │                                                                        │
│    ├─ Build RequestRecord:                                                  │
│    │  {ID, Timestamp, Protocol, Provider, Model, Tokens, Duration, Status} │
│    │                                                                        │
│    └─ session.Manager.RecordRequest(sessionID, record)                      │
│       ├─ Update session aggregates:                                         │
│       │  ├─ RequestCount++                                                  │
│       │  ├─ ContextTokens = latestRecord.InputTokens                        │
│       │  ├─ OutputTokens += record.OutputTokens                             │
│       │  ├─ TotalTokens += record.TotalTokens                               │
│       │  ├─ LastModel = record.Model                                        │
│       │  ├─ Add model to Models[]                                           │
│       │  └─ Update ModelStats[model]                                        │
│       │                                                                     │
│       ├─ Append record to RecentRequests (keep last 50)                     │
│       │                                                                     │
│       ├─ Notify MetricsRecorder (if set):                                   │
│       │  └─ metricsRecorder.Record(time, provider, model, in, out, isErr)   │
│       │     └─ INSERT INTO hourly_metrics (hour, provider, model, ...)      │
│       │                                                                     │
│       └─ Session now visible in:                                            │
│          ├─ TUI: next render cycle polls Sessions().ListSessions()          │
│          └─ Web: next /api/sessions poll returns updated session            │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────────┐
│ 5. DISPLAY LAYER UPDATES                                                    │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                              │
│  TUI (200ms render loop):                                                    │
│    ├─ Poll Sessions().ListSessions() & GetSummary()                         │
│    ├─ Poll logger.RingBuffer.Lines()                                        │
│    ├─ Poll engine.GetProviderStates()                                       │
│    ├─ Render panes with latest data                                         │
│    └─ Write ANSI buffer to stdout                                           │
│                                                                              │
│  Web Dashboard (JavaScript polling):                                         │
│    ├─ Poll /api/sessions (every 2–5 sec)                                    │
│    ├─ Poll /api/status (less frequently)                                    │
│    ├─ Poll /api/metrics/* (on user request)                                 │
│    ├─ Re-render HTML table rows with latest data                            │
│    └─ Update stat card values                                               │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## 5. Key Integration Points

### 5.1 RequestTrace → TUI Live Log

**Path:**
```
RequestTrace fields
  ↓
logger.RingBuffer (from loggingMiddleware)
  ↓
TUI.render() → computeVisibleLogs() → colorizeLogLine()
  ↓
ANSI terminal
```

**Information Displayed:**
- Log line from `logger` (contains timestamps, status codes, endpoints)
- Color highlighting applied for readability

**Not Yet Displayed in TUI:**
- Smart routing tier decision
- TTFT / token throughput metrics
- Provider/target model details

---

### 5.2 RequestTrace → Web Dashboard Session Table

**Path:**
```
RequestTrace + Session
  ↓
SessionManager.RecordRequest(rec RequestRecord)
  ↓
Session.RecentRequests[] + Session aggregate fields
  ↓
/api/sessions endpoint returns JSON
  ↓
JavaScript re-renders session table rows
  ↓
HTML table updated
```

**Information Displayed:**
- Session ID, client name, protocol
- Request count, context/output token totals
- Throughput (tokens/sec)
- Last active time
- Last model used

**Not Yet Displayed in Web:**
- Individual request trace details (TTFT, stream duration)
- Smart tier decision per request
- Request-level timing breakdown

---

### 5.3 Smart Router Decision → Trace Notes

**Path:**
```
smart.Router.Decide()
  ↓
Engine.ApplySmart() → tr.AddNote(...)
  ↓
RequestTrace.notes[] (internal field)
  ↓
trace.FormatBreakdown() includes notes
  ↓
Currently logged to file/TUI logs, not exposed to UIs
```

**Opportunity:**
- `RequestRecord` could store `SmartTierDecision` field with tier/reason
- Could be displayed in per-request details view in web dashboard
- Could be shown in TUI with keystroke to expand request details

---

## 6. Current Limitations & Display Gaps

### What's Displayed

| Metric | TUI | Web | Level |
|--------|-----|-----|-------|
| Session count | ✓ (header) | ✓ (stat card) | Aggregate |
| Total requests | ✓ (header) | ✓ (stat card) | Aggregate |
| Token counts (in/out/total) | ✓ (header) | ✓ (stat cards) | Aggregate |
| Per-session request count | ✓ | ✓ | Session |
| Per-session token usage | ✓ | ✓ | Session |
| Per-session throughput (tok/s) | ✓ | ✓ | Session |
| Last active time | ✓ | ✓ | Session |
| Last model used | ✓ | ✓ | Session |
| Client application | ✓ | ✓ | Session |
| Provider enabled/disabled status | ✓ | ✓ | Provider |
| Provider models count | ✓ | — | Provider |
| Live request logs | ✓ (TUI logs pane) | ✓ (file sink, web logs not yet exposed) | Request |
| Hourly/daily metrics | — | ✓ (analytics tabs) | Aggregate |
| Token throughput chart | — | ✓ (speed metrics) | Time-series |

### What's NOT Displayed

| Metric | Why Important | Where Available |
|--------|---------------|-----------------|
| **Per-request timing breakdown** (TTFT, stream duration, read duration) | Diagnose bottlenecks | `RequestTrace.FormatBreakdown()` (logged, not exposed) |
| **Smart routing tier decision** per request | Understand routing logic | `RequestTrace.notes[]` (logged, not exposed) |
| **Per-request error details** | Troubleshoot failures | `RequestTrace` fields, not persisted to session |
| **Individual request records** (not aggregated) | Audit trail | Session stores last 50 in `RecentRequests[]` but not exposed in web UI |
| **Provider fallback chain** used per request | Debugging retries | `RequestTrace.notes[]` fallback attempts, not exposed |
| **Request-level cached input tokens** | Optimize cache usage | `RequestRecord.CachedInputTokens` available but not displayed |
| **Request-level reasoning tokens** | Monitor thinking usage | `RequestRecord.ReasoningTokens` available but not displayed |
| **Destination URL per request** | Verify routing | `RequestTrace.Destination`, not persisted to session |
| **Model vs. Target Model** discrepancies | Audit alias resolution | `RequestTrace.RequestedModel` vs `TargetModel`, not exposed |

---

## 7. Data Structure Summary for UI Display

### RequestRecord (Available in Session)

```go
type RequestRecord struct {
    ID                   string          // Unique request ID
    Timestamp            time.Time       // When request started
    Protocol             string          // "anthropic", "openai", "google"
    Provider             string          // Provider name
    Model                string          // Model with provider prefix
    Stream               bool            // Was streaming?
    
    DurationMs           int64           // Total time (client to server)
    GenerationDurationMs int64           // Time generating tokens (TTFT to EOF)
    
    InputTokens          int             // Tokens in request
    OutputTokens         int             // Tokens in response
    CachedInputTokens    int             // Cached tokens (if supported)
    ReasoningTokens      int             // Thinking tokens (if supported)
    TotalTokens          int
    
    TokensPerSecond      float64         // Throughput calculation
    Status               string          // "success" or "error"
    ErrorMessage         string          // Error details
}
```

### Session (Primary Display Object)

```go
type Session struct {
    ID                string
    Client            string            // "Claude Code", "Web Playground", etc.
    LastProtocol      string
    LastModel         string
    RequestCount      int
    LastActive        time.Time
    
    ContextTokens     int               // Latest request input
    OutputTokens      int               // Aggregate output
    TotalTokens       int               // Total
    
    TokensPerSecond   float64           // Throughput
    RecentRequests    []RequestRecord   // Last 50 (detailed audit trail available)
    ModelStats        map[string]*ModelUsage
}
```

### RequestTrace (Not Persisted, Available During Request)

```go
type RequestTrace struct {
    RequestedModel   string            // What client asked for
    Provider         string            // Where routed
    TargetModel      string            // What was sent
    Destination      string            // Upstream URL
    
    TTFT             time.Duration     // First token latency
    StreamDuration   time.Duration     // Token generation time
    ReadDuration     time.Duration     // Request body read time
    UpstreamDuration time.Duration     // Non-streaming total
    
    InputTokens      int
    OutputTokens     int
    IsStream         bool
    
    notes            []string          // Routing decisions & fallbacks
}
```

---

## 8. Recommended Display Extensions

To expose smart routing decisions and per-request timing:

### For TUI
1. **Request Details Pane** (new 4th pane, switchable)
   - Show selected request from current session's `RecentRequests[]`
   - Display: model, provider, timing breakdown, status, error (if any)

2. **Inline Smart Tier Display**
   - Add column to session table showing last tier decision
   - Expand notes to show tier reason

3. **Request Timing Sparkline**
   - Visual TTFT/stream duration for recent requests per session
   - Helps spot performance degradation

### For Web Dashboard
1. **Request Details Modal**
   - Click on session row → expand recent requests table
   - Each row: timestamp, model, duration, tokens, status, smart tier (if available)

2. **Request-Level Analytics**
   - New tab: per-request audit trail with full trace breakdown
   - Filter by model, provider, date range
   - Export CSV

3. **Smart Routing Analytics**
   - Show tier distribution over time
   - Confidence scores per tier
   - Effectiveness (TTFT/throughput by tier)

---

## 9. Key Files Reference

| File | Lines | Purpose |
|------|-------|---------|
| `pkg/server/trace/trace.go` | 333 | RequestTrace definition & formatting |
| `pkg/session/manager.go` | 643 | Session store & aggregation |
| `pkg/router/smart.go` | 117 | Smart routing integration |
| `pkg/smart/router.go` | 181 | Smart tier decision logic |
| `pkg/smart/heuristic.go` | 173 | Request classification scoring |
| `pkg/tui/tui.go` | 1000 | TUI renderer & interaction |
| `pkg/server/web/ui.go` | 2976 | Web dashboard & API handlers |
| `pkg/server/inbound/helpers.go` | 250+ | Session resolution & request recording |
| `pkg/logger/` | — | RingBuffer for log streaming |
| `pkg/metrics/store.go` | — | SQLite hourly metrics (not analyzed) |

---

## 10. Integration Checklist for New Features

To add a new metric or timing to both UIs:

- [ ] Add field to `RequestTrace` struct (if per-request timing)
- [ ] Add field to `RequestRecord` struct (if needs persistence)
- [ ] Update `Session` aggregate logic in `RecordRequest()` (if aggregatable)
- [ ] Update TUI render logic in `pkg/tui/tui.go`
- [ ] Update web API endpoint (e.g., `/api/sessions`)
- [ ] Update JavaScript table rendering in dashboardHTML
- [ ] Add any formatting helper (e.g., `FormatDuration()`)
- [ ] Test with both TUI and web under live traffic

---

## Summary

BabelGate's telemetry architecture cleanly separates **collection** (request traces, sessions) from **display** (TUI, web), with a common session/metrics layer. This enables adding new metrics to either UI independently. The smart routing decision data is captured in `RequestTrace.notes[]` but not yet exposed; exposing it would require adding a field to `RequestRecord` and updating both UI layers.
