package metrics

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ModelMetric captures aggregated counters for a specific model within a bucket.
type ModelMetric struct {
	Model        string `json:"model"`
	Provider     string `json:"provider"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	TotalTokens  int64  `json:"total_tokens"`
	Requests     int64  `json:"requests"`
	Errors       int64  `json:"errors"`
}

// ProviderMetric captures aggregated counters for a provider within a bucket.
type ProviderMetric struct {
	Provider     string `json:"provider"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	TotalTokens  int64  `json:"total_tokens"`
	Requests     int64  `json:"requests"`
	Errors       int64  `json:"errors"`
}

// DailyMetricBucket contains metrics aggregated for a single day.
type DailyMetricBucket struct {
	Date         string        `json:"date"` // YYYY-MM-DD
	TotalTokens  int64         `json:"total_tokens"`
	InputTokens  int64         `json:"input_tokens"`
	OutputTokens int64         `json:"output_tokens"`
	Requests     int64         `json:"requests"`
	Errors       int64         `json:"errors"`
	Models       []ModelMetric `json:"models"` // Sorted by TotalTokens desc
}

// HourlyMetricBucket contains metrics aggregated for a single hour.
type HourlyMetricBucket struct {
	Hour         string        `json:"hour"`      // "00" through "23"
	Timestamp    string        `json:"timestamp"` // ISO8601 UTC: YYYY-MM-DDTHH:00:00Z
	TotalTokens  int64         `json:"total_tokens"`
	InputTokens  int64         `json:"input_tokens"`
	OutputTokens int64         `json:"output_tokens"`
	Requests     int64         `json:"requests"`
	Errors       int64         `json:"errors"`
	Models       []ModelMetric `json:"models"` // Sorted by TotalTokens desc
}

// MetricsSummary contains overall totals and top lists for a time range.
type MetricsSummary struct {
	TotalTokens        int64            `json:"total_tokens"`
	InputTokens        int64            `json:"input_tokens"`
	OutputTokens       int64            `json:"output_tokens"`
	Requests           int64            `json:"requests"`
	Errors             int64            `json:"errors"`
	TopModels          []ModelMetric    `json:"top_models"`
	TopProviders       []ProviderMetric `json:"top_providers"`
	AvailableProviders []string         `json:"available_providers"`
	AvailableModels    []string         `json:"available_models"`
	StartDate          string           `json:"start_date"`
	EndDate            string           `json:"end_date"`
}

// ModelSpeedPoint captures throughput for a specific model within a time bucket.
type ModelSpeedPoint struct {
	Model                string  `json:"model"`
	Provider             string  `json:"provider"`
	Requests             int64   `json:"requests"`
	OutputTokens         int64   `json:"output_tokens"`
	GenerationDurationMs int64   `json:"generation_duration_ms"`
	TokensPerSecond      float64 `json:"tokens_per_second"`
}

// SpeedBucket contains model throughput metrics for a single time bucket (minute, hour, or day).
type SpeedBucket struct {
	Bucket    string            `json:"bucket"`    // Display key: "YYYY-MM-DD HH:MM", "YYYY-MM-DD HH:00", or "YYYY-MM-DD"
	Timestamp string            `json:"timestamp"` // ISO8601 UTC
	Models    []ModelSpeedPoint `json:"models"`    // Sorted by TokensPerSecond desc
}

// ModelOverallSpeed summarizes speed stats for a single model across the full selected range.
type ModelOverallSpeed struct {
	Model                string  `json:"model"`
	Provider             string  `json:"provider"`
	TotalRequests        int64   `json:"total_requests"`
	TotalOutputTokens    int64   `json:"total_output_tokens"`
	GenerationDurationMs int64   `json:"generation_duration_ms"`
	AvgTokensPerSecond   float64 `json:"avg_tokens_per_second"`
	MinTokensPerSecond   float64 `json:"min_tokens_per_second,omitempty"`
	MaxTokensPerSecond   float64 `json:"max_tokens_per_second,omitempty"`
}

// SpeedMetricsResponse is the response payload for model speed analytics.
type SpeedMetricsResponse struct {
	Granularity string              `json:"granularity"` // "minute", "hour", "day"
	StartDate   string              `json:"start_date"`
	EndDate     string              `json:"end_date"`
	Provider    string              `json:"provider,omitempty"`
	Models      []ModelOverallSpeed `json:"models"`  // Ranked by AvgTokensPerSecond desc
	Buckets     []SpeedBucket       `json:"buckets"` // Chronological order
}

