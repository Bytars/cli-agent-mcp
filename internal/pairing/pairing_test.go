// SPDX-License-Identifier: Apache-2.0

package pairing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// confirm walks a record through the launch that turns pairing on: a launcher
// presents a valid token, and enforcement is permanent from then on.
//
// Tests about refusing anything need it. A freshly minted record does not
// refuse — it serves until a token is seen to arrive (see Status Armed) — so
// "paired" and "enforcing" are two states these tests have to step through
// rather than assume.
func confirm(t *testing.T, dir, secret, launcher string) {
	t.Helper()
	r, err := Verify(dir, secret, launcher)
	if err != nil {
		t.Fatalf("confirming the pairing: %v", err)
	}
	if r.Status != OK {
		t.Fatalf("confirming the pairing gave %v (%s), want OK", r.Status, r.Detail)
	}
}

// An install that predates pairing must keep working. Bricking a working setup
// on upgrade would be a worse outcome than the exposure this feature closes.
//
// The verdict changed with issue #27 — a fresh record now records its launcher
// instead of serving anonymously for ever — but the promise this test exists to
// hold did not: whatever was working before the upgrade still works after it.
// So it asserts Allowed(), which is the promise, rather than Unpaired, which
// was only ever how the promise happened to be kept.
func TestUnpairedServesAsBefore(t *testing.T) {
	dir := t.TempDir()

	res, err := Verify(dir, "", "/usr/bin/whatever")
	if err != nil {
		t.Fatalf("Verify on an unpaired dir: %v", err)
	}
	if !res.Allowed() {
		t.Fatalf("an unpaired server refused to serve (status %v); that breaks every existing install on upgrade", res.Status)
	}
}

// The case the test above caught when #27 first refused everyone after the
// first launcher: an install where TWO programs start the server. Whoever got
// there first would have won, and the other would have stopped working on
// upgrade — precisely the outcome that test was written to prevent.
func TestDosProgramasQueYaFuncionabanSiguenFuncionando(t *testing.T) {
	dir := t.TempDir()
	const cliente = `C:\Tools\claude.exe`
	const editor = `C:\Tools\algun-editor.exe`

	if r, _ := Verify(dir, "", cliente); !r.Allowed() {
		t.Fatalf("el primer programa fue rechazado: %v", r.Status)
	}
	r, err := Verify(dir, "", editor)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !r.Allowed() {
		t.Fatalf("el segundo programa que ya usaba este servidor quedó afuera al actualizar (status %v).\n"+
			"Eso es romper una instalación que funcionaba, que es peor que la exposición que esto cierra.", r.Status)
	}

	f, _, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !f.Trusts(cliente) || !f.Trusts(editor) {
		t.Errorf("no quedaron los dos en la lista: %+v", f.TrustedLaunchers)
	}
}

// Y su control: pasada la ventana, la lista se cierra. Sin esto, "sigue
// aprendiendo" sería indistinguible de "nunca aprende", y el mecanismo no
// protegería de nada.
func TestPasadaLaVentanaNoSeAdoptaNadieMas(t *testing.T) {
	dir := t.TempDir()
	const cliente = `C:\Tools\claude.exe`
	const intruso = `C:\Temp\algo-raro.exe`

	if r, _ := Verify(dir, "", cliente); r.Status != TrustedLauncher {
		t.Fatalf("preparación: %v", r.Status)
	}

	// Envejecer el registro es lo único que hace falta: la ventana se mide
	// desde la entrada más vieja.
	f, _, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	f.TrustedLaunchers[0].Recorded = time.Now().UTC().Add(-2 * learningWindow)
	if err := Save(dir, f); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// El rechazo cae en el segundo arranque: al primero se lo avisa y se lo
	// sirve (announce.go). Lo que este test mide sigue siendo la ventana —que
	// el intruso termine afuera— y trasElAviso afirma la otra mitad, que se lo
	// dijimos antes.
	r := trasElAviso(t, dir, "", intruso)
	if r.Status != ForeignLauncher || r.Allowed() {
		t.Fatalf("status = %v (allowed=%v) sobre un registro viejo: la ventana no cierra nunca, "+
			"así que cualquier programa entra siempre", r.Status, r.Allowed())
	}
}

