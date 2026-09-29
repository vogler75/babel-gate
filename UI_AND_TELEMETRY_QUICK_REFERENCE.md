# BabelGate UI & Telemetry: Quick Reference

## Data Collection → Display Flow

```
┌─────────────────────────────────────────────────────────────────────┐
│                    REQUEST ARRIVES                                  │
│              (POST /v1/messages, /chat/completions)                │
└──────────────────────────┬──────────────────────────────────────────┘
                           │
                           ▼
        ┌──────────────────────────────────────────┐
        │  1. CREATE & ATTACH TRACE                │
        │  pkg/server/trace/trace.go               │
        │  - RequestTrace.New()                    │
        │  - Extract session ID, client IP        │
        │  └─ Store in context                     │
        └──────────────────────────┬───────────────┘
                                   │
                                   ▼
        ┌──────────────────────────────────────────┐
        │  2. RESOLVE SESSION                      │
        │  pkg/session/manager.go                  │
        │  - GetOrCreateWithProtocol()             │
        │  └─ Create Session if new                │
        └──────────────────────────┬───────────────┘
                                   │
                                   ▼
        ┌──────────────────────────────────────────┐
        │  3. SMART ROUTING (if enabled)           │
        │  pkg/smart/router.go                     │
        │  - Classify by complexity                │
        │  - Sticky tier for same turn             │
        │  └─ Add decision note to trace           │
        └──────────────────────────┬───────────────┘
                                   │
                                   ▼
        ┌──────────────────────────────────────────┐
        │  4. DISPATCH TO PROVIDER                 │
        │  - SetRoute() on RequestTrace             │
        │  - Update provider/model/destination     │
        └──────────────────────────┬───────────────┘
                                   │
                                   ▼
        ┌──────────────────────────────────────────┐
        │  5. STREAM RESPONSE                      │
        │  - MarkFirstToken() → TTFT               │
        │  - Accumulate output tokens              │
        │  - MarkStreamDone() → StreamDuration     │
        └──────────────────────────┬───────────────┘
                                   │
                                   ▼
        ┌──────────────────────────────────────────┐
        │  6. RECORD TO SESSION                    │
        │  pkg/session/manager.go                  │
        │  - Build RequestRecord                   │
        │  - Update Session aggregates             │
        │  - Notify MetricsRecorder (SQLite)       │
        └──────────────────────────┬───────────────┘
                                   │
        ┌──────────────┬───────────┴─────────┬──────────────┐
        │              │                     │              │
        ▼              ▼                     ▼              ▼
   ┌────────────┐ ┌──────────────┐ ┌───────────────┐ ┌──────────┐
   │ TUI Loop   │ │ Web API /    │ │ Metrics SQLite│ │ Log File │
   │ (200ms)    │ │ api/sessions │ │ (hourly)      │ │ + Ring   │
   │            │ │ (poll 2-5s)  │ │               │ │ Buffer   │
   └────────────┘ └──────────────┘ └───────────────┘ └──────────┘
        │              │                     │              │
        ▼              ▼                     ▼              ▼
   ┌────────────────────────────────────────────────────────────┐
   │         USER SEES UPDATED DASHBOARD / TUI                  │
   └────────────────────────────────────────────────────────────┘
```

---

## TUI Display Structure

