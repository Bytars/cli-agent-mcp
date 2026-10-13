#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
check-no-ai-attribution.py — rule 29 as a guard, not a promise.

CANONICAL SOURCE: Bytars/exchange `tools/check-no-ai-attribution.py`. The other
repositories carry a byte-identical copy at `.githooks/check-no-ai-attribution.py`.
Do not edit a copy: change it here and copy it.

WHY THIS EXISTS (4-Oct-2026). `exchange#27` was merged with
`Co-authored-by: Claude <noreply@anthropic.com>` at the end of its squash message.
**No commit message contained that line.** GitHub writes one `Co-authored-by` into
the squash message for every commit AUTHOR other than the person who merges, and
every commit of the session was authored as `Claude <noreply@anthropic.com>`. A
check that reads only the text of the message would have passed all eight commits.
So this guard looks at three things:

  1. the **author** and the **committer** identity of each commit,
  2. the **trailers**: `Co-authored-by`, `Signed-off-by`, `Reviewed-by`, ... naming an
     AI tool, and any `Claude-Session:` / `Cursor-Session:` line,
  3. the **footers**: «Generated with [Claude Code]», 🤖 lines, claude.ai/code links.

Human co-authors are untouched: that is real credit (rule 29).

THE ENTRY POINTS (one file, so there is one definition of «AI attribution»):

  commit-msg FILE      git `commit-msg` hook: the message about to be committed and
                       the identity git will use (`git var GIT_AUTHOR_IDENT`).
  pre-push             git `pre-push` hook: every commit about to leave the machine.
  range REV-RANGE      CI, or by hand: every commit in `base..head`.
  pretooluse           Claude Code `PreToolUse` hook (event JSON on stdin): refuses a
                       `git commit` whose identity or message is an AI's, and a
                       `git push` that would publish such a commit. It works in a
                       fresh clone, where `core.hooksPath` is not set yet.

WHAT IT STOPS, AND WHAT IT DOES NOT. A local hook stops accidents and habits. It does
not stop a deliberate `--no-verify`, and whoever has write access can edit it: a lock
with the key in the room. **The wall is the CI job** (`range` on the pull request), made
a required check, because the squash message is generated at merge time from the
commits that CI has already inspected. «Zero findings» means zero this instrument can
see: an identity spelled in a way the patterns below do not know is not caught.

