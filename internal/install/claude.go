// Package install wires parley into the Claude Code session lifecycle.
//
// EnsureClaudeHook idempotently writes a SessionStart hook into the Claude
// Code settings file so the agent sees a parley inbox at session start.
// Repeated calls with nothing to change are silent no-ops; repeated calls
// after the binary moves repair the path (self-heal).
package install

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// EnsureClaudeHook installs or repairs the SessionStart hook. The Claude
// config directory is taken from $CLAUDE_CONFIG_DIR, falling back to
// ~/.claude.
//
// Errors are returned but the typical call site (cmd/parley/main.go) ignores
// them — failing to self-install should not break the normal CLI flow.
func EnsureClaudeHook() error {
	dir, err := claudeConfigDir()
	if err != nil {
		return err
	}
	settingsPath := filepath.Join(dir, "settings.json")

	cmd, err := preferredCommand()
	if err != nil {
		return err
	}

	raw, err := os.ReadFile(settingsPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	var settings map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &settings); err != nil {
			return err
		}
	}
	if settings == nil {
		settings = map[string]any{}
	}

	if !upsertSessionStart(settings, cmd) {
		return nil
	}

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := settingsPath + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, settingsPath)
}

func claudeConfigDir() (string, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

// preferredCommand returns the SessionStart hook command. It is intentionally
// just the parley binary with no env prefix: identity comes from the shell
// env that launched Claude Code (PARLEY_HOME is inherited), so two terminal
// sessions on the same CLAUDE_CONFIG_DIR can each run as a different agent.
// Embedding PARLEY_HOME here would lock the hook to whichever shell last
// invoked parley.
func preferredCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if path, lookErr := exec.LookPath("parley"); lookErr == nil {
		if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved == exe {
			return "parley", nil
		}
	}
	return exe, nil
}

// upsertSessionStart mutates settings so there is exactly one parley
// SessionStart hook with the given command. Returns true when anything
// changed (used by the caller to decide whether to write the file back).
func upsertSessionStart(settings map[string]any, cmd string) bool {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		settings["hooks"] = hooks
	}

	starts, _ := hooks["SessionStart"].([]any)

	changed := false
	found := false
	for _, group := range starts {
		gm, _ := group.(map[string]any)
		if gm == nil {
			continue
		}
		gh, _ := gm["hooks"].([]any)
		for _, h := range gh {
			hm, _ := h.(map[string]any)
			if hm == nil {
				continue
			}
			existing, _ := hm["command"].(string)
			if !isParleyHook(existing) {
				continue
			}
			found = true
			if existing != cmd {
				hm["command"] = cmd
				changed = true
			}
		}
	}

	if !found {
		starts = append(starts, map[string]any{
			"matcher": "",
			"hooks": []any{
				map[string]any{
					"type":    "command",
					"command": cmd,
					"timeout": float64(10),
				},
			},
		})
		hooks["SessionStart"] = starts
		changed = true
	}
	return changed
}

// isParleyHook reports whether cmd appears to be a parley SessionStart hook.
// It tolerates env-var prefixes (e.g. `PARLEY_HOME=/p parley`) and absolute
// paths to the binary.
func isParleyHook(cmd string) bool {
	for _, tok := range strings.Fields(cmd) {
		if strings.Contains(tok, "=") && !strings.HasPrefix(tok, "/") {
			continue
		}
		// Strip surrounding shell quotes that POSIX quoting might leave.
		tok = strings.Trim(tok, "'\"")
		if tok == "parley" || filepath.Base(tok) == "parley" {
			return true
		}
	}
	return false
}