```
┌────────────────────────────────────────────────────────────────────┐
│ BabelGate LLM Router :8080 │ Uptime: 01:23:45                   │
├────────────────────────────────────────────────────────────────────┤
│ Status: RUNNING │ Requests: 42 │ Tokens: In: 12.5k / Out: 850   │
├─ ▶ Providers [2] 1/2 ─────────────────────────────────────────────┤
│ ▶ [Prio 1] OPENAI (openai) ● Enabled (45 models) api.openai.com   │
│   [Prio 2] ANTHROPIC (anthropic) ● Enabled (8 models) default API │
├─ Sessions [5] 1/5 ─────────────────────────────────────────────────┤
│ ▶ Claude Code  Req:42  Ctx:12.5k  Out:850  Speed:16.3 tok/s      │
│                Active:5m ago  gpt-4o  ID:sess_abc123             │
│   Web Browser  Req:8   Ctx:~2.1k  Out:142  Speed:—               │
│                Active:2m ago  claude-3-5-sonnet  ID:sess_xyz789   │
├─ Live Request Logs ─────────────────────────────────────────────────┤
│ 2025/01/15 14:23:45 POST /v1/chat/completions -> 200 gpt-4o      │
│ 2025/01/15 14:23:42 POST /v1/messages -> 200 claude-opus          │
│ 2025/01/15 14:23:38 POST /v1/messages -> 200 claude-opus          │
│   ...scrollable with h/l, j/k, PgUp/PgDn...                      │
├────────────────────────────────────────────────────────────────────┤
│ q Quit │ Tab/⇧Tab Pane │ ↑/↓ Select │ Space Toggle │ r Reload     │
└────────────────────────────────────────────────────────────────────┘

DATA SOURCES PER PANE:
┌──────────────────┬──────────────────────────────────────────────────┐
│ Pane             │ Source                                           │
├──────────────────┼──────────────────────────────────────────────────┤
│ Status Line      │ Sessions.GetSummary()                           │
│ Providers        │ Engine.GetProviderStates()                      │
│ Sessions         │ Sessions.ListSessions() → Session[]             │
│ Live Logs        │ Logger.RingBuffer.Lines() (from log output)     │
└──────────────────┴──────────────────────────────────────────────────┘
```

---

## Web Dashboard Structure

```
https://localhost:8080/

┌─────────────────────────────────────────────────────────────────────┐
│ 🗼 BabelGate [Active] Multi-Protocol Proxy │ 📖 Setup Help        │
├─────────────────────────────────────────────────────────────────────┤
│ ┌──────────────────┬──────────────────┬──────────────┐             │
│ │ Active Sessions  │ Total Requests   │ Input Tokens │             │
│ │        5         │        42        │    12.5k     │             │
│ └──────────────────┴──────────────────┴──────────────┘             │
│                                                                    │
│ ┌─────────────────────────────────────────────────────────────┐   │
│ │ Sessions                                                    │   │
│ ├─────────────────────────────────────────────────────────────┤   │
│ │ Client │ Requests │ Ctx Tokens │ Output │ Speed │ Last ... │   │
│ ├─────────────────────────────────────────────────────────────┤   │
│ │ Claude │ 42       │ 12.5k      │ 850    │ 16.3  │ 5m ago  │   │
│ │ Code   │          │            │        │ tok/s │ gpt-4o  │   │
│ ├─────────────────────────────────────────────────────────────┤   │
│ │ Web    │ 8        │ ~2.1k      │ 142    │ —     │ 2m ago  │   │
│ │ Browser│          │            │        │       │ claude  │   │
│ └─────────────────────────────────────────────────────────────┘   │
│                                                                    │
│ ┌────────────────────┐  ┌────────────────────────────────────┐   │
│ │ Providers          │  │ Routing & Analytics               │   │
│ ├────────────────────┤  ├────────────────────────────────────┤   │
│ │ [x] OpenAI         │  │ 📊 Daily Breakdown                │   │
│ │ [x] Anthropic      │  │ 📊 Hourly Breakdown               │   │
│ │ [ ] Google Gemini  │  │ 📊 Token Throughput               │   │
│ │ [x] GitHub Copilot │  │ ⚙️  Model Aliases & Fallbacks     │   │
│ └────────────────────┘  └────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────┘

REST API ENDPOINTS POLLED BY JAVASCRIPT:
┌────────────────────────────────────────────────────────────────────┐
│ Endpoint              │ Poll Rate │ Updates                        │
├───────────────────────┼───────────┼────────────────────────────────┤
│ /api/sessions         │ 2-5s      │ Session table, stat cards      │
│ /api/status           │ 5-10s     │ Provider status indicator      │
│ /api/metrics/daily    │ On demand │ Daily usage chart             │
│ /api/metrics/hourly   │ On demand │ Hourly usage chart            │
│ /api/metrics/speed    │ On demand │ Token throughput sparklines   │
│ /api/providers/:name  │ On action │ Enable/disable toggle         │
│ /api/routing          │ On demand │ Alias & fallback editor       │
└────────────────────────────────────────────────────────────────────┘
```

