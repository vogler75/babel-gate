package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/vogler75/babel-gate/pkg/config"
	"github.com/vogler75/babel-gate/pkg/metrics"
	"github.com/vogler75/babel-gate/pkg/router"
	"github.com/vogler75/babel-gate/pkg/session"
)

type DashboardHandler struct {
	engine   *router.Engine
	catalog  *router.Catalog
	sessions *session.Manager
	metrics  *metrics.Store
}

func NewDashboardHandler(engine *router.Engine, catalog *router.Catalog, sessions *session.Manager, metrics *metrics.Store) *DashboardHandler {
	return &DashboardHandler{
		engine:   engine,
		catalog:  catalog,
		sessions: sessions,
		metrics:  metrics,
	}
}

func (d *DashboardHandler) HandleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(dashboardHTML))
}

func (d *DashboardHandler) HandleSetup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(setupHTML))
}

func (d *DashboardHandler) HandleAPIStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "healthy",
		"providers": d.engine.GetProviderStates(),
		"routes":    d.engine.GetRoutes(),
	})
}

func (d *DashboardHandler) HandleAPIProvider(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", http.MethodPut)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/providers/")
	if name == "" || strings.Contains(name, "/") {
		http.Error(w, "invalid provider name", http.StatusBadRequest)
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Enabled == nil {
		http.Error(w, "body must contain an enabled boolean", http.StatusBadRequest)
		return
	}
	persisted, err := d.engine.SetProviderEnabled(name, *body.Enabled)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "enabled": *body.Enabled, "persisted": persisted})
}

func (d *DashboardHandler) HandleAPIRouting(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(d.engine.GetRouting())
		return
	}
	if r.Method == http.MethodPost {
		if err := d.engine.ReloadRouting(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"routing": d.engine.GetRouting(), "reloaded": true})
		return
	}
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPut+", "+http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var routing config.RoutingConfig
	if err := json.NewDecoder(r.Body).Decode(&routing); err != nil {
		http.Error(w, "invalid routing JSON", http.StatusBadRequest)
		return
	}
	if routing.Routes == nil {
		routing.Routes = make(map[string]string)
	}
	if routing.Fallbacks == nil {
		routing.Fallbacks = make(map[string][]string)
	}
	persisted, err := d.engine.SetRouting(routing)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"routing": d.engine.GetRouting(), "persisted": persisted})
}

func (d *DashboardHandler) HandleAPIModels(w http.ResponseWriter, r *http.Request) {
	models, err := d.catalog.ListAll(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"models": models,
	})
}

func (d *DashboardHandler) HandleAPISessions(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		d.HandleAPIClearSessions(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if d.sessions == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"summary":  session.Summary{},
			"sessions": []*session.Session{},
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"summary":  d.sessions.GetSummary(),
		"sessions": d.sessions.ListSessions(),
	})
}

func (d *DashboardHandler) HandleAPIClearSessions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if d.sessions == nil {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "noop"})
		return
	}

	id := r.URL.Query().Get("id")
	if id != "" {
		deleted := d.sessions.DeleteSession(id)
		if deleted {
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "deleted", "id": id})
		} else {
			http.Error(w, "session not found", http.StatusNotFound)
		}
		return
	}

	d.sessions.Clear()
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "cleared"})
}

func (d *DashboardHandler) HandleAPIMetricsSummary(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if d.metrics == nil {
		_ = json.NewEncoder(w).Encode(&metrics.MetricsSummary{})
		return
	}

	startStr := r.URL.Query().Get("start")
	endStr := r.URL.Query().Get("end")
	provider := r.URL.Query().Get("provider")

	now := time.Now().UTC()
	start := now.AddDate(0, 0, -6)
	end := now

	if startStr != "" {
		if t, err := time.Parse("2006-01-02", startStr); err == nil {
			start = t
		}
	}
	if endStr != "" {
		if t, err := time.Parse("2006-01-02", endStr); err == nil {
			end = t
		}
	}

	summary, err := d.metrics.GetSummary(start, end, provider)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_ = json.NewEncoder(w).Encode(summary)
}

func (d *DashboardHandler) HandleAPIMetricsDaily(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if d.metrics == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"days": []any{}})
		return
	}

	startStr := r.URL.Query().Get("start")
	endStr := r.URL.Query().Get("end")
	provider := r.URL.Query().Get("provider")

	now := time.Now().UTC()
	start := now.AddDate(0, 0, -6)
	end := now

	if startStr != "" {
		if t, err := time.Parse("2006-01-02", startStr); err == nil {
			start = t
		}
	}
	if endStr != "" {
		if t, err := time.Parse("2006-01-02", endStr); err == nil {
			end = t
		}
	}

	days, err := d.metrics.GetDailyMetrics(start, end, provider)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"days":       days,
		"start_date": start.Format("2006-01-02"),
		"end_date":   end.Format("2006-01-02"),
		"provider":   provider,
	})
}

