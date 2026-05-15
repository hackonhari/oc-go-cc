package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	// keyStateFilePath mirrors server.go's stateFilePath. Kept as a separate
	// constant here to avoid importing the server package (which has its
	// own initialization side-effects).
	keyStateFilePath = "~/.config/oc-go-cc/key-states.json"
	rotationLogDir   = "~/.cache/oc-go-cc"
)

// keysStatusCmd builds the `oc-go-cc keys-status` subcommand.
func keysStatusCmd() *cobra.Command {
	var tailMode bool
	var tailLines int
	var jsonMode bool

	cmd := &cobra.Command{
		Use:   "keys-status",
		Short: "Show key pool state and rotation activity",
		Long: `Display the current state of the API key pool: which keys are
active, which are exhausted, when they reset, when they were last used.

Modes:
  (default)           Pretty-printed table
  --tail [N]          Tail the last N (default 50) lines of today's rotation log
  --json              Emit structured JSON (token-redacted) for scripting`,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := expandHome(keyStateFilePath)
			state, err := loadKeyState(path)
			if err != nil {
				return fmt.Errorf("read state file: %w", err)
			}

			switch {
			case tailMode:
				return printRotationTail(os.Stdout, tailLines)
			case jsonMode:
				return printJSON(os.Stdout, state)
			default:
				return printTable(os.Stdout, state)
			}
		},
	}

	cmd.Flags().BoolVar(&tailMode, "tail", false, "Tail the rotation event log instead of showing the state table")
	cmd.Flags().IntVar(&tailLines, "lines", 50, "Number of trailing log lines to show with --tail (default 50)")
	cmd.Flags().BoolVar(&jsonMode, "json", false, "Output state as JSON (tokens redacted)")
	return cmd
}

// keyStateRow mirrors the keypool.Key shape — duplicated here to avoid
// pulling the full keypool runtime into the CLI binary path. The CLI
// runs as a separate process and only needs JSON read access to the
// state file.
type keyStateRow struct {
	Token               string     `json:"token"`
	Account             string     `json:"account"`
	ExhaustedAt         *time.Time `json:"exhaustedAt,omitempty"`
	ResetDate           time.Time  `json:"resetDate"`
	LastUsed            *time.Time `json:"lastUsed,omitempty"`
	WeeklyUsagePercent  *int       `json:"weeklyUsagePercent,omitempty"`
	MonthlyUsagePercent *int       `json:"monthlyUsagePercent,omitempty"`
}

type freeFallbackBlock struct {
	BaseURL string   `json:"base_url"`
	Models  []string `json:"models"`
}

type keyStateFile struct {
	APIKeys      []keyStateRow     `json:"api_keys"`
	FreeFallback freeFallbackBlock `json:"free_fallback"`
}

// loadKeyState parses ~/.config/oc-go-cc/key-states.json. Missing file
// is treated as empty state (returns zero-value struct, no error).
func loadKeyState(path string) (*keyStateFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &keyStateFile{}, nil
		}
		return nil, err
	}
	var sf keyStateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, err
	}
	return &sf, nil
}

// redactToken returns a viewable form of the token: first 6 + *** + last 4.
// Short tokens (≤14 chars) are fully masked to avoid leaking ratios.
func redactToken(t string) string {
	if len(t) <= 14 {
		return "***"
	}
	return t[:6] + "***" + t[len(t)-4:]
}

// statusLabel returns "ACTIVE" or "EXHAUSTED" for the row.
func statusLabel(r keyStateRow) string {
	if r.ExhaustedAt == nil {
		return "ACTIVE"
	}
	return "EXHAUSTED"
}

