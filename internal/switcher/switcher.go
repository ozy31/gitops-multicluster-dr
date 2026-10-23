// Package switcher orchestrates a failover or failback.
//
// A switch to target T runs these steps:
//
//  1. health-check T (platform probe; abort if it fails)
//  2. set activeSite=both and commit: ArgoCD scales T up, the old site keeps running
//  3. wait for T's ready probe
//  4. shift Route53 weights to T (when drswitch manages DNS)
//  5. wait the drain delay so cached DNS answers expire
//  6. set activeSite=T and commit: ArgoCD scales the old site down to standby
//
// Every step is idempotent, so re-running after a partial failure resumes
// where the previous run stopped.
package switcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ozy31/gitops-multicluster-dr/internal/dns"
	"github.com/ozy31/gitops-multicluster-dr/internal/health"
	"github.com/ozy31/gitops-multicluster-dr/internal/site"
)

// Flag is the GitOps activeSite flag.
type Flag interface {
	Active(ctx context.Context) (site.Site, error)
	SetActive(ctx context.Context, value site.Site, reason string) (commit string, err error)
}

// DNS moves client traffic between sites.
type DNS interface {
	Weights(ctx context.Context) (map[site.Site]int64, error)
	Shift(ctx context.Context, active site.Site, comment string) (changeID string, err error)
}

