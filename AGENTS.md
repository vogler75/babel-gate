# AGENTS.md

This file provides guidance to coding agents (Claude Code, and others) when working with code in this repository.

## Project

**BabelGate** (`github.com/vogler75/babel-gate`) — an any-to-any LLM gateway in Go. It accepts requests in the Anthropic Messages, OpenAI Chat Completions, or Google Gemini REST protocols, normalizes them into a canonical representation, and dispatches to any configured upstream provider (Anthropic, OpenAI, Google, GitHub Copilot, or any OpenAI-compatible endpoint), translating streaming SSE, tool calls, and usage telemetry in both directions.

Standard library only (plus `yaml.v3`, `golang.org/x/term`, `modernc.org/sqlite`) — keep it that way; "zero external dependencies" is a headline feature.

## Commands

```bash
make build                    # -> bin/babelgate (go build ./cmd/router)
make test                     # go test -v ./...
make run                      # build + run with config.example.yaml
./build.sh -t -r -c           # build with tests / release (-s -w, trimpath) / clean

go test ./pkg/router/...                          # single package
go test ./pkg/providers/openai -run TestStream     # single test
go build -o bin\babelgate.exe .\cmd\router         # Windows
```

Config resolution: `-config <path>`, else `config.yaml` in CWD if present, else pure env vars (`GEMINI_API_KEY`/`GOOGLE_API_KEY`, `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GITHUB_TOKEN`/`COPILOT_API_KEY`) — but env-var auto-population only happens when *no* providers are declared in the config file. A `.env` file in the root is auto-loaded. API keys in YAML support `${VAR}` / `$VAR` expansion.

Other CLI flags (`cmd/router/main.go`): `-port`, `-copilot-login` (RFC 8628 device flow, writes to `copilot.GetTokenFilePath()`), `-background`/`-d` (daemon, no TUI), `-no-tui`, `-tray` (detach into the background + Windows notification-area icon; implies `-background`), `-log-file`, `-log-max-size-mb`, `-log-max-backups`.

## Architecture

Request flow: **inbound protocol handler → canonical → routing engine → provider driver → canonical events → outbound protocol SSE**.

- **`pkg/canonical`** — the pivot. `types.go` holds `CanonicalRequest`/`CanonicalResponse`; `ContentPart` is a tagged union (`PartText | PartThinking | PartImage | PartToolCall | PartToolResult`). `events.go` holds `CanonicalEvent` with a fixed 9-event vocabulary (`message_start`, `thinking_delta`, `text_delta`, `tool_call_start/delta/done`, `message_delta`, `message_done`, `error`); note `CandidateIndex` (which response choice, for n>1) is deliberately distinct from `Index` (content/tool block index). Every translation is *protocol ↔ canonical*, never protocol ↔ protocol — adding a protocol or provider is O(1), not O(n²). Extend `CanonicalEvent` rather than special-casing a provider downstream.

- **`pkg/server`** — `server.go` wires the mux (all endpoint paths live here). `inbound/{anthropic,google,openai}.go` parse the client protocol into canonical and re-serialize the event stream back out; `inbound/tool_stream.go`'s `completeToolStream` buffers tool-call start/deltas per `{candidate,index}` and emits a single `tool_call_done` only once the args parse as a JSON object (protocols like Anthropic and Google need whole tool blocks); a truncated stream surfaces as `io.ErrUnexpectedEOF` rather than silent success. `web/ui.go` serves the dashboard and `/setup` — the HTML/CSS/JS are Go string constants (`dashboardHTML`, `setupHTML`) inside that single 2000-line file; there is **no static asset directory and no `go:embed` for any web asset**, so UI changes mean editing Go string literals. (The only `go:embed` in the tree is `pkg/tray/icon.ico`, which is not a web asset.) `trace/trace.go` is a per-request `RequestTrace` in `context` recording read duration, TTFT, destination URL, resolved provider/target model, and token counts; `loggingMiddleware` also ticks every 15s printing `[WAIT]`/`[STREAM]` progress for long requests. Auth (`server.Server.authMiddleware`, optional `server.api_key`) accepts `Authorization: Bearer`, `x-api-key`, or `x-goog-api-key`, and exempts `/`, `/setup`, and `/api/*`.

- **`pkg/router`** — `engine.go` builds providers from config and resolves models. `ResolveModel` tries, in order: alias in `routing.routes` → explicit `provider/model` prefix → priority-ordered catalog lookup (`ProviderConfig.Priority`, default 100) → family-prefix heuristic (`claude-*`→anthropic, `gemini-*`→google, `gpt-*|o1*|o3*`→openai/copilot) → sole registered provider → `routing.default`. `routing.fallbacks[model]` chains apply only when the *initial* call errors, each entry re-resolved through `ResolveModel`. Duplicate model IDs across providers are preserved and disambiguated by provider prefix. `catalog.go` merges all providers' models and formats the catalog per protocol (`FormatOpenAI` / `FormatAnthropic` / `FormatGoogle`). `SetProviderEnabled`/`SetRouting` mutate the running config *and* write back to the source YAML via node-level edits (`pkg/config/update.go`) so comments and `${VAR}` refs survive.

- **`pkg/providers`** — `provider.go` defines the sole interface a new driver must satisfy: `Name/Type/Endpoint/Execute/Stream/ListModels`. Register the new type in the `buildProvider` switch in `pkg/router/engine.go`. Each driver directory splits `client.go` (HTTP/auth), `transform.go` (↔ canonical), `types.go` (wire structs). By convention drivers also expose `NewClient(name, apiKey, baseURL, enabledModels, httpClient)` and resolve keys via a `getAPIKey(req)` that prefers the configured key, falls back to the client's `req.AuthToken`, and ignores `${...}`/`dummy`/`test` placeholders. `copilot/auth.go` also discovers existing VS Code / JetBrains credentials in `~/.config/github-copilot`. `google/cache.go` is a global LRU mapping tool-call-ID → `thoughtSignature`, needed to round-trip Gemini thought signatures through protocols with no field for them.

