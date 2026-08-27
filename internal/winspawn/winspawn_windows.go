// SPDX-License-Identifier: Apache-2.0

//go:build windows

// Package winspawn centraliza los ajustes de lanzado que este servidor siempre
// quiere en Windows.
//
// POR QUE ES UN PAQUETE PROPIO Y NO VIVE EN internal/agent
// La lógica nació ahí, privada, y por eso `internal/gitx` e `internal/task` no
// podían usarla: durante un tiempo lanzaron `git` y `taskkill` sin ella, y cada
// llamada le parpadeaba una consola al usuario en la cara (issue #18).
//
// No se podía resolver importando `internal/agent` desde `gitx`: `agent` y
// `gitx` son hojas —hoy ninguno importa otro paquete interno— y colgar una de
// la otra invierte la capa. Un paquete hoja que los tres importen no crea
// ciclos y deja el ajuste en un solo lugar.
package winspawn

import (
	"os/exec"
	"syscall"
)

const (
	createNewProcessGroup = 0x00000200
	createNoWindow        = 0x08000000
)

// Harden aplica los ajustes de lanzado de Windows: sin ventana de consola, y un
// grupo de procesos nuevo para poder señalizar al hijo como una unidad.
//
// CREATE_NO_WINDOW importa más de lo que parece. Este servidor lo lanza
// normalmente una aplicación gráfica que no tiene consola propia; sin el flag,
// cada hijo o le parpadea una ventana al usuario, o hereda un handle de consola
// que puede no existir.
//
// LO QUE ESTE FLAG **NO** ARREGLA, MEDIDO EL 26-AGO-2026
// No gobierna a los NIETOS. El agente hijo lanza sus propios procesos de consola
// —`bash.exe`, entre otros— y ésos abren ventana por su cuenta: una sola
// invocación del servidor llegó a producir ~12 consolas por ese camino.
//
// Se probó la alternativa obvia en una reproducción de juguete, mismo padre y
// mismas condiciones:
//
//	A) CREATE_NO_WINDOW           -> 1 conhost + 3 nietos
//	B) CREATE_NEW_CONSOLE + hide  -> 1 conhost + 3 nietos   (IDENTICO)
//
// O sea que cambiar el flag no habría servido de nada. Si alguien vuelve sobre
// esto: la hipótesis de que CREATE_NEW_CONSOLE oculta hace que los nietos
// hereden una consola escondida **ya se midió y es falsa**. El problema de los
// nietos está en el proceso hijo, no acá.
func Harden(cmd *exec.Cmd) *exec.Cmd {
	if cmd == nil {
		return nil
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow | createNewProcessGroup
	return cmd
}
