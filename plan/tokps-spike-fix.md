# Plan: Fix Unrealistic Tok/s Spikes at Minute Granularity

## Bug
Minute-granularity speed chart shows implausible spikes (e.g. 36,967 tok/s) for
`google/gemini-3.8-flash`. Confirmed via SQL:

```sql
SELECT timestamp, model, stream, duration_ms, generation_duration_ms, output_tokens, tokens_per_second, status
FROM session_requests WHERE timestamp LIKE '2026-09-15T14:23%' ORDER BY timestamp;
```
Result: `duration_ms=55013, generation_duration_ms=243, output_tokens=8983` → a 55s
request recorded only 243ms of "generation duration", producing 36,967 tok/s.

## Root Cause
`completeToolStream` (`pkg/server/inbound/tool_stream.go`) buffers tool-call
argument deltas internally and only emits `EventToolCallDone` once the full
JSON object parses. In `anthropic.go` (`handleStreaming`, ~line 603-628) and
`google.go` (`HandleStreamGenerateContent`, ~line 221+), the outer loop calls
`tr.MarkFirstToken()` only when it sees an event come out of
`completeToolStream`. For tool-call-heavy responses, that means the *first
visible* event to the handler can arrive near the very end of the stream —
after most of the real generation time has already elapsed inside the
buffering goroutine.

`RequestTrace.MarkStreamDone()` sets `StreamDuration = time.Since(firstTokenAt)`,
so if `firstTokenAt` is set 243ms before the stream actually finishes (even
though the whole request took 55s), `GenerationDuration()` returns 243ms.
`generationDurationMs()` (`pkg/server/inbound/helpers.go`) then reports that
tiny value as authoritative (it's > 0, so no fallback to total duration
kicks in), and `session/manager.go`'s `RecordRequest` computes
`TokensPerSecond = OutputTokens * 1000 / generationDurationMs` → wildly
inflated.

This only affects **Anthropic and Google (Gemini) streaming paths**, since
both route through `completeToolStream`. OpenAI's streaming handler
(`openai.go`) iterates the raw `eventsChan` directly and is not affected.

## Fix (two parts)

### 1. Measurement-side plausibility guard (primary fix)
In `pkg/session/manager.go` `RecordRequest`, after computing
`generationDurationMs` and before using it to compute `TokensPerSecond`, add a
sanity check: if the implied tok/s would exceed a realistic ceiling (e.g.
~1000-2000 tok/s — well above realistic frontier-model streaming throughput),
discard the (bogus, too-short) `generationDurationMs` and fall back to
`rec.DurationMs` (the full wall-clock request duration) instead. This keeps
using real data already available on `rec` — no new instrumentation needed.

```go
const maxPlausibleTokensPerSecond = 1000.0

generationDurationMs := rec.GenerationDurationMs
if generationDurationMs <= 0 {
    generationDurationMs = rec.DurationMs
}
if rec.OutputTokens > 0 && generationDurationMs > 0 {
    impliedTPS := float64(rec.OutputTokens) * 1000 / float64(generationDurationMs)
    if impliedTPS > maxPlausibleTokensPerSecond && rec.DurationMs > generationDurationMs {
        generationDurationMs = rec.DurationMs
    }
    rec.TokensPerSecond = float64(rec.OutputTokens) * 1000 / float64(generationDurationMs)
}
```

Also persist the corrected `generationDurationMs` onto `rec.GenerationDurationMs`
before it's saved via `sessionStore.SaveRequest(sessionID, rec)`, so the fix
applies to the stored row too (not just the in-memory `TokensPerSecond`), since
the metrics/speed chart reads `generation_duration_ms` directly from SQLite.

### 2. Aggregation-side safeguard (defense in depth, for already-stored bad rows)
In `pkg/metrics/store.go` `GetModelSpeedMetrics`, tighten the existing filter
`generation_duration_ms >= 50` — 243ms passes it but is still wrong. Add a
per-row implied-speed guard directly in SQL, e.g.:

```sql
AND (output_tokens * 1000.0 / generation_duration_ms) <= 1000
```

applied alongside the existing `status = 'success' AND output_tokens > 0 AND
generation_duration_ms >= 50` filters, in both the overall-summary query and
the per-bucket query. This prevents historical bad rows (recorded before the
manager.go fix) from continuing to skew the chart.

## Files to touch
- `pkg/session/manager.go` — `RecordRequest`, add plausibility guard, persist corrected `GenerationDurationMs` on `rec`.
- `pkg/metrics/store.go` — `GetModelSpeedMetrics`, tighten SQL filter (both queries).
- `pkg/session/manager_test.go` — new test: huge output_tokens + tiny generation_duration_ms + large duration_ms should fall back and NOT produce an outlier tok/s.
- `pkg/metrics/store_test.go` — new test: a stored row with implausible per-row tok/s is excluded from `GetModelSpeedMetrics` results.

## Verification
- `CGO_ENABLED=0 go test ./...` (cgo build blocked locally by unaccepted Xcode license).
- Manually re-check the SQL query above against `data/metrics.db` after a fresh request with tool calls to confirm `generation_duration_ms` now reflects full stream duration when the short one is implausible.
- Confirm chart no longer shows spike at minute granularity.

## Not yet done
User has not yet said "commit" for this fix — implement, test, verify, then wait for explicit commit instruction (established pattern from prior speed-chart feature).
