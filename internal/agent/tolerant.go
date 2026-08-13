// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"encoding/json"
	"strings"
)

// sessionKeys are the key spellings different agents use for a resumable
// conversation id.
var sessionKeys = []string{"session_id", "sessionId", "chatId", "chat_id", "thread_id", "threadId"}

// finalTypes are the "type" values that mark a terminal result event.
var finalTypes = map[string]bool{"result": true, "final": true, "done": true, "complete": true}

// parseTolerantLine interprets one output line from an agent whose exact JSON
// schema is not pinned by this project. It recognizes the common session and
// result key spellings, and always preserves the raw line so no output is lost
// if the schema drifts. Task completion is backstopped by the process exit code
// in the task manager, so an unrecognized schema still works.
//
// plainAsText controls what happens to non-JSON lines: agents that print plain
// text (see CustomAdapter) want each line surfaced as progress; agents that emit
// structured JSON do not.
func parseTolerantLine(line string, plainAsText bool) Event {
	ev := Event{Raw: line}
	trimmed := strings.TrimSpace(line)

	plain := func() Event {
		if plainAsText {
			ev.Text = trimmed
		}
		return ev
	}

	if !strings.HasPrefix(trimmed, "{") {
		return plain()
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
		return plain()
	}

	for _, k := range sessionKeys {
		if s, ok := m[k].(string); ok && s != "" {
			ev.SessionID = s
			break
		}
	}

	typ, _ := m["type"].(string)
	switch {
	case finalTypes[typ]:
		ev.Final = true
		if b, ok := m["is_error"].(bool); ok {
			ev.FinalError = b
		}
		if s, ok := m["result"].(string); ok {
			ev.FinalText = s
		} else if s, ok := m["text"].(string); ok {
			ev.FinalText = s
		}
	case typ == "assistant" || typ == "message" || typ == "text":
		if s, ok := m["text"].(string); ok {
			ev.Text = s
		}
	default:
		// Unrecognized JSON shape: for plain-text agents still show something.
		if plainAsText {
			if s, ok := m["text"].(string); ok {
				ev.Text = s
			}
		}
	}
	return ev
}
