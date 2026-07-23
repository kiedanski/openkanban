# Configuration Guide

OpenKanban configuration lives in `~/.config/openkanban/config.json`.

## Default Configuration

```json
{
  "defaults": {
    "default_agent": "opencode",
    "branch_prefix": "task/",
    "branch_naming": "template",
    "branch_template": "{prefix}{slug}",
    "slug_max_length": 40,
    "auto_spawn_agent": true,
    "auto_create_branch": true
  },
  "agents": {
    "opencode": {
      "command": "opencode",
      "args": [],
      "status_file": ".opencode/status.json"
    },
    "claude": {
      "command": "claude",
      "args": ["--dangerously-skip-permissions"],
      "status_file": ".claude/status.json"
    },
    "gemini": {
      "command": "gemini",
      "args": ["--yolo"]
    },
    "codex": {
      "command": "codex",
      "args": ["--full-auto"]
    },
    "aider": {
      "command": "aider",
      "args": ["--yes"]
    }
  },
  "ui": {
    "theme": "catppuccin-mocha",
    "show_agent_status": true,
    "refresh_interval": 5,
    "column_width": 40,
    "ticket_height": 4,
    "sidebar_visible": true,
    "scrollback_lines": 10000
  },
  "cleanup": {
    "delete_worktree": true,
    "delete_branch": false,
    "force_worktree_removal": false
  },
  "behavior": {
    "confirm_quit_with_agents": true
  },
  "daemon": {
    "autostart": true
  },
  "opencode": {
    "server_enabled": true,
    "server_port": 4096,
    "poll_interval": 1,
    "startup_timeout": 10
  }
}
```

## Agents

Define any CLI-based agent. The command runs in the ticket's worktree directory.

```json
{
  "agents": {
    "my-agent": {
      "command": "my-agent-cli",
      "args": ["--flag", "value"],
      "env": {
        "CUSTOM_VAR": "value"
      },
      "init_prompt": "Custom prompt template with {{.Title}} and {{.Description}}"
    }
  }
}
```

Each agent supports:

- `command` — the binary to run (looked up on `PATH`). Multiple agents may share the same `command`.
- `args` — default CLI arguments.
- `env` — environment variables injected into the agent's process. A leading `~/` in a value is expanded to your home directory.
- `label` — display name shown in the sidebar and status toasts (defaults to the config key).
- `status_file` — where the agent writes status, relative to the worktree.
- `init_prompt` — a per-agent prompt template override (inline string).
- `init_prompt_file` — a path to a file whose contents become the prompt template, so a long custom starting prompt lives in a file instead of inline JSON. When set and readable it takes precedence over `init_prompt`. A leading `~/` expands to your home; a relative path resolves against the config directory. An unreadable or blank file is ignored (it falls through to `init_prompt`, then the built-in default), so a bad link never blanks the prompt or blocks a spawn. Editable in the project/agent editor (`e`) as the agent's `prompt:` field. Both `init_prompt` and `init_prompt_file` also exist under `defaults` as a global fallback when no per-agent value applies.

The shipped default prompt is intentionally generic and does **not** invoke any personal skill (e.g. `/prime`) — that would fail with "Unknown skill" wherever the skill isn't installed. To open every session with your own priming command or preamble, put it in a file and point `init_prompt_file` at it.

Argument wrapping and status detection are keyed off `command`, so an agent whose `command` is `claude` is treated as Claude regardless of its config key.

### Multiple Claude profiles (e.g. work vs personal)

Claude Code selects its account/config via the `CLAUDE_CONFIG_DIR` environment variable. To manage projects for two Claude installs, define two agents that share `command: "claude"` and differ only by `env`:

```json
{
  "agents": {
    "claude": {
      "label": "Claude (Default)",
      "command": "claude",
      "args": ["--dangerously-skip-permissions"],
      "status_file": ".claude/status.json"
    },
    "claude-custom": {
      "label": "Claude (Custom)",
      "command": "claude",
      "args": ["--dangerously-skip-permissions"],
      "env": { "CLAUDE_CONFIG_DIR": "~/.claude-personal" },
      "status_file": ".claude/status.json"
    }
  }
}
```

