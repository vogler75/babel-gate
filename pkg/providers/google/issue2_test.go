package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vogler75/babel-gate/pkg/canonical"
)

func collectGoogleEvents(t *testing.T, ch <-chan canonical.CanonicalEvent) []canonical.CanonicalEvent {
	t.Helper()
	var events []canonical.CanonicalEvent
	for ev := range ch {
		events = append(events, ev)
	}
	return events
}

func googleStreamClient(server *httptest.Server, timeouts Timeouts) *Client {
	return NewClientWithTimeouts("google", "test", server.URL, nil, server.Client(), timeouts)
}

func TestIssue2GoogleSSEFramingAndCompletion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": keepalive\r\n")
		fmt.Fprint(w, "event: message\r\n")
		fmt.Fprint(w, "data:{\"candidates\":[\r\n")
		fmt.Fprint(w, "data: {\"index\":0,\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}\r\n")
		fmt.Fprint(w, "data: ]}\r\n\r\n")
	}))
	defer server.Close()

	client := googleStreamClient(server, Timeouts{ResponseHeader: time.Second, StreamIdle: time.Second})
	ch, err := client.Stream(context.Background(), &canonical.CanonicalRequest{Model: "gemini-test", Messages: []canonical.Message{{Role: canonical.RoleUser, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: "hi"}}}}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := collectGoogleEvents(t, ch)
	var text, finish string
	var done bool
	for _, ev := range events {
		if ev.Type == canonical.EventError {
			t.Fatalf("unexpected stream error: %v", ev.Error)
		}
		if ev.Type == canonical.EventTextDelta {
			text += ev.Text
		}
		if ev.Type == canonical.EventMessageDelta && ev.FinishReason != "" {
			finish = ev.FinishReason
		}
		done = done || ev.Type == canonical.EventMessageDone
	}
	if text != "ok" || finish != "stop" || !done {
		t.Fatalf("text=%q finish=%q done=%v events=%+v", text, finish, done, events)
	}
}

