// Package toolnames implements request-scoped reversible tool names.
package toolnames

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	"github.com/vogler75/babel-gate/pkg/canonical"
)

// Constraints are supplied by each target protocol, independently.
type Constraints struct {
	MaxLength int
	Allowed   func(rune) bool
}

func ASCII(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-'
}

type Mapping struct{ forward, reverse map[string]string }

func (m *Mapping) Original(name string) string {
	if original, ok := m.reverse[name]; ok {
		return original
	}
	return name
}

// Normalize copies every slice/struct it edits; the caller's request is reusable.
// Valid names are reserved first, so they cannot be displaced by a normalized name.
func Normalize(req *canonical.CanonicalRequest, c Constraints) (*canonical.CanonicalRequest, *Mapping) {
	m := &Mapping{forward: map[string]string{}, reverse: map[string]string{}}
	names := map[string]bool{}
	for _, t := range req.Tools {
		names[t.Name] = true
	}
	for _, msg := range req.Messages {
		for _, p := range msg.Parts {
			if p.Type == canonical.PartToolCall {
				names[p.ToolCallName] = true
			}
		}
	}
	if req.ToolChoice != nil && req.ToolChoice.Name != "" {
		names[req.ToolChoice.Name] = true
	}
	var invalid []string
	for name := range names {
		valid := name != "" && len(name) <= c.MaxLength
		for _, r := range name {
			valid = valid && c.Allowed(r)
		}
		if valid {
			m.forward[name] = name
			m.reverse[name] = name
		} else {
			invalid = append(invalid, name)
		}
	}
	sort.Strings(invalid)
	for _, name := range invalid {
		base := strings.Map(func(r rune) rune {
			if c.Allowed(r) {
				return r
			}
			return '_'
		}, name)
		if base == "" {
			base = "tool"
		}
		for n := 0; ; n++ {
			sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", name, n)))
			suffix := fmt.Sprintf("_%x", sum[:8])
			prefix := base
			if len(prefix) > c.MaxLength-len(suffix) {
				prefix = prefix[:c.MaxLength-len(suffix)]
			}
			mapped := prefix + suffix
			if _, exists := m.reverse[mapped]; !exists {
				m.forward[name] = mapped
				m.reverse[mapped] = name
				break
			}
		}
	}
	clone := *req
	clone.Tools = append([]canonical.ToolDeclaration(nil), req.Tools...)
	for i := range clone.Tools {
		clone.Tools[i].Name = m.forward[clone.Tools[i].Name]
	}
	clone.Messages = append([]canonical.Message(nil), req.Messages...)
	for i := range clone.Messages {
		clone.Messages[i].Parts = append([]canonical.ContentPart(nil), req.Messages[i].Parts...)
		for j := range clone.Messages[i].Parts {
			p := &clone.Messages[i].Parts[j]
			if p.Type == canonical.PartToolCall {
				p.ToolCallName = m.forward[p.ToolCallName]
			}
		}
	}
	if req.ToolChoice != nil {
		choice := *req.ToolChoice
		if choice.Name != "" {
			choice.Name = m.forward[choice.Name]
		}
		clone.ToolChoice = &choice
	}
	return &clone, m
}
func (m *Mapping) RestoreResponse(resp *canonical.CanonicalResponse, err error) (*canonical.CanonicalResponse, error) {
	if err != nil || resp == nil {
		return resp, err
	}
	for i := range resp.Message.Parts {
		p := &resp.Message.Parts[i]
		if p.Type == canonical.PartToolCall {
			p.ToolCallName = m.Original(p.ToolCallName)
		}
	}
	return resp, nil
}
func (m *Mapping) RestoreStream(ctx context.Context, input <-chan canonical.CanonicalEvent) <-chan canonical.CanonicalEvent {
	out := make(chan canonical.CanonicalEvent, 64)
	go func() {
		defer close(out)
		for {
			var ev canonical.CanonicalEvent
			var ok bool
			select {
			case <-ctx.Done():
				return
			case ev, ok = <-input:
			}
			if !ok {
				return
			}
			if ev.ToolCallName != "" {
				ev.ToolCallName = m.Original(ev.ToolCallName)
			}
			select {
			case <-ctx.Done():
				return
			case out <- ev:
			}
		}
	}()
	return out
}

func (m *Mapping) Name(original string) string {
	if name, ok := m.forward[original]; ok {
		return name
	}
	return original
}
func (m *Mapping) Changed() bool {
	for original, name := range m.forward {
		if original != name {
			return true
		}
	}
	return false
}
