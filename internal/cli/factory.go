package cli

import (
	"context"
	"fmt"
	"io"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"

	"github.com/ozy31/gitops-multicluster-dr/internal/config"
	"github.com/ozy31/gitops-multicluster-dr/internal/dns"
	"github.com/ozy31/gitops-multicluster-dr/internal/gitops"
	"github.com/ozy31/gitops-multicluster-dr/internal/health"
	"github.com/ozy31/gitops-multicluster-dr/internal/site"
	"github.com/ozy31/gitops-multicluster-dr/internal/switcher"
)

// DefaultFactory wires the real Git, Route53 and health implementations.
func DefaultFactory(ctx context.Context, cfg *config.Config, g *GlobalOptions, out io.Writer) (*switcher.Switcher, error) {
	sw := &switcher.Switcher{
		Flag: &gitops.FlagStore{
			Repo: &gitops.ExecRepo{Dir: cfg.Git.RepoPath, Remote: cfg.Git.Remote, Branch: cfg.Git.Branch},
			File: cfg.Git.FlagFile,
			Key:  cfg.Git.FlagKey,
			Push: cfg.Git.Push || g.Push,
		},
		Health:        map[site.Site]health.Checker{},
		Ready:         map[site.Site]health.Checker{},
		ReadyTimeout:  cfg.Ready.Timeout,
		ReadyInterval: cfg.Ready.Interval,
		Out:           out,
	}

	for _, s := range site.All() {
		sc := cfg.Sites.Get(s)
		if c := probe(fmt.Sprintf("sites.%s.health", s), sc.Health, g.Kubeconfig); c != nil {
			sw.Health[s] = c
		}
		if c := probe(fmt.Sprintf("sites.%s.ready", s), sc.Ready, g.Kubeconfig); c != nil {
			sw.Ready[s] = c
		}
	}

	if cfg.Route53.Enabled && !g.SkipDNS {
		api, err := newRoute53Client(ctx, cfg.Route53, g)
		if err != nil {
			return nil, err
		}
		sw.DNS = &dns.WeightedRecords{
			Client:       api,
			HostedZoneID: cfg.Route53.HostedZoneID,
			Name:         cfg.Route53.RecordName,
			Type:         types.RRType(cfg.Route53.RecordType),
			SetIDs: map[site.Site]string{
				site.Primary: cfg.Route53.SetIdentifiers.Primary,
				site.DR:      cfg.Route53.SetIdentifiers.DR,
			},
			ActiveWeight:  cfg.Route53.ActiveWeight,
			StandbyWeight: cfg.Route53.StandbyWeight,
		}
	}
	return sw, nil
}

// probe builds a checker. A probe that cannot be built (for example the
// kubeconfig of a lost region is gone) becomes a checker that always fails,
// so it only matters when that site is actually probed.
func probe(name string, p *config.Probe, kubeconfig string) health.Checker {
	c, err := health.FromProbe(p, kubeconfig)
	if err != nil {
		return &health.Unavailable{Name: name, Err: err}
	}
	return c
}

// newRoute53Client uses the standard AWS credential chain, so the same
// binary works with a named profile on a laptop, SSO, or IRSA / EKS Pod
// Identity when it runs as a Job inside a cluster.
func newRoute53Client(ctx context.Context, rc config.Route53, g *GlobalOptions) (*route53.Client, error) {
	profile, region := rc.Profile, rc.Region
	if g.AWSProfile != "" {
		profile = g.AWSProfile
	}
	if g.AWSRegion != "" {
		region = g.AWSRegion
	}
	var opts []func(*awsconfig.LoadOptions) error
	if profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
	}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	} else {
		// Route53 is a global service; any region works for signing.
		opts = append(opts, awsconfig.WithDefaultRegion("us-east-1"))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return route53.NewFromConfig(awsCfg), nil
}
