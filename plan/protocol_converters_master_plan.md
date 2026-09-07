# Master Plan: Protocol Converters Analysis, LiteLLM Comparison & Action Plan

## Overview & Executive Summary

This master plan synthesizes **all findings** from the in-depth architectural audit of `llm-router`'s protocol converters and provider adapters, evaluated against the battle-tested reference implementation in [**LiteLLM** (`BerriAI/litellm`)](https://github.com/BerriAI/litellm).

The goal of this document is to serve as the single authoritative reference for all known protocol quirks, schema validation pitfalls, edge cases, and required fixes across:
1. **Google Gemini Provider & Inbound** (`pkg/providers/google`, `pkg/server/inbound/google.go`)
2. **Anthropic Converter & Inbound** (`pkg/providers/anthropic`, `pkg/server/inbound/anthropic.go`)
3. **OpenAI Converter & Inbound** (`pkg/providers/openai`, `pkg/server/inbound/openai.go`)
4. **GitHub Copilot Provider** (`pkg/providers/copilot`)

---

## 1. Google Gemini Protocol Findings

### Context & Errors Encountered
When proxying Claude Code or OpenAI-compatible clients to Google Gemini models (`gemini-2.5-pro`, `gemini-2.5-flash`, `gemini-3.7-flash`):
1. `Function call is missing a thought_signature in functionCall parts`: Gemini reasoning models enforce that the cryptographic thought signature generated during reasoning must be replayed alongside historic function calls.
2. `Requests ending with a model turn are not supported`: Gemini strictly forbids requests ending with a `model` turn; assistant prefills or unanswered tool-call sequences trigger immediate 400 rejections.

### Comparative Analysis: `llm-router` vs LiteLLM

