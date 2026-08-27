// SPDX-License-Identifier: Apache-2.0

package winspawn_test

import (
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// corretEnWindows dice si un archivo llega a compilar en Windows.
//
// EL BARRIDO SÓLO TIENE SENTIDO SOBRE LO QUE CORRE EN WINDOWS, y no mirarlo
// costó un rojo de CI: al mergear el pairing entró `internal/pairing/
// parent_other.go`, que llama a `ps` para resolver el proceso padre en macOS
// dentro de un `//go:build !windows`. Ese archivo NO EXISTE para el compilador
// de Windows, y en las demás plataformas `Harden` es un no-op por definición
// (`winspawn_other.go` devuelve el cmd sin tocar). Marcarlo pedía envolver algo
// para que no pasara nada: puro ritual.
//
// Saltearlo no es una exención — es la definición del problema. Una consola que
// parpadea es un fenómeno de Windows; un archivo que Windows nunca compila no
// puede producirla.
//
// Se miran las dos formas en que Go decide esto: la directiva `//go:build` y el
// sufijo del nombre (`foo_linux.go`). El sufijo `_other.go` no es un GOOS, así
// que no filtra nada por sí solo — esos archivos dependen de su directiva.
func correnEnWindows(ruta string, archivo *ast.File) bool {
	// 1. El sufijo del nombre. Go trata `_<GOOS>.go` como restricción implícita.
	base := strings.TrimSuffix(filepath.Base(ruta), ".go")
	for goos := range otrosSistemas {
		if strings.HasSuffix(base, "_"+goos) {
			return false
		}
	}

	// 2. La directiva //go:build, evaluada como si GOOS=windows. Sólo se responde
	//    por los tags de sistema operativo: cualquier otro (una etiqueta propia,
	//    una arquitectura) se da por cierto, para no excluir un archivo por una
	//    condición que no tiene nada que ver con la plataforma.
	for _, grupo := range archivo.Comments {
		for _, c := range grupo.List {
			if !constraint.IsGoBuild(c.Text) {
				continue
			}
			expr, err := constraint.Parse(c.Text)
			if err != nil {
				// Una directiva que no parsea no autoriza a saltear el archivo:
				// ante la duda se revisa, que es el lado seguro.
				return true
			}
			return expr.Eval(esCiertoEnWindows)
		}
	}
	return true
}

// otrosSistemas son los GOOS que no son Windows, más los meta-tags que Go
// resuelve por sistema operativo.
var otrosSistemas = map[string]bool{
	"linux": true, "darwin": true, "freebsd": true, "openbsd": true,
	"netbsd": true, "dragonfly": true, "solaris": true, "aix": true,
	"android": true, "ios": true, "js": true, "plan9": true, "wasip1": true,
	"illumos": true, "hurd": true, "unix": true, "posix": true,
}

// esCiertoEnWindows evalúa un tag de build como si GOOS fuera windows.
//
// Los GOOS ajenos dan falso, `windows` da verdadero, y **cualquier otro tag da
// verdadero a propósito**: una etiqueta propia o una arquitectura no dice nada
// sobre la plataforma, y darla por falsa excluiría del barrido archivos que sí
// compilan en Windows. Ante la duda, se revisa.
func esCiertoEnWindows(tag string) bool {
	if tag == "windows" {
		return true
	}
	return !otrosSistemas[tag]
}

// wrappers son los nombres que cuentan como envolver un lanzado.
//
// `hardenSpawn` es el envoltorio privado de internal/agent, que hoy no hace más
// que delegar en Harden. Se acepta por nombre para no obligar a reescribir sus
// cinco sitios de llamada.
var wrappers = map[string]bool{
	"Harden":      true,
	"hardenSpawn": true,
}

// exenciones son los archivos donde un lanzado SIN envolver es correcto, con la
// razón. Cualquier otro archivo que lance un proceso pone este test en rojo.
//
// Se listan por ruta exacta a propósito: una exención por patrón (por ejemplo
// "todo lo que esté bajo internal/inspect") se llevaría puesto un archivo nuevo
// que nadie revisó.
//
// OJO: ÉSTA NO ES LA ÚNICA EXENCIÓN. El walk de abajo saltea TODOS los
// `_test.go`, que es una exención mucho más ancha y vive escondida en el
// recorrido en vez de acá. Hoy cubre seis lanzados sueltos, enumerados:
//
//	internal/gitx/gitx_test.go:30,51,156
//	internal/task/lifecycle_test.go:48
//	internal/task/workspace_test.go:29,39
//
// El issue #18 la autoriza explícitamente, pero conviene decirla en voz alta por
// dos razones: estaba sin declarar en el PR original —lo señaló una sesión de
// verificación—, y es incoherente con haber envuelto `internal/agent/mock.go`
// con el argumento de que un test lanza procesos reales en la máquina de quien
// lo corre. `gitx_test.go` hace exactamente eso, en un test que tarda ~21s.
var exenciones = map[string]string{
	"internal/inspect/web.go": "abre el navegador del usuario; acá la ventana es el punto",
}

// TestTodoLanzadoPasaPorHarden recorre los .go del repositorio y falla si
// encuentra un exec.Command o exec.CommandContext que no esté envuelto.
//
// POR QUE UN BARRIDO Y NO UN TEST DE COMPORTAMIENTO
// El defecto que esto cuida (issue #18) no es que Harden esté mal —está bien—
// sino que tres sitios no lo llamaban. Eso no lo detecta ningún test de
// comportamiento: cada uno de esos tres sitios funcionaba perfectamente, sólo
// que abriendo una ventana. Lo único que se puede afirmar es una propiedad del
// código fuente, así que se afirma sobre el código fuente.
//
// El test corre en toda plataforma aunque el síntoma sea de Windows: si sólo
// corriera en Windows, un cambio hecho en Linux entraría sin que nadie lo vea.
//
// SU CONTROL, que hay que correr a mano cuando se toque este test: agregar un
// `exec.Command("git")` suelto en cualquier .go no exento y confirmar que este
// test se pone rojo NOMBRANDO ese archivo y esa línea. Un barrido que no
// encuentra nada y un barrido roto se ven exactamente igual desde afuera.
//
// LO QUE ESTE BARRIDO NO VE, medido por una sesión de verificación que fue a
// buscarlo. Los cuatro compilan y pasan `go vet`, así que el verde es ceguera y
// no un no-dato:
//
//  1. Un import con alias lo esquiva entero: la comparación es literal contra
//     "exec", así que `execx "os/exec"` + `execx.Command(...)` pasa. Un
//     `. "os/exec"` con `Command(...)` pelado tampoco es un SelectorExpr.
//  2. Un `&exec.Cmd{Path: ...}` armado a mano no es una llamada a exec.Command
//     y pasa. Lo mismo os.StartProcess y syscall.CreateProcess. Hoy no hay
//     ninguno: barrido con `exec\.Cmd\{|&exec\.Cmd` y
//     `os\.StartProcess|syscall\.(CreateProcess|StartProcess)`, cero fuera de
//     los dos &syscall.SysProcAttr{} legítimos.
//  3. La guarda de `revisados < 20` es más floja de lo que parece: de los 51
//     archivos no-test, 40 están bajo internal/, así que un walk roto que sólo
//     cubriera ese subárbol revisaría 39 y pasaría igual. Es la misma clase de
//     defecto que tuvo el grep manual del issue, que se perdió cmd/.
//  4. Envolver en dos pasos da un FALSO POSITIVO: el match es posicional, así
//     que `c := exec.Command(...)` seguido de `winspawn.Harden(c)` se reporta
//     como sin envolver. Fuerza el estilo anidado en una línea.
//
// Ninguno se tapa acá a propósito: taparlos pide un análisis de tipos, y el
// costo no se justifica para un repo de este tamaño mientras estén escritos.
// Que Harden HAGA algo lo cuida harden_windows_test.go, que es el agujero que sí
// dejaba reintroducir el defecto entero en verde.
func TestTodoLanzadoPasaPorHarden(t *testing.T) {
	raiz := raizDelModulo(t)

	// envueltos junta la posición de cada expresión que ya está adentro de un
	// envoltorio. Se recorre el árbol dos veces —una para juntar, otra para
	// verificar— porque el AST de Go no da el padre de un nodo.
	var (
		revisados int
		ajenos    int // archivos que Windows nunca compila; ver correnEnWindows
		hallazgos []string
	)

	err := filepath.Walk(raiz, func(ruta string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if n := info.Name(); n == ".git" || n == "vendor" || n == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(ruta, ".go") || strings.HasSuffix(ruta, "_test.go") {
			return nil
		}

		rel, _ := filepath.Rel(raiz, ruta)
		rel = filepath.ToSlash(rel)
		if _, exento := exenciones[rel]; exento {
			return nil
		}

		fset := token.NewFileSet()
		// ParseComments y no 0: las directivas //go:build son comentarios, y sin
		// ellas correnEnWindows no puede ver nada.
		archivo, err := parser.ParseFile(fset, ruta, nil, parser.ParseComments)
		if err != nil {
			// Un archivo que no parsea no se puede afirmar nada sobre él, y
			// callarse sería convertir un error en un verde.
			t.Errorf("%s: no parsea, así que no se pudo revisar: %v", rel, err)
			return nil
		}
		if !correnEnWindows(ruta, archivo) {
			ajenos++
			return nil
		}
		revisados++

		envueltos := map[token.Pos]bool{}
		ast.Inspect(archivo, func(n ast.Node) bool {
			llamada, ok := n.(*ast.CallExpr)
			if !ok || !esEnvoltorio(llamada.Fun) {
				return true
			}
			for _, arg := range llamada.Args {
				envueltos[arg.Pos()] = true
			}
			return true
		})

		ast.Inspect(archivo, func(n ast.Node) bool {
			llamada, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := llamada.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "exec" {
				return true
			}
			if sel.Sel.Name != "Command" && sel.Sel.Name != "CommandContext" {
				return true
			}
			if envueltos[llamada.Pos()] {
				return true
			}
			pos := fset.Position(llamada.Pos())
			hallazgos = append(hallazgos, rel+":"+itoa(pos.Line)+"  exec."+sel.Sel.Name)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("no se pudo recorrer el repositorio: %v", err)
	}

	// Si el barrido no revisó archivos, no encontró nada porque no miró: eso es
	// un no-dato, no un verde. Sin esta guarda, romper `raizDelModulo` dejaría
	// el test en verde para siempre.
	if revisados < 20 {
		t.Fatalf("el barrido sólo revisó %d archivos .go; no está mirando el repositorio", revisados)
	}

	if len(hallazgos) > 0 {
		t.Errorf(
			"%d lanzado(s) de proceso sin pasar por winspawn.Harden.\n"+
				"En Windows cada uno de éstos le parpadea una consola al usuario (issue #18).\n"+
				"Envolvelo: winspawn.Harden(exec.Command(...)) — o, si la ventana es\n"+
				"deliberada, agregalo a `exenciones` en este archivo con la razón.\n\n  %s",
			len(hallazgos), strings.Join(hallazgos, "\n  "))
	}
	t.Logf("revisados %d archivos .go, %d salteado(s) por no compilar en Windows, %d exención(es) declarada(s)",
		revisados, ajenos, len(exenciones))
}

// esEnvoltorio reconoce tanto `Harden(...)` como `winspawn.Harden(...)` y
// `hardenSpawn(...)`.
func esEnvoltorio(fun ast.Expr) bool {
	switch f := fun.(type) {
	case *ast.Ident:
		return wrappers[f.Name]
	case *ast.SelectorExpr:
		return wrappers[f.Sel.Name]
	}
	return false
}

// raizDelModulo sube desde este archivo hasta el directorio que tiene go.mod.
func raizDelModulo(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		padre := filepath.Dir(dir)
		if padre == dir {
			break
		}
		dir = padre
	}
	t.Fatalf("no se encontró go.mod subiendo desde %s", dir)
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