EXIT CODES  0 clean · 1 AI attribution found · 2 usage / not a repository · 4 broke itself
For `pretooluse` the answer is JSON on stdout and the exit code is always 0 (the
protocol of `.githooks/guard.py`).
"""

import json
import os
import re
import subprocess
import sys

sys.dont_write_bytecode = True

VERSION = 1

AI_EMAIL_DOMAINS = ("anthropic.com", "openai.com", "cursor.com", "cursor.sh",
                    "codeium.com", "windsurf.com", "tabnine.com")
# An identity NAME that is an AI product. Anchored on purpose: a person called
# Gemini or Cursor is possible; «Claude Code» is not a person.
AI_NAME = re.compile(
    r"^\s*(claude(\s+(code|opus|sonnet|haiku|agent)\b.*)?|cursor(\s+agent)?|"
    r"(github\s+)?copilot(\s+\w+)?|chat\s?gpt|openai\s+codex|codex|"
    r"gemini(\s+(cli|code\s+assist))?|aider|windsurf|cody)\s*$", re.I)
AI_EMAIL = re.compile(
    r"@(?:[\w-]+\.)*(?:" + "|".join(re.escape(d) for d in AI_EMAIL_DOMAINS) + r")\b", re.I)
# Words that mark an AI tool inside a free-text trailer value.
AI_WORD = re.compile(r"\b(claude|anthropic|cursor|copilot|chat\s?gpt|openai|codex|"
                     r"gemini|aider|windsurf)\b", re.I)

TRAILER = re.compile(
    r"^\s*(co-authored-by|signed-off-by|reviewed-by|generated-by|assisted-by|"
    r"made-with|created-with)\s*:\s*(.*\S)\s*$", re.I)
SESSION_TRAILER = re.compile(r"^\s*(claude|cursor|codex|copilot)-(session|agent|code)\s*:", re.I)
FOOTER = re.compile(
    r"^[\W_]*(generated|created|written|made)\s+(with|by)\b.*\b"
    r"(claude|cursor|copilot|chat\s?gpt|codex|gemini|aider)\b", re.I)
SESSION_URL = re.compile(r"claude\.ai/(code|chat)/\S*session", re.I)

HOW_TO_FIX = (
    "How to fix it:\n"
    "  · The IDENTITY is what GitHub turns into `Co-authored-by` when the PR is squashed, "
    "even if every message is clean. Commit as the person who owns this work: "
    "`git config user.name` and `git config user.email` (ask the user which identity to "
    "use; do not invent one).\n"
    "  · A commit already made: `git commit --amend --reset-author --no-edit` for the "
    "last one, or `git rebase -r <base> --exec 'git commit --amend --reset-author --no-edit'` "
    "for a branch, then push with `--force-with-lease` (only on your own branch).\n"
    "  · A trailer or footer in the message: delete the line. Human co-authors stay.\n"
    "  · Rule 29 (exchange/RULES.md): no AI co-authorship marks. This overrides the "
    "default attribution lines that agent tooling adds on its own."
)


def parse_ident(text):
    """`Name <email> 1700000000 +0000` or `Name <email>` -> (name, email)."""
    m = re.match(r"^\s*(.*?)\s*<([^>]*)>", text or "")
    return (m.group(1), m.group(2)) if m else ((text or "").strip(), "")


def ident_is_ai(name, email):
    if AI_EMAIL.search(email or ""):
        return True
    return bool(AI_NAME.match(name or ""))


def value_is_ai(value):
    name, email = parse_ident(value)
    if email or "<" in value:
        return ident_is_ai(name, email) or bool(AI_WORD.search(name))
    return bool(AI_WORD.search(value))


def message_findings(message):
    """[(line number, what, text)] for AI trailers and footers in a message."""
    out = []
    for i, line in enumerate((message or "").splitlines(), 1):
        if line.lstrip().startswith("#") and not SESSION_TRAILER.match(line):
            continue                                   # git strips comment lines
        m = TRAILER.match(line)
        if m and value_is_ai(m.group(2)):
            out.append((i, f"AI `{m.group(1)}` trailer", line.strip()))
        elif SESSION_TRAILER.match(line):
            out.append((i, "AI session trailer", line.strip()))
        elif FOOTER.match(line):
            out.append((i, "«generated with» footer", line.strip()))
        elif SESSION_URL.search(line):
            out.append((i, "link to an AI session", line.strip()))
    return out


def git(args, cwd=None, env=None):
    try:
        r = subprocess.run(["git", *args], cwd=cwd, env=env, stdout=subprocess.PIPE,
                           stderr=subprocess.PIPE)
    except OSError as e:
        return 127, str(e)
    return r.returncode, r.stdout.decode("utf-8", "replace")


def commit_findings(sha, an, ae, cn, ce, message):
    out = []
    if ident_is_ai(an, ae):
        out.append((sha, "AUTHOR is an AI identity", f"{an} <{ae}>"))
    if ident_is_ai(cn, ce):
        out.append((sha, "COMMITTER is an AI identity", f"{cn} <{ce}>"))
    for ln, what, text in message_findings(message):
        out.append((sha, f"{what} (message line {ln})", text))
    return out


def log_findings(rev_args, cwd=None):
    rc, out = git(["log", "--format=%H%x1f%an%x1f%ae%x1f%cn%x1f%ce%x1f%B%x1e", *rev_args], cwd=cwd)
    if rc != 0:
        return None, out
    found = []
    for rec in out.split("\x1e"):
        rec = rec.strip("\n")
        if not rec.strip():
            continue
        sha, an, ae, cn, ce, body = (rec.split("\x1f") + [""] * 6)[:6]
        found += commit_findings(sha.strip()[:10], an, ae, cn, ce, body)
    return found, ""


def report(found, where):
    print(f"check-no-ai-attribution: BLOCKED — AI attribution found {where}:", file=sys.stderr)
    for sha, what, text in found:
        print(f"  {sha}  {what}: {text[:140]}", file=sys.stderr)
    print("\n" + HOW_TO_FIX, file=sys.stderr)


def current_identities(cwd=None, env=None):
    res = []
    for var in ("GIT_AUTHOR_IDENT", "GIT_COMMITTER_IDENT"):
        rc, out = git(["var", var], cwd=cwd, env=env)
        res.append(parse_ident(out.strip()) if rc == 0 else ("", ""))
    return res


# --- entry points ---------------------------------------------------------------

def mode_commit_msg(path):
    with open(path, encoding="utf-8", errors="replace") as fh:
        message = fh.read()
    (an, ae), (cn, ce) = current_identities()
    found = commit_findings("(this commit)", an, ae, cn, ce, message)
    if found:
        report(found, "in the commit being created")
        return 1
    return 0


ZERO = "0" * 40


def mode_pre_push(stdin):
    bad = []
    for line in stdin:
        parts = line.split()
        if len(parts) < 4:
            continue
        _, local_sha, _, remote_sha = parts[:4]
        if local_sha == ZERO:
            continue                                   # a deletion publishes no commit
        revs = [local_sha, "--not", "--remotes"] if remote_sha == ZERO else [f"{remote_sha}..{local_sha}"]
        found, err = log_findings(revs)
        if found is None:
            print(f"check-no-ai-attribution: could not read the commits to push: {err}", file=sys.stderr)
            return 1                                   # fail closed
        bad += found
    if bad:
        report(bad, "in the commits about to be pushed")
        return 1
    return 0


def mode_range(rev_range):
    found, err = log_findings([rev_range])
    if found is None:
        print(f"check-no-ai-attribution: could not read `{rev_range}`: {err}", file=sys.stderr)
        return 2
    if found:
        report(found, f"in `{rev_range}`")
        return 1
    print(f"check-no-ai-attribution: OK — no AI attribution in `{rev_range}` (v{VERSION}).")
    return 0


GIT_COMMIT = re.compile(r"(?:^|[;&|(\s])git\b(?:\s+-[cC]\s+\S+|\s+--[\w-]+(?:=\S+)?)*\s+commit\b")
GIT_PUSH = re.compile(r"(?:^|[;&|(\s])git\b(?:\s+-[cC]\s+\S+|\s+--[\w-]+(?:=\S+)?)*\s+push\b")
OPT_C = re.compile(r"-c\s+(user\.(?:name|email))=(?:\"([^\"]*)\"|'([^']*)'|(\S+))")
ENV_ASSIGN = re.compile(r"\bGIT_(AUTHOR|COMMITTER)_(NAME|EMAIL)=(?:\"([^\"]*)\"|'([^']*)'|(\S+))")
AUTHOR_FLAG = re.compile(r"--author(?:=|\s+)(?:\"([^\"]*)\"|'([^']*)'|(\S+))")


def pretooluse_decision(command, cwd):
    """(deny?, reason). Pure enough to be tested without Claude Code."""
    if GIT_COMMIT.search(command):
        env = dict(os.environ)
        extra = []
        for m in OPT_C.finditer(command):
            extra += ["-c", f"{m.group(1)}={m.group(2) or m.group(3) or m.group(4)}"]
        for m in ENV_ASSIGN.finditer(command):
            env[f"GIT_{m.group(1)}_{m.group(2)}"] = m.group(3) or m.group(4) or m.group(5)
        idents = []
        for var in ("GIT_AUTHOR_IDENT", "GIT_COMMITTER_IDENT"):
            rc, out = git([*extra, "var", var], cwd=cwd, env=env)
            idents.append(parse_ident(out.strip()) if rc == 0 else ("", ""))
        am = AUTHOR_FLAG.search(command)
        if am:
            idents[0] = parse_ident(am.group(1) or am.group(2) or am.group(3))
        found = commit_findings("(this commit)", idents[0][0], idents[0][1],
                                idents[1][0], idents[1][1], command)
        if found:
            return True, found, "the commit this command would create"
    if GIT_PUSH.search(command):
        rc, up = git(["rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"], cwd=cwd)
        revs = [f"{up.strip()}..HEAD"] if rc == 0 and up.strip() else ["HEAD", "--not", "--remotes"]
        found, _ = log_findings(revs, cwd=cwd)
        if found:
            return True, found, "in the commits this push would publish"
    return False, [], ""


def mode_pretooluse(stdin):
    try:
        event = json.load(stdin)
    except ValueError:
        return 0
    if event.get("tool_name") != "Bash":
        return 0
    command = (event.get("tool_input") or {}).get("command", "")
    cwd = event.get("cwd") or None
    deny, found, where = pretooluse_decision(command, cwd)
    if deny:
        lines = [f"BLOCKED by check-no-ai-attribution.py (rule 29): AI attribution {where}."]
        lines += [f"  {sha} {what}: {text[:120]}" for sha, what, text in found]
        lines.append(HOW_TO_FIX)
        print(json.dumps({"hookSpecificOutput": {
            "hookEventName": "PreToolUse", "permissionDecision": "deny",
            "permissionDecisionReason": "\n".join(lines)}}))
    return 0


def main(argv):
    if len(argv) < 2:
        print(__doc__.split("THE ENTRY POINTS")[1].split("WHAT IT STOPS")[0], file=sys.stderr)
        return 2
    mode = argv[1]
    if mode == "commit-msg" and len(argv) == 3:
        return mode_commit_msg(argv[2])
    if mode == "pre-push":
        return mode_pre_push(sys.stdin)
    if mode == "range" and len(argv) == 3:
        return mode_range(argv[2])
    if mode == "pretooluse":
        return mode_pretooluse(sys.stdin)
    print(f"unknown mode `{mode}` or wrong arguments", file=sys.stderr)
    return 2


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv))
    except Exception:
        import traceback
        traceback.print_exc()
        print("THE GUARD BROKE ITSELF: nothing was checked. Refusing, on purpose.", file=sys.stderr)
        sys.exit(4)
