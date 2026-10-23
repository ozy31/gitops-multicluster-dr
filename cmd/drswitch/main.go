// Command drswitch flips a primary/DR pair of EKS clusters between sites by
// changing a GitOps flag and Route53 weights.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ozy31/gitops-multicluster-dr/internal/cli"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Execute(ctx, version, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
