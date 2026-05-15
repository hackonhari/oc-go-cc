package keypool

import (
	"context"
	"encoding/json"
	"os"
	"time"
)

// DefaultHotReloadInterval is the default poll cadence for state file changes.
const DefaultHotReloadInterval = 60 * time.Second

// StartHotReload runs a background goroutine that polls the state file's
// mtime every interval and reloads if changed. Returns immediately; the
// goroutine exits when ctx is canceled.
//
// Hot reload preserves in-memory LastUsed timestamps for keys still
// present after reload — only key additions/removals and exhaustion
// state are taken from disk.
//
// If interval <= 0, DefaultHotReloadInterval is used.
func (p *KeyPool) StartHotReload(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultHotReloadInterval
	}
	go p.hotReloadLoop(ctx, interval)
}

func (p *KeyPool) hotReloadLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.checkAndReload()
		}
	}
}

// checkAndReload reads the state file mtime; if newer than the last
// known mtime, reloads keys from disk while preserving in-memory LastUsed.
// Exported for test convenience.
func (p *KeyPool) checkAndReload() {
	info, err := os.Stat(p.statePath)
	if err != nil {
		return
	}

	p.mu.RLock()
	lastMtime := p.lastMtime
	p.mu.RUnlock()

	if !info.ModTime().After(lastMtime) {
		return
	}

	data, err := os.ReadFile(p.statePath)
	if err != nil {
		p.logger.Warn("hot reload: read failed", "err", err)
		return
	}
	var sf stateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		p.logger.Warn("hot reload: parse failed (skipping reload)", "err", err)
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	inMemLastUsed := map[string]*time.Time{}
	for _, k := range p.keys {
		if k.LastUsed != nil {
			inMemLastUsed[k.Token] = k.LastUsed
		}
	}
	for _, k := range sf.APIKeys {
		if lu, ok := inMemLastUsed[k.Token]; ok {
			k.LastUsed = lu
		}
	}

	p.keys = sf.APIKeys
	p.freeFallback = sf.FreeFallback
	p.lastMtime = info.ModTime()
	p.logger.Info("hot reload: state file refreshed", "keys", len(p.keys))
}
