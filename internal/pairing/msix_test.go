// SPDX-License-Identifier: Apache-2.0

package pairing

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// El nombre del paquete real de Claude Desktop en la máquina del incidente.
const paqueteClaude = "Claude_pzs8sxrjxfjjc"

// unRegistroVirtualizado arma, bajo un %LOCALAPPDATA% temporal, el mismo árbol
// que MSIX le da a un paquete: Packages\<paquete>\LocalCache\Roaming\cli-agent-mcp.
// Devuelve el directorio del registro.
//
// Nada de esto toca el %LOCALAPPDATA% real: la variable se reemplaza con
// t.Setenv y el árbol vive en un temporal. Importa decirlo — la máquina donde
// se escribió esto tiene una instalación viva, y un test que escriba en
// Packages\Claude_pzs8sxrjxfjjc rompe el servidor de quien lo corre.
func unRegistroVirtualizado(t *testing.T) string {
	t.Helper()
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)

	dir := filepath.Join(local, "Packages", paqueteClaude, "LocalCache", "Roaming", stateDirName)
	f := &File{Version: fileVersion}
	f.TrustedLaunchers = []Launcher{{Exe: `C:\Program Files\WindowsApps\Claude_1.0_x64__pzs8sxrjxfjjc\claude.exe`, Recorded: time.Now().UTC(), FirstUse: true}}
	if err := Save(dir, f); err != nil {
		t.Fatalf("armando el registro virtualizado: %v", err)
	}
	return dir
}

// capturaStdout corre fn con os.Stdout redirigido y devuelve lo que imprimió.
// Los comandos le hablan a una persona por stdout, así que la afirmación tiene
// que ser sobre eso y no sobre una función interna: lo que se rompió el 1-sep
// fue lo que el usuario LEYÓ.
func capturaStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	original := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = original }()

	leido := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		leido <- string(b)
	}()
	fn()
	w.Close()
	salida := <-leido
	r.Close()
	return salida
}

// tal cual resuelve el directorio sin tocarlo: los temporales de los tests ya
// son absolutos, y el resolvedor real ya tiene su propio test (statedir_test.go).
func talCual(d string) string { return d }

// EL AVISO. `pair --status` y `trust --status` tienen que decir, cuando el
// servidor lee otro archivo, CUÁLES son los dos y sobre cuál están hablando.
//
// EL DÍA QUE ESTO COSTÓ
// Claude Desktop es MSIX y MSIX virtualiza %APPDATA%. El servidor leía
// %LOCALAPPDATA%\Packages\Claude_pzs8sxrjxfjjc\LocalCache\Roaming\cli-agent-mcp
// mientras el comando desde la terminal leía %APPDATA%\cli-agent-mcp. Nunca fue
// el mismo archivo: `pair --status` decía NOT PAIRED con el servidor exigiendo
// token, y cuatro rescates seguidos no cambiaron nada. Ningún lado mentía;
// ningún lado nombraba el archivo del que hablaba.
//
// Esto no arregla la divergencia — los dos procesos ven valores distintos para
// la misma variable — sino que impide que engañe.
func TestElStatusAvisaQueElServidorLeeOtroArchivo(t *testing.T) {
	comandos := map[string]func([]string, func(string) string) int{
		"pair --status":  Run,
		"trust --status": RunTrust,
	}
	for nombre, correr := range comandos {
		t.Run(nombre, func(t *testing.T) {
			virtual := unRegistroVirtualizado(t)
			deLaTerminal := t.TempDir() // el %APPDATA%\cli-agent-mcp del incidente
			t.Setenv("CLI_AGENT_MCP_STATE_DIR", "")

			salida := capturaStdout(t, func() {
				if code := correr([]string{"--status", "--state-dir", deLaTerminal}, talCual); code != 0 {
					t.Errorf("%s devolvió %d, quería 0: el aviso es un aviso, no un rechazo", nombre, code)
				}
			})

			// Las dos rutas, completas. Nombrar una sola deja al lector
			// deduciendo la otra, y no puede: ve un %APPDATA% distinto del que
			// ve el servidor.
			if !strings.Contains(salida, Path(deLaTerminal)) {
				t.Errorf("%s no dice sobre qué archivo está actuando.\nQuería: %s\n\n%s", nombre, Path(deLaTerminal), salida)
			}
			if !strings.Contains(salida, Path(virtual)) {
				t.Errorf("%s no nombra el registro que lee el servidor del paquete.\nQuería: %s\n\n%s"+
					"\nSin esa ruta, esto es exactamente el status que hizo perder un día: correcto sobre un archivo que no es el que importa.",
					nombre, Path(virtual), salida)
			}
			// El comando, pegable tal cual. La primera versión de esto usaba %q
			// para citar la ruta y salía C:\\Users\\… — la única instrucción
			// que el aviso existe para dar, mal, en el único lugar donde se usa.
			quiero := `--state-dir "` + virtual + `"`
			if !strings.Contains(salida, quiero) {
				t.Errorf("%s no da el comando pegable.\nQuería: %s\n\n%s", nombre, quiero, salida)
			}
			// Y que diga que lo de acá no llega allá. Dos rutas sin esa frase
			// se leen como información de más, no como una advertencia.
			if !strings.Contains(salida, "WARNING") {
				t.Errorf("%s no marca el desacuerdo como advertencia:\n\n%s", nombre, salida)
			}
		})
	}
}

