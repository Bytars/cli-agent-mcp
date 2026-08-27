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
// the pairing token in its env block. It returns the file it wrote and whether
// that file had to be CREATED.
//
// # Why "created" is worth returning
//
// Writing the token is not the same as the client reading it, and this cannot
// tell the difference — it edits a path, and whether the client actually loads
// that path is beyond anything this process can see. The one signal available
// is whether the file was already there: **a config that did not exist is a
// config the client was not using**, and that is the case where pairing looks
// like it worked and silently did not (issue #25).
//
// Measured on a real install: this created `claude_desktop_config.json` from
// nothing, wrote a correct entry into it, and reported success — while the
// server that actually runs is registered as a connector held outside the
// filesystem entirely. There was no local file to edit, and the user found out
// by losing their MCP on the next restart.
//
// The file is decoded into plain maps and re-encoded, never into a struct: a
// client config holds entries for other servers and settings this build has
// never heard of, and round-tripping through a typed shape would quietly drop
// every one of them.
func InstallToken(path, exe, secret string) (written string, created bool, err error) {
	if path == "" {
		p, err := ConfigPath()
		if err != nil {
			return "", false, err
		}
		path = p
	}

	root := map[string]any{}
	buf, readErr := os.ReadFile(path)
	switch {
	case readErr == nil:
		if len(trimSpaceBytes(buf)) > 0 {
			if err := json.Unmarshal(buf, &root); err != nil {
				// Overwriting a config we failed to parse would destroy the
				// user's other servers. Refuse and say which file to look at.
				return "", false, fmt.Errorf("%s is not valid JSON (%w); fix or move it, then run pairing again", path, err)
			}
		}
	case errors.Is(readErr, os.ErrNotExist):
		created = true
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", false, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
		}
	default:
		return "", false, fmt.Errorf("read %s: %w", path, readErr)
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
		return "", false, err
	}
	out = append(out, '\n')

	// Keep a copy of whatever was there. This edits a file the user did not
	// write and may have spent real effort on.
	if len(buf) > 0 {
		backup := fmt.Sprintf("%s.bak-%s", path, time.Now().UTC().Format("20060102T150405Z"))
		if err := os.WriteFile(backup, buf, 0o600); err != nil {
			return "", false, fmt.Errorf("back up %s: %w", path, err)
		}
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return "", false, fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", false, fmt.Errorf("replace %s: %w", path, err)
	}
	return path, created, nil
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
