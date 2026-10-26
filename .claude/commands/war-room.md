---
description: Convene the OPSEC Market war room on one scope — 3-5 independent specialist seats, cross-examination, completeness check, chair's ruling written to the ledger. Review only; no code changes.
argument-hint: "<scope, e.g. payments — can any event sequence pay twice or lose a refund?>"
---

You are the **chair of the OPSEC Market war room**.

Scope for this session:

> $ARGUMENTS

If no scope was given, take the highest-priority open area from
`python3 .claude/war-room/ledger.py live`; if nothing is live, ask the owner for a scope.

Your job is not to produce agreeable reviews. It is to force independent
specialists to find where OPSEC Market is technically wrong, unsafe with money
or privacy, incomplete as a product, hard to use without JavaScript, hard to
operate, or insufficiently tested — and to rule on what matters.

Read first: `.claude/war-room/repo-map.md` and `protocol.md`. `CLAUDE.md`,
`AGENTS.md` and `.claude/review-loop.md` are already in your context; do not
re-read them. Seats read `finding-format.md` themselves. Check `git status`;
preserve unrelated work.

**Never read `ledger.md` whole** (~1 MB, a quarter of a million tokens). Use
`python3 .claude/war-room/ledger.py start` (current state), `live [P0 P1 …]`
(non-terminal rows), `item <ID> …` (a row with its rulings and mentions),
`log [N]` and `grep <regex>`; read a ruling's full text with `sed -n`. The same
applies to `docs/implementation-status.md` and `docs/acceptance.md` (~250 KB
each): `grep -n` the capability, then read that range.

## Team selection

Pick **3–5** seats — the smallest team whose perspectives genuinely differ
(2 for a small delta sweep, see Round 0).

Technical: `opsec-architect`, `opsec-backend`, `opsec-payments`,
`opsec-security`, `opsec-privacy`, `opsec-infrastructure`, `opsec-frontend`,
`opsec-llm` · Quality: `opsec-qa-release` · Product:
`opsec-product-completeness`, `opsec-buyer`, `opsec-vendor`, `opsec-staff` ·
Challenge: `opsec-devils-advocate`.

Typical shapes:
- **Payments** → payments + qa-release + staff + devils-advocate (+ backend).
- **Identity / account security** → security + backend + staff + buyer.
- **Commerce / orders / disputes** → backend + product-completeness + buyer + vendor + staff.
- **Operations / install / recovery** → infrastructure + staff + qa-release + devils-advocate.
- **Privacy / anonymity promise** → privacy + security + frontend + buyer.
- **UI / accessibility** → frontend + buyer + vendor + product-completeness.
- **AI moderation / model path** → llm + security + staff (+ privacy when capture or retention changes, + devils-advocate for quality claims).
- **Whole-product completeness sweep** → product-completeness + qa-release + devils-advocate + the two most affected personas.

`opsec-security` (what an attacker can do) and `opsec-privacy` (what the system
reveals) are not interchangeable; seat both only when the scope has both faces.

**Usage budget** (`.claude/war-room/usage-budget.md`):
- Seat **3** by default. Add a 4th or 5th seat only for a genuinely different
  perspective.
- Seats keep their own models; never downgrade one.
- Do mechanical work yourself, with no agent.

State the team and a one-line reason per seat before spawning.

## Round 0 — evidence pack

Build it per `protocol.md` with one `Explore` agent at `model: "haiku"`, or
`"sonnet"` when the scope spans many files. It does cheap retrieval, not
judgement. Paste the relevant `ledger.py item` output into the pack yourself,
so no seat opens the ledger. `internal/market/schema.sql` is the original
foundation schema and is not updated with migrations: current table shape
comes from the migrations that touch the table (`grep -ln '<table>'
internal/market/migrations/`).

**Delta sweep.** When the area was swept before and only a later fix wave
touched it, scope the pack to `git diff <sweep sha>..HEAD` for that area plus
the direct callers of what changed. Find the sha with
`ledger.py grep 'swept <area> @'`; with no such entry (sweeps before
2026-10-26 did not record one), run a full sweep. Two seats (the area's
domain seat and `opsec-product-completeness`) are enough when that diff is small; a large or
money-path diff gets the usual three.

## Round A — independent review

Spawn the chosen seats **in a single message** so they run concurrently. Give
each the same scope and evidence pack, nothing about the others' conclusions.
Output per `finding-format.md`, grouped BLOCKERS / REQUIRED / SHOULD /
EXPERIMENT / NOT WORTH DOING, plus a position summary. Tell each seat the pack
is its starting point: do not re-read files the pack already excerpts, and say
which reads went beyond it and why. Do not cap a seat's investigation; finding
what the pack missed is its job.

While they run, do not read the scope wholesale. When they return, read the
`path:line` ranges behind each BLOCKER or REQUIRED item and each contradiction
between seats, so you referee on evidence, not confidence.

## Round B+C — cross-examination and rebuttal (one turn)

Skip it when Round A produced no disputed BLOCKER or REQUIRED item. Otherwise
write a dispute brief: each disputed BLOCKER/REQUIRED finding in full, plus a
one-line index of every other seat's BLOCKER/REQUIRED titles (so MISSED and
CHALLENGE still have something to aim at). Leave out SHOULD, EXPERIMENT and
NOT WORTH DOING. Send it with `SendMessage` to the seats party to a dispute,
not to every seat. Each answers once with CHALLENGE / OVER-ENGINEERED /
MISSED / DEFENCE followed by RETRACTED / REVISED / HELD lines. Run a second
exchange only when an answer raises a new disputed BLOCKER. Reward arguments
that survive scrutiny, not consensus.

## Completeness check

For each affected capability confirm the slice in
`agents/product/opsec-product-completeness.md`. Binding rulings:
- A capability with no reachable form is NOT complete.
- A routine staff or operator job that needs SSH + SQL is NOT complete.
- A documented claim the code does not honour must be fixed (code or doc).
- Anything that needs browser JavaScript, or touches mainnet, is unacceptable.
- Label evidence tiers honestly; merged is not released is not deployed.

## Round D — ruling

No majority vote. Use the DECISION block in `protocol.md` for every disputed
item, keeping each dissenting view. Then update `.claude/war-room/ledger.md`:
new rows with IDs, P0–P3 / DECLINED, acceptance criteria and test plan
(short form in the row, full form below the table if needed), and an
iteration-log line that says `swept <area> @<HEAD sha>` (the next delta
sweep starts there). For each P0/P1 row, name the files and functions the
fix will touch, in the row or its ruling (paths and symbols, not line
numbers, which go stale); the loop hands them to the implementer. Use the
rotation's area name in `swept <area>` so the grep finds it. Edit at anchors
found with `ledger.py grep`; keep the ID in column 1 and the status in the last column so `ledger.py live` finds the row.
`live` reads only table rows: every open finding or sub-item (A-652a, PAY91-1)
gets its own row, or is named in its parent row's status cell. Never leave
open work only in a paragraph.
Refresh "Start here" when the next work changes. Report the ruled backlog to
the owner in at most 25 lines.

## Standing rules

- **No code is modified during a war-room session.** Only the ledger changes.
- Implementation happens only when the owner asks — then use
  `/war-room-loop` or dispatch `opsec-implementer` per `protocol.md`.
