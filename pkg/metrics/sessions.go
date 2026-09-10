package metrics

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/vogler75/babel-gate/pkg/session"
)

// SaveSession persists or updates session metadata and cumulative usage counters in SQLite.
func (s *Store) SaveSession(sess *session.Session) error {
	if s == nil || s.db == nil || sess == nil {
		return nil
	}

	modelsJSON, err := json.Marshal(sess.Models)
	if err != nil || len(sess.Models) == 0 {
		modelsJSON = []byte("[]")
	}

	modelStatsJSON, err := json.Marshal(sess.ModelStats)
	if err != nil || len(sess.ModelStats) == 0 {
		modelStatsJSON = []byte("{}")
	}

	createdAtStr := sess.CreatedAt.UTC().Format(time.RFC3339)
	lastActiveStr := sess.LastActive.UTC().Format(time.RFC3339)

	ctxEst := 0
	if sess.ContextTokensEstimated {
		ctxEst = 1
	}

	query := `
	INSERT INTO sessions (
		id, client, client_ip, user_agent, created_at, last_active,
		last_model, request_count, context_tokens, context_tokens_estimated,
		input_tokens, output_tokens, total_tokens, tokens_per_second,
		generation_duration_ms, measured_output_tokens, models, model_stats
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		client = excluded.client,
		client_ip = excluded.client_ip,
		user_agent = excluded.user_agent,
		created_at = excluded.created_at,
		last_active = excluded.last_active,
		last_model = excluded.last_model,
		request_count = excluded.request_count,
		context_tokens = excluded.context_tokens,
		context_tokens_estimated = excluded.context_tokens_estimated,
		input_tokens = excluded.input_tokens,
		output_tokens = excluded.output_tokens,
		total_tokens = excluded.total_tokens,
		tokens_per_second = excluded.tokens_per_second,
		generation_duration_ms = excluded.generation_duration_ms,
		measured_output_tokens = excluded.measured_output_tokens,
		models = excluded.models,
		model_stats = excluded.model_stats;
	`

	s.mu.Lock()
	defer s.mu.Unlock()

	_, err = s.db.Exec(query,
		sess.ID, sess.Client, sess.ClientIP, sess.UserAgent,
		createdAtStr, lastActiveStr, sess.LastModel,
		sess.RequestCount, sess.ContextTokens, ctxEst,
		sess.InputTokens, sess.OutputTokens, sess.TotalTokens,
		sess.TokensPerSecond, sess.GenerationDurationMs, sess.MeasuredOutputTokens,
		string(modelsJSON), string(modelStatsJSON),
	)
	return err
}

// SaveRequest records an individual request entry for a session.
func (s *Store) SaveRequest(sessionID string, rec session.RequestRecord) error {
	if s == nil || s.db == nil || sessionID == "" {
		return nil
	}

	if rec.ID == "" {
		rec.ID = session.GenerateID("req")
	}
	if rec.Timestamp.IsZero() {
		rec.Timestamp = time.Now()
	}

	tsStr := rec.Timestamp.UTC().Format(time.RFC3339)
	streamInt := 0
	if rec.Stream {
		streamInt = 1
	}
	inEstInt := 0
	if rec.InputTokensEstimated {
		inEstInt = 1
	}

	query := `
	INSERT INTO session_requests (
		id, session_id, timestamp, provider, model, stream,
		duration_ms, generation_duration_ms, input_tokens, input_tokens_estimated,
		output_tokens, total_tokens, tokens_per_second, status, error_message
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		session_id = excluded.session_id,
		timestamp = excluded.timestamp,
		provider = excluded.provider,
		model = excluded.model,
		stream = excluded.stream,
		duration_ms = excluded.duration_ms,
		generation_duration_ms = excluded.generation_duration_ms,
		input_tokens = excluded.input_tokens,
		input_tokens_estimated = excluded.input_tokens_estimated,
		output_tokens = excluded.output_tokens,
		total_tokens = excluded.total_tokens,
		tokens_per_second = excluded.tokens_per_second,
		status = excluded.status,
		error_message = excluded.error_message;
	`

	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(query,
		rec.ID, sessionID, tsStr, rec.Provider, rec.Model, streamInt,
		rec.DurationMs, rec.GenerationDurationMs, rec.InputTokens, inEstInt,
		rec.OutputTokens, rec.TotalTokens, rec.TokensPerSecond,
		rec.Status, rec.ErrorMessage,
	)
	return err
}

