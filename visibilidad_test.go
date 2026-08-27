// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// TestStartTaskDiceQueElUsuarioNoVeNada fija el issue #23.
//
// EL DEFECTO, Y POR QUÉ NO ERA UN DEFECTO DE CÓDIGO
// agent_start_task ya nombraba agent_task_board en su respuesta. No alcanzó:
// medido sobre una sesión real, hubo decenas de llamadas a agent_start_task y
// **ni una sola** a agent_task_board, mientras el usuario decía que las tareas
// corrían sin que él se enterara. El puntero estaba ahí y se saltó todas las
// veces.
//
// La causa es la forma, no la ausencia. El texto viejo ofrecía dos llamadas
// unidas por un "or": el board, que sirve a la persona, y agent_watch, que
// devuelve lo que el modelo necesita para su próximo paso. Quien elige es el
// modelo, así que gana watch siempre.
//
// Por eso lo que se afirma acá no es "menciona el board" —el texto viejo lo
// mencionaba y fallaba— sino que **declara el estado del usuario como un
// hecho** y que **no presenta watch como alternativa equivalente**.
//
// SU CONTROL: volvé el mensaje a la redacción vieja
//
//	"... Call agent_task_board to show the user a live panel of it, or
//	 agent_watch to block until it finishes."
//
// Este test tiene que ponerse ROJO. Si sigue verde con ese texto, sólo está
// midiendo que aparezca la palabra "agent_task_board", que es exactamente lo
// que ya se demostró insuficiente.
func TestStartTaskDiceQueElUsuarioNoVeNada(t *testing.T) {
	// El mensaje real que arma el handler, con datos de juguete. Se reconstruye
	// en vez de invocar la tool porque lo que se afirma es el texto, y montar un
	// servidor entero para leer una cadena escondería el sujeto de la prueba.
	msg := startTaskMessage("task-1-abcd", "mock", `C:\algun\lado`)

	t.Run("declara el estado del usuario como un hecho", func(t *testing.T) {
		if !strings.Contains(msg, "THE USER CANNOT SEE THIS TASK") {
			t.Errorf("el mensaje no dice que el usuario no ve la tarea.\n"+
				"Sin eso vuelve a ser una sugerencia entre otras, y el issue #23 mostró\n"+
				"que una sugerencia se ignora.\n\nmensaje:\n%s", msg)
		}
	})

	t.Run("manda llamar al board, sin ofrecer watch como equivalente", func(t *testing.T) {
		if !strings.Contains(msg, "Call agent_task_board now") {
			t.Errorf("el mensaje no manda llamar al board de inmediato.\n\nmensaje:\n%s", msg)
		}
		// La mitad que de verdad discrimina: el texto viejo también nombraba el
		// board, y perdía porque enseguida ofrecía watch con un "or". Si esa
		// construcción vuelve, esto se pone rojo.
		if strings.Contains(msg, "or agent_watch") {
			t.Errorf("el mensaje vuelve a ofrecer agent_watch como alternativa al board.\n"+
				"Ésa es la redacción que falló: quien elige es el modelo, y watch le sirve\n"+
				"a él mientras el board le sirve al usuario.\n\nmensaje:\n%s", msg)
		}
	})

	t.Run("nombra la salida de fuera de la conversación", func(t *testing.T) {
		if !strings.Contains(msg, "cli-agent-mcp logs --all") {
			t.Errorf("el mensaje no nombra el visor de terminal, que es lo único que\n"+
				"le sirve al usuario cuando el orquestador no cuenta nada.\n\nmensaje:\n%s", msg)
		}
	})

	t.Run("sigue diciendo los datos de la tarea", func(t *testing.T) {
		// Sin esto, un mensaje que fuera puro sermón pasaría los tres de arriba.
		for _, quiero := range []string{"task-1-abcd", "mock", `C:\algun\lado`} {
			if !strings.Contains(msg, quiero) {
				t.Errorf("el mensaje perdió %q, que es el dato por el que se llamó a la tool", quiero)
			}
		}
	})
}
