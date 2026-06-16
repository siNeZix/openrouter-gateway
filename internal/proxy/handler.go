package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"openrouter-gateway/internal/config"
	"openrouter-gateway/internal/keys"
	"openrouter-gateway/internal/models"
	"openrouter-gateway/internal/store"
)

type ProxyHandler struct {
	cfg        *config.Config
	store      *store.Store
	pool       *keys.KeyPool
	rankingMgr *models.RankingManager
	client     *http.Client
}

type ChatCompletionsRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream,omitempty"`
	// We preserve other fields as-is using dynamic mapping or raw JSON
}

func NewProxyHandler(cfg *config.Config, s *store.Store, p *keys.KeyPool, rm *models.RankingManager) *ProxyHandler {
	return &ProxyHandler{
		cfg:        cfg,
		store:      s,
		pool:       p,
		rankingMgr: rm,
		client:     &http.Client{Timeout: 10 * time.Minute}, // Large timeout for streaming/reasoning
	}
}

func (ph *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// CORS
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	// 1. Verify Gateway Client Token
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		http.Error(w, `{"error":{"message":"Missing or invalid Authorization header"}}`, http.StatusUnauthorized)
		return
	}
	clientToken := strings.TrimPrefix(authHeader, "Bearer ")
	if clientToken != ph.cfg.GatewayToken {
		http.Error(w, `{"error":{"message":"Unauthorized: invalid gateway token"}}`, http.StatusUnauthorized)
		return
	}

	// 2. Route request
	if r.URL.Path == "/v1/models" && r.Method == http.MethodGet {
		ph.handleModels(w, r)
		return
	}

	if r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost {
		ph.handleChatCompletions(w, r)
		return
	}

	http.Error(w, `{"error":{"message":"Not Found"}}`, http.StatusNotFound)
}

func (ph *ProxyHandler) handleModels(w http.ResponseWriter, r *http.Request) {
	topModels := ph.rankingMgr.GetTopModels()

	type ModelItem struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}

	var data []ModelItem

	// Add aliases
	data = append(data, ModelItem{ID: "top1", Object: "model", Created: time.Now().Unix(), OwnedBy: "shir-man"})
	data = append(data, ModelItem{ID: "top2", Object: "model", Created: time.Now().Unix(), OwnedBy: "shir-man"})
	data = append(data, ModelItem{ID: "top3", Object: "model", Created: time.Now().Unix(), OwnedBy: "shir-man"})
	data = append(data, ModelItem{ID: "openrouter/free", Object: "model", Created: time.Now().Unix(), OwnedBy: "openrouter"})

	// Add other real models
	for _, m := range topModels {
		data = append(data, ModelItem{
			ID:      m.ID,
			Object:  "model",
			Created: m.UpdatedAt.Unix(),
			OwnedBy: "openrouter",
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   data,
	})
}

func (ph *ProxyHandler) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	// Read request body
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":{"message":"Failed to read request body"}}`, http.StatusBadRequest)
		return
	}

	// Decode partially to inspect model and stream
	var chatReq ChatCompletionsRequest
	if err := json.Unmarshal(bodyBytes, &chatReq); err != nil {
		http.Error(w, `{"error":{"message":"Failed to parse JSON request"}}`, http.StatusBadRequest)
		return
	}

	originalModel := chatReq.Model
	resolvedModel := originalModel

	// Resolve model if alias (top1, top2, top3)
	if aliasModel, ok := ph.rankingMgr.ResolveAlias(originalModel); ok {
		resolvedModel = aliasModel
	}

	// Verify model is free
	if !ph.rankingMgr.IsFreeModel(resolvedModel) {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"Model %s is not supported (only Shir-Man free models allowed)"}}`, originalModel), http.StatusBadRequest)
		return
	}

	// Replace model in request body bytes if resolved to a different ID
	if resolvedModel != originalModel {
		var rawMap map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &rawMap); err == nil {
			rawMap["model"] = resolvedModel
			if updatedBytes, err := json.Marshal(rawMap); err == nil {
				bodyBytes = updatedBytes
			}
		}
	}

	// Execute request with retries over different keys
	var finalErr error
	for attempt := 1; attempt <= ph.cfg.MaxKeyRetries; attempt++ {
		keyState, err := ph.pool.GetBestKey()
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":{"message":"%v"}}`, err), http.StatusServiceUnavailable)
			return
		}

		// GetBestKey already reserved (registered) this key atomically.
		ph.pool.SyncKeyToDB(keyState)

		log.Printf("[Attempt %d/%d] Proxying %s -> %s using key %s", attempt, ph.cfg.MaxKeyRetries, originalModel, resolvedModel, keyState.MaskedKey)

		// Create OpenRouter Request
		req, err := http.NewRequest("POST", "https://openrouter.ai/api/v1/chat/completions", bytes.NewReader(bodyBytes))
		if err != nil {
			log.Printf("Failed to create OpenRouter request: %v", err)
			http.Error(w, `{"error":{"message":"Internal gateway error creating request"}}`, http.StatusInternalServerError)
			return
		}

		// Set Headers
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+keyState.RawKey)
		req.Header.Set("User-Agent", "OpenRouterGateway/1.0")
		req.Header.Set("HTTP-Referer", "https://shir-man.com/free-llm")
		req.Header.Set("X-Title", "OpenRouter Free Gateway")

		// We must not set connection close, let client decide or keepalive
		startTime := time.Now()
		resp, err := ph.client.Do(req)
		if err != nil {
			log.Printf("Network error making OpenRouter request: %v", err)
			keyState.SetCooldown(30*time.Second, "")
			ph.pool.SyncKeyToDB(keyState)
			finalErr = err
			continue
		}

		// Handle key cooldown / limits based on headers and status code
		ParseRateLimits(keyState, resp.Header)

		if resp.StatusCode >= 400 {
			// Read body to inspect if it's a credit/quota issue
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			cooldown := HandleProxyError(keyState, resp)

			// If it's explicitly "credit exhausted" or similar, mark as day_exhausted
			if IsQuotaExhaustedError(respBody) {
				log.Printf("Key %s has exhausted its quota (detected in response body). Marking day_exhausted.", keyState.MaskedKey)
				keyState.SetStatus("day_exhausted")
			}

			ph.pool.SyncKeyToDB(keyState)

			log.Printf("Request failed with status %d on key %s. Cooldown applied: %v", resp.StatusCode, keyState.MaskedKey, cooldown)
			finalErr = fmt.Errorf("openrouter returned status %d: %s", resp.StatusCode, string(respBody))

			// Retry with another key
			continue
		}

		// Success! Stream or return response
		defer resp.Body.Close()
		latencyMs := time.Since(startTime).Milliseconds()

		if chatReq.Stream {
			ph.handleStreamResponse(w, resp, keyState, resolvedModel, latencyMs)
		} else {
			ph.handleNormalResponse(w, resp, keyState, resolvedModel, latencyMs)
		}
		return
	}

	// If we exhausted all retries
	log.Printf("All %d retries failed for request. Last error: %v", ph.cfg.MaxKeyRetries, finalErr)
	http.Error(w, fmt.Sprintf(`{"error":{"message":"Gateway exhausted all retries. Last error: %v"}}`, finalErr), http.StatusBadGateway)
}

