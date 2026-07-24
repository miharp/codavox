package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadParsesFields(t *testing.T) {
	path := writeConfig(t, `
staging: /etc/puppetlabs/code-staging
state: /opt/puppetlabs/codavox/state
ssldir: /etc/puppetlabs/puppet/ssl
certname: primary.example.com
r10k: /opt/puppetlabs/puppet/bin/r10k
r10k_config: /etc/puppetlabs/r10k/r10k.yaml
publish:
  listen: ":8150"
  allow_roles: [openvox_compiler, deployer]
deploy_server:
  listen: ":8170"
  api_token: /etc/codavox/api.token
  history: 50
agent:
  publisher: https://puppet.example.com:8150
  interval: 45s
  keep: 5
  min_age: 3h
`)

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Staging != "/etc/puppetlabs/code-staging" {
		t.Errorf("staging = %q", c.Staging)
	}
	if c.Certname != "primary.example.com" {
		t.Errorf("certname = %q", c.Certname)
	}
	if c.Publish.Listen != ":8150" || len(c.Publish.AllowRoles) != 2 {
		t.Errorf("publish = %+v", c.Publish)
	}
	if c.DeployServer.Listen != ":8170" || c.DeployServer.History != 50 || c.DeployServer.APIToken != "/etc/codavox/api.token" {
		t.Errorf("deploy_server = %+v", c.DeployServer)
	}
	if c.Agent.Publisher != "https://puppet.example.com:8150" || c.Agent.Interval != "45s" || c.Agent.Keep != 5 || c.Agent.MinAge != "3h" {
		t.Errorf("agent = %+v", c.Agent)
	}
}

func TestLoadExplicitMissingIsError(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Error("expected error for a config file named explicitly but missing")
	}
}

func TestLoadNoConfigIsEmpty(t *testing.T) {
	t.Setenv(PathEnvVar, "") // ensure the env var does not point anywhere
	if _, err := os.Stat(DefaultPath); err == nil {
		t.Skip("a config file exists at the default path on this host")
	}
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load with no config should not error: %v", err)
	}
	if c.Staging != "" || c.Publish.Listen != "" {
		t.Errorf("expected an empty config, got %+v", c)
	}
}

func TestLoadUsesEnvVar(t *testing.T) {
	path := writeConfig(t, "staging: /from/env\n")
	t.Setenv(PathEnvVar, path)
	c, err := Load("") // no flag path; env should be consulted
	if err != nil {
		t.Fatal(err)
	}
	if c.Staging != "/from/env" {
		t.Errorf("staging = %q, want /from/env", c.Staging)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := writeConfig(t, "stagng: /typo\n") // misspelled "staging"
	if _, err := Load(path); err == nil {
		t.Error("expected error for an unknown field (a typo would otherwise be silently ignored)")
	}
}

func TestLoadRejectsMalformedYAML(t *testing.T) {
	path := writeConfig(t, "staging: [unterminated\n")
	if _, err := Load(path); err == nil {
		t.Error("expected error for malformed YAML")
	}
}
