package store_test

import (
	"path/filepath"
	"testing"

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
