// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// nombreServidorDePrueba es deliberadamente distinto del binario real. Todo lo
// que este test cuenta y mata lo hace por este nombre de imagen, así que no
// puede alcanzar al servidor cli-agent-mcp que el usuario tenga corriendo.
const nombreServidorDePrueba = "capm-t39"

// plazoDeApagado es el <10 s del criterio 2 del issue #39.
const plazoDeApagado = 10 * time.Second

// TestCerrarStdinMataElTurnoEnCurso fija el criterio 2 del issue #39.
//
// EL DEFECTO
// main.go corre `srv.Run(context.Background(), &mcp.StdioTransport{})` y no
// instala manejador de señal ni de fin de stdin. StartTask, runDetached y
// Followup cuelgan cada turno de otro context.Background(). No hay ningún
// contexto que se cancele cuando el cliente se va, así que nada le avisa al
// árbol de procesos del turno que el servidor terminó.
//
// POR QUÉ EXTREMO A EXTREMO Y NO UN TEST DE UNIDAD
// El issue anota como NO VERIFICADO si `srv.Run` del SDK vuelve de verdad al
// cerrarse stdin. Eso no se puede contestar con un doble: hay que cerrar el
// pipe contra el binario construido y mirar. Por eso el test habla MCP sobre
// pipes propios (IOTransport) en vez de CommandTransport, que no deja cerrar
// stdin por separado.
//
// SU CONTROL: el test se apoya en que el turno mock esté realmente vivo antes
// de cerrar stdin — lo exige abajo con procesosVivos() >= 2 (servidor + hijo).
// Sin esa espera pasaría en verde midiendo un turno que todavía no arrancó.
func TestCerrarStdinMataElTurnoEnCurso(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, nombreServidorDePrueba+sufijoEjecutable())

	build := exec.Command("go", "build", "-o", exe, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("construyendo el servidor: %v", err)
	}

	srv := exec.Command(exe)
	srv.Env = append(os.Environ(),
		"CLI_AGENT_MCP_DEFAULT_AGENT=mock",
		"CLI_AGENT_MCP_STATE_DIR="+filepath.Join(dir, "estado"),
		// Sin esto el servidor levanta el broker de aprobaciones, que abre un
		// puerto y no tiene nada que ver con lo que se mide acá.
		"CLI_AGENT_MCP_ASK_PERMISSION=false",
		// Hermético a propósito: la estación de quien desarrolla puede tener
		// CLI_AGENT_MCP_ALLOWED_CWDS exportado, y entonces el turno se rechaza
		// antes de arrancar y el test mide un apagado que nunca tuvo nada que
		// apagar.
		"CLI_AGENT_MCP_ALLOWED_CWDS="+dir,
	)
	stdin, err := srv.StdinPipe()
	if err != nil {
		t.Fatalf("stdin del servidor: %v", err)
	}
	stdout, err := srv.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout del servidor: %v", err)
	}
	srv.Stderr = os.Stderr
	if err := srv.Start(); err != nil {
		t.Fatalf("arrancando el servidor: %v", err)
	}
	// Red de contención: si el test falla, lo que quede vivo es exactamente lo
	// que el defecto deja vivo, y no puede quedar corriendo después.
	t.Cleanup(func() { matáPorNombre(nombreServidorDePrueba + sufijoEjecutable()) })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cliente := mcp.NewClient(&mcp.Implementation{Name: "apagado39", Version: "0.1.0"}, nil)
	sesión, err := cliente.Connect(ctx, &mcp.IOTransport{Reader: stdout, Writer: stdin}, nil)
	if err != nil {
		t.Fatalf("conectando por MCP: %v", err)
	}

	// sleep:600000 — el mock corre diez minutos, muchísimo más que el plazo que
	// se mide, así que si termina es porque lo mataron.
	res, err := sesión.CallTool(ctx, &mcp.CallToolParams{
		Name:      "agent_start_task",
		Arguments: map[string]any{"prompt": "sleep:600000", "agent": "mock", "cwd": dir},
	})
	if err != nil {
		t.Fatalf("agent_start_task: %v", err)
	}
	if res.IsError {
		t.Fatalf("agent_start_task devolvió un error: %s", textoDe(res))
	}
	t.Logf("agent_start_task: %s", textoDe(res))

	imagen := nombreServidorDePrueba + sufijoEjecutable()
	esperá(t, 30*time.Second, "que el turno mock esté corriendo de verdad", func() bool {
		return procesosVivos(t, imagen) >= 2
	}, func() string {
		return fmt.Sprintf("procesos vivos de %s: %d", imagen, procesosVivos(t, imagen))
	})

	// EL CONTROL, y va primero a propósito: si el árbol se muriera solo, la
	// medición de abajo daría verde sin que cerrar stdin tenga nada que ver.
	// Acá nadie cierra nada y el turno tiene que seguir vivo todo el plazo.
	inicio := time.Now()
	for time.Now().Before(inicio.Add(plazoDeApagado)) {
		if n := procesosVivos(t, imagen); n == 0 {
			t.Fatalf("control: el árbol desapareció solo a los %s, sin que nadie cerrara stdin; "+
				"este test no puede medir el apagado", time.Since(inicio).Round(10*time.Millisecond))
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("control: tras %s sin cerrar stdin siguen vivos %d procesos, como debe ser", plazoDeApagado, procesosVivos(t, imagen))

	// El cliente se fue: esto es lo que Claude Desktop le hace al servidor.
	if err := stdin.Close(); err != nil {
		t.Fatalf("cerrando stdin: %v", err)
	}
	cierre := time.Now()

	for time.Now().Before(cierre.Add(plazoDeApagado)) {
		if procesosVivos(t, imagen) == 0 {
			t.Logf("todo el árbol murió %s después de cerrar stdin", time.Since(cierre).Round(10*time.Millisecond))
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("a los %s de cerrar stdin siguen vivos %d procesos del árbol del turno; el criterio 2 pide 0 antes de %s",
		plazoDeApagado, procesosVivos(t, imagen), plazoDeApagado)
}

func sufijoEjecutable() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// procesosVivos cuenta los procesos cuya imagen es nombre. El nombre es único
// de este test, así que la cuenta no puede confundirse con ningún otro binario.
func procesosVivos(t *testing.T, nombre string) int {
	t.Helper()
	if runtime.GOOS == "windows" {
		// /NH quita el encabezado; sin tareas, tasklist escribe una línea
		// "INFO:" que no contiene el nombre de la imagen.
		out, _ := exec.Command("tasklist", "/FI", "IMAGENAME eq "+nombre, "/NH", "/FO", "CSV").Output()
		return strings.Count(string(out), `"`+nombre+`"`)
	}
	out, _ := exec.Command("pgrep", "-c", "-f", nombre).Output()
	n := 0
	for _, l := range strings.Fields(string(out)) {
		if v, err := time.ParseDuration(l + "ns"); err == nil {
			n = int(v)
		}
	}
	return n
}

func matáPorNombre(nombre string) {
	if runtime.GOOS == "windows" {
		_ = exec.Command("taskkill", "/IM", nombre, "/F", "/T").Run()
		return
	}
	_ = exec.Command("pkill", "-9", "-f", nombre).Run()
}

func esperá(t *testing.T, límite time.Duration, qué string, ok func() bool, diag func() string) {
	t.Helper()
	deadline := time.Now().Add(límite)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("se agotaron %s esperando %s — %s", límite, qué, diag())
}

// textoDe junta el texto de un resultado de herramienta, para que un fallo diga
// lo que el servidor contestó en vez de sólo que contestó mal.
func textoDe(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