func TestIssue2GoogleToolCallStopAcrossFrames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"candidates":[{"index":0,"content":{"parts":[{"functionCall":{"name":"tool","args":{}}}]}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"candidates":[{"index":0,"finishReason":"STOP"}]}`+"\n\n")
	}))
	defer server.Close()
	client := googleStreamClient(server, Timeouts{ResponseHeader: time.Second, StreamIdle: time.Second})
	ch, err := client.Stream(context.Background(), &canonical.CanonicalRequest{Model: "gemini-test"})
	if err != nil {
		t.Fatal(err)
	}
	events := collectGoogleEvents(t, ch)
	var finish string
	for _, ev := range events {
		if ev.Type == canonical.EventMessageDelta {
			finish = ev.FinishReason
		}
	}
	if finish != "tool_calls" || events[len(events)-1].Type != canonical.EventMessageDone {
		t.Fatalf("tool call STOP was not preserved: %+v", events)
	}
}

func TestIssue2GooglePrematureEOFIsError(t *testing.T) {
	tests := map[string]string{
		"empty":   "",
		"partial": `data:{"candidates":[{"index":0,"content":{"parts":[{"text":"partial"}]}}]}` + "\n\n",
		"usage":   `data:{"usageMetadata":{"promptTokenCount":4,"totalTokenCount":4}}` + "\n\n",
		"unknown": `data:{}` + "\n\n",
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, payload)
			}))
			defer server.Close()
			client := googleStreamClient(server, Timeouts{ResponseHeader: time.Second, StreamIdle: time.Second})
			ch, err := client.Stream(context.Background(), &canonical.CanonicalRequest{Model: "gemini-test", Messages: []canonical.Message{{Role: canonical.RoleUser, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: "hi"}}}}})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			events := collectGoogleEvents(t, ch)
			var gotError, gotDone bool
			for _, ev := range events {
				gotError = gotError || ev.Type == canonical.EventError
				gotDone = gotDone || ev.Type == canonical.EventMessageDone
			}
			if !gotError || gotDone {
				t.Fatalf("expected error without message_done, got %+v", events)
			}
		})
	}
}

func TestIssue2GoogleTimeouts(t *testing.T) {
	t.Run("response headers", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(time.Second):
			}
		}))
		defer server.Close()
		client := googleStreamClient(server, Timeouts{ResponseHeader: 20 * time.Millisecond, StreamIdle: time.Second})
		_, err := client.Stream(context.Background(), &canonical.CanonicalRequest{Model: "gemini-test"})
		if !errors.Is(err, ErrResponseHeaderTimeout) {
			t.Fatalf("expected response-header timeout, got %v", err)
		}
	})

	t.Run("between events", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		defer server.Close()
		client := googleStreamClient(server, Timeouts{ResponseHeader: time.Second, StreamIdle: 20 * time.Millisecond})
		ch, err := client.Stream(context.Background(), &canonical.CanonicalRequest{Model: "gemini-test"})
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		events := collectGoogleEvents(t, ch)
		if len(events) != 1 || events[0].Type != canonical.EventError || !errors.Is(events[0].Error, ErrStreamIdleTimeout) {
			t.Fatalf("expected idle timeout event, got %+v", events)
		}
	})

	t.Run("overall generation", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, `data:{"candidates":[{"index":0,"content":{"parts":[{"text":"working"}]}}]}`+"\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		defer server.Close()
		client := googleStreamClient(server, Timeouts{ResponseHeader: time.Second, StreamIdle: time.Second, Generation: 20 * time.Millisecond})
		ch, err := client.Stream(context.Background(), &canonical.CanonicalRequest{Model: "gemini-test"})
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		events := collectGoogleEvents(t, ch)
		if len(events) < 2 || events[len(events)-1].Type != canonical.EventError || !errors.Is(events[len(events)-1].Error, ErrGenerationTimeout) {
			t.Fatalf("expected generation timeout event, got %+v", events)
		}
	})
}

func TestIssue2GoogleToolTranslation(t *testing.T) {
	for _, tc := range []struct {
		choice canonical.ToolChoice
		mode   string
		named  bool
	}{
		{canonical.ToolChoice{Mode: "auto"}, "AUTO", false},
		{canonical.ToolChoice{Mode: "none"}, "NONE", false},
		{canonical.ToolChoice{Mode: "required"}, "ANY", false},
		{canonical.ToolChoice{Mode: "named", Name: "invalid/name"}, "ANY", true},
	} {
		req := &canonical.CanonicalRequest{
			Model: "gemini-test", ToolChoice: &tc.choice,
			Tools:    []canonical.ToolDeclaration{{Name: "invalid/name", Parameters: map[string]any{"type": "object"}}},
			Messages: []canonical.Message{{Role: canonical.RoleUser, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: "hi"}}}},
		}
		wire, err := ToGoogleRequest(req)
		if err != nil {
			t.Fatalf("choice %+v: %v", tc.choice, err)
		}
		cfg := wire.ToolConfig.FunctionCallingConfig
		if cfg.Mode != tc.mode {
			t.Fatalf("choice %+v mapped to %q", tc.choice, cfg.Mode)
		}
		if tc.named && (len(cfg.AllowedFunctionNames) != 1 || cfg.AllowedFunctionNames[0] != wire.Tools[0].FunctionDeclarations[0].Name) {
			t.Fatalf("forced name was not normalized consistently: %+v", cfg)
		}
		if wire.Tools[0].FunctionDeclarations[0].Name == "invalid/name" {
			t.Fatal("invalid Google tool name was not normalized")
		}
	}

	events, err := ParseGoogleStreamEvent([]byte(`{"candidates":[{"index":0,"content":{"parts":[{"functionCall":{"name":"f"}}]},"finishReason":"STOP"}]}`), "gemini-test")
	if err != nil {
		t.Fatal(err)
	}
	var args, finish string
	for _, ev := range events {
		if ev.Type == canonical.EventToolCallDelta {
			args = ev.ToolCallArgs
		}
		if ev.Type == canonical.EventMessageDelta {
			finish = ev.FinishReason
		}
	}
	if args != "{}" || finish != "tool_calls" {
		t.Fatalf("zero args/finish mismatch: args=%q finish=%q", args, finish)
	}
}

func TestIssue2GoogleWireAndErrorSemantics(t *testing.T) {
	b, err := json.Marshal(Part{Text: "x", ThoughtSignature: "sig"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"thoughtSignature":"sig"`) || strings.Contains(string(b), "thought_signature") {
		t.Fatalf("wrong signature key: %s", b)
	}

	for _, reason := range []string{"SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "MALFORMED_FUNCTION_CALL"} {
		_, err := ParseGoogleStreamEvent([]byte(fmt.Sprintf(`{"candidates":[{"index":0,"finishReason":%q}]}`, reason)), "gemini-test")
		if err == nil || !strings.Contains(err.Error(), reason) {
			t.Fatalf("reason %s was not preserved in error: %v", reason, err)
		}
	}
	_, err = ParseGoogleStreamEvent([]byte(`{"promptFeedback":{"blockReason":"SAFETY","blockReasonMessage":"blocked prompt"}}`), "gemini-test")
	if err == nil || !strings.Contains(err.Error(), "SAFETY") || !strings.Contains(err.Error(), "blocked prompt") {
		t.Fatalf("prompt block was not diagnostic: %v", err)
	}
}

