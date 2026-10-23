// Package health probes a site before and after traffic is moved to it.
package health

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ozy31/gitops-multicluster-dr/internal/config"
)

// Checker reports whether a target is healthy.
type Checker interface {
	Check(ctx context.Context) error
	String() string
}

// HTTPChecker issues a GET and treats any 2xx response as healthy.
type HTTPChecker struct {
	URL     string
	Client  *http.Client
	Timeout time.Duration
	// Describe is used by String; it defaults to URL.
	Describe string
}

// Check implements Checker.
func (h *HTTPChecker) Check(ctx context.Context) error {
	if h.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL, nil)
	if err != nil {
		return err
	}
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", h, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("GET %s: status %d: %s", h, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (h *HTTPChecker) String() string {
	if h.Describe != "" {
		return h.Describe
	}
	return h.URL
}

// NewKubeChecker builds a Checker that GETs a path on a Kubernetes API
// server (for example /readyz or a service proxy path) using either the
// in-cluster service account or a kubeconfig context. kubeconfigOverride,
// when non-empty, replaces the kubeconfig path from the probe config.
func NewKubeChecker(p *config.KubeProbe, timeout time.Duration, kubeconfigOverride string) (*HTTPChecker, error) {
	restCfg, desc, err := restConfig(p, kubeconfigOverride)
	if err != nil {
		return nil, err
	}
	client, err := rest.HTTPClientFor(restCfg)
	if err != nil {
		return nil, fmt.Errorf("kube client for %s: %w", desc, err)
	}
	path := p.Path
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return &HTTPChecker{
		URL:      strings.TrimSuffix(restCfg.Host, "/") + path,
		Client:   client,
		Timeout:  timeout,
		Describe: fmt.Sprintf("kube[%s]%s", desc, path),
	}, nil
}

func restConfig(p *config.KubeProbe, kubeconfigOverride string) (*rest.Config, string, error) {
	if p.InCluster {
		cfg, err := rest.InClusterConfig()
		if err != nil {
			return nil, "", fmt.Errorf("in-cluster config: %w", err)
		}
		return cfg, "in-cluster", nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	switch {
	case kubeconfigOverride != "":
		rules.ExplicitPath = kubeconfigOverride
	case p.Kubeconfig != "":
		rules.ExplicitPath = p.Kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: p.Context}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("kubeconfig context %q: %w", p.Context, err)
	}
	desc := p.Context
	if desc == "" {
		desc = "current-context"
	}
	return cfg, desc, nil
}

// FromProbe builds the right Checker for a probe definition. It returns
// nil, nil when p is nil.
func FromProbe(p *config.Probe, kubeconfigOverride string) (Checker, error) {
	if p == nil {
		return nil, nil
	}
	if p.Kube != nil {
		return NewKubeChecker(p.Kube, p.Timeout, kubeconfigOverride)
	}
	return &HTTPChecker{URL: p.URL, Timeout: p.Timeout}, nil
}

// WaitHealthy polls c every interval until it succeeds or ctx is done. The
// last check error is returned on timeout.
func WaitHealthy(ctx context.Context, c Checker, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var last error
	for {
		if last = c.Check(ctx); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s not healthy before deadline: %w", c, last)
		case <-ticker.C:
		}
	}
}

// Unavailable is a Checker that always fails with Err. It stands in for a
// probe that could not be constructed (for example a missing kubeconfig
// context), so one broken site does not prevent operating on the other.
type Unavailable struct {
	Name string
	Err  error
}

// Check implements Checker.
func (u *Unavailable) Check(context.Context) error { return u.Err }

func (u *Unavailable) String() string { return u.Name }