// Switcher wires the pieces together. DNS, Health and Ready entries are
// optional; a nil DNS means traffic follows the flag through external-dns
// weights rendered by the charts (see dr/README.md).
type Switcher struct {
	Flag          Flag
	DNS           DNS
	Health        map[site.Site]health.Checker
	Ready         map[site.Site]health.Checker
	ReadyTimeout  time.Duration
	ReadyInterval time.Duration
	Out           io.Writer
	// Sleep is used for the drain delay; nil means a context-aware sleep.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Options control a single switch.
type Options struct {
	DryRun          bool
	SkipHealthCheck bool
	// Force re-applies the flag and DNS even if they already point at the target.
	Force bool
	// Drain is how long to keep the old site running after DNS has moved.
	Drain  time.Duration
	Reason string
}

// Result describes what a switch did (or would do, in dry-run mode).
type Result struct {
	From     site.Site
	To       site.Site
	Steps    []string
	Commits  []string
	ChangeID string
	NoOp     bool
	DryRun   bool
}

// SiteHealth is the probe outcome for one site.
type SiteHealth struct {
	Probe  string `json:"probe,omitempty"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// Status is a snapshot of where the flag and DNS point.
type Status struct {
	FlagActive site.Site                `json:"flagActiveSite"`
	DNSEnabled bool                     `json:"dnsEnabled"`
	DNSWeights map[site.Site]int64      `json:"dnsWeights,omitempty"`
	DNSActive  site.Site                `json:"dnsActiveSite,omitempty"`
	Health     map[site.Site]SiteHealth `json:"health"`
	Consistent bool                     `json:"consistent"`
}

// ErrHealthCheck is returned when the target site fails its pre-check.
var ErrHealthCheck = errors.New("target site failed health check")

// Status reports the current state without changing anything.
func (s *Switcher) Status(ctx context.Context) (*Status, error) {
	active, err := s.Flag.Active(ctx)
	if err != nil {
		return nil, err
	}
	st := &Status{FlagActive: active, Health: map[site.Site]SiteHealth{}, Consistent: active != site.Both}
	if s.DNS != nil {
		st.DNSEnabled = true
		w, err := s.DNS.Weights(ctx)
		if err != nil {
			return nil, err
		}
		st.DNSWeights = w
		st.DNSActive = dns.ActiveFrom(w)
		st.Consistent = st.DNSActive == active
	}
	for _, x := range site.All() {
		c := s.Health[x]
		if c == nil {
			st.Health[x] = SiteHealth{Status: "not configured"}
			continue
		}
		if err := c.Check(ctx); err != nil {
			st.Health[x] = SiteHealth{Probe: c.String(), Status: "unhealthy", Error: err.Error()}
			continue
		}
		st.Health[x] = SiteHealth{Probe: c.String(), Status: "healthy"}
	}
	return st, nil
}

// SwitchTo moves the active site to target.
func (s *Switcher) SwitchTo(ctx context.Context, target site.Site, opts Options) (*Result, error) {
	if !target.Valid() {
		return nil, fmt.Errorf("invalid target site %q", target)
	}
	current, err := s.Flag.Active(ctx)
	if err != nil {
		return nil, err
	}
	res := &Result{From: current, To: target, DryRun: opts.DryRun}

	dnsActive := site.Site("")
	if s.DNS != nil {
		w, err := s.DNS.Weights(ctx)
		if err != nil {
			return nil, err
		}
		dnsActive = dns.ActiveFrom(w)
	}
	flagNeeded := current != target || opts.Force
	dnsNeeded := s.DNS != nil && (dnsActive != target || opts.Force)
	if !flagNeeded && !dnsNeeded {
		res.NoOp = true
		s.logf("%s is already the active site; nothing to do (use --force to re-apply)", target)
		return res, nil
	}

	// 1. Pre-flight health check of the site we are moving to.
	if opts.SkipHealthCheck {
		res.Steps = append(res.Steps, fmt.Sprintf("skip health check of %s (--skip-health-check)", target))
	} else {
		c := s.Health[target]
		if c == nil {
			return nil, fmt.Errorf("no health probe configured for site %q; configure sites.%s.health or pass --skip-health-check", target, target)
		}
		s.logf("checking health of %s via %s", target, c)
		if err := c.Check(ctx); err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrHealthCheck, target, err)
		}
		res.Steps = append(res.Steps, fmt.Sprintf("health check %s: ok", target))
	}

	// Plan the remaining steps so dry-run and the real run print the same list.
	overlap := flagNeeded && current != site.Both && current != target
	ready := s.Ready[target]
	if overlap {
		res.Steps = append(res.Steps, fmt.Sprintf("set activeSite %s -> %s and commit (scale up %s, keep %s running)", current, site.Both, target, current))
	}
	if ready != nil {
		res.Steps = append(res.Steps, fmt.Sprintf("wait up to %s for %s", s.ReadyTimeout, ready))
	}
	switch {
	case dnsNeeded:
		res.Steps = append(res.Steps, fmt.Sprintf("shift Route53 weights to %s", target))
	case s.DNS == nil:
		res.Steps = append(res.Steps, "DNS: external-dns follows the flag (route53 not managed by drswitch)")
	}
	if flagNeeded && opts.Drain > 0 {
		res.Steps = append(res.Steps, fmt.Sprintf("drain for %s", opts.Drain))
	}
	if flagNeeded {
		res.Steps = append(res.Steps, fmt.Sprintf("set activeSite -> %s and commit (scale %s down to standby)", target, target.Other()))
	}

	if opts.DryRun {
		for _, st := range res.Steps {
			s.logf("[dry-run] %s", st)
		}
		return res, nil
	}

	// 2. Run both sites so the old one keeps serving while the target scales up.
	if overlap {
		if err := s.setFlag(ctx, res, site.Both, opts.Reason); err != nil {
			return res, err
		}
	}

	// 3. Wait for workloads on the target to report ready before moving users.
	if ready != nil {
		s.logf("waiting for %s to become ready", ready)
		wctx, cancel := context.WithTimeout(ctx, s.ReadyTimeout)
		err := health.WaitHealthy(wctx, ready, s.ReadyInterval)
		cancel()
		if err != nil {
			return res, fmt.Errorf("%s is not ready; DNS was NOT changed and the old site is still running (re-run to resume): %w", target, err)
		}
	}

	// 4. Move traffic.
	if dnsNeeded {
		comment := strings.TrimSpace(fmt.Sprintf("drswitch: activeSite=%s %s", target, opts.Reason))
		id, err := s.DNS.Shift(ctx, target, comment)
		if err != nil {
			return res, fmt.Errorf("DNS shift to %s failed; both sites are still running (re-run to resume): %w", target, err)
		}
		res.ChangeID = id
		s.logf("route53 change submitted: %s", id)
	}

	if flagNeeded {
		// 5. Let resolvers and clients drop the old answers.
		if opts.Drain > 0 {
			s.logf("draining for %s before scaling %s down", opts.Drain, target.Other())
			if err := s.sleep(ctx, opts.Drain); err != nil {
				return res, fmt.Errorf("drain interrupted; flag left at %q: %w", site.Both, err)
			}
		}
		// 6. Final state: only the target runs full capacity.
		if err := s.setFlag(ctx, res, target, opts.Reason); err != nil {
			return res, err
		}
	}
	return res, nil
}

func (s *Switcher) setFlag(ctx context.Context, res *Result, v site.Site, reason string) error {
	sha, err := s.Flag.SetActive(ctx, v, reason)
	if err != nil {
		return fmt.Errorf("set activeSite=%s: %w", v, err)
	}
	if sha != "" {
		res.Commits = append(res.Commits, sha)
		s.logf("committed activeSite=%s (%s)", v, sha)
	}
	return nil
}

func (s *Switcher) sleep(ctx context.Context, d time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (s *Switcher) logf(format string, args ...any) {
	if s.Out == nil {
		return
	}
	_, _ = fmt.Fprintf(s.Out, format+"\n", args...)
}
