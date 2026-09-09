package server

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/vogler75/babel-gate/pkg/config"
	"github.com/vogler75/babel-gate/pkg/metrics"
	"github.com/vogler75/babel-gate/pkg/router"
	"github.com/vogler75/babel-gate/pkg/server/inbound"
	"github.com/vogler75/babel-gate/pkg/server/web"
	"github.com/vogler75/babel-gate/pkg/session"
)

type Server struct {
	cfg        *config.Config
	engine     *router.Engine
	catalog    *router.Catalog
	sessions   *session.Manager
	metrics    *metrics.Store
	httpServer *http.Server
}

func NewServer(cfg *config.Config, engine *router.Engine) *Server {
	catalog := router.NewCatalog(engine)
	sessions := session.NewManager()

	metricsStore, err := metrics.NewStore(cfg.Database.Path, cfg.Database.RetentionDays)
	if err != nil {
		log.Printf("Warning: failed to initialize SQLite metrics store at %s: %v", cfg.Database.Path, err)
	} else {
		sessions.SetMetricsRecorder(metricsStore)
	}

	anthropicHandler := inbound.NewAnthropicHandler(engine, catalog, sessions)
	openaiHandler := inbound.NewOpenAIHandler(engine, catalog, sessions)
	googleHandler := inbound.NewGoogleHandler(engine, catalog, sessions)
	dashboardHandler := web.NewDashboardHandler(engine, catalog, sessions, metricsStore)

	mux := http.NewServeMux()

	// Web Dashboard & Management APIs
	mux.HandleFunc("/", dashboardHandler.HandleIndex)
	mux.HandleFunc("/setup", dashboardHandler.HandleSetup)
	mux.HandleFunc("/api/status", dashboardHandler.HandleAPIStatus)
	mux.HandleFunc("/api/models", dashboardHandler.HandleAPIModels)
	mux.HandleFunc("/api/sessions", dashboardHandler.HandleAPISessions)
	mux.HandleFunc("/api/sessions/clear", dashboardHandler.HandleAPIClearSessions)
	mux.HandleFunc("/api/metrics/summary", dashboardHandler.HandleAPIMetricsSummary)
	mux.HandleFunc("/api/metrics/daily", dashboardHandler.HandleAPIMetricsDaily)
	mux.HandleFunc("/api/metrics/hourly", dashboardHandler.HandleAPIMetricsHourly)

	// Anthropic Messages API (Claude Code / Anthropic SDK)
	mux.HandleFunc("/v1/messages", anthropicHandler.HandleMessages)
	mux.HandleFunc("/anthropic/v1/messages", anthropicHandler.HandleMessages)
	mux.HandleFunc("/anthropic/v1/models", anthropicHandler.HandleModels)

	// OpenAI Chat Completions API
	mux.HandleFunc("/v1/chat/completions", openaiHandler.HandleChatCompletions)
	mux.HandleFunc("/openai/v1/chat/completions", openaiHandler.HandleChatCompletions)
	mux.HandleFunc("/openai/v1/models", openaiHandler.HandleModels)

	// Unified models endpoint
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		// If request has Anthropic headers or ?format=anthropic, return Anthropic models format
		if r.Header.Get("anthropic-version") != "" || r.Header.Get("x-api-key") != "" || r.URL.Query().Get("format") == "anthropic" {
			anthropicHandler.HandleModels(w, r)
			return
		}
		openaiHandler.HandleModels(w, r)
	})

	// Google Gemini API (v1beta and v1)
	googleDispatch := func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasSuffix(path, ":generateContent") {
			googleHandler.HandleGenerateContent(w, r)
		} else if strings.HasSuffix(path, ":streamGenerateContent") {
			googleHandler.HandleStreamGenerateContent(w, r)
		} else if strings.HasSuffix(path, "/models") || strings.HasSuffix(path, "/models/") {
			googleHandler.HandleModels(w, r)
		} else {
			googleHandler.HandleModels(w, r)
		}
	}

	mux.HandleFunc("/v1beta/models/", googleDispatch)
	mux.HandleFunc("/v1beta/models", googleHandler.HandleModels)
	mux.HandleFunc("/v1/models/", googleDispatch)

	// Wrap middleware chain
	handler := loggingMiddleware(corsMiddleware(authMiddleware(cfg.Server.APIKey, mux)))

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  time.Duration(cfg.Server.TimeoutSeconds) * time.Second,
		WriteTimeout: time.Duration(cfg.Server.TimeoutSeconds) * time.Second,
	}

	return &Server{
		cfg:        cfg,
		engine:     engine,
		catalog:    catalog,
		sessions:   sessions,
		metrics:    metricsStore,
		httpServer: srv,
	}
}

func (s *Server) Start() error {
	log.Printf("Starting BabelGate on http://localhost:%d", s.cfg.Server.Port)
	return s.httpServer.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.metrics != nil {
		_ = s.metrics.Close()
	}
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) Engine() *router.Engine {
	return s.engine
}

func (s *Server) Catalog() *router.Catalog {
	return s.catalog
}

func (s *Server) Sessions() *session.Manager {
	return s.sessions
}

func (s *Server) Metrics() *metrics.Store {
	return s.metrics
}

func (s *Server) Config() *config.Config {
	return s.cfg
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hijacker.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// loggingMiddleware logs HTTP request details.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)

		clientName := session.DetectClient(r.Header.Get("x-client"), r.UserAgent())
		sessID := inbound.ExtractSessionID(r)

		var clientInfo []string
		if clientName != "unknown" && clientName != "" {
			clientInfo = append(clientInfo, "client: "+clientName)
		}
		if sessID != "" {
			if len(sessID) > 12 {
				clientInfo = append(clientInfo, "sess: "+sessID[:12]+"…")
			} else {
				clientInfo = append(clientInfo, "sess: "+sessID)
			}
		}

		clientMeta := ""
		if len(clientInfo) > 0 {
			clientMeta = fmt.Sprintf(" (%s)", strings.Join(clientInfo, ", "))
		}

		log.Printf("[%s] %s -> %d %s%s took %v", r.Method, r.URL.Path, sw.status, r.RemoteAddr, clientMeta, time.Since(start))
	})
}

// corsMiddleware sets CORS headers and handles preflight requests.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key, anthropic-version, anthropic-beta, x-goog-api-key, x-session-id, x-client")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// authMiddleware enforces router-level API key if configured.
func authMiddleware(requiredKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requiredKey == "" {
			next.ServeHTTP(w, r)
			return
		}

		// Don't require key for root dashboard, health, catalog, sessions, or metrics
		if r.URL.Path == "/" || r.URL.Path == "/setup" || strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}

		// Check Bearer token, x-api-key, or x-goog-api-key
		authHeader := r.Header.Get("Authorization")
		apiKey := r.Header.Get("x-api-key")
		googKey := r.Header.Get("x-goog-api-key")

		token := ""
		if strings.HasPrefix(authHeader, "Bearer ") {
			token = strings.TrimPrefix(authHeader, "Bearer ")
		} else if apiKey != "" {
			token = apiKey
		} else if googKey != "" {
			token = googKey
		}

		if token != requiredKey {
			http.Error(w, "Unauthorized: invalid router API key", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}
