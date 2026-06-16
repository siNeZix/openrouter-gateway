package keys

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"openrouter-gateway/internal/store"
)

type KeyPool struct {
	store    *store.Store
	keysFile string

	mu      sync.RWMutex
	keys    []*KeyState
	keysMap map[string]*KeyState // hash -> KeyState
}

func NewKeyPool(s *store.Store, keysFile string) (*KeyPool, error) {
	pool := &KeyPool{
		store:    s,
		keysFile: keysFile,
		keysMap:  make(map[string]*KeyState),
	}

	if err := pool.Load(); err != nil {
		return nil, fmt.Errorf("failed to load key pool: %w", err)
	}

	return pool, nil
}

func (kp *KeyPool) Load() error {
	kp.mu.Lock()
	defer kp.mu.Unlock()

	// 1. Read keys from file
	absPath, err := filepath.Abs(kp.keysFile)
	if err != nil {
		return fmt.Errorf("invalid keys file path: %w", err)
	}

	file, err := os.Open(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			// If file doesn't exist, create an empty one
			dir := filepath.Dir(absPath)
			if err := os.MkdirAll(dir, 0755); err != nil {
				return fmt.Errorf("failed to create directory %s: %w", dir, err)
			}
			emptyFile, err := os.Create(absPath)
			if err != nil {
				return fmt.Errorf("failed to create empty keys file: %w", err)
			}
			emptyFile.Close()
			log.Printf("Created empty keys file at %s", absPath)
			return nil
		}
		return err
	}
	defer file.Close()

	var rawKeys []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		rawKeys = append(rawKeys, line)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading keys file: %w", err)
	}

	// Remove duplicates
	rawKeys = uniqueStrings(rawKeys)

	// 2. Sync with SQLite
	if err := kp.store.SyncKeys(rawKeys); err != nil {
		return fmt.Errorf("failed to sync keys in database: %w", err)
	}

	// 3. Load from SQLite
	dbKeys, err := kp.store.GetKeys()
	if err != nil {
		return fmt.Errorf("failed to fetch keys from database: %w", err)
	}

	// Create a fast lookup map of raw keys by their hash
	rawKeysByHash := make(map[string]string)
	for _, rk := range rawKeys {
		rawKeysByHash[store.HashKey(rk)] = rk
	}

	// 4. Build in-memory states
	kp.keys = nil
	kp.keysMap = make(map[string]*KeyState)

	for _, dbK := range dbKeys {
		raw, ok := rawKeysByHash[dbK.KeyHash]
		if !ok {
			// This key is in SQLite but not in api_keys.txt. Store.SyncKeys should have deleted it,
			// but we can skip it to be safe.
			continue
		}
		ks := NewKeyState(raw, dbK)
		kp.keys = append(kp.keys, ks)
		kp.keysMap[dbK.KeyHash] = ks
	}

	log.Printf("Key pool initialized. Total active keys loaded: %d", len(kp.keys))
	return nil
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
