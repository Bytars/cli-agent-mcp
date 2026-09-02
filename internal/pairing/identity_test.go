// SPDX-License-Identifier: Apache-2.0

package pairing

import "testing"

// Las dos rutas reales del incidente del 1-sep, copiadas del log tal cual.
const (
	claudeVieja = `C:\Program Files\WindowsApps\Claude_1.40609.0.0_x64__pzs8sxrjxfjjc\app\claude.exe`
	claudeNueva = `C:\Program Files\WindowsApps\Claude_1.40609.1.0_x64__pzs8sxrjxfjjc\app\claude.exe`
)

// EL CASO QUE COSTÓ UN DÍA DE TRABAJO.
//
// Claude Desktop se actualizó solo en segundo plano, la ruta de su ejecutable
// cambió de versión, y el binding lo trató como un programa distinto. El
// usuario no hizo nada y se quedó sin servidor MCP. Ver issue #29.
func TestActualizarLaAppNoLaConvierteEnOtroPrograma(t *testing.T) {
	vieja := IdentityOf(claudeVieja)
	nueva := IdentityOf(claudeNueva)

	if vieja.Kind != "msix" {
		t.Fatalf("no se reconoció el paquete MSIX en %s: %+v", claudeVieja, vieja)
	}
	if vieja.Value != "Claude_pzs8sxrjxfjjc" {
		t.Errorf("familia = %q, quería \"Claude_pzs8sxrjxfjjc\" — la versión no puede formar parte de la identidad", vieja.Value)
	}
	if !vieja.Matches(nueva) {
		t.Errorf("una actualización de la app la volvió otro programa.\n"+
			"  antes:  %s\n  después: %s\n"+
			"Esto es exactamente lo que dejó la máquina sin MCP el 1-sep.", vieja.Value, nueva.Value)
	}
}

// El control del test anterior: si Matches dijera que sí a todo, aquél pasaría
// igual y no probaría nada.
func TestOtroPaqueteSigueSiendoOtroPrograma(t *testing.T) {
	claude := IdentityOf(claudeNueva)
	otro := IdentityOf(`C:\Program Files\WindowsApps\Malicioso_1.0.0.0_x64__aaaaaaaaaaaa\app\claude.exe`)

	if otro.Kind != "msix" {
		t.Fatalf("no se reconoció como MSIX: %+v", otro)
	}
	if claude.Matches(otro) {
		t.Error("dos paquetes distintos con el mismo nombre de ejecutable se dieron por iguales; " +
			"la identidad de paquete no está discriminando nada")
	}
}

// Fuera de MSIX no hay familia de paquete, así que la identidad es la carpeta
// que lo contiene más el nombre del ejecutable: sobrevive a una actualización
// en el lugar, y no confunde a dos programas homónimos en carpetas distintas.
func TestFueraDeMsixSeComparaCarpetaYNombre(t *testing.T) {
	a := IdentityOf(`C:\Tools\algun-cliente.exe`)
	b := IdentityOf(`C:\Tools\algun-cliente.exe`)
	c := IdentityOf(`D:\otra\ruta\algun-cliente.exe`)
	d := IdentityOf(`C:\Tools\cosa-distinta.exe`)

	if a.Kind != "exe" || a.Value != `c:\tools\algun-cliente.exe` {
		t.Fatalf("identidad inesperada: %+v", a)
	}
	if !a.Matches(b) {
		t.Error("el mismo programa no se reconoció")
	}
	if a.Matches(c) {
		t.Error("el mismo nombre en otra carpeta se dio por el mismo programa; " +
			"es el agujero por el que entraba el node de la caché de npm")
	}
	if a.Matches(d) {
		t.Error("dos ejecutables distintos se dieron por iguales")
	}
}

// Las dos rutas que aparecieron al medir esto en la máquina: el node con el que
// arranca el cliente, y el node bajo el que corre un `postinstall` de npm.
const (
	nodeDelCliente = `C:\Program Files\nodejs\node.exe`
	nodeDeLaCache  = `C:\Users\usuario\AppData\Local\npm-cache\_npx\a1b2\node_modules\.bin\node.exe`
)

// EL AGUJERO QUE DEJABA COMPARAR SOLO EL NOMBRE.
//
// El issue #29 nombra al atacante con precisión: código que se ejecuta pero no
// puede hurgar en el perfil — un `postinstall` de npm. Un postinstall corre bajo
// node.exe, así que para cualquier cliente basado en node el programa confiado y
// el atacante eran literalmente la misma cadena, y el atacante no necesitaba
// robar nada. Con la ruta completa —antes de este PR— no coincidían: era una
// regresión, no una concesión.
func TestElNodeDeLaCacheDeNpmNoEsElNodeDelCliente(t *testing.T) {
	cliente := IdentityOf(nodeDelCliente)
	cache := IdentityOf(nodeDeLaCache)

	if cliente.Kind != "exe" || cache.Kind != "exe" {
		t.Fatalf("no son identidades de ejecutable: %+v / %+v", cliente, cache)
	}
	if cliente.Matches(cache) {
		t.Errorf("el node de la caché de npm pasó por el node del cliente.\n"+
			"  confiado: %s\n  atacante: %s\n"+
			"Un postinstall puede llamarse node.exe; lo que no puede es escribir en C:\\Program Files.",
			cliente.Value, cache.Value)
	}
}