---

## Data Structures at Each Layer

### Level 1: During Request Processing

**RequestTrace** (transient, in context only)
```
├─ StartTime, Method, Path
├─ RequestedModel (what client asked for)
├─ Provider, Destination, TargetModel (after routing)
├─ TTFT, StreamDuration, ReadDuration (timing)
├─ InputTokens, OutputTokens (counted as stream arrives)
├─ notes[] (smart tier, fallback attempts)
└─ done (completion flag)
```

### Level 2: After Request Completes

**RequestRecord** (persisted in Session.RecentRequests[])
```
├─ ID, Timestamp, Protocol, Provider, Model
├─ DurationMs, GenerationDurationMs
├─ InputTokens, OutputTokens, CachedInputTokens, ReasoningTokens
├─ TokensPerSecond (calculated)
├─ Status, ErrorMessage
└─ [NOT INCLUDED: Smart tier decision, notes, TTFT/breakdown]
```

### Level 3: Session Aggregates

**Session** (primary UI object, in SessionManager.sessions{})
```
├─ ID, Client, LastProtocol, ClientIP
├─ RequestCount, LastModel, LastActive
├─ ContextTokens (latest request input)
├─ InputTokens, OutputTokens, TotalTokens (aggregates)
├─ TokensPerSecond (throughput)
├─ Models[], ModelStats{} (per-model breakdown)
└─ RecentRequests[] (last 50 RequestRecord objects)
```

### Level 4: Aggregate Metrics

**Summary** (across all sessions)
```
├─ TotalSessions
├─ TotalRequests
├─ TotalInputTokens, TotalOutputTokens, TotalTokens
├─ TotalCachedInputTokens, TotalReasoningTokens
└─ [Derived: Hourly metrics in SQLite]
```

---

## What Each UI Can Display (Today)

### TUI ✓ Displays
- Session count, request count, token totals (status line)
- Per-session: client, requests, context/output tokens, throughput, last active, last model
- Live request log stream with status code coloring
- Provider status, priority, model count, base URL
- Horizontal/vertical log scrolling

