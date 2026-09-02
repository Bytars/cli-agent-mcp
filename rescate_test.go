package main

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// El candado tiene que dejar pasar exactamente una herramienta.
//
// Si no deja pasar el rescate, un servidor bloqueado no se puede recuperar
// desde el cliente y volvemos al caso de #29: cuatro rescates desde una
// terminal que no hicieron nada porque escribían en otro archivo. Y si deja
// pasar cualquier otra, el candado no está cerrando nada.
//
// Las dos mitades importan, así que las dos están acá: la afirmación y su
// control, en el mismo test, para que no se pueda arreglar una rompiendo la
// otra sin que se note.
func TestElCandadoDejaPasarSoloElRescate(t *testing.T) {
	llegadas := 0
	siguiente := func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		llegadas++
		return &mcp.CallToolResult{}, nil
	}
	candado := lockdown("bloqueado")(siguiente)
	pedido := func(nombre string) mcp.Request {
		return &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: nombre}}
	}

	if _, err := candado(context.Background(), "tools/call", pedido(rescueTool)); err != nil {
		t.Fatalf("el rescate no atravesó el candado: %v", err)
	}
	if llegadas != 1 {
		t.Fatalf("llegadas = %d: el rescate no llegó al handler", llegadas)
	}

	res, err := candado(context.Background(), "tools/call", pedido("agent_run_task"))
	if err != nil {
		t.Fatalf("error de protocolo donde debía haber un rechazo legible: %v", err)
	}
	if llegadas != 1 {
		t.Fatal("una herramienta cualquiera atravesó el candado")
	}
	r, ok := res.(*mcp.CallToolResult)
	if !ok || !r.IsError {
		t.Fatalf("el rechazo no vino marcado como error: %+v", res)
	}
}

// El rechazo tiene que nombrar la salida.
//
// Un rescate del que el modelo nunca se entera es un rescate que nadie corre, y
// este mensaje es el único canal que llega a un usuario cuyo cliente no
// funciona — el mismo razonamiento por el que #25 puso la salida en el log.
func TestElRechazoNombraElRescate(t *testing.T) {
	siguiente := func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		return &mcp.CallToolResult{}, nil
	}
	candado := lockdown("no estás autorizado")(siguiente)
	res, _ := candado(context.Background(), "tools/call",
		&mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "agent_run_task"}})

	r, ok := res.(*mcp.CallToolResult)
	if !ok || len(r.Content) == 0 {
		t.Fatalf("el rechazo vino vacío: %+v", res)
	}
	texto, ok := r.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("el rechazo no trae texto: %+v", r.Content[0])
	}
	for _, debe := range []string{"no estás autorizado", rescueTool, "restart"} {
		if !strings.Contains(texto.Text, debe) {
			t.Errorf("el rechazo no menciona %q: %s", debe, texto.Text)
		}
	}
}
