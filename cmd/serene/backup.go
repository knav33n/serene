package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// backupConfig maps local data folders onto folders on the backup drive.
type backupConfig struct {
	Drive string       `yaml:"drive"`
	Pairs []backupPair `yaml:"pairs"`
}

type backupPair struct {
	From string `yaml:"from"` // local folder; relative to the working directory, ~/ allowed
	To   string `yaml:"to"`   // folder on the drive, relative to Drive
}

// expandHome turns a leading ~/ into the user's home directory.
func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~"))
}

func loadBackupConfig(path string) (backupConfig, error) {
	var c backupConfig
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if c.Drive == "" {
		return c, fmt.Errorf("%s: drive is empty", path)
	}
	if len(c.Pairs) == 0 {
		return c, fmt.Errorf("%s: no pairs", path)
	}
	c.Drive = expandHome(c.Drive)
	for i, p := range c.Pairs {
		if p.From == "" || p.To == "" {
			return c, fmt.Errorf("%s: pair %d needs both from and to", path, i+1)
		}
		if filepath.IsAbs(p.To) || strings.Contains(filepath.Clean(p.To), "..") {
			return c, fmt.Errorf("%s: pair %d: to %q must be a folder inside the drive", path, i+1, p.To)
		}
		c.Pairs[i].From = expandHome(p.From)
	}
	return c, nil
}

// checkMounted refuses a drive path that is not a mount point, so an
// unplugged drive never gets its backup written onto the local disk.
func checkMounted(drive string) error {
	st, err := os.Stat(drive)
	if err != nil {
		return fmt.Errorf("backup drive %s: %w (is it plugged in?)", drive, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("backup drive %s is not a directory", drive)
	}
	parent, err := os.Stat(filepath.Dir(filepath.Clean(drive)))
	if err != nil {
		return err
	}
	a, ok1 := st.Sys().(*syscall.Stat_t)
	b, ok2 := parent.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 {
		return errors.New("cannot tell whether the backup drive is mounted")
	}
	if a.Dev == b.Dev {
		return fmt.Errorf("backup drive %s is not mounted", drive)
	}
	return nil
}

// checkSource returns the resolved source folder, refusing a missing or
// empty folder or one that resolves onto the drive: with --delete any of
// those would wipe or corrupt the backup.
func checkSource(from, drive string) (string, error) {
	real, err := filepath.EvalSymlinks(from)
	if err != nil {
		return "", fmt.Errorf("source %s: %w", from, err)
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("source %s is not a directory", from)
	}
	if d, err := filepath.EvalSymlinks(drive); err == nil {
		drive = d
	}
	drive, _ = filepath.Abs(drive)
	if real == drive || strings.HasPrefix(real, drive+string(filepath.Separator)) {
		return "", fmt.Errorf("source %s is on the backup drive (%s): it should be a local folder", from, real)
	}
	entries, err := os.ReadDir(real)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("source %s is empty: refusing to mirror it over the backup", from)
	}
	return real, nil
}

// runBackup mirrors each local data folder onto the backup drive with rsync.
func runBackup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	configPath := fs.String("config", "configs/backup.yaml", "backup config")
	dryRun := fs.Bool("dry-run", false, "show what would change, copy nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadBackupConfig(*configPath)
	if err != nil {
		return err
	}
	if _, err := exec.LookPath("rsync"); err != nil {
		return errors.New("rsync is not installed")
	}
	if err := checkMounted(cfg.Drive); err != nil {
		return err
	}
	// Check every source before copying anything.
	srcs := make([]string, len(cfg.Pairs))
	for i, p := range cfg.Pairs {
		if srcs[i], err = checkSource(p.From, cfg.Drive); err != nil {
			return err
		}
	}

	start := time.Now()
	var failed []string
	for i, p := range cfg.Pairs {
		dst := filepath.Join(cfg.Drive, p.To)
		fmt.Printf("\n== %s -> %s\n", p.From, dst)
		if err := backupPairRsync(ctx, srcs[i], dst, *dryRun, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "backup %s: %v\n", p.From, err)
			failed = append(failed, p.From)
			if ctx.Err() != nil {
				break
			}
			continue
		}
	}
	mode := "backup"
	if *dryRun {
		mode = "dry run"
	}
	fmt.Printf("\n%s done in %s: %d of %d folders ok\n", mode, time.Since(start).Round(time.Second), len(cfg.Pairs)-len(failed), len(cfg.Pairs))
	if len(failed) > 0 {
		return fmt.Errorf("backup failed for %s", strings.Join(failed, ", "))
	}
	return nil
}

func backupPairRsync(ctx context.Context, src, dst string, dryRun bool, out io.Writer) error {
	if !dryRun {
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
	}
	// -rt, not -a: the drive is NTFS, which keeps no owner or Unix mode.
	// --modify-window=1 absorbs NTFS timestamp rounding.
	args := []string{"-rt", "--delete", "--modify-window=1", "--info=stats1"}
	if dryRun {
		args = append(args, "--dry-run", "--itemize-changes")
	} else {
		args = append(args, "--info=progress2")
	}
	args = append(args, src+"/", dst+"/")
	cmd := exec.CommandContext(ctx, "rsync", args...)
	cmd.Stdout, cmd.Stderr = out, os.Stderr
	return cmd.Run()
}
