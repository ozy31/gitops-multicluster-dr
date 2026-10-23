package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sample = `
git:
  repoPath: ..
  push: true
sites:
  primary:
    region: eu-central-1
    health:
      url: https://health.primary.example.com/healthz
  dr:
    region: eu-west-1
    health:
      kube:
        context: prod-dr
    ready:
      url: https://app-dr.example.com/readyz
      timeout: 2s
route53:
  enabled: true
  hostedZoneID: Z123
  recordName: app.example.com
ready:
  timeout: 5m
drain: 90s
`

func TestLoadAppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dr", "drswitch.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Git.RepoPath != dir {
		t.Errorf("RepoPath = %q, want %q", cfg.Git.RepoPath, dir)
	}
	if cfg.Git.FlagFile != "dr/active-site.yaml" || cfg.Git.FlagKey != "dr.activeSite" {
		t.Errorf("unexpected flag defaults: %+v", cfg.Git)
	}
	if cfg.Route53.RecordType != "CNAME" || cfg.Route53.ActiveWeight != 100 || cfg.Route53.StandbyWeight != 0 {
		t.Errorf("unexpected route53 defaults: %+v", cfg.Route53)
	}
	if got := cfg.Sites.DR.Health.Kube.Path; got != "/readyz" {
		t.Errorf("kube path default = %q", got)
	}
	if got := cfg.Sites.DR.Ready.Timeout; got != 2*time.Second {
		t.Errorf("ready timeout = %v", got)
	}
	if cfg.Drain != 90*time.Second {
		t.Errorf("drain = %v", cfg.Drain)
	}
	if cfg.Ready.Timeout != 5*time.Minute || cfg.Ready.Interval != 10*time.Second {
		t.Errorf("wait = %+v", cfg.Ready)
	}
}

func TestValidate(t *testing.T) {
	tests := map[string]struct {
		yaml string
		want string
	}{
		"route53 missing zone": {
			yaml: "route53:\n  enabled: true\n  recordName: a.example.com\n",
			want: "hostedZoneID is required",
		},
		"probe with both url and kube": {
			yaml: "sites:\n  dr:\n    health:\n      url: http://x\n      kube: {context: c}\n",
			want: "set exactly one of url or kube",
		},
		"probe with neither": {
			yaml: "sites:\n  primary:\n    health: {timeout: 1s}\n",
			want: "set exactly one of url or kube",
		},
		"inCluster and context": {
			yaml: "sites:\n  dr:\n    health:\n      kube: {context: c, inCluster: true}\n",
			want: "mutually exclusive",
		},
		"unknown field": {
			yaml: "gitt: {}\n",
			want: "field gitt not found",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestRepoConfigsAreValid(t *testing.T) {
	// The configs shipped in the repository must always load.
	for _, name := range []string{"drswitch.yaml", "drswitch.local.yaml"} {
		if _, err := Load(filepath.Join("..", "..", "dr", name)); err != nil {
			t.Errorf("dr/%s: %v", name, err)
		}
	}
}