func TestMintThenVerify(t *testing.T) {
	dir := t.TempDir()

	secret, err := Mint(dir, "claude-desktop", false)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !strings.HasPrefix(secret, tokenPrefix) {
		t.Errorf("secret %q does not carry the %q prefix", secret, tokenPrefix)
	}

	res, err := Verify(dir, secret, "/opt/Claude/claude")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Status != OK || res.Label != "claude-desktop" {
		t.Fatalf("got %v/%q, want OK/claude-desktop", res.Status, res.Label)
	}
}

// The secret must never reach the disk. If it did, anything that can read the
// state directory could authenticate, which is the whole thing this is meant to
// prevent.
func TestSecretIsNeverStored(t *testing.T) {
	dir := t.TempDir()

	secret, err := Mint(dir, "claude-desktop", false)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	buf, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	if strings.Contains(string(buf), secret) {
		t.Fatal("the pairing record contains the plaintext secret")
	}
}

func TestPairedRejectsMissingAndWrongTokens(t *testing.T) {
	dir := t.TempDir()
	secret, err := Mint(dir, "claude-desktop", false)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	confirm(t, dir, secret, "/opt/Claude/claude")

	for _, tc := range []struct {
		name   string
		secret string
		want   Status
	}{
		{"nothing presented", "", NoToken},
		{"a token from somewhere else", tokenPrefix + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", BadToken},
		{"not a token at all", "hunter2", BadToken},
		{"the right secret truncated", secret[:len(secret)-1], BadToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Verify(dir, tc.secret, "/opt/Claude/claude")
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if res.Status != tc.want {
				t.Fatalf("status = %v, want %v", res.Status, tc.want)
			}
			if res.Allowed() {
				t.Fatal("server would have served this launcher")
			}
			if res.Detail == "" {
				t.Error("no detail to show the user; they would see a silent failure")
			}
		})
	}
}

// Tokens are per-client so that revoking one leaves the other alone. That is
// what makes "Claude Desktop and Cowork hold separate credentials" meaningful.
func TestRevokeIsPerLabel(t *testing.T) {
	dir := t.TempDir()
	desktop, err := Mint(dir, "claude-desktop", false)
	if err != nil {
		t.Fatalf("Mint desktop: %v", err)
	}
	cowork, err := Mint(dir, "cowork", false)
	if err != nil {
		t.Fatalf("Mint cowork: %v", err)
	}
	confirm(t, dir, desktop, "")

	if ok, err := Revoke(dir, "cowork"); err != nil || !ok {
		t.Fatalf("Revoke: ok=%v err=%v", ok, err)
	}

	if res, _ := Verify(dir, cowork, ""); res.Status != BadToken {
		t.Errorf("revoked token still gives %v", res.Status)
	}
	if res, _ := Verify(dir, desktop, ""); res.Status != OK {
		t.Errorf("revoking one token broke the other: %v", res.Status)
	}
}

// Re-minting under a label replaces the old secret rather than accumulating a
// second live one, so `pair` doubles as rotation.
func TestMintRotatesInPlace(t *testing.T) {
	dir := t.TempDir()
	old, err := Mint(dir, "claude-desktop", false)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	confirm(t, dir, old, "")
	fresh, err := Mint(dir, "claude-desktop", false)
	if err != nil {
		t.Fatalf("re-Mint: %v", err)
	}

	f, _, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Tokens) != 1 {
		t.Fatalf("%d tokens after rotation, want 1", len(f.Tokens))
	}
	if res, _ := Verify(dir, old, ""); res.Status != BadToken {
		t.Errorf("the rotated-out secret still works: %v", res.Status)
	}
	if res, _ := Verify(dir, fresh, ""); res.Status != OK {
		t.Errorf("the new secret does not: %v", res.Status)
	}
}

