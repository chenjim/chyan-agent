package main

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBackupRestoreDiagnoseCommands: the three subcommands run end to end
// against a temp data root.
func TestBackupRestoreDiagnoseCommands(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "secret"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("api_key = \"top-secret\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "b.tgz")

	bc := backupCmd()
	bc.SetArgs([]string{"--home", home, "-o", archive})
	if err := bc.Execute(); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if fi, err := os.Stat(archive); err != nil || fi.Size() == 0 {
		t.Fatalf("archive = %v, %v", fi, err)
	}

	// Restore into a fresh home.
	dst := t.TempDir()
	rc := restoreCmd()
	rc.SetArgs([]string{"--home", dst, "-f", archive})
	if err := rc.Execute(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "config.toml")); err != nil {
		t.Error("config missing after restore")
	}
	// Restoring again refuses (chyan.db absent here, but config restore is
	// idempotent-safe; the refusal is covered in the backup package tests).

	// Diagnose: redacted bundle, no secret material (decompressed: the
	// bundle is gzipped, so a raw byte scan would prove nothing).
	diag := filepath.Join(t.TempDir(), "d.tgz")
	dc := diagnoseCmd()
	dc.SetArgs([]string{"--home", home, "-o", diag})
	if err := dc.Execute(); err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	f, err := os.Open(diag)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "top-secret") {
		t.Errorf("diagnostics leaked the api key:\n%s", body)
	}
	if !strings.Contains(string(body), "***") {
		t.Errorf("redaction marker missing:\n%s", body)
	}
}

// TestRestoreRequiresFile: --file is mandatory.
func TestRestoreRequiresFile(t *testing.T) {
	rc := restoreCmd()
	rc.SetArgs([]string{"--home", t.TempDir()})
	if err := rc.Execute(); err == nil {
		t.Error("restore without --file must fail")
	}
}
