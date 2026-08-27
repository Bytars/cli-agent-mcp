// SPDX-License-Identifier: Apache-2.0

package task

import (
	"os"
	"testing"
	"time"

	"github.com/Bytars/cli-agent-mcp/internal/agent"
	"github.com/Bytars/cli-agent-mcp/internal/state"
)

// TestUnaSesionNoAdoptaLasTareasDeOtraViva fija el issue #21.
//
// EL DEFECTO
// Restore adoptaba todo lo que hubiera en el directorio. Con dos servidores
// vivos —dos ventanas del mismo cliente son dos sesiones— la segunda se llevaba
// las tareas de la primera, las mostraba como "orphaned", y desde ahí se podían
// cancelar. Una ventana que no había arrancado nada podía matar el trabajo de
// otra.
//
// LO QUE **NO** DEBE ROMPER, y por eso está el segundo caso: tras un reinicio
// normal no hay nadie vivo, y ahí adoptar es exactamente lo correcto — es cómo
// un cliente cerrado y reabierto conserva su historial.
//
// SU CONTROL: sacá el `if foreign != nil { return 0 }` de Restore. El primer
// subtest tiene que ponerse ROJO diciendo que adoptó 1 tarea ajena, y el
// segundo tiene que SEGUIR VERDE. Un cambio que ponga rojos a los dos rompió el
// reinicio, no arregló el aislamiento.
func TestUnaSesionNoAdoptaLasTareasDeOtraViva(t *testing.T) {
	// dejaUnaTareaEnDisco simula lo que otra instancia dejó: un registro todavía
	// marcado como corriendo.
	dejaUnaTareaEnDisco := func(t *testing.T, store *state.Store) {
		t.Helper()
		primera := NewManager(10)
		primera.SetStore(store)
		tk := &Task{
			ID:        "task-1-ajena",
			AgentName: "mock",
			Cwd:       t.TempDir(),
			store:     store,
			status:    StatusRunning,
			running:   true,
			startedAt: time.Now(),
			turns:     []TurnInfo{{Prompt: "trabajo de la otra sesion"}},
		}
		tk.persist()
	}

	t.Run("con otra instancia viva, no adopta nada", func(t *testing.T) {
		store, err := state.Open(t.TempDir())
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		defer store.Close()
		dejaUnaTareaEnDisco(t, store)

		// Lo que Acquire reporta cuando el lock es de un proceso VIVO. Se usa el
		// pid de este mismo test porque lo único que Restore mira es que haya
		// un dueño: quién es, no le importa.
		viva := &state.Owner{PID: os.Getpid(), Started: time.Now()}

		segunda := NewManager(10)
		segunda.SetStore(store)
		if n := segunda.Restore(agent.NewRegistry(agent.NewMockAdapter()), viva); n != 0 {
			t.Errorf("adoptó %d tarea(s) de una sesión viva, quería 0.\n"+
				"Éste es el defecto del issue #21: la segunda ventana se lleva las tareas de la primera\n"+
				"y desde ahí las puede cancelar.", n)
		}

		// Y no alcanza con que el contador diga 0: la tarea no puede estar
		// listada. Un Restore que devolviera 0 pero igual la metiera en el mapa
		// pasaría la afirmación de arriba y dejaría el agujero abierto.
		if _, ok := segunda.Get("task-1-ajena"); ok {
			t.Error("la tarea ajena quedó accesible por Get pese a que Restore contó 0")
		}
		if n := len(segunda.List()); n != 0 {
			t.Errorf("List devuelve %d tarea(s), quería 0", n)
		}
	})

	t.Run("sin nadie vivo, el reinicio sigue adoptando", func(t *testing.T) {
		store, err := state.Open(t.TempDir())
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		defer store.Close()
		dejaUnaTareaEnDisco(t, store)

		siguiente := NewManager(10)
		siguiente.SetStore(store)
		if n := siguiente.Restore(agent.NewRegistry(agent.NewMockAdapter()), nil); n != 1 {
			t.Fatalf("restauró %d tarea(s) tras un reinicio, quería 1.\n"+
				"El aislamiento no debe costar el historial: un cliente cerrado y reabierto\n"+
				"tiene que volver a ver sus tareas.", n)
		}
		if _, ok := siguiente.Get("task-1-ajena"); !ok {
			t.Error("la tarea no quedó accesible tras el reinicio")
		}
	})
}
