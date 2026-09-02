// SPDX-License-Identifier: Apache-2.0

package pairing

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// trasElAviso corre el arranque de aviso y devuelve el veredicto del SEGUNDO,
// que es el que rechaza.
//
// Existe para que todo test de rechazo afirme, de paso, que el aviso llegó
// primero. Si algún día Verify vuelve a bloquear de una, esos tests fallan acá
// —con el nombre del problema puesto— en vez de fallar más abajo por una razón
// que no se parece a la causa.
func trasElAviso(t *testing.T, dir, secreto, exe string) Result {
	t.Helper()
	aviso, err := Verify(dir, secreto, exe)
	if err != nil {
		t.Fatalf("el arranque de aviso: %v", err)
	}
	if aviso.Status != Announced || !aviso.Allowed() {
		t.Fatalf("el primer arranque de %s dio status=%v (allowed=%v), quería Announced servido.\n"+
			"Bloquear sin haber avisado antes es indistinguible de que el software esté roto "+
			"(issue #29, punto 4).\n%s", exe, aviso.Status, aviso.Allowed(), aviso.Detail)
	}
	segundo, err := Verify(dir, secreto, exe)
	if err != nil {
		t.Fatalf("el segundo arranque: %v", err)
	}
	return segundo
}

// El punto 4 del issue #29, en los dos mecanismos: nadie queda afuera sin que un
// arranque anterior lo haya anunciado.
//
// LO QUE ESTO PREVIENE
// Claude Desktop se actualizó sola en segundo plano, la ruta cambió, y el
// servidor pasó de servir ayer a rechazar hoy sin nada en el medio. Desde
// adentro del cliente eso no se distingue de un binario roto: no hay diálogo,
// no hay motivo, y las seis veces que pasó en dos días se diagnosticaron como
// crashes y rutas mal puestas antes de que alguien sospechara del pairing.
//
// SU CONTROL es TestElSegundoArranqueSiRechaza, abajo: sin él, un Verify que
// sirviera siempre pasaría este test igual.
func TestElPrimerArranqueDeUnDesconocidoAvisaEnVezDeBloquear(t *testing.T) {
	t.Run("lanzador ajeno", func(t *testing.T) {
		dir := t.TempDir()
		if r, _ := Verify(dir, "", elCliente); r.Status != TrustedLauncher {
			t.Fatalf("preparación: %v", r.Status)
		}
		envejecerElRegistro(t, dir) // fuera de la ventana de aprendizaje

		r, err := Verify(dir, "", elOtro)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if r.Status != Announced || !r.Allowed() {
			t.Fatalf("status = %v (allowed=%v), quería Announced servido: %s", r.Status, r.Allowed(), r.Detail)
		}
		// El aviso sólo sirve si dice las tres cosas: que esto se termina,
		// cuándo, y qué comando lo evita.
		stderr, alModelo := Explain(r)
		for canal, msg := range map[string]string{"el log": stderr, "el mensaje al modelo": alModelo} {
			for _, quiero := range []string{"THIS ONCE", "trust --add"} {
				if !strings.Contains(msg, quiero) {
					t.Errorf("%s no dice %q, así que el usuario no sabe que le queda un arranque ni cómo evitarlo:\n\n%s",
						canal, quiero, msg)
				}
			}
		}
	})

	t.Run("token bindeado a otro programa", func(t *testing.T) {
		dir := t.TempDir()
		secreto, err := Mint(dir, "cowork", false)
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		confirm(t, dir, secreto, elCliente)

		r, err := Verify(dir, secreto, elOtro)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if r.Status != Announced || !r.Allowed() {
			t.Fatalf("status = %v (allowed=%v), quería Announced servido: %s", r.Status, r.Allowed(), r.Detail)
		}
		// Acá la salida NO es `trust --add`: sobre un registro con tokens ese
		// comando sale con error (ver trustAdd). Mandar a alguien a un rescate
		// que no puede funcionar es el error del issue #25 con otra ropa.
		_, alModelo := Explain(r)
		if !strings.Contains(alModelo, "pair --unbind cowork") {
			t.Errorf("el mensaje no nombra la salida del mecanismo que el usuario está usando:\n\n%s", alModelo)
		}
		if strings.Contains(alModelo, "trust --add") {
			t.Errorf("le ofrece `trust --add` a un registro con tokens, donde ese comando falla:\n\n%s", alModelo)
		}
	})
}

