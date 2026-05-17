package keypool

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// stateFile is the persisted JSON shape on disk at
// ~/.config/oc-go-cc/key-states.json.
type stateFile struct {
	APIKeys      []*Key       `json:"api_keys"`
	FreeFallback FreeFallback `json:"free_fallback"`
}

// FreeFallback is the configuration block describing the free-tier endpoint
// and model fallback order. Persisted alongside keys so the entire pool
// configuration lives in one file.
type FreeFallback struct {
	BaseURL string   `json:"base_url"`
	Models  []string `json:"models"`
}

// KeyPool holds a thread-safe pool of API keys with state persistence.
//
// Concurrency: all public methods acquire the internal RWMutex appropriately.
// Acquire/MarkExhausted/SeedKeys take the write lock; Snapshot/FreeFallback
// take the read lock. Callers must NOT hold the lock when invoking public
// methods.
type KeyPool struct {
	mu           sync.RWMutex
	statePath    string
	keys         []*Key
	freeFallback FreeFallback
	logger       *slog.Logger
	emitter      EventEmitter
	lastMtime    time.Time
}

// New constructs a KeyPool bound to a state file path. The pool is empty
// until LoadFromDisk() succeeds or SeedKeys() is called.
func New(statePath string, logger *slog.Logger) *KeyPool {
	if logger == nil {
		logger = slog.Default()
	}
	return &KeyPool{
		statePath: statePath,
		logger:    logger,
		emitter:   noopEmitter{},
	}
}

// SetEmitter wires the rotation event log. Pass nil to disable events.
func (p *KeyPool) SetEmitter(e EventEmitter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e == nil {
		e = noopEmitter{}
	}
	p.emitter = e
}

// LoadFromDisk reads the state file and populates the pool. If the file
// does not exist, returns nil error and leaves the pool empty (the caller
// is expected to invoke SeedKeys with the initial set).
//
// If the state file exists but contains invalid JSON, it is renamed to
// "<path>.corrupt.<timestamp>" and the pool is left empty so the proxy
// can recover without a startup crash.
func (p *KeyPool) LoadFromDisk() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	data, err := os.ReadFile(p.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read state file %s: %w", p.statePath, err)
	}

	var sf stateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		corruptPath := p.statePath + ".corrupt." + time.Now().UTC().Format("20060102-150405")
		if renameErr := os.Rename(p.statePath, corruptPath); renameErr != nil {
			p.logger.Error("corrupted state file could not be renamed", "path", p.statePath, "err", renameErr)
		} else {
			p.logger.Warn("corrupted state file renamed; pool starting empty", "from", p.statePath, "to", corruptPath, "parse_err", err)
		}
		return nil
	}

	p.keys = sf.APIKeys
	p.freeFallback = sf.FreeFallback

	if info, statErr := os.Stat(p.statePath); statErr == nil {
		p.lastMtime = info.ModTime()
	}
	return nil
}

// SeedKeys replaces the in-memory pool and persists immediately. Intended
// for config-driven initialization (first run or migration).
func (p *KeyPool) SeedKeys(keys []*Key, freeFallback FreeFallback) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys = keys
	p.freeFallback = freeFallback
	return p.saveLocked()
}

// FreeFallback returns the free-tier configuration. Safe for concurrent reads.
func (p *KeyPool) FreeFallback() FreeFallback {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.freeFallback
}

// EarliestResetDate returns the soonest reset date across all exhausted keys.
// Used by the all-exhausted 502 response so clients know when to retry.
// Returns zero time if no keys are exhausted.
func (p *KeyPool) EarliestResetDate() time.Time {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var earliest time.Time
	for _, k := range p.keys {
		if k.IsActive() {
			continue
		}
		if earliest.IsZero() || k.ResetDate.Before(earliest) {
			earliest = k.ResetDate
		}
	}
	return earliest
}

// Snapshot returns a deep copy of pool keys for read-only inspection
// (e.g. by the keys-status CLI).
func (p *KeyPool) Snapshot() []Key {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]Key, len(p.keys))
	for i, k := range p.keys {
		out[i] = *k
	}
	return out
}

