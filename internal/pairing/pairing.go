// SPDX-License-Identifier: Apache-2.0

// Package pairing decides who is allowed to drive this server.
//
// # What this is not
//
// The MCP transport here is stdio: the client launches the binary as a child
// process and talks to it over an anonymous pipe pair. That channel is already
// private — there is no port, no socket, and nothing on the wire for anyone to
// read without the privileges to debug the process outright, at which point no
// token would help either. So none of this encrypts or signs the conversation.
// There is nothing there to protect.
//
// # What it is
//
// The gap that does exist is authorization to launch. Today any local process
// can execute this binary, speak MCP down its stdio, and use the server as a
// confused deputy: it spawns a headless coding agent that inherits this
// machine's environment — SSH keys, VPN routes, an unlocked credential agent —
// and by default may edit files. Nothing distinguishes the MCP client the user
// configured from a rogue npm postinstall script.
//
// Pairing closes that. A secret is minted once, stored here as a hash, and
// handed to the client through its own config; a launcher that cannot present
// it gets a server that refuses to do anything.
//
// # The limit, stated plainly
//
// The secret lives in the client's config file and in this process's
// environment, both readable by anything running as the same user. An attacker
// who already has that read access takes the token and is indistinguishable
// from the real client. Pairing raises the bar — it stops code that can execute
// but not rummage through the user's profile — it is not a wall against a
// same-user attacker. Parent binding (see parent.go) is the second layer that
// narrows what a stolen token is worth.
package pairing

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FileName is the pairing record inside the state directory.
const FileName = "pairing.json"

// EnvVar carries the secret from the client's config into this process.
const EnvVar = "CLI_AGENT_MCP_TOKEN"

// tokenPrefix marks a secret as ours, so a value pasted into the wrong field is
// recognisable on sight and a mistyped one fails with a useful message rather
// than a bare mismatch.
const tokenPrefix = "cam1_"

// secretBytes is the entropy behind each token. 32 bytes from crypto/rand is
// far beyond guessing, which is why the stored form is a plain SHA-256 and not
// a password KDF: there is no low-entropy secret here for Argon2 to protect.
const secretBytes = 32

// fileVersion guards the on-disk format against a future change being read as
// if it were this one.
const fileVersion = 1

// Token is one issued credential. Labels exist so the clients that drive this
// server — Claude Desktop and Cowork are separate launchers, with separate
// config files — hold separate secrets: either can be revoked without
// disturbing the other, and the audit trail names which one was used.
type Token struct {
	Label    string    `json:"label"`
	Hash     string    `json:"hash"` // hex SHA-256 of the secret
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used,omitempty"`

	// Parent is the launcher this token has been bound to, recorded on first
	// successful use. Nil means "not bound yet"; NoBind means the operator
	// turned binding off for this token.
	Parent *Parent `json:"parent,omitempty"`
	NoBind bool    `json:"no_bind,omitempty"`
}

// Parent is the fingerprint of the process that launched a paired server.
type Parent struct {
	Exe      string    `json:"exe"`
	Recorded time.Time `json:"recorded"`
}

// File is the whole pairing record.
type File struct {
	Version int     `json:"version"`
	Tokens  []Token `json:"tokens"`

	// EnforceNow skips the trial described on Armed: no window, locked from the
	// moment the record is written. Waiting for the token is the right default,
	// not the right answer for everyone — a scripted install already knows the
	// token arrives, and some operators simply will not have the window.
	EnforceNow bool `json:"enforce_now,omitempty"`

	// ConfirmedAt is when a launcher first presented a valid token — the
	// evidence enforcement waits for.
	//
	// It lives on the record rather than being derived from Token.LastUsed, and
	// the difference matters: rotating a token would leave a record whose only
	// token had never been used, drop it back into its trial, and silently
	// reopen a door shut months ago. Rotating a credential is not evidence that
	// pairing stopped working.
	//
	// Only --unpair clears it, by removing the record.
	//
	// The omitempty is inert — encoding/json does not omit a zero struct, so an
	// unconfirmed record carries "confirmed_at":"0001-01-01T00:00:00Z" rather
	// than nothing. Harmless, since every reader goes through Confirmed() and
	// IsZero(), but the tag promises something it does not do and someone will
	// eventually read the file and wonder. Kept for the day it stops being a
	// struct; not worth a pointer to fix the cosmetics of a field nobody edits
	// by hand.
	//
	// AN OLDER SERVER READING THIS RECORD DOES NOT KNOW ANY OF IT. It sees a
	// pairing with tokens and enforces immediately — the lockout the trial
	// exists to prevent, arriving through a version mismatch. Measured against
	// the v0.13.0 release binary: it refuses where this build serves. That is
	// why runMint tells the user to install first and pair afterwards.
	ConfirmedAt time.Time `json:"confirmed_at,omitempty"`

	// TrustedLaunchers authorizes by who starts the server instead of by a
	// secret. See launcher.go for why that is the better default: the token
	// protects only as well as it is kept, and on a client with no config file
	// it ends up somewhere every process of this user can read.
	TrustedLaunchers []Launcher `json:"trusted_launchers,omitempty"`
}

