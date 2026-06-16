package keys

import (
	"sync"
	"testing"
	"time"

	"openrouter-gateway/internal/store"
)

func newState(rateLimit int) *KeyState {
	return NewKeyState("sk-or-v1-test", &store.DBKey{
		KeyHash:      "h",
		MaskedKey:    "m",
		Status:       "active",
		RateLimitReq: rateLimit,
	})
}

// TryReserve must never let more reservations through than the per-minute limit,
// even under concurrent callers. This is the core ratelimit invariant.
func TestTryReserveRespectsLimitConcurrently(t *testing.T) {
	const limit = 20
	ks := newState(limit)
	now := time.Now()

	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ks.TryReserve(now) {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if granted != limit {
		t.Fatalf("expected exactly %d reservations, got %d", limit, granted)
	}
	if ks.UsageToday != int64(limit) {
		t.Fatalf("UsageToday = %d, want %d", ks.UsageToday, limit)
	}
}

// Window must reopen once old timestamps age out past one minute.
func TestTryReserveWindowRolls(t *testing.T) {
	ks := newState(2)
	base := time.Now()

	if !ks.TryReserve(base) || !ks.TryReserve(base) {
		t.Fatal("first two reservations should succeed")
	}
	if ks.TryReserve(base) {
		t.Fatal("third reservation within the minute must fail")
	}
	if !ks.TryReserve(base.Add(61 * time.Second)) {
		t.Fatal("reservation after window expiry should succeed")
	}
}

// Daily usage resets when the calendar day changes (local time).
func TestResetDailyUsageIfNewDay(t *testing.T) {
	ks := newState(20)
	ks.LastUsedAt = time.Now().Add(-48 * time.Hour)
	ks.UsageToday = 50
	ks.Status = "day_exhausted"

	if !ks.ResetDailyUsageIfNewDay() {
		t.Fatal("expected reset to report a day change")
	}
	if ks.UsageToday != 0 {
		t.Fatalf("UsageToday not reset: %d", ks.UsageToday)
	}
	if ks.Status != "active" {
		t.Fatalf("status not reactivated: %s", ks.Status)
	}
}
