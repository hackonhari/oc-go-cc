package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

// clearExhaustedCmd builds the `oc-go-cc clear-exhausted` subcommand.
//
// The command writes the state file atomically (tmp + rename) following the
// same pattern keypool.saveLocked uses, so a running proxy's hot-reload
// (60s mtime poll) picks it up cleanly. No proxy restart is required.
//
// Use cases:
//   - Hand-recovery from a false-positive 30-day exhaustion (the original
//     bug this CLI addresses)
//   - Quickly resurrect a key after the operator has manually verified its
//     balance via the OpenCode dashboard
//   - Reset state for testing
func clearExhaustedCmd() *cobra.Command {
	var clearAll bool
	var stateFile string

	cmd := &cobra.Command{
		Use:   "clear-exhausted [account]",
		Short: "Clear an exhausted key so the pool can use it again",
		Long: `Clear the exhaustion mark from one or all keys in the pool state file.

Use this after manually verifying a key still has balance — for example,
when the proxy classifier wrongly marked it exhausted on a burst-throttle
429 response.

The running proxy will pick up the change via hot-reload within 60s;
no restart needed.

Examples:
  oc-go-cc clear-exhausted ocgo-3-fresh
  oc-go-cc clear-exhausted --all
  oc-go-cc clear-exhausted ocgo-primary --state-file /tmp/test.json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := stateFile
			if path == "" {
				path = expandHome(keyStateFilePath)
			}

			if !clearAll && len(args) == 0 {
				return fmt.Errorf("provide an account name or use --all")
			}
			if clearAll && len(args) > 0 {
				return fmt.Errorf("cannot combine --all with an account name")
			}

			state, err := loadKeyState(path)
			if err != nil {
				return fmt.Errorf("read state file: %w", err)
			}
			if len(state.APIKeys) == 0 {
				return fmt.Errorf("state file is empty: %s", path)
			}

			cleared, err := clearKeys(state, args, clearAll)
			if err != nil {
				return err
			}
			if len(cleared) == 0 {
				return nil // unreachable — clearKeys errors on no-match
			}

			if err := saveKeyState(path, state); err != nil {
				return fmt.Errorf("write state file: %w", err)
			}

			for _, account := range cleared {
				fmt.Printf("Cleared exhaustion for: %s\n", account)
			}
			fmt.Printf("\nState written to: %s\n", path)
			fmt.Println("Running proxy will pick up changes within 60s (no restart needed).")
			return nil
		},
	}

	cmd.Flags().BoolVar(&clearAll, "all", false, "Clear exhaustion for all keys")
	cmd.Flags().StringVar(&stateFile, "state-file", "", "Override path to key-states.json (default ~/.config/oc-go-cc/key-states.json)")
	return cmd
}

// clearKeys mutates the in-memory state: clears ExhaustedAt + ResetDate
// for either a single named account or all keys. Returns the list of
// accounts actually cleared (skips keys that were already active).
//
// Returns an error if a named account doesn't exist in the pool, or if
// --all was used but no keys were exhausted (caller signal for "nothing
// to do").
func clearKeys(state *keyStateFile, args []string, all bool) ([]string, error) {
	cleared := make([]string, 0)

	if all {
		for i := range state.APIKeys {
			if state.APIKeys[i].ExhaustedAt == nil {
				continue
			}
			state.APIKeys[i].ExhaustedAt = nil
			state.APIKeys[i].ResetDate = time.Time{}
			cleared = append(cleared, state.APIKeys[i].Account)
		}
		if len(cleared) == 0 {
			return nil, fmt.Errorf("no exhausted keys to clear")
		}
		return cleared, nil
	}

	target := args[0]
	for i := range state.APIKeys {
		if state.APIKeys[i].Account != target {
			continue
		}
		if state.APIKeys[i].ExhaustedAt == nil {
			fmt.Printf("Key %q is already active — nothing to clear\n", target)
			return cleared, nil
		}
		state.APIKeys[i].ExhaustedAt = nil
		state.APIKeys[i].ResetDate = time.Time{}
		cleared = append(cleared, target)
		return cleared, nil
	}
	return nil, fmt.Errorf("account %q not found in pool", target)
}

// saveKeyState writes the state file atomically: write to tmp, then rename.
// Mirrors keypool.saveLocked. Permissions 0600 to protect token contents.
func saveKeyState(path string, state *keyStateFile) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write tmp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename tmp → %s: %w", path, err)
	}
	return nil
}
