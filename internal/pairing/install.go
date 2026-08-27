// SPDX-License-Identifier: Apache-2.0

package pairing

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// ServerKey is the name this server is registered under in a client's
// mcpServers map.
const ServerKey = "cli-agent-mcp"

// ConfigPath is where Claude Desktop keeps its MCP configuration.
//
// Cowork and other hosts read their own files; --config exists for those, and
// for anyone whose install does not sit in the usual place.
func ConfigPath() (string, error) {
	switch runtime.GOOS {
	case "windows":
		if dir := os.Getenv("APPDATA"); dir != "" {
			return filepath.Join(dir, "Claude", "claude_desktop_config.json"), nil
		}
		return "", errors.New("APPDATA is not set, so the Claude Desktop config cannot be located")
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json"), nil
	default:
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".config", "Claude", "claude_desktop_config.json"), nil
	}
}

// InstallToken adds or updates this server's entry in a client config, setting
// the pairing token in its env block. It returns the file it wrote.
//
// The file is decoded into plain maps and re-encoded, never into a struct: a
// client config holds entries for other servers and settings this build has
// never heard of, and round-tripping through a typed shape would quietly drop
// every one of them.
func InstallToken(path, exe, secret string) (string, error) {
	if path == "" {
		p, err := ConfigPath()
		if err != nil {
			return "", err
		}
		path = p
	}

	root := map[string]any{}
	buf, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(trimSpaceBytes(buf)) > 0 {
			if err := json.Unmarshal(buf, &root); err != nil {
				// Overwriting a config we failed to parse would destroy the
				// user's other servers. Refuse and say which file to look at.
				return "", fmt.Errorf("%s is not valid JSON (%w); fix or move it, then run pairing again", path, err)
			}
		}
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", fmt.Errorf("create %s: %w", filepath.Dir(path), err)
		}
	default:
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	servers, _ := root["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
		root["mcpServers"] = servers
	}
	entry, _ := servers[ServerKey].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
		servers[ServerKey] = entry
	}
	// Only fill in the command when the entry is new. An existing install may
	// point at a launcher script or a pinned path on purpose, and pairing is
	// not the place to second-guess that.
	if _, ok := entry["command"]; !ok {
		entry["command"] = exe
	}
	env, _ := entry["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
		entry["env"] = env
	}
	env[EnvVar] = secret

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return "", err
	}
	out = append(out, '\n')

	// Keep a copy of whatever was there. This edits a file the user did not
	// write and may have spent real effort on.
	if len(buf) > 0 {
		backup := fmt.Sprintf("%s.bak-%s", path, time.Now().UTC().Format("20060102T150405Z"))
		if err := os.WriteFile(backup, buf, 0o600); err != nil {
			return "", fmt.Errorf("back up %s: %w", path, err)
		}
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("replace %s: %w", path, err)
	}
	return path, nil
}

// trimSpaceBytes reports the content of buf ignoring surrounding whitespace, so
// an empty-but-present config is treated as absent rather than as broken JSON.
func trimSpaceBytes(buf []byte) []byte {
	start, end := 0, len(buf)
	for start < end && isSpace(buf[start]) {
		start++
	}
	for end > start && isSpace(buf[end-1]) {
		end--
	}
	return buf[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