// Confirmed reports whether a launcher has ever presented a valid token here.
func (f *File) Confirmed() bool {
	return f != nil && !f.ConfirmedAt.IsZero()
}

// Enforcing reports whether a launch without a valid token should be refused.
func (f *File) Enforcing() bool {
	if f == nil {
		return false
	}
	return f.EnforceNow || f.Confirmed()
}

// Status is the outcome of checking a launcher's credentials.
type Status int

const (
	// Unpaired means no pairing record exists. The server runs as it always
	// did — an upgrade must not brick a working install — and says so on
	// stderr. Running `pair` once switches enforcement on permanently.
	Unpaired Status = iota

	// OK means the presented secret matched a live token.
	OK

	// NoToken means the server is paired but the launcher presented nothing.
	NoToken

	// BadToken means a secret was presented and matched nothing.
	BadToken

	// ForeignParent means the secret was valid but the process that launched
	// this one is not the launcher the token is bound to.
	ForeignParent

	// Armed means the record exists but no token has ever worked here, so this
	// launch is served instead of refused.
	//
	// Turning on authentication can cost you the ability to undo it: if the
	// token never reaches the server, the client stops working and the fix is a
	// terminal command nobody knows. SSH keeps password auth until you prove
	// the key in a second session; a router reverts unless you confirm. Same
	// shape: the risky change stays provisional until it is seen to work.
	//
	// So enforcement waits for evidence. The first launch presenting a valid
	// token confirms the pairing, permanently. Until then the server serves and
	// says so loudly.
	//
	// The price, stated rather than hidden: in that window any launcher gets in.
	// It closes at the user's next client restart — which is exactly when they
	// would otherwise be locked out — and --enforce-now skips it entirely.
	Armed

	// TrustedLauncher means this server is authorized by WHO launched it rather
	// than by a secret, and the launching program is on the list. See
	// launcher.go.
	TrustedLauncher

	// ForeignLauncher means the same, but the launching program is not on it.
	ForeignLauncher

	// EmptyRecord means the record holds neither a token nor a trusted
	// launcher, so nothing can authenticate. Revoking the last token and
	// removing the last launcher both land here, and the server cannot tell
	// which — which is why this is its own status: NoToken's message names only
	// the token way out, and sending somebody who never held a token to go fix
	// one is the mistake of issue #25 in a new place.
	EmptyRecord
)

// Result reports the check and carries enough detail to tell the user what to
// do about it, both on stderr and inside the client conversation.
type Result struct {
	Status Status
	Label  string // the matched token, when there is one
	Detail string

	// Launcher is the executable that started this process, when the platform
	// could answer. It is carried into the rejection because it is the single
	// most useful fact for someone locked out: the token has to live in THAT
	// program's configuration, and the installer may well have written it
	// somewhere else entirely (issue #25).
	Launcher string
}

// Allowed reports whether the server should expose its real tools.
func (r Result) Allowed() bool {
	return r.Status == OK || r.Status == Unpaired || r.Status == Armed || r.Status == TrustedLauncher
}

// launcherList names the trusted launchers for a rejection message. Somebody
// locked out needs to see what the server is comparing against, not just that
// the comparison failed.
func launcherList(f *File) string {
	if f == nil || len(f.TrustedLaunchers) == 0 {
		return "no program in particular"
	}
	out := f.TrustedLaunchers[0].Exe
	for _, l := range f.TrustedLaunchers[1:] {
		out += ", " + l.Exe
	}
	return out
}

