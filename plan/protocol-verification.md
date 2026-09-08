# Protocol implementation verification

Implementation and verification date: 2026-09-08.

## Implemented behavior

1. **Google streaming tools:** request-local argument accumulation preserves parallel calls, IDs, names, and signatures. Explicit tool completion emits a complete JSON object once. A finish reason flushes pending calls before the finish chunk; message completion also flushes calls when no finish reason arrived. Malformed, null, non-object, or truncated argument JSON produces a stream error rather than `{}`. An unconfirmed EOF does not flush pending arguments as successful calls.
2. **Google result correlation:** explicit IDs take precedence. ID-less results resolve against unresolved preceding calls of the same name only when exactly one match exists. Missing and ambiguous matches are request errors. Generated IDs are unique within the request and avoid supplied IDs; conversion preserves text/result ordering.
3. **Stream errors:** OpenAI and Copilot share a decoder and SSE reader. Non-null error envelopes and malformed chunks terminate with an error. Usage-only chunks remain valid. A finish reason or `[DONE]` is required for successful completion; bare EOF is an error. Cancellation closes the response body and unblocks channel sends. Inbound handlers cancel their upstream context on return. Valid Azure prompt-filter metadata is accepted without generating content.
4. **Reasoning:** compatible-provider `reasoning_content` and `reasoning` strings become thinking deltas. A nonempty `reasoning_content` takes precedence within a chunk. Google emits thought parts, Anthropic emits thinking blocks, and the OpenAI-compatible inbound endpoint emits `reasoning_content`. Reasoning remains separate from answer text; no signatures are invented.
5. **Tool names:** OpenAI, Copilot, and Anthropic use deterministic, request-local forward/reverse mappings. Valid names are reserved first. Invalid names receive a sanitized prefix and hash suffix, with collision resolution. Declarations, history, and named selection use the same mapping. Streaming and non-streaming responses restore original names. Provider calls do not mutate the reusable request. Anthropic direct passthrough also normalizes names while preserving other payload fields and signed blocks.
6. **Response IDs:** missing non-streaming OpenAI response IDs use `crypto/rand.Text`; supplied IDs are retained.
7. **Anthropic parallel tools:** complete tool blocks are emitted sequentially, preserving the identity and arguments of interleaved upstream calls. Tool arguments are buffered until completion; text and reasoning still stream.
8. **Candidate indexes:** canonical events carry `CandidateIndex` separately from tool/content `Index`. Google and OpenAI output preserve candidate identity. Google assigns distinct tool indexes across chunks. Anthropic's single-message output rejects nonzero candidates instead of merging them.

## Regression coverage

| Requirement | Tests |
| --- | --- |
| Fragmentation, interleaving, explicit/message completion, duplicate stops, empty objects, malformed/truncated arguments, EOF, upstream errors | `pkg/server/inbound/tool_stream_test.go` |
| Provider-to-inbound tools, reasoning/text, block closure, error delivery, multiple candidates | `pkg/server/inbound/protocol_stream_test.go` |
| Explicit/reordered IDs, repeated function names, unambiguous/ambiguous ID-less results, conversion round trip | `pkg/providers/google/correlation_test.go` |
| Distinct Google calls split across chunks | `pkg/providers/google/stream_index_test.go` |
| Error envelopes, malformed schemas, usage-only chunks, Azure metadata | `pkg/providers/openai/stream_errors_test.go` |
| Both OpenAI/Copilot HTTP clients: first/partial errors, malformed JSON, EOF, usage, finish without sentinel, cancellation under backpressure | `pkg/providers/openai/stream_http_test.go` |
| Reasoning aliases and precedence | `pkg/providers/openai/reasoning_test.go` |
| Invalid/long/colliding/valid names, concurrent request mappings, history, named choice | `pkg/providers/toolnames/names_test.go` |
| All three clients: HTTP request mapping, streaming/non-streaming name restoration, request immutability | `pkg/providers/openai/names_http_test.go` |
| Anthropic passthrough preserves unknown fields, exact numeric values, signatures, and restores response names | `pkg/providers/anthropic/passthrough_names_test.go`, `pkg/server/inbound/protocol_stream_test.go` |
| Distinct fallback IDs and preserved upstream IDs | `pkg/providers/openai/response_id_test.go` |

The correlation, fallback-ID, error-envelope, reasoning-alias, and observed Azure-metadata regression tests were run failing before their corresponding fixes.

## Protocol sources

Constraints were checked on 2026-09-08. Both current function-tool guides specify ASCII letters, digits, underscores, and dashes, with a 64-character maximum. Constraints are supplied separately by each provider.

- [OpenAI Chat Completions function tool schema](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create)
- [Anthropic client tool definitions](https://platform.claude.com/docs/en/agents-and-tools/tool-use/define-tools)
- [DeepSeek streaming reasoning_content example](https://api-docs.deepseek.com/guides/thinking_mode_api_example_streaming/)
- [OpenRouter reasoning field](https://openrouter.ai/docs/guides/best-practices/reasoning-tokens)
- [Azure streaming prompt annotations and filter metadata](https://learn.microsoft.com/en-us/azure/ai-foundry/openai/concepts/content-streaming?view=foundry-classic)

## Verification results

`go test ./...`, `go vet ./...`, and `go test -race ./...` all passed after the final implementation changes. Tests require local listening sockets; they were run with sandbox escalation because `httptest` cannot bind inside the restricted sandbox.

Live checks use only a synthetic addition request: call the supplied tool with `a=2`, `b=3`, return its result with the streamed call ID, then verify the final answer includes `5`. The entry point is Google inbound streaming followed by Google inbound non-streaming, exercising both translation directions. No credentials or private conversations are recorded.

- Google / `gemini-2.5-flash`: passed streaming arguments, IDs, and full result round trip.
- Anthropic / `claude-haiku-4-5@20251001`: passed streaming arguments, IDs, original-name restoration (`math.add`), and full result round trip.
- OpenAI gateway / `gpt-5-mini`: passed streaming arguments, IDs, original-name restoration (`math.add`), and full result round trip. The initial check identified an Azure prompt-filter metadata chunk with empty choices; a sanitized regression fixture and decoder fix preceded the successful repeat.
- Copilot / `gpt-4.1`: passed streaming arguments, IDs, original-name restoration (`math.add`), and full result round trip. An earlier attempt with catalog-listed `gpt-5.6-luna` returned `model_not_supported`; no compatibility change was inferred from that model restriction.

These outcomes establish the listed cases only, not universal compatibility across providers or models. Alternate Copilot response shapes, Gemini continuation changes, and signature-cache redesign remain behind the original plan's evidence gates.

Subsequent authentication diagnostic on 2026-09-08: the currently configured gateway key returned HTTP 401 `ApiKeyNotApproved` for both `gemini-2.5-flash` and `gemini-3.8-flash`. Google key selection, authentication headers, and URL construction are unchanged by this implementation. The earlier successful live checks describe their recorded runs, not current key approval.
