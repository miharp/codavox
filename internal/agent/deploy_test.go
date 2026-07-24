package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miharp/codavox/internal/testca"
)

// fakeR10k writes a stand-in r10k that stages one environment with the given
// content, so the deploy command has something to run.
func fakeR10k(t *testing.T, staging, env, body string) string {
	t.Helper()
	script := fmt.Sprintf("#!/bin/sh\nmkdir -p %q\nprintf %q > %q\nprintf '{\"name\":%q,\"signature\":\"abcdef1\"}' > %q\n",
		filepath.Join(staging, env, "manifests"),
		body, filepath.Join(staging, env, "manifests", "site.pp"),
		env, filepath.Join(staging, env, ".r10k-deploy.json"))
	path := filepath.Join(t.TempDir(), "r10k")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	return path
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	for range 50 {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("file never appeared: %s", path)
}

// TestDeployStagesSignalsAndServes exercises the whole deploy verb against a
// running publisher: `codavox deploy --wait` runs r10k, signals the publisher
// with SIGHUP, and blocks until the new version is served — after which a
// compiler converges onto it. This is the one-command Code Manager experience,
// end to end.
func TestDeployStagesSignalsAndServes(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and binds a port")
	}

	bin := build(t)
	ca := testca.New(t)
	staging := t.TempDir()
	state := t.TempDir()
	serverSSL := ca.SSLDir(t, "puppet.example.com", "openvox_server")

	// The publisher starts against empty staging and must be running — with its
	// pidfile written — before the deploy can signal it.
	pub := &publisher{bin: bin, staging: staging, addr: "127.0.0.1:18158", ssldir: serverSSL, state: state}
	pub.restart(t)
	t.Cleanup(pub.stop)
	waitForFile(t, filepath.Join(state, "publish.pid"))

	// Deploy: run the fake r10k, signal the publisher, wait until it serves.
	r10k := fakeR10k(t, staging, "production", "v1\n")
	cmd := exec.Command(bin, "deploy", "production", "--wait",
		"--r10k", r10k, "--staging", staging, "--state", state)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("deploy --wait failed: %v", err)
	}
	if !strings.Contains(string(out), "serving") {
		t.Errorf("deploy output did not report serving:\n%s", out)
	}

	// The publisher now serves production, so a compiler converges onto it.
	c := newCompiler(t, ca, "compiler01.example.com")
	syncReady(t, c, bin, pub.url())
	body, err := os.ReadFile(filepath.Join(c.envPath, "production", "manifests/site.pp"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "v1\n" {
		t.Errorf("compiler content = %q, want v1 after deploy", body)
	}
}
