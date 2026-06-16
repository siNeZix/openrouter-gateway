package keys

import (
	"fmt"
	"log"
	"sync"
	"time"

	"openrouter-gateway/internal/store"
)

type KeyPool struct {
	store *store.Store

	mu      sync.RWMutex
	keys    []*KeyState
	keysMap map[string]*KeyState // hash -> KeyState
}

func NewKeyPool(s *store.Store) (*KeyPool, error) {
	pool := &KeyPool{
		store:   s,
		keysMap: make(map[string]*KeyState),
	}

	if err := pool.Load(); err != nil {
		return nil, fmt.Errorf("failed to load key pool: %w", err)
	}

	return pool, nil
}

func (kp *KeyPool) Load() error {
	kp.mu.Lock()
	defer kp.mu.Unlock()

	// 1. Load from SQLite
	dbKeys, err := kp.store.GetKeys()
	if err != nil {
		return fmt.Errorf("failed to fetch keys from database: %w", err)
	}

	// 2. Build in-memory states, keeping existing KeyState pointers if possible to preserve cooldowns/counters
	newKeys := make([]*KeyState, 0, len(dbKeys))
	newKeysMap := make(map[string]*KeyState)

	for _, dbK := range dbKeys {
		if dbK.RawKey == "" {
			// ponytail: skip legacy hash-only keys. Upgrading to web GUI requires deleting/re-adding them.
			continue
		}

		var ks *KeyState
		if existing, ok := kp.keysMap[dbK.KeyHash]; ok {
			// Preserve in-memory counters/sliding windows but update DB parameters
			existing.mu.Lock()
			existing.Status = dbK.Status
			existing.LimitRemaining = dbK.LimitRemaining
			existing.MaxLimit = dbK.MaxLimit
			existing.IsFreeTier = dbK.IsFreeTier
			existing.RateLimitReq = dbK.RateLimitReq
			existing.RateLimitInterval = dbK.RateLimitInterval
			existing.CooldownUntil = dbK.CooldownUntil
			existing.LastCheckedAt = dbK.LastCheckedAt
			existing.LastUsedAt = dbK.LastUsedAt
			existing.mu.Unlock()
			ks = existing
		} else {
			ks = NewKeyState(dbK.RawKey, dbK)
		}

		newKeys = append(newKeys, ks)
		newKeysMap[dbK.KeyHash] = ks
	}

	kp.keys = newKeys
	kp.keysMap = newKeysMap

	log.Printf("Key pool loaded from database. Total active keys: %d", len(kp.keys))
	return nil
}

func (kp *KeyPool) AddKeys(rawKeys []string) (int, error) {
	// Deduplicate raw strings
	rawKeys = uniqueStrings(rawKeys)
	if len(rawKeys) == 0 {
		return 0, nil
	}

	added, err := kp.store.AddKeys(rawKeys)
	if err != nil {
		return 0, err
	}

	// Reload pool to pick up new keys
	if err := kp.Load(); err != nil {
		return added, fmt.Errorf("failed to reload pool after adding keys: %w", err)
	}

	return added, nil
}

func (kp *KeyPool) RemoveKey(hash string) error {
	if err := kp.store.DeleteKey(hash); err != nil {
		return err
	}

	// Reload pool to exclude deleted key
	return kp.Load()
}

// GetBestKey implements smart selection: least used today, not in cooldown, fits minute limit
func (kp *KeyPool) GetBestKey() (*KeyState, error) {
	kp.mu.RLock()
	defer kp.mu.RUnlock()

	if len(kp.keys) == 0 {
		return nil, fmt.Errorf("key pool is empty")
	}

	now := time.Now()
	var best *KeyState

	for _, k := range kp.keys {
		if !k.CanUse(now) {
			continue
		}

		if best == nil {
			best = k
			continue
		}

		// Pick key with least usage today
		best.mu.Lock()
		bestUsage := best.UsageToday
		best.mu.Unlock()

		k.mu.Lock()
		kUsage := k.UsageToday
		k.mu.Unlock()

		if kUsage < bestUsage {
			best = k
		}
	}

	if best == nil {
		return nil, fmt.Errorf("all keys are rate limited, exhausted or in cooldown (pool size: %d)", len(kp.keys))
	}

	return best, nil
}

func (kp *KeyPool) AllKeys() []*KeyState {
	kp.mu.RLock()
	defer kp.mu.RUnlock()

	res := make([]*KeyState, len(kp.keys))
	copy(res, kp.keys)
	return res
}

func (kp *KeyPool) SyncKeyToDB(ks *KeyState) {
	dbK := ks.ToDB()
	if err := kp.store.UpdateKey(dbK); err != nil {
		log.Printf("Failed to sync key %s to database: %v", ks.MaskedKey, err)
	}
}

func uniqueStrings(slice []string) []string {
	keys := make(map[string]bool)
	list := []string{}
	for _, entry := range slice {
		if _, value := keys[entry]; !value {
			keys[entry] = true
			list = append(list, entry)
		}
	}
	return list
}
