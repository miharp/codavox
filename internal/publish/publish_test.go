package publish

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/miharp/codavox/internal/puppetca"
	"github.com/miharp/codavox/internal/seal"
	"github.com/miharp/codavox/internal/testca"
)

func staging(t *testing.T, envs map[string]map[string]string) *Store {
	t.Helper()
	dir := t.TempDir()
	for env, files := range envs {
		for name, body := range files {
			full := filepath.Join(dir, env, name)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	s := NewStore(dir)
	if err := s.Reseal(); err != nil {
		t.Fatalf("Reseal: %v", err)
	}
	return s
}

func TestReseal(t *testing.T) {
	s := staging(t, map[string]map[string]string{
		"production": {"manifests/site.pp": "node default { }\n"},
		"testing":    {"manifests/site.pp": "node default { }\n"},
	})

	envs := s.Environments()
	if len(envs) != 2 {
		t.Fatalf("got %d environments, want 2", len(envs))
	}
	// Identical content in two environments must seal identically; the id is a
	// property of the tree, not of the environment name.
	if envs["production"] != envs["testing"] {
		t.Error("identical trees sealed to different code_ids")
	}

	t.Run("resealing unchanged content is stable", func(t *testing.T) {
		before := s.Environments()["production"]
		if err := s.Reseal(); err != nil {
			t.Fatal(err)
		}
		if after := s.Environments()["production"]; after != before {
			t.Errorf("reseal changed the id: %s -> %s", before, after)
		}
	})

	t.Run("changed content produces a new id", func(t *testing.T) {
		before := s.Environments()["production"]
		path := filepath.Join(s.StagingDir, "production/manifests/site.pp")
		if err := os.WriteFile(path, []byte("node default { notify { 'x': } }\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := s.Reseal(); err != nil {
			t.Fatal(err)
		}
		if after := s.Environments()["production"]; after == before {
			t.Error("changed content did not change the id")
		}
	})

	// One malformed directory must not take the whole publisher down.
	t.Run("invalid environment names are skipped", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Join(s.StagingDir, "bad-name"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := s.Reseal(); err != nil {
			t.Fatalf("Reseal failed because of one bad directory: %v", err)
		}
		if _, ok := s.Environments()["bad-name"]; ok {
			t.Error("an invalid environment name was published")
		}
	})

	t.Run("returned map is a copy", func(t *testing.T) {
		s.Environments()["production"] = "tampered"
		if s.Environments()["production"] == "tampered" {
			t.Error("callers can mutate the store's internal state")
		}
	})
}

func TestHandlerEnvironments(t *testing.T) {
	s := staging(t, map[string]map[string]string{
		"production": {"manifests/site.pp": "node default { }\n"},
	})
	srv := httptest.NewServer(Handler(s))
	defer srv.Close()

	resp, err := http.Get(srv.URL + EnvironmentsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// A cached version map would pin a compiler to a stale code_id and defeat
	// polling, which is the correctness mechanism.
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	var got map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["production"] != s.Environments()["production"] {
		t.Errorf("served id %q does not match the store", got["production"])
	}
}

func TestHandlerArtifact(t *testing.T) {
	s := staging(t, map[string]map[string]string{
		"production": {
			"manifests/site.pp":      "node default { }\n",
			"modules/apache/init.pp": "class apache { }\n",
		},
	})
	srv := httptest.NewServer(Handler(s))
	defer srv.Close()

	current := s.Environments()["production"]

	t.Run("serves an artifact that extracts to the same code_id", func(t *testing.T) {
		resp, err := http.Get(srv.URL + ArtifactPath("production", current))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}

		// The round trip is the real assertion: what a compiler unpacks must
		// seal back to the id it asked for, or content addressing is a lie.
		dst := filepath.Join(t.TempDir(), "extracted")
		if err := seal.ExtractArchive(bytes.NewReader(body), dst); err != nil {
			t.Fatalf("ExtractArchive: %v", err)
		}
		got, err := seal.CodeID(dst)
		if err != nil {
			t.Fatal(err)
		}
		if got != current {
			t.Errorf("artifact sealed to %s, want %s", got, current)
		}
	})

	t.Run("a stale code_id is refused", func(t *testing.T) {
		stale := "0000000000000000000000000000000000000000000000000000000000000000"
		resp, err := http.Get(srv.URL + ArtifactPath("production", stale))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("unknown environment", func(t *testing.T) {
		resp, err := http.Get(srv.URL + ArtifactPath("nosuchenv", current))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("malformed inputs are rejected before any filesystem access", func(t *testing.T) {
		for _, path := range []string{
			"/v1/artifact/bad-env/" + current,
			"/v1/artifact/production/has%2Fslash",
		} {
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Errorf("%s returned 200; expected rejection", path)
			}
		}
	})
}

// The end-to-end check that the whole authorization design rests on: a real
// TLS handshake, with certificates issued by a Puppet-like CA, where an
// ordinary agent must be refused even though its certificate is perfectly
// valid.
func TestMutualTLSEnforcesRole(t *testing.T) {
	ca := testca.New(t)

	serverCert := ca.TLSCert(t, "puppet.example.com", "openvox_server")
	compiler := ca.TLSCert(t, "compiler01.example.com", "openvox_compiler")
	agent := ca.TLSCert(t, "webserver01.example.com", "webserver")

	s := staging(t, map[string]map[string]string{
		"production": {"manifests/site.pp": "node default { }\n"},
	})

	srv := httptest.NewUnstartedServer(Handler(s))
	srv.TLS = &tls.Config{
		Certificates:     []tls.Certificate{serverCert},
		ClientCAs:        ca.Pool(t),
		ClientAuth:       tls.RequireAndVerifyClientCert,
		MinVersion:       tls.VersionTLS12,
		VerifyConnection: puppetca.VerifyConnectionRole("openvox_compiler"),
	}
	srv.StartTLS()
	defer srv.Close()

	get := func(t *testing.T, cert tls.Certificate) (*http.Response, error) {
		t.Helper()
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      ca.Pool(t),
			MinVersion:   tls.VersionTLS12,
		}}}
		return c.Get(srv.URL + EnvironmentsPath)
	}

	t.Run("compiler is admitted", func(t *testing.T) {
		resp, err := get(t, compiler)
		if err != nil {
			t.Fatalf("compiler was rejected: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	})

	// Without the role check every agent in the estate could read every
	// environment's manifests, which routinely reference internal hostnames,
	// credential paths, and topology.
	t.Run("ordinary agent is refused despite a valid certificate", func(t *testing.T) {
		resp, err := get(t, agent)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("an agent certificate was admitted")
		}
	})

	t.Run("no client certificate is refused", func(t *testing.T) {
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:    ca.Pool(t),
			MinVersion: tls.VersionTLS12,
		}}}
		if resp, err := c.Get(srv.URL + EnvironmentsPath); err == nil {
			_ = resp.Body.Close()
			t.Fatal("an anonymous client was admitted")
		}
	})
}

func TestTrimBase(t *testing.T) {
	for in, want := range map[string]string{
		"https://puppet:8150/":   "https://puppet:8150",
		"https://puppet:8150///": "https://puppet:8150",
		"https://puppet:8150":    "https://puppet:8150",
	} {
		if got := TrimBase(in); got != want {
			t.Errorf("TrimBase(%q) = %q, want %q", in, got, want)
		}
	}
}