// Path is where the pairing record lives for a given state directory.
func Path(stateDir string) string { return filepath.Join(stateDir, FileName) }

// Load reads the pairing record. A missing file is not an error: it is the
// unpaired state, and the caller distinguishes it with the returned bool.
func Load(stateDir string) (*File, bool, error) {
	buf, err := os.ReadFile(Path(stateDir))
	if errors.Is(err, os.ErrNotExist) {
		return &File{Version: fileVersion}, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", Path(stateDir), err)
	}
	var f File
	if err := json.Unmarshal(buf, &f); err != nil {
		return nil, true, fmt.Errorf("%s is corrupt: %w", Path(stateDir), err)
	}
	if f.Version != fileVersion {
		return nil, true, fmt.Errorf("%s was written by a different version of cli-agent-mcp (format %d, this build understands %d); re-run `cli-agent-mcp pair`", Path(stateDir), f.Version, fileVersion)
	}
	return &f, true, nil
}

// Save writes the record with an owner-only mode, replacing any previous one.
//
// The 0600 is honoured on Unix. Windows ignores the mode bits entirely — Go
// creates the file with the directory's inherited ACL — so on that platform the
// protection is the state directory's own location under the user's roaming
// profile, which is already user-scoped. Tightening the ACL further would mean
// hand-rolling SetNamedSecurityInfo for a file whose contents are hashes, not
// secrets; the secret itself is never written here.
func Save(stateDir string, f *File) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("state dir %q: %w", stateDir, err)
	}
	f.Version = fileVersion
	buf, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')

	// Write-then-rename, so an interrupted save cannot leave a truncated record
	// that locks the user out of their own server.
	tmp := Path(stateDir) + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, Path(stateDir)); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", Path(stateDir), err)
	}
	return nil
}

// hashSecret is the stored form of a token.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(secret)))
	return hex.EncodeToString(sum[:])
}

// newSecret mints a fresh token.
func newSecret() (string, error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// Mint issues a token under label, replacing any existing token with that
// label. It returns the secret, which is the only time it exists in plaintext
// here — nothing but the hash is ever written to disk.
func Mint(stateDir, label string, noBind bool) (secret string, err error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return "", errors.New("a label is required, so you can tell your tokens apart and revoke one without the others")
	}

	f, _, err := Load(stateDir)
	if err != nil {
		return "", err
	}
	secret, err = newSecret()
	if err != nil {
		return "", err
	}

	tok := Token{
		Label:   label,
		Hash:    hashSecret(secret),
		Created: time.Now().UTC(),
		NoBind:  noBind,
	}
	replaced := false
	for i := range f.Tokens {
		if strings.EqualFold(f.Tokens[i].Label, label) {
			f.Tokens[i] = tok
			replaced = true
			break
		}
	}
	if !replaced {
		f.Tokens = append(f.Tokens, tok)
	}
	if err := Save(stateDir, f); err != nil {
		return "", err
	}
	return secret, nil
}

// Revoke drops the token with the given label. It reports whether one matched.
func Revoke(stateDir, label string) (bool, error) {
	f, paired, err := Load(stateDir)
	if err != nil || !paired {
		return false, err
	}
	kept := f.Tokens[:0]
	found := false
	for _, t := range f.Tokens {
		if strings.EqualFold(t.Label, label) {
			found = true
			continue
		}
		kept = append(kept, t)
	}
	if !found {
		return false, nil
	}
	f.Tokens = kept
	return true, Save(stateDir, f)
}