// Store manages SQLite persistence for hourly metrics.
type Store struct {
	mu            sync.RWMutex
	db            *sql.DB
	retentionDays int
}

// NewStore opens or creates the SQLite metrics database.
func NewStore(dbPath string, retentionDays int) (*Store, error) {
	if dbPath == "" {
		dbPath = "data/metrics.db"
	}

	// Ensure parent directory exists
	dir := filepath.Dir(dbPath)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("creating db directory %s: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite db at %s: %w", dbPath, err)
	}

	// Performance and concurrency settings
	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA busy_timeout = 5000;",
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA foreign_keys = ON;",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("executing pragma %q: %w", p, err)
		}
	}

	schema := `
	CREATE TABLE IF NOT EXISTS hourly_metrics (
		hour_timestamp TEXT NOT NULL,
		provider TEXT NOT NULL,
		model TEXT NOT NULL,
		requests INTEGER NOT NULL DEFAULT 0,
		input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens INTEGER NOT NULL DEFAULT 0,
		errors INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (hour_timestamp, provider, model)
	);
	CREATE INDEX IF NOT EXISTS idx_hourly_metrics_time ON hourly_metrics(hour_timestamp);
	CREATE INDEX IF NOT EXISTS idx_hourly_metrics_provider ON hourly_metrics(provider);
	CREATE INDEX IF NOT EXISTS idx_hourly_metrics_model ON hourly_metrics(model);

	CREATE TABLE IF NOT EXISTS sessions (
		id TEXT PRIMARY KEY,
		client TEXT NOT NULL,
		client_ip TEXT NOT NULL DEFAULT '',
		user_agent TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		last_active TEXT NOT NULL,
		last_model TEXT NOT NULL DEFAULT '',
		request_count INTEGER NOT NULL DEFAULT 0,
		context_tokens INTEGER NOT NULL DEFAULT 0,
		context_tokens_estimated INTEGER NOT NULL DEFAULT 0,
		input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens INTEGER NOT NULL DEFAULT 0,
		tokens_per_second REAL NOT NULL DEFAULT 0.0,
		generation_duration_ms INTEGER NOT NULL DEFAULT 0,
		measured_output_tokens INTEGER NOT NULL DEFAULT 0,
		models TEXT NOT NULL DEFAULT '[]',
		model_stats TEXT NOT NULL DEFAULT '{}'
	);
	CREATE INDEX IF NOT EXISTS idx_sessions_last_active ON sessions(last_active);

	CREATE TABLE IF NOT EXISTS session_requests (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		timestamp TEXT NOT NULL,
		provider TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL,
		stream INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0,
		generation_duration_ms INTEGER NOT NULL DEFAULT 0,
		input_tokens INTEGER NOT NULL DEFAULT 0,
		input_tokens_estimated INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens INTEGER NOT NULL DEFAULT 0,
		tokens_per_second REAL NOT NULL DEFAULT 0.0,
		status TEXT NOT NULL DEFAULT 'success',
		error_message TEXT NOT NULL DEFAULT '',
		FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
	);
	CREATE INDEX IF NOT EXISTS idx_session_requests_session ON session_requests(session_id, timestamp DESC);
	`
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initializing db schema: %w", err)
	}

	// Safe column migrations for existing databases
	for _, alter := range []string{
		"ALTER TABLE sessions ADD COLUMN last_model TEXT NOT NULL DEFAULT '';",
		"ALTER TABLE sessions ADD COLUMN model_stats TEXT NOT NULL DEFAULT '{}';",
	} {
		_, _ = db.Exec(alter)
	}

	s := &Store{
		db:            db,
		retentionDays: retentionDays,
	}

	if retentionDays > 0 {
		_, _ = s.PurgeOldMetrics(retentionDays)
	}

	return s, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// Record increments the hourly counters for the given provider and model.
func (s *Store) Record(t time.Time, provider, model string, inTokens, outTokens, totalTokens int, isError bool) error {
	if s == nil || s.db == nil {
		return nil
	}

	if t.IsZero() {
		t = time.Now()
	}
	// Truncate to the start of the full hour in UTC
	hourTimestamp := t.UTC().Truncate(time.Hour).Format(time.RFC3339)

	if provider == "" {
		provider = "unknown"
	}
	if model == "" {
		model = "unknown"
	}
	if totalTokens == 0 {
		totalTokens = inTokens + outTokens
	}

	errCount := 0
	if isError {
		errCount = 1
	}

	query := `
	INSERT INTO hourly_metrics (hour_timestamp, provider, model, requests, input_tokens, output_tokens, total_tokens, errors)
	VALUES (?, ?, ?, 1, ?, ?, ?, ?)
	ON CONFLICT(hour_timestamp, provider, model) DO UPDATE SET
		requests = requests + 1,
		input_tokens = input_tokens + excluded.input_tokens,
		output_tokens = output_tokens + excluded.output_tokens,
		total_tokens = total_tokens + excluded.total_tokens,
		errors = errors + excluded.errors;
	`

	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(query, hourTimestamp, provider, model, inTokens, outTokens, totalTokens, errCount)
	return err
}