// The point of binding: a secret lifted from the client's config does not let
// some other program on the machine drive the server.
func TestBindingRejectsAnotherLauncher(t *testing.T) {
	dir := t.TempDir()
	secret, err := Mint(dir, "claude-desktop", false)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	if res, _ := Verify(dir, secret, "/opt/Claude/claude"); res.Status != OK {
		t.Fatalf("first use: %v", res.Status)
	}
	// Same secret, different program. The rejection lands on that program's
	// SECOND start; the first is served with a warning, because nothing here is
	// allowed to block without having announced it (announce.go). trasElAviso
	// asserts that warning happened, so this test still measures the binding
	// and now also pins the order.
	res := trasElAviso(t, dir, secret, "/tmp/definitely-not-claude")
	if res.Status != ForeignParent {
		t.Fatalf("status = %v, want ForeignParent — a stolen token just worked", res.Status)
	}
	if res.Allowed() {
		t.Fatal("server would have served a launcher it is not bound to")
	}
	if !strings.Contains(res.Detail, "--unbind") {
		t.Error("the rejection does not tell the user how to recover from a legitimate reinstall")
	}

	// And the bound launcher still works afterwards.
	if res, _ := Verify(dir, secret, "/opt/Claude/claude"); res.Status != OK {
		t.Fatalf("the real launcher was locked out too: %v", res.Status)
	}
}

func TestUnbindAllowsANewLauncher(t *testing.T) {
	dir := t.TempDir()
	secret, _ := Mint(dir, "claude-desktop", false)
	Verify(dir, secret, "/opt/Claude/claude")

	if ok, err := Unbind(dir, "claude-desktop"); err != nil || !ok {
		t.Fatalf("Unbind: ok=%v err=%v", ok, err)
	}
	if res, _ := Verify(dir, secret, "/new/location/claude"); res.Status != OK {
		t.Fatalf("status = %v after unbind, want OK", res.Status)
	}
}

// A platform that cannot name the parent must not lock the user out of their
// own server: binding is a hardening layer, not a prerequisite.
func TestUnknownParentDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	secret, _ := Mint(dir, "claude-desktop", false)
	if _, err := Verify(dir, secret, "/opt/Claude/claude"); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	res, _ := Verify(dir, secret, "")
	if res.Status != OK {
		t.Fatalf("status = %v with an unknown parent, want OK", res.Status)
	}
}

func TestNoBindSkipsBinding(t *testing.T) {
	dir := t.TempDir()
	secret, err := Mint(dir, "scripted", true)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if res, _ := Verify(dir, secret, "/one/launcher"); res.Status != OK {
		t.Fatalf("first launcher: %v", res.Status)
	}
	if res, _ := Verify(dir, secret, "/another/launcher"); res.Status != OK {
		t.Fatalf("status = %v, want OK — --no-bind should not record a launcher", res.Status)
	}
}

