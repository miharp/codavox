package config

import (
	"os"
	"path/filepath"
	"reflect"
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
basedir: /etc/puppetlabs/code/environments
state: /opt/puppetlabs/codavox/state
ssldir: /etc/puppetlabs/puppet/ssl
certname: primary.example.com
r10k: /opt/puppetlabs/puppet/bin/r10k
r10k_config: /etc/puppetlabs/r10k/r10k.yaml
r10k_timeout: 20m
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
  max_unpacked: 4G
`)

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.BaseDir != "/etc/puppetlabs/code/environments" {
		t.Errorf("basedir = %q", c.BaseDir)
	}
	if c.Certname != "primary.example.com" {
		t.Errorf("certname = %q", c.Certname)
	}
	if c.R10kTimeout != "20m" {
		t.Errorf("r10k_timeout = %q", c.R10kTimeout)
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
	if c.Agent.MaxUnpacked != "4G" {
		t.Errorf("agent.max_unpacked = %q", c.Agent.MaxUnpacked)
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
	if c.BaseDir != "" || c.Publish.Listen != "" {
		t.Errorf("expected an empty config, got %+v", c)
	}
}

func TestLoadUsesEnvVar(t *testing.T) {
	path := writeConfig(t, "basedir: /from/env\n")
	t.Setenv(PathEnvVar, path)
	c, err := Load("") // no flag path; env should be consulted
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseDir != "/from/env" {
		t.Errorf("basedir = %q, want /from/env", c.BaseDir)
	}
}

// A file holding no settings is a configuration that sets nothing, not a parse
// error. This is the shipped conffile's normal state and what commenting a
// setting out produces, so treating it as a failure would stop every daemon on
// a fresh install.
func TestLoadEmptyFileIsEmptyConfig(t *testing.T) {
	for name, body := range map[string]string{
		"empty file":        "",
		"only a newline":    "\n",
		"only comments":     "# basedir: /x\n# nothing set\n",
		"comments and gaps": "\n\n# codavox config\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := Load(writeConfig(t, body))
			if err != nil {
				t.Fatalf("Load(%q): %v", body, err)
			}
			if !reflect.DeepEqual(c, Config{}) {
				t.Errorf("expected a zero config, got %+v", c)
			}
		})
	}
}

// The conffile the package installs at /etc/codavox/config.yaml must load. It
// is fully commented out by design, which is exactly the case that used to
// fail, and nothing else in CI would catch a syntax error in it.
func TestShippedConffileLoads(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "packaging", "config.yaml"))
	if err != nil {
		t.Fatalf("the packaged conffile does not load: %v", err)
	}
	// Shipped inert: installing the package must not change any behavior.
	if !reflect.DeepEqual(c, Config{}) {
		t.Errorf("the packaged conffile sets values as shipped: %+v", c)
	}
}

// The example is a populated file, so it also has to parse — and, because
// KnownFields is on, it doubles as a check that every key it documents still
// exists.
func TestExampleConfigLoads(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("config.example.yaml does not load: %v", err)
	}
	if c.BaseDir == "" || c.Agent.Publisher == "" {
		t.Errorf("the example parsed but looks empty: %+v", c)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := writeConfig(t, "stagng: /typo\n") // misspelled "basedir"
	if _, err := Load(path); err == nil {
		t.Error("expected error for an unknown field (a typo would otherwise be silently ignored)")
	}
}

func TestLoadRejectsMalformedYAML(t *testing.T) {
	path := writeConfig(t, "basedir: [unterminated\n")
	if _, err := Load(path); err == nil {
		t.Error("expected error for malformed YAML")
	}
}

func TestLoadFlushEnvironmentCacheDistinguishesUnsetFromFalse(t *testing.T) {
	unset, err := Load(writeConfig(t, "agent:\n  publisher: https://p:8150\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if unset.Agent.FlushEnvironmentCache != nil {
		t.Errorf("flush_environment_cache left out should be nil (default on), got %v", *unset.Agent.FlushEnvironmentCache)
	}
	if unset.Agent.PuppetServer != "" {
		t.Errorf("puppetserver left out should be empty, got %q", unset.Agent.PuppetServer)
	}

	off, err := Load(writeConfig(t, `
agent:
  publisher: https://p:8150
  puppetserver: https://compiler01.example.com:8140
  flush_environment_cache: false
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if off.Agent.FlushEnvironmentCache == nil || *off.Agent.FlushEnvironmentCache {
		t.Errorf("flush_environment_cache: false should load as false, got %v", off.Agent.FlushEnvironmentCache)
	}
	if off.Agent.PuppetServer != "https://compiler01.example.com:8140" {
		t.Errorf("puppetserver = %q", off.Agent.PuppetServer)
	}
}
