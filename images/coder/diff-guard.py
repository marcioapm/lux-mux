#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# ///
"""Scan a git diff for the anti-patterns Márcio keeps having to flag by hand.

Runs against ADDED lines only, so pre-existing offenders in a file never fail the gate.
Intended as a hard gate after every agent run, before commit and before any push.

Rules are tiered:
  error  - sql-substring-assert, source-inspection-test, handbuilt-sql  (exit 1)
  warn   - javadoc-on-trivial-member                                    (exit 1 only with --strict)

Usage:
    diff-guard.py                       # working tree vs HEAD
    diff-guard.py --base origin/main    # branch vs base ref
    diff-guard.py --json
    diff-guard.py --strict              # warnings fail too
Exit codes: 0 clean, 1 findings, 2 usage/git error.
"""

from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
from dataclasses import dataclass, asdict


@dataclass(frozen=True)
class Finding:
    rule: str
    severity: str
    path: str
    line: int
    text: str
    why: str


@dataclass(frozen=True)
class Rule:
    name: str
    severity: str
    pattern: re.Pattern[str]
    why: str
    suffixes: tuple[str, ...]
    tests_only: bool = False
    main_only: bool = False


LINE_RULES: tuple[Rule, ...] = (
    Rule(
        name="sql-substring-assert",
        severity="error",
        pattern=re.compile(
            r"(assert\w*\s*\(\s*\w*[Ss]ql\w*\.contains|"
            r"assertThat\s*\(\s*\w*[Ss]ql\w*\s*\)\s*\.\s*contains|"
            r"\.toSql\(\)\s*\.\s*contains|"
            r"assert\s+[\"']?\w*\s*in\s+\w*sql\w*)"
        ),
        why="pins SQL text, not behaviour; passes on inverted logic. Execute the query and assert on rows.",
        suffixes=(".java", ".kt", ".py", ".ts"),
        tests_only=True,
    ),
    Rule(
        name="source-inspection-test",
        severity="error",
        pattern=re.compile(
            r"(inspect\.getsource|inspect\.getsourcelines|"
            r"Path\([^)]*\)\.read_text\(\)|"
            r"ast\.parse\(|"
            r"getClass\(\)\.getResourceAsStream\([^)]*\.java)"
        ),
        why="asserts on source spelling, not runtime behaviour; can pass while the property is broken.",
        suffixes=(".java", ".kt", ".py", ".ts"),
        tests_only=True,
    ),
    Rule(
        name="handbuilt-sql",
        severity="error",
        pattern=re.compile(
            r"(String\.format\s*\(\s*\"[^\"]*\b(SELECT|INSERT|UPDATE|DELETE|CREATE|ALTER|FROM|WHERE|JOIN)\b|"
            r"\bfmt\s*\(\s*\"[^\"]*\b(SELECT|INSERT|UPDATE|DELETE|CREATE)\b|"
            r"\"\s*\+\s*\w+\s*\+\s*\"[^\"]*\b(FROM|WHERE|JOIN|VALUES|SET)\b|"
            r"f\"[^\"]*\b(SELECT|INSERT|UPDATE|DELETE)\b[^\"]*\{)",
            re.IGNORECASE,
        ),
        why="SQL built by string formatting. Use the repo query builders (SelectQuery/UpdateQuery/DeleteQuery/"
        "CreateQuery/UnionQuery in abs) unless they cannot express the construct.",
        suffixes=(".java", ".kt", ".py"),
        main_only=True,
    ),
)

# A Javadoc block is only a finding when the member it documents is trivial. Documenting a genuinely
# complex contract is fine; documenting a getter is the defect. Matching the member rather than the
# block keeps this rule high-precision — a blanket "/** was added" check produced 106 hits across
# three real abs commits, which is noise nobody reads.
_TRIVIAL_MEMBER = re.compile(
    r"^\s*(public|protected|private|)\s*(static\s+|final\s+)*"
    r"(?:[\w<>\[\],.?\s]+\s+)?"
    r"("
    r"(get|is|has|set)[A-Z]\w*\s*\([^)]*\)\s*\{[^}]*\}"  # one-line accessor
    r"|(get|is|has|set)[A-Z]\w*\s*\([^)]*\)\s*;"  # interface accessor
    r"|\w+\s*(=|;)"  # plain field
    r"|builder\s*\(\s*\)"  # builder factory
    r")"
)
_JAVADOC_OPEN = re.compile(r"^\s*/\*\*")
_JAVADOC_CLOSE = re.compile(r"\*/")

_TEST_MARKERS = ("/test/", "/tests/", "test_", "Test.java", "Tests.java", "_test.py", "Spec.ts", ".test.ts")
_JAVADOC_WHY = (
    "Javadoc on a self-evident member (accessor/field/builder). A // line naming the non-obvious "
    "constraint beats a /** */ block; delete or downgrade it."
)


def _is_test_path(path: str) -> bool:
    return any(marker in path for marker in _TEST_MARKERS)


def _git(args: list[str]) -> str:
    proc = subprocess.run(["git", *args], capture_output=True, text=True)
    if proc.returncode != 0:
        print(proc.stderr.strip(), file=sys.stderr)
        raise SystemExit(2)
    return proc.stdout


