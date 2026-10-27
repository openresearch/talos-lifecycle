// installerprobe prints the installer image the controller would send each node. Read-only.
//
//	go run ./hack/installerprobe -talosconfig ./talosconfig -version vX.Y.Z <node-ip>...
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/openresearch/talos-lifecycle/internal/controller"
	"github.com/openresearch/talos-lifecycle/internal/talosutil"
)

func main() {
	home, _ := os.UserHomeDir()
	talosconfig := flag.String("talosconfig", filepath.Join(home, ".talos", "config"), "talosconfig path")
	version := flag.String("version", "", "target Talos version")
	flag.Parse()

	if *version == "" || flag.NArg() == 0 {
		_, _ = fmt.Fprintln(os.Stderr, "usage: installerprobe [-talosconfig path] -version vX.Y.Z <node-ip>...")
		os.Exit(2)
	}

	talosutil.SetConfigPath(*talosconfig)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NODE\tPLATFORM\tSECUREBOOT\tSCHEMATIC\tIMAGE")

	failed := false

	for _, address := range flag.Args() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		image, err := controller.ReadNodeImage(ctx, address)

		cancel()

		if err != nil {
			_, _ = fmt.Fprintf(w, "%s\tERROR: %v\n", address, err)

			failed = true

			continue
		}

		schematic := image.Schematic
		if schematic == "" {
			schematic = "(none, stock)"
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%t\t%s\t%s:%s\n",
			address, image.Platform, image.SecureBoot, schematic, image.FactoryBase(), *version)
	}

	_ = w.Flush()

	if failed {
		os.Exit(1)
	}
}
