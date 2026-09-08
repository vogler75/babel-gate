package router

import (
	"context"
	"fmt"
	"sort"
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

// ListAll returns all accessible models (native provider models + aliases),
// ordered by provider priority ascending and then model ID.
func (c *Catalog) ListAll(ctx context.Context) ([]CatalogModel, error) {
	var results []CatalogModel
	seen := make(map[string]bool)

	providersMap := c.engine.GetProviders()

	// 1. Sort provider names by priority ascending so higher-priority providers take precedence
	provNames := make([]string, 0, len(providersMap))
	for name := range providersMap {
		provNames = append(provNames, name)
	}
	sort.Slice(provNames, func(i, j int) bool {
		pI := c.engine.GetProviderPriority(provNames[i])
		pJ := c.engine.GetProviderPriority(provNames[j])
		if pI != pJ {
			return pI < pJ
		}
		return provNames[i] < provNames[j]
	})

	// 2. Query each active provider in priority order
	for _, name := range provNames {
		p := providersMap[name]
		models, err := p.ListModels(ctx)
		if err != nil {
			// If upstream call fails, log or provide graceful fallback
			continue
		}
		c.engine.SyncProviderModels(name, models)
		for _, m := range models {
			cleanID := strings.TrimPrefix(m.ID, name+"/")
			key := name + ":" + cleanID
			if !seen[key] {
				seen[key] = true
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

	// 3. Add configured aliases
	routes := c.engine.GetRoutes()
	aliasNames := make([]string, 0, len(routes))
	for alias := range routes {
		aliasNames = append(aliasNames, alias)
	}
	sort.Strings(aliasNames)
	for _, alias := range aliasNames {
		target := routes[alias]
		key := "router-alias:" + alias
		if !seen[key] {
			seen[key] = true
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

	// 4. Sort results by provider priority ascending, then provider name, then model name
	sort.Slice(results, func(i, j int) bool {
		pI := 999
		if results[i].Type == "native" {
			pI = c.engine.GetProviderPriority(results[i].Provider)
		}
		pJ := 999
		if results[j].Type == "native" {
			pJ = c.engine.GetProviderPriority(results[j].Provider)
		}
		if pI != pJ {
			return pI < pJ
		}
		if results[i].Provider != results[j].Provider {
			return results[i].Provider < results[j].Provider
		}
		nameI := strings.ToLower(results[i].ID)
		nameJ := strings.ToLower(results[j].ID)
		return nameI < nameJ
	})

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
