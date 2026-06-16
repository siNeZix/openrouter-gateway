package keys

import (
	"slices"
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

// cleanOldRequests drops timestamps older than 1 minute. Caller holds ks.mu.
func (ks *KeyState) cleanOldRequests(now time.Time) {
	threshold := now.Add(-time.Minute)
	ks.RequestTimes = slices.DeleteFunc(ks.RequestTimes, func(t time.Time) bool {
		return !t.After(threshold)
	})
}

// usable reports whether the key may serve a request now. Caller holds ks.mu.
func (ks *KeyState) usable(now time.Time) bool {
	ks.ResetDailyUsageIfNewDay()

	if ks.Status == "invalid" || ks.Status == "day_exhausted" {
		return false
	}
	if ks.CooldownUntil.After(now) {
		return false
	}

	ks.cleanOldRequests(now)

	// Default 20 requests/min if the per-key limit is unknown.
	limit := ks.RateLimitReq
	if limit <= 0 {
		limit = 20
	}
	return len(ks.RequestTimes) < limit
}

// CanUse checks if the key is allowed to make a request right now.
func (ks *KeyState) CanUse(now time.Time) bool {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	return ks.usable(now)
}

// TryReserve atomically checks usability and, if usable, registers the request.
// Returns false without mutating if the key cannot be used. This closes the
// select-then-use race between GetBestKey and RegisterRequest.
func (ks *KeyState) TryReserve(now time.Time) bool {
	ks.mu.Lock()
	defer ks.mu.Unlock()

	if !ks.usable(now) {
		return false
	}
	ks.registerLocked(now)
	return true
}

// RegisterRequest increments usage and records request timestamp for rate limiting.
func (ks *KeyState) RegisterRequest(now time.Time) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	ks.registerLocked(now)
}

// registerLocked performs the request accounting. Caller holds ks.mu.
func (ks *KeyState) registerLocked(now time.Time) {
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