Both presets ship by default; edit `claude-custom`'s `CLAUDE_CONFIG_DIR` to point at your alternate config directory. New built-in agents are merged into an existing `config.json` on load, so the second preset appears automatically after an upgrade.

### Per-project agent (the pin)

Which agent launches is chosen **per project, and nowhere else** — there is no per-ticket or global agent picker. Focus a project in the sidebar (`Tab`, then `j`/`k`) and press `g` to cycle its pinned agent; the choice is saved to the project and shown beneath its name. Every ticket in that project — including background spawns (`Ctrl+Space`) — launches the pinned agent.

A project with **no pin refuses to spawn** (`Pin a Claude for this project first — press g in the sidebar`). This is deliberate: it makes it impossible to accidentally launch the wrong Claude as long as you stay within your projects. Pin each project once.

`defaults.default_agent` is no longer used to choose the spawn agent; it is auto-detected from `PATH` and only influences whether the OpenCode server is pre-started.

### Per-project model override (claude only)

When a project has a **Model** set, openkanban passes `--model <value>` to the `claude` CLI on every spawn (new session and resume). Blank means no flag — claude uses its own configured default.

Set it in the project editor (`e`), **Model** field: use `←`/`→` to cycle the presets (`opus` / `opusplan` / `sonnet`) or type any full model ID. The setting only applies to claude-class agents; other agents (opencode, gemini, codex) ignore it.

### Per-project ticket-brief ignore

When a project has **Briefs** set to `ignore`, openkanban appends `tickets/` to the worktree's `.git/info/exclude` at agent spawn so the generated ticket brief (`tickets/<slug>.md`) is never picked up by `git add -A` and never committed into that repo. The brief file is still written to disk — the agent can read it for full context — it is simply git-excluded locally.

Set it in the project editor (`e`), **Briefs** field: press `←`/`→` to toggle between `land` (default — briefs can be committed, current behaviour) and `ignore` (briefs are excluded from git).

**How it works:** uses `.git/info/exclude`, not a committed `.gitignore`. That file is never tracked, works on any git clone without polluting the repo for other contributors, and is automatically scoped to the correct common git directory in linked-worktree setups.

**Turning it off again:** `.git/info/exclude` entries are append-only; flipping back to `land` does not remove the existing `tickets/` line. The line is harmless if you later want to commit briefs — remove it manually from `.git/info/exclude` with any text editor.

**Backup:** `openkanban backup` archives the repo's `tickets/` directory directly from the filesystem (not via git), so backup snapshots still capture briefs regardless of this setting. That is intentional: a local backup is not a repo commit, so the "don't land ticket files in this repo" goal is fully satisfied by the git-exclude alone.

