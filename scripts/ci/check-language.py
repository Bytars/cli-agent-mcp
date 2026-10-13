#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
check-language.py — rule 28 as an instrument, not as a promise.

WHY THIS FILE EXISTS (3-Oct-2026). Rule 28 («everything an agent reads is
English») was declared done three times, and each time a later measurement
refuted it. The first pass translated comments and documents and left 389 file
and directory names, ~3000 identifiers, log lines, test titles, YAML block
scalars, a NOTICE file with no comment syntax, and shell snippets inside
markdown. The renaming that fixed the names then left stale commands behind
(`ls estado`, `INVENTARIO-*`) in the very files that tell an agent where to
look. See `incidents/POSTMORTEM-rule-28-language-sweeps-2026-10-03.md`.

Every one of those was the same mistake: ONE instrument was used, it came back
empty on the class it could see, and the zero was read as «clean» (rules 2, 3
and 4). This tool is the second instrument, and it does not ask for belief: it
RATCHETS. A baseline records what is known and deliberate (product copy, frozen
migrations, quotations of evidence); anything beyond it fails.

THREE MEASUREMENTS, ALL OVER EVERY TRACKED TEXT FILE
  prose   lines with at least two distinct, unambiguous Spanish words
          (`el la los las del que para por una es se sin como pero ...`).
          It reads code, YAML, shell, markdown and plain text alike: that is
          the point. A parser-based sweep only sees comments or only sees
          strings; this sees the text.
  names   file or directory names with a Spanish token or an accented letter.
  former  citations of a NAME THAT WAS RENAMED, read from the `exchange`
          block of RENAMES.md (directories such as `estado/`, file prefixes
          such as `INVENTARIO-`, commands such as `ls estado`). A renaming that
          leaves its own citations behind is a defect, not progress.

WHAT IT DOES NOT PROVE. A heuristic: it misses Spanish in a line with fewer
than two of those words (`"Guardar"`), and it cannot tell product copy from a
leftover — the baseline is where a human says which is which. «Zero new
findings» means «nothing new that this instrument can see», and the report
says so. Identifiers are measured by AST in the per-language tools, not here.

THE BASELINE (`--baseline FILE`, default `tools/language-baseline.json` next to
this script). Per-file counts; a count that GROWS is an error, a count that
SHRINKS is a warning that tells you to lower it (so the ratchet only turns one
way). `--update-baseline` rewrites it: it is a deliberate act that shows in the
diff and in review, never something CI does by itself. Exempt paths are listed
in the baseline too (`exempt`), with a reason.

ESCAPE VALVE, per line: `<!--lint-ok-->` or `lang-ok` anywhere on the line.

  python3 tools/check-language.py --repo PATH [--baseline FILE] [--json]
  python3 tools/check-language.py --repo PATH --update-baseline

