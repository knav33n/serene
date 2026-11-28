package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"serene/internal/auth"
	"serene/internal/market"
)

// runFyersSync brings the Fyers store up to date: the F&O bhavcopies and the
// lots, strikes and universe derived from them (new expiries need them), then
// every underlying's missing spot days and expired options and futures. A
// Fyers login starts it by itself (startFyersSync). Only one sync or all runs
// at a time, so two never share Fyers' per-second cap.
func runFyersSync(ctx context.Context, s *fyersSession, names string) error {
	unlock, err := lockFyersAll(s.cfg.Dir)
	if err != nil {
		return err
	}
	defer unlock()
	stamp("=== bhavcopies and lots")
	if err := runFyersLots(ctx, s); err != nil {
		return fmt.Errorf("lots: %w", err)
	}
	// The universe may have grown, so read it after the lots.
	us, err := s.pick(names)
	if err != nil {
		return err
	}
	return runFyersAll(ctx, s, us)
}

// lockFyersAll takes <dir>/fetch/all.lock, held until the returned func or the
// process ends.
func lockFyersAll(dir string) (func(), error) {
	path := filepath.Join(dir, "fetch", "all.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("another serene fyers all/sync is already running")
		}
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// fyersFetchRunning reports a running `serene fyers all|sync`, found in /proc
// so that runs from binaries older than the lock count too.
func fyersFetchRunning() (int, bool) {
	procs, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	self := os.Getpid()
	for _, p := range procs {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		args := bytes.Split(bytes.TrimRight(b, "\x00"), []byte{0})
		for i := 0; i+1 < len(args); i++ {
			if string(args[i]) == "fyers" && (string(args[i+1]) == "all" || string(args[i+1]) == "sync") &&
				filepath.Base(string(args[0])) == "serene" {
				var pid int
				fmt.Sscan(filepath.Base(filepath.Dir(p)), &pid)
				if pid != self {
					return pid, true
				}
			}
		}
	}
	return 0, false
}

// startFyersSync starts `serene fyers sync` in the background after a Fyers
// login, logging to logs/fyers-all-<date>.log. A run already going isn't
// doubled: it picks up the new token itself within five minutes.
func startFyersSync(report func(auth.Delivery)) error {
	const to = "fyers data"
	if pid, ok := fyersFetchRunning(); ok {
		report(auth.Delivery{To: to, State: "ok"})
		fmt.Printf("fyers: download already running (pid %d); it picks up the new token within 5 minutes\n", pid)
		return nil
	}
	report(auth.Delivery{To: to, State: "sending"})
	fail := func(err error) error {
		report(auth.Delivery{To: to, State: "failed", Error: err.Error()})
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	if err := os.MkdirAll("logs", 0o755); err != nil {
		return fail(err)
	}
	logPath := filepath.Join("logs", "fyers-all-"+market.DateOf(time.Now()).String()+".log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fail(err)
	}
	defer log.Close()
	cmd := exec.Command(exe, "fyers", "sync")
	cmd.Stdout, cmd.Stderr = log, log
	// Its own session, so stopping serene auth doesn't stop the download.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fail(err)
	}
	go cmd.Wait() // reap it if serene auth outlives it
	report(auth.Delivery{To: to, State: "ok"})
	fmt.Printf("fyers: started the missing-data download (pid %d), log %s\n", cmd.Process.Pid, logPath)
	return nil
}
