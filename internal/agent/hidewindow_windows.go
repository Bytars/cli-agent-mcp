// SPDX-License-Identifier: Apache-2.0

//go:build windows

package agent

import (
	"os/exec"

	"github.com/Bytars/cli-agent-mcp/internal/winspawn"
)

// systemRoot returns the Windows directory, defaulting to the conventional path
// when the environment variable is missing.
func systemRoot() string {
	if r := getenv("SystemRoot"); r != "" {
		return r
	}
	return `C:\Windows`
}

// hardenSpawn delega en internal/winspawn.
//
// La lógica vivía acá y era privada, así que `internal/gitx` e `internal/task`
// no podían usarla y lanzaban `git` y `taskkill` sin ella (issue #18). Se movió
// a un paquete hoja para que haya un solo lugar donde se decide cómo se lanza
// un proceso en Windows. El envoltorio se conserva para no tocar los cinco
// sitios de llamada de este paquete.
func hardenSpawn(cmd *exec.Cmd) *exec.Cmd { return winspawn.Harden(cmd) }