```json
{
  "ignore_ticket_briefs": true
}
```
*(JSON field in `projects.json` under the project's `settings` key. Managed by the TUI editor; hand-editing is safe.)*

### Editing agents in the TUI (`e`)

Focus a project in the sidebar and press **`e`** for a unified editor that edits **both** the project (name + pinned agent + model → `projects.json`) **and** the shared agent registry (→ `config.json`) in one screen. Per agent you can edit the **label, command, args, env, and prompt-file** (the `prompt:` field — a path to an `init_prompt_file`, e.g. point `claude-custom`'s `CLAUDE_CONFIG_DIR` at a different directory, or link a custom starting prompt) and set its **enabled** state. `Tab`/`↑`/`↓` move between fields, `←`/`→` toggle selectors (the pin and each agent's enabled state), `Ctrl+S` saves, `Esc` cancels. (`g` remains a quick one-press cycle of just the pin.)

### Enabling / disabling agents

Each agent has an `enabled` tri-state controlling whether it's offered in the pin cycle and editor:

- omitted / `auto` (default) — shown only when its `command` resolves on `PATH`, so agents you haven't installed (aider, codex, …) don't clutter the picker.
- `true` — always shown, even if the command isn't on `PATH`.
- `false` — always hidden.

Set it in the `e` editor (the enabled selector) or by hand:

```json
{ "agents": { "aider": { "command": "aider", "enabled": false } } }
```

A project pinned to a disabled agent still spawns it (disabled only hides it from selection).

### Init Prompt Variables

When spawning an agent, OpenKanban can inject ticket context:

- `{{.Title}}` - Ticket title
- `{{.Description}}` - Ticket description
- `{{.BranchName}}` - Git branch name
- `{{.BaseBranch}}` - Base branch (e.g., main)

## Branch Naming

Control how branches are named:

```json
{
  "defaults": {
    "branch_prefix": "feature/",
    "branch_template": "{prefix}{slug}",
    "slug_max_length": 30
  }
}
```

A ticket titled "Add user authentication" becomes branch `feature/add-user-authentication`.

## Cleanup Behavior

When deleting tickets:

```json
{
  "cleanup": {
    "delete_worktree": true,
    "delete_branch": false,
    "force_worktree_removal": false
  }
}
```

- `delete_worktree` - Remove the git worktree directory
- `delete_branch` - Also delete the git branch
- `force_worktree_removal` - Force removal even with uncommitted changes

## Behavior

Application behavior preferences:

```json
{
  "behavior": {
    "confirm_quit_with_agents": true
  }
}
```

- `confirm_quit_with_agents` - Prompt before quitting when agents are running (default: true). Set to false to auto-close agents without confirmation.

## UI

Display preferences:

```json
{
  "ui": {
    "sidebar_visible": true,
    "scrollback_lines": 10000
  }
}
```

- `sidebar_visible` - Show project sidebar on startup (default: true). Toggle with `[` key during use.
- `scrollback_lines` - Number of lines to keep in terminal scrollback buffer (default: 10000). The scrollback buffer stores terminal output that has scrolled off-screen, allowing you to scroll back through agent history with mouse wheel or Shift+PgUp/PgDn.

## Themes

OpenKanban supports multiple color themes. Set the theme in your config:

```json
{
  "ui": {
    "theme": "tokyo-night"
  }
}
```

### Available Themes

**Dark themes:**
- `catppuccin-mocha` (default) - Warm dark theme
- `catppuccin-macchiato` - Slightly lighter Catppuccin
- `catppuccin-frappe` - Medium Catppuccin
- `tokyo-night` - Cool blue dark theme
- `tokyo-night-storm` - Darker Tokyo Night variant
- `gruvbox-dark` - Retro warm dark theme
- `nord` - Arctic blue theme
- `dracula` - Purple-accented dark theme
- `one-dark` - Atom-inspired theme
- `solarized-dark` - Classic low-contrast dark
- `rose-pine` - Muted warm dark theme
- `rose-pine-moon` - Lighter Rose Pine
- `kanagawa` - Japanese-inspired theme
- `everforest-dark` - Nature-inspired dark

**Light themes:**
- `catppuccin-latte` - Light Catppuccin
- `tokyo-night-light` - Light Tokyo Night
- `gruvbox-light` - Retro warm light theme
- `solarized-light` - Classic low-contrast light
- `rose-pine-dawn` - Light Rose Pine
- `everforest-light` - Nature-inspired light

### Custom Colors

Override specific colors while using a base theme:

```json
{
  "ui": {
    "theme": "catppuccin-mocha",
    "custom_colors": {
      "primary": "#7aa2f7",
      "success": "#9ece6a"
    }
  }
}
```

Available color fields:

**Backgrounds:** `base`, `surface`, `overlay`

**Text:** `text`, `subtext`, `muted`

**Semantic accents:**
- `primary` - Main accent (focus, selection, backlog column)
- `secondary` - Secondary accent (in-review column, special highlights)
- `success` - Positive states (done column, confirmations)
- `warning` - Caution states (in-progress column)
- `error` - Errors and destructive actions
- `info` - Informational elements

## Daemon

Controls how the TUI interacts with `openkanbankd`, the per-user daemon that owns long-lived agent PTYs:

```json
{
  "daemon": {
    "autostart": true
  }
}
```

- `autostart` - When `true` (default), the TUI forks `openkanban daemon` on launch if no daemon is currently running. When `false`, the TUI dials the existing daemon and degrades to "no agent spawn" mode if none is found. Set this to `false` after running `openkanban daemon install-service` so the launchd-managed daemon owns the lifecycle without the TUI racing it for the pidlock.

The CLI flag `--no-launch-daemon` overrides `autostart=true` for a single invocation. It is intentionally one-way: passing `=false` does NOT force autostart on. To re-enable autostart permanently, set `daemon.autostart` back to `true` in this file.

See [AGENT_INTEGRATION.md → Identity, mutex, paths](AGENT_INTEGRATION.md#identity-mutex-paths) for the two supported lifecycle modes (default TUI-managed vs system-managed via `openkanban daemon install-service`).

## OpenCode Integration

OpenKanban has deep integration with OpenCode. When enabled, it starts an OpenCode server and connects ticket terminals to it for accurate status detection.

```json
{
  "opencode": {
    "server_enabled": true,
    "server_port": 4096,
    "poll_interval": 1,
    "startup_timeout": 10
  }
}
```

- `server_enabled` - Start OpenCode server for enhanced status detection (default: true). When enabled, ticket terminals use `opencode attach` to connect to the shared server.
- `server_port` - Port for the OpenCode server (default: 4096). If a server is already running on this port, OpenKanban will reuse it.
- `poll_interval` - Agent status polling interval in seconds (default: 1).
- `startup_timeout` - Timeout in seconds for OpenCode server to become ready (default: 10).

When `server_enabled` is false, OpenCode runs in standalone mode per-ticket with basic status detection.

## Claude Code Integration

When using Claude Code with the [oh-my-claude](https://github.com/TechDufus/oh-my-claude) plugin, OpenKanban automatically receives live status updates. No configuration required.

**How it works:** OpenKanban sets `OPENKANBAN_SESSION` in agent terminals. oh-my-claude detects this and writes status to `~/.cache/openkanban-status/`. Status updates appear in real-time on your ticket cards.

| Status | Meaning |
|--------|---------|
| `idle` | Ready for input |
| `working` | Processing prompt or tools |
| `waiting` | Awaiting user permission |

To enable: Install [oh-my-claude](https://github.com/TechDufus/oh-my-claude) in Claude Code. That's it.

## In-App Settings

Press `O` to open the settings menu. You can configure these options without editing the config file:

| Setting | Description |
|---------|-------------|
| Theme | Color theme (use j/k to navigate, live preview) |
| Confirm Quit | Prompt before quitting with running agents |
| Branch Prefix | Prefix for auto-generated branch names |
| Delete Worktree | Remove git worktree when deleting tickets |
| Delete Branch | Delete git branch when deleting tickets |
| Force Cleanup | Force worktree removal even with uncommitted changes |
| Show Sidebar | Toggle project sidebar visibility |
| Filter Project | Show only tickets from a specific project |

Changes are saved immediately to `~/.config/openkanban/config.json`.

## Ticket Labels and Priority

Tickets support labels and priority levels:

**Labels**: Comma-separated tags (e.g., `bug, urgent, frontend`). Labels appear on ticket cards and can help organize work.

**Priority**: 1 (Critical) to 5 (Lowest). High-priority tickets (1-2) show a visual indicator on the card:
- `!!` - Critical (priority 1)
- `!` - High (priority 2)

Set labels and priority when creating or editing a ticket (`n` or `e`).

## Ticket Storage

Tickets are stored as Markdown files with YAML frontmatter, one file per ticket:

```
~/.config/openkanban/
├── config.json
├── projects.json
└── tickets/
    └── <project_id>/
        ├── <slug>-<uuid8>.md
        └── ...
```

Filename is cosmetic — identity comes from the `id` field in frontmatter. Renaming a ticket via the TUI changes the filename but preserves identity; an interrupted rename is reconciled on the next load (newer mtime wins).

**Editability**: Any `.md` file is fair game for `$EDITOR`. Save changes to the frontmatter (status, priority, labels) or the body (description) and the running TUI reflects them within ~150ms via fsnotify.

**Validation**: `status`, `agent_status`, and `agent_type` are validated against enum allowlists on load. A malformed value surfaces as a TUI notification and an entry in `~/.config/openkanban/watch-errors.log`; the prior in-memory copy is kept until you fix and save again.

**Migration**: On first launch after upgrading from a pre-Markdown release, the legacy `tickets/<project_id>.json` is converted to per-ticket `.md` files automatically. The original JSON is preserved as `<project_id>.json.migrated` so you can roll back (`mv <id>.json.migrated <id>.json && rm -rf <id>/` and reinstall the older binary).

If a stale legacy JSON ever reappears (e.g. an old binary was launched in another shell), the next load detects it is a strict subset of the per-ticket dir's state and renames it aside to `.stale-<timestamp>` rather than refusing to start.

## Environment Overrides

The config and status directories can be redirected via environment variables — primarily for tests, CI, and running multiple isolated instances:

| Variable | Overrides | Default |
|----------|-----------|---------|
| `OPENKANBAN_CONFIG_DIR` | The config directory (config, projects, tickets) | `~/.config/openkanban` |
| `XDG_CONFIG_HOME` | The config directory parent (`$XDG_CONFIG_HOME/openkanban`); ignored if `OPENKANBAN_CONFIG_DIR` is set | `~/.config` |
| `OPENKANBAN_STATUS_DIR` | The agent status-file directory (where live agent status is written/read) | `~/.cache/openkanban-status` |

`OPENKANBAN_CONFIG_DIR` takes precedence over `XDG_CONFIG_HOME`.

## Command Line

In addition to the TUI, OpenKanban exposes a small command-line surface for scripting — useful when a running agent wants to create a ticket without driving the TUI.

```bash
openkanban ticket new \
    --project <name|uuid|prefix> \
    --title "<title>" \
    [--description "<text>" | --description-file <path>] \
    [--status backlog|next|in_progress|in_review|done|archived] \
    [--labels foo,bar] \
    [--priority 1-5] \
    [--no-worktree] \
    [--session <uuid> [--migrate [--force]]] \
    [--created-by <name>]
```

`--project` accepts an exact project name, full UUID, or a unique 4+ character UUID prefix. Ambiguous prefixes return an error listing the candidates.

The created `.md` file's absolute path is printed to stdout — a parent agent can capture it and pass it as context to a child agent it spawns.

Description may also be piped on stdin:

```bash
echo "Long description" | openkanban ticket new --project myapp --title "Do thing"
```

**Session linking**: `--session <uuid>` attaches a Claude Code session as starting context. On first spawn, openkanban runs `claude --resume <uuid> --fork-session` so the original session is unaffected. Pass `--migrate` to skip the fork (the ticket then owns the session outright); openkanban will refuse to migrate if the session is currently held by another process. Use `--force` with `--migrate` to terminate that process (SIGTERM with 3s grace, then SIGKILL); any unsubmitted prompt in the source session will be lost.

`--created-by <name>` is a free-form audit field. It's not used by the spawn logic — just preserved in the ticket's frontmatter for "where did this come from" provenance.

## Keybindings

All keybindings are shown in-app with `?`. Custom keybindings coming soon.

## Full Keybindings Reference

### Board View

| Key | Action |
|-----|--------|
| `j/k` | Move cursor up/down |
| `h/l` | Move between columns |
| `g` | Go to first ticket |
| `G` | Go to last ticket |
| `space` | Move ticket to next column |
| `-` | Move ticket to previous column |
| `enter` | Attach to running agent |
| `n` | Create new ticket (lands in the focused column; in_review/done route to in_progress) |
| `e` | Edit ticket |
| `E` | Edit ticket description in `$EDITOR` (vim); falls back to `vi` |
| `s` | Spawn agent for ticket |
| `S` | Stop agent |
| `d` | Delete ticket |
| `/` | Search/filter tickets |
| `esc` | Clear filter |
| `tab` | Toggle sidebar focus |
| `[` | Toggle sidebar visibility |
| `O` | Open settings |
| `?` | Show help |
| `q` | Quit |

### Sidebar

| Key | Action |
|-----|--------|
| `h` | Focus sidebar (from column 0) |
| `l` | Return to board |
| `j/k` | Navigate projects |
| `enter` | Select project filter |
| `g` | Pin / cycle the project's agent (which Claude launches) |
| `e` | Edit project + agents (unified editor) |
| `a` | Add project |
| `d` | Delete project |
| `o` | Toggle open-only ticket counts |

### Agent View

| Key | Action |
|-----|--------|
| `ctrl+g` | Return to board |
| All other keys | Passed to agent |
