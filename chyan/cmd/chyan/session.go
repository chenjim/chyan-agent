// session.go — session JSON export/import subcommands (FR data portability).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"chyan.tech/chyan/internal/app"
	"chyan.tech/chyan/internal/store"
)

// exportCmd snapshots one session to a JSON envelope.
func exportCmd() *cobra.Command {
	f := &runFlags{}
	var out string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export one session to a JSON envelope",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := app.New(app.Options{WorkDir: f.workDir, Home: f.home})
			if err != nil {
				return err
			}
			defer a.Close()
			// Resolve "continue" to the latest session like run/status do.
			sid := f.session
			if sid == "" || sid == "continue" {
				if sid, err = latestSession(cmd.Context(), app.StoreOf(a)); err != nil {
					return err
				}
			}
			ex, err := store.ExportSession(cmd.Context(), app.StoreOf(a), sid)
			if err != nil {
				return err
			}
			raw, err := json.MarshalIndent(ex, "", "  ")
			if err != nil {
				return err
			}
			if out == "" || out == "-" {
				_, err = os.Stdout.Write(append(raw, '\n'))
				return err
			}
			return os.WriteFile(out, append(raw, '\n'), 0o600)
		},
	}
	cmd.Flags().StringVar(&f.home, "home", "", "chyan data root (default ~/.chyan)")
	cmd.Flags().StringVar(&f.workDir, "work-dir", "", "tool work directory (default: cwd)")
	cmd.Flags().StringVar(&f.session, "session", "continue", "session id (or \"continue\")")
	cmd.Flags().StringVar(&out, "out", "", "output file (default stdout)")
	return cmd
}

// importCmd loads a JSON envelope (chyan or opencode) as a fresh session.
func importCmd() *cobra.Command {
	f := &runFlags{}
	var file string
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import a session JSON envelope (chyan or opencode) as a new session",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				return fmt.Errorf("--file is required")
			}
			raw, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			a, err := app.New(app.Options{WorkDir: f.workDir, Home: f.home})
			if err != nil {
				return err
			}
			defer a.Close()
			sid, err := importSession(cmd.Context(), app.StoreOf(a), raw)
			if err != nil {
				return err
			}
			fmt.Printf("imported session %s\n", sid)
			return nil
		},
	}
	cmd.Flags().StringVar(&f.home, "home", "", "chyan data root (default ~/.chyan)")
	cmd.Flags().StringVar(&f.workDir, "work-dir", "", "tool work directory (default: cwd)")
	cmd.Flags().StringVar(&file, "file", "", "session JSON file to import")
	return cmd
}

// importSession tries the chyan envelope first, then the opencode document.
func importSession(ctx context.Context, st store.Store, raw []byte) (string, error) {
	ex, err := store.ParseSessionExport(raw)
	if err == nil {
		return store.ImportSession(ctx, st, ex)
	}
	return store.ImportOpencodeSession(ctx, st, raw)
}

// latestSession resolves the most recently updated session id.
func latestSession(ctx context.Context, st store.Store) (string, error) {
	sessions, err := st.ListSessions(ctx, 1, 0)
	if err != nil {
		return "", err
	}
	if len(sessions) == 0 {
		return "", fmt.Errorf("no sessions yet")
	}
	return sessions[0].ID, nil
}

// memoryCmd exposes memory maintenance (`chyan memory import`): importing
// hermes memory markdowns is explicit, mirroring `chyan cron import`.
func memoryCmd() *cobra.Command {
	f := &runFlags{}
	cmd := &cobra.Command{Use: "memory", Short: "Manage agent memory"}
	importC := &cobra.Command{
		Use:   "import [USER.md|MEMORY.md ...]",
		Short: "Import hermes memory markdowns into the global memory root",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				home, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				args = []string{
					filepath.Join(home, ".hermes", "memories", "USER.md"),
					filepath.Join(home, ".hermes", "memories", "MEMORY.md"),
				}
			}
			a, err := app.New(app.Options{WorkDir: f.workDir, Home: f.home})
			if err != nil {
				return err
			}
			defer a.Close()
			total := 0
			for _, p := range args {
				n, err := a.ImportHermesMemory(cmd.Context(), p)
				if err != nil {
					fmt.Fprintf(os.Stderr, "跳过 %s: %v\n", p, err)
					continue
				}
				total += n
				fmt.Printf("imported %d entries from %s\n", n, p)
			}
			fmt.Printf("共导入 %d 条记忆\n", total)
			return nil
		},
	}
	bindRunFlags(importC, f)
	cmd.AddCommand(importC)
	return cmd
}
