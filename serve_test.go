package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andresh0816/cli-agent-mcp/internal/ui"
)

// boardHandler mirrors what serveHTTP registers, so the gate can be exercised
// without standing up a listener.
func boardHandler(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/board", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(ui.BoardHTML))
	})
	return mux
}

// The board page carries a live credential in memory and drives a server that
// can run commands on this machine, so serving it to an unauthenticated request
// would hand both away.
func TestBoardPageRequiresTheToken(t *testing.T) {
	h := boardHandler("s3cret")

	for _, url := range []string{"/board", "/board?token=", "/board?token=wrong"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s returned %d, want 401", url, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/board?token=s3cret", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET with the right token returned %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Agent tasks") {
		t.Error("the served page is not the board")
	}
}

// One document serves both the host-rendered view and the standalone page. If
// either transport is dropped, one of the two stops working with no test
// failing anywhere else.
func TestBoardCarriesBothTransports(t *testing.T) {
	for _, want := range []string{
		"window.parent.postMessage", // the host-rendered path
		"ui/notifications/initialized",
		`fetch("mcp"`, // the standalone path
		"Mcp-Session-Id",
		"var standalone",
	} {
		if !strings.Contains(ui.BoardHTML, want) {
			t.Errorf("the board no longer contains %q", want)
		}
	}
}

// A board opened in a background tab — which is what happens when a page is
// opened programmatically, or when a host panel starts off-screen — used to sit
// on "connecting…" forever, because the same check that skips polling also
// skipped the first fetch.
func TestBoardPaintsBeforeItIsLookedAt(t *testing.T) {
	if !strings.Contains(ui.BoardHTML, "document.hidden && painted") {
		t.Error("refresh() gates the first fetch on visibility again; a board opened hidden will never paint")
	}
}

func TestClientRendersDetectsTheExtension(t *testing.T) {
	if ui.ClientRenders(nil) {
		t.Error("a client that declared no extensions cannot render views")
	}
	if ui.ClientRenders(map[string]any{"someone.else/thing": map[string]any{}}) {
		t.Error("an unrelated extension must not count as view support")
	}
	if !ui.ClientRenders(map[string]any{ui.ExtensionName: map[string]any{}}) {
		t.Error("a client declaring the ui extension should be reported as able to render")
	}
}
