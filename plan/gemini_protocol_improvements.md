# Plan: Gemini Protocol Improvements & LiteLLM Comparative Analysis

## Context & Objectives

When routing Claude Code (speaking the Anthropic Messages API) to Google Gemini reasoning models (`gemini-3.7-flash`, `gemini-3.8-flash`, `gemini-2.5-*`), two distinct 400 `INVALID_ARGUMENT` errors were encountered:
1. `Function call is missing a thought_signature in functionCall parts`: Gemini 2.5 and 3.x require the cryptographic thought signature emitted during model reasoning to be re-sent alongside historical function calls.
2. `Requests ending with a model turn are not supported`: Gemini requires requests to terminate on a `user` turn; incomplete tool-turn sequences or assistant prefills trigger strict rejection.

This document summarizes the architectural analysis of how [**LiteLLM** (`BerriAI/litellm`)](https://github.com/BerriAI/litellm) addresses these exact challenges, contrasts it with `llm-router`'s implementation, and defines concrete follow-up improvements.

---

## 1. LiteLLM Deep Dive: What LiteLLM Does

### A. Trailing Model Turns & Prefill Normalization
- **PR & Issue References**: [PR #38652](https://github.com/BerriAI/litellm/pull/38652), [Issue #38537](https://github.com/BerriAI/litellm/issues/38537).
- **LiteLLM Approach**:
  LiteLLM implements `_append_user_after_text_only_model_tail` in `litellm/llms/vertex_ai/gemini/transformation.py`. When the conversation history concludes with a text-only `model` turn, it appends a synthetic user turn:
  ```python
  ContentType(role="user", parts=[PartType(text=".")])
  ```
- **Empirical Findings from LiteLLM Live Benchmarks**:
  - `""` (empty text part): Rejected with `HTTP 400: Requests ending with a model turn are not supported.` (Gemini discards empty content parts).
  - `"continue"` or `" "` (space): Accepted with `HTTP 200`, **but** Gemini misinterprets the text as an independent conversational input, losing prior context and outputting a generic greeting (*"How can I help you today?"*).
  - `"."` (single dot): Accepted with `HTTP 200` and **preserves full prompt instruction retention** across 100% of test runs, seamlessly prompting Gemini to continue assistant generation.

### B. Tool Call & Response ID Pairing
- **PR Reference**: [PR #34603](https://github.com/BerriAI/litellm/pull/34603).
- **LiteLLM Approach**:
  LiteLLM gates forwarding `id` on `functionCall` and `functionResponse` using `_forward_gemini_function_call_id(model)`.
  - **Gemini 2.5 / 3.x+**: `id` is forwarded and required on both `functionCall` and `functionResponse` for multi-turn tool matching.
  - **Gemini 1.5**: `id` is explicitly stripped because older Gemini 1.5 schema validators reject unexpected `id` fields with `HTTP 400`.

### C. Thought Signatures
- **Issues & PR References**: [Issue #17949](https://github.com/BerriAI/litellm/issues/17949), [Issue #37849](https://github.com/BerriAI/litellm/issues/37849), [PR #25322](https://github.com/BerriAI/litellm/pull/25322).
- **LiteLLM Approach**:
  LiteLLM attempts to serialize the thought signature directly into the `tool_call_id` string on OpenAI-compatible responses:
  `call_<id>__thought__<base64_signature>`.
  When a subsequent request arrives, LiteLLM parses the ID and splits on `__thought__`.
  If no signature is found, it falls back to:
  `_get_dummy_thought_signature() -> "skip_thought_signature_validator"`.
- **LiteLLM Failure Modes**:
  This ID-embedding design caused widespread issues:
  1. Client SDKs (such as Claude Code and Anthropic API) enforce strict regex patterns on tool IDs (`^[a-zA-Z0-9_-]+$`) and fail on long or non-standard IDs.
  2. OpenAI client libraries frequently truncate IDs longer than 40–64 characters, corrupting the base64 signature payload.
  3. Parallel calls only have a signature on the first chunk/call; fabricating one for every call triggered mismatched-parts errors.

---

## 2. Comparative Analysis: `llm-router` vs `LiteLLM`

| Dimension | `llm-router` Implementation | LiteLLM (`BerriAI/litellm`) | Verdict & Rationale |
|---|---|---|---|
| **Signature Storage** | In-memory thread-safe LRU Cache (`pkg/providers/google/cache.go`) keyed by `toolCallID` | Embedded in `tool_call_id` via `__thought__` string | **`llm-router` is superior**: Zero wire pollution; avoids client truncation and character-validation failures in Claude Code / Anthropic API. |
| **Fallback Sentinel** | `"skip_thought_signature_validator"` injected if signature missing from cache | `"skip_thought_signature_validator"` injected if missing from ID | **Aligned**: Both use Google's official bypass sentinel for cross-model / prefill calls. |
| **Prefill Continuation Placeholder** | Appends `Part{Text: "Continue"}` | Appends `Part{Text: "."}` | **LiteLLM is superior**: `"Continue"` leads to generic AI greetings (*"How can I help you?"*), while `"."` reliably triggers continuation without resetting prompt context. |
| **Unanswered Tool Call Handling** | Synthesizes mock `FunctionResponse` to satisfy Gemini's turn-matching rules | Omits handling (fails if model turn ends on tool calls) | **`llm-router` is superior**: Prevents 400 errors when tool turns are aborted or interrupted. |
| **Tool ID Version Gating** | Emits `id` unconditionally if present | Gated by `_is_gemini_3_or_newer(model)` | **LiteLLM is superior**: Older models (Gemini 1.5) reject `id` in `functionCall` / `functionResponse` with 400. |

---

## 3. Proposed Enhancements for `llm-router`

### Feature 1: Switch Continuation Placeholder from `"Continue"` to `"."`
- **Location**: `pkg/providers/google/transform.go`
- **Change**: In trailing text-only model turn normalization:
  ```go
  // Before
  out.Contents = append(out.Contents, Content{
      Role:  "user",
      Parts: []Part{{Text: "Continue"}},
  })

  // Proposed
  out.Contents = append(out.Contents, Content{
      Role:  "user",
      Parts: []Part{{Text: "."}},
  })
  ```
- **Benefit**: Retains full instruction following when assistant prefills are replayed, avoiding conversational resets.

### Feature 2: Model Version Gating for Tool Call & Response `id`
- **Location**: `pkg/providers/google/transform.go`
- **Change**:
  Define a helper function:
  ```go
  func supportsToolCallID(model string) bool {
      m := strings.ToLower(model)
      // Gemini 2.5 and 3.x support and require ID pairing; Gemini 1.5 rejects ID fields
      return strings.Contains(m, "2.5") || strings.Contains(m, "3.") || strings.Contains(m, "3-")
  }
  ```
  When constructing `FunctionCall` and `FunctionResponse`, only populate the `ID` field if `supportsToolCallID(req.Model)` is true.
- **Benefit**: Ensures backward compatibility if users route requests to `gemini-1.5-pro` or `gemini-1.5-flash`.

### Feature 3: Parallel Tool Call Signature Scope
- **Location**: `pkg/providers/google/transform.go`
- **Change**:
  Ensure that for multi-tool parallel calls generated in a single turn, if only the first `functionCall` received a `thought_signature`, subsequent sibling calls in the same turn are handled gracefully without requiring fabricated signatures if Gemini rejects duplicates.

---

## 4. Verification Plan

1. **Unit Tests**:
   - Verify trailing turn normalization outputs `Part{Text: "."}`.
   - Verify `supportsToolCallID` returns true for `gemini-3.7-flash`, `gemini-3.8-flash`, `gemini-2.5-pro`, and false for `gemini-1.5-pro`, `gemini-1.5-flash`.
   - Ensure `ToGoogleRequest` strips `id` for `gemini-1.5-flash`.
2. **End-to-End Simulation**:
   - Run multi-turn conversation with Claude Code executing consecutive bash tools.
   - Verify zero 400 errors on `gemini-3.7-flash` and `gemini-1.5-flash`.
