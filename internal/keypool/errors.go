// Package keypool manages a pool of upstream API keys with rotation,
// exhaustion tracking, automatic recovery on reset date, and persistence
// to a JSON state file.
package keypool

import "errors"

// ErrAllExhausted indicates every key in the pool is currently marked
// exhausted. Handlers should fall through to the free-fallback resolver.
var ErrAllExhausted = errors.New("keypool: all keys exhausted")

// ErrKeyNotFound is returned when a token lookup misses.
var ErrKeyNotFound = errors.New("keypool: key not found")