// SU CONTROL. Sin registro virtualizado no puede haber aviso: si lo hubiera,
// el test de arriba estaría pasando por imprimir siempre lo mismo y no probaría
// nada.
func TestSinRegistroVirtualizadoNoHayAviso(t *testing.T) {
	comandos := map[string]func([]string, func(string) string) int{
		"pair --status":  Run,
		"trust --status": RunTrust,
	}
	for nombre, correr := range comandos {
		t.Run(nombre, func(t *testing.T) {
			// Un %LOCALAPPDATA% temporal y vacío: ni el árbol de paquetes existe.
			t.Setenv("LOCALAPPDATA", t.TempDir())
			t.Setenv("CLI_AGENT_MCP_STATE_DIR", "")
			deLaTerminal := t.TempDir()

			salida := capturaStdout(t, func() {
				if code := correr([]string{"--status", "--state-dir", deLaTerminal}, talCual); code != 0 {
					t.Errorf("%s devolvió %d, quería 0", nombre, code)
				}
			})
			if strings.Contains(salida, "WARNING") {
				t.Errorf("%s avisa de un desacuerdo que no existe:\n\n%s", nombre, salida)
			}
			// Y sigue contestando lo suyo: un control que pase porque el
			// comando no imprimió nada no controlaría nada.
			if !strings.Contains(salida, Path(deLaTerminal)) {
				t.Errorf("%s dejó de nombrar su propio registro:\n\n%s", nombre, salida)
			}
		})
	}
}

// EL OTRO CONTROL, y el que más importa: cuando el comando YA está actuando
// sobre el registro virtualizado — que es lo que pasa cuando alguien sigue el
// `--state-dir` que ahora traen los rechazos — no hay desacuerdo del que
// avisar. Un aviso que aparece siempre se vuelve ruido y deja de leerse.
func TestCuandoLasRutasCoincidenNoHayAviso(t *testing.T) {
	virtual := unRegistroVirtualizado(t)
	t.Setenv("CLI_AGENT_MCP_STATE_DIR", "")

	salida := capturaStdout(t, func() {
		if code := Run([]string{"--status", "--state-dir", virtual}, talCual); code != 0 {
			t.Errorf("pair --status devolvió %d, quería 0", code)
		}
	})
	if strings.Contains(salida, "WARNING") {
		t.Errorf("avisa de un desacuerdo consigo mismo:\n\n%s", salida)
	}
}

// La comparación es entre archivos, no entre programas: el separador y las
// mayúsculas no pueden inventar un desacuerdo, porque Windows no distingue
// ninguno de los dos y el aviso saldría sobre el archivo que el comando ya está
// leyendo.
func TestLaMismaRutaEscritaDistintoSigueSiendoLaMisma(t *testing.T) {
	virtual := unRegistroVirtualizado(t)
	conBarras := strings.ReplaceAll(virtual, `\`, "/")

	if w := virtualizedRecordWarning(conBarras); w != "" {
		t.Errorf("el mismo directorio escrito con / se tomó por otro:\n\n%s", w)
	}
	if w := virtualizedRecordWarning(strings.ToUpper(virtual)); w != "" {
		t.Errorf("el mismo directorio en mayúsculas se tomó por otro:\n\n%s", w)
	}

	// Y su control: un directorio realmente distinto sí tiene que avisar, o las
	// dos afirmaciones de arriba las cumpliría una función que nunca avisa.
	if w := virtualizedRecordWarning(t.TempDir()); w == "" {
		t.Error("un directorio distinto del virtualizado no produjo aviso: la comparación acepta cualquier cosa")
	}
}