### Web Dashboard ✓ Displays
- Same session metrics as TUI (via /api/sessions)
- Hourly/daily aggregated metrics (via /api/metrics/*)
- Token throughput by model (via /api/metrics/speed)
- Provider enable/disable toggles
- Model aliases and fallback chain editor
- SDK configuration generator (OpenAI/Anthropic)

### ✗ NOT Displayed (Either UI)
- **Per-request timing breakdown** (TTFT, stream duration, read latency)
- **Smart routing tier decision** per request
- **Individual provider fallback attempts** per request
- **Destination URL** for each request
- **Cached input tokens** per request
- **Reasoning/thinking tokens** per request
- **Request-level errors** with full trace details
- **Model vs. Target Model** mismatches (alias resolution audit)

---

## Integration Points

### TUI → Data Sources
```
TUI.render() (200ms loop)
  ├─ Sessions.ListSessions() [returns Session[]]
  ├─ Sessions.GetSummary() [returns Summary]
  ├─ Engine.GetProviderStates() [returns ProviderState[]]
  ├─ Logger.RingBuffer.Lines() [returns []string]
  └─ Renders to ANSI terminal via bufio.WriteString()
```

### Web → REST API → Data Sources
```
JavaScript (polling)
  ├─ GET /api/sessions
  │   └─ Sessions.ListSessions() + GetSummary()
  ├─ GET /api/status
  │   └─ Engine.GetProviderStates() + GetRoutes()
  ├─ GET /api/metrics/daily
  │   └─ MetricsStore.GetDailyMetrics(start, end)
  ├─ GET /api/metrics/speed
  │   └─ MetricsStore.GetModelSpeedMetrics(...)
  └─ PUT /api/providers/:name, PUT /api/routing
      └─ Engine.SetProviderEnabled(), SetRouting()

JSON Response → DOM Rendering
  ├─ <div id="statSessions">5</div>
  ├─ <table id="sessionsTableBody">...</table>
  └─ Chart.js / SVG for analytics
```

---

## Smart Routing Data Flow

```
Request arrives with model="smart"
  │
  ▼
Engine.ApplySmart(ctx, req)
  │
  ├─ smart.Router.Decide(ctx, req)
  │   │
  │   ├─ Check sticky tier (session-local, 30min TTL)
  │   │
  │   ├─ Heuristic score (if no sticky)
  │   │   ├─ Keywords (+3/-1)
  │   │   ├─ Instruction length (+2/+1/-1)
  │   │   ├─ Code blocks (+1)
  │   │   ├─ Tools available (+1)
  │   │   ├─ Thinking requested (+1/+2)
  │   │   └─ Context size (+2/+1)
  │   │
  │   └─ Return: Decision{Tier: "complex", Targets: [...], Reason: "..."}
  │
  ├─ tr.AddNote("smart complex: heuristic score 3 (...")
  │   └─ Stored in RequestTrace.notes[]
  │
  ├─ Rewrite req.Model = targets[0]
  │
  └─ Store targets[1:] in context for fallback chain

At response time:
  │
  └─ RequestRecord built from RequestTrace
      └─ [NOTE: SmartTierDecision NOT persisted to RequestRecord]
         └─ [OPPORTUNITY: Add field, expose in web UI]
```

---

## File Map for Implementing New Features

| Feature | Files to Modify |
|---------|-----------------|
| Add per-request TTFT display | `RequestRecord` struct, `TUI.getSessionsInfo()`, web API response, dashboardHTML |
| Add smart tier to session table | `RequestRecord`, `TUI.getSessionsInfo()`, web API, dashboardHTML |
| Add request-level error details modal | Web: add modal to dashboardHTML, add detail API, expand `/api/sessions` response |
| Add throughput sparkline per session | TUI: new render method, web: add SVG sparkline to dashboardHTML |
| Add provider fallback audit trail | `RequestRecord.FallbackChain[]`, update web UI to show per-request |
| Expose request-level metrics API | New endpoint `/api/requests?sessionId=X&limit=50`, return `RequestRecord[]` |

---

## Performance Notes

- **TUI**: 200ms render loop, reads from in-memory structures only (lock-free for reads via RWMutex)
- **Web**: Polls at 2–5s intervals (configurable in JavaScript)
- **Sessions**: Last 50 requests per session kept in memory
- **Metrics**: Hourly aggregates persisted to SQLite, with 30-day retention (configurable)
- **Smart Router**: Per-session sticky tier cached in-memory with 30-minute TTL

---

## Testing Checklist for UI Changes

- [ ] TUI renders correctly with 0, 1, 10+ sessions
- [ ] TUI logs refresh in real-time (200ms intervals)
- [ ] Web /api/sessions endpoint returns valid JSON
- [ ] Web dashboard updates stat cards on each poll
- [ ] Session table rows added/removed as sessions come and go
- [ ] Panes resize correctly when terminal is resized (TUI)
- [ ] Color codes render correctly in terminal
- [ ] No goroutine leaks from polling loops
- [ ] Metrics SQLite doesn't block UI rendering
- [ ] Session cleanup doesn't interfere with active sessions (30min timeout)
