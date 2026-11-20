package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	cases := map[string]string{
		"~/INSIGHTS/fyers": filepath.Join(home, "INSIGHTS/fyers"),
		"~":                home,
		"data":             "data",
		"/abs/~x":          "/abs/~x",
		"~other/x":         "~other/x",
	}
	for in, want := range cases {
		if got := expandHome(in); got != want {
			t.Errorf("expandHome(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadBackupConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "b.yaml")
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	c, err := loadBackupConfig(write("drive: /mnt/x\npairs:\n  - {from: ~/a, to: A}\n  - {from: data, to: MARKET_DATA}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Drive != "/mnt/x" || len(c.Pairs) != 2 || strings.HasPrefix(c.Pairs[0].From, "~") || c.Pairs[1].From != "data" {
		t.Errorf("got %+v", c)
	}
	for _, bad := range []string{
		"pairs:\n  - {from: a, to: A}\n",
		"drive: /mnt/x\n",
		"drive: /mnt/x\npairs:\n  - {from: a}\n",
		"drive: /mnt/x\npairs:\n  - {from: a, to: /etc}\n",
		"drive: /mnt/x\npairs:\n  - {from: a, to: ../up}\n",
	} {
		if _, err := loadBackupConfig(write(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestCheckSource(t *testing.T) {
	root := t.TempDir()
	drive := filepath.Join(root, "drive")
	full := filepath.Join(root, "full")
	empty := filepath.Join(root, "empty")
	for _, d := range []string{filepath.Join(drive, "MARKET_DATA"), full, empty} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(full, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(drive, "MARKET_DATA", "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "data")
	if err := os.Symlink(filepath.Join(drive, "MARKET_DATA"), link); err != nil {
		t.Fatal(err)
	}

	if got, err := checkSource(full, drive); err != nil || got != full {
		t.Errorf("full: %q %v", got, err)
	}
	if _, err := checkSource(filepath.Join(root, "missing"), drive); err == nil {
		t.Error("missing accepted")
	}
	if _, err := checkSource(empty, drive); err == nil {
		t.Error("empty accepted")
	}
	if _, err := checkSource(link, drive); err == nil {
		t.Error("symlink onto the drive accepted")
	}
	if _, err := checkSource(filepath.Join(full, "f"), drive); err == nil {
		t.Error("file accepted")
	}
}

func TestCheckMountedRejectsPlainDir(t *testing.T) {
	d := filepath.Join(t.TempDir(), "drive")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkMounted(d); err == nil {
		t.Error("plain directory accepted as a mounted drive")
	}
	if err := checkMounted(filepath.Join(d, "nope")); err == nil {
		t.Error("missing drive accepted")
	}
}
