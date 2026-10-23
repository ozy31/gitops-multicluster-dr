// Package config loads the drswitch configuration file.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ozy31/gitops-multicluster-dr/internal/site"
)

// Config is the top-level drswitch configuration.
type Config struct {
	Git     Git     `yaml:"git"`
	Sites   Sites   `yaml:"sites"`
	Route53 Route53 `yaml:"route53"`
	// Ready controls how long drswitch waits for workloads on the target
	// site to become ready after the Git flag flips and before DNS moves.
	Ready Wait `yaml:"ready"`
	// Drain is how long the old site keeps running after DNS has moved, so
	// resolvers and clients holding the old answer are not cut off. Set it
	// to at least the record TTL.
	Drain time.Duration `yaml:"drain"`
}

// Git describes where the activeSite flag lives.
type Git struct {
	// RepoPath is the local checkout. Relative paths are resolved against
	// the directory that contains the config file.
	RepoPath string `yaml:"repoPath"`
	// FlagFile is the YAML file (relative to RepoPath) holding the flag.
	FlagFile string `yaml:"flagFile"`
	// FlagKey is the dotted path of the flag inside FlagFile.
	FlagKey string `yaml:"flagKey"`
	// Remote and Branch are used when pushing the flag commit.
	Remote string `yaml:"remote"`
	Branch string `yaml:"branch"`
	// Push pushes the commit right after creating it.
	Push bool `yaml:"push"`
}

// Sites holds per-site settings.
type Sites struct {
	Primary SiteConfig `yaml:"primary"`
	DR      SiteConfig `yaml:"dr"`
}

// Get returns the settings for s.
func (s Sites) Get(x site.Site) SiteConfig {
	if x == site.DR {
		return s.DR
	}
	return s.Primary
}

// SiteConfig holds the settings of one site.
type SiteConfig struct {
	Region string `yaml:"region"`
	// Health is checked before switching traffic to this site. It should
	// reflect platform health (API server, ingress), not the application,
	// because a cold standby runs zero application replicas.
	Health *Probe `yaml:"health"`
	// Ready is polled after the flag flips; it should go green once the
	// application on this site is scaled up and serving.
	Ready *Probe `yaml:"ready"`
}

// Probe is either a plain HTTP(S) URL or a path on a Kubernetes API server.
type Probe struct {
	URL     string        `yaml:"url"`
	Kube    *KubeProbe    `yaml:"kube"`
	Timeout time.Duration `yaml:"timeout"`
}

// KubeProbe reaches a path through the Kubernetes API server, for example
// /readyz or a service proxy path. Credentials come from a kubeconfig
// context or from the in-cluster service account.
type KubeProbe struct {
	Kubeconfig string `yaml:"kubeconfig"`
	Context    string `yaml:"context"`
	InCluster  bool   `yaml:"inCluster"`
	Path       string `yaml:"path"`
}

// Route53 configures the weighted record pair that drswitch manages.
type Route53 struct {
	Enabled bool `yaml:"enabled"`
	// Profile is an AWS shared config profile. Empty uses the default
	// credential chain (env vars, IRSA / Pod Identity, instance role).
	Profile        string         `yaml:"profile"`
	Region         string         `yaml:"region"`
	HostedZoneID   string         `yaml:"hostedZoneID"`
	RecordName     string         `yaml:"recordName"`
	RecordType     string         `yaml:"recordType"`
	SetIdentifiers SetIdentifiers `yaml:"setIdentifiers"`
	ActiveWeight   int64          `yaml:"activeWeight"`
	StandbyWeight  int64          `yaml:"standbyWeight"`
}

// SetIdentifiers maps sites to Route53 SetIdentifier values.
type SetIdentifiers struct {
	Primary string `yaml:"primary"`
	DR      string `yaml:"dr"`
}

// Wait configures polling.
type Wait struct {
	Timeout  time.Duration `yaml:"timeout"`
	Interval time.Duration `yaml:"interval"`
}

// Load reads, defaults and validates the config at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if !filepath.IsAbs(cfg.Git.RepoPath) {
		cfg.Git.RepoPath = filepath.Join(filepath.Dir(path), cfg.Git.RepoPath)
	}
	return cfg, nil
}

// Parse decodes config bytes, applies defaults and validates the result.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Git.RepoPath == "" {
		c.Git.RepoPath = "."
	}
	if c.Git.FlagFile == "" {
		c.Git.FlagFile = "dr/active-site.yaml"
	}
	if c.Git.FlagKey == "" {
		c.Git.FlagKey = "dr.activeSite"
	}
	if c.Git.Remote == "" {
		c.Git.Remote = "origin"
	}
	if c.Git.Branch == "" {
		c.Git.Branch = "main"
	}
	if c.Route53.RecordType == "" {
		c.Route53.RecordType = "CNAME"
	}
	if c.Route53.ActiveWeight == 0 {
		c.Route53.ActiveWeight = 100
	}
	if c.Route53.SetIdentifiers.Primary == "" {
		c.Route53.SetIdentifiers.Primary = string(site.Primary)
	}
	if c.Route53.SetIdentifiers.DR == "" {
		c.Route53.SetIdentifiers.DR = string(site.DR)
	}
	if c.Ready.Timeout == 0 {
		c.Ready.Timeout = 10 * time.Minute
	}
	if c.Ready.Interval == 0 {
		c.Ready.Interval = 10 * time.Second
	}
	for _, sc := range []*SiteConfig{&c.Sites.Primary, &c.Sites.DR} {
		for _, p := range []*Probe{sc.Health, sc.Ready} {
			if p == nil {
				continue
			}
			if p.Timeout == 0 {
				p.Timeout = 5 * time.Second
			}
			if p.Kube != nil && p.Kube.Path == "" {
				p.Kube.Path = "/readyz"
			}
		}
	}
}

// Validate reports configuration errors.
func (c *Config) Validate() error {
	var errs []error
	if c.Drain < 0 {
		errs = append(errs, errors.New("drain must not be negative"))
	}
	if c.Route53.Enabled {
		if c.Route53.HostedZoneID == "" {
			errs = append(errs, errors.New("route53.hostedZoneID is required when route53.enabled"))
		}
		if c.Route53.RecordName == "" {
			errs = append(errs, errors.New("route53.recordName is required when route53.enabled"))
		}
		if c.Route53.SetIdentifiers.Primary == c.Route53.SetIdentifiers.DR {
			errs = append(errs, errors.New("route53.setIdentifiers.primary and .dr must differ"))
		}
		if c.Route53.ActiveWeight <= c.Route53.StandbyWeight {
			errs = append(errs, errors.New("route53.activeWeight must be greater than standbyWeight"))
		}
	}
	for _, s := range site.All() {
		sc := c.Sites.Get(s)
		for name, p := range map[string]*Probe{"health": sc.Health, "ready": sc.Ready} {
			if p == nil {
				continue
			}
			if (p.URL == "") == (p.Kube == nil) {
				errs = append(errs, fmt.Errorf("sites.%s.%s: set exactly one of url or kube", s, name))
			}
			if p.Kube != nil && p.Kube.InCluster && p.Kube.Context != "" {
				errs = append(errs, fmt.Errorf("sites.%s.%s.kube: inCluster and context are mutually exclusive", s, name))
			}
		}
	}
	return errors.Join(errs...)
}
