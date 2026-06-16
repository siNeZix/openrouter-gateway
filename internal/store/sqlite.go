package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type DBKey struct {
	KeyHash           string
	MaskedKey         string
	Status            string
	LimitRemaining    int64
	UsageToday        int64
	MaxLimit          int64
	IsFreeTier        bool
	RateLimitReq      int
	RateLimitInterval string
	CooldownUntil     time.Time
	LastCheckedAt     time.Time
	LastUsedAt        time.Time
	RawKey            string
}

type DBRequest struct {
	ID               int64
	Timestamp        time.Time
	KeyHash          string
	Model            string
	StatusCode       int
	PromptTokens     int
	CompletionTokens int
	LatencyMs        int64
	ErrorMsg         string
}

type DBModel struct {
	ID            string
	Name          string
	Rank          int
	ContextLength int64
	UpdatedAt     time.Time
}

func HashKey(key string) string {
	h := sha256.New()
	h.Write([]byte(key))
	return hex.EncodeToString(h.Sum(nil))
}

func MaskKey(key string) string {
	if len(key) <= 15 {
		return "sk-or-v1-***"
	}
	return fmt.Sprintf("%s...%s", key[:12], key[len(key)-6:])
}

func New(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite database: %w", err)
	}

	// Optimize SQLite performance for concurrent usage
	db.SetMaxOpenConns(1) // SQLite is single-writer anyway, modernc does best with 1 open conn or WAL mode

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to run database migrations: %w", err)
	}

	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	queries := []string{
		`PRAGMA journal_mode=WAL;`,
		`PRAGMA synchronous=NORMAL;`,
		`CREATE TABLE IF NOT EXISTS keys (
			key_hash TEXT PRIMARY KEY,
			masked_key TEXT NOT NULL,
			status TEXT NOT NULL,
			limit_remaining INTEGER NOT NULL DEFAULT 0,
			usage_today INTEGER NOT NULL DEFAULT 0,
			max_limit INTEGER NOT NULL DEFAULT 0,
			is_free_tier INTEGER NOT NULL DEFAULT 1,
			rate_limit_req INTEGER NOT NULL DEFAULT 20,
			rate_limit_interval TEXT NOT NULL DEFAULT '1m',
			cooldown_until DATETIME NOT NULL,
			last_checked_at DATETIME NOT NULL,
			last_used_at DATETIME NOT NULL,
			raw_key TEXT NOT NULL DEFAULT ''
		);`,
		`CREATE TABLE IF NOT EXISTS requests (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME NOT NULL,
			key_hash TEXT NOT NULL,
			model TEXT NOT NULL,
			status_code INTEGER NOT NULL,
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			latency_ms INTEGER NOT NULL DEFAULT 0,
			error_msg TEXT
		);`,
		`CREATE INDEX IF NOT EXISTS idx_requests_timestamp ON requests(timestamp);`,
		`CREATE INDEX IF NOT EXISTS idx_requests_key_hash ON requests(key_hash);`,
		`CREATE TABLE IF NOT EXISTS models_cache (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			rank INTEGER NOT NULL,
			context_length INTEGER NOT NULL,
			updated_at DATETIME NOT NULL
		);`,
	}

	for _, q := range queries {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migration query failed (%s): %w", q, err)
		}
	}

	// Migrations: Add raw_key to existing tables if it does not exist
	_, _ = s.db.Exec(`ALTER TABLE keys ADD COLUMN raw_key TEXT NOT NULL DEFAULT '';`)

	return nil
}

