// SPDX-License-Identifier: Apache-2.0

package pairing_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Bytars/cli-agent-mcp/internal/pairing"
	"github.com/Bytars/cli-agent-mcp/internal/state"
)

// TestPairEscribeDondeElServidorLee fija el arreglo del issue #22.
//
// EL DEFECTO QUE ESTO IMPIDE QUE VUELVA
// El servidor toma su directorio de estado de CLI_AGENT_MCP_STATE_DIR
// (internal/config/config.go). `pair` lo tomaba sólo de su propio flag, así que
// con la variable puesta y sin --state-dir escribía el registro en el directorio
// por-usuario mientras el servidor leía el de la variable.
//
// Lo peligroso no era el desacuerdo: era el silencio. `pair` imprimía
// `paired "cowork"` y su token, y el servidor seguía sirviendo a cualquier
// lanzador. **El operador creía haber cerrado el agujero y estaba abierto.** Un
// no-op que falla hacia el lado inseguro es peor que un error.
//
// SU CONTROL, para correr a mano cuando se toque esto: volvé `dir` en
// pairing.Run a `resolveStateDir(*stateDir)` a secas. Este test tiene que
// ponerse ROJO en el primer subtest, diciendo que el registro no apareció donde
// la variable manda. Si sigue verde, no está midiendo lo que dice medir.
func TestPairEscribeDondeElServidorLee(t *testing.T) {
	// Ni el subtest de la variable ni el del flag pueden correr en paralelo:
	// los dos escriben la misma variable de entorno del proceso.

	t.Run("sin flag, manda la variable de entorno", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("CLI_AGENT_MCP_STATE_DIR", dir)

		if code := pairing.Run([]string{"--label", "prueba"}, state.ResolveDir); code != 0 {
			t.Fatalf("pair devolvió %d, quería 0", code)
		}

		// La afirmación es sobre el archivo, no sobre lo que pair imprimió: un
		// mensaje de éxito es exactamente lo que el defecto ya sabía producir.
		if _, err := os.Stat(pairing.Path(dir)); err != nil {
			t.Errorf("pair no escribió el registro donde CLI_AGENT_MCP_STATE_DIR manda.\n"+
				"Esperaba %s\nstat dijo: %v\n\n"+
				"Éste es el defecto del issue #22: el servidor lee esa variable y pair no la miraba,\n"+
				"así que se pareaba un directorio y se servía otro, sin ningún error.",
				pairing.Path(dir), err)
		}

		// Y que el registro sea legible y esté realmente pareado: un archivo
		// vacío en el lugar correcto pasaría el stat de arriba.
		f, pareado, err := pairing.Load(dir)
		if err != nil {
			t.Fatalf("el registro que dejó pair no se puede leer: %v", err)
		}
		if !pareado {
			t.Error("el registro existe pero dice que no está pareado")
		}
		if got := len(f.Tokens); got != 1 {
			t.Errorf("tokens = %d, quería 1", got)
		}
	})

	t.Run("el flag explícito le gana a la variable", func(t *testing.T) {
		delDeLaVariable := t.TempDir()
		delFlag := t.TempDir()
		t.Setenv("CLI_AGENT_MCP_STATE_DIR", delDeLaVariable)

		if code := pairing.Run([]string{"--state-dir", delFlag, "--label", "prueba"}, state.ResolveDir); code != 0 {
			t.Fatalf("pair devolvió %d, quería 0", code)
		}

		if _, err := os.Stat(pairing.Path(delFlag)); err != nil {
			t.Errorf("el registro no está donde lo mandó --state-dir (%s): %v", delFlag, err)
		}
		// Lo explícito gana, y no de casualidad: el otro directorio tiene que
		// haber quedado intacto. Sin esta mitad, escribir en LOS DOS lugares
		// también pasaría el test.
		if _, err := os.Stat(pairing.Path(delDeLaVariable)); !os.IsNotExist(err) {
			t.Errorf("pair también escribió en el directorio de la variable (%s), y no debía: %v",
				filepath.Dir(pairing.Path(delDeLaVariable)), err)
		}
	})
}