def _untracked_as_diff() -> str:
    """Render untracked files as an all-added diff.

    `git diff` never reports untracked files, and a brand-new test file is the single most
    likely place for a banned test shape to arrive. Without this the gate reports clean on
    an agent that added `FooTest.java` and committed nothing.
    """
    names = [n for n in _git(["ls-files", "--others", "--exclude-standard"]).splitlines() if n.strip()]
    chunks: list[str] = []
    for name in names:
        if not name.endswith((".java", ".kt", ".py", ".ts")):
            continue
        try:
            with open(name, encoding="utf-8", errors="replace") as fh:
                lines = fh.read().splitlines()
        except OSError:
            continue
        body = "\n".join("+" + line for line in lines)
        chunks.append(f"+++ b/{name}\n@@ -0,0 +1,{len(lines)} @@\n{body}")
    return "\n".join(chunks)


def _git_diff(base: str | None, committed_only: bool) -> str:
    """Diff that includes UNCOMMITTED and UNTRACKED work by default.

    Agents routinely finish a turn with everything still in the working tree, so a
    `base...HEAD` range diff would report a clean gate on work that was never committed.
    Diffing against the merge-base without a second endpoint covers committed commits,
    staged changes and unstaged edits; untracked files are appended separately because git
    omits them from every diff form. `--committed-only` restores range semantics for
    auditing an already-pushed branch.
    """
    if committed_only:
        rev = f"{base}...HEAD" if base else "HEAD"
        return _git(["diff", "--unified=0", "--no-color", rev])
    if base is None:
        tracked = _git(["diff", "--unified=0", "--no-color", "HEAD"])
    else:
        merge_base = _git(["merge-base", base, "HEAD"]).strip()
        tracked = _git(["diff", "--unified=0", "--no-color", merge_base])
    return tracked + "\n" + _untracked_as_diff()


_HUNK = re.compile(r"^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@")


def _added_lines(diff: str) -> list[tuple[str, int, str]]:
    """(path, line_number, text) for every added line in the diff, in order."""
    out: list[tuple[str, int, str]] = []
    path = ""
    lineno = 0
    for raw in diff.splitlines():
        if raw.startswith("+++ b/"):
            path = raw[6:]
            continue
        hunk = _HUNK.match(raw)
        if hunk:
            lineno = int(hunk.group(1))
            continue
        if not raw.startswith("+") or raw.startswith("+++"):
            continue
        out.append((path, lineno, raw[1:]))
        lineno += 1
    return out


def _scan_javadoc(added: list[tuple[str, int, str]]) -> list[Finding]:
    """Flag a Javadoc block only when the next real member it precedes is trivial.

    Operates on contiguous added lines: a block whose documented member was not itself added is
    not attributable to this change and is skipped.
    """
    findings: list[Finding] = []
    i = 0
    while i < len(added):
        path, lineno, text = added[i]
        if not path.endswith((".java", ".kt")) or _is_test_path(path) or not _JAVADOC_OPEN.match(text):
            i += 1
            continue
        block_start_line = lineno
        j = i
        while j < len(added) and not _JAVADOC_CLOSE.search(added[j][2]):
            j += 1
        j += 1  # first line after the block
        # Skip annotations and blanks to reach the documented member.
        while j < len(added) and (not added[j][2].strip() or added[j][2].lstrip().startswith("@")):
            j += 1
        if j < len(added) and added[j][0] == path and _TRIVIAL_MEMBER.match(added[j][2]):
            findings.append(
                Finding("javadoc-on-trivial-member", "warn", path, block_start_line, added[j][2].strip()[:160], _JAVADOC_WHY)
            )
        i = j if j > i else i + 1
    return findings


def scan(diff: str) -> list[Finding]:
    added = _added_lines(diff)
    findings: list[Finding] = []
    for path, lineno, text in added:
        for rule in LINE_RULES:
            if not path.endswith(rule.suffixes):
                continue
            is_test = _is_test_path(path)
            if rule.tests_only and not is_test:
                continue
            if rule.main_only and is_test:
                continue
            if rule.pattern.search(text):
                findings.append(Finding(rule.name, rule.severity, path, lineno, text.strip()[:160], rule.why))
    findings.extend(_scan_javadoc(added))
    return findings


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--base", default=None, help="base ref, e.g. origin/main (default: working tree vs HEAD)")
    ap.add_argument("--json", action="store_true", dest="as_json")
    ap.add_argument("--strict", action="store_true", help="warnings fail the gate too")
    ap.add_argument(
        "--committed-only",
        action="store_true",
        help="with --base, diff base...HEAD and ignore uncommitted work (default includes it)",
    )
    args = ap.parse_args()

    findings = scan(_git_diff(args.base, args.committed_only))
    errors = [f for f in findings if f.severity == "error"]
    warns = [f for f in findings if f.severity == "warn"]

    if args.as_json:
        print(json.dumps([asdict(f) for f in findings], indent=2))
        return 1 if errors or (warns and args.strict) else 0

    if not findings:
        print("diff-guard: clean")
        return 0

    by_rule: dict[str, list[Finding]] = {}
    for f in findings:
        by_rule.setdefault(f.rule, []).append(f)
    for rule, items in by_rule.items():
        print(f"\n=== [{items[0].severity}] {rule} ({len(items)}) ===")
        print(f"    {items[0].why}")
        for f in items:
            print(f"  {f.path}:{f.line}: {f.text}")

    print(f"\ndiff-guard: {len(errors)} error(s), {len(warns)} warning(s).")
    if errors:
        print("Errors must be fixed or explicitly justified before committing.")
    return 1 if errors or (warns and args.strict) else 0


if __name__ == "__main__":
    raise SystemExit(main())
