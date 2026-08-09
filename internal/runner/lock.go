package runner

import (
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

type lockInfo struct {
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
	Host    string    `json:"host,omitempty"`
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
		if readErr != nil || !alive(held.PID) {
			// Stale: the writer is gone, or the file is unreadable garbage.
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