// SU CONTROL. Si la carpeta hiciera que nada coincidiera nunca, el test de
// arriba pasaría sin probar nada — y una actualización en el lugar, que
// reemplaza el binario sin moverlo, volvería a dejar al usuario afuera, que es
// justo lo que este PR existe para impedir.
func TestActualizarNodeEnSuCarpetaNoLoVuelveOtroPrograma(t *testing.T) {
	cliente := IdentityOf(nodeDelCliente)

	if !cliente.Matches(IdentityOf(`C:\Program Files\nodejs\node.exe`)) {
		t.Error("un binario nuevo en la misma carpeta se dio por otro programa; " +
			"así se rompe una actualización en el lugar")
	}
	// Y la misma carpeta escrita de otra manera sigue siendo la misma carpeta.
	// Windows devuelve la ruta con la caja que se le antoja, y este paquete lee
	// registros JSON que pudo escribir un build de otra plataforma: si la
	// derivación no normalizara, una diferencia de forma se leería como mudanza.
	if !cliente.Matches(IdentityOf(`C:/PROGRAM FILES/NodeJS/node.exe`)) {
		t.Error("otra forma de escribir la misma ruta produjo otra identidad")
	}
}

// La regla de la carpeta es sólo del ramal "exe". En MSIX la carpeta es
// exactamente lo que cambia al actualizar, así que dejarla entrar ahí habría
// reintroducido el bug del 1-sep que este archivo existe para arreglar.
func TestEnMsixLaCarpetaNoDecide(t *testing.T) {
	if claudeVieja == claudeNueva {
		t.Fatal("las dos rutas del incidente son iguales; el test no está midiendo nada")
	}
	vieja, nueva := IdentityOf(claudeVieja), IdentityOf(claudeNueva)
	if vieja.Kind != "msix" || nueva.Kind != "msix" {
		t.Fatalf("no se reconoció el paquete: %+v / %+v", vieja, nueva)
	}
	if !vieja.Matches(nueva) {
		t.Errorf("dos versiones del mismo paquete dejaron de coincidir: %s vs %s.\n"+
			"La comparación por carpeta se filtró al ramal MSIX.", vieja.Value, nueva.Value)
	}

	// El otro sentido: que MSIX ignore la carpeta no puede volverlo permisivo.
	otro := IdentityOf(`C:\Program Files\WindowsApps\Malicioso_1.0.0.0_x64__aaaaaaaaaaaa\app\claude.exe`)
	if otro.Kind != "msix" {
		t.Fatalf("no se reconoció como MSIX: %+v", otro)
	}
	if vieja.Matches(otro) {
		t.Error("otro paquete con el mismo ejecutable se dio por el mismo programa")
	}
}

// Una identidad vacía no coincide con nada — ni siquiera consigo misma.
//
// Es lo que hace que «no se pudo nombrar al lanzador» caiga en servir, en vez
// de coincidir con todo (que sería no proteger) o rechazar a todos (que sería
// dejar al usuario afuera, el defecto que #29 existe para eliminar).
func TestUnaIdentidadVaciaNoCoincideConNada(t *testing.T) {
	vacia := IdentityOf("")
	if vacia.Kind != "" {
		t.Errorf("una ruta vacía produjo %+v", vacia)
	}
	for nombre, otra := range map[string]Identity{
		"consigo misma": vacia,
		"un msix":       IdentityOf(claudeNueva),
		"un exe":        IdentityOf(`C:\Tools\x.exe`),
	} {
		if vacia.Matches(otra) || otra.Matches(vacia) {
			t.Errorf("la identidad vacía coincidió con %s", nombre)
		}
	}
}

// La ruta se guarda para que una persona reconozca el programa, pero no se
// compara. Si se comparara, volveríamos al bug del 1-sep.
func TestLaRutaSeGuardaPeroNoSeCompara(t *testing.T) {
	vieja := IdentityOf(claudeVieja)
	if vieja.Path != claudeVieja {
		t.Errorf("no se conservó la ruta para mostrarla: %q", vieja.Path)
	}
	nueva := IdentityOf(claudeNueva)
	if vieja.Path == nueva.Path {
		t.Fatal("las dos rutas del incidente son iguales; el test no está midiendo lo que dice")
	}
	if !vieja.Matches(nueva) {
		t.Error("rutas distintas impidieron la coincidencia, así que la ruta sí se está comparando")
	}
}

// El ejecutable puede estar en la raíz del paquete o bajo app\; los dos casos
// ocurren y los dos tienen que resolver a la misma familia.
func TestLaProfundidadDentroDelPaqueteNoImporta(t *testing.T) {
	bajoApp := IdentityOf(`C:\Program Files\WindowsApps\Cosa_2.0.0.0_x64__hhhhhhhhhhhh\app\cosa.exe`)
	enLaRaiz := IdentityOf(`C:\Program Files\WindowsApps\Cosa_2.0.0.0_x64__hhhhhhhhhhhh\cosa.exe`)
	hondo := IdentityOf(`C:\Program Files\WindowsApps\Cosa_2.0.0.0_x64__hhhhhhhhhhhh\app\bin\win\cosa.exe`)

	for nombre, id := range map[string]Identity{"bajo app": bajoApp, "en la raíz": enLaRaiz, "más hondo": hondo} {
		if id.Kind != "msix" {
			t.Errorf("%s: no se reconoció el paquete: %+v", nombre, id)
			continue
		}
		if id.Value != "Cosa_hhhhhhhhhhhh" {
			t.Errorf("%s: familia = %q", nombre, id.Value)
		}
	}
}
