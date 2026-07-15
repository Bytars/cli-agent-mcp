package agent

import (
	"encoding/json"
	"strings"
)

// claudeStreamEvent mirrors the subset of Claude Code's `--output-format
// stream-json` schema we care about. Unknown fields are ignored.
type claudeStreamEvent struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	Result    string          `json:"result"`
	IsError   bool            `json:"is_error"`
	Message   json.RawMessage `json:"message"`
}

type claudeMessage struct {
	Content []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`     // text blocks
		Name    string          `json:"name"`     // tool_use blocks
		Content json.RawMessage `json:"content"`  // tool_result output: string or []block
		IsError bool            `json:"is_error"` // tool_result blocks
	} `json:"content"`
}

// parseClaudeStreamLine interprets one JSONL line emitted by Claude Code (and,
// since it uses the same format, the built-in mock agent).
func parseClaudeStreamLine(line string) Event {
	ev := Event{Raw: line}
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return ev
	}

	var se claudeStreamEvent
	if err := json.Unmarshal([]byte(trimmed), &se); err != nil {
		return ev
	}

	if se.SessionID != "" {
		ev.SessionID = se.SessionID
	}

	switch se.Type {
	case "result":
		ev.Final = true
		ev.FinalError = se.IsError
		ev.FinalText = se.Result
	case "assistant":
		if len(se.Message) > 0 {
			var m claudeMessage
			if err := json.Unmarshal(se.Message, &m); err == nil {
				var b strings.Builder
				for _, c := range m.Content {
					var part string
					switch c.Type {
					case "text":
						part = c.Text
					case "tool_use":
						if c.Name != "" {
							part = "⚙ using " + c.Name
						}
					}
					appendPart(&b, part)
				}
				ev.Text = b.String()
			}
		}
	case "user":
		// Tool results come back as a user message; surface the last line of each
		// so progress shows what a command actually produced.
		if len(se.Message) > 0 {
			var m claudeMessage
			if err := json.Unmarshal(se.Message, &m); err == nil {
				var b strings.Builder
				for _, c := range m.Content {
					if c.Type != "tool_result" {
						continue
					}
					last := lastNonEmptyLine(rawToText(c.Content))
					if last == "" {
						continue
					}
					prefix := "↳ "
					if c.IsError {
						prefix = "↳ ✗ "
					}
					appendPart(&b, prefix+last)
				}
				ev.Text = b.String()
			}
		}
	}
	return ev
}

func appendPart(b *strings.Builder, part string) {
	if part == "" {
		return
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString(part)
}

// rawToText flattens a tool_result "content" value, which may be a plain string
// or an array of content blocks, into text.
func rawToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, bl := range blocks {
			if bl.Text != "" {
				appendPart(&b, bl.Text)
			}
		}
		return b.String()
	}
	return ""
}

// lastNonEmptyLine returns the last non-blank line of s, trimmed.
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}
