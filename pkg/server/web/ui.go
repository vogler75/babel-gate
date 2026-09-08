package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/vogler75/babel-gate/pkg/providers/copilot"
	"github.com/vogler75/babel-gate/pkg/router"
	"github.com/vogler75/babel-gate/pkg/session"
)

type DashboardHandler struct {
	engine   *router.Engine
	catalog  *router.Catalog
	sessions *session.Manager
}

func NewDashboardHandler(engine *router.Engine, catalog *router.Catalog, sessions *session.Manager) *DashboardHandler {
	return &DashboardHandler{
		engine:   engine,
		catalog:  catalog,
		sessions: sessions,
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
	providersMap := d.engine.GetProviders()
	provList := make([]map[string]any, 0, len(providersMap))
	for name, p := range providersMap {
		provList = append(provList, map[string]any{
			"name":     name,
			"type":     p.Type(),
			"priority": d.engine.GetProviderPriority(name),
		})
	}

	routes := d.engine.GetRoutes()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "healthy",
		"providers": provList,
		"routes":    routes,
	})
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

func (d *DashboardHandler) HandleCopilotDeviceCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	dcr, err := copilot.RequestDeviceCode(r.Context(), nil)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(dcr)
}

func (d *DashboardHandler) HandleCopilotPoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		DeviceCode string `json:"device_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DeviceCode == "" {
		http.Error(w, "missing device_code", http.StatusBadRequest)
		return
	}

	payload := map[string]string{
		"client_id":   copilot.DefaultClientID,
		"device_code": req.DeviceCode,
		"grant_type":  "urn:ietf:params:oauth:grant-type:device_code",
	}
	bodyBytes, _ := json.Marshal(payload)

	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, copilot.OAuthTokenURL, bytes.NewReader(bodyBytes))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": err.Error()})
		return
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": err.Error()})
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	var otr copilot.OAuthTokenResponse
	_ = json.Unmarshal(respBody, &otr)

	w.Header().Set("Content-Type", "application/json")
	if otr.AccessToken != "" {
		username, _ := copilot.GetAuthenticatedUser(r.Context(), nil, otr.AccessToken)
		_ = copilot.SaveTokenToDisk(otr.AccessToken, username)

		// Dynamically register or update copilot provider in the live engine
		client := copilot.NewClient("copilot", otr.AccessToken, "", nil, nil)
		d.engine.RegisterProvider(client)

		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":   "success",
			"username": username,
		})
		return
	}

	if otr.Error == "authorization_pending" {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "pending"})
		return
	}
	if otr.Error == "slow_down" {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "slow_down"})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":  "error",
		"error":   otr.Error,
		"message": otr.ErrorDescription,
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
    .container { max-width: 1140px; margin: 0 auto; }
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
    .pill-openai { background: #10a37f22; color: #10a37f; border: 1px solid #10a37f; }
    .pill-anthropic { background: #cc785c22; color: #cc785c; border: 1px solid #cc785c; }
    .pill-google { background: #1a73e822; color: #58a6ff; border: 1px solid #388bfd; }
    .pill-azure { background: #0078d422; color: #38bdf8; border: 1px solid #0284c7; }
    .pill-mistral { background: #ea580c22; color: #fb923c; border: 1px solid #f97316; }
    .pill-cohere { background: #14b8a622; color: #2dd4bf; border: 1px solid #14b8a6; }
    .pill-alias { background: #a371f722; color: #bc8cff; border: 1px solid #8957e5; }
    .pill-default { background: #388bfd1a; color: #79c0ff; border: 1px solid #388bfd66; }
    
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
    
    .token-in { color: var(--stat-in); font-weight: 600; font-family: ui-monospace, monospace; }
    .token-out { color: var(--stat-out); font-weight: 600; font-family: ui-monospace, monospace; }
    .token-total { color: var(--stat-total); font-weight: 700; font-family: ui-monospace, monospace; }
    
    .sub-table { width: 100%; background: #0c1016; border-radius: 6px; margin: 0.5rem 0; border: 1px solid var(--border); }
    .sub-table th, .sub-table td { padding: 0.45rem 0.6rem; font-size: 0.8rem; }
    .sub-table th { background: #161b22; }
    
    #responseOutput { min-height: 120px; white-space: pre-wrap; word-break: break-word; color: #7ee787; }
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
              <th>Input Tokens</th>
              <th>Output Tokens</th>
              <th>Total Tokens</th>
              <th>Last Active</th>
              <th>Action</th>
            </tr>
          </thead>
          <tbody id="sessionsTable">
            <tr><td colspan="9" style="text-align: center; color: #8b949e; padding: 1.5rem;">No active sessions yet. Use Claude Code, OpenAI SDK, or the playground below.</td></tr>
          </tbody>
        </table>
      </div>
    </div>

    <!-- Active Providers -->
    <div class="card" style="margin-bottom: 1.75rem;">
      <h2 style="margin-bottom: 0.75rem;">Connected Upstream Providers <span class="badge" id="provCount">0</span></h2>
      <div style="overflow-x: auto;">
        <table>
          <thead>
            <tr><th>Name</th><th>Driver</th><th>Priority</th><th>Status</th></tr>
          </thead>
          <tbody id="providersTable">
            <tr><td colspan="4">Loading providers...</td></tr>
          </tbody>
        </table>
      </div>
    </div>

    <!-- Active Models Catalog -->
    <div class="card">
      <h2>Available Models Catalog <span class="badge" id="modelCount">0</span></h2>
      <div style="overflow-x: auto;">
        <table>
          <thead>
            <tr><th>Model Identifier</th><th>Provider / Route</th><th>Type</th><th>Description</th></tr>
          </thead>
          <tbody id="modelsTable">
            <tr><td colspan="4">Loading models catalog...</td></tr>
          </tbody>
        </table>
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
    const openDetails = new Set();

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
          tableBody.innerHTML = '<tr><td colspan="9" style="text-align: center; color: #8b949e; padding: 1.5rem;">No active sessions yet. Use Claude Code, OpenAI SDK, or the playground below.</td></tr>';
          return;
        }

        tableBody.innerHTML = '';
        sessions.forEach(s => {
          const clientPill = getClientPillClass(s.client);
          const modelsHtml = (s.models && s.models.length > 0)
            ? s.models.map(m => '<span class="pill ' + getProviderPillClass(m) + '" style="margin-right: 0.25rem; margin-bottom: 0.25rem;">' + escapeHtml(m) + '</span>').join('')
            : '<span style="color: #8b949e;">-</span>';

          const reqCount = s.request_count || 0;
          const isExpanded = openDetails.has(s.id);

          const tr = document.createElement('tr');
          tr.innerHTML = '<td><code>' + escapeHtml(s.id) + '</code></td>' +
            '<td><span class="pill ' + clientPill + '">' + escapeHtml(s.client || 'Client') + '</span></td>' +
            '<td>' + modelsHtml + '</td>' +
            '<td><strong>' + formatNumber(reqCount) + '</strong></td>' +
            '<td><span class="token-in">' + formatNumber(s.input_tokens) + '</span></td>' +
            '<td><span class="token-out">' + formatNumber(s.output_tokens) + '</span></td>' +
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
              '<th>Time</th><th>Model</th><th>Type</th><th>Duration</th><th>Input Tokens</th><th>Output Tokens</th><th>Total</th><th>Status</th>' +
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
                  '<td><span class="token-total">' + formatNumber(r.total_tokens) + '</span></td>' +
                  '<td><span style="color: ' + statusColor + ';">● ' + escapeHtml(r.status) + '</span></td>' +
                  '</tr>';
              }).join('') +
              '</tbody></table>';
          }

          detailsTr.innerHTML = '<td colspan="9" style="background: #11151c; padding: 0.75rem 1rem; border-top: 1px dashed var(--border);">' +
            '<div style="font-size: 0.82rem; font-weight: 600; color: #8b949e; margin-bottom: 0.35rem;">Request History for Session ' + escapeHtml(s.id) + '</div>' +
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
            id: isAlias ? m.id : cleanName,
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

    async function loadData() {
      try {
        const [statusRes, modelsRes] = await Promise.all([
          fetch('/api/status').then(r => r.json()),
          fetch('/api/models').then(r => r.json())
        ]);

        allProviders = statusRes.providers || [];
        allModels = modelsRes.models || [];

        // Providers table
        const provBody = document.getElementById('providersTable');
        provBody.innerHTML = '';
        document.getElementById('provCount').textContent = allProviders.length;
        allProviders.forEach(p => {
          const tr = document.createElement('tr');
          const pillClass = getProviderPillClass(p.type || p.name);
          const prioText = p.priority !== undefined ? ('Prio ' + p.priority) : 'Prio 100';
          tr.innerHTML = '<td><strong>' + escapeHtml(p.name) + '</strong></td>' +
            '<td><span class="pill ' + pillClass + '">' + escapeHtml(p.type) + '</span></td>' +
            '<td><span class="badge" style="font-weight: 600; color: #58a6ff;">' + prioText + '</span></td>' +
            '<td><span style="color: #3fb950;">● Online</span></td>';
          provBody.appendChild(tr);
        });

        // Provider dropdown
        const provSelect = document.getElementById('providerSelect');
        provSelect.innerHTML = '<option value="">All Providers</option>';
        allProviders.forEach(p => {
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
        const modelBody = document.getElementById('modelsTable');
        modelBody.innerHTML = '';

        const displayedModels = [];
        const seenInTable = new Set();
        allModels.forEach(m => {
          let cleanId = m.id;
          if (m.provider && cleanId.startsWith(m.provider + '/')) {
            cleanId = cleanId.slice(m.provider.length + 1);
          }
          const dedupeKey = (m.provider || '') + ':' + cleanId;
          if (seenInTable.has(dedupeKey)) return;
          seenInTable.add(dedupeKey);
          displayedModels.push({
            ...m,
            id: cleanId
          });
        });

        document.getElementById('modelCount').textContent = displayedModels.length;

        displayedModels.forEach(m => {
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
          modelBody.appendChild(tr);
        });

        // Populate Model dropdown
        onProviderChange();
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
        // Refresh sessions immediately to show updated counts
        loadSessions();
      }
    }

    // Initial load
    loadData();
    loadSessions();

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

      <!-- Interactive Device Registration Widget -->
      <div style="background: #161b22; border: 1px solid #30363d; border-radius: 8px; padding: 1.25rem; margin: 1rem 0;">
        <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 0.75rem; flex-wrap: wrap; gap: 0.5rem;">
          <strong style="color: var(--text-bright); font-size: 0.95rem;">🔑 Connect with GitHub Device Code</strong>
          <button id="copilotAuthBtn" onclick="startCopilotRegistration()" class="btn-sm btn-primary" style="font-weight: 600;">Start Device Registration</button>
        </div>
        <p style="font-size: 0.85rem; color: #8b949e; margin-bottom: 0;">Connect your GitHub account without creating personal access tokens manually. You will receive an 8-character code to enter at GitHub.</p>

        <!-- Dynamic Auth Box -->
        <div id="copilotAuthBox" style="display: none; margin-top: 1rem; padding-top: 1rem; border-top: 1px solid #21262d;">
          <div style="display: flex; align-items: center; gap: 1rem; flex-wrap: wrap; margin-bottom: 1rem;">
            <div>
              <div style="font-size: 0.75rem; color: #8b949e; text-transform: uppercase; letter-spacing: 0.05em;">Device Code:</div>
              <div id="copilotUserCode" style="font-size: 1.6rem; font-weight: 700; color: #58a6ff; font-family: ui-monospace, monospace; letter-spacing: 0.1em;">----</div>
            </div>
            <button class="btn-sm" onclick="copyCopilotCode(this)" id="copilotCopyBtn">Copy Code</button>
            <a id="copilotAuthLink" href="https://github.com/login/device" target="_blank" rel="noopener noreferrer" class="btn-sm btn-primary" style="display: inline-flex; align-items: center; gap: 0.35rem;">
              Open https://github.com/login/device &rarr;
            </a>
          </div>
          <div id="copilotStatusMsg" style="font-size: 0.88rem; color: #d29922; display: flex; align-items: center; gap: 0.4rem;">
            <span>⏳ Waiting for you to authorize in your browser...</span>
          </div>
        </div>
      </div>

      <div class="step-title">Option B: CLI Device Registration</div>
      <div class="code-box">
        <button class="copy-btn" onclick="copyCode(this)">Copy</button>
        <pre><code class="lang-sh">./bin/llm-router -copilot-login</code></pre>
      </div>

      <div class="step-title">Option C: Configuration in config.yaml</div>
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

    let copilotPollTimer = null;
    async function startCopilotRegistration() {
      const btn = document.getElementById('copilotAuthBtn');
      const box = document.getElementById('copilotAuthBox');
      const codeEl = document.getElementById('copilotUserCode');
      const linkEl = document.getElementById('copilotAuthLink');
      const statusEl = document.getElementById('copilotStatusMsg');

      btn.disabled = true;
      btn.textContent = 'Requesting code...';

      try {
        const resp = await fetch('/api/auth/copilot/device-code', { method: 'POST' });
        if (!resp.ok) {
          const err = await resp.json();
          throw new Error(err.error || 'Failed to request device code');
        }
        const data = await resp.json();

        codeEl.textContent = data.user_code;
        linkEl.href = data.verification_uri || 'https://github.com/login/device';
        box.style.display = 'block';
        btn.textContent = 'Code Requested ✓';

        statusEl.innerHTML = '<span style="color: #d29922;">⏳ Waiting for authorization at <a href="' + (data.verification_uri || 'https://github.com/login/device') + '" target="_blank" style="color: #58a6ff;">github.com/login/device</a>...</span>';

        // Auto poll
        if (copilotPollTimer) clearInterval(copilotPollTimer);
        copilotPollTimer = setInterval(async () => {
          try {
            const pollResp = await fetch('/api/auth/copilot/poll', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ device_code: data.device_code })
            });
            const pollData = await pollResp.json();
            if (pollData.status === 'success') {
              clearInterval(copilotPollTimer);
              statusEl.innerHTML = '<span style="color: #3fb950; font-weight: 600;">🎉 Connected successfully as @' + (pollData.username || 'user') + '! GitHub Copilot models are now active.</span>';
              btn.textContent = 'Connected ✓';
              btn.style.background = '#238636';
            } else if (pollData.status === 'error') {
              clearInterval(copilotPollTimer);
              statusEl.innerHTML = '<span style="color: #f85149;">❌ ' + (pollData.message || 'Authorization failed') + '</span>';
              btn.disabled = false;
              btn.textContent = 'Retry Registration';
            }
          } catch (e) {}
        }, (data.interval || 5) * 1000);

      } catch (err) {
        btn.disabled = false;
        btn.textContent = 'Start Device Registration';
        alert('Error starting device registration: ' + err.message);
      }
    }

    function copyCopilotCode(btn) {
      const code = document.getElementById('copilotUserCode').textContent;
      navigator.clipboard.writeText(code).then(() => {
        const orig = btn.textContent;
        btn.textContent = 'Copied! ✓';
        setTimeout(() => btn.textContent = orig, 2000);
      });
    }
  </script>
</body>
</html>
`

