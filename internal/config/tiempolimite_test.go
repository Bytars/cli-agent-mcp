// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"
	"time"
)

// TestElTiempoLimiteTieneUnPorDefecto fija el criterio 4 del issue #39.
//
// EL DEFECTO
// TaskTimeout sale en 0 si nadie exporta CLI_AGENT_MCP_TASK_TIMEOUT_SECONDS, y
// en runTurn el temporizador sólo se arma con `if m.timeout > 0`. Sin
// configurar nada, un turno trabado —típicamente esperando un permiso que
// nadie puede contestar— vive hasta que muere el proceso. El servidor no tiene
// red de contención salvo que el operador se acuerde de pedirla, que es
// exactamente al revés de como tiene que ser una red de contención.
//
// SU CONTROL: las tres ramas miden cosas distintas a propósito. Si sólo se
// midiera el valor por defecto, cablear 60 min como constante y romper la
// variable de entorno pasaría en verde.
func TestElTiempoLimiteTieneUnPorDefecto(t *testing.T) {
	// El entorno de la estación puede traer la variable puesta; sin limpiarla
	// el test mediría la máquina, no el código.
	limpiáElEntorno := func(t *testing.T) {
		t.Helper()
		t.Setenv("CLI_AGENT_MCP_TASK_TIMEOUT_SECONDS", "")
	}

	t.Run("sin configurar nada son 60 minutos", func(t *testing.T) {
		limpiáElEntorno(t)
		if got := Load().TaskTimeout; got != 60*time.Minute {
			t.Errorf("TaskTimeout por defecto = %v, se esperaban 60m", got)
		}
	})

	t.Run("sigue siendo configurable", func(t *testing.T) {
		limpiáElEntorno(t)
		t.Setenv("CLI_AGENT_MCP_TASK_TIMEOUT_SECONDS", "120")
		if got := Load().TaskTimeout; got != 2*time.Minute {
			t.Errorf("TaskTimeout = %v con la variable en 120, se esperaban 2m", got)
		}
	})

	t.Run("cero lo apaga a propósito", func(t *testing.T) {
		limpiáElEntorno(t)
		t.Setenv("CLI_AGENT_MCP_TASK_TIMEOUT_SECONDS", "0")
		if got := Load().TaskTimeout; got != 0 {
			t.Errorf("TaskTimeout = %v con la variable en 0, se esperaba 0 — "+
				"poner un piso por defecto no puede quitarle al operador la salida de apagarlo", got)
		}
	})
}
