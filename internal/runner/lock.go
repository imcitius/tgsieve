package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// LockFile guards a plan directory. Two runs writing the same directory would
// interleave their plans and overwrite each other's provenance, producing a
// report that describes no single moment in time.
const LockFile = ".tgsieve-lock"

// staleAfter is how long a lock from another machine is respected. Liveness
// cannot be checked across hosts, so the only alternative to a timeout is a
// lock that outlives the run that took it.
const staleAfter = 6 * time.Hour

type lockInfo struct {
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
	Host    string    `json:"host,omitempty"`
}

// Lock claims dir for this process, giving up after wait. Two pipelines racing
// on one directory is normal in CI, where waiting a little beats failing the
// build; wait of zero keeps the immediate-failure behaviour.
func LockWait(ctx context.Context, dir string, wait time.Duration) (func(), error) {
	deadline := time.Now().Add(wait)
	for {
		release, err := Lock(dir)
		if err == nil {
			return release, nil
		}
		if wait <= 0 || time.Now().After(deadline) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(time.Second):
		}
	}
}

// Lock claims dir for this process and returns the release function. A lock
// left behind by a process that no longer exists is taken over rather than
// treated as fatal — a crashed run should not need manual cleanup.
func Lock(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, LockFile)

	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			host, _ := os.Hostname()
			body, _ := json.Marshal(lockInfo{PID: os.Getpid(), Started: time.Now(), Host: host})
			_, _ = f.Write(body)
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}

		held, readErr := readLock(path)
		if readErr != nil {
			// Unreadable garbage: nothing to respect.
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			continue
		}

		host, _ := os.Hostname()
		if held.Host != "" && host != "" && held.Host != host {
			// A pid from another machine says nothing about whether that run
			// is alive, so the only safe reading is "held" — until the lock is
			// old enough that no plan run could still be going.
			if age := time.Since(held.Started); age < staleAfter {
				return nil, fmt.Errorf("%s is in use by a tgsieve run on %s (pid %d, started %s)\n"+
					"  this directory is shared between machines; wait for that run, or use a different --keep-plans directory",
					dir, held.Host, held.PID, held.Started.Format(time.RFC3339))
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			continue
		}

		if !alive(held.PID) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			continue
		}
		return nil, fmt.Errorf("%s is in use by another tgsieve run (pid %d, started %s)\n"+
			"  wait for it to finish, or use a different --keep-plans directory",
			dir, held.PID, held.Started.Format(time.Kitchen))
	}
	return nil, fmt.Errorf("could not claim %s: the lock kept reappearing", dir)
}

func readLock(path string) (lockInfo, error) {
	var info lockInfo
	b, err := os.ReadFile(path)
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(b, &info); err != nil {
		return info, err
	}
	if info.PID <= 0 {
		return info, fmt.Errorf("no pid recorded")
	}
	return info, nil
}

// alive reports whether a process is still running. Signal 0 performs the
// permission and existence checks without delivering anything.
func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || err == syscall.EPERM
}
