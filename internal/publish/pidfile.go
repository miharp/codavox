package publish

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// PidFilePath is where the running publisher records its pid, so a deploy can
// signal it to reseal.
func PidFilePath(stateDir string) string { return filepath.Join(stateDir, "publish.pid") }

// PidFile is one publisher's claim on a state directory.
//
// It exists because the obvious implementation — write the pid, remove it on exit
// — lets a process that never started destroy the claim of the one that did. A
// second `codavox publish` sharing the state directory would overwrite a live
// pid, fail to bind the port, and then delete the file on its way out. The
// publisher it displaced kept running with no pidfile, and `codavox deploy` could
// no longer signal it: deploys updated the basedir and stopped reaching
// compilers, while still exiting 0.
//
// So a claim is only ever taken when nobody holds it, and only ever released by
// the holder.
type PidFile struct {
	path string
	pid  int
}

// ErrPublisherRunning reports that another live publisher already holds the
// state directory. It is a better diagnosis than the bind error that would
// otherwise surface, because it names the process and the reason.
var ErrPublisherRunning = errors.New("a publisher is already running")

// AcquirePidFile claims stateDir for this process.
//
// A pidfile naming a live process is refused. One naming a dead process is stale
// — a crashed or SIGKILLed publisher leaves it behind, since nothing gets to run
// a deferred cleanup — so it is taken over.
func AcquirePidFile(stateDir string) (*PidFile, error) {
	path := PidFilePath(stateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating state directory: %w", err)
	}

	self := os.Getpid()
	for range 2 {
		// O_EXCL so two publishers starting together cannot both believe they won.
		//nolint:gosec // G304: path is PidFilePath(stateDir), composed by this package
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			_, werr := f.WriteString(strconv.Itoa(self))
			cerr := f.Close()
			if werr != nil {
				_ = os.Remove(path)
				return nil, fmt.Errorf("writing pidfile: %w", werr)
			}
			if cerr != nil {
				_ = os.Remove(path)
				return nil, fmt.Errorf("writing pidfile: %w", cerr)
			}
			return &PidFile{path: path, pid: self}, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("creating pidfile: %w", err)
		}

		// Someone holds it, or used to.
		holder, rerr := readPid(path)
		switch {
		case rerr != nil:
			// Unreadable or garbage. Treat as stale rather than refusing to start
			// forever over a corrupt byte, but say so by taking it over.
		case holder == self:
			// Already ours, from an earlier call in this process.
			return &PidFile{path: path, pid: self}, nil
		case processAlive(holder):
			return nil, fmt.Errorf("%w as pid %d (state directory %s)", ErrPublisherRunning, holder, stateDir)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("removing stale pidfile: %w", err)
		}
	}
	return nil, fmt.Errorf("could not claim %s: it keeps being recreated", path)
}

// Release gives up the claim, and only if this process still holds it.
//
// Checking first is the point: without it, a publisher exiting would delete
// whatever pidfile happened to be there, including one a different publisher had
// legitimately written in the meantime.
func (p *PidFile) Release() {
	if p == nil {
		return
	}
	if holder, err := readPid(p.path); err != nil || holder != p.pid {
		return
	}
	_ = os.Remove(p.path)
}

func readPid(path string) (int, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- a path this package composed
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

// processAlive reports whether pid names a running process. Signal 0 delivers
// nothing and only checks reachability.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