// PurgeOldMetrics deletes hourly records older than retentionDays.
func (s *Store) PurgeOldMetrics(retentionDays int) (int64, error) {
	if s == nil || s.db == nil || retentionDays <= 0 {
		return 0, nil
	}

	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays).Truncate(time.Hour).Format(time.RFC3339)

	s.mu.Lock()
	defer s.mu.Unlock()

	_, _ = s.db.Exec("DELETE FROM session_requests WHERE session_id IN (SELECT id FROM sessions WHERE last_active < ?)", cutoff)
	_, _ = s.db.Exec("DELETE FROM sessions WHERE last_active < ?", cutoff)

	res, err := s.db.Exec("DELETE FROM hourly_metrics WHERE hour_timestamp < ?", cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetDailyMetrics returns daily buckets within [start, end], optionally filtered by provider.
func (s *Store) GetDailyMetrics(start, end time.Time, providerFilter string) ([]DailyMetricBucket, error) {
	if s == nil || s.db == nil {
		return []DailyMetricBucket{}, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	startStr := start.UTC().Truncate(24 * time.Hour).Format(time.RFC3339)
	endStr := end.UTC().Truncate(24*time.Hour).Add(24*time.Hour - time.Nanosecond).Format(time.RFC3339)

	query := `
	SELECT substr(hour_timestamp, 1, 10) AS day_str, provider, model,
	       SUM(requests), SUM(input_tokens), SUM(output_tokens), SUM(total_tokens), SUM(errors)
	FROM hourly_metrics
	WHERE hour_timestamp >= ? AND hour_timestamp <= ?
	`
	args := []any{startStr, endStr}

	if providerFilter != "" && providerFilter != "all" {
		query += " AND provider = ?"
		args = append(args, providerFilter)
	}

	query += " GROUP BY day_str, provider, model ORDER BY day_str ASC"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying daily metrics: %w", err)
	}
	defer rows.Close()

	dayMap := make(map[string]*DailyMetricBucket)
	dayOrder := make([]string, 0)

	// Ensure all days in the range have a bucket, even if zero usage
	curr := start.UTC().Truncate(24 * time.Hour)
	endDay := end.UTC().Truncate(24 * time.Hour)
	for !curr.After(endDay) {
		dStr := curr.Format("2006-01-02")
		bucket := &DailyMetricBucket{
			Date:   dStr,
			Models: make([]ModelMetric, 0),
		}
		dayMap[dStr] = bucket
		dayOrder = append(dayOrder, dStr)
		curr = curr.AddDate(0, 0, 1)
	}

	for rows.Next() {
		var (
			dayStr, prov, mod string
			reqs, inTok, outTok, totTok, errs int64
		)
		if err := rows.Scan(&dayStr, &prov, &mod, &reqs, &inTok, &outTok, &totTok, &errs); err != nil {
			return nil, fmt.Errorf("scanning daily row: %w", err)
		}

		bucket, exists := dayMap[dayStr]
		if !exists {
			bucket = &DailyMetricBucket{
				Date:   dayStr,
				Models: make([]ModelMetric, 0),
			}
			dayMap[dayStr] = bucket
			dayOrder = append(dayOrder, dayStr)
		}

		bucket.Requests += reqs
		bucket.InputTokens += inTok
		bucket.OutputTokens += outTok
		bucket.TotalTokens += totTok
		bucket.Errors += errs

		bucket.Models = append(bucket.Models, ModelMetric{
			Model:        mod,
			Provider:     prov,
			InputTokens:  inTok,
			OutputTokens: outTok,
			TotalTokens:  totTok,
			Requests:     reqs,
			Errors:       errs,
		})
	}

	result := make([]DailyMetricBucket, 0, len(dayOrder))
	for _, dStr := range dayOrder {
		bucket := dayMap[dStr]
		// Sort models descending by TotalTokens
		sort.Slice(bucket.Models, func(i, j int) bool {
			return bucket.Models[i].TotalTokens > bucket.Models[j].TotalTokens
		})
		result = append(result, *bucket)
	}

	return result, nil
}

// GetHourlyMetrics returns all 24 hourly buckets for a given day (YYYY-MM-DD), optionally filtered by provider.
func (s *Store) GetHourlyMetrics(day time.Time, providerFilter string) ([]HourlyMetricBucket, error) {
	if s == nil || s.db == nil {
		return []HourlyMetricBucket{}, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour)

	startStr := dayStart.Format(time.RFC3339)
	endStr := dayEnd.Format(time.RFC3339)

	query := `
	SELECT hour_timestamp, provider, model,
	       requests, input_tokens, output_tokens, total_tokens, errors
	FROM hourly_metrics
	WHERE hour_timestamp >= ? AND hour_timestamp < ?
	`
	args := []any{startStr, endStr}

	if providerFilter != "" && providerFilter != "all" {
		query += " AND provider = ?"
		args = append(args, providerFilter)
	}

	query += " ORDER BY hour_timestamp ASC"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying hourly metrics: %w", err)
	}
	defer rows.Close()

	// Initialize 24 slots (00 to 23)
	slots := make([]HourlyMetricBucket, 24)
	slotMap := make(map[string]*HourlyMetricBucket)
	for h := 0; h < 24; h++ {
		hTime := dayStart.Add(time.Duration(h) * time.Hour)
		hStr := fmt.Sprintf("%02d", h)
		tsStr := hTime.Format(time.RFC3339)
		slots[h] = HourlyMetricBucket{
			Hour:      hStr,
			Timestamp: tsStr,
			Models:    make([]ModelMetric, 0),
		}
		slotMap[tsStr] = &slots[h]
	}

	for rows.Next() {
		var (
			hourTs, prov, mod string
			reqs, inTok, outTok, totTok, errs int64
		)
		if err := rows.Scan(&hourTs, &prov, &mod, &reqs, &inTok, &outTok, &totTok, &errs); err != nil {
			return nil, fmt.Errorf("scanning hourly row: %w", err)
		}

		bucket, exists := slotMap[hourTs]
		if !exists {
			// Find by matching hour prefix if slight formatting difference
			for ts, b := range slotMap {
				if len(ts) >= 13 && len(hourTs) >= 13 && ts[:13] == hourTs[:13] {
					bucket = b
					break
				}
			}
		}

		if bucket != nil {
			bucket.Requests += reqs
			bucket.InputTokens += inTok
			bucket.OutputTokens += outTok
			bucket.TotalTokens += totTok
			bucket.Errors += errs

			bucket.Models = append(bucket.Models, ModelMetric{
				Model:        mod,
				Provider:     prov,
				InputTokens:  inTok,
				OutputTokens: outTok,
				TotalTokens:  totTok,
				Requests:     reqs,
				Errors:       errs,
			})
		}
	}

	for i := range slots {
		// Sort models descending by TotalTokens
		sort.Slice(slots[i].Models, func(a, b int) bool {
			return slots[i].Models[a].TotalTokens > slots[i].Models[b].TotalTokens
		})
	}

	return slots, nil
}