// A record left holding no tokens can authenticate nobody. Serving openly there
// would silently undo pairing, so it locks instead.
func TestEmptyRecordLocks(t *testing.T) {
	dir := t.TempDir()
	secret, _ := Mint(dir, "claude-desktop", false)
	if _, err := Revoke(dir, "claude-desktop"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	res, _ := Verify(dir, secret, "")
	if res.Allowed() {
		t.Fatalf("status = %v: revoking the last token reopened the server", res.Status)
	}
}

// A truncated or hand-edited record must not be read as "unpaired" — that would
// turn a corrupt file into an open server.
// A record nobody can read must not cost the user their server.
//
// This asserted the opposite until issue #29, and the reversal is the point
// rather than a relaxation. Measured against the binary: a record that is
// DELETED gets its deleter served and recorded as the trusted launcher, while a
// record that was CORRUPT refused everyone. So the refusal never stopped an
// attacker who can write this file — deleting it is strictly the better move
// for them — and it did hand them a one-line way to leave the user with no MCP.
// Three bytes of UTF-8 BOM, which PowerShell's Out-File writes by default, do
// it by accident.
//
// Serving is not the whole answer, so this pins the shouting too. A server that
// quietly ignored the record would be worse than either.
func TestUnRegistroIlegibleSirveYGrita(t *testing.T) {
	sano := func(t *testing.T, dir string) []byte {
		t.Helper()
		if _, err := Mint(dir, "claude-desktop", false); err != nil {
			t.Fatalf("Mint: %v", err)
		}
		buf, err := os.ReadFile(Path(dir))
		if err != nil {
			t.Fatalf("read record: %v", err)
		}
		return buf
	}
	casos := []struct {
		nombre string
		romper func(t *testing.T, dir string) []byte
	}{
		{"json roto", func(t *testing.T, dir string) []byte {
			sano(t, dir)
			return []byte("{ not json")
		}},
		{"BOM UTF-8 delante de un registro valido", func(t *testing.T, dir string) []byte {
			// The accident, not the attack: Out-File writes this by default.
			return append([]byte{0xEF, 0xBB, 0xBF}, sano(t, dir)...)
		}},
		{"formato de una version futura", func(t *testing.T, dir string) []byte {
			sano(t, dir)
			buf, _ := json.Marshal(File{Version: fileVersion + 1})
			return buf
		}},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			dir := t.TempDir()
			roto := c.romper(t, dir)
			if err := os.WriteFile(Path(dir), roto, 0o600); err != nil {
				t.Fatalf("write record: %v", err)
			}

			res, err := Verify(dir, "anything", `C:\\algun\\cliente.exe`)
			if !res.Allowed() {
				t.Fatalf("status = %v: an unreadable record took the server away from the user", res.Status)
			}
			if res.Status != UnreadableRecord {
				t.Fatalf("status = %v, want UnreadableRecord", res.Status)
			}
			// The shouting, in every channel a person or a model can read.
			if err == nil {
				t.Error("the caller got no error to log")
			}
			stderr, client := Explain(res)
			if stderr == "" || client == "" {
				t.Errorf("silently ignored: stderr=%q client=%q", stderr, client)
			}
			if linea := StartupLine(dir, res); !strings.Contains(linea, "SERVING") {
				t.Errorf("the startup line does not say it is serving anyway: %q", linea)
			}
			// And the record is left alone, so repairing the file brings back
			// the pairing the user actually wrote.
			ahora, err := os.ReadFile(Path(dir))
			if err != nil {
				t.Fatalf("re-read record: %v", err)
			}
			if string(ahora) != string(roto) {
				t.Fatal("the unreadable record was overwritten; repairing it can no longer bring the pairing back")
			}
		})
	}
}

// The control for the test above: a record this build CAN read still decides.
// Without it, "serve when unreadable" is indistinguishable from "always serve".
func TestUnRegistroLegibleSigueMandando(t *testing.T) {
	dir := t.TempDir()
	secret, err := Mint(dir, "claude-desktop", false)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	confirm(t, dir, secret, "")

	res, _ := Verify(dir, "el-secreto-equivocado", "")
	if res.Allowed() {
		t.Fatalf("status = %v: a readable record accepted a wrong token", res.Status)
	}
	if res.Status == UnreadableRecord {
		t.Fatal("a perfectly readable record was treated as unreadable")
	}
}

