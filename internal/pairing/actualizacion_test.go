// SPDX-License-Identifier: Apache-2.0

package pairing

import (
	"strings"
	"testing"
)

// El incidente completo, extremo a extremo: la app se actualiza sola y el
// servidor tiene que seguir sirviendo.
//
// Los tests de identity_test.go prueban la comparación aislada. Éste prueba que
// esa comparación es la que Verify usa de verdad — que es donde el bug vivía, y
// donde un arreglo puede quedar escrito sin estar conectado a nada.
//
// SU CONTROL está abajo, en TestOtraAppSiQuedaAfuera: sin él, un Verify que
// aceptara cualquier lanzador pasaría este test igual.
func TestUnaActualizacionDelClienteNoTeDejaAfuera(t *testing.T) {
	t.Run("con token bindeado", func(t *testing.T) {
		dir := t.TempDir()
		secreto, err := Mint(dir, "cowork", false)
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		// Primer arranque: queda bindeado a la versión vieja.
		if r, _ := Verify(dir, secreto, claudeVieja); r.Status != OK {
			t.Fatalf("primer arranque: %v", r.Status)
		}
		// La app se actualiza y vuelve a arrancar el servidor.
		r, err := Verify(dir, secreto, claudeNueva)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if r.Status != OK {
			t.Fatalf("tras actualizar la app: status = %v (%s), quería OK.\n"+
				"Esto es exactamente lo que dejó la máquina sin MCP el 1-sep: el usuario no hizo\n"+
				"nada, una app se actualizó en segundo plano, y el pairing la trató como un intruso.", r.Status, r.Detail)
		}
	})

	t.Run("con lanzador confiado", func(t *testing.T) {
		dir := t.TempDir()
		if r, _ := Verify(dir, "", claudeVieja); r.Status != TrustedLauncher {
			t.Fatalf("primer arranque: %v", r.Status)
		}
		envejecerElRegistro(t, dir) // fuera de la ventana de aprendizaje

		r, err := Verify(dir, "", claudeNueva)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if r.Status != TrustedLauncher || !r.Allowed() {
			t.Fatalf("tras actualizar la app: status = %v (%s), quería TrustedLauncher.\n"+
				"La ventana de aprendizaje NO cubre esto: una app se actualiza semanas después,\n"+
				"con la lista ya cerrada.", r.Status, r.Detail)
		}
	})
}

// EL CONTROL. Que una actualización pase no puede significar que pase
// cualquiera.
//
// El rechazo llega en el segundo arranque: al primero se le avisa y se lo sirve
// (announce.go). trasElAviso afirma esa mitad, así que este control sigue
// midiendo lo que dice —que el intruso termina afuera— y de paso comprueba que
// se lo dijimos antes.
func TestOtraAppSiQuedaAfuera(t *testing.T) {
	const otraApp = `C:\Program Files\WindowsApps\OtraCosa_3.0.0.0_x64__zzzzzzzzzzzz\app\claude.exe`

	t.Run("con token bindeado", func(t *testing.T) {
		dir := t.TempDir()
		secreto, _ := Mint(dir, "cowork", false)
		if r, _ := Verify(dir, secreto, claudeVieja); r.Status != OK {
			t.Fatalf("preparación: %v", r.Status)
		}
		r := trasElAviso(t, dir, secreto, otraApp)
		if r.Status != ForeignParent || r.Allowed() {
			t.Fatalf("status = %v (allowed=%v): otro paquete entró con un token robado", r.Status, r.Allowed())
		}
	})

	t.Run("con lanzador confiado", func(t *testing.T) {
		dir := t.TempDir()
		if r, _ := Verify(dir, "", claudeVieja); r.Status != TrustedLauncher {
			t.Fatalf("preparación: %v", r.Status)
		}
		envejecerElRegistro(t, dir)
		r := trasElAviso(t, dir, "", otraApp)
		if r.Status != ForeignLauncher || r.Allowed() {
			t.Fatalf("status = %v (allowed=%v): otro programa entró", r.Status, r.Allowed())
		}
	})
}

// Y el registro viejo, escrito con rutas completas antes de este cambio, tiene
// que seguir funcionando sin migración: la identidad se deriva al leer.
func TestUnRegistroViejoConRutasSigueSirviendo(t *testing.T) {
	dir := t.TempDir()
	f := &File{Version: fileVersion}
	f.TrustedLaunchers = append(f.TrustedLaunchers, Launcher{Exe: claudeVieja})
	if err := Save(dir, f); err != nil {
		t.Fatalf("Save: %v", err)
	}
	envejecerElRegistro(t, dir)

	r, err := Verify(dir, "", claudeNueva)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if r.Status != TrustedLauncher {
		t.Fatalf("un registro escrito antes del cambio dejó de servir: %v (%s)", r.Status, r.Detail)
	}
}

// El mensaje de rechazo tiene que nombrar algo que la persona reconozca. Una
// familia de paquete cruda no le dice nada a nadie.
func TestElRechazoSeEntiende(t *testing.T) {
	dir := t.TempDir()
	if r, _ := Verify(dir, "", claudeVieja); r.Status != TrustedLauncher {
		t.Fatalf("preparación: %v", r.Status)
	}
	envejecerElRegistro(t, dir)
	r := trasElAviso(t, dir, "", `C:\Temp\raro.exe`)
	if r.Status != ForeignLauncher {
		t.Fatalf("status = %v", r.Status)
	}
	for _, quiero := range []string{"raro.exe", "trust --add"} {
		if !strings.Contains(r.Detail, quiero) {
			t.Errorf("el rechazo no menciona %q:\n%s", quiero, r.Detail)
		}
	}
}
