// Package config loads codavox's shared configuration file.
//
// The file lets an operator set the paths and options that otherwise repeat
// across publish, deploy, and deploy-server — staging, state, ssldir, certname,
// r10k — in one place. Every value it holds is also a flag: a flag always wins,
// the file fills unset flags, and a built-in default is the last resort.
//
// The per-compile commands code-id and code-content deliberately do NOT read
// it. They are spawned on every catalog compile and must stay a single symlink
// read and a single file read, with no parsing on that path.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// DefaultPath is where codavox looks for its configuration when neither --config
// nor CODAVOX_CONFIG names one. The rpm and deb ship packaging/config.yaml here
// as a noreplace conffile, so an upgrade never overwrites a site's settings.
const DefaultPath = "/etc/codavox/config.yaml"

// PathEnvVar overrides the configuration file location.
const PathEnvVar = "CODAVOX_CONFIG"

// Config is the parsed configuration file. Shared settings sit at the top level;
// per-daemon settings sit in named sections, so the two servers' distinct listen
// addresses do not collide.
type Config struct {
	Staging         string `yaml:"staging"`
	State           string `yaml:"state"`
	SSLDir          string `yaml:"ssldir"`
	Certname        string `yaml:"certname"`
	EnvironmentPath string `yaml:"environmentpath"`
	R10k            string `yaml:"r10k"`
	R10kConfig      string `yaml:"r10k_config"`

	Publish struct {
		Listen     string   `yaml:"listen"`
		AllowRoles []string `yaml:"allow_roles"`
		// CertificateRevocation takes Puppet's values — chain, leaf, or false —
		// and defaults to chain, so it behaves like every other Puppet service.
		CertificateRevocation string `yaml:"certificate_revocation"`
	} `yaml:"publish"`

	DeployServer struct {
		Listen   string `yaml:"listen"`
		APIToken string `yaml:"api_token"`
		Secret   string `yaml:"secret"`
		History  int    `yaml:"history"`
	} `yaml:"deploy_server"`

	Agent struct {
		Publisher         string `yaml:"publisher"`
		Interval          string `yaml:"interval"`
		Keep              int    `yaml:"keep"`
		MinAge            string `yaml:"min_age"`
		PruneEnvironments bool   `yaml:"prune_environments"`
	} `yaml:"agent"`
}

// Load reads the configuration file.
//
// flagPath is the value of --config, or "". If it is empty, CODAVOX_CONFIG is
// consulted, then DefaultPath. A missing file at the default location is not an
// error — it means "no configuration" — but a file named explicitly (by flag or
// env) that cannot be read is, since the operator clearly meant to use it.
func Load(flagPath string) (Config, error) {
	path := flagPath
	explicit := path != ""
	if path == "" {
		if v := os.Getenv(PathEnvVar); v != "" {
			path = v
			explicit = true
		}
	}
	if path == "" {
		path = DefaultPath
	}

	b, err := os.ReadFile(path) // #nosec G304,G703 -- operator-supplied config path
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("reading config %s: %w", path, err)
	}

	var c Config
	// KnownFields makes a typo in the file an error rather than a silently
	// ignored setting that leaves the operator wondering why nothing changed.
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		// A file holding no YAML document — empty, or nothing but comments —
		// decodes as io.EOF. That is the shipped conffile's normal state and the
		// obvious result of commenting a setting out, so it means "no
		// configuration", not a parse failure. Reporting EOF here would stop
		// every daemon on a fresh install with an error naming nothing an
		// operator could act on.
		if errors.Is(err, io.EOF) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("parsing config %s: %w", path, err)
	}
	return c, nil
}