func (d *DashboardHandler) HandleAPIMetricsHourly(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if d.metrics == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"hours": []any{}})
		return
	}

	dateStr := r.URL.Query().Get("date")
	provider := r.URL.Query().Get("provider")

	day := time.Now().UTC()
	if dateStr != "" {
		if t, err := time.Parse("2006-01-02", dateStr); err == nil {
			day = t
		}
	}

	hours, err := d.metrics.GetHourlyMetrics(day, provider)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"date":     day.Format("2006-01-02"),
		"hours":    hours,
		"provider": provider,
	})
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>BabelGate - Any-to-Any LLM Gateway</title>
  <style>
    :root {
      --bg: #0d1117;
      --card-bg: #161b22;
      --border: #30363d;
      --text: #c9d1d9;
      --text-bright: #f0f6fc;
      --primary: #2f81f7;
      --primary-hover: #58a6ff;
      --accent: #238636;
      --badge-bg: #21262d;
      --code-bg: #090d13;
      --stat-in: #58a6ff;
      --stat-out: #bc8cff;
      --stat-total: #3fb950;
    }
    * { box-sizing: border-box; margin: 0; padding: 0; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif; }
    body { background-color: var(--bg); color: var(--text); padding: 2rem; line-height: 1.5; }
    .container { max-width: 100%; margin: 0 auto; }
    header { display: flex; justify-content: space-between; align-items: center; margin-bottom: 1.5rem; border-bottom: 1px solid var(--border); padding-bottom: 1rem; }
    h1 { color: var(--text-bright); font-size: 1.8rem; font-weight: 600; display: flex; align-items: center; gap: 0.5rem; }
    .status-badge { background: #1f6feb22; color: #58a6ff; border: 1px solid #1f6feb; font-size: 0.8rem; padding: 0.2rem 0.6rem; border-radius: 12px; font-weight: 500; }
    
    /* Stats Metrics Grid */
    .stats-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(190px, 1fr)); gap: 1rem; margin-bottom: 1.75rem; }
    .stat-card { background: var(--card-bg); border: 1px solid var(--border); border-radius: 8px; padding: 1rem 1.25rem; }
    .stat-title { font-size: 0.75rem; color: #8b949e; text-transform: uppercase; font-weight: 600; letter-spacing: 0.04em; margin-bottom: 0.35rem; }
    .stat-val { font-size: 1.7rem; font-weight: 700; color: var(--text-bright); }
    .stat-in { color: var(--stat-in); }
    .stat-out { color: var(--stat-out); }
    .stat-total { color: var(--stat-total); }

    .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(320px, 1fr)); gap: 1.5rem; margin-bottom: 1.75rem; }
    .card { background: var(--card-bg); border: 1px solid var(--border); border-radius: 8px; padding: 1.25rem; }
    .card h2 { font-size: 1.15rem; color: var(--text-bright); margin-bottom: 1rem; display: flex; align-items: center; justify-content: space-between; }
    .badge { background: var(--badge-bg); border: 1px solid var(--border); border-radius: 4px; padding: 0.2rem 0.5rem; font-size: 0.75rem; color: #8b949e; }
    .pill { display: inline-block; padding: 0.2rem 0.55rem; border-radius: 4px; font-size: 0.75rem; font-weight: 600; text-transform: uppercase; letter-spacing: 0.02em; }
    .pill-copilot { background: #23863622; color: #3fb950; border: 1px solid #238636; }
    .pill-openai { background: #10a37f22; color: #10a37f; border: 1px solid #10a37f; }
    .pill-anthropic { background: #cc785c22; color: #cc785c; border: 1px solid #cc785c; }
    .pill-google { background: #1a73e822; color: #58a6ff; border: 1px solid #388bfd; }
    .pill-azure { background: #0078d422; color: #38bdf8; border: 1px solid #0284c7; }
    .pill-mistral { background: #ea580c22; color: #fb923c; border: 1px solid #f97316; }
    .pill-cohere { background: #14b8a622; color: #2dd4bf; border: 1px solid #14b8a6; }
    .pill-alias { background: #a371f722; color: #bc8cff; border: 1px solid #8957e5; }
    .pill-default { background: #388bfd1a; color: #79c0ff; border: 1px solid #388bfd66; }
    .model-pct { font-size: 0.72rem; opacity: 0.9; margin-left: 0.3rem; font-weight: 700; background: rgba(0,0,0,0.25); padding: 0.05rem 0.25rem; border-radius: 3px; }
    .last-tag { display: inline-block; background: #238636; color: #ffffff; border-radius: 3px; font-size: 0.65rem; padding: 0.05rem 0.3rem; margin-left: 0.35rem; line-height: 1.2; font-weight: 700; letter-spacing: 0.02em; vertical-align: middle; }
    
    table { width: 100%; border-collapse: collapse; margin-top: 0.5rem; }
    th, td { text-align: left; padding: 0.65rem 0.75rem; border-bottom: 1px solid var(--border); font-size: 0.88rem; }
    th { color: #8b949e; font-weight: 500; }
    tr:last-child td { border-bottom: none; }
    code { font-family: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace; font-size: 0.85em; background: var(--code-bg); padding: 0.15rem 0.35rem; border-radius: 4px; color: #79c0ff; }
    pre { background: var(--code-bg); border: 1px solid var(--border); border-radius: 6px; padding: 1rem; overflow-x: auto; font-family: monospace; font-size: 0.85rem; color: #e6edf3; margin-top: 0.5rem; }
    
    .interactive-panel { margin-top: 1.75rem; background: var(--card-bg); border: 1px solid var(--border); border-radius: 8px; padding: 1.5rem; }
    .form-group { margin-bottom: 1rem; }
    label { display: block; font-size: 0.85rem; font-weight: 500; color: #8b949e; margin-bottom: 0.4rem; }
    select, input, textarea { width: 100%; background: var(--code-bg); border: 1px solid var(--border); border-radius: 6px; color: var(--text-bright); padding: 0.6rem; font-size: 0.9rem; outline: none; }
    select:focus, input:focus, textarea:focus { border-color: var(--primary); }
    button { background: var(--accent); color: white; border: none; padding: 0.6rem 1.2rem; border-radius: 6px; font-size: 0.9rem; font-weight: 600; cursor: pointer; transition: background 0.15s; }
    button:hover { background: #2ea043; }
    .btn-sm { background: #21262d; border: 1px solid var(--border); color: #c9d1d9; padding: 0.35rem 0.75rem; font-size: 0.78rem; font-weight: 500; border-radius: 6px; cursor: pointer; }
    .btn-sm:hover { background: #30363d; color: var(--text-bright); }
    .btn-outline { background: transparent; border: 1px solid var(--border); color: #8b949e; }
    .btn-outline:hover { background: #21262d; color: #f85149; border-color: #f85149; }
    .toggle { position: relative; display: inline-block; width: 42px; height: 24px; margin: 0; }
    .toggle input { opacity: 0; width: 0; height: 0; }
    .toggle-slider { position: absolute; inset: 0; cursor: pointer; background: #484f58; border-radius: 20px; transition: .2s; }
    .toggle-slider:before { content: ""; position: absolute; width: 18px; height: 18px; left: 3px; top: 3px; background: white; border-radius: 50%; transition: .2s; }
    .toggle input:checked + .toggle-slider { background: var(--accent); }
    .toggle input:checked + .toggle-slider:before { transform: translateX(18px); }
    .toggle input:disabled + .toggle-slider { opacity: .55; cursor: wait; }
    .route-row { display: grid; grid-template-columns: minmax(150px, 1fr) minmax(220px, 1.4fr) auto; gap: .6rem; align-items: center; margin-bottom: .6rem; }
    .alias-route-row, .alias-route-header { grid-template-columns: minmax(140px, .8fr) minmax(140px, .7fr) minmax(220px, 1.4fr) auto; }
    .alias-route-header { display: grid; gap: .6rem; color: #8b949e; font-size: .75rem; margin-bottom: .35rem; padding: 0 .1rem; }
    .route-row input { min-width: 0; }
    .route-row select { min-width: 0; }
    .muted { color: #8b949e; font-size: .8rem; }
    .model-select { width: auto; accent-color: var(--accent); cursor: pointer; }
    .model-actions { display: flex; align-items: center; gap: .75rem; flex-wrap: wrap; margin-bottom: .75rem; }
    .model-filters { display: grid; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: .75rem; margin-bottom: .75rem; }
    .model-actions button:disabled { opacity: .5; cursor: not-allowed; }
    #openCodeConfigPanel, #claudeCodeConfigPanel { margin-top: 1rem; }
    #openCodeConfigOutput, #claudeCodeConfigOutput { max-height: 360px; white-space: pre; }
    .scroll-frame { overflow: auto; max-height: 460px; border: 1px solid var(--border); border-radius: 8px; }
    .scroll-frame table { margin-top: 0; }
    .scroll-frame th { position: sticky; top: 0; z-index: 1; background: var(--card-bg); }
    
    .token-in { color: var(--stat-in); font-weight: 600; font-family: ui-monospace, monospace; }
    .token-out { color: var(--stat-out); font-weight: 600; font-family: ui-monospace, monospace; }
    .token-total { color: var(--stat-total); font-weight: 700; font-family: ui-monospace, monospace; }
    
    .sub-table { width: 100%; background: #0c1016; border-radius: 6px; margin: 0.5rem 0; border: 1px solid var(--border); }
    .sub-table th, .sub-table td { padding: 0.45rem 0.6rem; font-size: 0.8rem; }
    .sub-table th { background: #161b22; }
    
    #responseOutput { min-height: 120px; white-space: pre-wrap; word-break: break-word; color: #7ee787; }
    
    /* Analytics & Stacked Bar Chart Styles */
    .analytics-header { display: flex; justify-content: space-between; align-items: center; flex-wrap: wrap; gap: 0.75rem; margin-bottom: 1rem; }
    .filter-group { display: flex; align-items: center; gap: 0.4rem; flex-wrap: wrap; }
    .range-btn { background: #21262d; border: 1px solid var(--border); color: #c9d1d9; padding: 0.3rem 0.65rem; font-size: 0.78rem; font-weight: 500; border-radius: 6px; cursor: pointer; transition: all 0.15s; }
    .range-btn:hover { background: #30363d; color: var(--text-bright); }
    .range-btn.active { background: #1f6feb; border-color: #388bfd; color: #fff; font-weight: 600; }
    .date-input { width: 125px; min-width: 110px; max-width: 130px; background: var(--code-bg); border: 1px solid var(--border); border-radius: 6px; color: var(--text-bright); padding: 0.25rem 0.4rem; font-size: 0.78rem; outline: none; color-scheme: dark; box-sizing: border-box; flex-shrink: 0; }
    .select-input { width: auto; min-width: 120px; background: var(--code-bg); border: 1px solid var(--border); border-radius: 6px; color: var(--text-bright); padding: 0.25rem 0.5rem; font-size: 0.78rem; outline: none; box-sizing: border-box; }
    .chart-container { position: relative; width: 100%; background: #0c1017; border: 1px solid var(--border); border-radius: 8px; padding: 1rem; margin-top: 0.5rem; min-height: 320px; display: flex; flex-direction: column; }
    .chart-tooltip { position: absolute; display: none; background: #1c2128; border: 1px solid #444c56; border-radius: 8px; padding: 0.75rem 1rem; color: #f0f6fc; font-size: 0.8rem; pointer-events: none; z-index: 100; box-shadow: 0 8px 24px rgba(0,0,0,0.55); min-width: 220px; max-width: 320px; }
    .chart-tooltip h4 { margin-bottom: 0.35rem; font-size: 0.85rem; color: #58a6ff; border-bottom: 1px solid #30363d; padding-bottom: 0.25rem; display: flex; justify-content: space-between; align-items: center; }
    .chart-legend { display: flex; flex-wrap: wrap; gap: 0.6rem; margin-top: 0.75rem; padding-top: 0.75rem; border-top: 1px solid var(--border); font-size: 0.78rem; }
    .legend-item { display: inline-flex; align-items: center; gap: 0.4rem; background: #161b22; padding: 0.25rem 0.55rem; border-radius: 6px; border: 1px solid var(--border); }
    .legend-color { width: 10px; height: 10px; border-radius: 3px; flex-shrink: 0; }
    .bar-group { cursor: pointer; }
    .bar-group:hover rect { filter: brightness(1.2); }
    .axis-label { fill: #8b949e; font-size: 11px; font-family: -apple-system, sans-serif; }
    .grid-line { stroke: #21262d; stroke-dasharray: 2, 2; }
  </style>
</head>
<body>
  <div class="container">
    <header>
      <h1>🗼 BabelGate <span class="status-badge" id="liveStatus">Active</span></h1>
      <div style="display: flex; gap: 0.75rem; align-items: center;">
        <a href="/setup" class="btn-sm" style="text-decoration: none; display: inline-flex; align-items: center; gap: 0.4rem; padding: 0.4rem 0.85rem; font-weight: 600; color: #f0f6fc; background: #21262d; border: 1px solid #30363d; border-radius: 6px;">📖 Setup Help</a>
        <span class="badge">Multi-Protocol Proxy</span>
      </div>
    </header>

    <!-- Top Summary Metrics Grid -->
    <div class="stats-grid">
      <div class="stat-card">
        <div class="stat-title">Active Sessions</div>
        <div class="stat-val" id="statSessions">0</div>
      </div>
      <div class="stat-card">
        <div class="stat-title">Total Requests</div>
        <div class="stat-val" id="statRequests">0</div>
      </div>
      <div class="stat-card">
        <div class="stat-title">Input Tokens</div>
        <div class="stat-val stat-in" id="statInputTokens">0</div>
      </div>
      <div class="stat-card">
        <div class="stat-title">Output Tokens</div>
        <div class="stat-val stat-out" id="statOutputTokens">0</div>
      </div>
      <div class="stat-card">
        <div class="stat-title">Total Tokens</div>
        <div class="stat-val stat-total" id="statTotalTokens">0</div>
      </div>
    </div>

    <!-- Persistent Token Analytics & Trends (SQLite) -->
    <div class="card" style="margin-bottom: 1.75rem;">
      <div class="analytics-header">
        <div>
          <h2 style="margin-bottom: 0.2rem; display: flex; align-items: center; gap: 0.5rem;">
            📊 Historical Token Analytics
            <span class="badge" style="color: #3fb950; border-color: #238636;">SQLite Store</span>
          </h2>
          <div style="font-size: 0.78rem; color: #8b949e;">
            Hourly aggregated counters per provider and model. Click any day bar to zoom into the 24-hour hourly view.
          </div>
        </div>
        <div style="display: flex; gap: 0.5rem; align-items: center; flex-wrap: wrap;">
          <button id="zoomBackBtn" class="btn-sm" style="display: none; background: #1f6feb; color: white; border-color: #388bfd;" onclick="zoomBackToDaily()">← Back to Daily View</button>
          <button class="btn-sm" onclick="loadAnalytics()">↻ Refresh Metrics</button>
        </div>
      </div>

      <!-- Controls Bar: Range Presets, Custom Dates, Provider Filter -->
      <div style="display: flex; justify-content: space-between; align-items: center; flex-wrap: wrap; gap: 0.75rem; background: #11151c; padding: 0.75rem; border-radius: 8px; border: 1px solid var(--border); margin-bottom: 1rem;">
        <div class="filter-group">
          <span style="font-size: 0.78rem; color: #8b949e; font-weight: 600;">RANGE:</span>
          <button class="range-btn" id="btn-range-24h" onclick="selectRange('24h')">24h</button>
          <button class="range-btn active" id="btn-range-7d" onclick="selectRange('7d')">7 Days</button>
          <button class="range-btn" id="btn-range-14d" onclick="selectRange('14d')">14 Days</button>
          <button class="range-btn" id="btn-range-30d" onclick="selectRange('30d')">30 Days</button>
          <button class="range-btn" id="btn-range-90d" onclick="selectRange('90d')">90 Days</button>
        </div>

        <div class="filter-group" style="flex-wrap: nowrap;">
          <span style="font-size: 0.78rem; color: #8b949e; font-weight: 600;">CUSTOM:</span>
          <input type="date" id="customStartDate" class="date-input">
          <span style="color: #8b949e; font-size: 0.8rem;">to</span>
          <input type="date" id="customEndDate" class="date-input">
          <button class="btn-sm" onclick="applyCustomRange()">Apply</button>
        </div>

        <div class="filter-group" style="flex-wrap: nowrap;">
          <span style="font-size: 0.78rem; color: #8b949e; font-weight: 600;">PROVIDER:</span>
          <select id="metricsProviderSelect" class="select-input" onchange="onMetricsProviderChange()">
            <option value="">All Providers</option>
          </select>
        </div>
      </div>

      <!-- View Title & Top Model Banner -->
      <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 0.75rem; font-size: 0.85rem; flex-wrap: wrap; gap: 0.5rem;">
        <div id="analyticsViewTitle" style="font-weight: 600; color: #f0f6fc;">
          📅 Daily View
        </div>
        <div id="analyticsTopModelBanner" style="color: #8b949e; font-size: 0.8rem;"></div>
      </div>

      <!-- Summary KPI Strip -->
      <div class="stats-grid" style="margin-bottom: 1rem; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));">
        <div class="stat-card" style="padding: 0.65rem 0.9rem; background: #0c1017;">
          <div class="stat-title">Period Tokens</div>
          <div class="stat-val stat-total" id="kpiTotalTokens" style="font-size: 1.35rem;">0</div>
        </div>
        <div class="stat-card" style="padding: 0.65rem 0.9rem; background: #0c1017;">
          <div class="stat-title">Input Tokens</div>
          <div class="stat-val stat-in" id="kpiInputTokens" style="font-size: 1.35rem;">0</div>
        </div>
        <div class="stat-card" style="padding: 0.65rem 0.9rem; background: #0c1017;">
          <div class="stat-title">Output Tokens</div>
          <div class="stat-val stat-out" id="kpiOutputTokens" style="font-size: 1.35rem;">0</div>
        </div>
        <div class="stat-card" style="padding: 0.65rem 0.9rem; background: #0c1017;">
          <div class="stat-title">Requests</div>
          <div class="stat-val" id="kpiRequests" style="font-size: 1.35rem;">0</div>
        </div>
        <div class="stat-card" style="padding: 0.65rem 0.9rem; background: #0c1017;">
          <div class="stat-title">Errors</div>
          <div class="stat-val" id="kpiErrors" style="font-size: 1.35rem; color: #f85149;">0</div>
        </div>
      </div>

      <!-- Chart Container & Tooltip -->
      <div class="chart-container" id="chartWrapper">
        <div id="chartTooltip" class="chart-tooltip"></div>
        <div id="chartEmpty" style="display: none; margin: auto; text-align: center; color: #8b949e; padding: 2.5rem 1rem;">
          <div style="font-size: 1.8rem; margin-bottom: 0.4rem;">📊</div>
          <div style="font-weight: 600; color: #c9d1d9;">No metrics recorded for this time range</div>
          <div style="font-size: 0.8rem; margin-top: 0.2rem;">Send requests through the router or use the playground below.</div>
        </div>
        <div id="chartSvgWrapper" style="width: 100%; height: 320px; position: relative;"></div>
      </div>

      <!-- Chart Model Legend -->
      <div class="chart-legend" id="chartLegend"></div>
    </div>

    <!-- Active Sessions & Token Usage Card -->
    <div class="card" style="margin-bottom: 1.75rem;">
      <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 0.75rem; flex-wrap: wrap; gap: 0.5rem;">
        <div>
          <h2 style="margin-bottom: 0.2rem;">Sessions & Token Usage <span class="badge" id="sessionCount">0</span></h2>
          <div style="font-size: 0.78rem; color: #8b949e;">Retention: Up to 24h in-memory (max 200 sessions, 50 requests/session). Idle timeout: 30m.</div>
        </div>
        <div>
          <button class="btn-sm" onclick="loadSessions()">↻ Refresh</button>
          <button class="btn-sm btn-outline" style="margin-left: 0.5rem;" onclick="clearSessions()">Reset All Stats</button>
        </div>
      </div>
      <div style="overflow-x: auto;">
        <table>
          <thead>
            <tr>
              <th>Session ID</th>
              <th>Client</th>
              <th>Models Used</th>
              <th>Requests</th>
              <th title="Full input of the latest generation request, including cached tokens. ~ means estimated. Session statistics reset on restart.">Context Tokens</th>
              <th title="Cumulative input tokens across the session">Input Total</th>
              <th>Output Tokens</th>
              <th>Output Speed</th>
              <th>Total Tokens</th>
              <th>Last Active</th>
              <th>Action</th>
            </tr>
          </thead>
          <tbody id="sessionsTable">
            <tr><td colspan="11" style="text-align: center; color: #8b949e; padding: 1.5rem;">No active sessions yet. Use Claude Code, OpenAI SDK, or the playground below.</td></tr>
          </tbody>
        </table>
      </div>
    </div>

    <!-- Provider Configuration -->
    <div class="card" style="margin-bottom: 1.75rem;">
      <h2 style="margin-bottom: 0.25rem;">Upstream Providers <span class="badge" id="provCount">0</span></h2>
      <div class="muted" style="margin-bottom: .75rem;">Changes take effect immediately and are saved when a YAML configuration file is active.</div>
      <div style="overflow-x: auto;">
        <table>
          <thead>
            <tr><th>Name</th><th>Driver</th><th>Priority</th><th>Status</th><th>Enabled</th></tr>
          </thead>
          <tbody id="providersTable">
            <tr><td colspan="5">Loading providers...</td></tr>
          </tbody>
        </table>
      </div>
    </div>

    <!-- Visual Routing Configuration -->
    <div class="card" style="margin-bottom: 1.75rem;">
      <h2 style="margin-bottom: 0.25rem;">Visual Route Configuration <span class="badge">Live</span></h2>
      <div class="muted" style="margin-bottom: 1rem;">Use provider-prefixed targets such as <code>google/gemini-2.5-flash</code>.</div>
      <div class="form-group">
        <label for="defaultRoute">Default route (optional)</label>
        <input id="defaultRoute" list="routeTargets" placeholder="provider/model">
      </div>
      <div style="display:flex; justify-content:space-between; align-items:center; margin: 1rem 0 .6rem;">
        <strong>Aliases</strong><button class="btn-sm" onclick="addRouteRow()">+ Add alias</button>
      </div>
      <div class="alias-route-header"><span>Virtual route</span><span>Provider</span><span>Model</span><span></span></div>
      <div id="routeRows"></div>
      <div style="display:flex; justify-content:space-between; align-items:center; margin: 1rem 0 .6rem;">
        <strong>Fallback chains</strong><button class="btn-sm" onclick="addFallbackRow()">+ Add fallback</button>
      </div>
      <div id="fallbackRows"></div>
      <datalist id="routeTargets"></datalist>
      <div style="display:flex; align-items:center; gap:.75rem; margin-top:1rem;">
        <button onclick="saveRouting()">Save Routes</button>
        <button class="btn-sm" onclick="reloadRouting()">↻ Reload from YAML</button>
        <span id="routingStatus" class="muted"></span>
      </div>
    </div>

    <!-- Active Models Catalog -->
    <div class="card">
      <h2>Available Models Catalog <span class="badge" id="modelCount">0</span></h2>
      <div class="model-filters">
        <select id="modelProviderFilter" aria-label="Filter models by provider" onchange="renderModelCatalog()">
          <option value="">All providers</option>
        </select>
        <input id="modelNameFilter" type="search" aria-label="Filter models by name" placeholder="Search models by name…" oninput="renderModelCatalog()">
      </div>
      <div class="model-actions">
        <span class="muted" id="selectedModelCount">0 models selected</span>
        <button id="generateOpenCodeButton" class="btn-sm" onclick="generateOpenCodeConfig()" disabled>Generate OpenCode Config</button>
        <button id="generateClaudeCodeButton" class="btn-sm" onclick="generateClaudeCodeConfig()" disabled>Generate Claude Code Config</button>
      </div>
      <div class="scroll-frame">
        <table>
          <thead>
            <tr><th><input id="selectAllModels" class="model-select" type="checkbox" aria-label="Select all visible models" title="Select all visible models" onchange="toggleAllOpenCodeModels(this.checked)"></th><th>Model Identifier</th><th>Provider / Route</th><th>Type</th><th>Description</th></tr>
          </thead>
          <tbody id="modelsTable">
            <tr><td colspan="5">Loading models catalog...</td></tr>
          </tbody>
        </table>
      </div>
      <div id="openCodeConfigPanel" hidden>
        <div style="display:flex; align-items:center; justify-content:space-between; gap:.75rem;">
          <strong>OpenCode configuration fragment</strong>
          <button id="copyOpenCodeButton" class="btn-sm" onclick="copyOpenCodeConfig()">Copy JSON</button>
        </div>
        <pre id="openCodeConfigOutput"></pre>
        <div class="muted">Merge this fragment into your <code>opencode.json</code> configuration.</div>
      </div>
      <div id="claudeCodeConfigPanel" hidden>
        <div style="display:flex; align-items:center; justify-content:space-between; gap:.75rem;">
          <strong>Claude Code configuration fragment</strong>
          <button id="copyClaudeCodeButton" class="btn-sm" onclick="copyClaudeCodeConfig()">Copy JSON</button>
        </div>
        <pre id="claudeCodeConfigOutput"></pre>
        <div class="muted">Add or merge this <code>modelPicker</code> block into your Claude Code <code>settings.json</code> (user settings at <code>~/.claude/settings.json</code> or project settings at <code>.claude/settings.json</code>). Make sure <code>ANTHROPIC_BASE_URL</code> points to your BabelGate server (e.g. <code><span id="claudeCodeBaseUrl">http://localhost:8080</span></code>).</div>
      </div>
    </div>

    <!-- Live Test Playground -->
    <div class="interactive-panel">
      <div style="display: grid; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: 1rem; margin-bottom: 1rem;">
        <div class="form-group" style="margin-bottom: 0;">
          <label for="providerSelect">Select Provider</label>
          <select id="providerSelect" onchange="onProviderChange()">
            <option value="">All Providers</option>
          </select>
        </div>
        <div class="form-group" style="margin-bottom: 0;">
          <label for="modelSelect">Select Model</label>
          <select id="modelSelect"><option>Loading...</option></select>
        </div>
      </div>
      <div class="form-group">
        <label for="promptInput">Prompt</label>
        <textarea id="promptInput" rows="2" placeholder="Explain the concept of an API router in one sentence."></textarea>
      </div>
      <div style="display: flex; align-items: center; justify-content: space-between;">
        <button id="sendBtn" onclick="sendTestRequest()">Send Request</button>
        <span id="playgroundMetrics" style="font-size: 0.85rem; color: #8b949e;"></span>
      </div>

      <div style="margin-top: 1.5rem;">
        <label>Live Streaming Output</label>
        <pre id="responseOutput">// Response will appear here...</pre>
      </div>
    </div>
  </div>

  <script>
    function escapeHtml(str) {
      if (str == null) return '';
      return String(str)
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;');
    }

    function formatNumber(num) {
      if (num == null) return '0';
      return Number(num).toLocaleString();
    }

    function formatTokensPerSecond(value) {
      const rate = Number(value || 0);
      return rate > 0 ? rate.toFixed(1) + ' tok/s' : '—';
    }

    function formatRelativeTime(dateStr) {
      if (!dateStr) return '';
      const date = new Date(dateStr);
      const diffSecs = Math.round((new Date() - date) / 1000);
      if (diffSecs < 10) return 'Just now';
      if (diffSecs < 60) return diffSecs + 's ago';
      const diffMins = Math.round(diffSecs / 60);
      if (diffMins < 60) return diffMins + 'm ago';
      const diffHours = Math.round(diffMins / 60);
      if (diffHours < 24) return diffHours + 'h ago';
      return date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    }

    function getProviderPillClass(provider) {
      if (!provider) return 'pill-default';
      const p = String(provider).toLowerCase();
      if (p.startsWith('copilot/') || p === 'copilot' || p.includes('github-copilot')) return 'pill-copilot';
      if (p.startsWith('openai/') || p === 'openai') return 'pill-openai';
      if (p.startsWith('anthropic/') || p === 'anthropic') return 'pill-anthropic';
      if (p.startsWith('google/') || p === 'google') return 'pill-google';
      if (p.includes('copilot')) return 'pill-copilot';
      if (p.includes('openai')) return 'pill-openai';
      if (p.includes('anthropic') || p.includes('claude')) return 'pill-anthropic';
      if (p.includes('google') || p.includes('gemini')) return 'pill-google';
      if (p.includes('azure')) return 'pill-azure';
      if (p.includes('mistral')) return 'pill-mistral';
      if (p.includes('cohere')) return 'pill-cohere';
      if (p.includes('alias')) return 'pill-alias';
      return 'pill-default';
    }

    function getClientPillClass(client) {
      if (!client) return 'pill-default';
      const c = String(client).toLowerCase();
      if (c.includes('claude')) return 'pill-anthropic';
      if (c.includes('openai')) return 'pill-openai';
      if (c.includes('google') || c.includes('gemini')) return 'pill-google';
      if (c.includes('playground') || c.includes('browser')) return 'pill-default';
      return 'pill-alias';
    }

    let allProviders = [];
    let allModels = [];
    let catalogModels = [];
    let priorityMap = {};
    let currentRouting = { default: '', routes: {}, fallbacks: {} };
    const selectedOpenCodeModels = new Set();
    const openDetails = new Set();

    function routeInput(value, placeholder, className, list) {
      const input = document.createElement('input');
      input.value = value || '';
      input.placeholder = placeholder;
      input.className = className;
      if (list) input.setAttribute('list', 'routeTargets');
      return input;
    }

    function addRouteRow(alias, target) {
      const row = document.createElement('div');
      row.className = 'route-row alias-route-row';
      row.appendChild(routeInput(alias, 'Alias, e.g. fast', 'route-alias', false));

      let providerName = '';
      let modelName = target || '';
      const slash = modelName.indexOf('/');
      if (slash > 0) {
        providerName = modelName.slice(0, slash);
        modelName = modelName.slice(slash + 1);
      } else if (!target) {
        const preferred = allProviders.find(p => p.enabled) || allProviders[0];
        providerName = preferred ? preferred.name : '';
      }

      const providerSelect = document.createElement('select');
      providerSelect.className = 'route-provider';
      const placeholder = document.createElement('option');
      placeholder.value = '';
      placeholder.textContent = 'Select provider';
      providerSelect.appendChild(placeholder);
      allProviders.forEach(provider => {
        const option = document.createElement('option');
        option.value = provider.name;
        option.textContent = provider.name.toUpperCase() + (provider.enabled ? '' : ' (disabled)');
        providerSelect.appendChild(option);
      });
      if (providerName && !allProviders.some(provider => provider.name === providerName)) {
        const option = document.createElement('option');
        option.value = providerName;
        option.textContent = providerName.toUpperCase() + ' (unavailable)';
        providerSelect.appendChild(option);
      }
      providerSelect.value = providerName;
      row.appendChild(providerSelect);

      const modelSelect = document.createElement('select');
      modelSelect.className = 'route-model';
      populateRouteModelSelect(modelSelect, providerName, modelName);
      providerSelect.onchange = () => populateRouteModelSelect(modelSelect, providerSelect.value, '');
      row.appendChild(modelSelect);

      const remove = document.createElement('button');
      remove.className = 'btn-sm btn-outline';
      remove.textContent = 'Remove';
      remove.onclick = () => row.remove();
      row.appendChild(remove);
      document.getElementById('routeRows').appendChild(row);
    }

    function providerModels(providerName) {
      const seen = new Set();
      return allModels.filter(model => model.type !== 'alias' && model.provider === providerName).map(model => {
        const rawID = String(model.id || '');
        return rawID.startsWith(providerName + '/') ? rawID.slice(providerName.length + 1) : rawID;
      }).filter(modelID => {
        if (!modelID || seen.has(modelID)) return false;
        seen.add(modelID);
        return true;
      }).sort((a, b) => a.localeCompare(b));
    }

    function populateRouteModelSelect(select, providerName, selectedModel) {
      select.innerHTML = '';
      const models = providerModels(providerName);
      const selectedUnavailable = !!selectedModel && !models.includes(selectedModel);
      if (selectedUnavailable) models.unshift(selectedModel);
      if (models.length === 0) {
        const option = document.createElement('option');
        option.value = selectedModel || '';
        option.textContent = providerName ? 'No models available' : (selectedModel || 'Select a provider first');
        select.appendChild(option);
        return;
      }
      models.forEach(modelID => {
        const option = document.createElement('option');
        option.value = modelID;
        option.textContent = modelID + (modelID === selectedModel && selectedUnavailable ? ' (current)' : '');
        select.appendChild(option);
      });
      select.value = selectedModel && models.includes(selectedModel) ? selectedModel : models[0];
    }

    function addFallbackRow(model, targets) {
      const row = document.createElement('div');
      row.className = 'route-row fallback-route-row';
      row.appendChild(routeInput(model, 'Requested model or alias', 'fallback-model', false));
      row.appendChild(routeInput((targets || []).join(', '), 'provider/model, provider/model', 'fallback-targets', false));
      const remove = document.createElement('button');
      remove.className = 'btn-sm btn-outline';
      remove.textContent = 'Remove';
      remove.onclick = () => row.remove();
      row.appendChild(remove);
      document.getElementById('fallbackRows').appendChild(row);
    }

    function renderRouting() {
      document.getElementById('defaultRoute').value = currentRouting.default || '';
      document.getElementById('routeRows').innerHTML = '';
      Object.entries(currentRouting.routes || {}).sort().forEach(([alias, target]) => addRouteRow(alias, target));
      document.getElementById('fallbackRows').innerHTML = '';
      Object.entries(currentRouting.fallbacks || {}).sort().forEach(([model, targets]) => addFallbackRow(model, targets));
      const targets = document.getElementById('routeTargets');
      targets.innerHTML = '';
      allModels.filter(m => m.type !== 'alias').forEach(m => {
        const option = document.createElement('option');
        const rawID = String(m.id || '');
        const clean = m.provider && rawID.startsWith(m.provider + '/') ? rawID.slice(m.provider.length + 1) : rawID;
        option.value = m.provider + '/' + clean;
        targets.appendChild(option);
      });
    }

    async function toggleProvider(name, enabled, checkbox) {
      checkbox.disabled = true;
      try {
        const res = await fetch('/api/providers/' + encodeURIComponent(name), {
          method: 'PUT', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({enabled: enabled})
        });
        if (!res.ok) throw new Error(await res.text());
        await loadData();
      } catch (err) {
        checkbox.checked = !enabled;
        alert('Could not update provider: ' + err.message);
      } finally {
        checkbox.disabled = false;
      }
    }

    async function saveRouting() {
      const status = document.getElementById('routingStatus');
      const routes = {};
      const fallbacks = {};
      document.querySelectorAll('.alias-route-row').forEach(row => {
        const alias = row.querySelector('.route-alias').value.trim();
        const provider = row.querySelector('.route-provider').value.trim();
        const model = row.querySelector('.route-model').value.trim();
        const target = provider && model ? provider + '/' + model : model;
        if (alias || target) routes[alias] = target;
      });
      document.querySelectorAll('.fallback-route-row').forEach(row => {
        const model = row.querySelector('.fallback-model').value.trim();
        const targets = row.querySelector('.fallback-targets').value.split(',').map(v => v.trim()).filter(Boolean);
        if (model || targets.length) fallbacks[model] = targets;
      });
      status.textContent = 'Saving…';
      try {
        const res = await fetch('/api/routing', {method: 'PUT', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({
          default: document.getElementById('defaultRoute').value.trim(), routes: routes, fallbacks: fallbacks
        })});
        if (!res.ok) throw new Error(await res.text());
        const data = await res.json();
        currentRouting = data.routing;
        status.textContent = data.persisted ? 'Saved and active.' : 'Active for this process (no config file loaded).';
        await loadData();
      } catch (err) {
        status.textContent = 'Save failed: ' + err.message.trim();
      }
    }

    async function reloadRouting() {
      const status = document.getElementById('routingStatus');
      status.textContent = 'Reloading…';
      try {
        const res = await fetch('/api/routing', { method: 'POST' });
        if (!res.ok) throw new Error(await res.text());
        const data = await res.json();
        currentRouting = data.routing || { default: '', routes: {}, fallbacks: {} };
        renderRouting();
        status.textContent = 'Reloaded from YAML and active now.';
        await loadData();
      } catch (err) {
        status.textContent = 'Reload failed: ' + err.message.trim();
      }
    }

    function toggleDetails(sessionId) {
      const detailsRow = document.getElementById('details-' + sessionId);
      const btn = document.getElementById('btn-' + sessionId);
      if (!detailsRow) return;

      if (detailsRow.style.display === 'none' || detailsRow.style.display === '') {
        detailsRow.style.display = 'table-row';
        openDetails.add(sessionId);
        if (btn) btn.textContent = 'Hide Details';
      } else {
        detailsRow.style.display = 'none';
        openDetails.delete(sessionId);
        if (btn) btn.textContent = 'View Details';
      }
    }

    async function loadSessions() {
      try {
        const res = await fetch('/api/sessions');
        if (!res.ok) return;
        const data = await res.json();

        const summary = data.summary || {};
        const sessions = data.sessions || [];

        // Update top summary cards
        document.getElementById('statSessions').textContent = formatNumber(summary.total_sessions || sessions.length);
        document.getElementById('statRequests').textContent = formatNumber(summary.total_requests || 0);
        document.getElementById('statInputTokens').textContent = formatNumber(summary.total_input_tokens || 0);
        document.getElementById('statOutputTokens').textContent = formatNumber(summary.total_output_tokens || 0);
        document.getElementById('statTotalTokens').textContent = formatNumber(summary.total_tokens || 0);
        document.getElementById('sessionCount').textContent = sessions.length;

        const tableBody = document.getElementById('sessionsTable');
        if (sessions.length === 0) {
          tableBody.innerHTML = '<tr><td colspan="11" style="text-align: center; color: #8b949e; padding: 1.5rem;">No active sessions yet. Use Claude Code, OpenAI SDK, or the playground below.</td></tr>';
          return;
        }

        tableBody.innerHTML = '';
        sessions.forEach(s => {
          const clientPill = getClientPillClass(s.client);
          let modelsHtml = '<span style="color: #8b949e;">-</span>';
          if (s.models && s.models.length > 0) {
            modelsHtml = s.models.map(m => {
              const stat = (s.model_stats && s.model_stats[m]) ? s.model_stats[m] : null;
              const isLast = (s.last_model && s.last_model === m) ||
                             (!s.last_model && s.recent_requests && s.recent_requests.length > 0 && s.recent_requests[0].model === m);

              let pctReq = 0;
              let pctTok = 0;
              let reqs = 0;
              let toks = 0;
              if (stat) {
                pctReq = (stat.percent_req !== undefined && stat.percent_req !== null) ? Math.round(stat.percent_req) : 0;
                pctTok = (stat.percent_tok !== undefined && stat.percent_tok !== null) ? Math.round(stat.percent_tok) : 0;
                reqs = stat.request_count || 0;
                toks = stat.total_tokens || 0;
              } else if (s.request_count > 0 && s.models.length === 1) {
                pctReq = 100;
                pctTok = 100;
                reqs = s.request_count;
                toks = s.total_tokens;
              }

              const pctLabel = pctReq > 0 ? (pctReq + '%') : '';
              const tooltip = stat
                ? (m + '\n' + reqs + ' reqs (' + pctReq + '%)\n' + formatNumber(toks) + ' tokens (' + pctTok + '%)' + (isLast ? '\n● Last called' : ''))
                : (m + (isLast ? '\n● Last called' : ''));

              const lastBadge = isLast
                ? '<span class="last-tag" title="Used in most recent call">LAST</span>'
                : '';
              const extraStyle = isLast
                ? 'box-shadow: 0 0 0 1px #58a6ff; font-weight: 700;'
                : 'opacity: 0.9;';

              return '<span class="pill ' + getProviderPillClass(m) + '" style="margin-right: 0.25rem; margin-bottom: 0.25rem; ' + extraStyle + '" title="' + escapeHtml(tooltip) + '">' +
                escapeHtml(m) +
                (pctLabel ? '<span class="model-pct">' + pctLabel + '</span>' : '') +
                lastBadge +
                '</span>';
            }).join('');
          }

          const reqCount = s.request_count || 0;
          const isExpanded = openDetails.has(s.id);

          const tr = document.createElement('tr');
          tr.innerHTML = '<td><code>' + escapeHtml(s.id) + '</code></td>' +
            '<td><span class="pill ' + clientPill + '">' + escapeHtml(s.client || 'Client') + '</span></td>' +
            '<td>' + modelsHtml + '</td>' +
            '<td><strong>' + formatNumber(reqCount) + '</strong></td>' +
            '<td title="' + (s.context_tokens_estimated ? 'Estimated input of the latest request' : 'Provider-reported input of the latest request, including cache') + '"><strong class="token-in">' + (s.context_tokens_estimated ? '~' : '') + formatNumber(s.context_tokens) + '</strong></td>' +
            '<td><span class="token-in">' + formatNumber(s.input_tokens) + '</span></td>' +
            '<td><span class="token-out">' + formatNumber(s.output_tokens) + '</span></td>' +
            '<td><strong style="color:#7ee787; white-space:nowrap;">' + formatTokensPerSecond(s.tokens_per_second) + '</strong></td>' +
            '<td><span class="token-total">' + formatNumber(s.total_tokens) + '</span></td>' +
            '<td title="' + escapeHtml(s.last_active) + '">' + formatRelativeTime(s.last_active) + '</td>' +
            '<td><div style="display: flex; gap: 0.35rem; align-items: center;">' +
              '<button id="btn-' + escapeHtml(s.id) + '" class="btn-sm" onclick="toggleDetails(\'' + escapeHtml(s.id) + '\')">' + (isExpanded ? 'Hide Details' : 'View Details') + '</button>' +
              '<button class="btn-sm btn-outline" title="Delete this session" onclick="deleteSession(\'' + escapeHtml(s.id) + '\')">🗑</button>' +
            '</div></td>';
          tableBody.appendChild(tr);

          // Details sub-row
          const detailsTr = document.createElement('tr');
          detailsTr.id = 'details-' + s.id;
          detailsTr.style.display = isExpanded ? 'table-row' : 'none';

          let requestsHtml = '<p style="color: #8b949e; font-size: 0.8rem; padding: 0.5rem;">No request details recorded.</p>';
          if (s.recent_requests && s.recent_requests.length > 0) {
            requestsHtml = '<table class="sub-table">' +
              '<thead><tr>' +
              '<th>Time</th><th>Model</th><th>Type</th><th>Duration</th><th>Input Tokens</th><th>Output Tokens</th><th>Output Speed</th><th>Total</th><th>Status</th>' +
              '</tr></thead><tbody>' +
              s.recent_requests.map(r => {
                const statusColor = r.status === 'success' ? '#3fb950' : '#f85149';
                const typeBadge = r.stream ? '<span class="badge">stream</span>' : '<span class="badge">sync</span>';
                const timeStr = r.timestamp ? new Date(r.timestamp).toLocaleTimeString() : '';
                return '<tr>' +
                  '<td>' + escapeHtml(timeStr) + '</td>' +
                  '<td><code>' + escapeHtml(r.model) + '</code></td>' +
                  '<td>' + typeBadge + '</td>' +
                  '<td>' + r.duration_ms + 'ms</td>' +
                  '<td><span class="token-in">' + formatNumber(r.input_tokens) + '</span></td>' +
                  '<td><span class="token-out">' + formatNumber(r.output_tokens) + '</span></td>' +
                  '<td><strong style="color:#7ee787; white-space:nowrap;">' + formatTokensPerSecond(r.tokens_per_second) + '</strong></td>' +
                  '<td><span class="token-total">' + formatNumber(r.total_tokens) + '</span></td>' +
                  '<td><span style="color: ' + statusColor + ';">● ' + escapeHtml(r.status) + '</span></td>' +
                  '</tr>';
              }).join('') +
              '</tbody></table>';
          }

          let modelSummaryHtml = '';
          if (s.models && s.models.length > 0 && s.model_stats) {
            const statsList = Object.values(s.model_stats);
            if (statsList.length > 0) {
              modelSummaryHtml = '<div style="display: flex; flex-wrap: wrap; gap: 0.5rem; margin-bottom: 0.6rem; font-size: 0.78rem;">' +
                statsList.map(st => {
                  const isLast = (s.last_model === st.model);
                  const pReq = Math.round(st.percent_req || 0);
                  const pTok = Math.round(st.percent_tok || 0);
                  return '<div style="background: #161b22; border: 1px solid ' + (isLast ? '#388bfd' : 'var(--border)') + '; border-radius: 4px; padding: 0.25rem 0.5rem; display: flex; align-items: center;">' +
                    '<span class="pill ' + getProviderPillClass(st.model) + '" style="padding: 0.1rem 0.35rem; font-size: 0.7rem; margin-right: 0.35rem;">' + escapeHtml(st.model) + '</span>' +
                    '<span><strong>' + pReq + '%</strong> reqs (' + st.request_count + ') · ' +
                    '<strong>' + pTok + '%</strong> tokens (' + formatNumber(st.total_tokens) + ')</span>' +
                    (isLast ? '<span class="last-tag" style="margin-left: 0.35rem;">LAST</span>' : '') +
                    '</div>';
                }).join('') +
                '</div>';
            }
          }

          detailsTr.innerHTML = '<td colspan="11" style="background: #11151c; padding: 0.75rem 1rem; border-top: 1px dashed var(--border);">' +
            '<div style="font-size: 0.82rem; font-weight: 600; color: #8b949e; margin-bottom: 0.35rem;">Request History for Session ' + escapeHtml(s.id) + '</div>' +
            modelSummaryHtml +
            requestsHtml +
            '</td>';
          tableBody.appendChild(detailsTr);
        });
      } catch (err) {
        console.error('Failed to load sessions:', err);
      }
    }

    async function deleteSession(sessionId) {
      if (!confirm('Delete session ' + sessionId + '?')) return;
      try {
        const res = await fetch('/api/sessions/clear?id=' + encodeURIComponent(sessionId), { method: 'POST' });
        if (res.ok) {
          openDetails.delete(sessionId);
          await loadSessions();
        }
      } catch (err) {
        console.error('Failed to delete session:', err);
      }
    }

    async function clearSessions() {
      if (!confirm('Are you sure you want to reset all token statistics and clear all session history?')) return;
      try {
        await fetch('/api/sessions/clear', { method: 'POST' });
        openDetails.clear();
        await loadSessions();
      } catch (err) {
        console.error('Failed to clear sessions:', err);
      }
    }

    function onProviderChange() {
      const selectedProv = document.getElementById('providerSelect').value;
      const select = document.getElementById('modelSelect');
      select.innerHTML = '';

      const seen = new Set();
      const filtered = [];

      allModels.forEach(m => {
        const isAlias = m.type === 'alias';
        const provName = isAlias ? 'alias' : (m.provider || '');

        if (selectedProv && selectedProv !== provName && (!isAlias || selectedProv !== 'alias')) {
          return;
        }

        // Clean name without provider prefix
        let cleanName = m.id;
        if (m.provider && cleanName.startsWith(m.provider + '/')) {
          cleanName = cleanName.slice(m.provider.length + 1);
        }

        const key = (m.provider || '') + ':' + cleanName;
        if (!seen.has(key)) {
          seen.add(key);
          filtered.push({
            id: isAlias ? m.id : (m.provider && m.provider !== 'router-alias' ? m.provider + '/' + cleanName : cleanName),
            name: cleanName,
            provider: m.provider,
            isAlias: isAlias
          });
        }
      });

      if (filtered.length === 0) {
        const opt = document.createElement('option');
        opt.value = '';
        opt.textContent = 'No models available';
        select.appendChild(opt);
        return;
      }

      filtered.sort((a, b) => {
        const pA = priorityMap[a.provider] !== undefined ? priorityMap[a.provider] : (a.isAlias ? 999 : 100);
        const pB = priorityMap[b.provider] !== undefined ? priorityMap[b.provider] : (b.isAlias ? 999 : 100);
        if (pA !== pB) return pA - pB;
        const provA = String(a.provider || '');
        const provB = String(b.provider || '');
        if (provA !== provB) return provA.localeCompare(provB);
        return String(a.name || a.id || '').localeCompare(String(b.name || b.id || ''));
      });

      filtered.forEach(m => {
        const opt = document.createElement('option');
        opt.value = m.id;
        if (!selectedProv && m.provider && m.provider !== 'router-alias') {
          opt.textContent = m.name + ' (' + m.provider.toUpperCase() + ')';
        } else {
          opt.textContent = m.name;
        }
        select.appendChild(opt);
      });
    }

    function openCodeModelKey(model) {
      return [model.type || 'native', model.provider || '', model.id || ''].join(':');
    }

    function filteredCatalogModels() {
      const provider = document.getElementById('modelProviderFilter').value;
      const query = document.getElementById('modelNameFilter').value.trim().toLowerCase();
      return catalogModels.filter(model => {
        if (provider && model.provider !== provider) return false;
        if (!query) return true;
        return String(model.id || '').toLowerCase().includes(query) ||
          String(model.display_name || '').toLowerCase().includes(query);
      });
    }

    function populateModelProviderFilter() {
      const select = document.getElementById('modelProviderFilter');
      const previous = select.value;
      const providers = [];
      const seen = new Set();
      catalogModels.forEach(model => {
        if (model.provider && !seen.has(model.provider)) {
          seen.add(model.provider);
          providers.push(model.provider);
        }
      });
      select.innerHTML = '<option value="">All providers</option>';
      providers.forEach(provider => {
        const option = document.createElement('option');
        option.value = provider;
        option.textContent = provider === 'router-alias' ? 'Aliases / Virtual Routes' : provider.toUpperCase();
        select.appendChild(option);
      });
      select.value = seen.has(previous) ? previous : '';
    }

    function renderModelCatalog() {
      const modelBody = document.getElementById('modelsTable');
      const visibleModels = filteredCatalogModels();
      modelBody.innerHTML = '';
      document.getElementById('modelCount').textContent = visibleModels.length === catalogModels.length
        ? catalogModels.length
        : visibleModels.length + ' / ' + catalogModels.length;

      if (visibleModels.length === 0) {
        modelBody.innerHTML = '<tr><td colspan="5" style="text-align:center; color:#8b949e; padding:1.5rem;">No models match the current filters.</td></tr>';
        updateOpenCodeSelectionControls();
        return;
      }

      visibleModels.forEach(m => {
        const tr = document.createElement('tr');
        const isAlias = m.type === 'alias';
        const providerPillClass = getProviderPillClass(isAlias ? 'alias' : m.provider);
        const providerCell = isAlias && m.target
          ? '<span class="pill ' + providerPillClass + '">alias</span> <span style="color: #8b949e; font-size: 0.8rem;">&rarr; ' + escapeHtml(m.target) + '</span>'
          : '<span class="pill ' + providerPillClass + '">' + escapeHtml(m.provider || 'default') + '</span>';
        const typeBadge = isAlias
          ? '<span class="pill pill-alias">alias</span>'
          : '<span class="badge" style="text-transform: uppercase;">native</span>';

        tr.innerHTML = '<td><code>' + escapeHtml(m.id) + '</code></td>' +
          '<td>' + providerCell + '</td>' +
          '<td>' + typeBadge + '</td>' +
          '<td>' + escapeHtml(m.description || m.display_name || '') + '</td>';
        const selectCell = document.createElement('td');
        const checkbox = document.createElement('input');
        checkbox.type = 'checkbox';
        checkbox.className = 'model-select opencode-model-checkbox';
        checkbox.checked = selectedOpenCodeModels.has(openCodeModelKey(m));
        checkbox.setAttribute('aria-label', 'Select ' + m.id);
        checkbox.onchange = () => {
          const key = openCodeModelKey(m);
          if (checkbox.checked) selectedOpenCodeModels.add(key);
          else selectedOpenCodeModels.delete(key);
          updateOpenCodeSelectionControls();
        };
        selectCell.appendChild(checkbox);
        tr.insertBefore(selectCell, tr.firstChild);
        modelBody.appendChild(tr);
      });
      updateOpenCodeSelectionControls();
    }

    function updateOpenCodeSelectionControls() {
      const available = new Set(catalogModels.map(openCodeModelKey));
      Array.from(selectedOpenCodeModels).forEach(key => {
        if (!available.has(key)) selectedOpenCodeModels.delete(key);
      });
      const selectedCount = selectedOpenCodeModels.size;
      document.getElementById('selectedModelCount').textContent = selectedCount + (selectedCount === 1 ? ' model selected' : ' models selected');
      document.getElementById('generateOpenCodeButton').disabled = selectedCount === 0;
      const claudeBtn = document.getElementById('generateClaudeCodeButton');
      if (claudeBtn) claudeBtn.disabled = selectedCount === 0;
      const selectAll = document.getElementById('selectAllModels');
      const visibleModels = filteredCatalogModels();
      const visibleSelected = visibleModels.filter(model => selectedOpenCodeModels.has(openCodeModelKey(model))).length;
      selectAll.checked = visibleModels.length > 0 && visibleSelected === visibleModels.length;
      selectAll.indeterminate = visibleSelected > 0 && visibleSelected < visibleModels.length;
    }

    function toggleAllOpenCodeModels(checked) {
      filteredCatalogModels().forEach(model => {
        const key = openCodeModelKey(model);
        if (checked) selectedOpenCodeModels.add(key);
        else selectedOpenCodeModels.delete(key);
      });
      document.querySelectorAll('.opencode-model-checkbox').forEach(input => { input.checked = checked; });
      updateOpenCodeSelectionControls();
    }

    function formatConfigFragment(obj) {
      const raw = JSON.stringify(obj, null, 2);
      const lines = raw.split('\n');
      if (lines.length >= 2 && lines[0].trim() === '{' && lines[lines.length - 1].trim() === '}') {
        return lines.slice(1, -1).map(line => line.startsWith('  ') ? line.slice(2) : line).join('\n');
      }
      return raw;
    }

    function generateOpenCodeConfig() {
      const selected = catalogModels.filter(model => selectedOpenCodeModels.has(openCodeModelKey(model)));
      if (selected.length === 0) return;

      const models = {};
      selected.forEach(model => {
        let modelID = model.id;
        if (model.type !== 'alias' && model.provider) {
          modelID = model.provider + '/' + model.id;
        }
        models[modelID] = { name: model.type === 'alias' ? model.id : (model.display_name || model.id) };
      });

      const origin = window.location.origin && window.location.origin !== 'null'
        ? window.location.origin.replace(/\/$/, '')
        : 'http://localhost:8080';
      const fragment = {
        provider: {
          babelgate: {
            name: 'BabelGate',
            npm: '@ai-sdk/openai-compatible',
            options: { baseURL: origin + '/v1' },
            models: models
          }
        }
      };
      document.getElementById('openCodeConfigOutput').textContent = formatConfigFragment(fragment);
      const panel = document.getElementById('openCodeConfigPanel');
      panel.hidden = false;
      const claudePanel = document.getElementById('claudeCodeConfigPanel');
      if (claudePanel) claudePanel.hidden = true;
      panel.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
    }

    async function copyOpenCodeConfig() {
      const text = document.getElementById('openCodeConfigOutput').textContent;
      const button = document.getElementById('copyOpenCodeButton');
      try {
        if (navigator.clipboard && window.isSecureContext) {
          await navigator.clipboard.writeText(text);
        } else {
          const textarea = document.createElement('textarea');
          textarea.value = text;
          textarea.style.position = 'fixed';
          textarea.style.opacity = '0';
          document.body.appendChild(textarea);
          textarea.select();
          document.execCommand('copy');
          textarea.remove();
        }
        button.textContent = 'Copied! ✓';
        setTimeout(() => { button.textContent = 'Copy JSON'; }, 2000);
      } catch (err) {
        button.textContent = 'Copy failed';
        setTimeout(() => { button.textContent = 'Copy JSON'; }, 2000);
      }
    }

    function generateClaudeCodeConfig() {
      const selected = catalogModels.filter(model => selectedOpenCodeModels.has(openCodeModelKey(model)));
      if (selected.length === 0) return;

      const options = selected.map(model => {
        let modelID = model.id;
        if (model.type !== 'alias' && model.provider) {
          modelID = model.provider + '/' + model.id;
        }
        const label = model.type === 'alias' ? model.id : (model.display_name || model.id);
        const description = model.description ||
          (model.type === 'alias' && model.target ? ('Routes to ' + model.target) :
          (model.provider ? (model.provider.toUpperCase() + ' - ' + label) : label));
        return {
          model: modelID,
          label: label,
          description: description
        };
      });

      const origin = window.location.origin && window.location.origin !== 'null'
        ? window.location.origin.replace(/\/$/, '')
        : 'http://localhost:8080';
      const baseUrlEl = document.getElementById('claudeCodeBaseUrl');
      if (baseUrlEl) baseUrlEl.textContent = origin;

      const fragment = {
        modelPicker: {
          options: options,
          replaceBuiltInOptions: true
        }
      };
      document.getElementById('claudeCodeConfigOutput').textContent = formatConfigFragment(fragment);
      const panel = document.getElementById('claudeCodeConfigPanel');
      panel.hidden = false;
      const openCodePanel = document.getElementById('openCodeConfigPanel');
      if (openCodePanel) openCodePanel.hidden = true;
      panel.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
    }

    async function copyClaudeCodeConfig() {
      const text = document.getElementById('claudeCodeConfigOutput').textContent;
      const button = document.getElementById('copyClaudeCodeButton');
      try {
        if (navigator.clipboard && window.isSecureContext) {
          await navigator.clipboard.writeText(text);
        } else {
          const textarea = document.createElement('textarea');
          textarea.value = text;
          textarea.style.position = 'fixed';
          textarea.style.opacity = '0';
          document.body.appendChild(textarea);
          textarea.select();
          document.execCommand('copy');
          textarea.remove();
        }
        button.textContent = 'Copied! ✓';
        setTimeout(() => { button.textContent = 'Copy JSON'; }, 2000);
      } catch (err) {
        button.textContent = 'Copy failed';
        setTimeout(() => { button.textContent = 'Copy JSON'; }, 2000);
      }
    }

    async function loadData() {
      try {
        const [statusRes, modelsRes, routingRes] = await Promise.all([
          fetch('/api/status').then(r => r.json()),
          fetch('/api/models').then(r => r.json()),
          fetch('/api/routing').then(r => r.json())
        ]);

        allProviders = statusRes.providers || [];
        priorityMap = {};
        allProviders.forEach(p => {
          priorityMap[p.name] = p.priority !== undefined ? p.priority : 100;
        });
        allProviders.sort((a, b) => {
          const pA = a.priority !== undefined ? a.priority : 100;
          const pB = b.priority !== undefined ? b.priority : 100;
          if (pA !== pB) return pA - pB;
          return String(a.name || '').localeCompare(String(b.name || ''));
        });
        allModels = modelsRes.models || [];
        currentRouting = routingRes || { default: '', routes: {}, fallbacks: {} };

        // Providers table
        const provBody = document.getElementById('providersTable');
        provBody.innerHTML = '';
        document.getElementById('provCount').textContent = allProviders.length;
        allProviders.forEach(p => {
          const tr = document.createElement('tr');
          const pillClass = getProviderPillClass(p.type || p.name);
          const prioText = p.priority !== undefined ? ('Prio ' + p.priority) : 'Prio 100';
          const online = p.enabled
            ? '<span style="color: #3fb950;">● Online</span>'
            : '<span style="color: #8b949e;">○ Disabled</span>';
          tr.innerHTML = '<td><strong>' + escapeHtml(p.name) + '</strong></td>' +
            '<td><span class="pill ' + pillClass + '">' + escapeHtml(p.type) + '</span></td>' +
            '<td><span class="badge" style="font-weight: 600; color: #58a6ff;">' + prioText + '</span></td>' +
            '<td>' + online + '</td>';
          const action = document.createElement('td');
          action.style.whiteSpace = 'nowrap';
          const label = document.createElement('label');
          label.className = 'toggle';
          const checkbox = document.createElement('input');
          checkbox.type = 'checkbox';
          checkbox.checked = !!p.enabled;
          checkbox.setAttribute('aria-label', (p.enabled ? 'Disable ' : 'Enable ') + p.name);
          checkbox.onchange = () => toggleProvider(p.name, checkbox.checked, checkbox);
          const slider = document.createElement('span');
          slider.className = 'toggle-slider';
          label.appendChild(checkbox);
          label.appendChild(slider);
          action.appendChild(label);
          const actionText = document.createElement('span');
          actionText.className = 'muted';
          actionText.style.marginLeft = '.5rem';
          actionText.textContent = p.enabled ? 'Disable' : 'Enable';
          action.appendChild(actionText);
          tr.appendChild(action);
          provBody.appendChild(tr);
        });

        // Provider dropdown
        const provSelect = document.getElementById('providerSelect');
        provSelect.innerHTML = '<option value="">All Providers</option>';
        allProviders.filter(p => p.enabled).forEach(p => {
          const opt = document.createElement('option');
          opt.value = p.name;
          opt.textContent = p.name.toUpperCase() + ' (' + p.type + ')';
          provSelect.appendChild(opt);
        });
        if (allModels.some(m => m.type === 'alias')) {
          const opt = document.createElement('option');
          opt.value = 'alias';
          opt.textContent = 'Aliases / Virtual Routes';
          provSelect.appendChild(opt);
        }

        // Models table - deduplicate and strip provider prefix
        catalogModels = [];
        const seenInTable = new Set();
        allModels.forEach(m => {
          let cleanId = m.id;
          if (m.provider && cleanId.startsWith(m.provider + '/')) {
            cleanId = cleanId.slice(m.provider.length + 1);
          }
          const dedupeKey = (m.provider || '') + ':' + cleanId;
          if (seenInTable.has(dedupeKey)) return;
          seenInTable.add(dedupeKey);
          catalogModels.push({
            ...m,
            id: cleanId
          });
        });

        catalogModels.sort((a, b) => {
          const pA = priorityMap[a.provider] !== undefined ? priorityMap[a.provider] : (a.type === 'alias' ? 999 : 100);
          const pB = priorityMap[b.provider] !== undefined ? priorityMap[b.provider] : (b.type === 'alias' ? 999 : 100);
          if (pA !== pB) return pA - pB;
          const provA = String(a.provider || '');
          const provB = String(b.provider || '');
          if (provA !== provB) return provA.localeCompare(provB);
          return String(a.id || '').localeCompare(String(b.id || ''));
        });

        populateModelProviderFilter();
        renderModelCatalog();

        // Populate Model dropdown
        onProviderChange();
        renderRouting();
      } catch (err) {
        console.error('Error loading dashboard data:', err);
      }
    }

    function getPlaygroundSessionId() {
      let id = sessionStorage.getItem('llm_router_session_id');
      if (!id) {
        id = 'sess_play_' + Math.random().toString(36).substring(2, 9);
        sessionStorage.setItem('llm_router_session_id', id);
      }
      return id;
    }

    async function sendTestRequest() {
      const model = document.getElementById('modelSelect').value;
      const prompt = document.getElementById('promptInput').value || 'Hello';
      const output = document.getElementById('responseOutput');
      const btn = document.getElementById('sendBtn');
      const metricsSpan = document.getElementById('playgroundMetrics');

      output.textContent = 'Connecting and streaming...\n';
      metricsSpan.textContent = 'Streaming...';
      btn.disabled = true;

      const startTime = performance.now();
      const sessId = getPlaygroundSessionId();

      try {
        const resp = await fetch('/v1/chat/completions', {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json',
            'X-Session-ID': sessId,
            'X-Client': 'Web Playground'
          },
          body: JSON.stringify({
            model: model,
            messages: [{ role: 'user', content: prompt }],
            stream: true
          })
        });

        if (!resp.ok) {
          const errText = await resp.text();
          output.textContent = 'Error: ' + resp.status + ' ' + errText;
          metricsSpan.textContent = 'Request failed (' + resp.status + ')';
          btn.disabled = false;
          loadSessions();
          return;
        }

        output.textContent = '';
        const reader = resp.body.getReader();
        const decoder = new TextDecoder();
        let buffer = '';
        let reportedUsage = null;

        while (true) {
          const { done, value } = await reader.read();
          if (done) break;
          buffer += decoder.decode(value, { stream: true });
          const lines = buffer.split('\n');
          buffer = lines.pop();

          for (const line of lines) {
            const trimmed = line.trim();
            if (trimmed.startsWith('data: ')) {
              const dataStr = trimmed.slice(6);
              if (dataStr === '[DONE]') continue;
              try {
                const chunk = JSON.parse(dataStr);
                if (chunk.usage) {
                  reportedUsage = chunk.usage;
                }
                if (chunk.choices && chunk.choices[0] && chunk.choices[0].delta && chunk.choices[0].delta.content) {
                  output.textContent += chunk.choices[0].delta.content;
                }
              } catch (e) {}
            }
          }
        }

        const elapsedMs = Math.round(performance.now() - startTime);
        if (reportedUsage) {
          metricsSpan.innerHTML = '⚡ ' + elapsedMs + 'ms | <span class="token-in">In: ' + formatNumber(reportedUsage.prompt_tokens) + '</span> | <span class="token-out">Out: ' + formatNumber(reportedUsage.completion_tokens) + '</span> | <span class="token-total">Total: ' + formatNumber(reportedUsage.total_tokens) + '</span>';
        } else {
          metricsSpan.textContent = '⚡ Completed in ' + elapsedMs + 'ms';
        }

      } catch (err) {
        output.textContent += '\nNetwork error: ' + err.message;
        metricsSpan.textContent = 'Error: ' + err.message;
      } finally {
        btn.disabled = false;
        // Refresh sessions and analytics immediately
        loadSessions();
        loadAnalytics();
      }
    }

    // ----------------------------------------------------
    // Historical Token Analytics (SQLite Store)
    // ----------------------------------------------------
    let analyticsMode = 'daily'; // 'daily' or 'hourly'
    let activeRangePreset = '7d';
    let customStart = '';
    let customEnd = '';
    let selectedMetricsProvider = '';
    let currentZoomDate = '';
    let cachedDailyData = [];
    const modelColorMap = {};
    const chartPalette = [
      '#388bfd', '#2ea043', '#bc8cff', '#f0883e', '#56d4dd',
      '#e3b341', '#f778ba', '#39d353', '#79c0ff', '#d2a8ff',
      '#ff7b72', '#a5d6ff', '#ffa657', '#a2d2fb', '#7ee787'
    ];
    let nextPaletteIndex = 0;

    function getModelColor(model) {
      if (!modelColorMap[model]) {
        modelColorMap[model] = chartPalette[nextPaletteIndex % chartPalette.length];
        nextPaletteIndex++;
      }
      return modelColorMap[model];
    }

    function formatShortNumber(num) {
      if (num == null || num === 0) return '0';
      if (num >= 1000000) {
        return (num / 1000000).toFixed(1).replace(/\.0$/, '') + 'M';
      }
      if (num >= 1000) {
        return (num / 1000).toFixed(1).replace(/\.0$/, '') + 'k';
      }
      return String(num);
    }

    function getDateRangeForPreset(preset) {
      const end = new Date();
      const start = new Date();
      if (preset === '24h') {
        start.setDate(start.getDate() - 1);
      } else if (preset === '7d') {
        start.setDate(start.getDate() - 6);
      } else if (preset === '14d') {
        start.setDate(start.getDate() - 13);
      } else if (preset === '30d') {
        start.setDate(start.getDate() - 29);
      } else if (preset === '90d') {
        start.setDate(start.getDate() - 89);
      }
      return {
        start: start.toISOString().slice(0, 10),
        end: end.toISOString().slice(0, 10)
      };
    }

    function selectRange(preset) {
      activeRangePreset = preset;
      analyticsMode = 'daily';
      currentZoomDate = '';
      const backBtn = document.getElementById('zoomBackBtn');
      if (backBtn) backBtn.style.display = 'none';

      ['24h', '7d', '14d', '30d', '90d'].forEach(p => {
        const btn = document.getElementById('btn-range-' + p);
        if (btn) btn.classList.toggle('active', p === preset);
      });

      const range = getDateRangeForPreset(preset);
      customStart = range.start;
      customEnd = range.end;
      const sInput = document.getElementById('customStartDate');
      const eInput = document.getElementById('customEndDate');
      if (sInput) sInput.value = range.start;
      if (eInput) eInput.value = range.end;

      loadAnalytics();
    }

    function applyCustomRange() {
      const s = document.getElementById('customStartDate').value;
      const e = document.getElementById('customEndDate').value;
      if (!s || !e) return;
      customStart = s;
      customEnd = e;
      activeRangePreset = 'custom';
      analyticsMode = 'daily';
      currentZoomDate = '';
      const backBtn = document.getElementById('zoomBackBtn');
      if (backBtn) backBtn.style.display = 'none';

      ['24h', '7d', '14d', '30d', '90d'].forEach(p => {
        const btn = document.getElementById('btn-range-' + p);
        if (btn) btn.classList.remove('active');
      });

      loadAnalytics();
    }

    function onMetricsProviderChange() {
      selectedMetricsProvider = document.getElementById('metricsProviderSelect').value;
      if (analyticsMode === 'hourly' && currentZoomDate) {
        loadHourlyMetrics(currentZoomDate);
      } else {
        loadAnalytics();
      }
    }

    function zoomIntoDay(dateStr) {
      analyticsMode = 'hourly';
      currentZoomDate = dateStr;
      const backBtn = document.getElementById('zoomBackBtn');
      if (backBtn) backBtn.style.display = 'inline-block';
      const titleEl = document.getElementById('analyticsViewTitle');
      if (titleEl) titleEl.innerHTML = '🔍 Zoomed into: <strong>' + escapeHtml(dateStr) + '</strong> (24-Hour Breakdown)';
      loadHourlyMetrics(dateStr);
    }

    function zoomBackToDaily() {
      analyticsMode = 'daily';
      currentZoomDate = '';
      const backBtn = document.getElementById('zoomBackBtn');
      if (backBtn) backBtn.style.display = 'none';
      loadAnalytics();
    }

    async function loadAnalytics() {
      if (analyticsMode === 'hourly' && currentZoomDate) {
        loadHourlyMetrics(currentZoomDate);
        return;
      }

      if (!customStart || !customEnd) {
        const r = getDateRangeForPreset(activeRangePreset);
        customStart = r.start;
        customEnd = r.end;
        const sInput = document.getElementById('customStartDate');
        const eInput = document.getElementById('customEndDate');
        if (sInput) sInput.value = r.start;
        if (eInput) eInput.value = r.end;
      }

      const titleEl = document.getElementById('analyticsViewTitle');
      if (titleEl) titleEl.innerHTML = '📅 Daily View: <strong>' + escapeHtml(customStart) + '</strong> to <strong>' + escapeHtml(customEnd) + '</strong>';

      try {
        const pParam = selectedMetricsProvider ? ('&provider=' + encodeURIComponent(selectedMetricsProvider)) : '';
        const [dailyRes, sumRes] = await Promise.all([
          fetch('/api/metrics/daily?start=' + customStart + '&end=' + customEnd + pParam).then(r => r.json()),
          fetch('/api/metrics/summary?start=' + customStart + '&end=' + customEnd + pParam).then(r => r.json())
        ]);

        // Populate provider dropdown if empty
        const pSelect = document.getElementById('metricsProviderSelect');
        if (pSelect && pSelect.children.length <= 1 && sumRes && sumRes.available_providers) {
          sumRes.available_providers.forEach(p => {
            const opt = document.createElement('option');
            opt.value = p;
            opt.textContent = p.toUpperCase();
            pSelect.appendChild(opt);
          });
          if (selectedMetricsProvider) pSelect.value = selectedMetricsProvider;
        }

        cachedDailyData = dailyRes.days || [];
        renderAnalytics(cachedDailyData, sumRes, false);
      } catch (err) {
        console.error('Failed to load daily analytics:', err);
      }
    }

    async function loadHourlyMetrics(dateStr) {
      try {
        const pParam = selectedMetricsProvider ? ('&provider=' + encodeURIComponent(selectedMetricsProvider)) : '';
        const [hourlyRes, sumRes] = await Promise.all([
          fetch('/api/metrics/hourly?date=' + dateStr + pParam).then(r => r.json()),
          fetch('/api/metrics/summary?start=' + dateStr + '&end=' + dateStr + pParam).then(r => r.json())
        ]);

        renderAnalytics(hourlyRes.hours || [], sumRes, true, dateStr);
      } catch (err) {
        console.error('Failed to load hourly analytics:', err);
      }
    }

    function renderAnalytics(buckets, summary, isHourly, activeDate) {
      // Remember the last render so a window resize can redraw at the new width.
      window.__lastAnalytics = { buckets: buckets, summary: summary, isHourly: isHourly, activeDate: activeDate };
      // Update KPI cards
      const s = summary || {};
      document.getElementById('kpiTotalTokens').textContent = formatNumber(s.total_tokens || 0);
      document.getElementById('kpiInputTokens').textContent = formatNumber(s.input_tokens || 0);
      document.getElementById('kpiOutputTokens').textContent = formatNumber(s.output_tokens || 0);
      document.getElementById('kpiRequests').textContent = formatNumber(s.requests || 0);
      document.getElementById('kpiErrors').textContent = formatNumber(s.errors || 0);

      const bannerEl = document.getElementById('analyticsTopModelBanner');
      if (s.top_models && s.top_models.length > 0) {
        const topM = s.top_models[0];
        bannerEl.innerHTML = 'Top Model: <strong>' + escapeHtml(topM.model) + '</strong> (' + formatNumber(topM.total_tokens) + ' tokens)';
      } else {
        bannerEl.textContent = '';
      }

      const emptyEl = document.getElementById('chartEmpty');
      const svgWrapper = document.getElementById('chartSvgWrapper');

      if (!buckets || buckets.length === 0 || (s.total_tokens === 0 && s.requests === 0)) {
        emptyEl.style.display = 'block';
        svgWrapper.innerHTML = '';
        document.getElementById('chartLegend').innerHTML = '';
        return;
      }
      emptyEl.style.display = 'none';

      // Dimensions: match the viewBox 1:1 to the wrapper's pixel size so nothing
      // gets stretched when the page is wide (preserveAspectRatio="none").
      const vbWidth = Math.max(360, Math.round(svgWrapper.clientWidth) || 1000);
      const vbHeight = Math.max(200, Math.round(svgWrapper.clientHeight) || 300);
      const marginLeft = 65;
      const marginRight = 20;
      const marginTop = 20;
      const marginBottom = 40;
      const plotWidth = vbWidth - marginLeft - marginRight;
      const plotHeight = vbHeight - marginTop - marginBottom;
      const baselineY = marginTop + plotHeight;

      const maxVal = Math.max(...buckets.map(b => Number(b.total_tokens) || 0), 100);

      // SVG Elements
      let svg = '<svg viewBox="0 0 ' + vbWidth + ' ' + vbHeight + '" preserveAspectRatio="none" style="width: 100%; height: 100%; overflow: visible;">';

      // 1. Gridlines and Y-axis labels
      const gridTicks = [0, 0.25, 0.5, 0.75, 1.0];
      gridTicks.forEach(tick => {
        const y = Math.round(baselineY - tick * plotHeight);
        const val = Math.round(tick * maxVal);
        svg += '<line x1="' + marginLeft + '" y1="' + y + '" x2="' + (marginLeft + plotWidth) + '" y2="' + y + '" class="grid-line" />';
        svg += '<text x="' + (marginLeft - 8) + '" y="' + (y + 4) + '" text-anchor="end" class="axis-label">' + formatShortNumber(val) + '</text>';
      });

      // 2. Bars
      const n = buckets.length;
      const slotWidth = plotWidth / n;
      const barWidth = Math.max(6, Math.min(42, slotWidth * 0.74));

      // Tooltip data lookup
      window.__chartBucketData = buckets;

      buckets.forEach((b, idx) => {
        const x = marginLeft + idx * slotWidth + (slotWidth - barWidth) / 2;
        const totalTok = Number(b.total_tokens) || 0;
        const clickAttr = isHourly ? '' : 'onclick="zoomIntoDay(\'' + escapeHtml(b.date) + '\')"';
        const cursorStyle = isHourly ? 'cursor: default;' : 'cursor: pointer;';

        svg += '<g class="bar-group" style="' + cursorStyle + '" ' + clickAttr + ' onmouseenter="showChartTooltip(event, ' + idx + ', ' + isHourly + ')" onmousemove="moveChartTooltip(event)" onmouseleave="hideChartTooltip()">';

        // Invisible hover hit area for easy targeting
        svg += '<rect x="' + (marginLeft + idx * slotWidth) + '" y="' + marginTop + '" width="' + slotWidth + '" height="' + (plotHeight + marginBottom) + '" fill="transparent" />';

        if (totalTok === 0) {
          // Zero tokens placeholder
          svg += '<rect x="' + x + '" y="' + (baselineY - 2) + '" width="' + barWidth + '" height="2" fill="#21262d" rx="1" />';
        } else {
          // Stacked segments:
          // Requirement: "the bar should be a stack bar showing the most amount used tokens of a model at top and etc."
          // Sort models in ascending order so smaller ones are placed at bottom and LARGEST is at the very TOP!
          const modelsAsc = [...(b.models || [])].sort((m1, m2) => (m1.total_tokens || 0) - (m2.total_tokens || 0));

          let currentY = baselineY;
          modelsAsc.forEach((m, mIdx) => {
            const mTok = Number(m.total_tokens) || 0;
            if (mTok <= 0) return;
            const segH = Math.max(2, Math.round((mTok / maxVal) * plotHeight));
            currentY -= segH;
            const color = getModelColor(m.model);
            const isTop = (mIdx === modelsAsc.length - 1);
            const rx = isTop ? '2' : '0';
            svg += '<rect class="bar-segment" x="' + x + '" y="' + currentY + '" width="' + barWidth + '" height="' + segH + '" fill="' + color + '" rx="' + rx + '" />';
          });
        }

        // X-Axis Labels
        let labelText = '';
        let showLabel = true;
        if (isHourly) {
          labelText = b.hour + ':00';
          if (n > 12 && idx % 2 !== 0 && idx !== n - 1) showLabel = false;
        } else {
          // Format date: "09-08"
          const parts = (b.date || '').split('-');
          labelText = parts.length === 3 ? (parts[1] + '/' + parts[2]) : b.date;
          if (n > 14 && idx % Math.ceil(n / 10) !== 0 && idx !== n - 1) showLabel = false;
        }

        if (showLabel) {
          svg += '<text x="' + (x + barWidth / 2) + '" y="' + (baselineY + 18) + '" text-anchor="middle" class="axis-label">' + escapeHtml(labelText) + '</text>';
        }

        svg += '</g>';
      });

      svg += '</svg>';
      svgWrapper.innerHTML = svg;

      // 3. Render Legend
      const legendEl = document.getElementById('chartLegend');
      let legendHtml = '';
      if (s.top_models && s.top_models.length > 0) {
        s.top_models.forEach(m => {
          const color = getModelColor(m.model);
          const pBadge = m.provider ? ('<span style="color: #8b949e; font-size: 0.72rem; margin-left: 0.2rem;">[' + escapeHtml(m.provider) + ']</span>') : '';
          legendHtml += '<div class="legend-item">' +
            '<span class="legend-color" style="background: ' + color + ';"></span>' +
            '<span>' + escapeHtml(m.model) + '</span>' + pBadge +
            '<strong style="color: #58a6ff; margin-left: 0.25rem;">' + formatShortNumber(m.total_tokens) + '</strong>' +
            '</div>';
        });
      }
      legendEl.innerHTML = legendHtml;
    }

    let chartResizeTimer = null;
    window.addEventListener('resize', function() {
      clearTimeout(chartResizeTimer);
      chartResizeTimer = setTimeout(function() {
        const last = window.__lastAnalytics;
        if (last) renderAnalytics(last.buckets, last.summary, last.isHourly, last.activeDate);
      }, 150);
    });

    function showChartTooltip(event, idx, isHourly) {
      const tooltip = document.getElementById('chartTooltip');
      const buckets = window.__chartBucketData;
      if (!tooltip || !buckets || !buckets[idx]) return;

      const b = buckets[idx];
      const title = isHourly ? ('Hour ' + b.hour + ':00 UTC') : ('Day ' + b.date);
      const totalTok = Number(b.total_tokens) || 0;
      const reqCount = Number(b.requests) || 0;
      const errCount = Number(b.errors) || 0;

      let modelsHtml = '';
      if (b.models && b.models.length > 0) {
        // Models sorted descending for clear reading in tooltip
        const sortedDesc = [...b.models].sort((m1, m2) => (m2.total_tokens || 0) - (m1.total_tokens || 0));
        modelsHtml = '<div style="margin-top: 0.5rem; border-top: 1px dashed #30363d; padding-top: 0.4rem;">';
        sortedDesc.forEach(m => {
          const color = getModelColor(m.model);
          const pct = totalTok > 0 ? Math.round((m.total_tokens / totalTok) * 100) : 0;
          modelsHtml += '<div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 0.3rem; gap: 0.5rem;">' +
            '<div style="display: flex; align-items: center; gap: 0.35rem; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;">' +
              '<span class="legend-color" style="background: ' + color + '; width: 8px; height: 8px;"></span>' +
              '<span>' + escapeHtml(m.model) + '</span>' +
            '</div>' +
            '<div style="font-family: monospace; font-weight: 600;">' + formatNumber(m.total_tokens) + ' <span style="color: #8b949e; font-size: 0.72rem;">(' + pct + '%)</span></div>' +
          '</div>';
        });
        modelsHtml += '</div>';
      } else {
        modelsHtml = '<div style="color: #8b949e; font-size: 0.75rem; margin-top: 0.3rem;">No requests in this period.</div>';
      }

      const hintHtml = (!isHourly && totalTok > 0)
        ? '<div style="margin-top: 0.6rem; color: #58a6ff; font-size: 0.75rem; border-top: 1px solid #30363d; padding-top: 0.35rem;">👉 Click day to zoom into hourly breakdown</div>'
        : '';

      tooltip.innerHTML = '<h4><span>' + escapeHtml(title) + '</span><span class="token-total">' + formatNumber(totalTok) + ' tok</span></h4>' +
        '<div style="display: flex; justify-content: space-between; font-size: 0.75rem; color: #8b949e;">' +
          '<span>In: ' + formatNumber(b.input_tokens || 0) + ' | Out: ' + formatNumber(b.output_tokens || 0) + '</span>' +
          '<span>Reqs: ' + reqCount + (errCount > 0 ? (' | <span style="color:#f85149">Err: ' + errCount + '</span>') : '') + '</span>' +
        '</div>' +
        modelsHtml +
        hintHtml;

      tooltip.style.display = 'block';
      moveChartTooltip(event);
    }

    function moveChartTooltip(event) {
      const tooltip = document.getElementById('chartTooltip');
      const chartWrapper = document.getElementById('chartWrapper');
      if (!tooltip || !chartWrapper) return;

      const rect = chartWrapper.getBoundingClientRect();
      const x = event.clientX - rect.left + 15;
      const y = event.clientY - rect.top + 10;

      const tipWidth = tooltip.offsetWidth || 220;
      const finalX = (x + tipWidth > rect.width) ? (x - tipWidth - 30) : x;

      tooltip.style.left = Math.max(10, finalX) + 'px';
      tooltip.style.top = Math.max(10, y) + 'px';
    }

    function hideChartTooltip() {
      const tooltip = document.getElementById('chartTooltip');
      if (tooltip) tooltip.style.display = 'none';
    }

    // Initial load
    loadData();
    loadSessions();
    loadAnalytics();

    // Auto-refresh sessions every 4 seconds
    setInterval(loadSessions, 4000);
  </script>
</body>
</html>
`

const setupHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Setup Help & Client Integrations - BabelGate</title>
  <style>
    :root {
      --bg: #0d1117;
      --card-bg: #161b22;
      --border: #30363d;
      --text: #c9d1d9;
      --text-bright: #f0f6fc;
      --primary: #2f81f7;
      --primary-hover: #58a6ff;
      --accent: #238636;
      --badge-bg: #21262d;
      --code-bg: #090d13;
      --pill-claude: #cc785c;
      --pill-codex: #10a37f;
      --pill-agy: #58a6ff;
    }
    * { box-sizing: border-box; margin: 0; padding: 0; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif; }
    body { background-color: var(--bg); color: var(--text); padding: 2rem; line-height: 1.6; }
    .container { max-width: 1080px; margin: 0 auto; }
    header { display: flex; justify-content: space-between; align-items: center; margin-bottom: 1.5rem; border-bottom: 1px solid var(--border); padding-bottom: 1rem; flex-wrap: wrap; gap: 1rem; }
    h1 { color: var(--text-bright); font-size: 1.7rem; font-weight: 600; display: flex; align-items: center; gap: 0.6rem; }
    .subtitle { font-size: 0.88rem; color: #8b949e; margin-top: 0.2rem; }
    .badge { background: var(--badge-bg); border: 1px solid var(--border); border-radius: 4px; padding: 0.2rem 0.55rem; font-size: 0.78rem; color: #8b949e; }
    
    .btn-sm { background: #21262d; border: 1px solid var(--border); color: #c9d1d9; padding: 0.4rem 0.85rem; font-size: 0.85rem; font-weight: 500; border-radius: 6px; cursor: pointer; text-decoration: none; display: inline-flex; align-items: center; gap: 0.4rem; transition: background 0.15s, color 0.15s; }
    .btn-sm:hover { background: #30363d; color: var(--text-bright); }
    .btn-primary { background: #1f6feb; border-color: #388bfd; color: #ffffff; }
    .btn-primary:hover { background: #388bfd; }

    /* Nav Pills */
    .nav-pills { display: flex; gap: 0.6rem; margin-bottom: 1.75rem; flex-wrap: wrap; }
    .nav-pill { background: var(--card-bg); border: 1px solid var(--border); color: #8b949e; padding: 0.45rem 0.9rem; border-radius: 6px; font-size: 0.85rem; font-weight: 600; text-decoration: none; display: inline-flex; align-items: center; gap: 0.4rem; transition: all 0.15s; }
    .nav-pill:hover, .nav-pill.active { background: #21262d; color: var(--text-bright); border-color: var(--primary); }

    /* Banner */
    .endpoint-banner { background: #1f6feb15; border: 1px solid #1f6feb44; border-radius: 8px; padding: 0.9rem 1.25rem; margin-bottom: 2rem; display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 0.75rem; }
    .endpoint-banner-title { font-size: 0.85rem; color: #8b949e; }
    .endpoint-banner-url { font-family: ui-monospace, monospace; font-weight: 600; color: #58a6ff; font-size: 0.95rem; }

    /* Card */
    .card { background: var(--card-bg); border: 1px solid var(--border); border-radius: 8px; padding: 1.5rem; margin-bottom: 2rem; scroll-margin-top: 1.5rem; }
    .card-header { display: flex; align-items: center; justify-content: space-between; margin-bottom: 1rem; border-bottom: 1px solid #21262d; padding-bottom: 0.75rem; }
    .card-title { font-size: 1.25rem; color: var(--text-bright); font-weight: 600; display: flex; align-items: center; gap: 0.6rem; }
    .tag { font-size: 0.75rem; padding: 0.2rem 0.6rem; border-radius: 12px; font-weight: 600; text-transform: uppercase; letter-spacing: 0.03em; }
    .tag-claude { background: #cc785c22; color: #cc785c; border: 1px solid #cc785c44; }
    .tag-codex { background: #10a37f22; color: #10a37f; border: 1px solid #10a37f44; }
    .tag-agy { background: #1a73e822; color: #58a6ff; border: 1px solid #388bfd44; }
    .tag-openai { background: #10a37f22; color: #10a37f; border: 1px solid #10a37f44; }

    p { margin-bottom: 0.75rem; font-size: 0.92rem; color: #c9d1d9; }
    .step-title { font-weight: 600; color: var(--text-bright); font-size: 0.95rem; margin-top: 1.25rem; margin-bottom: 0.4rem; display: flex; align-items: center; gap: 0.4rem; }

    /* Code Container */
    .code-box { position: relative; margin: 0.6rem 0 1.25rem 0; }
    pre { background: var(--code-bg); border: 1px solid var(--border); border-radius: 6px; padding: 1rem 1.1rem; overflow-x: auto; font-family: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace; font-size: 0.86rem; color: #e6edf3; line-height: 1.45; }
    code { font-family: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace; font-size: 0.88em; background: var(--code-bg); padding: 0.15rem 0.35rem; border-radius: 4px; color: #79c0ff; }
    pre code { background: none; padding: 0; color: inherit; font-size: inherit; }
    .copy-btn { position: absolute; top: 0.5rem; right: 0.5rem; background: #21262d; border: 1px solid var(--border); color: #8b949e; padding: 0.25rem 0.6rem; border-radius: 4px; font-size: 0.75rem; cursor: pointer; transition: all 0.15s; font-family: inherit; }
    .copy-btn:hover { background: #30363d; color: var(--text-bright); }
    .copy-btn.copied { background: #238636; color: #ffffff; border-color: #2ea043; }

    .note { background: #21262d55; border-left: 3px solid #58a6ff; padding: 0.6rem 0.9rem; font-size: 0.85rem; color: #8b949e; margin: 0.75rem 0; border-radius: 0 6px 6px 0; }
  </style>
</head>
<body>
  <div class="container">
    <header>
      <div>
        <h1>🗼 BabelGate</h1>
        <div class="subtitle">Client Configuration &amp; Integration Guide</div>
      </div>
      <div>
        <a href="/" class="btn-sm">← Back to Dashboard</a>
      </div>
    </header>

    <!-- Detected Endpoint Banner -->
    <div class="endpoint-banner">
      <div>
        <div class="endpoint-banner-title">Detected Router Base Endpoint</div>
        <div class="endpoint-banner-url" id="bannerOrigin">http://localhost:8080</div>
      </div>
      <div style="font-size: 0.82rem; color: #8b949e;">
        ✓ All code snippets below are automatically adapted to this router address.
      </div>
    </div>

    <!-- Quick Navigation Pills -->
    <div class="nav-pills">
      <a href="#claude" class="nav-pill">💬 Claude Code</a>
      <a href="#codex" class="nav-pill">🤖 Codex CLI (codex)</a>
      <a href="#agy" class="nav-pill">🚀 Antigravity CLI (agy)</a>
      <a href="#openai" class="nav-pill">🐍 OpenAI SDK &amp; cURL</a>
      <a href="#gemini" class="nav-pill">✨ Google GenAI SDK</a>
      <a href="#copilot" class="nav-pill">🐙 GitHub Copilot</a>
    </div>

    <!-- 1. Claude Code -->
    <div class="card" id="claude">
      <div class="card-header">
        <div class="card-title">
          <span>💬 Claude Code Setup</span>
          <span class="tag tag-claude">Anthropic Protocol</span>
        </div>
        <span class="badge">Native Messages API</span>
      </div>
      <p>Claude Code uses the Anthropic Messages API. BabelGate serves Anthropic requests directly on <code>/v1/messages</code> and translates them to any configured upstream provider (OpenAI, Anthropic, Gemini, Azure, etc.).</p>

      <div class="step-title">Configuration in <code>.claude/settings.json</code>:</div>
      <p>The following environment settings are needed in your project's <code>.claude/settings.json</code> (or global <code>~/.claude/settings.json</code>):</p>
      <div class="code-box">
        <button class="copy-btn" onclick="copyCode(this)">Copy</button>
        <pre><code class="lang-json">{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:8080/",
    "ANTHROPIC_API_KEY": "dummy",
    "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS": "1",
    "CLAUDE_CODE_ENABLE_TELEMETRY": "1",
    "CLAUDE_CODE_USE_BEDROCK": "0",
    "DISABLE_PROMPT_CACHING": "0",
    "CLAUDE_CODE_DISABLE_1M_CONTEXT": "0",
    "CLAUDE_CODE_BLOCKING_LIMIT_OVERRIDE": "200000",
    "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE": "50",
    "CLAUDE_CODE_EFFORT_LEVEL": "medium",
    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"
  }
}</code></pre>
      </div>

      <div class="step-title">Quick Launch (Current Terminal Session):</div>
      <div class="code-box">
        <button class="copy-btn" onclick="copyCode(this)">Copy</button>
        <pre><code class="lang-sh">export ANTHROPIC_BASE_URL="http://localhost:8080"
export ANTHROPIC_API_KEY="dummy-key"
claude</code></pre>
      </div>

      <div class="step-title">Run with a Specific Model:</div>
      <p>You can request any model or router alias defined in your configuration:</p>
      <div class="code-box">
        <button class="copy-btn" onclick="copyCode(this)">Copy</button>
        <pre><code class="lang-sh"># Use Claude 3.7 Sonnet or any router model
claude --model claude-3-7-sonnet

# Or route Claude Code prompts to GPT-4o or Gemini through router aliases:
claude --model gpt-4o</code></pre>
      </div>

      <div class="step-title">Persistent Shell Configuration:</div>
      <p>Add these environment variables to your shell startup file (<code>~/.zshrc</code> or <code>~/.bashrc</code>):</p>
      <div class="code-box">
        <button class="copy-btn" onclick="copyCode(this)">Copy</button>
        <pre><code class="lang-sh"># BabelGate configuration for Claude Code
export ANTHROPIC_BASE_URL="http://localhost:8080"
export ANTHROPIC_API_KEY="dummy-key"</code></pre>
      </div>
    </div>

    <!-- 2. Codex CLI -->
    <div class="card" id="codex">
      <div class="card-header">
        <div class="card-title">
          <span>🤖 OpenAI Codex CLI (<code style="font-size: 1rem; color: #7ee787;">codex</code>)</span>
          <span class="tag tag-codex">OpenAI Protocol</span>
        </div>
        <span class="badge">Chat Completions API</span>
      </div>
      <p>The OpenAI Codex CLI connects via the OpenAI Chat Completions API. BabelGate handles requests at <code>/v1/chat/completions</code> and automatically routes to any upstream model.</p>

      <div class="step-title">Option A: Environment Variables (Quick Run):</div>
      <div class="code-box">
        <button class="copy-btn" onclick="copyCode(this)">Copy</button>
        <pre><code class="lang-sh">export OPENAI_BASE_URL="http://localhost:8080/v1"
export OPENAI_API_KEY="dummy-key"
codex</code></pre>
      </div>

      <div class="step-title">Option B: Persistent Configuration (<code>~/.codex/config.toml</code>):</div>
      <p>Set <code>openai_base_url</code> in your Codex configuration file (<code>~/.codex/config.toml</code>):</p>
      <div class="code-box">
        <button class="copy-btn" onclick="copyCode(this)">Copy</button>
        <pre><code class="lang-toml"># Add to ~/.codex/config.toml
openai_base_url = "http://localhost:8080/v1"</code></pre>
      </div>

      <div class="step-title">Option C: Command-Line Flag Override:</div>
      <p>Pass the router endpoint dynamically using the <code>-c</code> configuration override flag:</p>
      <div class="code-box">
        <button class="copy-btn" onclick="copyCode(this)">Copy</button>
        <pre><code class="lang-sh"># Connect to router via CLI flag
codex -c openai_base_url="http://localhost:8080/v1"

# Specify a target model or routed alias:
codex -c openai_base_url="http://localhost:8080/v1" -m gpt-4o</code></pre>
      </div>
    </div>

    <!-- 3. Antigravity CLI (agy) -->
    <div class="card" id="agy">
      <div class="card-header">
        <div class="card-title">
          <span>🚀 Google Antigravity CLI (<code style="font-size: 1rem; color: #58a6ff;">agy</code>)</span>
          <span class="tag tag-agy">Gemini Protocol</span>
        </div>
        <span class="badge">Google Gemini API</span>
      </div>
      <p>The Antigravity CLI (<code>agy</code>) supports direct Gemini API connections. BabelGate serves Google Gemini endpoints natively on <code>/v1beta/models/...</code> and <code>/v1/models/...</code>.</p>

      <div class="step-title">Step 1: Set modelProvider in <code>~/.gemini/antigravity-cli/settings.json</code>:</div>
      <p>Ensure <code>modelProvider</code> is configured to <code>"gemini"</code>:</p>
      <div class="code-box">
        <button class="copy-btn" onclick="copyCode(this)">Copy</button>
        <pre><code class="lang-json">{
  "modelProvider": "gemini"
}</code></pre>
      </div>

      <div class="step-title">Step 2: Set Environment Variables &amp; Launch:</div>
      <p>Point <code>GOOGLE_GEMINI_BASE_URL</code> to the router:</p>
      <div class="code-box">
        <button class="copy-btn" onclick="copyCode(this)">Copy</button>
        <pre><code class="lang-sh">export GOOGLE_GEMINI_BASE_URL="http://localhost:8080"
export GEMINI_API_KEY="dummy-key"
agy</code></pre>
      </div>

      <div class="step-title">Step 3: Specify a Model:</div>
      <div class="code-box">
        <button class="copy-btn" onclick="copyCode(this)">Copy</button>
        <pre><code class="lang-sh"># Run agy with a native Gemini model or router alias
agy --model gemini-2.5-flash

# You can also use routed aliases (e.g. Claude or GPT):
agy --model claude-3-7-sonnet</code></pre>
      </div>
      <div class="note">
        <strong>Tip:</strong> You can permanently append <code>export GOOGLE_GEMINI_BASE_URL="http://localhost:8080"</code> and <code>export GEMINI_API_KEY="dummy-key"</code> to your <code>~/.zshrc</code>.
      </div>
    </div>

    <!-- 4. OpenAI SDK & cURL -->
    <div class="card" id="openai">
      <div class="card-header">
        <div class="card-title">
          <span>🐍 OpenAI SDK &amp; HTTP / cURL</span>
          <span class="tag tag-openai">Standard OpenAI</span>
        </div>
        <span class="badge">Python &amp; REST</span>
      </div>

      <div class="step-title">Python (OpenAI Official SDK):</div>
      <div class="code-box">
        <button class="copy-btn" onclick="copyCode(this)">Copy</button>
        <pre><code class="lang-python">from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="dummy-key"  # Router ignores if no auth configured
)

response = client.chat.completions.create(
    model="gpt-4o",
    messages=[
        {"role": "system", "content": "You are a helpful assistant."},
        {"role": "user", "content": "Explain router architecture in one sentence."}
    ]
)
print(response.choices[0].message.content)</code></pre>
      </div>

      <div class="step-title">cURL (OpenAI Chat Completions):</div>
      <div class="code-box">
        <button class="copy-btn" onclick="copyCode(this)">Copy</button>
        <pre><code class="lang-sh">curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer dummy-key" \
  -d '{
    "model": "gpt-4o",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'</code></pre>
      </div>
    </div>

    <!-- 5. Google GenAI SDK -->
    <div class="card" id="gemini">
      <div class="card-header">
        <div class="card-title">
          <span>✨ Google GenAI SDK (Python)</span>
          <span class="tag tag-agy">Google GenAI</span>
        </div>
        <span class="badge">Python</span>
      </div>

      <div class="step-title">Python (Google GenAI Client):</div>
      <div class="code-box">
        <button class="copy-btn" onclick="copyCode(this)">Copy</button>
        <pre><code class="lang-python">from google import genai
from google.genai import types

client = genai.Client(
    api_key="dummy-key",
    http_options=types.HttpOptions(
        base_url="http://localhost:8080"
    )
)

response = client.models.generate_content(
    model="gemini-2.5-flash",
    contents="Hello from BabelGate!"
)
print(response.text)</code></pre>
      </div>
    </div>

    <!-- 6. GitHub Copilot -->
    <div class="card" id="copilot">
      <div class="card-header">
        <div class="card-title">
          <span>🐙 GitHub Copilot Setup</span>
          <span class="tag tag-openai">Upstream Destination</span>
        </div>
        <span class="badge">Device Flow &amp; API</span>
      </div>

      <p>Use GitHub Copilot as a destination LLM provider. Route prompts, tools, and streams from Claude Code, OpenAI SDK, or Gemini SDK directly to Copilot models (<code>copilot/gpt-4o</code>, <code>copilot/gpt-4o-mini</code>, <code>copilot/claude-3.5-sonnet</code>, <code>copilot/o1</code>, etc.).</p>

      <div class="step-title">Option A: CLI Device Registration (Recommended)</div>
      <p>Authenticate GitHub Copilot directly from your terminal using the built-in CLI device code flow:</p>
      <div class="code-box">
        <button class="copy-btn" onclick="copyCode(this)">Copy</button>
        <pre><code class="lang-sh">./bin/babelgate -copilot-login</code></pre>
      </div>
      <p style="font-size: 0.85rem; color: #8b949e;">Follow the terminal instructions to open GitHub, enter the 8-character code, and authorize. Credentials are securely stored to <code>~/.config/github-copilot/hosts.json</code> and loaded automatically on startup.</p>

      <div class="step-title">Option B: Configuration in config.yaml</div>
      <div class="code-box">
        <button class="copy-btn" onclick="copyCode(this)">Copy</button>
        <pre><code class="lang-yaml">providers:
  copilot:
    type: copilot
    # api_key is optional: automatically discovered from device login,
    # or ~/.config/github-copilot/apps.json, or GITHUB_TOKEN environment variable.

routing:
  routes:
    # Route Claude Code requests to Copilot's GPT-4o
    "claude-3-7-sonnet": "copilot/gpt-4o"
    "claude-3-5-sonnet": "copilot/gpt-4o-mini"</code></pre>
      </div>
    </div>
  </div>

  <script>
    // Dynamically replace default http://localhost:8080 with actual host/port
    const origin = window.location.origin;
    if (origin && origin !== 'null') {
      document.getElementById('bannerOrigin').textContent = origin;
      document.querySelectorAll('pre code').forEach(el => {
        el.textContent = el.textContent.split('http://localhost:8080').join(origin);
      });
    }

    // Copy to clipboard helper
    function copyCode(btn) {
      const pre = btn.nextElementSibling;
      const text = pre ? pre.textContent : '';
      navigator.clipboard.writeText(text).then(() => {
        const origText = btn.textContent;
        btn.textContent = 'Copied! ✓';
        btn.classList.add('copied');
        setTimeout(() => {
          btn.textContent = origText;
          btn.classList.remove('copied');
        }, 2000);
      }).catch(err => {
        console.error('Failed to copy text: ', err);
      });
    }
  </script>
</body>
</html>
`
