---
name: fan-out-a-plan
description: Use at the end of a planning/spec session when the plan decomposes into several discrete tasks. Instead of the user hand-copying each task into a ticket, this creates one openkanban ticket per task and wires their dependencies with --blocked-by, so the execution order is encoded on the board automatically.
---

# Fan out a plan into OpenKanban tickets

## Overview

You've produced a plan that breaks into N discrete tasks — often ordered, where
one task can't start until an earlier one is done. Copying each into a ticket by
hand is tedious and throws away the dependency structure. This skill creates one
ticket per task and links them with `--blocked-by`, so the board reflects the
plan's dependency graph and downstream tickets visibly point at their upstreams.

**Announce at start:** "I'm using the fan-out-a-plan skill to create the tickets
for this plan."

**Core principle:** one ticket per genuinely-separable task, each with a
self-contained brief (see spin-off-a-ticket's brief guidance), linked by real
dependencies — not a flat dump of every bullet point.

## When to use
- A planning/spec session ended with a concrete, ordered list of tasks.
- The user wants those turned into tickets ("create the tickets", "fan these out").

## When NOT to use
- The plan is really one cohesive task — make a single ticket (or just implement it).
- The tasks are still vague — refine the plan first; vague tickets spawn vague sessions.
- Not inside an openkanban session (`$OPENKANBAN_TICKET_ID` unset) — then the caller
  must pass `--project` explicitly on each command.

## The process

### Step 1: Enumerate the tasks
List the tasks in dependency order. For each: a short title, a one-paragraph
brief, and which earlier task(s) it depends on. **Confirm the list with the user
before creating anything** — creating N tickets is not free to undo.

### Step 2: Create tickets in dependency order, capturing ids
Create upstream tasks FIRST so their ids exist to reference. Invoke openkanban via
`"${OPENKANBAN_BIN:-openkanban}"` — `$OPENKANBAN_BIN` is the exact build running the
board (the daemon sets it on every spawned session), so a stale/other `openkanban`
earlier on PATH can't shadow it and reject `ticket`. For each task, write its brief
to a temp file, then:

```bash
# First task (no dependency):
A=$("${OPENKANBAN_BIN:-openkanban}" ticket new --title "Research the auth flow" \
      --description-file /tmp/okb-plan-1.md --json | jq -r .id)

# A task that depends on A:
B=$("${OPENKANBAN_BIN:-openkanban}" ticket new --title "Implement OAuth callback" \
      --description-file /tmp/okb-plan-2.md --blocked-by "$A" --json | jq -r .id)

# A task with multiple upstreams (comma-separated):
"${OPENKANBAN_BIN:-openkanban}" ticket new --title "Review the change" \
  --description-file /tmp/okb-plan-3.md --blocked-by "$A,$B" --json
```

- `--blocked-by` takes a comma-separated list of ticket ids for multiple deps.
- `--project` is derived from `$OPENKANBAN_TICKET_ID`; you don't need to name it.
- Every referenced id must already exist — always create upstreams before the
  tickets that depend on them.

### Step 3: Report the map
Summarize what you created as a short dependency tree: each ticket's title, id,
and what it's blocked by. Do NOT start any of them — the user starts tickets when
ready, and the dependency links are there to signal the order.

## Notes
- Prefer `"${OPENKANBAN_BIN:-openkanban}"` over a bare `openkanban`: `$OPENKANBAN_BIN`
  is the absolute path to the build running the board, so you never hit a stale
  `openkanban` on PATH that lacks the `ticket` command. It falls back to
  `openkanban` when unset (e.g. outside a spawned session).
- Keep each brief self-contained: goal, relevant files, decisions, out-of-scope,
  and an acceptance step that ends in a runnable check.
- If `jq` isn't available, capture the full `--json` object and read its `id` field.
- Dependencies are recorded as links that inform ordering. Hard enforcement —
  refusing to START a ticket whose upstream isn't done — is a separate openkanban
  gate, layered on top of these links.
