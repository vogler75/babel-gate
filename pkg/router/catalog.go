package router

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type CatalogModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Provider    string `json:"provider"`
	Type        string `json:"type"` // "native" or "alias"
	Target      string `json:"target,omitempty"`
	Description string `json:"description,omitempty"`
}

type Catalog struct {
	engine *Engine
}

func NewCatalog(engine *Engine) *Catalog {
	return &Catalog{engine: engine}
}

// ListAll returns all accessible models (native provider models + aliases).
func (c *Catalog) ListAll(ctx context.Context) ([]CatalogModel, error) {
	var results []CatalogModel
	seen := make(map[string]bool)

	providersMap := c.engine.GetProviders()

	// 1. Query each active provider
	for name, p := range providersMap {
		models, err := p.ListModels(ctx)
		if err != nil {
			// If upstream call fails, log or provide graceful fallback
			continue
		}
		c.engine.SyncProviderModels(name, models)
		for _, m := range models {
			cleanID := strings.TrimPrefix(m.ID, name+"/")
			if !seen[cleanID] {
				seen[cleanID] = true
				results = append(results, CatalogModel{
					ID:          cleanID,
					DisplayName: m.Name,
					Provider:    name,
					Type:        "native",
					Description: m.Description,
				})
			}
		}
	}

	// 2. Add configured aliases
	routes := c.engine.GetRoutes()
	for alias, target := range routes {
		if !seen[alias] {
			seen[alias] = true
			results = append(results, CatalogModel{
				ID:          alias,
				DisplayName: fmt.Sprintf("%s (alias -> %s)", alias, target),
				Provider:    "router-alias",
				Type:        "alias",
				Target:      target,
				Description: fmt.Sprintf("Routes to %s", target),
			})
		}
	}

	return results, nil
}

// FormatOpenAI returns the models list in OpenAI format.
func (c *Catalog) FormatOpenAI(models []CatalogModel) map[string]any {
	var list []map[string]any
	now := time.Now().Unix()
	for _, m := range models {
		list = append(list, map[string]any{
			"id":       m.ID,
			"object":   "model",
			"created":  now,
			"owned_by": m.Provider,
		})
	}
	return map[string]any{
		"object": "list",
		"data":   list,
	}
}

// FormatAnthropic returns the models list in Anthropic format.
func (c *Catalog) FormatAnthropic(models []CatalogModel) map[string]any {
	var list []map[string]any
	for _, m := range models {
		list = append(list, map[string]any{
			"id":           m.ID,
			"type":         "model",
			"display_name": m.DisplayName,
			"created_at":   time.Now().Format(time.RFC3339),
		})
	}
	firstID := ""
	lastID := ""
	if len(list) > 0 {
		firstID = list[0]["id"].(string)
		lastID = list[len(list)-1]["id"].(string)
	}
	return map[string]any{
		"data":     list,
		"has_more": false,
		"first_id": firstID,
		"last_id":  lastID,
	}
}

// FormatGoogle returns the models list in Google Gemini format.
func (c *Catalog) FormatGoogle(models []CatalogModel) map[string]any {
	var list []map[string]any
	for _, m := range models {
		cleanID := strings.TrimPrefix(m.ID, "models/")
		list = append(list, map[string]any{
			"name":                       "models/" + cleanID,
			"displayName":                m.DisplayName,
			"description":                m.Description,
			"supportedGenerationMethods": []string{"generateContent", "countTokens"},
		})
	}
	return map[string]any{
		"models": list,
	}
}