// Unpair removes the whole record, returning the server to its open, unpaired
// behaviour.
func Unpair(stateDir string) error {
	err := os.Remove(Path(stateDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Unbind clears a token's recorded launcher, so the next successful use records
// a new one. This is the way out of a legitimate change — the client was
// reinstalled somewhere else, or is now started through a different wrapper —
// without minting a new secret and editing the client's config again.
func Unbind(stateDir, label string) (bool, error) {
	f, paired, err := Load(stateDir)
	if err != nil || !paired {
		return false, err
	}
	for i := range f.Tokens {
		if strings.EqualFold(f.Tokens[i].Label, label) {
			f.Tokens[i].Parent = nil
			return true, Save(stateDir, f)
		}
	}
	return false, nil
}

// Verify checks a presented secret against the record and, on a match, enforces
// (or on first use records) the launcher binding.
//
// parentExe is the executable of the process that launched this one, empty if
// it could not be determined. An unknown parent never blocks a valid token:
// this layer is here to narrow what a stolen secret is worth, and failing shut
// on a platform where the lookup is unavailable would deny the legitimate user
// their own server for no gain in safety.
func Verify(stateDir, secret, parentExe string) (Result, error) {
	f, paired, err := Load(stateDir)
	if err != nil {
		return Result{Status: BadToken, Detail: err.Error()}, err
	}
	if !paired {
		// Nothing configured. Rather than serving anyone forever, record who
		// launched us and answer to that from now on (issue #27, launcher.go).
		//
		// The user runs no command and sees no secret; the machinery that used
		// to be needed here — a token, a config file to put it in, a trial to
		// confirm it arrived — was guarding a value that in the setup this was
		// written for is readable by every process of the same user anyway.
		//
		// A platform that cannot name the parent falls through to the old
		// behaviour. Refusing there would deny people their own server on a
		// system where this layer simply cannot apply, and that trade went the
		// wrong way once already (see TestUnknownParentDoesNotBlock).
		if parentExe == "" {
			return Result{Status: Unpaired}, nil
		}
		fresh := &File{Version: fileVersion}
		fresh.Trust(parentExe, true)
		if err := Save(stateDir, fresh); err != nil {
			// Serve anyway. Failing to write the record is not a reason to
			// withhold a server from the program that just started it — it only
			// means the same launcher gets trusted again next time.
			return Result{Status: Unpaired, Launcher: parentExe}, err
		}
		return Result{
			Status:   TrustedLauncher,
			Launcher: parentExe,
			Detail:   "first launch: " + parentExe + " is now the program allowed to start this server",
		}, nil
	}

	// Authorization by launcher: no secret involved, so none of the token
	// checks below apply.
	if f.TrustsLaunchers() {
		if parentExe == "" {
			// The list exists but this platform cannot say who launched us, so
			// the check cannot be made. Serving is the same call as above: the
			// alternative locks someone out of their own server over a fact the
			// operating system would not report.
			return Result{
				Status: TrustedLauncher,
				Detail: "this platform cannot name the launching program, so the launcher check was skipped",
			}, nil
		}
		if f.Trusts(parentExe) {
			return Result{
				Status:   TrustedLauncher,
				Launcher: parentExe,
				Detail:   parentExe + " is a trusted launcher",
			}, nil
		}
		// Inside the learning window a new launcher is adopted rather than
		// refused, so an install where two programs start the server keeps
		// working across the upgrade (see learningWindow, and the test that
		// insisted on it).
		if f.StillLearning(time.Now().UTC()) {
			f.Trust(parentExe, true)
			detail := parentExe + " was added to the trusted launchers: this record is less than a day old and is " +
				"still learning which programs start this server. After that it refuses anything new"
			if err := Save(stateDir, f); err != nil {
				// Serve, and say the record did not stick. Refusing here would
				// lock out a legitimate second client over a failed write.
				return Result{Status: TrustedLauncher, Launcher: parentExe, Detail: detail}, err
			}
			return Result{Status: TrustedLauncher, Launcher: parentExe, Detail: detail}, nil
		}
		return Result{
			Status:   ForeignLauncher,
			Launcher: parentExe,
			Detail: "this server answers to " + launcherList(f) + ", but it was launched by " + parentExe +
				". If that is your client — it moved, or updated to a new path — run `cli-agent-mcp trust --add` from it, " +
				"or `cli-agent-mcp trust --reset` to start over",
		}, nil
	}

	if len(f.Tokens) == 0 {
		return Result{
			Status: EmptyRecord,
			Detail: "the pairing record holds neither a token nor a trusted launcher, so nothing can authenticate",
		}, nil
	}

	// armed is the verdict for a record that has never seen a working token.
	// "You configured pairing" and "pairing works" are different things, and
	// only the second is worth locking the door on.
	armed := func(why string) Result {
		return Result{
			Status:   Armed,
			Detail:   why + ", and no launcher has ever presented a valid token, so this server is still serving. Enforcement starts the first time one arrives",
			Launcher: parentExe,
		}
	}

	secret = strings.TrimSpace(secret)
	if secret == "" {
		// The detail names the launcher whenever the platform could resolve one.
		// That is what separates a useless message — "presented nothing" — from
		// an actionable one: it says whose configuration the token has to live
		// in, which may not be the file the installer wrote (issue #25).
		//
		// It says "launched by" rather than asserting that program is the
		// client, because the parent can be a shim or a shell standing between
		// the two. It is still the best signal available, and a name the user
		// can recognise beats no name at all.
		detail := "this server is paired, and the process that launched it presented no " + EnvVar
		if parentExe != "" {
			detail += " (launched by " + parentExe + " — the token has to reach that program, or whatever it passes its environment to)"
		}
		if !f.Enforcing() {
			return armed("this server is paired but the launcher presented no " + EnvVar), nil
		}
		return Result{
			Status:   NoToken,
			Detail:   detail,
			Launcher: parentExe,
		}, nil
	}

	// Compare against every token rather than stopping at the first match: the
	// work is a handful of hashes and it keeps the timing independent of which
	// token matched, or of whether one did.
	want := []byte(hashSecret(secret))
	match := -1
	for i := range f.Tokens {
		if subtle.ConstantTimeCompare(want, []byte(f.Tokens[i].Hash)) == 1 {
			match = i
		}
	}
	if match < 0 {
		detail := "the presented " + EnvVar + " matches no issued token"
		if !strings.HasPrefix(secret, tokenPrefix) {
			detail += fmt.Sprintf("; it does not even look like one (tokens start with %q), so check the value was copied whole", tokenPrefix)
		}
		if !f.Enforcing() {
			return armed("the presented " + EnvVar + " matches no issued token"), nil
		}
		return Result{Status: BadToken, Detail: detail}, nil
	}

	tok := &f.Tokens[match]
	res := Result{Status: OK, Label: tok.Label}

	switch {
	case tok.NoBind, parentExe == "":
		// Binding disabled, or this platform cannot name the parent.
	case tok.Parent == nil:
		// Trust on first use: whoever launched the server the first time the
		// token worked is what the token means from now on.
		tok.Parent = &Parent{Exe: parentExe, Recorded: time.Now().UTC()}
	case !sameExe(tok.Parent.Exe, parentExe):
		return Result{
			Status: ForeignParent,
			Label:  tok.Label,
			Detail: fmt.Sprintf(
				"token %q is bound to %s but this server was launched by %s. "+
					"If you moved or reinstalled that client, run `cli-agent-mcp pair --unbind %s` and start it again.",
				tok.Label, tok.Parent.Exe, parentExe, tok.Label),
		}, nil
	}

	now := time.Now().UTC()
	tok.LastUsed = now
	// The evidence the trial waits for: a token reached the server, so pairing
	// works and the door can close for good.
	if f.ConfirmedAt.IsZero() {
		f.ConfirmedAt = now
	}
	// A failed write costs the timestamp, the binding on a first launch, and
	// this confirmation — worth warning about, never worth refusing a client
	// that just proved it holds a valid token. Losing the confirmation is the
	// mildest of the three: the record stays in its trial one launch longer,
	// which errs towards the user keeping a working client.
	if err := Save(stateDir, f); err != nil {
		return res, err
	}
	return res, nil
}

// sameExe compares two executable paths the way the host filesystem would.
func sameExe(a, b string) bool {
	// By identity, not by path. Comparing paths is what let a background update
	// of the client lock the user out of their own server: an MSIX executable
	// carries its version in its path, so `Claude_1.40609.0.0_...` became
	// `Claude_1.40609.1.0_...` and the binding stopped matching (issue #29).
	//
	// The three callers — Trusts, Untrust and the ForeignParent check — all go
	// through here, so this one line is the whole fix.
	//
	// Deliberately still takes paths: records written before this change hold
	// paths, and deriving the identity on read keeps them working without a
	// migration.
	return IdentityOf(a).Matches(IdentityOf(b))
}