// Acquire returns a snapshot of the first active key in the pool. Reset
// dates are checked first — any key whose resetDate has passed is
// reactivated and a key_reset_autoclear event is emitted.
//
// Returns nil, ErrAllExhausted when no active key is available. Handlers
// receiving this error should fall through to the free-fallback resolver.
//
// The returned *Key is a defensive copy; mutating it does not affect
// pool state.
func (p *KeyPool) Acquire() (*Key, error) {
	now := time.Now()

	p.mu.Lock()
	defer p.mu.Unlock()

	resetHappened := false
	for _, k := range p.keys {
		if !k.IsActive() && k.IsResetDue(now) {
			k.ExhaustedAt = nil
			p.emitter.Emit(Event{
				Timestamp: now,
				Type:      EventKeyResetAutoclear,
				Account:   k.Account,
				Reason:    "resetDate <= today",
			})
			resetHappened = true
		}
	}
	if resetHappened {
		_ = p.saveLocked()
	}

	for _, k := range p.keys {
		if k.IsActive() {
			ts := now
			k.LastUsed = &ts
			p.emitter.Emit(Event{
				Timestamp: now,
				Type:      EventKeyAcquired,
				Account:   k.Account,
			})
			cp := *k
			return &cp, nil
		}
	}

	return nil, ErrAllExhausted
}

// MarkExhausted records that the key with the matching token has hit its
// quota. resetDate is when the key will reactivate (computed by the 429
// classifier). Reason is a free-form label written to the rotation log.
//
// Returns ErrKeyNotFound if no key in the pool has the given token.
func (p *KeyPool) MarkExhausted(token string, resetDate time.Time, reason string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	for _, k := range p.keys {
		if k.Token != token {
			continue
		}
		k.ExhaustedAt = &now
		if !resetDate.IsZero() {
			k.ResetDate = resetDate
		}
		p.emitter.Emit(Event{
			Timestamp: now,
			Type:      EventKeyExhaustedHard,
			Account:   k.Account,
			Reason:    reason,
		})
		return p.saveLocked()
	}
	return ErrKeyNotFound
}

// ClearExhausted reverts a key's exhaustion state — clears ExhaustedAt and
// ResetDate, persists, and emits EventKeyRevived. Used by the background
// revalidator when a probe succeeds, by the manual clear-exhausted CLI,
// and by future operator workflows.
//
// Returns ErrKeyNotFound if no key in the pool has the given token.
// No-op if the key is already active — returns nil without emit.
func (p *KeyPool) ClearExhausted(token, reason string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	for _, k := range p.keys {
		if k.Token != token {
			continue
		}
		if k.IsActive() {
			return nil
		}
		k.ExhaustedAt = nil
		k.ResetDate = time.Time{}
		p.emitter.Emit(Event{
			Timestamp: now,
			Type:      EventKeyRevived,
			Account:   k.Account,
			Reason:    reason,
		})
		return p.saveLocked()
	}
	return ErrKeyNotFound
}

// MarkTransient records a transient throttle event. The key is NOT marked
// exhausted; only the rotation log gets an entry. Used when the 429
// classifier returns DecisionTransient.
func (p *KeyPool) MarkTransient(token string, retryAfter time.Duration, reason string) {
	p.mu.RLock()
	var account string
	for _, k := range p.keys {
		if k.Token == token {
			account = k.Account
			break
		}
	}
	p.mu.RUnlock()

	retryAfterSec := int(retryAfter.Seconds())
	p.emitter.Emit(Event{
		Timestamp:  time.Now(),
		Type:       EventKeyThrottleTransient,
		Account:    account,
		RetryAfter: &retryAfterSec,
		Reason:     reason,
	})
}

// saveLocked writes the state file atomically via tmp-rename. Caller MUST
// hold the write lock.
func (p *KeyPool) saveLocked() error {
	sf := stateFile{
		APIKeys:      p.keys,
		FreeFallback: p.freeFallback,
	}
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	dir := filepath.Dir(p.statePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	tmpPath := p.statePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("write tmp file: %w", err)
	}
	if err := os.Rename(tmpPath, p.statePath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename to %s: %w", p.statePath, err)
	}

	if info, err := os.Stat(p.statePath); err == nil {
		p.lastMtime = info.ModTime()
	}
	return nil
}
