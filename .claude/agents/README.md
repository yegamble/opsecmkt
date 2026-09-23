# The OPSEC Market war room

Thirteen read-only review seats and one implementer, checked into this repo,
that scrutinise OPSEC Market from genuinely different positions and loop until
it is a complete, releasable test-network product. Adapted from the Vidra
council (`~/github/vidra/.claude/agents`) for a single Go/PostgreSQL repository
with no browser JavaScript and test-network-only payments.

## Launch

```
/war-room <scope>            # one council session: review + ruling → ledger
/war-room-loop [scope|resume] [max-iterations=N] [merge=owner|delegated]
                             # review → rule → implement → integrate → verify, repeated
/scrutinise <claim>          # existing single-task completion check
```

Run the chair as an **Opus session with `/advisor fable`** (see
`.claude/war-room/protocol.md`). The chair picks **3–5** seats per session —
never all thirteen.

## Roster

| Agent | Seat | Primary question |
|---|---|---|
| `opsec-architect` | principal architect | Does this fit the system — and will the next change still fit? |
| `opsec-backend` | Go/PostgreSQL engineer | Does the server do exactly what the form claims, for every caller and race? |
| `opsec-payments` | payments engineer | Can any event sequence pay the wrong party, pay twice or lose a refund? |
| `opsec-security` | application security | What can an attacker reach? |
| `opsec-privacy` | privacy & anonymity | What does the system reveal about its users? |
| `opsec-infrastructure` | self-hosting SRE | Can an operator install, run, upgrade and recover it from the docs? |
| `opsec-frontend` | UI & accessibility | Can anyone finish every task with scripts off, on any device? |
| `opsec-qa-release` | QA / release lead | Does it work end to end, and what evidence tier proves it? Verifies fixes. |
| `opsec-product-completeness` | principal PM | Is each claimed capability a complete vertical slice? |
| `opsec-buyer` | buyer advocate | Would a careful buyer understand every step and every failure? |
| `opsec-vendor` | vendor advocate | Can a vendor run the shop from the desk alone? |
| `opsec-staff` | moderator/admin advocate | Can staff run and recover the market without SSH or SQL? |
| `opsec-devils-advocate` | adversarial reviewer | What are we fooling ourselves about? |
| `opsec-implementer` | implementation engineer | One ruled ledger item, test-first, in its own worktree. |

## Shared contracts

- `.claude/war-room/repo-map.md` — layout, invariants, verification gates,
  evidence tiers and traps. Binding for every seat.
- `.claude/war-room/finding-format.md` — the one finding schema and severities.
- `.claude/war-room/protocol.md` — Round 0 evidence → A blind → B
  cross-examination → C rebuttal → D ruling; implementation rules.
- `.claude/war-room/ledger.md` — the persistent backlog and iteration log; the
  loop's source of truth. Seeded with the 2026-09-23 audit.
- `CLAUDE.md`, `.claude/review-loop.md`, `.ralph/AGENT.md` — binding on the
  implementer.

## Design decisions

- **Review seats are read-only** (`Read, Grep, Glob, Bash`; security, privacy
  and infrastructure also have web access). They may run tests against
  disposable databases they start, but never edit, commit or push. Only
  `opsec-implementer` has `Edit`/`Write`, and it never pushes or merges.
- **All seats run on Opus with `effort: high`** in frontmatter, as in Vidra:
  the seats both suffer and perform cross-examination, so a weaker challenger
  weakens the mechanism. Cost is controlled by seating 3–5, the cheap Round 0
  evidence pack, and not re-reviewing already-ruled items.
- **Security and privacy are separate seats.** Security asks what an attacker
  can do; privacy asks what a seized server, backup or network observer
  learns — the product's core promise deserves its own reviewer.
- **Buyer, vendor and staff are separate** because they want different things
  (protection, control, oversight); the tension should surface.
- **One implementer per ledger item, one worktree each, no overlapping files
  per wave.** The chair integrates, runs the full gate, opens one PR and waits
  for the required **CI passed** check. Merging to `main` stays with the owner
  unless the loop is started with `merge=delegated`.
- **The existing Stop/SubagentStop scrutiny hook still applies** to every seat
  and implementer; it is a reminder, not proof.
- New agent definitions are picked up when a Claude Code session starts; restart
  the session after pulling changes to this directory.
