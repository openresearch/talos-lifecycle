// Package talosutil builds Talos API clients.
package talosutil

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/client/config"
)

var talosconfig = "/var/run/secrets/talos.dev/config"

// use the returned context: it selects the node
func Client(ctx context.Context, address string) (*client.Client, context.Context, error) {
	cfg, err := config.Open(talosconfig)
	if err != nil {
		return nil, ctx, fmt.Errorf("load talos config: %w", err)
	}
	// one node per client: apid does not proxy COSI to several
	ctx = client.WithNode(ctx, address)
	c, err := client.New(ctx, client.WithConfig(cfg))

	if err != nil {
		return nil, ctx, fmt.Errorf("create talos client: %w", err)
	}

	return c, ctx, nil
}

func Namespace() (string, error) {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns, nil
	}

	const namespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

	nsBytes, err := os.ReadFile(namespaceFile)
	if err != nil {
		return "", fmt.Errorf("read namespace file: %w", err)
	}

	return strings.TrimSpace(string(nsBytes)), nil
}