func (s *Store) AddKeys(keys []string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO keys (key_hash, masked_key, status, cooldown_until, last_checked_at, last_used_at, raw_key)
		VALUES (?, ?, 'unchecked', ?, ?, ?, ?)
		ON CONFLICT(key_hash) DO UPDATE SET masked_key=excluded.masked_key, raw_key=excluded.raw_key;
	`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	zeroTime := time.Unix(0, 0)
	added := 0

	for _, k := range keys {
		h := HashKey(k)
		masked := MaskKey(k)
		res, err := stmt.Exec(h, masked, zeroTime, zeroTime, zeroTime, k)
		if err != nil {
			return 0, err
		}
		rows, err := res.RowsAffected()
		if err == nil && rows > 0 {
			added++
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	return added, nil
}

func (s *Store) DeleteKey(hash string) error {
	_, err := s.db.Exec("DELETE FROM keys WHERE key_hash = ?", hash)
	return err
}

func (s *Store) UpdateKey(k *DBKey) error {
	_, err := s.db.Exec(`
		UPDATE keys SET
			status = ?,
			limit_remaining = ?,
			usage_today = ?,
			max_limit = ?,
			is_free_tier = ?,
			rate_limit_req = ?,
			rate_limit_interval = ?,
			cooldown_until = ?,
			last_checked_at = ?,
			last_used_at = ?
		WHERE key_hash = ?
	`, k.Status, k.LimitRemaining, k.UsageToday, k.MaxLimit,
		k.IsFreeTier, k.RateLimitReq, k.RateLimitInterval,
		k.CooldownUntil, k.LastCheckedAt, k.LastUsedAt, k.KeyHash)
	return err
}

func (s *Store) UpdateKeyLastUsed(hash string, lastUsed time.Time) error {
	_, err := s.db.Exec(`UPDATE keys SET last_used_at = ? WHERE key_hash = ?`, lastUsed, hash)
	return err
}

func (s *Store) UpdateKeyCooldown(hash string, cooldownUntil time.Time) error {
	_, err := s.db.Exec(`UPDATE keys SET cooldown_until = ? WHERE key_hash = ?`, cooldownUntil, hash)
	return err
}

func (s *Store) UpdateKeyStatus(hash string, status string) error {
	_, err := s.db.Exec(`UPDATE keys SET status = ? WHERE key_hash = ?`, status, hash)
	return err
}

func (s *Store) GetKeys() ([]*DBKey, error) {
	rows, err := s.db.Query(`
		SELECT key_hash, masked_key, status, limit_remaining, usage_today, max_limit, 
		       is_free_tier, rate_limit_req, rate_limit_interval, cooldown_until, 
		       last_checked_at, last_used_at, raw_key
		FROM keys
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var res []*DBKey
	for rows.Next() {
		k := &DBKey{}
		var isFree int
		err := rows.Scan(
			&k.KeyHash, &k.MaskedKey, &k.Status, &k.LimitRemaining, &k.UsageToday, &k.MaxLimit,
			&isFree, &k.RateLimitReq, &k.RateLimitInterval, &k.CooldownUntil,
			&k.LastCheckedAt, &k.LastUsedAt, &k.RawKey,
		)
		if err != nil {
			return nil, err
		}
		k.IsFreeTier = isFree != 0
		res = append(res, k)
	}
	return res, nil
}

