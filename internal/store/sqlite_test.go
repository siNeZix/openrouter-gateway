package store_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"openrouter-gateway/internal/store"
)

func TestStore_GetGeneralStats_Empty(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_stats.db")

	s, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer s.Close()

	stats, err := s.GetGeneralStats()
	if err != nil {
		t.Fatalf("GetGeneralStats failed on empty DB: %v", err)
	}
	if stats.TotalKeys != 0 || stats.ActiveKeys != 0 || stats.BlockedKeys != 0 {
		t.Errorf("expected 0 keys, got %+v", stats)
	}
}

func TestStore_AddAndDeleteKeys(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	s, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer s.Close()

	// 1. Add some keys
	keys := []string{"sk-or-v1-abc123xyz", "sk-or-v1-def456uvw"}
	added, err := s.AddKeys(keys)
	if err != nil {
		t.Fatalf("failed to add keys: %v", err)
	}
	if added != 2 {
		t.Errorf("expected 2 added keys, got %d", added)
	}

	// 2. Fetch keys and verify fields
	dbKeys, err := s.GetKeys()
	if err != nil {
		t.Fatalf("failed to get keys: %v", err)
	}
	if len(dbKeys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(dbKeys))
	}

	var foundFirst, foundSecond bool
	for _, k := range dbKeys {
		if k.RawKey == "sk-or-v1-abc123xyz" {
			foundFirst = true
			if k.MaskedKey != "sk-or-v1-abc...123xyz" { // MaskKey length checks
				t.Errorf("unexpected masked key format: %s", k.MaskedKey)
			}
		}
		if k.RawKey == "sk-or-v1-def456uvw" {
			foundSecond = true
		}
	}

	if !foundFirst || !foundSecond {
		t.Errorf("added keys not found in dbKeys list")
	}

	// 3. Test duplicate addition (ON CONFLICT DO UPDATE should run successfully)
	_, err = s.AddKeys([]string{"sk-or-v1-abc123xyz"})
	if err != nil {
		t.Fatalf("failed to add duplicate key: %v", err)
	}

	// 4. Delete a key
	hashToDelete := store.HashKey("sk-or-v1-abc123xyz")
	if err := s.DeleteKey(hashToDelete); err != nil {
		t.Fatalf("failed to delete key: %v", err)
	}

	// 5. Verify deletion
	dbKeysAfter, err := s.GetKeys()
	if err != nil {
		t.Fatalf("failed to get keys after deletion: %v", err)
	}
	if len(dbKeysAfter) != 1 {
		t.Errorf("expected 1 key after deletion, got %d", len(dbKeysAfter))
	}
	if dbKeysAfter[0].RawKey != "sk-or-v1-def456uvw" {
		t.Errorf("expected remaining key to be sk-or-v1-def456uvw, got %s", dbKeysAfter[0].RawKey)
	}
}

func TestStore_BulkOperations(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_bulk.db")

	s, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer s.Close()

	keys := []string{"key1", "key2", "key3"}
	_, err = s.AddKeys(keys)
	if err != nil {
		t.Fatalf("failed to add keys: %v", err)
	}

	h1 := store.HashKey("key1")
	h2 := store.HashKey("key2")
	h3 := store.HashKey("key3")

	// Test bulk status update to disabled
	err = s.UpdateKeysStatus([]string{h1, h2}, "disabled")
	if err != nil {
		t.Fatalf("failed to update keys status: %v", err)
	}

	dbKeys, err := s.GetKeys()
	if err != nil {
		t.Fatalf("failed to get keys: %v", err)
	}

	disabledCount := 0
	for _, k := range dbKeys {
		if k.Status == "disabled" {
			disabledCount++
		}
	}
	if disabledCount != 2 {
		t.Errorf("expected 2 disabled keys, got %d", disabledCount)
	}

	// Test bulk delete
	err = s.DeleteKeys([]string{h1, h2, h3})
	if err != nil {
		t.Fatalf("failed to delete keys: %v", err)
	}

	dbKeysAfter, err := s.GetKeys()
	if err != nil {
		t.Fatalf("failed to get keys after deletion: %v", err)
	}
	if len(dbKeysAfter) != 0 {
		t.Errorf("expected 0 keys after bulk deletion, got %d", len(dbKeysAfter))
	}
}

func TestStore_RateLimitsAndRequestsLog(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_logging.db")

	s, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer s.Close()

	// 1. Test LogRequest with new fields
	reqTime := time.Now()
	req := &store.DBRequest{
		Timestamp:        reqTime,
		KeyHash:          "test_hash",
		Model:            "test_model",
		StatusCode:       200,
		PromptTokens:     10,
		CompletionTokens: 20,
		LatencyMs:        150,
		TTFTMs:           80,
		IsStream:         true,
	}

	if err := s.LogRequest(req); err != nil {
		t.Fatalf("failed to LogRequest: %v", err)
	}

	// 2. Test LogRateLimit
	limitTime := time.Now()
	rl := &store.DBRateLimit{
		Timestamp:         limitTime,
		KeyHash:           "test_key_hash",
		Source:            "proxy",
		LimitTotal:        sql.NullInt64{Int64: 1000, Valid: true},
		LimitRemaining:    sql.NullInt64{Int64: 950, Valid: true},
		Usage:             sql.NullInt64{Int64: 50, Valid: true},
		RateLimitReq:      sql.NullInt64{Int64: 20, Valid: true},
		RateLimitInterval: sql.NullString{String: "1m", Valid: true},
		ResetRaw:          sql.NullString{String: "60", Valid: true},
	}

	if err := s.LogRateLimit(rl); err != nil {
		t.Fatalf("failed to LogRateLimit: %v", err)
	}

	logs, err := s.GetRateLimitsLog()
	if err != nil {
		t.Fatalf("failed to GetRateLimitsLog: %v", err)
	}

	if len(logs) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(logs))
	}

	entry := logs[0]
	if entry.KeyHash != "test_key_hash" || entry.Source != "proxy" {
		t.Errorf("unexpected values: %+v", entry)
	}
	if !entry.LimitTotal.Valid || entry.LimitTotal.Int64 != 1000 {
		t.Errorf("expected LimitTotal to be 1000, got %+v", entry.LimitTotal)
	}
	if !entry.RateLimitInterval.Valid || entry.RateLimitInterval.String != "1m" {
		t.Errorf("expected RateLimitInterval to be '1m', got %+v", entry.RateLimitInterval)
	}
}
