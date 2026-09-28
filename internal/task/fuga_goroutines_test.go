// SPDX-License-Identifier: Apache-2.0

package task

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Bytars/cli-agent-mcp/internal/agent"
	"github.com/Bytars/cli-agent-mcp/internal/state"
)

// turnosDePrueba es el N del criterio 1 del issue #39.
const turnosDePrueba = 100

// TestCienTurnosExitososNoDejanGoroutinesVivas fija el criterio 1 del issue #39.
//
// EL DEFECTO
// runTurn abre su contexto con context.WithCancel y arranca
// `go watchCancelRequest(ctx, t, cancel)`, un ticker de 1 s. El único modo en
// que esa goroutine sale es `<-ctx.Done()`. Pero no hay `defer cancel()`:
// cancel() se llama en fail(), en el timeout y en Manager.Cancel — es decir,
// sólo cuando el turno sale MAL. Un turno que termina bien devuelve sin
// cancelar nada, así que cada turno exitoso deja una goroutine viva para
// siempre, despertándose una vez por segundo hasta que muere el proceso.
//
// SU CONTROL: sacá el `m.SetStore(store)` de acá abajo y este test pasa en
// verde con la fuga intacta — watchCancelRequest devuelve en la primera línea
// cuando no hay store, así que no habría nada que fugar. El store no es
// decorado: es la condición que hace que el defecto exista, y el servidor real
// siempre tiene uno (main.go abre el suyo al arrancar).
func TestCienTurnosExitososNoDejanGoroutinesVivas(t *testing.T) {
	t.Setenv(helperEnv, "1")

	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer store.Close()

	m := NewManager(turnosDePrueba + 10)
	m.SetStore(store)
	ws := At(t.TempDir())

	corréUnTurno := func(n int) {
		t.Helper()
		tk, err := m.StartTask(sleepAdapter{ms: 0}, ws, agent.RunSpec{Prompt: "work"}, Options{})
		if err != nil {
			t.Fatalf("StartTask (turno %d): %v", n, err)
		}
		waitFor(t, 30*time.Second, "que el turno termine", func() bool {
			return tk.Snapshot().Status != StatusRunning
		})
		if s := tk.Snapshot().Status; s != StatusDone {
			t.Fatalf("turno %d terminó como %q, este test mide turnos EXITOSOS", n, s)
		}
	}

	// Calentamiento. El primer turno crea de una vez lo que después se reusa
	// (el ayudante, los buffers del store). Medir antes contaría eso como fuga.
	corréUnTurno(0)
	base := goroutinesEstables()

	for i := 1; i <= turnosDePrueba; i++ {
		corréUnTurno(i)
	}

	final := goroutinesEstables()
	if final > base+2 {
		t.Errorf("tras %d turnos exitosos hay %d goroutines vivas, se esperaban %d ±2 (sobran %d)",
			turnosDePrueba, final, base, final-base)
		t.Errorf("goroutines detenidas en watchCancelRequest: %d", goroutinesEn("watchCancelRequest"))
	}
}

// goroutinesEstables devuelve el menor conteo observado en una ventana corta.
//
// Las goroutines de un turno recién terminado tardan un instante en irse, y
// tomar una sola muestra convierte eso en un test intermitente. El mínimo de la
// ventana es lo que distingue "todavía está saliendo" de "no se va".
func goroutinesEstables() int {
	runtime.GC()
	menor := runtime.NumGoroutine()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		if n := runtime.NumGoroutine(); n < menor {
			menor = n
		}
	}
	return menor
}

// goroutinesEn cuenta cuántas goroutines vivas tienen a fn en su pila. Un
// número crudo dice que algo se fuga; éste dice qué.
func goroutinesEn(fn string) int {
	buf := make([]byte, 1<<22)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), fn+"(")
}
