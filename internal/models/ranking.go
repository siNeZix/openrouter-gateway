package models

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"openrouter-gateway/internal/store"
)

type ShirManResponse struct {
	Models []struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		Rank          int    `json:"rank"`
		ContextLength int64  `json:"contextLength"`
	} `json:"models"`
}

type OpenRouterModelsResponse struct {
	Data []struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		ContextLength int64  `json:"context_length"`
	} `json:"data"`
}

type RankingManager struct {
	store      *store.Store
	refreshInt time.Duration

	shirManURL    string
	openRouterURL string

	mu         sync.RWMutex
	models     []store.DBModel
	freeModels []store.DBModel
	fallbackID string
}

func NewRankingManager(s *store.Store, refreshInterval time.Duration) *RankingManager {
	return &RankingManager{
		store:         s,
		refreshInt:    refreshInterval,
		shirManURL:    "https://shir-man.com/api/free-llm/top-models",
		openRouterURL: "https://openrouter.ai/api/v1/models",
		fallbackID:    "openrouter/free",
	}
}

func (rm *RankingManager) Start() {
	// Try loading from SQLite cache first
	if cached, err := rm.store.GetCachedModels(); err == nil && len(cached) > 0 {
		rm.mu.Lock()
		rm.models = cached
		rm.mu.Unlock()
		log.Printf("Loaded %d models from database cache", len(cached))
	}
	if cachedFree, err := rm.store.GetCachedFreeModels(); err == nil && len(cachedFree) > 0 {
		rm.mu.Lock()
		rm.freeModels = cachedFree
		rm.mu.Unlock()
		log.Printf("Loaded %d free models from database cache", len(cachedFree))
	}

	// Initial fetch
	if err := rm.fetch(); err != nil {
		log.Printf("Initial Shir-Man ranking fetch failed: %v", err)
	}
	if err := rm.fetchFree(); err != nil {
		log.Printf("Initial OpenRouter free models fetch failed: %v", err)
	}

	// Periodical background fetch
	go func() {
		ticker := time.NewTicker(rm.refreshInt)
		defer ticker.Stop()
		for range ticker.C {
			if err := rm.fetch(); err != nil {
				log.Printf("Shir-Man ranking fetch failed: %v", err)
			}
			if err := rm.fetchFree(); err != nil {
				log.Printf("OpenRouter free models fetch failed: %v", err)
			}
		}
	}()
}

func (rm *RankingManager) fetch() error {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(rm.shirManURL)
	if err != nil {
		return fmt.Errorf("failed to make HTTP request to Shir-Man API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad HTTP status from Shir-Man API: %s", resp.Status)
	}

	var data ShirManResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return fmt.Errorf("failed to decode Shir-Man response: %w", err)
	}

	if len(data.Models) == 0 {
		return fmt.Errorf("Shir-Man API returned 0 models")
	}

	var dbModels []store.DBModel
	now := time.Now()
	for _, m := range data.Models {
		dbModels = append(dbModels, store.DBModel{
			ID:            m.ID,
			Name:          m.Name,
			Rank:          m.Rank,
			ContextLength: m.ContextLength,
			UpdatedAt:     now,
		})
	}

	// Update memory cache
	rm.mu.Lock()
	rm.models = dbModels
	rm.mu.Unlock()

	// Cache in SQLite
	if err := rm.store.CacheModels(dbModels); err != nil {
		log.Printf("Failed to cache models in DB: %v", err)
	}

	log.Printf("Updated model rankings. Total free models: %d. Top-1: %s", len(dbModels), dbModels[0].ID)
	return nil
}

func (rm *RankingManager) fetchFree() error {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(rm.openRouterURL)
	if err != nil {
		return fmt.Errorf("failed to make HTTP request to OpenRouter API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad HTTP status from OpenRouter API: %s", resp.Status)
	}

	var data OpenRouterModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return fmt.Errorf("failed to decode OpenRouter models: %w", err)
	}

	var dbModels []store.DBModel
	now := time.Now()
	for _, m := range data.Data {
		// ponytail: we only need models with :free tag
		if len(m.ID) > 5 && m.ID[len(m.ID)-5:] == ":free" {
			dbModels = append(dbModels, store.DBModel{
				ID:            m.ID,
				Name:          m.Name,
				ContextLength: m.ContextLength,
				UpdatedAt:     now,
			})
		}
	}

	if len(dbModels) == 0 {
		return fmt.Errorf("OpenRouter API returned 0 free models")
	}

	// Update memory cache
	rm.mu.Lock()
	rm.freeModels = dbModels
	rm.mu.Unlock()

	// Cache in SQLite
	if err := rm.store.CacheFreeModels(dbModels); err != nil {
		log.Printf("Failed to cache free models in DB: %v", err)
	}

	log.Printf("Updated OpenRouter free models cache. Total free models: %d", len(dbModels))
	return nil
}

func (rm *RankingManager) ResolveAlias(alias string) (string, bool) {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	switch alias {
	case "top1":
		if len(rm.models) > 0 {
			return rm.models[0].ID, true
		}
	case "top2":
		if len(rm.models) > 1 {
			return rm.models[1].ID, true
		}
	case "top3":
		if len(rm.models) > 2 {
			return rm.models[2].ID, true
		}
	}

	return "", false
}

func (rm *RankingManager) IsFreeModel(modelID string) bool {
	if modelID == rm.fallbackID {
		return true
	}

	rm.mu.RLock()
	defer rm.mu.RUnlock()

	for _, m := range rm.models {
		if m.ID == modelID {
			return true
		}
	}
	for _, m := range rm.freeModels {
		if m.ID == modelID {
			return true
		}
	}
	return false
}

func (rm *RankingManager) GetTopModels() []store.DBModel {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	// Return a copy to prevent race conditions or modifications
	res := make([]store.DBModel, len(rm.models))
	copy(res, rm.models)
	return res
}

func (rm *RankingManager) GetFreeModels() []store.DBModel {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	res := make([]store.DBModel, len(rm.freeModels))
	copy(res, rm.freeModels)
	return res
}