func (s *Store) LogRequest(r *DBRequest) error {
	_, err := s.db.Exec(`
		INSERT INTO requests (timestamp, key_hash, model, status_code, prompt_tokens, completion_tokens, latency_ms, error_msg)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, r.Timestamp, r.KeyHash, r.Model, r.StatusCode, r.PromptTokens, r.CompletionTokens, r.LatencyMs, r.ErrorMsg)
	return err
}

func (s *Store) CacheModels(models []DBModel) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec("DELETE FROM models_cache")
	if err != nil {
		return err
	}

	stmt, err := tx.Prepare(`
		INSERT INTO models_cache (id, name, rank, context_length, updated_at)
		VALUES (?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, m := range models {
		_, err := stmt.Exec(m.ID, m.Name, m.Rank, m.ContextLength, m.UpdatedAt)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *Store) GetCachedModels() ([]DBModel, error) {
	rows, err := s.db.Query(`SELECT id, name, rank, context_length, updated_at FROM models_cache ORDER BY rank ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var res []DBModel
	for rows.Next() {
		m := DBModel{}
		err := rows.Scan(&m.ID, &m.Name, &m.Rank, &m.ContextLength, &m.UpdatedAt)
		if err != nil {
			return nil, err
		}
		res = append(res, m)
	}
	return res, nil
}

// Stats helper structures
type GeneralStats struct {
	TotalRequests int64
	TodayRequests int64
	ActiveKeys    int
	BlockedKeys   int
	InvalidKeys   int
	UncheckedKeys int
	TotalKeys     int
}

type ModelStats struct {
	Model         string
	TotalRequests int64
	AvgLatencyMs  int64
	TotalTokens   int64
}

type KeyUsageStats struct {
	MaskedKey     string
	KeyHash       string
	Status        string
	TodayUsage    int64
	Limit         int64
	TotalRequests int64
	ErrorRequests int64
	CooldownUntil time.Time
}

func (s *Store) GetGeneralStats() (*GeneralStats, error) {
	stats := &GeneralStats{}

	// Counts
	err := s.db.QueryRow(`
		SELECT 
			COUNT(*),
			SUM(CASE WHEN status='active' THEN 1 ELSE 0 END),
			SUM(CASE WHEN status='rate_limited' OR status='day_exhausted' THEN 1 ELSE 0 END),
			SUM(CASE WHEN status='invalid' THEN 1 ELSE 0 END),
			SUM(CASE WHEN status='unchecked' THEN 1 ELSE 0 END)
		FROM keys
	`).Scan(&stats.TotalKeys, &stats.ActiveKeys, &stats.BlockedKeys, &stats.InvalidKeys, &stats.UncheckedKeys)
	if err != nil {
		return nil, err
	}

	// Request stats
	err = s.db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&stats.TotalRequests)
	if err != nil {
		return nil, err
	}

	todayStart := time.Now().Truncate(24 * time.Hour)
	err = s.db.QueryRow(`SELECT COUNT(*) FROM requests WHERE timestamp >= ?`, todayStart).Scan(&stats.TodayRequests)
	if err != nil {
		return nil, err
	}

	return stats, nil
}

func (s *Store) GetModelStats() ([]ModelStats, error) {
	rows, err := s.db.Query(`
		SELECT 
			model, 
			COUNT(*), 
			CAST(AVG(latency_ms) AS INTEGER), 
			SUM(prompt_tokens + completion_tokens) 
		FROM requests 
		GROUP BY model 
		ORDER BY COUNT(*) DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var res []ModelStats
	for rows.Next() {
		m := ModelStats{}
		var totalTokens sql.NullInt64
		err := rows.Scan(&m.Model, &m.TotalRequests, &m.AvgLatencyMs, &totalTokens)
		if err != nil {
			return nil, err
		}
		m.TotalTokens = totalTokens.Int64
		res = append(res, m)
	}
	return res, nil
}

func (s *Store) GetKeyUsageStats() ([]KeyUsageStats, error) {
	rows, err := s.db.Query(`
		SELECT 
			k.masked_key, 
			k.key_hash, 
			k.status, 
			k.usage_today, 
			k.max_limit, 
			k.cooldown_until,
			COUNT(r.id) as total_reqs,
			SUM(CASE WHEN r.status_code >= 400 THEN 1 ELSE 0 END) as err_reqs
		FROM keys k
		LEFT JOIN requests r ON k.key_hash = r.key_hash
		GROUP BY k.key_hash
		ORDER BY k.usage_today DESC, total_reqs DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var res []KeyUsageStats
	for rows.Next() {
		k := KeyUsageStats{}
		var totalReqs, errReqs sql.NullInt64
		err := rows.Scan(
			&k.MaskedKey, &k.KeyHash, &k.Status, &k.TodayUsage, &k.Limit, &k.CooldownUntil,
			&totalReqs, &errReqs,
		)
		if err != nil {
			return nil, err
		}
		k.TotalRequests = totalReqs.Int64
		k.ErrorRequests = errReqs.Int64
		res = append(res, k)
	}
	return res, nil
}