- **Tool-name mangling** — `pkg/providers/toolnames` builds a *request-scoped reversible* mapping of tool names to satisfy each target protocol's `Constraints` (max length, allowed runes). `Normalize` deep-copies the request and returns a `*Mapping`; already-valid names are reserved first so they can't be displaced, invalid ones become `sanitized_prefix + "_" + sha256[:8]`. The `Mapping` is stashed in an **unexported struct field** on the provider's wire request and undone on the way out by `RestoreResponse`/`RestoreStream` (`{openai,anthropic}/names.go`). Google instead uses its own `sanitizeToolID` in `google/transform.go`. Always map on the way out and reverse on the way back.

- **Anthropic→Anthropic passthrough** — `inbound/anthropic.go` `handlePassthrough` bypasses canonical entirely when the client protocol matches the upstream, preserving unknown fields and *signed thinking blocks* (which canonicalization would destroy). Since the canonical `Mapping` isn't available on that byte-proxy path, `anthropic/passthrough_names.go` does surgical JSON edits of only `name` fields (in `tools`, `tool_use`, `tool_choice`) using `json.Decoder.UseNumber()` to avoid float mangling, returning input unchanged when nothing changed. Changes to Anthropic handling usually need to be made on both the canonical path and this passthrough path. Separately, `sanitizeAnthropicPayload` works around upstream Anthropic/Vertex validation by rewriting empty `thinking` blocks to `redacted_thinking` and injecting a `" "` text block into otherwise-empty messages.

- **`pkg/session`** — in-memory session store keyed off `x-session-id`/`anthropic-session-id`/`x-claude-code-session-id`/`?session_id`, with client fingerprinting from `x-client`/User-Agent (`DetectClient` → "Claude Code", "OpenAI SDK", "Web Playground", …). Retention defaults: 30m idle, 24h TTL, 200 sessions × 50 requests. `EstimateTokens` (`len/4`) fills in whenever upstream omits usage. Forwards to a `MetricsRecorder` interface so `session` does not import `metrics`.

- **`pkg/metrics`** — SQLite (`modernc.org/sqlite`, cgo-free, WAL) at `database.path` (default `data/metrics.db`). One table, `hourly_metrics`, PK `(hour, provider, model)`, upserted per request; `retention_days` purge runs at open. `GetSummary`/`GetDailyMetrics`/`GetHourlyMetrics` back the dashboard's `/api/metrics/*`.

- **`pkg/tui`** — the default foreground UI, a hand-rolled ANSI renderer with CJK-aware width handling; keys: `q`/Ctrl-C quit, `j/k`+arrows/PgUp/PgDn scroll, `h/l`/`0` horizontal scroll, `c` clear, `r` re-measure. Disabled by `-no-tui`/`-background` or a non-TTY. `pkg/logger` provides the size-based rotating file writer plus a 1000-line `RingBuffer`, combined via `MultiWriterWithRing` so the TUI and log file share one `log.SetOutput`. `sigwinch_{posix,windows}.go` are build-tagged.

- **`pkg/tray`** — optional notification-area icon for background instances, enabled by `-tray` (which implies `-background`). Windows-only: `tray_windows.go` is pure `syscall` against `user32.dll`/`shell32.dll` (hidden top-level window + `Shell_NotifyIconW` + `TrackPopupMenu`), so `CGO_ENABLED=0` cross-builds keep working — **never** reach for a systray library or cgo here. `tray_other.go` makes `Supported()` false everywhere else and `-tray` degrades to plain background mode. Pointers handed to `LazyProc.Call` are held in named locals with `runtime.KeepAlive`, since the `unsafe.Pointer`→`uintptr` GC exemption does not cover variadic `Call` arguments. Icon selection is split into the pure `ico.go` (`pickIconEntry`) so it is testable off-Windows; `icon.ico` is generated art, not hand-drawn. The menu is deliberately just Open Dashboard + Quit.

- **`pkg/daemon`** — `Detach()` re-execs the current binary with the same args and cwd, stdio pointed at `os.DevNull`, and marks the child with the `BABELGATE_DAEMON=1` env var that `IsChild()` reads to prevent a respawn loop; `sysproc_{windows,other}.go` supply `DETACHED_PROCESS|CREATE_NEW_PROCESS_GROUP` and `Setsid` respectively. Only `-tray` uses it — **`-background` deliberately stays in the foreground** so Docker/systemd supervision keeps working. `main.go` calls it *after* config load and rotator init so those failures still reach the console, and everything after it is log-file-only.

Startup also runs a 10s provider health check and `ListModels` discovery, feeding `engine.SyncProviderModels`.

## Conventions

- Tests are colocated (`*_test.go`) and cover the tricky translation invariants — stream index/candidate correlation (`google/stream_index_test.go`, `correlation_test.go`), tool-response ordering, reasoning and response-ID handling (`openai/`), and SSE framing (`inbound/protocol_stream_test.go`). When touching a transform, extend these rather than adding end-to-end tests.
- Release CI (`.github/workflows/release.yml`) runs `go test -v ./...` and cross-builds linux/darwin/windows × amd64/arm64 on `v*` tags — avoid anything cgo-dependent.
