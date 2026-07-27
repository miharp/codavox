package publish

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

// liveProcess starts something real and returns its pid, so liveness probes are
// exercised rather than stubbed.
func liveProcess(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting a process: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd.Process.Pid
}

func TestAcquirePidFileWritesOurPid(t *testing.T) {
	state := t.TempDir()
	p, err := AcquirePidFile(state)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release()

	got, err := readPid(PidFilePath(state))
	if err != nil {
		t.Fatal(err)
	}
	if got != os.Getpid() {
		t.Errorf("pidfile holds %d, want %d", got, os.Getpid())
	}
}

// The bug (#56): a second publisher used to overwrite a live pid, fail to bind,
// and then delete the incumbent's pidfile on the way out — leaving a healthy
// publisher unreachable to `codavox deploy`.
func TestAcquirePidFileRefusesALivePublisher(t *testing.T) {
	state := t.TempDir()
	pid := liveProcess(t)
	if err := os.WriteFile(PidFilePath(state), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := AcquirePidFile(state)
	if err == nil {
		got.Release()
		t.Fatal("claimed a state directory already held by a live publisher")
	}
	if !errors.Is(err, ErrPublisherRunning) {
		t.Errorf("error = %v, want ErrPublisherRunning", err)
	}
	// And crucially, the incumbent's claim is untouched.
	after, rerr := readPid(PidFilePath(state))
	if rerr != nil || after != pid {
		t.Errorf("incumbent pidfile became %d/%v, want %d", after, rerr, pid)
	}
}

// A crashed or SIGKILLed publisher never runs a deferred cleanup, so a pidfile
// naming a dead process has to be reclaimable or the service could never restart.
func TestAcquirePidFileTakesOverAStaleClaim(t *testing.T) {
	state := t.TempDir()
	// Start and reap a process, so the pid is real but no longer running.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	dead := cmd.Process.Pid
	if err := os.WriteFile(PidFilePath(state), []byte(strconv.Itoa(dead)), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := AcquirePidFile(state)
	if err != nil {
		t.Fatalf("refused a stale claim: %v", err)
	}
	defer p.Release()
	if got, _ := readPid(PidFilePath(state)); got != os.Getpid() {
		t.Errorf("pidfile holds %d, want this process %d", got, os.Getpid())
	}
}

// Garbage must not wedge the service permanently.
func TestAcquirePidFileTakesOverGarbage(t *testing.T) {
	state := t.TempDir()
	if err := os.WriteFile(PidFilePath(state), []byte("not-a-pid"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := AcquirePidFile(state)
	if err != nil {
		t.Fatalf("a corrupt pidfile blocked startup: %v", err)
	}
	defer p.Release()
	if got, _ := readPid(PidFilePath(state)); got != os.Getpid() {
		t.Errorf("pidfile holds %d, want %d", got, os.Getpid())
	}
}

// The other half of the bug: releasing must not delete a claim this process does
// not hold, or an exiting publisher takes out whoever replaced it.
func TestReleaseOnlyRemovesOurOwnClaim(t *testing.T) {
	state := t.TempDir()
	p, err := AcquirePidFile(state)
	if err != nil {
		t.Fatal(err)
	}

	// Someone else takes the file over while we are still running.
	other := liveProcess(t)
	if err := os.WriteFile(PidFilePath(state), []byte(strconv.Itoa(other)), 0o644); err != nil {
		t.Fatal(err)
	}

	p.Release()

	got, err := readPid(PidFilePath(state))
	if err != nil {
		t.Fatalf("Release deleted a pidfile it did not own: %v", err)
	}
	if got != other {
		t.Errorf("pidfile holds %d, want the other holder %d", got, other)
	}
}

func TestReleaseRemovesOurClaim(t *testing.T) {
	state := t.TempDir()
	p, err := AcquirePidFile(state)
	if err != nil {
		t.Fatal(err)
	}
	p.Release()
	if _, err := os.Stat(PidFilePath(state)); !os.IsNotExist(err) {
		t.Errorf("pidfile survived Release: %v", err)
	}
}

// Acquiring twice in one process is not an error: it is the same holder.
func TestAcquirePidFileIsIdempotentForTheHolder(t *testing.T) {
	state := t.TempDir()
	a, err := AcquirePidFile(state)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, err := AcquirePidFile(state)
	if err != nil {
		t.Fatalf("the holder was refused its own claim: %v", err)
	}
	if b.pid != a.pid {
		t.Errorf("pids differ: %d vs %d", a.pid, b.pid)
	}
}

func TestReleaseToleratesNil(t *testing.T) {
	var p *PidFile
	p.Release()
}
