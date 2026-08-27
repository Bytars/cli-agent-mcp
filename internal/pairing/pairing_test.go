// SPDX-License-Identifier: Apache-2.0

package pairing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An install that predates pairing must keep working. Bricking a working setup
// on upgrade would be a worse outcome than the exposure this feature closes.
func TestUnpairedServesAsBefore(t *testing.T) {
	dir := t.TempDir()

	res, err := Verify(dir, "", "/usr/bin/whatever")
	if err != nil {
		t.Fatalf("Verify on an unpaired dir: %v", err)
	}
	if res.Status != Unpaired {
		t.Fatalf("status = %v, want Unpaired", res.Status)
	}
	if !res.Allowed() {
		t.Fatal("an unpaired server refused to serve; that breaks every existing install on upgrade")
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
	// Same secret, different program.
	res, _ := Verify(dir, secret, "/tmp/definitely-not-claude")
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
func TestCorruptRecordDoesNotOpenTheServer(t *testing.T) {
	dir := t.TempDir()
	if _, err := Mint(dir, "claude-desktop", false); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if err := os.WriteFile(Path(dir), []byte("{ not json"), 0o600); err != nil {
		t.Fatalf("corrupt the record: %v", err)
	}

	res, err := Verify(dir, "anything", "")
	if err == nil {
		t.Error("a corrupt record was accepted without complaint")
	}
	if res.Allowed() {
		t.Fatal("a corrupt record left the server open to any launcher")
	}
}

func TestFutureFormatIsNotIgnored(t *testing.T) {
	dir := t.TempDir()
	buf, _ := json.Marshal(File{Version: fileVersion + 1})
	if err := os.WriteFile(Path(dir), buf, 0o600); err != nil {
		t.Fatalf("write record: %v", err)
	}

	res, err := Verify(dir, "anything", "")
	if err == nil {
		t.Error("a record from a newer format was read as if it were this one")
	}
	if res.Allowed() {
		t.Fatal("a record this build cannot understand left the server open")
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
	otro, err := Verify(dir, secreto, `C:\Windows\System32\cmd.exe`)
	if err != nil {
		t.Fatalf("Verify desde otro lanzador: %v", err)
	}
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
	for _, st := range []Status{NoToken, BadToken, ForeignParent} {
		stderr, client := Explain(Result{Status: st, Label: "claude-desktop"})
		if stderr == "" || client == "" {
			t.Errorf("%v: stderr=%q client=%q; a silent rejection is indistinguishable from a crash", st, stderr, client)
		}
		if !strings.Contains(client, "pair") {
			t.Errorf("%v: the client-facing message never mentions pairing: %q", st, client)
		}
	}
	if stderr, client := Explain(Result{Status: OK}); stderr != "" || client != "" {
		t.Error("an authorized server produced a rejection message")
	}
	if stderr, client := Explain(Result{Status: Unpaired}); stderr != "" || client != "" {
		t.Error("an unpaired server produced a rejection message")
	}
}