// EL CONTROL: avisar una vez no puede significar servir para siempre.
func TestElSegundoArranqueSiRechaza(t *testing.T) {
	t.Run("lanzador ajeno", func(t *testing.T) {
		dir := t.TempDir()
		if r, _ := Verify(dir, "", elCliente); r.Status != TrustedLauncher {
			t.Fatalf("preparación: %v", r.Status)
		}
		envejecerElRegistro(t, dir)

		r := trasElAviso(t, dir, "", elOtro)
		if r.Status != ForeignLauncher || r.Allowed() {
			t.Fatalf("segundo arranque: status = %v (allowed=%v), quería ForeignLauncher rechazado.\n"+
				"Un aviso que nunca se cumple es sólo un default más débil.", r.Status, r.Allowed())
		}
	})

	t.Run("token bindeado a otro programa", func(t *testing.T) {
		dir := t.TempDir()
		secreto, err := Mint(dir, "cowork", false)
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		confirm(t, dir, secreto, elCliente)

		r := trasElAviso(t, dir, secreto, elOtro)
		if r.Status != ForeignParent || r.Allowed() {
			t.Fatalf("segundo arranque: status = %v (allowed=%v), quería ForeignParent rechazado", r.Status, r.Allowed())
		}
	})
}

// EL OTRO CONTROL, y el que dice que la marca no es global: haber avisado sobre
// un programa no gasta el aviso de los demás.
//
// Sin esto, una implementación que guardara un solo booleano —«ya avisé
// alguna vez»— pasaría los dos tests de arriba y dejaría afuera al segundo
// programa sin avisarle nunca, que es exactamente el bug que se está cerrando.
func TestElAvisoDeUnProgramaNoGastaElDeOtro(t *testing.T) {
	dir := t.TempDir()
	if r, _ := Verify(dir, "", elCliente); r.Status != TrustedLauncher {
		t.Fatalf("preparación: %v", r.Status)
	}
	envejecerElRegistro(t, dir)

	// El primer desconocido gasta su aviso y su rechazo.
	if r := trasElAviso(t, dir, "", elOtro); r.Status != ForeignLauncher {
		t.Fatalf("preparación: %v", r.Status)
	}

	// Un tercer programa, sin relación con ninguno de los dos, empieza de cero.
	r, err := Verify(dir, "", `C:\Temp\tercero.exe`)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if r.Status != Announced || !r.Allowed() {
		t.Fatalf("status = %v (allowed=%v): un programa que nunca fue avisado quedó afuera "+
			"porque otro sí lo había sido", r.Status, r.Allowed())
	}
}

// La marca va por IDENTIDAD, no por ruta — que es la regla que costó el
// incidente entero (identity.go).
//
// Se avisa sobre la app vieja; la app se actualiza sola, cambia de ruta, y el
// segundo arranque tiene que reconocerla como la misma y rechazarla. Una marca
// guardada por ruta avisaría eternamente y no rechazaría nunca: cada
// actualización estrenaría aviso.
func TestLaMarcaSobreviveAQueElProgramaCambieDeRuta(t *testing.T) {
	dir := t.TempDir()
	if r, _ := Verify(dir, "", elCliente); r.Status != TrustedLauncher {
		t.Fatalf("preparación: %v", r.Status)
	}
	envejecerElRegistro(t, dir)

	if r, _ := Verify(dir, "", claudeVieja); r.Status != Announced {
		t.Fatalf("el aviso sobre la app vieja: %v", r.Status)
	}
	r, err := Verify(dir, "", claudeNueva)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if r.Status != ForeignLauncher {
		t.Fatalf("status = %v: la app actualizada estrenó un aviso nuevo, así que la marca está guardada "+
			"por ruta y nunca llega a rechazar nada", r.Status)
	}
}