EXIT CODES  0 clean · 1 findings · 2 no tree · 4 the tool broke itself
"""

import argparse
import fnmatch
import json
import os
import re
import subprocess
import sys

sys.dont_write_bytecode = True

SKIP_DIRS = {".git", "node_modules", ".next", "vendor", "__pycache__", ".venv"}
BINARY_EXT = (".png", ".jpg", ".jpeg", ".gif", ".ico", ".svg", ".woff", ".woff2",
              ".ttf", ".pdf", ".zip", ".gz", ".tgz", ".lock", ".sum", ".bin",
              ".webp", ".mp4", ".keystore", ".tar", ".jar")
MAX_BYTES = 2_000_000

# Unambiguous Spanish function words. «no», «con», «a», «me», «he», «sin» were
# left out where English uses them too; what stays here is what an English line
# practically never contains twice.
SPANISH_WORDS = frozenset(
    "el la los las del que para por una es se sin como pero este esta cuando "
    "porque sobre entre también más ya muy cada todo todos desde hasta aún así "
    "dónde qué cómo está están será fue han hay solo nunca siempre aquí ahora "
    "antes después".split())
WORD = re.compile(r"[A-Za-záéíóúñü]+")
ESCAPE = re.compile(r"<!--\s*lint-ok\s*-->|\blang-ok\b", re.IGNORECASE)

# Tokens that mark a NAME as Spanish. Deliberately a list, not a dictionary
# service: this runs in CI with no network and no third-party package.
SPANISH_NAME_TOKENS = frozenset(
    "estado negocio identidad incidente incidentes metodo metodologia protocolo "
    "reglas carriles carril medicion hallazgo hallazgos cierre propuesta "
    "pendientes inventario guia guion memoria nota relevo encargo "
    "evaluacion discusion contrato catalogo puesta marcha arquitectura flujo "
    "trabajo modos malla secreto secretos migracion migraciones sede sedes "
    "dispositivo dispositivos tablero tableros grupo grupos usuario usuarios "
    "cliente clientes factura facturas servicio servicios respaldo respaldos "
    "copia baja recolector vigia ingesta analisis estatico calidad elegir "
    "pruebas lienzo informe reporte plantilla borrador carpeta ejemplo "
    "de del la el los las para con sin por".split())
NAME_SPLIT = re.compile(r"[^A-Za-zÁÉÍÓÚáéíóúñÑüÜ]+")


def tracked_files(root):
    """`git ls-files` when it is a clone, a walk otherwise (toy trees in tests,
    sparse CI checkouts). Never `git status`: it takes the index lock."""
    try:
        out = subprocess.run(["git", "-C", root, "-c", "core.quotepath=off",
                              "ls-files", "-z"], stdout=subprocess.PIPE,
                             stderr=subprocess.DEVNULL, check=True).stdout
        files = [f for f in out.decode("utf-8", "replace").split("\0") if f]
        if files:
            return sorted(f for f in files if os.path.isfile(os.path.join(root, f)))
    except (OSError, subprocess.CalledProcessError):
        pass
    found = []
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
        for fn in filenames:
            found.append(os.path.relpath(os.path.join(dirpath, fn), root).replace("\\", "/"))
    return sorted(found)


def read_text(root, rel):
    if rel.lower().endswith(BINARY_EXT):
        return None
    path = os.path.join(root, rel)
    try:
        if os.path.getsize(path) > MAX_BYTES:
            return None
        with open(path, "rb") as fh:
            data = fh.read()
    except OSError:
        return None
    if b"\0" in data:
        return None
    try:
        return data.decode("utf-8")
    except UnicodeDecodeError:
        return None


def is_exempt(rel, exempt):
    return any(fnmatch.fnmatch(rel, pat) for pat in exempt)


def prose_hits(root, files, exempt):
    """{path: [(line, text)]} of lines with >= 2 distinct Spanish function words."""
    res = {}
    for rel in files:
        if is_exempt(rel, exempt):
            continue
        text = read_text(root, rel)
        if text is None:
            continue
        hits = []
        for i, line in enumerate(text.split("\n"), 1):
            if ESCAPE.search(line):
                continue
            if len({w.lower() for w in WORD.findall(line)} & SPANISH_WORDS) >= 2:
                hits.append((i, line.strip()[:160]))
        if hits:
            res[rel] = hits
    return res


def name_hits(files, exempt):
    """Paths with a Spanish token or an accented letter in any segment."""
    res = []
    for rel in files:
        if is_exempt(rel, exempt):
            continue
        for seg in rel.split("/"):
            stem = seg.rsplit(".", 1)[0]
            toks = {t.lower() for t in NAME_SPLIT.split(re.sub(r"([a-z])([A-Z])", r"\1 \2", stem)) if t}
            if toks & SPANISH_NAME_TOKENS or re.search(r"[áéíóúñü]", seg, re.I):
                res.append(rel)
                break
    return res


RE_BLOCK = re.compile(r"^## exchange\b.*?\n```text\n(.*?)\n```", re.S | re.M)


def former_patterns(root):
    """Former directory names and file prefixes, from RENAMES.md (`exchange`
    section). Empty when the file is absent: then this measurement is reported
    as not done, never as zero."""
    path = os.path.join(root, "RENAMES.md")
    text = read_text(root, "RENAMES.md") if os.path.isfile(path) else None
    if text is None:
        return None
    m = RE_BLOCK.search(text)
    if not m:
        return None
    dirs, prefixes = set(), set()
    for line in m.group(1).split("\n"):
        old, _, new = line.partition(" -> ")
        if not new:
            continue
        o, n = old.strip().split("/"), new.strip().split("/")
        for a, b in zip(o[:-1], n[:-1]):
            if a != b and len(a) >= 4:
                dirs.add(a)
        t = o[-1].split("-")[0]
        if o[-1] != n[-1] and re.fullmatch(r"[A-ZÁÉÍÓÚÑ]{4,}", t) and t not in n[-1].split("-"):
            prefixes.add(t)
    return dirs, prefixes


def former_hits(root, files, exempt):
    pats = former_patterns(root)
    if pats is None:
        return None
    dirs, prefixes = pats
    parts = []
    if dirs:
        d = "|".join(sorted(re.escape(x) for x in dirs))
        parts.append(rf"(?<![\w/.-])(?:{d})/")                                # estado/…
        parts.append(rf"\b(?:ls|cd|cat|grep|find|git\s+add|git\s+mv)\s+(?:-\S+\s+)*(?:{d})\b")
    if prefixes:
        p = "|".join(sorted(re.escape(x) for x in prefixes))
        parts.append(rf"(?<![A-Za-z])(?:{p})-")                               # INVENTARIO-…
    if not parts:
        return {}
    rx = re.compile("|".join(parts))
    res = {}
    for rel in files:
        if rel == "RENAMES.md" or is_exempt(rel, exempt):
            continue
        text = read_text(root, rel)
        if text is None:
            continue
        hits = []
        for i, line in enumerate(text.split("\n"), 1):
            if not ESCAPE.search(line) and rx.search(line):
                hits.append((i, line.strip()[:160]))
        if hits:
            res[rel] = hits
    return res


def measure(root, exempt=()):
    files = tracked_files(root)
    return {
        "files": len(files),
        "prose": prose_hits(root, files, exempt),
        "names": name_hits(files, exempt),
        "former": former_hits(root, files, exempt),     # None = not measurable
    }


def load_baseline(path):
    if not os.path.isfile(path):
        return None
    with open(path, encoding="utf-8") as fh:
        return json.load(fh)


def to_baseline(m, exempt):
    return {
        "version": 1,
        "note": ("Known and deliberate Spanish (product copy, frozen migrations, quotations of "
                 "evidence). A count may only go DOWN. Regenerate with --update-baseline: it is "
                 "a decision that shows in review."),
        "exempt": sorted(exempt),
        "prose": {f: len(h) for f, h in sorted(m["prose"].items())},
        "names": sorted(m["names"]),
        "former": ({f: len(h) for f, h in sorted(m["former"].items())}
                   if m["former"] is not None else {}),
    }


def compare(m, base):
    """(errors, warnings, skips) — each a (kind, file, line, message, evidence)."""
    errors, warns, skips = [], [], []
    for kind, label in (("prose", "Spanish prose"), ("former", "citation of a renamed name")):
        cur = m[kind]
        if cur is None:
            skips.append((kind, "-", 0, "RENAMES.md has no `exchange` block: I could not measure "
                          "citations of former names. A skip is not a zero.", None))
            continue
        old = base.get(kind, {})
        for f, hits in sorted(cur.items()):
            if len(hits) > old.get(f, 0):
                shown = hits[old.get(f, 0):][:3] if old.get(f) else hits[:3]
                for ln, text in shown:
                    errors.append((kind, f, ln,
                                   f"{label}: {len(hits)} line(s), baseline allows {old.get(f, 0)}. "
                                   "Write it in English, or mark the line `lang-ok` if it is a "
                                   "quotation or product copy.", text))
        for f, n in sorted(old.items()):
            if len(cur.get(f, [])) < n:
                warns.append((kind, f, 0, f"{label}: baseline says {n}, now {len(cur.get(f, []))}. "
                              "Lower the baseline (`--update-baseline`) so it cannot grow back.", None))
    old_names = set(base.get("names", []))
    for f in sorted(set(m["names"]) - old_names):
        errors.append(("names", f, 0, "file or directory name with a Spanish token or an "
                       "accented letter (rule 28: names are English).", f))
    for f in sorted(old_names - set(m["names"])):
        warns.append(("names", f, 0, "in the baseline but no longer a Spanish name: remove it.", None))
    return errors, warns, skips


def main(argv=None):
    ap = argparse.ArgumentParser(description="rule 28 as a ratchet")
    ap.add_argument("--repo", required=True)
    here = os.path.dirname(os.path.abspath(__file__))
    ap.add_argument("--baseline", default=os.path.join(here, "language-baseline.json"))
    ap.add_argument("--update-baseline", action="store_true")
    ap.add_argument("--json", action="store_true")
    args = ap.parse_args(argv)
    root = os.path.abspath(args.repo)
    if not os.path.isdir(root):
        print(f"the tree does not exist: {root}", file=sys.stderr)
        return 2
    base = load_baseline(args.baseline)
    exempt = tuple(base.get("exempt", [])) if base else ("RENAMES.md",)
    m = measure(root, exempt)
    if args.update_baseline:
        with open(args.baseline, "w", encoding="utf-8", newline="\n") as fh:
            json.dump(to_baseline(m, exempt), fh, ensure_ascii=False, indent=1)
            fh.write("\n")
        print(f"baseline written: {len(m['prose'])} file(s) with prose, "
              f"{len(m['names'])} name(s), {len(m['former'] or {})} file(s) with former names")
        return 0
    if base is None:
        print(f"no baseline at {args.baseline}: run --update-baseline once, on purpose.", file=sys.stderr)
        return 1
    errors, warns, skips = compare(m, base)
    if args.json:
        print(json.dumps({"files": m["files"], "errors": errors, "warnings": warns, "skips": skips},
                         ensure_ascii=False, indent=1))
    else:
        print(f"files measured: {m['files']} · prose lines: {sum(len(h) for h in m['prose'].values())} · "
              f"former-name lines: {sum(len(h) for h in (m['former'] or {}).values())} · "
              f"Spanish names: {len(m['names'])}")
        for lvl, group in (("SKIP", skips), ("WARNING", warns), ("ERROR", errors)):
            for kind, f, ln, msg, ev in group:
                print(f"[{lvl}] {kind} {f}{':' + str(ln) if ln else ''}\n    {msg}" + (f"\n    → {ev}" if ev else ""))
        print(f"RESULT: {len(errors)} error(s), {len(warns)} warning(s), {len(skips)} skip(s). "
              "Heuristic: zero new findings is not proof of zero Spanish.")
    return 1 if errors else 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception:
        import traceback
        traceback.print_exc()
        print("THE TOOL BROKE ITSELF: the repo was left unmeasured.", file=sys.stderr)
        sys.exit(4)