func TestIssue2GoogleRequestIsolationThinkingAndUsage(t *testing.T) {
	schema := map[string]any{"anyOf": []any{map[string]any{"type": "string"}}}
	before := map[string]any{"anyOf": []any{map[string]any{"type": "string"}}}
	budget := 128
	include := true
	req := &canonical.CanonicalRequest{
		Model: "gemini-test", SessionID: "scope-a",
		Tools:    []canonical.ToolDeclaration{{Name: "tool", Parameters: schema}},
		Thinking: &canonical.ThinkingConfig{Type: "enabled", BudgetTokens: &budget, Level: "HIGH", IncludeThoughts: &include},
		Messages: []canonical.Message{{Role: canonical.RoleUser, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: "hi"}}}},
	}
	wire, err := ToGoogleRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(schema, before) {
		t.Fatalf("canonical schema mutated: before=%v after=%v", before, schema)
	}
	if wire.GenerationConfig == nil || wire.GenerationConfig.ThinkingConfig == nil || wire.GenerationConfig.ThinkingConfig.ThinkingBudget == nil || *wire.GenerationConfig.ThinkingConfig.ThinkingBudget != budget || wire.GenerationConfig.ThinkingConfig.ThinkingLevel != "HIGH" {
		t.Fatalf("thinking controls lost: %+v", wire.GenerationConfig)
	}
	googleHistory, err := ToGoogleRequest(&canonical.CanonicalRequest{
		Model: "gemini-test",
		Messages: []canonical.Message{
			{Role: canonical.RoleUser, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: "question"}}},
			{Role: canonical.RoleAssistant, Parts: []canonical.ContentPart{{Type: canonical.PartThinking, Thinking: "thought", ThoughtSignature: "google-signature", ThoughtSignatureProvider: "google"}}},
			{Role: canonical.RoleUser, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: "continue"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := googleHistory.Contents[1].Parts[0].ThoughtSignature; got != "google-signature" {
		t.Fatalf("Google signed thinking history was lost: %q", got)
	}
	googleHistory, err = ToGoogleRequest(&canonical.CanonicalRequest{
		Model: "gemini-test",
		Messages: []canonical.Message{
			{Role: canonical.RoleUser, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: "question"}}},
			{Role: canonical.RoleAssistant, Parts: []canonical.ContentPart{{Type: canonical.PartThinking, Thinking: "thought", ThoughtSignature: "anthropic-signature", ThoughtSignatureProvider: "anthropic"}}},
			{Role: canonical.RoleUser, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: "continue"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := googleHistory.Contents[1].Parts[0].ThoughtSignature; got != "" {
		t.Fatalf("foreign signed thinking history was forwarded unsafely: %q", got)
	}

	var stores sync.WaitGroup
	stores.Add(2)
	go func() {
		defer stores.Done()
		StoreThoughtSignatureForScope("scope-a", "same-call", "sig-a")
	}()
	go func() {
		defer stores.Done()
		StoreThoughtSignatureForScope("scope-b", "same-call", "sig-b")
	}()
	stores.Wait()
	if GetThoughtSignatureForScope("scope-a", "same-call") != "sig-a" || GetThoughtSignatureForScope("scope-b", "same-call") != "sig-b" || GetThoughtSignatureForScope("scope-c", "same-call") != "" {
		t.Fatal("thought signatures leaked across scopes")
	}

	foreignResponse, err := ToGoogleResponse(&canonical.CanonicalResponse{Message: canonical.Message{
		Role: canonical.RoleAssistant,
		Parts: []canonical.ContentPart{{
			Type: canonical.PartToolCall, ToolCallID: "call", ToolCallName: "tool", ToolCallArgs: `{}`,
			ThoughtSignature: "anthropic-signature", ThoughtSignatureProvider: "anthropic",
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := foreignResponse.Candidates[0].Content.Parts[0].ThoughtSignature; got != "skip_thought_signature_validator" {
		t.Fatalf("foreign response signature was forwarded to Google: %q", got)
	}
	knownUnsignedResponse, err := ToGoogleResponse(&canonical.CanonicalResponse{Message: canonical.Message{
		Role: canonical.RoleAssistant,
		Parts: []canonical.ContentPart{{
			Type: canonical.PartToolCall, ToolCallID: "parallel-second", ToolCallName: "tool", ToolCallArgs: `{}`,
			ThoughtSignatureProvider: "google",
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := knownUnsignedResponse.Candidates[0].Content.Parts[0].ThoughtSignature; got != "" {
		t.Fatalf("known unsigned parallel call gained a signature: %q", got)
	}

	usage := canonicalGoogleUsage(&UsageMetadata{PromptTokenCount: 10, CandidatesTokenCount: 8, TotalTokenCount: 18, CachedContentTokenCount: 4, ThoughtsTokenCount: 6})
	if usage.CacheReadInputTokens != 4 || usage.ReasoningTokens != 6 || usage.CompletionTokens != 8 {
		t.Fatalf("usage detail lost: %+v", usage)
	}
}

func TestIssue2ParallelGoogleCallsPreserveSignaturePlacement(t *testing.T) {
	const scope = "parallel-signature-scope"
	response := &GenerateContentResponse{Candidates: []Candidate{{
		Content: Content{Role: "model", Parts: []Part{
			{FunctionCall: &FunctionCall{ID: "first", Name: "one", Args: map[string]any{}}, ThoughtSignature: "signed-first"},
			{FunctionCall: &FunctionCall{ID: "second", Name: "two", Args: map[string]any{}}},
		}},
		FinishReason: "STOP",
	}}}
	canonicalResponse, err := fromGoogleResponse(response, "gemini-test", nil, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(canonicalResponse.Message.Parts) != 2 || canonicalResponse.Message.Parts[0].ThoughtSignature != "signed-first" || canonicalResponse.Message.Parts[1].ThoughtSignature != "" {
		t.Fatalf("parallel call signature placement changed: %+v", canonicalResponse.Message.Parts)
	}

	request := &canonical.CanonicalRequest{
		Model: "gemini-test", SessionID: scope,
		Messages: []canonical.Message{
			{Role: canonical.RoleUser, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: "run both"}}},
			{Role: canonical.RoleAssistant, Parts: []canonical.ContentPart{
				{Type: canonical.PartToolCall, ToolCallID: "first", ToolCallName: "one", ToolCallArgs: `{}`},
				{Type: canonical.PartToolCall, ToolCallID: "second", ToolCallName: "two", ToolCallArgs: `{}`},
			}},
			{Role: canonical.RoleTool, Parts: []canonical.ContentPart{
				{Type: canonical.PartToolResult, ToolResultID: "first", ToolResultContent: "ok"},
				{Type: canonical.PartToolResult, ToolResultID: "second", ToolResultContent: "ok"},
			}},
		},
	}
	wire, err := ToGoogleRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	modelParts := wire.Contents[1].Parts
	if len(modelParts) != 2 || modelParts[0].ThoughtSignature != "signed-first" || modelParts[1].ThoughtSignature != "" {
		t.Fatalf("parallel call signatures were not replayed exactly: %+v", modelParts)
	}
}

func TestIssue2GoogleRejectsFabricatedToolResults(t *testing.T) {
	_, err := ToGoogleRequest(&canonical.CanonicalRequest{
		Model: "gemini-test",
		Messages: []canonical.Message{
			{Role: canonical.RoleUser, Parts: []canonical.ContentPart{{Type: canonical.PartText, Text: "run"}}},
			{Role: canonical.RoleAssistant, Parts: []canonical.ContentPart{{Type: canonical.PartToolCall, ToolCallID: "call", ToolCallName: "tool", ToolCallArgs: `{}`}}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "unanswered") {
		t.Fatalf("expected malformed history error, got %v", err)
	}
}
