package publish

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/miharp/codavox/internal/testca"
)

// buildBinary compiles the real codavox binary for the test to exercise.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "codavox")
	cmd := exec.Command("go", "build", "-o", bin, "../../cmd/codavox")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building binary: %v\n%s", err, out)
	}
	return bin
}

// TestPublishBinaryEndToEnd runs the shipped binary against SSL material laid
// out the way ovadm leaves it on a real node, and checks that mutual TLS plus
// the role constraint behave over an actual network connection.
//
// The in-process tests exercise the handler; this exercises everything the
// operator actually runs — argument parsing, certificate discovery from
// certname and ssldir, and the TLS wiring.
func TestPublishBinaryEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and binds a port")
	}

	bin := buildBinary(t)
	ca := testca.New(t)
	ssldir := ca.SSLDir(t, "puppet.example.com", "openvox_server")

	staging := t.TempDir()
	manifests := filepath.Join(staging, "production", "manifests")
	if err := os.MkdirAll(manifests, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifests, "site.pp"), []byte("node default { }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const addr = "127.0.0.1:18151"
	cmd := exec.Command(bin, "publish",
		"--staging", staging,
		"--listen", addr,
		"--certname", "puppet.example.com",
		"--ssldir", ssldir,
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	client := func(cert tls.Certificate) *http.Client {
		return &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{cert},
				RootCAs:      ca.Pool(t),
				ServerName:   "puppet.example.com",
				MinVersion:   tls.VersionTLS12,
			}},
		}
	}
	compiler := client(ca.TLSCert(t, "compiler01.example.com", "openvox_compiler"))

	// Poll for readiness rather than sleeping a fixed interval.
	var resp *http.Response
	var err error
	for range 40 {
		resp, err = compiler.Get("https://" + addr + EnvironmentsPath)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("publisher never became ready: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var envs map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&envs); err != nil {
		t.Fatal(err)
	}
	if len(envs["production"]) != 64 {
		t.Errorf("production code_id = %q, want a 64-character hex digest", envs["production"])
	}

	// The property the whole authorization design rests on: a certificate from
	// the same CA, valid in every other respect, is refused because it is not
	// a compiler.
	t.Run("ordinary agent is refused by the running binary", func(t *testing.T) {
		agent := client(ca.TLSCert(t, "web01.example.com", "webserver"))
		if r, err := agent.Get("https://" + addr + EnvironmentsPath); err == nil {
			_ = r.Body.Close()
			t.Fatal("the running binary admitted an ordinary agent certificate")
		}
	})
}
