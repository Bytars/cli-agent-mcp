// SPDX-License-Identifier: Apache-2.0

package inspect

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func servedBy(t *testing.T, g *guard, r *http.Request) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	reached := false
	g.next = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.Write([]byte("transcript"))
	})
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, r)
	return rec, reached
}

func TestGuardRejectsWithoutTheToken(t *testing.T) {
	g := &guard{token: "s3cret", local: true}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7788/api/log?id=task-1", nil)

	rec, reached := servedBy(t, g, req)
	if reached {
		t.Fatal("a transcript was served to a request carrying no session token")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
}

func TestGuardAcceptsTheCookie(t *testing.T) {
	g := &guard{token: "s3cret", local: true}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7788/api/tasks", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "s3cret"})

	if _, reached := servedBy(t, g, req); !reached {
		t.Fatal("a request holding the right cookie was turned away")
	}
}

func TestGuardRejectsTheWrongCookie(t *testing.T) {
	g := &guard{token: "s3cret", local: true}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7788/api/tasks", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "guessed"})

	if _, reached := servedBy(t, g, req); reached {
		t.Fatal("a wrong cookie was accepted")
	}
}

// The URL printed at startup is how the browser acquires the cookie. The token
// is then dropped from the address so it stops appearing in history and Referer.
func TestGuardTradesTheURLTokenForACookie(t *testing.T) {
	g := &guard{token: "s3cret", local: true}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7788/?t=s3cret", nil)

	rec, reached := servedBy(t, g, req)
	if reached {
		t.Error("served the page directly instead of redirecting to a clean URL")
	}
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("code = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want %q — the token is still in the URL", loc, "/")
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value != "s3cret" {
		t.Fatalf("cookies = %v, want the session token", cookies)
	}
	if !cookies[0].HttpOnly {
		t.Error("the session cookie is readable from JavaScript")
	}
	if cookies[0].SameSite != http.SameSiteStrictMode {
		t.Error("the session cookie is not SameSite=Strict")
	}
}

// DNS rebinding: the browser resolves the attacker's hostname to 127.0.0.1 and
// the request arrives here with their Host. The cookie will not be attached to
// that hostname, but the Host check refuses it before any handler runs.
func TestGuardRejectsARebindingHost(t *testing.T) {
	g := &guard{token: "s3cret", local: true}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7788/api/tasks", nil)
	req.Host = "evil.example.com"
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "s3cret"})

	rec, reached := servedBy(t, g, req)
	if reached {
		t.Fatal("a request for someone else's hostname was served")
	}
	if rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("code = %d, want 421", rec.Code)
	}
}

// A page on another origin fetching this viewer gets an Origin header attached
// by the browser; the viewer's own page never sends one that does not match.
func TestGuardRejectsACrossOriginFetch(t *testing.T) {
	g := &guard{token: "s3cret", local: true}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7788/api/tasks", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "s3cret"})

	rec, reached := servedBy(t, g, req)
	if reached {
		t.Fatal("a cross-origin request was served the transcripts")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("code = %d, want 403", rec.Code)
	}
}

func TestGuardAllowsItsOwnOrigin(t *testing.T) {
	g := &guard{token: "s3cret", local: true}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7788/api/tasks", nil)
	req.Header.Set("Origin", "http://127.0.0.1:7788")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "s3cret"})

	if _, reached := servedBy(t, g, req); !reached {
		t.Fatal("the viewer's own page was treated as cross-origin")
	}
}

// --no-token stays honest: it disables authentication, not the origin checks
// that keep a web page from reading the port.
func TestGuardWithoutATokenStillRefusesRebinding(t *testing.T) {
	g := &guard{token: "", local: true}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7788/api/tasks", nil)
	req.Host = "evil.example.com"

	if _, reached := servedBy(t, g, req); reached {
		t.Fatal("--no-token also dropped the rebinding defence")
	}
}

func TestGuardWithoutATokenServesLocally(t *testing.T) {
	g := &guard{token: "", local: true}
	req := httptest.NewRequest(http.MethodGet, "http://localhost:7788/api/tasks", nil)

	if _, reached := servedBy(t, g, req); !reached {
		t.Fatal("--no-token refused a plain local request")
	}
}

// An empty cookie must never satisfy an empty token by accident.
func TestGuardEmptyValuesNeverMatch(t *testing.T) {
	if tokenMatches("", "") {
		t.Error("empty matched empty")
	}
	if tokenMatches("", "s3cret") || tokenMatches("s3cret", "") {
		t.Error("an empty value matched a real token")
	}
}

func TestHostIsLocal(t *testing.T) {
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"127.0.0.1:7788", true},
		{"localhost:7788", true},
		{"localhost", true},
		{"[::1]:7788", true},
		{"127.0.0.2:7788", true}, // the whole 127/8 loops back
		{"evil.example.com", false},
		{"192.168.1.10:7788", false},
		{"localhost.evil.example.com", false},
		{"", false},
	} {
		if got := hostIsLocal(tc.host); got != tc.want {
			t.Errorf("hostIsLocal(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestOriginMatches(t *testing.T) {
	for _, tc := range []struct {
		origin, host string
		want         bool
	}{
		{"http://127.0.0.1:7788", "127.0.0.1:7788", true},
		{"http://127.0.0.1:7788", "127.0.0.1:9999", false},
		{"https://evil.example.com", "127.0.0.1:7788", false},
		{"null", "127.0.0.1:7788", false},
		{"http://127.0.0.1:7788.evil.com", "127.0.0.1:7788", false},
	} {
		if got := originMatches(tc.origin, tc.host); got != tc.want {
			t.Errorf("originMatches(%q, %q) = %v, want %v", tc.origin, tc.host, got, tc.want)
		}
	}
}

func TestSessionTokensDiffer(t *testing.T) {
	a, err := newSessionToken()
	if err != nil {
		t.Fatalf("newSessionToken: %v", err)
	}
	b, err := newSessionToken()
	if err != nil {
		t.Fatalf("newSessionToken: %v", err)
	}
	if a == b {
		t.Fatal("two session tokens came out identical")
	}
	if len(a) < 20 {
		t.Errorf("session token %q is short enough to be worth guessing", a)
	}
}
