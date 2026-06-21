package keys

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"
)

type OpenRouterKeyResponse struct {
	Data struct {
		Limit          *int64 `json:"limit"`
		Usage          *int64 `json:"usage"`
		LimitRemaining *int64 `json:"limit_remaining"`
		IsFreeTier     *bool  `json:"is_free_tier"`
		RateLimit      *struct {
			Requests int     `json:"requests"`
			Interval *string `json:"interval"`
		} `json:"rate_limit"`
	} `json:"data"`
}

type KeyChecker struct {
	pool        *KeyPool
	ttl         time.Duration
	rateLimit   int
	interval    time.Duration
	concurrency int
	client      *http.Client
	stopChan    chan struct{}
	wg          sync.WaitGroup
}

func NewKeyChecker(pool *KeyPool, ttl time.Duration, rateLimit int, interval time.Duration, concurrency int) *KeyChecker {
	return &KeyChecker{
		pool:        pool,
		ttl:         ttl,
		rateLimit:   rateLimit,
		interval:    interval,
		concurrency: concurrency,
		client:      &http.Client{Timeout: 10 * time.Second},
		stopChan:    make(chan struct{}),
	}
}

func (kc *KeyChecker) Start() {
	log.Printf("Starting background Key Checker (rate limit: %d per %v, concurrency: %d)...", kc.rateLimit, kc.interval, kc.concurrency)

	kc.wg.Add(1)
	go kc.runLoop()
}

func (kc *KeyChecker) Stop() {
	close(kc.stopChan)
	kc.wg.Wait()
	log.Println("Background Key Checker stopped.")
}

func (kc *KeyChecker) runLoop() {
	defer kc.wg.Done()

	// Delay between putting keys into work queue to strictly respect the global rate limit.
	// E.g., 200 checks/min -> 60s / 200 = 300ms delay.
	delay := kc.interval / time.Duration(kc.rateLimit)
	ticker := time.NewTicker(delay)
	defer ticker.Stop()

	// Channel for worker tasks
	taskChan := make(chan *KeyState, kc.concurrency)

	// Start workers
	var workerWg sync.WaitGroup
	for i := 0; i < kc.concurrency; i++ {
		workerWg.Add(1)
		go func(workerID int) {
			defer workerWg.Done()
			for ks := range taskChan {
				kc.CheckKey(ks)
			}
		}(i)
	}

	for {
		select {
		case <-kc.stopChan:
			close(taskChan)
			workerWg.Wait()
			return
		case <-ticker.C:
			// Find a key that needs verification
			ks := kc.findKeyToVerify()
			if ks != nil {
				select {
				case taskChan <- ks:
				case <-kc.stopChan:
					close(taskChan)
					workerWg.Wait()
					return
				}
			}
		}
	}
}

func (kc *KeyChecker) findKeyToVerify() *KeyState {
	allKeys := kc.pool.AllKeys()
	now := time.Now()

	// E.g., we want to check keys that are unchecked, or check active/limited keys older than TTL.
	// Prioritize unchecked, then old keys.
	var bestCandidate *KeyState
	var bestPriority int // 3: unchecked, 2: rate_limited (older than 10m), 1: other keys older than TTL

	for _, ks := range allKeys {
		ks.mu.Lock()
		status := ks.Status
		lastChecked := ks.LastCheckedAt
		ks.mu.Unlock()

		if status == "disabled" {
			continue
		}

		if status == "unchecked" || lastChecked.IsZero() {
			bestCandidate = ks
			bestPriority = 3
			break // Top priority, check immediately
		}

		if status == "rate_limited" && now.Sub(lastChecked) > 10*time.Minute && bestPriority < 2 {
			bestCandidate = ks
			bestPriority = 2
		}

		if now.Sub(lastChecked) > kc.ttl && status != "invalid" && bestPriority < 1 {
			bestCandidate = ks
			bestPriority = 1
		}
	}

	return bestCandidate
}

func (kc *KeyChecker) CheckKey(ks *KeyState) {
	req, err := http.NewRequest("GET", "https://openrouter.ai/api/v1/key", nil)
	if err != nil {
		log.Printf("Failed to create verification request: %v", err)
		return
	}

	ks.mu.Lock()
	rawKey := ks.RawKey
	masked := ks.MaskedKey
	ks.mu.Unlock()

	req.Header.Set("Authorization", "Bearer "+rawKey)
	req.Header.Set("User-Agent", "OpenRouterGateway/1.0")

	now := time.Now()
	resp, err := kc.client.Do(req)
	if err != nil {
		// Connection error or timeout
		log.Printf("Network error verifying key %s: %v", masked, err)
		ks.SetCooldown(1*time.Minute, "")
		return
	}
	defer resp.Body.Close()

	// Decode the body BEFORE taking the lock so we never hold ks.mu across I/O.
	var keyResp OpenRouterKeyResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&keyResp); err != nil {
			log.Printf("Failed to decode key response for %s: %v", masked, err)
			return
		}
	}

	ks.mu.Lock()
	defer func() {
		ks.LastCheckedAt = now
		ks.mu.Unlock()
		kc.pool.SyncKeyToDB(ks)
	}()

	switch resp.StatusCode {
	case http.StatusOK:
		// Update state with server data
		if keyResp.Data.Limit != nil {
			ks.MaxLimit = *keyResp.Data.Limit
		}
		if keyResp.Data.LimitRemaining != nil {
			ks.LimitRemaining = *keyResp.Data.LimitRemaining
		}
		if keyResp.Data.IsFreeTier != nil {
			ks.IsFreeTier = *keyResp.Data.IsFreeTier
		}
		if keyResp.Data.RateLimit != nil {
			ks.RateLimitReq = keyResp.Data.RateLimit.Requests
			if keyResp.Data.RateLimit.Interval != nil {
				ks.RateLimitInterval = *keyResp.Data.RateLimit.Interval
			}
		}

		// Update Status
		if ks.LimitRemaining <= 0 && ks.MaxLimit > 0 {
			ks.Status = "day_exhausted"
		} else {
			ks.Status = "active"
		}

	case http.StatusUnauthorized, http.StatusForbidden:
		log.Printf("Key %s is INVALID (Status: %d)", masked, resp.StatusCode)
		ks.Status = "invalid"

	case http.StatusTooManyRequests:
		log.Printf("Key %s returned 429 during check, setting cooldown", masked)
		ks.Status = "rate_limited"
		ks.CooldownUntil = now.Add(5 * time.Minute)

	default:
		log.Printf("Unexpected status verifying key %s: %d", masked, resp.StatusCode)
		ks.CooldownUntil = now.Add(1 * time.Minute)
	}
}