// GetSummary returns high-level totals, top models, top providers, and distinct filters for a time range.
func (s *Store) GetSummary(start, end time.Time, providerFilter string) (*MetricsSummary, error) {
	if s == nil || s.db == nil {
		return &MetricsSummary{}, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	startStr := start.UTC().Truncate(24 * time.Hour).Format(time.RFC3339)
	endStr := end.UTC().Truncate(24*time.Hour).Add(24*time.Hour - time.Nanosecond).Format(time.RFC3339)

	summary := &MetricsSummary{
		StartDate:          startStr[:10],
		EndDate:            endStr[:10],
		TopModels:          make([]ModelMetric, 0),
		TopProviders:       make([]ProviderMetric, 0),
		AvailableProviders: make([]string, 0),
		AvailableModels:    make([]string, 0),
	}

	// 1. Overall totals
	totQuery := `
	SELECT COALESCE(SUM(requests), 0), COALESCE(SUM(input_tokens), 0),
	       COALESCE(SUM(output_tokens), 0), COALESCE(SUM(total_tokens), 0),
	       COALESCE(SUM(errors), 0)
	FROM hourly_metrics
	WHERE hour_timestamp >= ? AND hour_timestamp <= ?
	`
	totArgs := []any{startStr, endStr}
	if providerFilter != "" && providerFilter != "all" {
		totQuery += " AND provider = ?"
		totArgs = append(totArgs, providerFilter)
	}

	row := s.db.QueryRow(totQuery, totArgs...)
	_ = row.Scan(&summary.Requests, &summary.InputTokens, &summary.OutputTokens, &summary.TotalTokens, &summary.Errors)

	// 2. Models breakdown
	modQuery := `
	SELECT model, provider,
	       SUM(requests), SUM(input_tokens), SUM(output_tokens), SUM(total_tokens), SUM(errors)
	FROM hourly_metrics
	WHERE hour_timestamp >= ? AND hour_timestamp <= ?
	`
	modArgs := []any{startStr, endStr}
	if providerFilter != "" && providerFilter != "all" {
		modQuery += " AND provider = ?"
		modArgs = append(modArgs, providerFilter)
	}
	modQuery += " GROUP BY model, provider ORDER BY SUM(total_tokens) DESC"

	modRows, err := s.db.Query(modQuery, modArgs...)
	if err == nil {
		defer modRows.Close()
		for modRows.Next() {
			var mm ModelMetric
			if err := modRows.Scan(&mm.Model, &mm.Provider, &mm.Requests, &mm.InputTokens, &mm.OutputTokens, &mm.TotalTokens, &mm.Errors); err == nil {
				summary.TopModels = append(summary.TopModels, mm)
			}
		}
	}

	// 3. Providers breakdown
	provQuery := `
	SELECT provider,
	       SUM(requests), SUM(input_tokens), SUM(output_tokens), SUM(total_tokens), SUM(errors)
	FROM hourly_metrics
	WHERE hour_timestamp >= ? AND hour_timestamp <= ?
	GROUP BY provider ORDER BY SUM(total_tokens) DESC
	`
	provRows, err := s.db.Query(provQuery, startStr, endStr)
	if err == nil {
		defer provRows.Close()
		for provRows.Next() {
			var pm ProviderMetric
			if err := provRows.Scan(&pm.Provider, &pm.Requests, &pm.InputTokens, &pm.OutputTokens, &pm.TotalTokens, &pm.Errors); err == nil {
				summary.TopProviders = append(summary.TopProviders, pm)
			}
		}
	}

	// 4. Distinct providers and models across all time
	pListRows, err := s.db.Query("SELECT DISTINCT provider FROM hourly_metrics ORDER BY provider ASC")
	if err == nil {
		defer pListRows.Close()
		for pListRows.Next() {
			var p string
			if err := pListRows.Scan(&p); err == nil && p != "" {
				summary.AvailableProviders = append(summary.AvailableProviders, p)
			}
		}
	}

	mListRows, err := s.db.Query("SELECT DISTINCT model FROM hourly_metrics ORDER BY model ASC")
	if err == nil {
		defer mListRows.Close()
		for mListRows.Next() {
			var m string
			if err := mListRows.Scan(&m); err == nil && m != "" {
				summary.AvailableModels = append(summary.AvailableModels, m)
			}
		}
	}

	return summary, nil
}

// GetModelSpeedMetrics queries session_requests within [start, end] and calculates average token
// output generation throughput (tok/s) grouped by model and time bucket (minute, hour, or day).
func (s *Store) GetModelSpeedMetrics(start, end time.Time, granularity, providerFilter string) (*SpeedMetricsResponse, error) {
	resp := &SpeedMetricsResponse{
		Granularity: granularity,
		Models:      []ModelOverallSpeed{},
		Buckets:     []SpeedBucket{},
		Provider:    providerFilter,
	}

	if s == nil || s.db == nil {
		return resp, nil
	}

	// Normalize granularity
	switch granularity {
	case "minute":
		// Safeguard: minute granularity clamped to at most 24 hours
		if end.Sub(start) > 24*time.Hour {
			start = end.Add(-24 * time.Hour)
		}
	case "day":
		// OK
	default:
		granularity = "hour"
	}
	resp.Granularity = granularity

	s.mu.RLock()
	defer s.mu.RUnlock()

	startStr := start.UTC().Format(time.RFC3339)
	endStr := end.UTC().Format(time.RFC3339)
	resp.StartDate = startStr
	resp.EndDate = endStr

	var strftimeFmt string
	switch granularity {
	case "minute":
		strftimeFmt = "%Y-%m-%d %H:%M"
	case "day":
		strftimeFmt = "%Y-%m-%d"
	default: // hour
		strftimeFmt = "%Y-%m-%d %H:00"
	}

	// 1. Query Overall Model Speeds across the entire time range
	overallQuery := `
	SELECT model, provider,
	       COUNT(*) AS req_count,
	       SUM(output_tokens) AS sum_tokens,
	       SUM(generation_duration_ms) AS sum_dur,
	       MIN(tokens_per_second) AS min_tps,
	       MAX(tokens_per_second) AS max_tps
	FROM session_requests
	WHERE timestamp >= ? AND timestamp <= ?
	  AND status = 'success'
	  AND output_tokens > 0
	  AND generation_duration_ms >= 50
	`
	overallArgs := []any{startStr, endStr}
	if providerFilter != "" && providerFilter != "all" {
		overallQuery += " AND provider = ?"
		overallArgs = append(overallArgs, providerFilter)
	}
	overallQuery += " GROUP BY model, provider"

	rows, err := s.db.Query(overallQuery, overallArgs...)
	if err != nil {
		return nil, fmt.Errorf("querying overall speed metrics: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var o ModelOverallSpeed
		var minTps, maxTps sql.NullFloat64
		if err := rows.Scan(&o.Model, &o.Provider, &o.TotalRequests, &o.TotalOutputTokens, &o.GenerationDurationMs, &minTps, &maxTps); err != nil {
			continue
		}
		if o.GenerationDurationMs > 0 {
			o.AvgTokensPerSecond = math.Round((float64(o.TotalOutputTokens)*1000.0/float64(o.GenerationDurationMs))*10) / 10
		}
		if minTps.Valid {
			o.MinTokensPerSecond = math.Round(minTps.Float64*10) / 10
		}
		if maxTps.Valid {
			o.MaxTokensPerSecond = math.Round(maxTps.Float64*10) / 10
		}
		resp.Models = append(resp.Models, o)
	}

	// Sort models by AvgTokensPerSecond descending
	sort.Slice(resp.Models, func(i, j int) bool {
		return resp.Models[i].AvgTokensPerSecond > resp.Models[j].AvgTokensPerSecond
	})

	// 2. Query Time-Series Buckets
	bucketQuery := `
	SELECT strftime(?, timestamp) AS bucket_key,
	       model, provider,
	       COUNT(*) AS req_count,
	       SUM(output_tokens) AS sum_tokens,
	       SUM(generation_duration_ms) AS sum_dur
	FROM session_requests
	WHERE timestamp >= ? AND timestamp <= ?
	  AND status = 'success'
	  AND output_tokens > 0
	  AND generation_duration_ms >= 50
	`
	bucketArgs := []any{strftimeFmt, startStr, endStr}
	if providerFilter != "" && providerFilter != "all" {
		bucketQuery += " AND provider = ?"
		bucketArgs = append(bucketArgs, providerFilter)
	}
	bucketQuery += " GROUP BY bucket_key, model, provider ORDER BY bucket_key ASC, model ASC"

	bRows, err := s.db.Query(bucketQuery, bucketArgs...)
	if err != nil {
		return nil, fmt.Errorf("querying bucket speed metrics: %w", err)
	}
	defer bRows.Close()

	bucketMap := make(map[string]*SpeedBucket)
	bucketOrder := make([]string, 0)

	for bRows.Next() {
		var bucketKey, model, provider string
		var reqCount, sumTokens, sumDur int64
		if err := bRows.Scan(&bucketKey, &model, &provider, &reqCount, &sumTokens, &sumDur); err != nil {
			continue
		}

		b, exists := bucketMap[bucketKey]
		if !exists {
			b = &SpeedBucket{
				Bucket:    bucketKey,
				Timestamp: bucketKey,
				Models:    []ModelSpeedPoint{},
			}
			bucketMap[bucketKey] = b
			bucketOrder = append(bucketOrder, bucketKey)
		}

		var tps float64
		if sumDur > 0 {
			tps = math.Round((float64(sumTokens)*1000.0/float64(sumDur))*10) / 10
		}

		b.Models = append(b.Models, ModelSpeedPoint{
			Model:                model,
			Provider:             provider,
			Requests:             reqCount,
			OutputTokens:         sumTokens,
			GenerationDurationMs: sumDur,
			TokensPerSecond:      tps,
		})
	}

	for _, k := range bucketOrder {
		b := bucketMap[k]
		// Sort models in bucket by speed desc
		sort.Slice(b.Models, func(i, j int) bool {
			return b.Models[i].TokensPerSecond > b.Models[j].TokensPerSecond
		})
		resp.Buckets = append(resp.Buckets, *b)
	}

	return resp, nil
}

