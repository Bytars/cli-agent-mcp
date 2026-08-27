// SPDX-License-Identifier: Apache-2.0

package inspect

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// cookieName holds the viewer's session token in the browser.
const cookieName = "cam_ui_session"

// queryParam carries the token in the URL printed at startup, which is how the
// browser gets it in the first place.
const queryParam = "t"

// guard is what stands between a transcript and everything else on the machine.
//
// The viewer serves prompts, file contents and command output — everything the
// worker saw and did. It listens on a TCP port, which makes it the one part of
// this project that is genuinely reachable by something other than the process
// that started it, and until now it answered anyone who asked.
//
// Two different callers had to be shut out:
//
// Other local processes and other users on the machine. A fixed port on
// localhost is not a permission: anything that can open a socket can read the
// whole history. The session token fixes that — it is minted per run, printed
// once in the URL, and never written to disk.
//
// Web pages the user happens to visit. A page cannot read a cross-origin
// response, but DNS rebinding turns that into a same-origin one: the attacker's
// hostname is re-resolved to 127.0.0.1 and the browser hands the page whatever
// this server returns. The cookie is the defence that actually holds — it was
// set for 127.0.0.1 and is not sent to the attacker's hostname — and the Host
// and Origin checks below reject the request before it reaches a handler.
type guard struct {
	next  http.Handler
	token string // empty disables authentication
	local bool   // enforce a loopback Host header
}

// newSessionToken mints the per-run secret. 16 bytes is not a password; it is a
// capability handed straight to the browser, and it lives only as long as the
// process.
func newSessionToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func (g *guard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if g.local && !hostIsLocal(r.Host) {
		// A loopback listener reached through some other name is either a
		// misconfiguration or a rebinding attempt. Neither deserves an answer.
		writeErr(w, http.StatusMisdirectedRequest, "this viewer only answers to localhost")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && !originMatches(origin, r.Host) {
		// Browsers attach Origin to exactly the requests that did not come from
		// this viewer's own page.
		writeErr(w, http.StatusForbidden, "cross-origin requests are not accepted")
		return
	}

	if g.token == "" {
		g.next.ServeHTTP(w, r)
		return
	}

	if c, err := r.Cookie(cookieName); err == nil && tokenMatches(c.Value, g.token) {
		g.next.ServeHTTP(w, r)
		return
	}

	// The token arrives in the URL the user was given. Trade it for a cookie and
	// bounce to the bare path, so it stops riding along in the address bar,
	// browser history and any Referer the page later sends.
	if tok := r.URL.Query().Get(queryParam); tokenMatches(tok, g.token) {
		http.SetCookie(w, &http.Cookie{
			Name:     cookieName,
			Value:    g.token,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
		})
		clean := *r.URL
		q := clean.Query()
		q.Del(queryParam)
		clean.RawQuery = q.Encode()
		http.Redirect(w, r, clean.RequestURI(), http.StatusSeeOther)
		return
	}

	writeErr(w, http.StatusUnauthorized,
		"open the URL printed by `cli-agent-mcp ui`; it carries the session token this viewer needs")
}

// tokenMatches compares in constant time and refuses the empty value, so a
// missing cookie can never match a disabled token by accident.
func tokenMatches(got, want string) bool {
	if got == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// hostIsLocal checks the Host header names this machine. An address literal is
// judged by its IP; a name is only ever accepted as "localhost".
func hostIsLocal(host string) bool {
	h := host
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		h = parsed
	}
	h = strings.Trim(strings.TrimSpace(h), "[]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// originMatches reports whether an Origin header names this same server.
func originMatches(origin, host string) bool {
	origin = strings.TrimSpace(origin)
	for _, scheme := range []string{"http://", "https://"} {
		if strings.HasPrefix(origin, scheme) {
			return strings.EqualFold(origin[len(scheme):], host)
		}
	}
	return false
}
