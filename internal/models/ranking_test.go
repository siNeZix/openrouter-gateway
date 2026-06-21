package models

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"openrouter-gateway/internal/store"
)

func TestRankingManager_FetchFree(t *testing.T) {
	// 1. Create a mock HTTP server to simulate OpenRouter models API
	mockORResponse := `{
		"data": [
			{"id": "google/gemini-2.5-flash:free", "name": "Gemini 2.5 Flash Free", "context_length": 1048576},
			{"id": "meta-llama/llama-3-8b-instruct:free", "name": "Llama 3 8B Free", "context_length": 8192},
			{"id": "openai/gpt-4o", "name": "GPT-4o Paid", "context_length": 128000}
		]
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, mockORResponse)
	}))
	defer server.Close()

	// 2. Initialize a temporary sqlite store
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_models.db")
	s, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer s.Close()

	// 3. Create RankingManager, override URLs to avoid network requests
	rm := NewRankingManager(s, 10*time.Minute)
	rm.openRouterURL = server.URL
	rm.shirManURL = "http://localhost:12345/nonexistent" // will fail, that's fine

	// 4. Run fetchFree
	err = rm.fetchFree()
	if err != nil {
		t.Fatalf("fetchFree failed: %v", err)
	}

	// 5. Verify cached free models in memory
	freeModels := rm.GetFreeModels()
	if len(freeModels) != 2 {
		t.Fatalf("expected exactly 2 free models, got %d", len(freeModels))
	}

	// Check if only ":free" suffix models are cached
	for _, m := range freeModels {
		if m.ID == "openai/gpt-4o" {
			t.Errorf("paid model openai/gpt-4o should not be present in free models list")
		}
	}

	// Verify specific model data
	m1 := freeModels[0]
	if m1.ID != "google/gemini-2.5-flash:free" || m1.Name != "Gemini 2.5 Flash Free" || m1.ContextLength != 1048576 {
		t.Errorf("unexpected free model data: %+v", m1)
	}

	// 6. Test IsFreeModel
	if !rm.IsFreeModel("google/gemini-2.5-flash:free") {
		t.Errorf("IsFreeModel should return true for google/gemini-2.5-flash:free")
	}
	if rm.IsFreeModel("openai/gpt-4o") {
		t.Errorf("IsFreeModel should return false for openai/gpt-4o")
	}

	// 7. Verify SQLite cache persistence
	cachedFromDB, err := s.GetCachedFreeModels()
	if err != nil {
		t.Fatalf("GetCachedFreeModels failed: %v", err)
	}
	if len(cachedFromDB) != 2 {
		t.Fatalf("expected 2 persisted free models in sqlite, got %d", len(cachedFromDB))
	}
}
