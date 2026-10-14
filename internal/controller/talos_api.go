package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/siderolabs/go-kubernetes/kubernetes/ssa"
	"github.com/siderolabs/go-kubernetes/kubernetes/upgrade"
	taloscluster "github.com/siderolabs/talos/pkg/cluster"
	k8s "github.com/siderolabs/talos/pkg/cluster/kubernetes"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/resources/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openresearch/talos-lifecycle/internal/talosutil"
)

type talosAPI interface {
	Version(ctx context.Context, address string) (string, error)
	Stage(ctx context.Context, address string) (runtime.MachineStage, error)
	// an error means the node did not take it, unless ctx ended first
	Upgrade(ctx context.Context, address, image string) error
	Rollback(ctx context.Context, address string) error
	Health(ctx context.Context, c client.Reader) (healthy bool, reason string, err error)
	KubernetesVersion(ctx context.Context, endpoint string) (string, error)
	// errKubernetesUpgradeFailed freezes the plan; other errors retry
	UpgradeKubernetes(ctx context.Context, endpoint, from, target string) error
}

var errKubernetesUpgradeFailed = errors.New("kubernetes upgrade failed")

func (r *UpgradePlanReconciler) nodes() talosAPI {
	if r.talos == nil {
		return liveTalos{}
	}

	return r.talos
}

type liveTalos struct{}

func (liveTalos) Version(ctx context.Context, address string) (string, error) {
	c, talosCtx, err := talosutil.Client(ctx, address)
	if err != nil {
		return "", err
	}
	defer c.Close() //nolint:errcheck

	v, err := c.Version(talosCtx)
	if err != nil {
		return "", err
	}

	if len(v.Messages) != 1 {
		return "", fmt.Errorf("expected version from exactly one node, got %d", len(v.Messages))
	}

	return v.Messages[0].Version.Tag, nil
}

func (liveTalos) Stage(ctx context.Context, address string) (runtime.MachineStage, error) {
	c, talosCtx, err := talosutil.Client(ctx, address)
	if err != nil {
		return runtime.MachineStageUnknown, err
	}
	defer c.Close() //nolint:errcheck

	ms, err := safe.StateGetByID[*runtime.MachineStatus](talosCtx, c.COSI, runtime.MachineStatusID)
	if err != nil {
		return runtime.MachineStageUnknown, fmt.Errorf("read machine status: %w", err)
	}

	return ms.TypedSpec().Stage, nil
}

func (liveTalos) Upgrade(ctx context.Context, address, image string) error {
	c, talosCtx, err := talosutil.Client(ctx, address)
	if err != nil {
		return err
	}
	defer c.Close() //nolint:errcheck

	// stage=false fails a bad image here, not at boot; force=false lets Talos refuse
	// while etcd is unhealthy
	_, err = c.Upgrade(talosCtx, image, false, false) //nolint:staticcheck

	return err
}

func (liveTalos) Rollback(ctx context.Context, address string) error {
	c, talosCtx, err := talosutil.Client(ctx, address)
	if err != nil {
		return err
	}
	defer c.Close() //nolint:errcheck

	return c.Rollback(talosCtx)
}

func (liveTalos) Health(ctx context.Context, c client.Reader) (bool, string, error) {
	return ClusterHealth(ctx, c)
}

type kubernetesProvider struct {
	taloscluster.ClientProvider
	taloscluster.K8sProvider
}

func newKubernetesProvider(c *talosclient.Client, endpoint string) *kubernetesProvider {
	cp := &taloscluster.ConfigClientProvider{DefaultClient: c}

	return &kubernetesProvider{
		ClientProvider: cp,
		K8sProvider:    &taloscluster.KubernetesClient{ClientProvider: cp, ForceEndpoint: endpoint},
	}
}

func (liveTalos) KubernetesVersion(ctx context.Context, endpoint string) (string, error) {
	c, talosCtx, err := talosutil.Client(ctx, endpoint)
	if err != nil {
		return "", err
	}
	defer c.Close() //nolint:errcheck

	v, err := k8s.DetectLowestVersion(talosCtx, newKubernetesProvider(c, endpoint), k8s.UpgradeOptions{})
	if err != nil {
		return "", err
	}

	return "v" + strings.TrimPrefix(v, "v"), nil
}

func (liveTalos) UpgradeKubernetes(ctx context.Context, endpoint, from, target string) error {
	c, talosCtx, err := talosutil.Client(ctx, endpoint)
	if err != nil {
		return err
	}
	defer c.Close() //nolint:errcheck

	upgradePath, err := upgrade.NewPath(from, target)
	if err != nil {
		return fmt.Errorf("%w: no upgrade path from %s to %s: %w", errKubernetesUpgradeFailed, from, target, err)
	}

	options := k8s.UpgradeOptions{
		Path:                   upgradePath,
		ControlPlaneEndpoint:   endpoint,
		PrePullImages:          true,
		UpgradeKubelet:         true,
		KubeletImage:           constants.KubeletImage,
		APIServerImage:         constants.KubernetesAPIServerImage,
		ControllerManagerImage: constants.KubernetesControllerManagerImage,
		SchedulerImage:         constants.KubernetesSchedulerImage,
		ProxyImage:             constants.KubeProxyImage,
		EncoderOpt:             encoder.WithComments(encoder.CommentsDisabled),
		// talosctl's default; MustMatch fails on objects without an inventory annotation
		InventoryPolicy: ssa.InventoryPolicyAdoptIfNoInventory,
	}

	log.FromContext(ctx).Info("kubernetes upgrade starting", "from", from, "to", target)

	if err := k8s.Upgrade(talosCtx, newKubernetesProvider(c, endpoint), options); err != nil {
		return fmt.Errorf("%w: upgrade to %s: %w", errKubernetesUpgradeFailed, target, err)
	}

	return nil
}
