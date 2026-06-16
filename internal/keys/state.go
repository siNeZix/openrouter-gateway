package keys

import (
	"sync"
	"time"

	"openrouter-gateway/internal/store"
)

type KeyState struct {
	mu sync.Mutex

	RawKey            string
	KeyHash           string
	MaskedKey         string
	Status            string // unchecked, active, rate_limited, day_exhausted, invalid
	LimitRemaining    int64
	UsageToday        int64
	MaxLimit          int64
	IsFreeTier        bool
	RateLimitReq      int
	RateLimitInterval string
	CooldownUntil     time.Time
	LastCheckedAt     time.Time
	LastUsedAt        time.Time

	// Sliding window rate limit tracking (1 minute)
	RequestTimes []time.Time
}

func NewKeyState(rawKey string, dbKey *store.DBKey) *KeyState {
	return &KeyState{
		RawKey:            rawKey,
		KeyHash:           dbKey.KeyHash,
		MaskedKey:         dbKey.MaskedKey,
		Status:            dbKey.Status,
		LimitRemaining:    dbKey.LimitRemaining,
		UsageToday:        dbKey.UsageToday,
		MaxLimit:          dbKey.MaxLimit,
		IsFreeTier:        dbKey.IsFreeTier,
		RateLimitReq:      dbKey.RateLimitReq,
		RateLimitInterval: dbKey.RateLimitInterval,
		CooldownUntil:     dbKey.CooldownUntil,
		LastCheckedAt:     dbKey.LastCheckedAt,
		LastUsedAt:        dbKey.LastUsedAt,
		RequestTimes:      make([]time.Time, 0),
	}
}

func (ks *KeyState) ToDB() *store.DBKey {
	ks.mu.Lock()
	defer ks.mu.Unlock()

	return &store.DBKey{
		KeyHash:           ks.KeyHash,
		MaskedKey:         ks.MaskedKey,
		Status:            ks.Status,
		LimitRemaining:    ks.LimitRemaining,
		UsageToday:        ks.UsageToday,
		MaxLimit:          ks.MaxLimit,
		IsFreeTier:        ks.IsFreeTier,
		RateLimitReq:      ks.RateLimitReq,
		RateLimitInterval: ks.RateLimitInterval,
		CooldownUntil:     ks.CooldownUntil,
		LastCheckedAt:     ks.LastCheckedAt,
		LastUsedAt:        ks.LastUsedAt,
	}
}

// ResetDailyUsageIfNewDay resets usage_today if the calendar day has changed
func (ks *KeyState) ResetDailyUsageIfNewDay() bool {
	if ks.LastUsedAt.IsZero() {
		return false
	}

	now := time.Now()
	// Compare year, month, and day in local timezone
	y1, m1, d1 := ks.LastUsedAt.Date()
	y2, m2, d2 := now.Date()

	if y1 != y2 || m1 != m2 || d1 != d2 {
		ks.UsageToday = 0
		if ks.Status == "day_exhausted" {
			ks.Status = "active"
		}
		return true
	}
	return false
}

// CleanOldRequests cleans up history older than 1 minute
func (ks *KeyState) cleanOldRequests(now time.Time) {
	threshold := now.Add(-time.Minute)
	validIdx := 0
	for i, t := range ks.RequestTimes {
		if t.After(threshold) {
			validIdx = i
			break
		}
		if i == len(ks.RequestTimes)-1 {
			// all are old
			ks.RequestTimes = ks.RequestTimes[:0]
			return
		}
	}
	if validIdx > 0 {
		ks.RequestTimes = ks.RequestTimes[validIdx:]
	}
}

// CanUse checks if the key is allowed to make a request right now
func (ks *KeyState) CanUse(now time.Time) bool {
	ks.mu.Lock()
	defer ks.mu.Unlock()

	// Check calendar day reset
	ks.ResetDailyUsageIfNewDay()

	if ks.Status == "invalid" {
		return false
	}
	if ks.Status == "day_exhausted" {
		return false
	}
	if ks.CooldownUntil.After(now) {
		return false
	}

	ks.cleanOldRequests(now)

	// Check if minute limit exceeded (default 20 requests/min if zero/unset)
	limit := ks.RateLimitReq
	if limit <= 0 {
		limit = 20
	}
	if len(ks.RequestTimes) >= limit {
		return false
	}

	return true
}

// RegisterRequest increments usage and records request timestamp for rate limiting
func (ks *KeyState) RegisterRequest(now time.Time) {
	ks.mu.Lock()
	defer ks.mu.Unlock()

	ks.ResetDailyUsageIfNewDay()

	ks.UsageToday++
	if ks.LimitRemaining > 0 {
		ks.LimitRemaining--
	}
	ks.LastUsedAt = now
	ks.RequestTimes = append(ks.RequestTimes, now)

	// Optimistically assume active if it was unchecked or rate_limited (and cooldown is over)
	if ks.Status == "unchecked" || ks.Status == "rate_limited" {
		ks.Status = "active"
	}
}

// SetCooldown sets cooldown after a 429 or 5xx error
func (ks *KeyState) SetCooldown(duration time.Duration, status string) {
	ks.mu.Lock()
	defer ks.mu.Unlock()

	now := time.Now()
	ks.CooldownUntil = now.Add(duration)
	if status != "" {
		ks.Status = status
	}
}

// SetStatus directly sets the key status
func (ks *KeyState) SetStatus(status string) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	ks.Status = status
}

// UpdateLimitRemaining thread-safely updates the remaining limit of the key
func (ks *KeyState) UpdateLimitRemaining(val int64) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	ks.LimitRemaining = val
}
