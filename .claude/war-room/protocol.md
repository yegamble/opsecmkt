# War-room protocol — how the rounds run

The chair (the main session) runs the rounds. Seats obey their round.

## Round 0 — shared evidence pack (chair, before any seat is dispatched)

Enumeration is not judgement. Before Round A the chair builds one evidence pack
for the scope with a cheap retrieval pass (`Explore` agent, low/medium effort):
relevant routes (`routes.go` action/page registries), handlers, templates,
migrations, tests per evidence tier, docs claims (`implementation-status.md`,
`acceptance.md`) and the open items in `.claude/war-room/ledger.md`. Every seat
reads the pack and greps only to chase what it missed — saying so when it does.

## Effort per round (the chair's own passes)

| Round | Effort | Why |
|---|---|---|
| 0 — evidence | low / medium | enumeration |
| A — blind review | high (seat frontmatter) | open-ended investigation |
| B — cross-examination | high (fixed per seat) | bounded four-part response |
| C — rebuttal | high (fixed per seat) | bounded response |
| D — ruling | high | ambiguous judgement concentrates here |

`effort` is fixed in each seat's frontmatter; there is no per-invocation
override. The table guides the chair's passes and whether a scope justifies
editing a seat up to `xhigh`.

## Round A — blind review

Each seat investigates independently, never seeing another seat's output.
Output in the shared finding format, grouped:

```
BLOCKERS
REQUIRED
SHOULD
EXPERIMENT
NOT WORTH DOING   ← mandatory: in-scope ideas you decline, with why
```

Close with a **Position summary**: at most five lines — what you believe and
what evidence would change your mind.

## Round B — cross-examination

Each seat receives the others' Round A output (via `SendMessage`, so it keeps
its context) and MUST return all four:

1. **CHALLENGE** — attack at least one substantive recommendation of another
   seat: name the seat, the finding and the evidence that undermines it.
2. **OVER-ENGINEERED** — exactly one proposal (yours included) that costs more
   than the failure it prevents.
3. **MISSED** — one risk inside your expertise that everyone overlooked.
4. **DEFENCE** — answer every challenge aimed at you, with evidence.

## Round C — rebuttal

```
RETRACTED: <finding> — <who disproved it and how>
REVISED:   <finding> — <old> → <new> — <what changed it>
HELD:      <finding> — <the challenge> — <why it does not land>
```

Changing position under good evidence is success. Holding an indefensible
position is the failure.

## Round D — ruling (chair only)

No majority vote. For every disputed item:

```
DECISION:            ACCEPT | MODIFY | EXPERIMENT | DEFER | REJECT | BLOCK RELEASE
WHY:
DISSENTING VIEW:     (name the seat; never delete a losing argument)
USER IMPACT:         buyer / vendor / moderator / administrator
OPERATOR IMPACT:
TECHNICAL COST/RISK:
ACCEPTANCE CRITERIA:
TEST PLAN:
```

Then one prioritised backlog written to `.claude/war-room/ledger.md`:
**P0** release blockers · **P1** required for a coherent product · **P2**
high-value improvements · **P3** experiments · **DECLINED** with reasons.

## The chair seat

Run the chair as an **Opus session with `/advisor fable`**: Fable steers panel
selection and the Round D ruling (its architect seat, ~5–15% of the work); Opus
runs the rounds and the plumbing. Never add wording that asks a model to show
or echo its reasoning to this protocol or to any seat prompt — it silently
reroutes Fable to Opus. "What would change your mind" is a falsification
condition, not a reasoning echo, and is allowed.

## Implementation rules (only when the owner has authorised implementation)

- Review seats never edit. Implementation uses `opsec-implementer`, one ledger
  item per implementer, each in its own git worktree (`isolation: "worktree"`).
- Never run two implementers on overlapping files in the same wave; the chair
  groups items by the files they touch.
- Each implementer follows `.claude/review-loop.md` and `.ralph/AGENT.md`:
  test first, one coherent slice, the gate from `repo-map.md` pasted into its
  report, commits on its own branch, never pushes or merges.
- The chair integrates branches, pushes, opens the PR and waits for **CI
  passed**. Only the owner merges to `main` unless they explicitly delegate it
  for this run.
- A fix is closed only when the seat that raised it (or `opsec-qa-release`)
  re-checks the acceptance criteria against the integrated code.
