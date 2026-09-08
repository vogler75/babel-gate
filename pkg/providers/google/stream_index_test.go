package google

import (
	"context"
	"fmt"
	"github.com/vogler75/babel-gate/pkg/canonical"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStreamToolIndexesAcrossChunks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 2; i++ {
			fmt.Fprint(w, "data: {\"candidates\":[{\"index\":2,\"content\":{\"role\":\"model\",\"parts\":[{\"functionCall\":{\"name\":\"add\",\"args\":{}}}]}}]}\n\n")
		}
		fmt.Fprint(w, "data: {\"candidates\":[{\"index\":2,\"finishReason\":\"STOP\"}]}\n\n")
	}))
	defer srv.Close()
	ch, err := NewClient("test", "", srv.URL, nil, srv.Client()).Stream(context.Background(), &canonical.CanonicalRequest{Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	indexes := map[int]bool{}
	for ev := range ch {
		if ev.Error != nil {
			t.Fatal(ev.Error)
		}
		if ev.Type == canonical.EventToolCallStart {
			if ev.CandidateIndex != 2 || ids[ev.ToolCallID] || indexes[ev.Index] {
				t.Fatalf("call collision: %+v", ev)
			}
			ids[ev.ToolCallID] = true
			indexes[ev.Index] = true
		}
	}
	if len(ids) != 2 {
		t.Fatalf("lost calls: %v", ids)
	}
}