// Punto 3 del encargo: confiar en un programa le borra la marca, y hay que
// poder demostrar que se fue de verdad.
//
// LA PRUEBA de que se fue no es leer el registro —eso mide la implementación—
// sino quitarlo de la lista otra vez: si la marca siguiera ahí, ese arranque
// rechazaría de una. Tiene que volver a avisar.
func TestConfiarEnUnProgramaLeBorraLaMarca(t *testing.T) {
	dir := t.TempDir()
	if r, _ := Verify(dir, "", elCliente); r.Status != TrustedLauncher {
		t.Fatalf("preparación: %v", r.Status)
	}
	envejecerElRegistro(t, dir)
	if r, _ := Verify(dir, "", elOtro); r.Status != Announced {
		t.Fatalf("el aviso: %v", r.Status)
	}

	// El usuario lo legitima, que es lo que `trust --add` hace por dentro.
	f, _, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !f.Trust(elOtro, false) {
		t.Fatal("Trust no agregó nada")
	}
	if f.WasAnnounced(IdentityOf(elOtro)) {
		t.Error("la marca sobrevivió a que el usuario aprobara el programa")
	}
	if err := Save(dir, f); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if r, _ := Verify(dir, "", elOtro); r.Status != TrustedLauncher {
		t.Fatalf("el programa aprobado no entró: %v", r.Status)
	}

	// Y ahora la prueba de fuego: se lo saca de la lista y tiene que volver a
	// avisar. Si la marca hubiera quedado guardada, esto rechazaría sin aviso a
	// un programa que el usuario aprobó a mano en el medio.
	f, _, err = Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !f.Untrust(elOtro) {
		t.Fatal("Untrust no quitó nada")
	}
	if err := Save(dir, f); err != nil {
		t.Fatalf("Save: %v", err)
	}
	r, err := Verify(dir, "", elOtro)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if r.Status != Announced || !r.Allowed() {
		t.Fatalf("status = %v (allowed=%v): la marca vieja rechazó a un programa que había sido aprobado "+
			"después de que se emitiera", r.Status, r.Allowed())
	}
}

// Lo mismo del lado del token: bindear al lanzador también retira su marca.
func TestBindearUnTokenTambienBorraLaMarca(t *testing.T) {
	dir := t.TempDir()
	secreto, err := Mint(dir, "cowork", false)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	confirm(t, dir, secreto, elCliente)

	if r, _ := Verify(dir, secreto, elOtro); r.Status != Announced {
		t.Fatalf("el aviso: %v", r.Status)
	}
	// El usuario reconoce el cambio y desata el token, que es la salida que el
	// propio mensaje le dio.
	if ok, err := Unbind(dir, "cowork"); err != nil || !ok {
		t.Fatalf("Unbind: ok=%v err=%v", ok, err)
	}
	if r, _ := Verify(dir, secreto, elOtro); r.Status != OK {
		t.Fatalf("tras el unbind el lanzador no quedó bindeado: %v", r.Status)
	}

	f, _, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if f.WasAnnounced(IdentityOf(elOtro)) {
		t.Error("el programa quedó bindeado al token y sigue marcado; el próximo --unbind lo echaría sin avisar")
	}
}

// Un registro escrito antes de este cambio no trae marcas, y eso es exactamente
// la migración que hace falta: todo lo que ese registro rechazaría hoy recibe su
// aviso una vez, en vez de quedar bloqueado por una notificación que nunca
// existió.
//
// El JSON está escrito a mano, sin el campo, porque es lo único que prueba que
// un archivo viejo se lee. Serializar un File de este build metería el campo
// vacío y no probaría nada.
func TestUnRegistroViejoSeLeeYRecibeSuAviso(t *testing.T) {
	dir := t.TempDir()
	viejo := `{
  "version": 1,
  "tokens": [],
  "trusted_launchers": [
    {"exe": "C:\\Tools\\claude.exe", "recorded": "2020-01-01T00:00:00Z", "first_use": true}
  ]
}`
	if err := os.WriteFile(Path(dir), []byte(viejo), 0o600); err != nil {
		t.Fatalf("escribir el registro viejo: %v", err)
	}

	f, existe, err := Load(dir)
	if err != nil || !existe {
		t.Fatalf("un registro escrito antes del cambio dejó de cargarse: existe=%v err=%v", existe, err)
	}
	if !f.Trusts(elCliente) {
		t.Fatalf("se perdió la lista de lanzadores al leerlo: %+v", f.TrustedLaunchers)
	}
	if f.WasAnnounced(IdentityOf(elOtro)) {
		t.Error("un registro sin marcas dice tener una")
	}

	r, err := Verify(dir, "", elOtro)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if r.Status != Announced || !r.Allowed() {
		t.Fatalf("status = %v (allowed=%v): un registro viejo bloqueó sin avisar, que es el caso que "+
			"este cambio existe para cerrar", r.Status, r.Allowed())
	}
}