func (ph *ProxyHandler) handleNormalResponse(w http.ResponseWriter, resp *http.Response, ks *keys.KeyState, model string, latencyMs int64) {
	// Read body
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read response body: %v", err)
		http.Error(w, `{"error":{"message":"Failed to read response from upstream"}}`, http.StatusBadGateway)
		return
	}

	// Copy Headers
	for k, v := range resp.Header {
		if k != "Content-Length" && k != "Content-Encoding" {
			w.Header()[k] = v
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)

	// Parse Usage
	var promptTokens, completionTokens int
	var usageStruct struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(respBody, &usageStruct); err == nil {
		promptTokens = usageStruct.Usage.PromptTokens
		completionTokens = usageStruct.Usage.CompletionTokens
	}

	// Log request to DB
	err = ph.store.LogRequest(&store.DBRequest{
		Timestamp:        time.Now(),
		KeyHash:          ks.KeyHash,
		Model:            model,
		StatusCode:       resp.StatusCode,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		LatencyMs:        latencyMs,
	})
	if err != nil {
		log.Printf("Failed to log request to DB: %v", err)
	}
}

func (ph *ProxyHandler) handleStreamResponse(w http.ResponseWriter, resp *http.Response, ks *keys.KeyState, model string, latencyMs int64) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("ResponseWriter does not support Flusher")
		http.Error(w, `{"error":{"message":"Streaming not supported by gateway"}}`, http.StatusInternalServerError)
		return
	}

	// Copy Headers
	for k, v := range resp.Header {
		if k != "Content-Encoding" && k != "Content-Length" {
			w.Header()[k] = v
		}
	}
	w.WriteHeader(resp.StatusCode)

	reader := bufio.NewReader(resp.Body)
	var promptTokens, completionTokens int

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			// Write chunk to client
			_, writeErr := w.Write(line)
			if writeErr != nil {
				log.Printf("Client disconnected during stream: %v", writeErr)
				break
			}
			flusher.Flush()

			// Parse usage in stream chunks if present.
			// OpenRouter SSE stream contains lines: `data: {"id":"...","usage":{"prompt_tokens":10,"completion_tokens":20}}`
			lineStr := string(line)
			if strings.HasPrefix(lineStr, "data:") {
				dataJSON := strings.TrimPrefix(lineStr, "data:")
				dataJSON = strings.TrimSpace(dataJSON)

				if dataJSON != "[DONE]" {
					var usageStruct struct {
						Usage struct {
							PromptTokens     int `json:"prompt_tokens"`
							CompletionTokens int `json:"completion_tokens"`
						} `json:"usage"`
					}
					if err := json.Unmarshal([]byte(dataJSON), &usageStruct); err == nil {
						if usageStruct.Usage.PromptTokens > 0 {
							promptTokens = usageStruct.Usage.PromptTokens
							completionTokens = usageStruct.Usage.CompletionTokens
						}
					}
				}
			}
		}

		if err != nil {
			if err != io.EOF {
				log.Printf("Error reading stream from upstream: %v", err)
			}
			break
		}
	}

	// Log request to DB
	err := ph.store.LogRequest(&store.DBRequest{
		Timestamp:        time.Now(),
		KeyHash:          ks.KeyHash,
		Model:            model,
		StatusCode:       resp.StatusCode,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		LatencyMs:        latencyMs,
	})
	if err != nil {
		log.Printf("Failed to log request to DB: %v", err)
	}
}
