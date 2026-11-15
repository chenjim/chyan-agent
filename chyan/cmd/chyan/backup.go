// backup/restore/diagnose subcommands (P5-4): archive and restore the
// ~/.chyan data root, and emit a redacted diagnostics bundle.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"chyan.tech/chyan/internal/backup"
	"chyan.tech/chyan/internal/config"
)

// homeFlag resolves the data root from --home or the standard location.
func homeFlag(home string) (string, error) {
	if home != "" {
		return filepath.Clean(home), nil
	}
	return config.Home()
}

func backupCmd() *cobra.Command {
	var home, out string
	var includeCache bool
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Archive ~/.chyan to a tar.gz (cache excluded by default)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			h, err := homeFlag(home)
			if err != nil {
				return err
			}
			var w *os.File
			if out == "" || out == "-" {
				w = os.Stdout
			} else {
				if w, err = os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600); err != nil {
					return err
				}
				defer w.Close()
			}
			return backup.Backup(cmd.Context(), h, w, backup.Options{IncludeCache: includeCache})
		},
	}
	cmd.Flags().StringVar(&home, "home", "", "chyan data root (default ~/.chyan)")
	cmd.Flags().StringVarP(&out, "output", "o", "", "output file (default stdout)")
	cmd.Flags().BoolVar(&includeCache, "include-cache", false, "pack cache/ artifacts too")
	return cmd
}

func restoreCmd() *cobra.Command {
	var home, file string
	var force bool
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Restore a backup archive (refuses to overwrite an existing chyan.db without --force)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				return fmt.Errorf("restore: --file is required")
			}
			h, err := homeFlag(home)
			if err != nil {
				return err
			}
			f, err := os.Open(file)
			if err != nil {
				return err
			}
			defer f.Close()
			return backup.RestoreWithOptions(cmd.Context(), f, h, backup.RestoreOptions{Force: force})
		},
	}
	cmd.Flags().StringVar(&home, "home", "", "chyan data root (default ~/.chyan)")
	cmd.Flags().StringVarP(&file, "file", "f", "", "backup archive to restore")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing chyan.db (snapshots the current home first)")
	return cmd
}

func diagnoseCmd() *cobra.Command {
	var home, out string
	cmd := &cobra.Command{
		Use:   "diagnose",
		Short: "Write a redacted diagnostics bundle (no secrets)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			h, err := homeFlag(home)
			if err != nil {
				return err
			}
			var w *os.File
			if out == "" || out == "-" {
				w = os.Stdout
			} else {
				if w, err = os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600); err != nil {
					return err
				}
				defer w.Close()
			}
			return backup.Diagnose(cmd.Context(), h, w, backup.DiagInfo{
				Version: version, OS: runtime.GOOS, Arch: runtime.GOARCH,
			})
		},
	}
	cmd.Flags().StringVar(&home, "home", "", "chyan data root (default ~/.chyan)")
	cmd.Flags().StringVarP(&out, "output", "o", "", "output file (default stdout)")
	return cmd
}