// El campo no aparece cuando no hay nada que guardar. Otros builds leen este
// archivo, y una llave nueva en todo registro es ruido que alguien va a tener
// que explicar.
func TestSinMarcasElCampoNoSeEscribe(t *testing.T) {
	dir := t.TempDir()
	if r, _ := Verify(dir, "", elCliente); r.Status != TrustedLauncher {
		t.Fatalf("preparación: %v", r.Status)
	}
	buf, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatalf("leer el registro: %v", err)
	}
	if strings.Contains(string(buf), "announced_launchers") {
		t.Errorf("el registro trae el campo vacío:\n%s", buf)
	}

	// EL CONTROL de la línea de arriba: cuando SÍ hay una marca tiene que
	// escribirse, o el test anterior estaría celebrando un campo que no
	// funciona.
	envejecerElRegistro(t, dir)
	if r, _ := Verify(dir, "", elOtro); r.Status != Announced {
		t.Fatalf("el aviso: %v", r.Status)
	}
	buf, err = os.ReadFile(Path(dir))
	if err != nil {
		t.Fatalf("leer el registro: %v", err)
	}
	if !strings.Contains(string(buf), "announced_launchers") {
		t.Fatalf("el aviso no se persistió, así que el próximo arranque lo vuelve a avisar y nunca rechaza:\n%s", buf)
	}
	// Y por identidad, no por ruta: lo que se guarda es con qué se va a
	// comparar mañana.
	var leido File
	if err := json.Unmarshal(buf, &leido); err != nil {
		t.Fatalf("el registro quedó ilegible: %v", err)
	}
	if len(leido.AnnouncedLaunchers) != 1 {
		t.Fatalf("marcas = %+v, quería una", leido.AnnouncedLaunchers)
	}
	if leido.AnnouncedLaunchers[0].ID.Kind == "" || leido.AnnouncedLaunchers[0].ID.Value == "" {
		t.Errorf("la marca se guardó sin identidad con qué comparar: %+v", leido.AnnouncedLaunchers[0])
	}
	if leido.AnnouncedLaunchers[0].Reason != StatusName(ForeignLauncher) {
		t.Errorf("la marca no dice qué rechazo retuvo (%q); es la única evidencia de que se avisó",
			leido.AnnouncedLaunchers[0].Reason)
	}
}

// Que no se pueda escribir el registro NO puede convertirse en un rechazo. Es la
// misma disciplina que Verify tiene desde el binding del token: se sirve, se
// devuelve el error, y el que llama lo loguea.
//
// El truco para romper la escritura es ocupar el archivo temporal con un
// directorio, que es portable y no toca permisos.
func TestSiNoSePuedeEscribirElRegistroSeSirveIgual(t *testing.T) {
	dir := t.TempDir()
	if r, _ := Verify(dir, "", elCliente); r.Status != TrustedLauncher {
		t.Fatalf("preparación: %v", r.Status)
	}
	envejecerElRegistro(t, dir)
	if err := os.Mkdir(Path(dir)+".tmp", 0o700); err != nil {
		t.Fatalf("bloquear la escritura: %v", err)
	}

	r, err := Verify(dir, "", elOtro)
	if err == nil {
		t.Error("la escritura falló y Verify no lo reportó; nadie lo va a loguear")
	}
	if r.Status != Announced || !r.Allowed() {
		t.Fatalf("status = %v (allowed=%v): un fallo de escritura terminó en un rechazo", r.Status, r.Allowed())
	}

	// Y como la marca no llegó a disco, el arranque siguiente vuelve a avisar en
	// vez de rechazar: el error cae del lado de que el usuario conserve su
	// cliente.
	r2, _ := Verify(dir, "", elOtro)
	if r2.Status != Announced || !r2.Allowed() {
		t.Fatalf("status = %v (allowed=%v): se rechazó apoyándose en una marca que nunca se guardó", r2.Status, r2.Allowed())
	}
}
