// Package cli implements the drswitch command line.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/ozy31/gitops-multicluster-dr/internal/config"
	"github.com/ozy31/gitops-multicluster-dr/internal/site"
	"github.com/ozy31/gitops-multicluster-dr/internal/switcher"
)

// GlobalOptions are flags shared by every subcommand.
type GlobalOptions struct {
	ConfigPath string
	AWSProfile string
	AWSRegion  string
	Kubeconfig string
	SkipDNS    bool
	Push       bool
}

// Factory builds a Switcher from configuration. It is injected so tests can
// replace Git, Route53 and health checks with fakes.
type Factory func(ctx context.Context, cfg *config.Config, g *GlobalOptions, out io.Writer) (*switcher.Switcher, error)

// NewRootCmd returns the drswitch command tree.
func NewRootCmd(version string, factory Factory) *cobra.Command {
	g := &GlobalOptions{}
	root := &cobra.Command{
		Use:   "drswitch",
		Short: "Flip the GitOps activeSite flag and Route53 weights between primary and DR",
		Long: `drswitch moves a workload between a primary and a DR cluster.

A switch runs these steps in order:
  1. health-check the target site (platform probe); abort if it fails
  2. commit activeSite=both: ArgoCD scales the target up, old site keeps running
  3. wait until the target reports ready (optional ready probe)
  4. shift the Route53 weighted records (when route53.enabled)
  5. wait the drain delay so cached DNS answers expire
  6. commit activeSite=<target>: ArgoCD scales the old site down to standby

Every step is idempotent: after a partial failure, re-run the same command.`,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringVarP(&g.ConfigPath, "config", "c", "dr/drswitch.yaml", "path to drswitch config")
	pf.StringVar(&g.AWSProfile, "aws-profile", "", "AWS shared config profile (overrides route53.profile; default credential chain if empty)")
	pf.StringVar(&g.AWSRegion, "aws-region", "", "AWS region for the Route53 client (overrides route53.region)")
	pf.StringVar(&g.Kubeconfig, "kubeconfig", "", "kubeconfig path for kube probes (overrides config and $KUBECONFIG)")
	pf.BoolVar(&g.SkipDNS, "skip-dns", false, "do not read or change Route53 even if enabled in config")

	root.AddCommand(newStatusCmd(g, factory))
	root.AddCommand(newSwitchCmd(g, factory, site.DR))
	root.AddCommand(newSwitchCmd(g, factory, site.Primary))
	return root
}

func build(ctx context.Context, g *GlobalOptions, factory Factory, out io.Writer) (*switcher.Switcher, *config.Config, error) {
	cfg, err := config.Load(g.ConfigPath)
	if err != nil {
		return nil, nil, err
	}
	sw, err := factory(ctx, cfg, g, out)
	return sw, cfg, err
}

func newStatusCmd(g *GlobalOptions, factory Factory) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show where the flag and DNS point and probe both sites",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if output != "text" && output != "json" {
				return fmt.Errorf("--output must be text or json, got %q", output)
			}
			sw, _, err := build(cmd.Context(), g, factory, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			st, err := sw.Status(cmd.Context())
			if err != nil {
				return err
			}
			if output == "json" {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(st)
			}
			printStatus(cmd.OutOrStdout(), st)
			if !st.Consistent {
				return errors.New("flag and DNS disagree or a switch is in progress; re-run failover/failback for the intended site to reconcile")
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "text", "output format: text|json")
	return cmd
}

func printStatus(w io.Writer, st *switcher.Status) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "flag activeSite:\t%s\n", st.FlagActive)
	if st.DNSEnabled {
		active := string(st.DNSActive)
		if active == "" {
			active = "mixed"
		}
		_, _ = fmt.Fprintf(tw, "route53 active:\t%s\t(weights primary=%d dr=%d)\n", active, st.DNSWeights[site.Primary], st.DNSWeights[site.DR])
	} else {
		_, _ = fmt.Fprintf(tw, "route53:\tnot managed by drswitch\n")
	}
	sites := make([]string, 0, len(st.Health))
	for s := range st.Health {
		sites = append(sites, string(s))
	}
	sort.Strings(sites)
	for _, s := range sites {
		h := st.Health[site.Site(s)]
		line := h.Status
		if h.Probe != "" {
			line += "\t" + h.Probe
		}
		if h.Error != "" {
			line += "\t" + h.Error
		}
		_, _ = fmt.Fprintf(tw, "health %s:\t%s\n", s, line)
	}
	_ = tw.Flush()
}

func newSwitchCmd(g *GlobalOptions, factory Factory, target site.Site) *cobra.Command {
	var (
		opts switcher.Options
		yes  bool
	)
	name, short := "failover", "Move traffic and workloads to the DR site"
	if target == site.Primary {
		name, short = "failback", "Move traffic and workloads back to the primary site"
	}
	cmd := &cobra.Command{
		Use:   name,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !opts.DryRun && !yes {
				if err := confirm(cmd.InOrStdin(), cmd.ErrOrStderr(), name, target); err != nil {
					return err
				}
			}
			sw, cfg, err := build(cmd.Context(), g, factory, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("drain") {
				opts.Drain = cfg.Drain
			}
			res, err := sw.SwitchTo(cmd.Context(), target, opts)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			switch {
			case res.NoOp:
				_, _ = fmt.Fprintf(out, "no change: %s already active\n", target)
			case res.DryRun:
				_, _ = fmt.Fprintf(out, "dry-run: %s would move %s -> %s (%d steps); nothing was changed\n", name, res.From, res.To, len(res.Steps))
			default:
				_, _ = fmt.Fprintf(out, "%s complete: %s -> %s", name, res.From, res.To)
				if len(res.Commits) > 0 {
					_, _ = fmt.Fprintf(out, " commits=%s", strings.Join(res.Commits, ","))
				}
				if res.ChangeID != "" {
					_, _ = fmt.Fprintf(out, " route53=%s", res.ChangeID)
				}
				_, _ = fmt.Fprintln(out)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&opts.DryRun, "dry-run", false, "run the health check and print the plan without changing anything")
	f.BoolVar(&opts.SkipHealthCheck, "skip-health-check", false, "do not probe the target site first (last resort)")
	f.BoolVar(&opts.Force, "force", false, "re-apply flag and DNS even if they already point at the target")
	f.DurationVar(&opts.Drain, "drain", 0, "keep the old site running this long after DNS moved (default: drain from config)")
	f.StringVar(&opts.Reason, "reason", "", "reason recorded in the commit message and Route53 change comment")
	f.BoolVar(&g.Push, "push", false, "push the flag commit (overrides git.push)")
	f.BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

func confirm(in io.Reader, out io.Writer, action string, target site.Site) error {
	_, _ = fmt.Fprintf(out, "About to %s to %q. Type the site name to continue: ", action, target)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if strings.TrimSpace(line) != string(target) {
		return errors.New("aborted: confirmation did not match")
	}
	return nil
}

// Execute runs the CLI and returns the process exit code.
func Execute(ctx context.Context, version string, args []string, stdout, stderr io.Writer) int {
	cmd := NewRootCmd(version, DefaultFactory)
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	if err := cmd.ExecuteContext(ctx); err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		if errors.Is(err, switcher.ErrHealthCheck) {
			return 3
		}
		return 1
	}
	return 0
}
