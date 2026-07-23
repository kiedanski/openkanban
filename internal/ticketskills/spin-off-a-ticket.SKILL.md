---
name: spin-off-a-ticket
description: Use mid-session when a NEW, distinct piece of work surfaces (the classic "yes, let's also do that" / "one more thing" / "while we're here we should…") that deserves its own ticket instead of ballooning the current conversation. Distills the relevant context into a self-contained brief and creates an openkanban ticket for it — optionally reusing the current worktree for an in-place continuation.
---

# Spin off an OpenKanban ticket

## Overview

You are running inside an openkanban-spawned session — a git worktree with
`$OPENKANBAN_TICKET_ID` set, scoped to one ticket. When a distinct *next* piece
of work comes up ("yes, let's also do X", or you notice a real follow-up), the
right move is usually NOT to do it in this conversation. Piling another feature
onto this context bloats it and blurs the work.

Instead, capture it as its own ticket. This mirrors Anthropic's own guidance:
start distinct work in a **fresh session** with a **self-contained spec**, rather
than sprawling one context across many tasks.

**Announce at start:** "I'm using the spin-off-a-ticket skill to capture this as
its own ticket."

**Core principle — distill, don't dump.** Do NOT paste the transcript into the
new ticket. Write a tight, self-contained brief: the few files/interfaces that
matter, the decisions already made, the gotchas you hit, what's out of scope, and
an acceptance/verification step. That distilled brief is exactly what makes the
fresh session effective — it carries the value without the mess, which is the
whole reason spinning off beats continuing here.

## When to use

- This ticket's work is essentially done and a NEW follow-up surfaced.
- The user says some variant of "let's also…", "one more thing", "while we're here…".
- You notice a distinct, separable task worth tracking on its own.

## When NOT to use

- The extra work is genuinely part of THIS ticket's acceptance — just do it here.
- The user asked to finish/close this ticket — use `finishing-an-openkanban-ticket`.
- You are not inside an openkanban session (`$OPENKANBAN_TICKET_ID` unset).

## The process

### Step 1: Confirm scope (one line)
State, in one sentence, the ticket you're about to create: "Spin off: '<title>' —
<one-line scope>." If the intent is ambiguous, ask before creating.

### Step 2: Choose the worktree
Ask the user (AskUserQuestion) which fits:
- **New worktree** (default): the follow-up is independent and could run in
  parallel. openkanban provisions a fresh worktree when the ticket is started.
- **Reuse THIS worktree** (`--worktree-from "$OPENKANBAN_TICKET_ID"`): the
  follow-up is a continuation / next phase of the SAME branch, to run
  sequentially after this one. Pick this only for genuine in-place
  continuations: two live agents may NOT share a worktree (openkanban blocks it),
  so the new ticket has to wait until this session ends before it can start.

### Step 3: Write the brief
Write a self-contained brief to a temp file, e.g. `/tmp/okb-spinoff-<short>.md`:
- **Goal** — one paragraph: what, and why.
- **Relevant files/interfaces** — the handful that matter (paths) and how they relate.
- **Decisions already made** — what's settled, so the new agent doesn't relitigate it.
- **Gotchas** — traps you hit that the new agent would otherwise rediscover.
- **Out of scope** — what NOT to touch.
- **Acceptance** — how to know it's done, ending with a runnable check (build/test/command).

### Step 4: Create the ticket
Run from inside the session (project + parent ticket are derived from the env):

```bash
# New worktree (provisioned lazily when the ticket is started):
openkanban ticket new --title "<title>" \
  --description-file /tmp/okb-spinoff-<short>.md --json

# OR continue in THIS worktree/branch (sequential phase):
openkanban ticket new --title "<title>" \
  --description-file /tmp/okb-spinoff-<short>.md \
  --worktree-from "$OPENKANBAN_TICKET_ID" --json
```

Capture the `id` from the JSON output.

### Step 5: Report, don't auto-start
Tell the user the new ticket's title and id, and that it's parked in the backlog
for them to start when ready. Do NOT spawn or start it yourself — the user
decides when a ticket goes in progress.

## Notes
- `--project` is derived from `$OPENKANBAN_TICKET_ID`; you don't need the project name.
- To link this new ticket after an existing one (a dependency), add
  `--blocked-by "<other-ticket-id>"`.
- If context beyond the brief is worth preserving, you MAY note this session's id
  in the brief so the new agent can consult the transcript — but the brief must
  stand on its own.