// LoadActiveSessions loads unexpired sessions along with their recent requests from SQLite.
func (s *Store) LoadActiveSessions(since time.Time, maxSessions int, maxRequestsPerSession int) ([]*session.Session, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}

	if maxSessions <= 0 {
		maxSessions = 200
	}
	if maxRequestsPerSession <= 0 {
		maxRequestsPerSession = 50
	}

	cutoffStr := since.UTC().Format(time.RFC3339)

	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
		SELECT id, client, client_ip, user_agent, created_at, last_active,
		       last_model, request_count, context_tokens, context_tokens_estimated,
		       input_tokens, output_tokens, total_tokens, tokens_per_second,
		       generation_duration_ms, measured_output_tokens, models, model_stats
		FROM sessions
		WHERE last_active >= ?
		ORDER BY last_active ASC
		LIMIT ?;
	`, cutoffStr, maxSessions)
	if err != nil {
		return nil, fmt.Errorf("loading sessions: %w", err)
	}
	defer rows.Close()

	var sessions []*session.Session
	for rows.Next() {
		var (
			id, client, clientIP, userAgent string
			createdAtStr, lastActiveStr      string
			lastModel                        string
			reqCount, ctxTok, ctxTokEst      int
			inTok, outTok, totTok            int
			tps                              float64
			genDurMs                         int64
			measOutTok                       int
			modelsJSON, modelStatsJSON       string
		)
		if err := rows.Scan(
			&id, &client, &clientIP, &userAgent,
			&createdAtStr, &lastActiveStr,
			&lastModel,
			&reqCount, &ctxTok, &ctxTokEst,
			&inTok, &outTok, &totTok,
			&tps, &genDurMs, &measOutTok, &modelsJSON, &modelStatsJSON,
		); err != nil {
			return nil, fmt.Errorf("scanning session row: %w", err)
		}

		createdAt, err := time.Parse(time.RFC3339, createdAtStr)
		if err != nil {
			createdAt = time.Now()
		}
		lastActive, err := time.Parse(time.RFC3339, lastActiveStr)
		if err != nil {
			lastActive = time.Now()
		}

		var models []string
		if modelsJSON != "" {
			_ = json.Unmarshal([]byte(modelsJSON), &models)
		}
		if models == nil {
			models = make([]string, 0)
		}

		var modelStats map[string]*session.ModelUsage
		if modelStatsJSON != "" && modelStatsJSON != "{}" {
			_ = json.Unmarshal([]byte(modelStatsJSON), &modelStats)
		}
		if modelStats == nil {
			modelStats = make(map[string]*session.ModelUsage)
		}

		sess := &session.Session{
			ID:                     id,
			Client:                 client,
			ClientIP:               clientIP,
			UserAgent:              userAgent,
			CreatedAt:              createdAt,
			LastActive:             lastActive,
			LastModel:              lastModel,
			RequestCount:           reqCount,
			ContextTokens:          ctxTok,
			ContextTokensEstimated: ctxTokEst != 0,
			InputTokens:            inTok,
			OutputTokens:           outTok,
			TotalTokens:            totTok,
			TokensPerSecond:        tps,
			GenerationDurationMs:   genDurMs,
			MeasuredOutputTokens:   measOutTok,
			Models:                 models,
			ModelStats:             modelStats,
			RecentRequests:         make([]session.RequestRecord, 0),
		}
		sessions = append(sessions, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating sessions: %w", err)
	}

	// For each loaded session, query its recent requests
	reqStmt, err := s.db.Prepare(`
		SELECT id, timestamp, provider, model, stream,
		       duration_ms, generation_duration_ms, input_tokens, input_tokens_estimated,
		       output_tokens, total_tokens, tokens_per_second, status, error_message
		FROM session_requests
		WHERE session_id = ?
		ORDER BY timestamp DESC
		LIMIT ?;
	`)
	if err != nil {
		return nil, fmt.Errorf("preparing request query: %w", err)
	}
	defer reqStmt.Close()

	for _, sess := range sessions {
		reqRows, err := reqStmt.Query(sess.ID, maxRequestsPerSession)
		if err != nil {
			continue
		}

		for reqRows.Next() {
			var (
				reqID, tsStr, provider, model   string
				streamInt                       int
				durMs, genDurMs                 int64
				inTok, inTokEst, outTok, totTok int
				tps                             float64
				status, errMsg                  string
			)
			if err := reqRows.Scan(
				&reqID, &tsStr, &provider, &model, &streamInt,
				&durMs, &genDurMs, &inTok, &inTokEst,
				&outTok, &totTok, &tps, &status, &errMsg,
			); err != nil {
				continue
			}

			ts, err := time.Parse(time.RFC3339, tsStr)
			if err != nil {
				ts = time.Now()
			}

			sess.RecentRequests = append(sess.RecentRequests, session.RequestRecord{
				ID:                   reqID,
				Timestamp:            ts,
				Provider:             provider,
				Model:                model,
				Stream:               streamInt != 0,
				DurationMs:           durMs,
				GenerationDurationMs: genDurMs,
				InputTokens:          inTok,
				InputTokensEstimated: inTokEst != 0,
				OutputTokens:         outTok,
				TotalTokens:          totTok,
				TokensPerSecond:      tps,
				Status:               status,
				ErrorMessage:         errMsg,
			})
		}
		reqRows.Close()

		if sess.LastModel == "" && len(sess.RecentRequests) > 0 {
			sess.LastModel = sess.RecentRequests[0].Model
		}
		if len(sess.ModelStats) == 0 && len(sess.RecentRequests) > 0 {
			sess.ModelStats = make(map[string]*session.ModelUsage)
			for _, r := range sess.RecentRequests {
				if r.Model == "" {
					continue
				}
				st, exists := sess.ModelStats[r.Model]
				if !exists {
					st = &session.ModelUsage{Model: r.Model}
					sess.ModelStats[r.Model] = st
				}
				st.RequestCount++
				st.InputTokens += r.InputTokens
				st.OutputTokens += r.OutputTokens
				st.TotalTokens += r.TotalTokens
				if st.LastUsed.Before(r.Timestamp) {
					st.LastUsed = r.Timestamp
				}
			}
			if sess.RequestCount > 0 {
				for _, st := range sess.ModelStats {
					st.PercentReq = (float64(st.RequestCount) / float64(sess.RequestCount)) * 100.0
					if sess.TotalTokens > 0 {
						st.PercentTok = (float64(st.TotalTokens) / float64(sess.TotalTokens)) * 100.0
					}
				}
			}
		}
	}

	return sessions, nil
}

// DeleteSession removes a single session and its requests from SQLite.
func (s *Store) DeleteSession(sessionID string) error {
	if s == nil || s.db == nil || sessionID == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	_, _ = s.db.Exec("DELETE FROM session_requests WHERE session_id = ?", sessionID)
	_, err := s.db.Exec("DELETE FROM sessions WHERE id = ?", sessionID)
	return err
}

// ClearSessions deletes all sessions and session requests from SQLite.
func (s *Store) ClearSessions() error {
	if s == nil || s.db == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	_, _ = s.db.Exec("DELETE FROM session_requests")
	_, err := s.db.Exec("DELETE FROM sessions")
	return err
}

// PurgeOldSessions removes sessions whose last activity is before cutoff.
func (s *Store) PurgeOldSessions(cutoff time.Time) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}

	cutoffStr := cutoff.UTC().Format(time.RFC3339)

	s.mu.Lock()
	defer s.mu.Unlock()

	_, _ = s.db.Exec("DELETE FROM session_requests WHERE session_id IN (SELECT id FROM sessions WHERE last_active < ?)", cutoffStr)
	res, err := s.db.Exec("DELETE FROM sessions WHERE last_active < ?", cutoffStr)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