| Dimension | `llm-router` Current Implementation | LiteLLM (`BerriAI/litellm`) Approach | Verdict & Action Required |
|---|---|---|---|
| **Signature Storage** | In-memory thread-safe LRU Cache (`pkg/providers/google/cache.go`) keyed by `toolCallID` | Embedded into `tool_call_id` as `call_<id>__thought__<signature>` | **Keep `llm-router` approach**. LiteLLM's string-embedding strategy causes tool call ID length truncations and regex validation errors in Claude Code / Anthropic (`^[a-zA-Z0-9_-]+$`). `llm-router`'s cache prevents on-the-wire pollution. |
| **Fallback Sentinel** | Injects `"skip_thought_signature_validator"` when signature is missing from cache | Injects `"skip_thought_signature_validator"` when missing from ID | **Aligned**. Both use Google's official bypass sentinel for prefill/cross-model tool turns. |
| **Trailing Model Turn Continuation** | Appends `Part{Text: "Continue"}` | Appends `Part{Text: "."}` ([PR #38652](https://github.com/BerriAI/litellm/pull/38652)) | **Adopt LiteLLM `"."` placeholder**. Empirical benchmarks in LiteLLM proved that `"Continue"` resets context causing generic greetings (*"How can I help you today?"*), whereas `"."` reliably triggers continuation while retaining 100% prompt context. |
| **Tool Call & Response ID Gating** | Emits `id` unconditionally if present | Gated by `_is_gemini_3_or_newer(model)` ([PR #34603](https://github.com/BerriAI/litellm/pull/34603)) | **Adopt LiteLLM version gating**. Gemini 2.5 and 3.x require `id` for tool pairing, but Gemini 1.5 rejects `id` fields in `functionCall`/`functionResponse` with `HTTP 400`. |
| **Unanswered Tool Call Handling** | Synthesizes dummy `FunctionResponse` to complete the turn | Fails if model turn ends on tool calls | **Keep `llm-router` approach**. Prevents 400 errors when tool call turns are interrupted. |
| **Google Inbound Tool Streaming** | Emits empty `args: {}` on `EventToolCallStart`; ignores `EventToolCallDelta`/`Done` | Buffers delta arguments and emits complete `functionCall` | **Critical Bug in `llm-router`**: Streaming Gemini clients receive blank tool call arguments. Must buffer deltas and emit on `EventToolCallDone`. |
| **FunctionResponse ID Correlation** | Uses `p.FunctionResponse.Name` for `ToolResultID` | Prefers `p.FunctionResponse.ID`, falls back to `Name` | **Fix in `llm-router`**: `FromGoogleRequest` must prioritize `ID` over `Name` to disambiguate parallel tool calls. |

---

## 2. Anthropic Protocol Findings

### Context & Requirements
Anthropic's `/v1/messages` API is much more restrictive than OpenAI's chat completions. Violations of its strict message structure cause immediate `HTTP 400 invalid_request_error`.

### Comparative Analysis: `llm-router` vs LiteLLM

| Dimension | `llm-router` Current Implementation | LiteLLM (`BerriAI/litellm`) Approach | Verdict & Action Required |
|---|---|---|---|
| **Tool Name Validation** | Passes `tool.Name` raw to Anthropic | Replaces invalid chars with `_` to match `^[a-zA-Z0-9_-]{1,128}$` + request-scoped bidirectional map | **High Priority Fix**: MCP server tools frequently contain `:` or `.` (e.g., `default_api:Bash`, `workspace.readFile`). Anthropic rejects these with 400. Must sanitize outgoing names and reverse-map incoming responses. |
| **Strict Role Alternation** | Emits consecutive `Role: "user"` messages for consecutive tool results or user turns | Merges consecutive `{"user", "tool", "function"}` turns into a single `user` message with multiple content blocks | **High Priority Fix**: Anthropic throws `HTTP 400: messages: roles must alternate between "user" and "assistant"`. Must merge consecutive same-role turns. |
| **Initial Turn Constraint** | Passes through directly | Injects dummy user continue message if history begins with `assistant` | **Fix**: Prepend placeholder user message if history begins with an assistant turn. |
| **Empty Text Content Blocks** | Allows empty strings in some array blocks | Filters out empty text blocks (`text: ""`) across all turns (`_sanitize_empty_text_content`) | **Fix**: Anthropic rejects empty text blocks (`messages: text content blocks must be non-empty`). Must filter them out. |
| **Assistant Prefill Trailing Whitespace** | Does not trim trailing whitespace | Trims trailing whitespace on assistant turns | **Fix**: Anthropic rejects assistant prefills with trailing whitespace. Must trim whitespace. |
| **Tool Declaration for Historic Tool Calls** | Sends `req.Tools` as provided | Injects dummy tool declaration if messages contain `tool_use`/`tool_result` but `tools` is omitted | **Fix**: Avoids 400 when continuing conversation with prior tool turns if client omits tools parameter. |
| **Streaming Chunk Separation** | Flushes events sequentially | Uses `_CombinedChunkSplitter` to separate content deltas from finish chunks | **Adopt pattern**: Ensure active content blocks are cleanly closed before emitting `message_delta` stop reason. |

---

## 3. OpenAI Protocol Findings

### Context & Requirements
The OpenAI chat completions protocol is the de-facto industry standard, but has evolved rapidly with reasoning models (`o1`, `o3-mini`, `o4-preview`), DeepSeek-R1, and custom OpenAI-compatible inference servers (vLLM, sglang, Ollama).

### Comparative Analysis: `llm-router` vs LiteLLM

| Dimension | `llm-router` Current Implementation | LiteLLM (`BerriAI/litellm`) Approach | Verdict & Action Required |
|---|---|---|---|
| **Streaming Reasoning Deltas** | `StreamDelta` only has `Role`, `Content`, `ToolCalls`; reasoning tokens completely dropped | Maps `delta.reasoning_content` (DeepSeek/vLLM/Ollama), `delta.reasoning` (GLM), and `delta.thought` (Gemini) | **Critical Fix**: Update `StreamDelta` in `pkg/providers/openai/types.go` to include `ReasoningContent` and `Reasoning`. In `ParseOpenAIStreamEvent`, emit `canonical.EventThinkingDelta` so reasoning models stream thinking to clients. |
| **HTTP 200 Stream Errors** | Assumes all `data:` lines unmarshal into valid `StreamChunk` | `_extract_error_from_chunk` inspects chunks for top-level `error` keys (common in vLLM/sglang) | **Fix**: Check for `error` payload in SSE chunks and emit `EventError` instead of failing unmarshaling or hanging. |
| **Tool Name 64-Char Limit** | Sends raw function name | Truncates names > 64 chars to `{prefix[:55]}_{sha256[:8]}` and reverse-maps on execution | **Fix**: OpenAI rejects tool names > 64 chars with 400. Truncate with deterministic hash if `len > 64`. |
| **Image Hoisting from Tool Messages** | Serializes tool result to plain text | Hoists image parts out of tool messages into subsequent user turns (`hoist_images_from_tool_messages`) | **Improvement**: OpenAI rejects images inside `role: "tool"`. Hoist images to a user turn if present. |
| **Timestamp Generation Stub** | `timeNowUnixMilli() int64 { return 1700000000000 }` hardcoded stub | Dynamic `time.Now()` / UUID | **Bug Fix**: Replace hardcoded stub with `time.Now().UnixMilli()`. |

---

## 4. GitHub Copilot Provider Findings

### Context & Requirements
GitHub Copilot provides OpenAI-compatible `/chat/completions` endpoints for both OpenAI models (`gpt-4o`, `o1`, `o3-mini`) and Anthropic models (`claude-3.5-sonnet`, `claude-3.7-sonnet`).

### Comparative Analysis: `llm-router` vs LiteLLM

| Dimension | `llm-router` Current Implementation | LiteLLM (`BerriAI/litellm`) Approach | Verdict & Action Required |
|---|---|---|---|
| **Anthropic-Native Response Handling** | Deserializes response directly into `openai.ChatCompletionResponse`; fails with `"no choices returned by OpenAI"` when `Choices` is empty | Implements `_synthesize_choices_for_anthropic_native` ([Issue #29391](https://github.com/BerriAI/litellm/issues/29391)): converts Anthropic content blocks into OpenAI choices | **Critical Bug in `llm-router`**: Copilot returns Anthropic-native JSON (`content: [...]`, `stop_reason: ...`) for Claude models. `llm-router` breaks 100% of the time on Claude via Copilot in non-streaming mode. Must synthesize choices. |
| **Request Headers** | Sends basic editor headers | Sends `x-github-api-version: "2025-04-01"`, `x-request-id: <uuid>`, `x-vscode-user-agent-library-version: "electron-fetch"` | **Improvement**: Add API version and unique request ID for tracing and stability. |

---

## 5. Master Issue & Priority Matrix

| Component | Issue Description | Root Cause | Impact | Priority |
|---|---|---|---|---|
| **Copilot** | Claude models return Anthropic-native JSON without `choices` | Unmarshaling expects OpenAI `choices` array | 100% failure on Claude via Copilot | **P0** |
| **Google Inbound** | Streaming tool arguments are dropped | Emits empty args on start, ignores deltas | Blank function args on streaming | **P0** |
| **OpenAI** | `StreamDelta` lacks `reasoning_content` / `reasoning` | Missing struct fields in `StreamDelta` | Thinking tokens lost from o1/o3/R1 | **P0** |
| **Anthropic** | Consecutive same-role turns cause 400 | Consecutive tool/user turns not merged | Rejection on multi-tool conversations | **P0** |
| **Gemini Outbound** | Continuation placeholder resets context | `"Continue"` treated as new query by Gemini | Generic greetings instead of continuation | **P1** |
| **Anthropic** | Tool names with `:`, `.`, `/` cause 400 | No regex sanitization (`^[a-zA-Z0-9_-]{1,128}$`) | Rejection on MCP tools (e.g. `default_api:Bash`) | **P1** |
| **Gemini Outbound** | Older Gemini 1.5 models reject `id` in function calls | Unconditional emission of `id` | 400 on `gemini-1.5-*` | **P1** |
| **Google Inbound** | `FunctionResponse` uses `Name` instead of `ID` | Hardcoded use of `p.FunctionResponse.Name` | Multi-tool response mismatch | **P1** |
| **OpenAI** | Tool names > 64 chars trigger 400 | No truncation on long tool names | Rejection on long tool names | **P1** |
| **OpenAI** | HTTP 200 stream error payloads ignored | Assumes every SSE chunk is valid completion | Hangs or silent errors on vLLM/sglang | **P1** |
| **Anthropic** | Empty text blocks cause 400 | `text: ""` passed through | Rejection on empty content blocks | **P2** |
| **Anthropic** | Trailing whitespace in assistant prefill causes 400 | Untrimmed assistant turns | Rejection on assistant prefills | **P2** |
| **OpenAI** | Mock timestamp `1700000000000` | Leftover test stub in production code | Incorrect generated IDs | **P2** |

---

## 6. Detailed Implementation Blueprint

### Phase 1: Critical Bug Fixes (P0)

#### 1.1 GitHub Copilot: Anthropic-Native Response Synthesis
- **Files**: `pkg/providers/copilot/client.go`
- **Logic**:
  Before parsing into `openai.ChatCompletionResponse`, check if the raw JSON payload has an empty `choices` array and a non-empty `content` array:
  ```go
  // If response contains "content" array and no "choices", synthesize OpenAI choices
  if len(rawMap["choices"]) == 0 && rawMap["content"] != nil {
      // Parse Anthropic content blocks (text, tool_use, thinking)
      // Map stop_reason ("end_turn" -> "stop", "tool_use" -> "tool_calls")
      // Map usage ("input_tokens" -> "prompt_tokens", "output_tokens" -> "completion_tokens")
      // Synthesize choices: [{"index": 0, "message": synthesizedMsg, "finish_reason": finishReason}]
  }
  ```

#### 1.2 Google Inbound: Streaming Tool Argument Assembly
- **Files**: `pkg/server/inbound/google.go`
- **Logic**:
  Maintain an argument accumulator `toolArgsBuffer map[int]string` during streaming:
  - On `EventToolCallStart`: Record tool call ID and name. Do NOT emit chunk yet.
  - On `EventToolCallDelta`: Append `ev.ToolCallArgs` to the buffer for that index.
  - On `EventToolCallDone`: Parse accumulated JSON arguments and emit the complete Google `functionCall` chunk.

#### 1.3 OpenAI: StreamDelta Reasoning Extraction
- **Files**: `pkg/providers/openai/types.go`, `pkg/providers/openai/transform.go`
- **Logic**:
  Add `ReasoningContent` and `Reasoning` to `StreamDelta`:
  ```go
  type StreamDelta struct {
      Role             string     `json:"role,omitempty"`
      Content          string     `json:"content,omitempty"`
      ReasoningContent string     `json:"reasoning_content,omitempty"`
      Reasoning        string     `json:"reasoning,omitempty"`
      ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
  }
  ```
  In `ParseOpenAIStreamEvent`, if `delta.ReasoningContent != ""` or `delta.Reasoning != ""`, emit `canonical.EventThinkingDelta`.

#### 1.4 Anthropic: Message Turn Merging (Role Alternation)
- **Files**: `pkg/providers/anthropic/transform.go`
- **Logic**:
  In `ToAnthropicRequest`, iterate through canonical messages and merge adjacent turns of the same Anthropic role (`user` + `tool` ➔ single `user` message with both `text` and `tool_result` content blocks; adjacent `assistant` turns ➔ single `assistant` message).

#### 1.5 OpenAI: Replace Hardcoded Timestamp Stub
- **Files**: `pkg/providers/openai/transform.go`
- **Logic**:
  Replace `timeNowUnixMilli() int64 { return 1700000000000 }` with `time.Now().UnixMilli()`.

---

### Phase 2: Protocol Sanitization & Edge-Case Hardening (P1)

#### 2.1 Gemini: Switch Continuation Placeholder to `"."`
- **Files**: `pkg/providers/google/transform.go`
- **Logic**:
  Change trailing model turn normalization from `Part{Text: "Continue"}` to `Part{Text: "."}`.

#### 2.2 Anthropic: Tool Name Sanitization with Bidirectional Mapping
- **Files**: `pkg/providers/anthropic/transform.go`
- **Logic**:
  Sanitize tool names matching `^[a-zA-Z0-9_-]{1,128}$`. Maintain a bidirectional map (`forward` for request, `reverse` for response) so client tools like `default_api:Bash` are safely passed to Anthropic as `default_api_Bash` and translated back on output.

#### 2.3 Gemini: Tool ID Version Gating
- **Files**: `pkg/providers/google/transform.go`
- **Logic**:
  Only emit `ID` on `FunctionCall` and `FunctionResponse` for Gemini 2.5 and 3.x models (`strings.Contains(model, "2.5") || strings.Contains(model, "3.")`). Strip for Gemini 1.5.

#### 2.4 Google Inbound: Prioritize FunctionResponse ID
- **Files**: `pkg/providers/google/transform.go`
- **Logic**:
  In `FromGoogleRequest`, set `ToolResultID: p.FunctionResponse.ID` if present, falling back to `p.FunctionResponse.Name`.

#### 2.5 OpenAI: Tool Name 64-Character Truncation
- **Files**: `pkg/providers/openai/transform.go`
- **Logic**:
  If a tool name exceeds 64 characters, format as `{name[:55]}_{hash[:8]}`.

#### 2.6 Stream Error Payload Handling
- **Files**: `pkg/providers/openai/transform.go`, `pkg/providers/copilot/client.go`
- **Logic**:
  In SSE stream scanner, check if parsed chunk contains an `error` key. If present, emit `canonical.EventError`.

---

### Phase 3: Advanced Parity & Cleanups (P2)

#### 3.1 Anthropic Content Block & Prefill Hygiene
- Strip empty text blocks (`text: ""`).
- Trim trailing whitespace on assistant turns.
- Inject a dummy continue message if conversation begins with an assistant turn.
- Inject a dummy tool declaration if messages contain `tool_use`/`tool_result` but `tools` parameter is omitted.

#### 3.2 Copilot Headers
- Add `x-github-api-version: "2025-04-01"` and generate unique `x-request-id` (UUIDv4) per request.

#### 3.3 Prompt Caching (`cache_control`)
- Add `CacheControl` to `canonical.ContentPart`.
- Forward `cache_control: {"type": "ephemeral"}` in Anthropic and Gemini adapters.

---

## 7. Verification & Testing Strategy

1. **Unit Tests**:
   - `pkg/providers/copilot/client_test.go`: Test unmarshaling Anthropic-native Copilot response into canonical response.
   - `pkg/server/inbound/google_test.go`: Test streaming tool call delta accumulation and final JSON argument delivery.
   - `pkg/providers/openai/transform_test.go`: Test `delta.reasoning_content` extraction to `EventThinkingDelta`.
   - `pkg/providers/anthropic/transform_test.go`: Test turn merging for consecutive `user` + `tool` messages and tool name sanitization.
   - `pkg/providers/google/transform_test.go`: Test `"."` continuation placeholder and version-gated tool call IDs.

2. **End-to-End Client Testing**:
   - Test Claude Code CLI with `gemini-2.5-pro` and `gemini-3.7-flash` (verify thought signatures and prefill continuation).
   - Test Claude Code CLI with Copilot `claude-3.7-sonnet` (verify non-streaming response synthesis).
   - Test OpenCode / Aider with OpenAI reasoning models (verify reasoning deltas stream properly).
   - Test Gemini REST client with `streamGenerateContent` using tool calls.

