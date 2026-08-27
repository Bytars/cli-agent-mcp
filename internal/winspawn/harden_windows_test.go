// SPDX-License-Identifier: Apache-2.0

//go:build windows

package winspawn

import (
	"os/exec"
	"syscall"
	"testing"
)

// TestHardenAplicaLosFlags afirma que Harden HACE algo, no sólo que se lo llama.
//
// POR QUÉ EXISTE, Y QUÉ AGUJERO TAPA
// El barrido de sweep_test.go comprueba que todo lanzado pase por Harden. Eso
// deja sin cuidar la mitad que importa: **la función llamada**. Lo señaló una
// sesión de verificación independiente y lo probó destripándola —
//
//	func Harden(cmd *exec.Cmd) *exec.Cmd { return cmd }
//
// — con lo que el defecto del issue #18 vuelve entero, cero flags y consola en
// todos lados, y **la suite quedaba verde**. Una guarda que cuida los sitios de
// llamada y no lo llamado no cuida nada.
//
// Se afirma sobre los bits concretos y no sobre "es distinto de cero", porque
// un OR con un flag equivocado también sería distinto de cero.
func TestHardenAplicaLosFlags(t *testing.T) {
	t.Run("sobre un comando sin SysProcAttr", func(t *testing.T) {
		cmd := Harden(exec.Command("cmd", "/c", "ver"))

		if cmd.SysProcAttr == nil {
			t.Fatal("Harden no creó SysProcAttr, así que no aplicó ningún flag")
		}
		if !cmd.SysProcAttr.HideWindow {
			t.Error("HideWindow quedó en false")
		}
		if got := cmd.SysProcAttr.CreationFlags & createNoWindow; got == 0 {
			t.Errorf("falta CREATE_NO_WINDOW (0x%08x) en CreationFlags=0x%08x.\n"+
				"Es el flag por el que existe este paquete: sin él, cada hijo le abre una\n"+
				"consola al usuario, que es el defecto del issue #18.",
				createNoWindow, cmd.SysProcAttr.CreationFlags)
		}
		if got := cmd.SysProcAttr.CreationFlags & createNewProcessGroup; got == 0 {
			t.Errorf("falta CREATE_NEW_PROCESS_GROUP (0x%08x) en CreationFlags=0x%08x.\n"+
				"Sin él no se puede señalizar al hijo como una unidad.",
				createNewProcessGroup, cmd.SysProcAttr.CreationFlags)
		}
	})

	t.Run("conserva los flags que ya venían", func(t *testing.T) {
		// El OR importa: si Harden ASIGNARA en vez de OR-ear, se llevaría por
		// delante lo que el sitio de llamada hubiera puesto. Se usa un flag real
		// y ajeno a los dos nuestros — DETACHED_PROCESS — para que la afirmación
		// no pueda pasar por accidente.
		const detachedProcess = 0x00000008
		cmd := exec.Command("cmd", "/c", "ver")
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess}

		Harden(cmd)

		if got := cmd.SysProcAttr.CreationFlags & detachedProcess; got == 0 {
			t.Error("Harden pisó los CreationFlags que ya estaban en vez de agregarse a ellos")
		}
		if got := cmd.SysProcAttr.CreationFlags & createNoWindow; got == 0 {
			t.Error("Harden no agregó CREATE_NO_WINDOW sobre los flags existentes")
		}
	})

	t.Run("un cmd nil no explota", func(t *testing.T) {
		// La guarda de nil es lo único que Harden agregó sobre el hardenSpawn
		// original, así que conviene que algo la sostenga.
		if got := Harden(nil); got != nil {
			t.Errorf("Harden(nil) devolvió %v, quería nil", got)
		}
	})
}