func TestSaveIsOwnerOnly(t *testing.T) {
	if os.Getuid() == -1 {
		t.Skip("Windows does not apply the mode bits")
	}
	dir := t.TempDir()
	if _, err := Mint(dir, "claude-desktop", false); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	fi, err := os.Stat(Path(dir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("record mode is %o; it is readable beyond its owner", perm)
	}
	// No temp file left behind holding the same content.
	if _, err := os.Stat(Path(dir) + ".tmp"); !os.IsNotExist(err) {
		t.Error("the write left its temporary file in place")
	}
}

func TestMintRequiresALabel(t *testing.T) {
	if _, err := Mint(t.TempDir(), "   ", false); err == nil {
		t.Fatal("minted a token with a blank label")
	}
}

func TestInstallTokenPreservesOtherServers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude_desktop_config.json")
	existing := `{
  "mcpServers": {
    "github": {"command": "github-mcp-server", "args": ["stdio"]}
  },
  "someOtherSetting": {"kept": true}
}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if _, _, err := InstallToken(path, "/usr/local/bin/cli-agent-mcp", "cam1_secret"); err != nil {
		t.Fatalf("InstallToken: %v", err)
	}

	var root map[string]any
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if err := json.Unmarshal(buf, &root); err != nil {
		t.Fatalf("the rewritten config is not valid JSON: %v", err)
	}
	servers, _ := root["mcpServers"].(map[string]any)
	if _, ok := servers["github"]; !ok {
		t.Error("another server's entry was dropped")
	}
	if _, ok := root["someOtherSetting"]; !ok {
		t.Error("an unrelated top-level setting was dropped")
	}
	entry, _ := servers[ServerKey].(map[string]any)
	env, _ := entry["env"].(map[string]any)
	if env[EnvVar] != "cam1_secret" {
		t.Errorf("token not written: %v", env[EnvVar])
	}
	if entry["command"] != "/usr/local/bin/cli-agent-mcp" {
		t.Errorf("command = %v", entry["command"])
	}
}

// Re-pairing an install that already points at a launcher script must not
// rewrite that choice out from under the user.
func TestInstallTokenKeepsAnExistingCommand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	seed := `{"mcpServers": {"cli-agent-mcp": {"command": "/opt/wrapper.sh", "env": {"CLI_AGENT_MCP_DEFAULT_AGENT": "cursor"}}}}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if _, _, err := InstallToken(path, "/usr/local/bin/cli-agent-mcp", "cam1_new"); err != nil {
		t.Fatalf("InstallToken: %v", err)
	}

	var root map[string]any
	buf, _ := os.ReadFile(path)
	if err := json.Unmarshal(buf, &root); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	entry := root["mcpServers"].(map[string]any)[ServerKey].(map[string]any)
	if entry["command"] != "/opt/wrapper.sh" {
		t.Errorf("command = %v, want the wrapper the user configured", entry["command"])
	}
	env := entry["env"].(map[string]any)
	if env["CLI_AGENT_MCP_DEFAULT_AGENT"] != "cursor" {
		t.Error("an existing env entry was dropped")
	}
	if env[EnvVar] != "cam1_new" {
		t.Errorf("token = %v", env[EnvVar])
	}
}

// Refusing to touch a config we cannot parse: overwriting it would destroy
// every other server the user has configured.
func TestInstallTokenRefusesBrokenConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	broken := []byte(`{"mcpServers": {`)
	if err := os.WriteFile(path, broken, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, _, err := InstallToken(path, "exe", "cam1_x"); err == nil {
		t.Fatal("overwrote a config that could not be parsed")
	}
	buf, _ := os.ReadFile(path)
	if string(buf) != string(broken) {
		t.Error("the unparseable config was modified anyway")
	}
}

func TestInstallTokenCreatesAMissingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "cfg.json")
	_, created, err := InstallToken(path, "exe", "cam1_x")
	if err != nil {
		t.Fatalf("InstallToken: %v", err)
	}
	var root map[string]any
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := json.Unmarshal(buf, &root); err != nil {
		t.Fatalf("wrote invalid JSON: %v", err)
	}

	// Y tiene que DECIR que lo creó (issue #25). Escribir bien un archivo que el
	// cliente no lee es el modo de falla que dejó una máquina sin MCP: la
	// escritura funcionó, el mensaje se leyó como éxito, y el enforcement quedó
	// encendido sin nadie que pudiera presentar el token.
	if !created {
		t.Error("created = false sobre un archivo que no existía.\n" +
			"Ese booleano es lo único que distingue «actualicé tu configuración» de\n" +
			"«inventé una configuración que quizá nadie lea», y el segundo caso necesita\n" +
			"una advertencia, no una felicitación.")
	}
}

