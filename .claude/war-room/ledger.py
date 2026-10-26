#!/usr/bin/env python3
"""Query the war-room ledger without loading it whole.

ledger.md is ~1 MB of history. The chair and agents read slices:

  ledger.py start             the "Start here" section
  ledger.py live [--fixed] [P1 ...]
                              rows whose status is not terminal (open, in-progress,
                              blocked, partly verified, ...), optionally by section prefix;
                              --fixed adds "fixed" rows, implemented but never verified
  ledger.py item A-652a ...   each item's table row(s), every heading that names it with
                              that heading's body, the parent row of a sub-item (A-652a →
                              A-652) and its latest prose mentions with line numbers;
                              read a ruling's full text with sed -n 'START,ENDp' ledger.md
  ledger.py log [N]           the last N iteration-log rows (default 5)
  ledger.py grep REGEX        matching lines, each cut to 300 characters

Read-only. Output is plain text for a terminal or an agent prompt.
"""
import re
import sys
from pathlib import Path

LEDGER = Path(__file__).with_name("ledger.md")
TERMINAL = ("verified", "fixed", "closed", "shipped", "declined", "superseded", "done", "rejected")
ID_RE = re.compile(r"^[A-Z][A-Z0-9]*-[\w.-]+$")
WIDTH = 300
MENTIONS = 12


def lines():
    return LEDGER.read_text(encoding="utf-8").splitlines()


def cells(line):
    return [c.strip() for c in line.strip().strip("|").split("|")]


def item_rows(ls, status_tables=False):
    """Yield (line_no, section, cells) for every ledger table row keyed by an item ID.

    With status_tables, only rows of tables whose header ends in a Status column.
    """
    section, header = "", []
    for n, line in enumerate(ls, 1):
        if line.startswith("## "):
            section = line[3:].strip()
        if not line.startswith("|"):
            header = []
            continue
        if line.startswith("|--") or line.startswith("|---"):
            continue
        c = cells(line)
        if c and c[0] == "ID":
            header = c
            continue
        if c and ID_RE.match(c[0]):
            if status_tables and (not header or header[-1] != "Status"):
                continue
            yield n, section, c


def is_live(status, include_fixed=False):
    s = status.lower().lstrip("*`( ")
    if include_fixed and s.startswith("fixed"):
        return True
    return bool(s) and not s.startswith(TERMINAL)


def clip(s, width=WIDTH):
    return s if len(s) <= width else s[: width - 1] + "…"


def cmd_start(ls, _):
    out, on = [], False
    for line in ls:
        if line.startswith("## "):
            if on:
                break
            on = line.startswith("## Start here")
        if on:
            out.append(line)
    print("\n".join(out))


def cmd_live(ls, args):
    include_fixed = "--fixed" in args
    prefixes = [a.upper() for a in args if a != "--fixed"]
    for n, section, c in item_rows(ls, status_tables=True):
        if not is_live(c[-1], include_fixed):
            continue
        if prefixes and not any(section.upper().startswith(p) for p in prefixes):
            continue
        print(clip(f"{n}: [{section[:20]}] {c[0]} | {c[-1]} | {c[1]}"))


def cmd_item(ls, ids):
    if not ids:
        sys.exit("usage: ledger.py item ID [ID ...]")
    for item in ids:
        word = re.compile(r"(?<![\w-])" + re.escape(item) + r"(?![\w-])")
        print(f"=== {item}")
        found = False
        for n, section, c in item_rows(ls):
            if c[0] == item:
                found = True
                print(f"row {n} [{section}]:\n{ls[n - 1]}\n")
        # Headings (### or ####) that name the item, with their body up to the next heading.
        for n, line in enumerate(ls, 1):
            if re.match(r"^#{3,4} ", line) and word.search(line):
                found = True
                level = len(line) - len(line.lstrip("#"))
                body = [line]
                for nxt in ls[n:]:
                    m = re.match(r"^(#+) ", nxt)
                    if m and len(m.group(1)) <= level:
                        break
                    body.append(nxt)
                print(f"section at {n}:\n" + "\n".join(body).rstrip() + "\n")
        parent = re.sub(r"[a-z]$", "", item)
        if not found and parent != item:
            for n, section, c in item_rows(ls):
                if c[0] == parent:
                    print(f"no row of its own; parent row {n} [{section}]:\n{ls[n - 1]}\n")
        # Prose mentions (rulings, owner decisions, log rows), clipped. Sub-items such as
        # A-652a often live only here or in a range like "A-652a–e".
        stem = re.compile(re.escape(parent) + r"(?![\d])")
        hits = [(n, line) for n, line in enumerate(ls, 1) if word.search(line) or (not found and stem.search(line))]
        if not found and not hits:
            print("not found")
        for n, line in hits[-MENTIONS:]:
            print(clip(f"mention {n}: {line}"))
        if len(hits) > MENTIONS:
            print(f"({len(hits) - MENTIONS} earlier mentions omitted; ledger.py grep '{item}' lists all)")


def cmd_log(ls, args):
    count = int(args[0]) if args else 5
    rows, on = [], False
    for line in ls:
        if line.startswith("## "):
            on = line.startswith("## Iteration log")
            continue
        if on and line.startswith("| ") and re.match(r"^\| *\d+ *\|", line):
            rows.append(line)
    print("\n".join(rows[-count:]))


def cmd_grep(ls, args):
    if not args:
        sys.exit("usage: ledger.py grep REGEX")
    rx = re.compile(args[0])
    for n, line in enumerate(ls, 1):
        if rx.search(line):
            print(clip(f"{n}: {line}"))


COMMANDS = {"start": cmd_start, "live": cmd_live, "item": cmd_item, "log": cmd_log, "grep": cmd_grep}

if __name__ == "__main__":
    if len(sys.argv) < 2 or sys.argv[1] not in COMMANDS:
        sys.exit(__doc__)
    COMMANDS[sys.argv[1]](lines(), sys.argv[2:])
