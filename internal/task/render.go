// SPDX-License-Identifier: Apache-2.0

package task

import (
	"strings"

	"github.com/Bytars/cli-agent-mcp/internal/agent"
)

// RenderLine produces the compact, human-facing form of one stored transcript
// line — the same text agent_get_output and the board show for it.
//
// It is exported so a reader outside the server process (the `logs` command,
// the local web viewer) renders a transcript identically without keeping a
// second copy of the rules. A nil adapter means the agent that produced the line
// is no longer configured here; the raw line is returned rather than nothing.
func RenderLine(a agent.Adapter, line string) string {
	return renderRestored(line, strings.HasPrefix(line, "[stderr] "), a)
}
