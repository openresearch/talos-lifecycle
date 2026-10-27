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
	factoryRegistry = "factory.talos.dev"

	// Image Factory schematic without extensions: the stock Talos installer
	vanillaSchematic = "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba"

	// the Image Factory records the schematic ID as this extension's version
	schematicExtensionName = "schematic"
)

type NodeImage struct {
	Schematic  string // empty: not an Image Factory image
	Platform   string
	SecureBoot bool
}

// untagged: factory.talos.dev/<platform>-installer[-secureboot]/<schematic>
func (n NodeImage) FactoryBase() string {
	repo := n.Platform + "-installer"
	if n.SecureBoot {
		repo += "-secureboot"
	}

	schematic := n.Schematic
	if schematic == "" {
		schematic = vanillaSchematic
	}

	return fmt.Sprintf("%s/%s/%s", factoryRegistry, repo, schematic)
}

// resolveInstallerImage uses, in order: spec.installerImageBase, the node's own
// Image Factory schematic (so it keeps its extensions), INSTALLER_IMAGE_BASE,
// the stock installer. The platform and Secure Boot always come from the node.
func (r *UpgradePlanReconciler) resolveInstallerImage(
	ctx context.Context,
	plan *upgradev1alpha1.UpgradePlan,
	node *upgradev1alpha1.NodeStatus,
) (string, error) {
	log := log.FromContext(ctx)

	if base := plan.Spec.InstallerImageBase; base != "" {
		return fmt.Sprintf("%s:%s", base, plan.Spec.TalosVersion), nil
	}

	nodeImage, err := ReadNodeImage(ctx, node.Address)
	if err != nil {
		return "", fmt.Errorf(
			"read image from node %s: %w (set spec.installerImageBase to override)",
			node.Name, err)
	}

	if nodeImage.Schematic != "" {
		image := fmt.Sprintf("%s:%s", nodeImage.FactoryBase(), plan.Spec.TalosVersion)
		log.V(1).Info("resolved installer image from node",
			"node", node.Name, "schematic", nodeImage.Schematic,
			"platform", nodeImage.Platform, "secureBoot", nodeImage.SecureBoot)

		return image, nil
	}

	base := os.Getenv("INSTALLER_IMAGE_BASE")
	if base == "" {
		base = nodeImage.FactoryBase()
	}

	image := fmt.Sprintf("%s:%s", base, plan.Spec.TalosVersion)
	r.event(plan, corev1.EventTypeNormal, "InstallerImageDefault",
		fmt.Sprintf("node %s has no Image Factory schematic; using %s", node.Name, image))

	return image, nil
}

// the installer bakes in talos.platform, so an unknown platform is an error, not metal
func ReadNodeImage(ctx context.Context, address string) (NodeImage, error) {
	c, talosCtx, err := talosutil.Client(ctx, address)
	if err != nil {
		return NodeImage{}, err
	}
	defer c.Close() //nolint:errcheck

	var image NodeImage

	extensions, err := safe.StateListAll[*talosruntime.ExtensionStatus](talosCtx, c.COSI)
	if err != nil {
		return NodeImage{}, fmt.Errorf("list extension statuses: %w", err)
	}

	for ext := range extensions.All() {
		if ext.TypedSpec().Metadata.Name == schematicExtensionName {
			image.Schematic = ext.TypedSpec().Metadata.Version

			break
		}
	}

	platform, err := safe.StateGetByID[*talosruntime.PlatformMetadata](talosCtx, c.COSI, talosruntime.PlatformMetadataID)
	if err != nil {
		return NodeImage{}, fmt.Errorf("get platform metadata: %w", err)
	}

	if image.Platform = platform.TypedSpec().Platform; image.Platform == "" {
		return NodeImage{}, fmt.Errorf("platform metadata has no platform")
	}

	security, err := safe.StateGetByID[*talosruntime.SecurityState](talosCtx, c.COSI, talosruntime.SecurityStateID)
	if err != nil {
		return NodeImage{}, fmt.Errorf("get security state: %w", err)
	}

	image.SecureBoot = security.TypedSpec().SecureBoot

	return image, nil
}
