package controller

import (
	"context"
	"errors"
	"fmt"
	"io"

	clusterapi "github.com/siderolabs/talos/pkg/machinery/api/cluster"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
	"github.com/openresearch/talos-lifecycle/internal/talosutil"
)

type clusterNode struct {
	Name, Address, Role, Kubelet string
}

func clusterNodes(ctx context.Context, c client.Reader) ([]clusterNode, error) {
	var list corev1.NodeList
	if err := c.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	nodes := make([]clusterNode, 0, len(list.Items))

	for i := range list.Items {
		n := &list.Items[i]

		node := clusterNode{Name: n.Name, Role: upgradev1alpha1.RoleWorker, Kubelet: n.Status.NodeInfo.KubeletVersion}
		if _, ok := n.Labels["node-role.kubernetes.io/control-plane"]; ok {
			node.Role = upgradev1alpha1.RoleControlPlane
		}

		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP {
				node.Address = a.Address

				break
			}
		}

		nodes = append(nodes, node)
	}

	return nodes, nil
}

func ClusterHealth(ctx context.Context, c client.Reader) (healthy bool, reason string, err error) {
	nodes, err := clusterNodes(ctx, c)
	if err != nil {
		return false, "", err
	}

	var controlPlane, workers []string

	for _, n := range nodes {
		switch {
		case n.Address == "":
		case n.Role == upgradev1alpha1.RoleControlPlane:
			controlPlane = append(controlPlane, n.Address)
		default:
			workers = append(workers, n.Address)
		}
	}

	return talosHealth(ctx, controlPlane, workers)
}

func talosHealth(ctx context.Context, controlPlane, workers []string) (healthy bool, reason string, err error) {
	if len(controlPlane) == 0 {
		return false, "", errors.New("no control-plane nodes with addresses")
	}

	endpoint := controlPlane[0]

	c, talosCtx, err := talosutil.Client(ctx, endpoint)
	if err != nil {
		return false, "", err
	}
	defer c.Close() //nolint:errcheck

	stream, err := c.ClusterHealthCheck(talosCtx, healthAttemptTimeout, &clusterapi.ClusterInfo{
		ControlPlaneNodes: controlPlane,
		WorkerNodes:       workers,
		ForceEndpoint:     endpoint,
	})
	if err != nil {
		return false, fmt.Sprintf("health check could not start: %v", err), nil
	}

	// a clean EOF means every check passed
	for {
		progress, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return true, "", nil
			}

			return false, fmt.Sprintf("health check failed: %v", err), nil
		}

		if progress.GetMessage() != "" {
			log.FromContext(ctx).V(1).Info("health check", "progress", progress.GetMessage())
		}
	}
}