// printTable writes a human-readable table to w. Sort: ACTIVE rows first,
// then EXHAUSTED by reset date ascending.
func printTable(w io.Writer, sf *keyStateFile) error {
	if len(sf.APIKeys) == 0 {
		fmt.Fprintln(w, "(empty — no keys in pool. Run the proxy once or edit ~/.config/oc-go-cc/config.json with api_keys[])")
		return nil
	}

	rows := append([]keyStateRow(nil), sf.APIKeys...)
	sort.SliceStable(rows, func(i, j int) bool {
		ai := rows[i].ExhaustedAt == nil
		aj := rows[j].ExhaustedAt == nil
		if ai != aj {
			return ai // ACTIVE first
		}
		if !ai {
			return rows[i].ResetDate.Before(rows[j].ResetDate)
		}
		return rows[i].Account < rows[j].Account
	})

	// Compute column widths.
	const (
		accountW = 16
		statusW  = 10
		resetW   = 12
		lastUseW = 19
		exhW     = 12
	)
	header := fmt.Sprintf("%-*s  %-*s  %-*s  %-*s  %-*s  %s",
		accountW, "Account",
		statusW, "Status",
		resetW, "Reset Date",
		lastUseW, "Last Used",
		exhW, "Exhausted At",
		"Token")
	fmt.Fprintln(w, header)
	fmt.Fprintln(w, strings.Repeat("-", len(header)))

	for _, r := range rows {
		lastUsed := "—"
		if r.LastUsed != nil {
			lastUsed = r.LastUsed.Local().Format("2006-01-02 15:04")
		}
		exhausted := "—"
		if r.ExhaustedAt != nil {
			exhausted = r.ExhaustedAt.Local().Format("2006-01-02")
		}
		reset := "—"
		if !r.ResetDate.IsZero() {
			reset = r.ResetDate.Format("2006-01-02")
		}
		fmt.Fprintf(w, "%-*s  %-*s  %-*s  %-*s  %-*s  %s\n",
			accountW, truncate(r.Account, accountW),
			statusW, statusLabel(r),
			resetW, reset,
			lastUseW, lastUsed,
			exhW, exhausted,
			redactToken(r.Token),
		)
	}

	// Footer: free fallback summary.
	fmt.Fprintln(w)
	if sf.FreeFallback.BaseURL != "" {
		fmt.Fprintf(w, "Free fallback: %s\n", sf.FreeFallback.BaseURL)
		fmt.Fprintf(w, "  models: %s\n", strings.Join(sf.FreeFallback.Models, ", "))
	} else {
		fmt.Fprintln(w, "Free fallback: NOT configured")
	}
	return nil
}

// printJSON writes the entire state with tokens redacted.
func printJSON(w io.Writer, sf *keyStateFile) error {
	redacted := *sf
	redacted.APIKeys = make([]keyStateRow, len(sf.APIKeys))
	for i, k := range sf.APIKeys {
		k.Token = redactToken(k.Token)
		redacted.APIKeys[i] = k
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(redacted)
}

// printRotationTail reads today's rotation log and prints the last N lines.
// Lines are JSONL — we parse each, then pretty-print with timestamp + event + key fields.
func printRotationTail(w io.Writer, n int) error {
	if n <= 0 {
		n = 50
	}
	logPath := filepath.Join(expandHome(rotationLogDir),
		"rotation-"+time.Now().UTC().Format("20060102")+".log")

	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(w, "(no events yet today — %s does not exist)\n", logPath)
			return nil
		}
		return err
	}
	defer f.Close()

	// Read whole file; cheap because daily logs are bounded.
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return err
	}

	start := 0
	if len(lines) > n {
		start = len(lines) - n
	}
	for _, line := range lines[start:] {
		var evt map[string]any
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			fmt.Fprintln(w, line) // fallback: raw
			continue
		}
		ts := ""
		if t, ok := evt["ts"].(string); ok {
			if parsed, err := time.Parse(time.RFC3339Nano, t); err == nil {
				ts = parsed.Local().Format("15:04:05")
			}
		}
		typeStr, _ := evt["event"].(string)
		fields := make([]string, 0, 4)
		for _, k := range []string{"account", "model", "reason", "mode", "retry_after"} {
			if v, ok := evt[k]; ok && v != nil && v != "" {
				fields = append(fields, fmt.Sprintf("%s=%v", k, v))
			}
		}
		fmt.Fprintf(w, "[%s] %-30s  %s\n", ts, typeStr, strings.Join(fields, " "))
	}
	return nil
}

// truncate clips s to width, adding "…" if cut.
func truncate(s string, width int) string {
	if len(s) <= width {
		return s
	}
	if width <= 1 {
		return "…"
	}
	return s[:width-1] + "…"
}

// expandHome replaces a leading ~/ with the user's home directory.
func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return filepath.Join(home, p[2:])
	}
	return p
}
