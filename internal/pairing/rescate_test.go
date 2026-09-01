// SPDX-License-Identifier: Apache-2.0

package pairing

import (
	"strings"
	"testing"
)

// El directorio virtualizado real de la máquina donde ocurrió el incidente.
const dirVirtualizado = `C:\Users\anher\AppData\Local\Packages\Claude_pzs8sxrjxfjjc\LocalCache\Roaming\cli-agent-mcp`

// Toda instrucción de rescate tiene que traer la ruta puesta.
//
// EL DÍA QUE ESTO COSTÓ
// Claude Desktop es un paquete MSIX y MSIX virtualiza %APPDATA%. El servidor
// leía el directorio de arriba mientras `pair` desde una terminal escribía en
// %APPDATA%\cli-agent-mcp. Nunca fue el mismo archivo. Cuatro rescates
// seguidos no cambiaron nada, y ninguno de los dos lados podía notarlo: el
// comando decía «unpaired», el servidor seguía rechazando, y los dos tenían
// razón sobre archivos distintos.
//
// Que el mensaje diga `trust --add` a secas asume que el lector puede resolver
// la ruta. No puede: ve un %APPDATA% distinto del que ve el servidor.
func TestElRescateTraeLaRutaPuesta(t *testing.T) {
	casos := map[string]Result{
		"lanzador ajeno": {
			Status:   ForeignLauncher,
			Detail:   "algo",
			Launcher: `C:\Temp\raro.exe`,
			StateDir: dirVirtualizado,
		},
		"registro vacío": {
			Status:   EmptyRecord,
			Detail:   "algo",
			StateDir: dirVirtualizado,
		},
		"sin token": {
			Status:   NoToken,
			Detail:   "algo",
			Launcher: `C:\Tools\cliente.exe`,
			StateDir: dirVirtualizado,
		},
	}

	for nombre, r := range casos {
		t.Run(nombre, func(t *testing.T) {
			stderr, alModelo := Explain(r)
			for canal, msg := range map[string]string{"el log": stderr, "el mensaje al modelo": alModelo} {
				if msg == "" {
					t.Fatalf("%s vino vacío", canal)
				}
				if !strings.Contains(msg, dirVirtualizado) {
					t.Errorf("%s no nombra el directorio del que salió el veredicto.\n"+
						"Sin --state-dir, el comando resuelve OTRO directorio bajo MSIX y parece funcionar "+
						"sin cambiar nada — que es lo que pasó cuatro veces seguidas el 1-sep.\n\n%s", canal, msg)
				}
				if !strings.Contains(msg, "--state-dir") {
					t.Errorf("%s da un comando sin --state-dir:\n\n%s", canal, msg)
				}
			}
		})
	}
}

// EL CONTROL. Si `cmd` pegara la ruta siempre, esto pasaría igual sin probar
// nada: sin StateDir el comando tiene que salir limpio, no con una bandera
// vacía que rompa al pegarla.
func TestSinDirectorioElComandoSaleLimpio(t *testing.T) {
	r := Result{Status: ForeignLauncher, Detail: "algo", Launcher: `C:\Temp\raro.exe`}
	stderr, alModelo := Explain(r)
	for canal, msg := range map[string]string{"el log": stderr, "el mensaje al modelo": alModelo} {
		if strings.Contains(msg, "--state-dir") {
			t.Errorf("%s ofrece --state-dir sin tener una ruta que poner; pegar eso da un error de sintaxis:\n\n%s", canal, msg)
		}
		if !strings.Contains(msg, "cli-agent-mcp trust") {
			t.Errorf("%s dejó de nombrar el comando de rescate:\n\n%s", canal, msg)
		}
	}
}

// La línea de arranque siempre dice qué registro está leyendo. Es lo único que
// permite comparar, desde afuera, el lado del espejo en que está cada uno.
func TestElArranqueSiempreDiceQueRegistroLee(t *testing.T) {
	dir := t.TempDir()
	for nombre, r := range map[string]Result{
		"sin parear": {Status: Unpaired},
		"autorizado": {Status: TrustedLauncher, Launcher: `C:\Tools\x.exe`},
		"rechazando": {Status: ForeignLauncher, Detail: "algo"},
	} {
		linea := StartupLine(dir, r)
		if !strings.Contains(linea, Path(dir)) {
			t.Errorf("%s: la línea de arranque no nombra el registro:\n%s", nombre, linea)
		}
	}
}

// Y Verify estampa el directorio en TODO veredicto, incluidos los que salen
// por caminos que nadie tocó en años.
func TestTodoVeredictoSabeDeDondeSalio(t *testing.T) {
	dir := t.TempDir()

	// sin registro
	r, _ := Verify(dir, "", `C:\Tools\cliente.exe`)
	if r.StateDir != dir {
		t.Errorf("veredicto sin registro: StateDir = %q, quería %q", r.StateDir, dir)
	}

	// con token, rechazando
	secreto, err := Mint(dir, "x", false)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	confirm(t, dir, secreto, `C:\Tools\cliente.exe`)
	r2, _ := Verify(dir, "", `C:\Tools\cliente.exe`)
	if r2.StateDir != dir {
		t.Errorf("veredicto de rechazo: StateDir = %q, quería %q", r2.StateDir, dir)
	}
}
