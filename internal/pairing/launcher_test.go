// SPDX-License-Identifier: Apache-2.0

package pairing

import (
	"strings"
	"testing"
	"time"
)

// envejecerElRegistro empuja la entrada más vieja fuera de la ventana de
// aprendizaje, que es lo único que hace falta para que el registro deje de
// adoptar lanzadores y empiece a rechazarlos.
func envejecerElRegistro(t *testing.T, dir string) {
	t.Helper()
	f, _, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for i := range f.TrustedLaunchers {
		f.TrustedLaunchers[i].Recorded = time.Now().UTC().Add(-2 * learningWindow)
	}
	if err := Save(dir, f); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

const (
	elCliente = `C:\Tools\claude.exe`
	elOtro    = `C:\Windows\System32\cmd.exe`
)

// El caso que motiva todo #27: el usuario no hace NADA y queda protegido.
//
// Cinco rondas de arreglos sobre el token terminaron con alguien que siguió
// cada paso y nunca logró que el pairing se activara. Acá no hay pasos: el
// servidor arranca, anota quién lo lanzó, y de ahí en más contesta sólo a ése.
func TestElPrimerLanzadorQuedaRegistradoSinQueNadieHagaNada(t *testing.T) {
	dir := t.TempDir()

	r, err := Verify(dir, "", elCliente)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if r.Status != TrustedLauncher {
		t.Fatalf("primer arranque: status = %v (%s), quería TrustedLauncher", r.Status, r.Detail)
	}
	if !r.Allowed() {
		t.Fatal("el primer arranque no fue servido; nadie podría usar el servidor jamás")
	}

	// Y quedó escrito, que es lo que lo hace valer para el arranque siguiente.
	f, existe, err := Load(dir)
	if err != nil || !existe {
		t.Fatalf("no se persistió el registro: existe=%v err=%v", existe, err)
	}
	if !f.Trusts(elCliente) {
		t.Errorf("el registro no confía en %s: %+v", elCliente, f.TrustedLaunchers)
	}
	if len(f.TrustedLaunchers) != 1 || !f.TrustedLaunchers[0].FirstUse {
		t.Errorf("quería un solo lanzador marcado FirstUse, hay %+v", f.TrustedLaunchers)
	}

	// El mismo programa entra otra vez.
	if r, _ := Verify(dir, "", elCliente); r.Status != TrustedLauncher || !r.Allowed() {
		t.Errorf("el lanzador registrado fue rechazado en el segundo arranque: %v", r.Status)
	}
}

// EL CONTROL de lo de arriba, y sin él aquel test no prueba nada: un Verify que
// sirviera siempre lo pasaría igual.
//
// Hay que envejecer el registro antes de probar el rechazo, y esa necesidad es
// en sí misma información: durante el primer día el servidor ADOPTA a quien
// aparezca (ver learningWindow), así que un control escrito sin envejecerlo
// mediría la ventana en vez del rechazo. Éste falló exactamente así cuando se
// agregó la ventana, que es la señal de que estaba midiendo lo que decía.
//
// Y pasa por trasElAviso por la misma razón, una capa más tarde: el rechazo
// llega recién en el SEGUNDO arranque, porque el primero avisa (announce.go).
func TestOtroProgramaNoEntra(t *testing.T) {
	dir := t.TempDir()
	if r, _ := Verify(dir, "", elCliente); r.Status != TrustedLauncher {
		t.Fatalf("preparación: %v", r.Status)
	}
	envejecerElRegistro(t, dir)

	r := trasElAviso(t, dir, "", elOtro)
	if r.Status != ForeignLauncher {
		t.Fatalf("status = %v, quería ForeignLauncher — otro programa acaba de usar el servidor", r.Status)
	}
	if r.Allowed() {
		t.Fatal("el servidor sirvió a un programa que no lo configuró; eso es el agujero entero")
	}

	// El rechazo tiene que decir contra qué comparó y cómo salir. Quien lo lee
	// no tiene cliente funcionando.
	for _, quiero := range []string{elCliente, elOtro, "trust --add", "trust --reset"} {
		if !strings.Contains(r.Detail, quiero) {
			t.Errorf("el detalle no menciona %q, así que el usuario no sabe qué pasó ni cómo arreglarlo:\n%s", quiero, r.Detail)
		}
	}
	_, alModelo := Explain(r)
	if !strings.Contains(alModelo, "trust --add") {
		t.Errorf("el mensaje al modelo no nombra la salida:\n%s", alModelo)
	}
}

// Un registro con tokens sigue exigiendo el token. Cambiar el default no puede
// degradar en silencio a quien se tomó el trabajo de emitir una credencial.
func TestUnRegistroConTokensNoSeDegrada(t *testing.T) {
	dir := t.TempDir()
	secreto, err := Mint(dir, "claude-desktop", false)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	confirm(t, dir, secreto, elCliente)

	// Sin el token no entra, aunque el lanzador sea siempre el mismo.
	r, err := Verify(dir, "", elCliente)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if r.Status == TrustedLauncher || r.Allowed() {
		t.Fatalf("un registro con token pasó a confiar en el lanzador (status=%v); eso rebaja la seguridad "+
			"de una instalación que ya estaba configurada, en silencio", r.Status)
	}
}

// Una plataforma que no puede nombrar al padre no puede aplicar esto, y dejar
// afuera a alguien de su propio servidor por un dato que el sistema operativo
// no da sería el peor de los dos errores posibles.
func TestSinPadreResolubleSirveComoAntes(t *testing.T) {
	dir := t.TempDir()

	r, err := Verify(dir, "", "")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if r.Status != Unpaired || !r.Allowed() {
		t.Fatalf("status = %v, quería Unpaired servido", r.Status)
	}
	// Y no dejó un registro a medio hacer que después rechace a todo el mundo.
	if _, existe, _ := Load(dir); existe {
		t.Error("se escribió un registro sin saber en quién confiar; el próximo arranque quedaría bloqueado")
	}
}

// Añadir y quitar, que es todo lo que `trust` hace por dentro.
func TestAgregarYQuitarLanzadores(t *testing.T) {
	f := &File{Version: fileVersion}

	if !f.Trust(elCliente, true) {
		t.Fatal("Trust devolvió false sobre una lista vacía")
	}
	if f.Trust(elCliente, false) {
		t.Error("Trust agregó dos veces el mismo programa")
	}
	if !f.Trust(elOtro, false) {
		t.Error("no se pudo agregar un segundo lanzador")
	}
	if !f.TrustsLaunchers() {
		t.Error("TrustsLaunchers dice que no, con dos lanzadores y ningún token")
	}

	// Con un token presente manda el token, aunque haya lanzadores.
	f.Tokens = append(f.Tokens, Token{Label: "x"})
	if f.TrustsLaunchers() {
		t.Error("TrustsLaunchers dice que sí habiendo un token; eso degradaría la instalación")
	}
	f.Tokens = nil

	if !f.Untrust(elOtro) {
		t.Error("Untrust no encontró un programa que estaba")
	}
	if f.Trusts(elOtro) {
		t.Error("el programa sigue confiado después de quitarlo")
	}
	if !f.Trusts(elCliente) {
		t.Error("quitar uno se llevó puesto al otro")
	}
	if f.Untrust(`C:\nada.exe`) {
		t.Error("Untrust dijo haber quitado algo que no estaba")
	}
}

// The learning window is the one thing about a launcher record that changes on
// its own, and `trust --status` now reports it. So it needs an end somebody can
// be told about, and that end has to actually arrive.
func TestTheLearningWindowHasADeadlineAndItArrives(t *testing.T) {
	f := &File{Version: fileVersion}
	if !f.Trust("/opt/cliente", true) {
		t.Fatal("Trust recorded nothing")
	}
	recorded := f.TrustedLaunchers[0].Recorded

	closes, learning := f.LearningUntil(recorded)
	if !learning {
		t.Fatal("a record written just now is already done learning")
	}
	if got := closes.Sub(recorded); got != learningWindow {
		t.Fatalf("window = %v, want %v", got, learningWindow)
	}
	if _, learning := f.LearningUntil(closes); learning {
		t.Fatal("the window is still open at the very moment it should shut")
	}
}

// A launcher-mode user walks into the empty record one way: trusted on first
// launch, then that single launcher removed. The refusal they get has to name
// the way back for the mechanism they were actually using.
//
// It answered with NoToken before, whose message tells them to run
// `pair --install` and put a token where their client will read it — a secret
// they never held, for a mechanism they never used. That is the mistake of
// issue #25 wearing different clothes: the remedy points at the wrong thing.
func TestAnEmptyRecordDoesNotSendYouToFixATokenYouNeverHad(t *testing.T) {
	dir := t.TempDir()
	const exe = "/opt/cliente"

	if res, err := Verify(dir, "", exe); err != nil || res.Status != TrustedLauncher {
		t.Fatalf("first launch: status=%v err=%v", res.Status, err)
	}
	f, _, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !f.Untrust(exe) {
		t.Fatal("Untrust removed nothing")
	}
	if err := Save(dir, f); err != nil {
		t.Fatalf("Save: %v", err)
	}

	res, _ := Verify(dir, "", exe)
	if res.Allowed() {
		t.Fatalf("status = %v: a record that trusts nothing served anyway", res.Status)
	}
	if res.Status != EmptyRecord {
		t.Fatalf("status = %v, want EmptyRecord", res.Status)
	}
	stderr, client := Explain(res)
	for _, msg := range []string{stderr, client} {
		if !strings.Contains(msg, "trust --reset") {
			t.Errorf("the way back is missing: %q", msg)
		}
	}
}
