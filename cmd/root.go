package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/techdufus/openkanban/internal/app"
	"github.com/techdufus/openkanban/internal/config"
	"github.com/techdufus/openkanban/internal/finishskill"
	"github.com/techdufus/openkanban/internal/ticketskills"
)

var (
	cfgFile         string
	projectPath     string
	noUpdateCheck   bool
	noLaunchDaemon  bool
)

var rootCmd = &cobra.Command{
	Use:   "openkanban",
	Short: "TUI kanban board for orchestrating AI coding agents",
	Long: `OpenKanban is a terminal-based kanban board that helps you manage
multiple AI coding agents across different tasks and git worktrees.

Each ticket spawns an embedded terminal pane with its own git worktree
for safe parallel development.`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// Refuse to run if this is a stub binary (bare `go install .`
		// produced it, missing the install-time metadata that update,
		// version reporting, and source-clone awareness all depend on).
		// `version` is allowed through so users can see WHY their
		// binary is broken; everything else is gated.
		return guardStubBuild(cmd)
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		cfg, result, err := config.LoadWithValidation(cfgFile)
		if err != nil {
			if result != nil && result.HasErrors() {
				fmt.Fprintf(os.Stderr, "Configuration errors:\n\n%s", result.FormatErrors())
				fmt.Fprintln(os.Stderr, "Run 'openkanban config validate' for details")
				return errors.New("invalid configuration")
			}
			return fmt.Errorf("failed to load config: %w", err)
		}

		if result != nil && result.HasErrors() {
			fmt.Fprintf(os.Stderr, "Configuration errors:\n\n%s", result.FormatErrors())
			fmt.Fprintln(os.Stderr, "Run 'openkanban config validate' for details")
			return errors.New("invalid configuration")
		}

		if result != nil && result.HasWarnings() {
			fmt.Fprintf(os.Stderr, "Config warnings:\n%s\n", result.FormatWarnings())
		}

		isTTY := isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stderr.Fd())

		// Keep the standardized close-out skill in sync with this binary,
		// and surface a one-line hint if its review subagents are missing.
		// Both are best-effort and non-blocking — they run before the
		// update prompt so an update re-exec re-applies the fresh embed.
		if home, herr := os.UserHomeDir(); herr == nil && home != "" {
			if _, serr := finishskill.EnsureInstalled(home); serr != nil {
				fmt.Fprintf(os.Stderr, "openkanban: could not install close-out skill: %v\n", serr)
			}
			if _, serr := ticketskills.EnsureInstalled(home); serr != nil {
				fmt.Fprintf(os.Stderr, "openkanban: could not install ticket-graph skills: %v\n", serr)
			}
			warnMissingAgentsIfNeeded(finishskill.RequiredAgents(), agentResolver(home), isTTY, os.Stderr)
		}

		if handled, err := MaybePromptForUpdate(cfg, isTTY, noUpdateCheck); handled {
			// Either we re-exec'd into the freshly installed binary
			// (in which case this line is unreachable on success) or
			// the user chose to quit. Either way, don't run the TUI.
			return nil
		} else if err != nil {
			fmt.Fprintf(os.Stderr, "update check: %v\n", err)
			// Fall through to app.Run anyway — a failed update should
			// not block the user from working on tickets.
		}

		// Effective autostart = config default (DaemonSettings.Autostart,
		// defaults to true in DefaultConfig) AND NOT --no-launch-daemon.
		// The CLI flag is one-way: it can only suppress autostart, never
		// force it on. See the flag registration comment below.
		autostartDaemon := cfg.Daemon.Autostart && !noLaunchDaemon
		return app.Run(cfg, projectPath, autostartDaemon)
	},
}

func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.config/openkanban/config.json)")
	rootCmd.PersistentFlags().StringVarP(&projectPath, "project", "p", "", "project or repository path")
	rootCmd.PersistentFlags().BoolVar(&noUpdateCheck, "no-update-check", false, "Skip the launch-time check for openkanban updates")
	// --no-launch-daemon is intentionally one-way: passing =true disables
	// the TUI's on-demand daemon fork; passing =false does NOT force
	// autostart (config.daemon.autostart controls the default). Don't
	// refactor this into a tri-state without consulting the design.
	rootCmd.PersistentFlags().BoolVar(&noLaunchDaemon, "no-launch-daemon", false, "Don't autostart openkanbankd; dial existing one or run in degraded mode")

	rootCmd.AddCommand(newCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(deleteCmd)
}

var newCmd = &cobra.Command{
	Use:   "new [name]",
	Short: "Create a new project",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, result, err := config.LoadWithValidation(cfgFile)
		if err != nil || (result != nil && result.HasErrors()) {
			if result != nil && result.HasErrors() {
				fmt.Fprintf(os.Stderr, "Configuration errors:\n\n%s", result.FormatErrors())
				return errors.New("invalid configuration")
			}
			return fmt.Errorf("failed to load config: %w", err)
		}

		repoPath := projectPath
		if repoPath == "" {
			repoPath, _ = os.Getwd()
		}

		repoPath, err = filepath.Abs(repoPath)
		if err != nil {
			return fmt.Errorf("failed to resolve path: %w", err)
		}

		name := filepath.Base(repoPath)
		if len(args) > 0 {
			name = args[0]
		}

		return app.CreateProject(cfg, name, repoPath)
	},
}

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all projects",
	RunE: func(cmd *cobra.Command, args []string) error {
		return app.ListProjects()
	},
}

var deleteCmd = &cobra.Command{
	Use:   "delete <name-or-id>",
	Short: "Delete a project",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return app.DeleteProject(args[0])
	},
}
