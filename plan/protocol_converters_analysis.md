# Protocol Converters Analysis & Comparison with LiteLLM

## Executive Summary

This document presents a comprehensive comparative analysis of the protocol converters and provider implementations in `llm-router` against [LiteLLM (`BerriAI/litellm`)](https://github.com/BerriAI/litellm), the leading open-source LLM proxy.

The analysis covers:
1. **Anthropic Protocol Converter**: Outbound (`pkg/providers/anthropic/transform.go`) & Inbound (`pkg/server/inbound/anthropic.go`).
2. **OpenAI Protocol Converter**: Outbound (`pkg/providers/openai/transform.go`) & Inbound (`pkg/server/inbound/openai.go`).
3. **GitHub Copilot Provider**: Authentication, request routing & response handling (`pkg/providers/copilot/client.go`, `auth.go`).
4. **Google Gemini Inbound Converter**: REST & SSE handlers (`pkg/server/inbound/google.go`) and inbound parsing (`pkg/providers/google/transform.go`).

Through production battle-testing, LiteLLM has encountered and solved numerous subtle edge cases, provider quirks, and schema validation pitfalls. This audit highlights critical gaps in `llm-router` and provides an actionable implementation plan.

---

## 1. Anthropic Protocol Converter

### Current `llm-router` Implementation
- **Outbound**: `ToAnthropicRequest` maps canonical messages to Anthropic `MessageRequest`.
- **Inbound**: `AnthropicHandler` handles `/v1/messages` (non-streaming and SSE streaming).
- **Streaming Parser**: `ParseAnthropicStreamEvent` converts SSE events (`content_block_start`, `content_block_delta`, `message_delta`, etc.) into `CanonicalEvent`.

### LiteLLM Reference Implementation
- `litellm/llms/anthropic/chat/transformation.py` (`AnthropicConfig`)
- `litellm/litellm_core_utils/prompt_templates/factory.py` (`anthropic_messages_pt`)
- `litellm/llms/anthropic/experimental_pass_through/adapters/streaming_iterator.py` (`AnthropicStreamWrapper`, `_CombinedChunkSplitter`)
- `litellm/llms/anthropic/common_utils.py`

### Gap Analysis & Key Findings

#### A. Tool Name Character Validation & Collisions (CRITICAL)
- **Anthropic Requirement**: Anthropic strictly validates tool names with `^[a-zA-Z0-9_-]{1,128}$`.
- **The Problem**: MCP (Model Context Protocol) servers and agent frameworks frequently emit names with colons, dots, or slashes (e.g. `default_api:Bash`, `workspace.readFile`, `tools/run_command`). Anthropic returns `HTTP 400: invalid tool name`.
- **LiteLLM Approach**:
  - `_basic_sanitize_anthropic_tool_name`: Replaces `[^a-zA-Z0-9_-]` with `_` and truncates to 128 characters.
  - `_build_anthropic_tool_name_maps`: Constructs bidirectional `forward` (original -> sanitized) and `reverse` (sanitized -> original) maps per request.
  - Rewrites tool names in outgoing `tools` declarations and message history `tool_use` blocks.
  - On incoming response chunks / streaming deltas, transparently maps the sanitized name back to the original using the reverse map so the client receives the exact tool name it registered.
- **`llm-router` Status**: Passes `tool.Name` directly without sanitization. Sending MCP tools with `:` or `.` to Anthropic fails with 400.

#### B. Strict Alternation of Roles & Consecutive Same-Role Merging (CRITICAL)
- **Anthropic Requirement**: The Anthropic Messages API strictly requires alternating `user` and `assistant` roles. Multiple consecutive `user` turns or `assistant` turns are rejected with `HTTP 400: messages: roles must alternate between "user" and "assistant"`.
- **The Problem**:
  - In canonical/OpenAI format, clients commonly send consecutive `user` messages, or `user` followed by one or more `tool` messages.
  - In `llm-router`'s `ToAnthropicRequest`:
    ```go
    // For each message, if m.Role == RoleTool, it appends a separate Message{Role: "user", ...}
    ```
    If there are two consecutive tool results, `llm-router` emits two consecutive `Role: "user"` messages. Anthropic immediately rejects the request.
- **LiteLLM Approach**:
  - In `anthropic_messages_pt`, LiteLLM merges consecutive turns where `role in {"user", "tool", "function"}` into a single Anthropic `user` turn containing multiple content blocks (`text` blocks and `tool_result` blocks).
  - Similarly merges consecutive `assistant` messages into one assistant turn.

#### C. First Message Must Be `user` Role (MODERATE)
- **Anthropic Requirement**: Anthropic requires that the first message in `messages` must be a `user` turn.
- **LiteLLM Approach**: If conversation history begins with an `assistant` turn (or is empty), LiteLLM automatically prepends a dummy user turn (`DEFAULT_USER_CONTINUE_MESSAGE_TYPED = {"role": "user", "content": "..."}`) to prevent a 400 rejection.
- **`llm-router` Status**: Passes through directly; if history starts with an assistant turn, Anthropic rejects it.

#### D. Empty Content Block Rejection (MODERATE)
- **Anthropic Requirement**: Anthropic explicitly rejects empty text content blocks: `messages: text content blocks must be non-empty`.
- **LiteLLM Approach**: Automatically filters out empty text blocks from system prompts, user turns, and assistant turns (`_sanitize_empty_text_content`).
- **`llm-router` Status**: Partially checks string content, but allows empty text blocks in some array structures.

#### E. Assistant Prefill Trailing Whitespace (MODERATE)
- **Anthropic Requirement**: When prefilling assistant responses (the last message is `role: assistant`), Anthropic rejects trailing whitespace (`trailing whitespace is not allowed in prefill assistant message`).
- **LiteLLM Approach**: Trims trailing whitespace on assistant turns that conclude the request.
- **`llm-router` Status**: Does not trim trailing whitespace.

#### F. Injected Dummy Tool for Tool Result History (EDGE CASE)
- **Anthropic Requirement**: If `messages` contains `tool_use` or `tool_result` blocks from previous turns, Anthropic requires the `tools` parameter to be present in the request. If the client omits `tools` on a follow-up call, Anthropic returns `HTTP 400`.
- **LiteLLM Approach**: If `has_tool_call_blocks(messages)` is true but `tools` is not provided, LiteLLM injects a dummy tool definition to satisfy the API validator.
- **`llm-router` Status**: Does not check or inject dummy tools.

#### G. Streaming: Combined Chunk Splitting (EDGE CASE)
- **LiteLLM Insight**: In `_CombinedChunkSplitter`, LiteLLM splits streaming chunks that contain both response content (text/thinking/tools) AND a `finish_reason` into two separate events: a content chunk followed by a finish chunk. In Anthropic SSE, emitting `stop_reason` before closing active content blocks corrupts client state machines.
- **`llm-router` Status**: `inbound/anthropic.go` closes the active block on `EventMessageDelta` and `EventMessageDone`, which is good, but could be made more resilient to mixed deltas.

---

## 2. OpenAI Protocol Converter

### Current `llm-router` Implementation
- **Outbound**: `ToOpenAIRequest` maps canonical requests to `ChatCompletionRequest`. Handles `max_tokens` vs `max_completion_tokens` and clears temperature for `o1`/`o3`.
- **Inbound**: `OpenAIHandler` serves `/v1/chat/completions` (JSON and SSE).
- **Streaming Parser**: `ParseOpenAIStreamEvent` extracts `EventTextDelta`, `EventToolCall*`, and `EventMessageDelta`.

### LiteLLM Reference Implementation
- `litellm/llms/openai/chat/gpt_transformation.py` (`OpenAIGPTConfig`)
- `litellm/litellm_core_utils/llm_response_utils/convert_dict_to_response.py`

### Gap Analysis & Key Findings

#### A. Reasoning Content & Thinking Deltas are Dropped (CRITICAL)
- **The Problem**: Newer reasoning models (OpenAI `o1`, `o3-mini`, `o4-preview`, DeepSeek-R1, and local engines like Ollama/vLLM) emit reasoning tokens during streaming. These are delivered under:
  - `delta.reasoning_content` (DeepSeek-R1, vLLM, Ollama, OpenRouter)
  - `delta.reasoning` (GLM-5, hosted_vllm)
  - `delta.thought` (Gemini OpenAI adapter)
- **`llm-router` Status**:
  In `pkg/providers/openai/types.go`:
  ```go
  type StreamDelta struct {
      Role      string     `json:"role,omitempty"`
      Content   string     `json:"content,omitempty"`
      ToolCalls []ToolCall `json:"tool_calls,omitempty"`
  }
  ```
  `StreamDelta` completely lacks `ReasoningContent` or `Reasoning` fields!
  As a result, `ParseOpenAIStreamEvent` completely drops reasoning tokens. When a user connects Claude Code or an Anthropic-compatible tool to `llm-router` backed by a reasoning model, no thinking blocks are displayed.
- **LiteLLM Approach**:
  `_map_reasoning_to_reasoning_content` maps `reasoning` and `thought` to `reasoning_content`. LiteLLM's stream adapter then emits `thinking_delta` events with the reasoning text.

#### B. Tool Name 64-Character Limit (MODERATE)
- **OpenAI Requirement**: OpenAI enforces a strict 64-character limit on tool/function names. Names exceeding 64 characters trigger `HTTP 400: 'name' is too long`.
- **LiteLLM Approach**:
  `truncate_tool_name` checks `len(name) > 64`. If exceeded, it formats the name as:
  `{name[:55]}_{sha256(name)[:8]}` (total 64 chars) and maintains a bidirectional map to restore the original name when the model invokes the tool.
- **`llm-router` Status**: Sends original tool names regardless of length.

#### C. HTTP 200 Stream Error Payloads (MODERATE)
- **The Problem**: Several OpenAI-compatible servers (vLLM, sglang, TGI) occasionally return an HTTP 200 SSE stream where the first or subsequent event contains an error payload:
  `data: {"error": {"message": "...", "code": 400}}`
- **LiteLLM Approach**: `_extract_error_from_chunk` inspects incoming chunks for an `error` key. If present, it stops iteration and raises a proper API error.
- **`llm-router` Status**: Assumes all `data:` lines unmarshal into valid `StreamChunk`. An error JSON payload fails unmarshaling or is silently ignored, leaving streams hanging.

#### D. Image Hoisting from Tool Messages (EDGE CASE)
- **OpenAI Requirement**: OpenAI rejects multipart content or images embedded inside `role: "tool"` messages.
- **LiteLLM Approach**: `hoist_images_from_tool_messages` extracts image parts from tool messages and moves them into a subsequent `user` turn.
- **`llm-router` Status**: Converts tool messages strictly to string content (`p.ToolResultContent`). If a tool returns an image, the image is omitted.

#### E. Hardcoded Mock Timestamp Bug (CODE SMELL)
- In `pkg/providers/openai/transform.go`:
  ```go
  func timeNowUnixMilli() int64 {
      return 1700000000000 // Hardcoded test stub!
  }
  ```
  `ToOpenAIResponse` uses this hardcoded timestamp for non-streaming response IDs when `resp.ID` is empty. Should be `time.Now().UnixMilli()`.

---

## 3. GitHub Copilot Provider

### Current `llm-router` Implementation
- `pkg/providers/copilot/auth.go`: Exchanges GitHub OAuth token for Copilot session token (`tid=...`) with caching.
- `pkg/providers/copilot/client.go`: Forwards canonical requests by converting them to OpenAI format and posting to Copilot's `/chat/completions`.

### LiteLLM Reference Implementation
- `litellm/llms/github_copilot/chat/transformation.py` (`GithubCopilotConfig`)
- `litellm/llms/github_copilot/common_utils.py`

### Gap Analysis & Key Findings

#### A. Anthropic-Native Responses for Claude Models via Copilot (CRITICAL BUG)
- **The Problem**: GitHub Copilot hosts models from both OpenAI (`gpt-4o`, `o1`, `o3-mini`) and Anthropic (`claude-3.5-sonnet`, `claude-3.7-sonnet`).
  When querying Claude models through Copilot's `/chat/completions`, Copilot often returns an **Anthropic-native JSON structure** rather than an OpenAI `choices` array:
  ```json
  {
    "id": "msg_...",
    "content": [{"type": "text", "text": "..."}],
    "stop_reason": "end_turn",
    "usage": {"input_tokens": 120, "output_tokens": 45}
  }
  ```
- **The Failure in `llm-router`**:
  In `pkg/providers/copilot/client.go`:
  ```go
  var openAIResp openai.ChatCompletionResponse
  json.Unmarshal(respBody, &openAIResp)
  return openai.FromOpenAIResponse(&openAIResp)
  ```
  Because `openAIResp.Choices` is empty, `FromOpenAIResponse` throws:
  `"no choices returned by OpenAI"`.
  This completely breaks all Claude models routed through GitHub Copilot in non-streaming mode!
- **LiteLLM Solution**:
  LiteLLM discovered this exact behavior (tracked in [LiteLLM Issue #29391](https://github.com/BerriAI/litellm/issues/29391)).
  In `GithubCopilotConfig._synthesize_choices_for_anthropic_native(response_json)`:
  1. Checks if `choices` is missing or empty.
  2. If `content` is a list of Anthropic content blocks, extracts text, tool calls, and thinking blocks.
  3. Maps `stop_reason` (`end_turn` -> `stop`, `tool_use` -> `tool_calls`, `max_tokens` -> `length`).
  4. Normalizes `usage` (`input_tokens` -> `prompt_tokens`, `output_tokens` -> `completion_tokens`).
  5. Synthesizes a valid `choices: [{"index": 0, "message": ..., "finish_reason": ...}]` structure before unmarshaling!

#### B. Modern Copilot Request Headers (MODERATE)
- **LiteLLM Approach**:
  ```python
  "x-github-api-version": "2025-04-01",
  "x-request-id": str(uuid4()),
  "x-vscode-user-agent-library-version": "electron-fetch",
  "copilot-integration-id": "vscode-chat",
  "editor-version": "vscode/1.95.0",
  "editor-plugin-version": "copilot-chat/0.26.7",
  "openai-intent": "conversation-panel"
  ```
- **`llm-router` Status**: Sends `Editor-Version`, `Editor-Plugin-Version`, `Copilot-Integration-Id`, and `Openai-Intent`, but is missing `x-github-api-version` and unique `x-request-id` per request.

---

## 4. Google Inbound Converter

### Current `llm-router` Implementation
- `pkg/server/inbound/google.go`: Serves Google Gemini REST endpoints (`/v1beta/models/{model}:generateContent` and `:streamGenerateContent`).
- `pkg/providers/google/transform.go`: `FromGoogleRequest` converts Gemini requests to canonical.

### Gap Analysis & Key Findings

#### A. Missing Streaming Tool Arguments in Google Inbound (CRITICAL BUG)
- **The Problem**: In `pkg/server/inbound/google.go`:
  ```go
  if ev.Type == canonical.EventToolCallStart {
      chunk := map[string]any{
          "candidates": []map[string]any{
              {
                  "content": map[string]any{
                      "role": "model",
                      "parts": []map[string]any{
                          {
                              "functionCall": map[string]any{
                                  "name": ev.ToolCallName,
                                  "args": map[string]any{}, // Empty args!
                              },
                          },
                      },
                  },
              },
          },
      }
      // Sends chunk immediately...
  }
  ```
  Notice that `EventToolCallDelta` and `EventToolCallDone` are **NOT handled at all**!
  When an upstream model streams tool arguments, `inbound/google.go` emits an empty `args: {}` on start, and completely drops the argument tokens! Any Gemini client using streaming tool calling receives blank arguments.
- **Fix Required**:
  Accumulate tool call argument chunks per tool call index/ID, and emit the complete `functionCall` part with parsed `args` once `EventToolCallDone` arrives.

#### B. FunctionResponse ID vs Name Mapping (MODERATE)
- In `FromGoogleRequest`:
  ```go
  if p.FunctionResponse != nil {
      out.Messages = append(out.Messages, canonical.Message{
          Role: canonical.RoleTool,
          Parts: []canonical.ContentPart{
              {
                  Type: canonical.PartToolResult,
                  ToolResultID: p.FunctionResponse.Name, // Using Name instead of ID!
              },
          },
      })
  }
  ```
  In Gemini 2.5 and 3.x, `FunctionResponse` carries both `id` and `name`. If `id` is populated, `ToolResultID` MUST use `p.FunctionResponse.ID` to properly pair with the upstream `ToolCallID`. Using `Name` breaks multi-tool disambiguation.

---

## 5. Summary Matrix: LiteLLM vs `llm-router`

| Feature / Quirk | LiteLLM Solution | `llm-router` Current State | Severity |
|---|---|---|---|
| **Anthropic Tool Name Validation** | Sanitizes to `^[a-zA-Z0-9_-]{1,128}$` + bidirectional request map | Raw name pass-through (fails on MCP `:` and `.`) | **High** |
| **Anthropic Role Alternation** | Automatically merges consecutive same-role turns (`user`/`tool`) | Emits consecutive `user` turns (rejected by Anthropic 400) | **High** |
| **OpenAI Streaming Reasoning** | Parses `reasoning_content` / `reasoning` and emits thinking deltas | `StreamDelta` ignores reasoning fields (tokens lost) | **High** |
| **Copilot Claude Model Parsing** | Synthesizes `choices` from Anthropic-native response structure | Errors: `"no choices returned by OpenAI"` | **High** |
| **Google Inbound Tool Streaming** | Buffers and emits complete `functionCall.args` | Emits empty `args: {}` and drops deltas | **High** |
| **Anthropic Empty Text Blocks** | Strips empty text blocks from turns | Occasionally forwards `""` text blocks | **Medium** |
| **Anthropic Prefill Whitespace** | Trims trailing whitespace on assistant turns | Untrimmed | **Medium** |
| **OpenAI 64-char Tool Name Limit** | Hashes long names: `{prefix}_{hash}` | Untruncated | **Medium** |
| **Stream Error Payloads** | Inspects chunks for `{"error": ...}` | Fails unmarshaling or hangs | **Medium** |
| **Gemini FunctionResponse ID** | Prefers `id`, falls back to `name` | Uses `name` only | **Medium** |
| **OpenAI ID Generation** | Dynamic `time.Now()` | Hardcoded `1700000000000` timestamp stub | **Low** |

---

## 6. Implementation Action Plan

### Phase 1: Critical Bug Fixes (High Priority)
1. **GitHub Copilot Claude Support**:
   - In `pkg/providers/copilot/client.go`, check if `choices` is empty and `content` is present in response JSON.
   - If Anthropic-native content blocks are detected, synthesize OpenAI-style `choices` (extract text, tool calls, thinking, and normalize usage).
2. **OpenAI Reasoning Delta Extraction**:
   - Update `StreamDelta` in `pkg/providers/openai/types.go` to include `ReasoningContent string `json:"reasoning_content,omitempty"`` and `Reasoning string `json:"reasoning,omitempty"``.
   - In `ParseOpenAIStreamEvent`, emit `canonical.EventThinkingDelta` when reasoning content is present.
3. **Google Inbound Streaming Tool Arguments**:
   - In `pkg/server/inbound/google.go`, accumulate streaming tool call arguments in a map keyed by index/ID.
   - Emit the `functionCall` chunk on `EventToolCallDone` with parsed JSON arguments rather than on `EventToolCallStart` with empty args.
4. **Anthropic Message Role Alternation**:
   - In `pkg/providers/anthropic/transform.go`, implement turn-merging in `ToAnthropicRequest`: merge consecutive `canonical.RoleUser` and `canonical.RoleTool` messages into a single Anthropic `user` turn with multiple content blocks (`text` and `tool_result`).
5. **Fix Hardcoded Timestamp**:
   - Replace `timeNowUnixMilli() int64 { return 1700000000000 }` in `pkg/providers/openai/transform.go` with `time.Now().UnixMilli()`.

### Phase 2: Protocol Sanitization & Edge-Case Hardening (Medium Priority)
1. **Anthropic Tool Name Sanitization**:
   - In `pkg/providers/anthropic/transform.go`, sanitize tool names matching `^[a-zA-Z0-9_-]{1,128}$`.
   - Maintain a request-scoped translation table to map sanitized names back to canonical names in tool calls and tool responses.
2. **Anthropic History Cleanups**:
   - Strip empty text blocks (`text: ""`).
   - Trim trailing whitespace on trailing assistant prefill messages.
   - Inject a continue message if history starts with an assistant turn.
   - Inject a dummy tool declaration if history has `tool_use`/`tool_result` but `req.Tools` is empty.
3. **OpenAI Tool Name Truncation**:
   - Truncate function names exceeding 64 characters to `{prefix}_{hash}`.
4. **Stream Error Payload Handling**:
   - In OpenAI and Copilot SSE scanners, inspect parsed chunks for top-level `error` keys and emit `EventError`.
5. **Google Inbound FunctionResponse ID**:
   - In `FromGoogleRequest`, prioritize `p.FunctionResponse.ID` over `p.FunctionResponse.Name` for `ToolResultID`.

### Phase 3: Advanced Feature Parity (Future Enhancements)
1. **Prompt Caching (`cache_control`)**:
   - Add `CacheControl` field to `canonical.ContentPart`.
   - Forward `cache_control: {"type": "ephemeral"}` in Anthropic and Gemini adapters.
2. **Copilot Headers Update**:
   - Add `x-github-api-version: "2025-04-01"` and generate unique `x-request-id` (UUIDv4) per Copilot request.
3. **Image Hoisting**:
   - Hoist images returned in tool results into subsequent user turns for OpenAI compatibility.

