package controller

import (
	"context"
	"fmt"
	"os"

	"github.com/cosi-project/runtime/pkg/safe"
	talosruntime "github.com/siderolabs/talos/pkg/machinery/resources/runtime"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
	"github.com/openresearch/talos-lifecycle/internal/talosutil"
)

const (
	factoryInstallerBase = "factory.talos.dev/installer"

	// Image Factory schematic without extensions: the stock Talos installer
	vanillaSchematic = "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba"

	// the Image Factory records the schematic ID as this extension's version
	schematicExtensionName = "schematic"
)

// resolveInstallerImage uses, in order: spec.installerImageBase, the node's own
// Image Factory schematic (so it keeps its extensions), INSTALLER_IMAGE_BASE,
// the stock installer.
func (r *UpgradePlanReconciler) resolveInstallerImage(
	ctx context.Context,
	plan *upgradev1alpha1.UpgradePlan,
	node *upgradev1alpha1.NodeStatus,
) (string, error) {
	log := log.FromContext(ctx)

	if base := plan.Spec.InstallerImageBase; base != "" {
		return fmt.Sprintf("%s:%s", base, plan.Spec.TalosVersion), nil
	}

	schematicID, err := nodeSchematicID(ctx, node.Address)
	if err != nil {
		return "", fmt.Errorf(
			"read schematic from node %s: %w (set spec.installerImageBase to override)",
			node.Name, err)
	}

	if schematicID != "" {
		image := fmt.Sprintf("%s/%s:%s", factoryInstallerBase, schematicID, plan.Spec.TalosVersion)
		log.V(1).Info("resolved installer image from node schematic",
			"node", node.Name, "schematic", schematicID)

		return image, nil
	}

	image := fmt.Sprintf("%s:%s", defaultInstallerImageBase(), plan.Spec.TalosVersion)
	r.event(plan, corev1.EventTypeNormal, "InstallerImageDefault",
		fmt.Sprintf("node %s has no Image Factory schematic; using %s", node.Name, image))

	return image, nil
}

func nodeSchematicID(ctx context.Context, address string) (string, error) {
	c, talosCtx, err := talosutil.Client(ctx, address)
	if err != nil {
		return "", err
	}
	defer c.Close() //nolint:errcheck

	extensions, err := safe.StateListAll[*talosruntime.ExtensionStatus](talosCtx, c.COSI)
	if err != nil {
		return "", fmt.Errorf("list extension statuses: %w", err)
	}

	for ext := range extensions.All() {
		if ext.TypedSpec().Metadata.Name == schematicExtensionName {
			return ext.TypedSpec().Metadata.Version, nil
		}
	}

	return "", nil
}

func defaultInstallerImageBase() string {
	if base := os.Getenv("INSTALLER_IMAGE_BASE"); base != "" {
		return base
	}

	return factoryInstallerBase + "/" + vanillaSchematic
}