// TestInstallTokenNoMienteSobreUnConfigQueYaExistia es la otra mitad del control
// de #25: si `created` fuera true siempre, la advertencia aparecería también en
// el caso bueno y se volvería ruido que nadie lee.
func TestInstallTokenNoMienteSobreUnConfigQueYaExistia(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"otro":{"command":"x"}}}`), 0o600); err != nil {
		t.Fatalf("preparar: %v", err)
	}
	_, created, err := InstallToken(path, "exe", "cam1_x")
	if err != nil {
		t.Fatalf("InstallToken: %v", err)
	}
	if created {
		t.Error("created = true sobre un archivo que YA existía; la advertencia saldría cuando no corresponde")
	}
}

// TestElTokenDelEntornoSirve prueba el camino que queda cuando el cliente no
// tiene archivo de configuración: presentar el secreto por el entorno.
//
// Es la vía que se ofrece en `printEnvFallback`, y ofrecer algo sin probar que
// funciona es exactamente cómo se llega a un usuario sin MCP (issue #25). Acá se
// prueba el mecanismo entero: se paira, se presenta el secreto tal cual lo
// presentaría el entorno, y el veredicto tiene que ser OK.
func TestElTokenDelEntornoSirve(t *testing.T) {
	dir := t.TempDir()
	secreto, err := Mint(dir, "cowork", false)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// El lanzador es el mismo en los dos arranques, que es la situación real:
	// una vez que el binding se registra, sólo ese programa sirve.
	const lanzador = `C:\Tools\algun-cliente.exe`

	r, err := Verify(dir, secreto, lanzador)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if r.Status != OK {
		t.Fatalf("con el secreto correcto el estado es %v (%s), quería OK.\n"+
			"Si esto falla, la salida por variable de entorno que `pair` ofrece no sirve\n"+
			"para nada y el usuario se queda sin cliente.", r.Status, r.Detail)
	}
	if r.Label != "cowork" {
		t.Errorf("label = %q, quería \"cowork\"", r.Label)
	}

	// Y el control de la afirmación: sin el secreto, el mismo montaje rechaza.
	// Sin esta mitad, un Verify que dijera OK siempre pasaría la prueba de
	// arriba.
	sin, err := Verify(dir, "", lanzador)
	if err != nil {
		t.Fatalf("Verify sin secreto: %v", err)
	}
	if sin.Status != NoToken {
		t.Errorf("sin secreto el estado es %v, quería NoToken; el gate no está discriminando", sin.Status)
	}

	// Y desde OTRO lanzador, con el secreto correcto: tiene que rechazar. Es lo
	// que hace que la variable de entorno siga valiendo algo pese a ser legible
	// por cualquier proceso del usuario.
	// En el segundo arranque de ese otro lanzador: al primero se lo avisa y se
	// lo sirve (announce.go), y trasElAviso afirma esa mitad.
	otro := trasElAviso(t, dir, secreto, `C:\Windows\System32\cmd.exe`)
	if otro.Status != ForeignParent {
		t.Errorf("desde otro lanzador el estado es %v, quería ForeignParent.\n"+
			"Éste es el argumento por el que la variable de entorno es aceptable: aunque el\n"+
			"secreto se lea, sigue atado al programa que lo usó primero. Si esto no se cumple,\n"+
			"ofrecer el entorno es entregar el pairing entero.", otro.Status)
	}
}

// TestElRechazoNombraAlLanzadorYLaSalida fija la segunda mitad de #25.
//
// El mensaje viejo decía sólo «corré pair --install», que es exactamente lo que
// el usuario acababa de hacer cuando se quedó sin MCP. Un remedio que es la
// acción que falló no es un remedio.
func TestElRechazoNombraAlLanzadorYLaSalida(t *testing.T) {
	const launcher = `C:\Tools\algun-cliente.exe`
	stderr, client := Explain(Result{
		Status:   NoToken,
		Label:    "claude-desktop",
		Detail:   "this server is paired, and the process that launched it presented no " + EnvVar,
		Launcher: launcher,
	})
	if stderr == "" {
		t.Fatal("stderr vacío")
	}
	// The log has to carry the way out too, not only the message for the model.
	// In this state the user's client is broken, so the model's relay may never
	// reach them — the log is the one channel they certainly have, and it is
	// where this gets diagnosed. An escape only in the message they cannot read
	// repeats the mistake this whole change exists to fix.
	if !strings.Contains(stderr, "pair --unpair") {
		t.Errorf("the log names no way out, so a user whose client is dead cannot rescue themselves "+
			"from what they can actually read:\n%s", stderr)
	}
	if !strings.Contains(client, launcher) {
		t.Errorf("el mensaje al cliente no nombra al lanzador (%s).\n"+
			"Es el dato que dice en la configuración de QUÉ programa tiene que estar el token,\n"+
			"que puede no ser el archivo donde el instalador lo escribió.\n\n%s", launcher, client)
	}
	if !strings.Contains(client, "pair --unpair") {
		t.Errorf("el mensaje al cliente no nombra la salida.\n"+
			"En este estado el usuario NO TIENE MCP, así que la respuesta no puede ser\n"+
			"«andá a leer algo»: hay que darle el comando que se lo devuelve.\n\n%s", client)
	}

	// Sin lanzador resuelto —hay plataformas donde no se puede— el mensaje tiene
	// que seguir siendo útil en vez de quedar a medio armar.
	_, sinLanzador := Explain(Result{Status: NoToken, Label: "x", Detail: "d"})
	if !strings.Contains(sinLanzador, "pair --unpair") {
		t.Errorf("sin lanzador resuelto el mensaje pierde la salida:\n%s", sinLanzador)
	}
}

func TestExplainAlwaysSaysWhatToDo(t *testing.T) {
	// Every status that refuses, plus the one that announces a coming refusal,
	// not a subset. ForeignLauncher shipped without being listed here, which is
	// how a rejection that says nothing gets in: the list is kept by hand and
	// the compiler does not check it.
	//
	// Announced belongs on it even though it serves. It is the start where the
	// user still has a working client to act in, so it is the one message that
	// has to arrive with something to do in it; a silent Announced would mean
	// the refusal that follows was, in every way the user can observe,
	// unannounced.
	//
	// The assertion is that the message names A way out, not one particular one.
	// The launcher statuses are escaped with `trust`, not `pair`, and demanding
	// the word "pair" from them would only push their message back toward the
	// mechanism the user is not on.
	for _, st := range []Status{NoToken, BadToken, ForeignParent, ForeignLauncher, EmptyRecord, Announced} {
		stderr, client := Explain(Result{Status: st, Label: "claude-desktop"})
		if stderr == "" || client == "" {
			t.Errorf("%v: stderr=%q client=%q; a silent rejection is indistinguishable from a crash", st, stderr, client)
		}
		if !strings.Contains(client, "pair") && !strings.Contains(client, "trust") {
			t.Errorf("%v: the client-facing message names no way out: %q", st, client)
		}
	}
	if stderr, client := Explain(Result{Status: OK}); stderr != "" || client != "" {
		t.Error("an authorized server produced a rejection message")
	}
	if stderr, client := Explain(Result{Status: Unpaired}); stderr != "" || client != "" {
		t.Error("an unpaired server produced a rejection message")
	}
}

// The launcher reaches the log only because Verify writes it into Detail, and
// Explain then passes Detail through. That coupling is invisible from either
// side on its own, so it is worth one test that walks the whole path: a change
// to how Verify words its detail would otherwise drop the launcher from the log
// with every unit test still green.
//
// It matters because the log is what a locked-out user reads, and the launcher
// is the fact that says whose configuration the token has to live in.
func TestTheLogNamesTheLauncherEndToEnd(t *testing.T) {
	dir := t.TempDir()
	secret, err := Mint(dir, "claude-desktop", false)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	const launcher = `C:\Tools\some-client.exe`
	confirm(t, dir, secret, launcher)

	r, err := Verify(dir, "", launcher)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if r.Status != NoToken {
		t.Fatalf("status = %v, want NoToken — this test needs the rejected path", r.Status)
	}

	stderr, client := Explain(r)
	for _, want := range []string{launcher, "pair --unpair"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the log is missing %q, which is what the user has to act on:\n%s", want, stderr)
		}
		if !strings.Contains(client, want) {
			t.Errorf("the message for the model is missing %q:\n%s", want, client)
		}
	}
}

// Pairing must not be able to cost someone their client. A record that has
// never seen a working token serves instead of refusing, so a token written
// into a config the client does not read leaves them with a client that still
// works and a log that says why — rather than the failure #25 describes, where
// the first sign of trouble is the MCP disappearing after a restart.
func TestAFreshPairingDoesNotLockAnyoneOut(t *testing.T) {
	dir := t.TempDir()
	if _, err := Mint(dir, "claude-desktop", false); err != nil {
		t.Fatalf("Mint: %v", err)
	}

	r, err := Verify(dir, "", "/opt/Claude/claude")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if r.Status != Armed {
		t.Fatalf("status = %v, want Armed", r.Status)
	}
	if !r.Allowed() {
		t.Fatal("a pairing nobody has confirmed locked the user out; that is exactly the failure this prevents")
	}

	stderr, client := Explain(r)
	for name, msg := range map[string]string{"log": stderr, "model message": client} {
		if !strings.Contains(msg, "NOT") {
			t.Errorf("the %s does not make clear that pairing is not in effect yet:\n%s", name, msg)
		}
	}
}

// The trial closes on evidence, not on a timer: the first launch that presents
// a valid token turns enforcement on, and it stays on.
func TestTheFirstGoodTokenTurnsEnforcementOn(t *testing.T) {
	dir := t.TempDir()
	secret, err := Mint(dir, "claude-desktop", false)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	if r, _ := Verify(dir, "", "/opt/Claude/claude"); r.Status != Armed {
		t.Fatalf("before any successful use: status = %v, want Armed", r.Status)
	}
	confirm(t, dir, secret, "/opt/Claude/claude")

	r, err := Verify(dir, "", "/opt/Claude/claude")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if r.Status != NoToken {
		t.Fatalf("after a successful use: status = %v, want NoToken — enforcement never engaged", r.Status)
	}
	if r.Allowed() {
		t.Fatal("the server still serves an unauthenticated launcher after pairing was confirmed")
	}
}

// Rotating a credential is not evidence that pairing stopped working. Deriving
// confirmation from the tokens rather than the record would reopen a door that
// had been shut for months, and it would do it silently — the operator rotates
// a token, and the next unauthenticated launcher is served.
func TestRotatingATokenDoesNotReopenTheDoor(t *testing.T) {
	dir := t.TempDir()
	first, err := Mint(dir, "claude-desktop", false)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	confirm(t, dir, first, "")

	if _, err := Mint(dir, "claude-desktop", false); err != nil {
		t.Fatalf("re-Mint: %v", err)
	}

	r, err := Verify(dir, "", "/opt/Claude/claude")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if r.Status == Armed || r.Allowed() {
		t.Fatalf("status = %v after rotation: rotating a token dropped a confirmed install back into its trial", r.Status)
	}
}

// The window is a default, not a verdict. Whoever will not have it can close it
// without waiting for anything.
func TestEnforceNowSkipsTheTrial(t *testing.T) {
	dir := t.TempDir()
	if _, err := Mint(dir, "claude-desktop", false); err != nil {
		t.Fatalf("Mint: %v", err)
	}

	f, _, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	f.EnforceNow = true
	if err := Save(dir, f); err != nil {
		t.Fatalf("Save: %v", err)
	}

	r, err := Verify(dir, "", "/opt/Claude/claude")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if r.Status != NoToken || r.Allowed() {
		t.Fatalf("status = %v with --enforce-now set, want a refusal", r.Status)
	}
}
