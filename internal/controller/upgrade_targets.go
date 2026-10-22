package controller

import (
	"context"
	"fmt"
	"sync"

	"github.com/siderolabs/go-kubernetes/kubernetes/upgrade"
	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/compatibility"
	"golang.org/x/mod/semver"
	corev1 "k8s.io/api/core/v1"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
)

// The Talos target is capped at the highest minor the compiled-in machinery
// knows: past it, the next Kubernetes upgrade fails.

const maxProbedTalosMinor = 99

// only has to parse: an unknown Talos minor fails before the Kubernetes range check
const probeKubernetesVersion = "v1.0.0"

var (
	maxTalosMinorOnce  sync.Once
	maxTalosMinorCache string
)

func talosMinorSupported(majorMinor string) bool {
	target, err := compatibility.ParseTalosVersion(&machine.VersionInfo{
		Tag: majorMinor + ".0",
	})
	if err != nil {
		return false
	}

	k8s, err := compatibility.ParseKubernetesVersion(probeKubernetesVersion)
	if err != nil {
		return false
	}

	err = k8s.SupportedWith(target)
	if err == nil {
		return true
	}

	return err.Error() != fmt.Sprintf("compatibility with version %s is not supported", target.String())
}

func maxSupportedTalosMinor() string {
	maxTalosMinorOnce.Do(func() {
		for minor := 0; minor <= maxProbedTalosMinor; minor++ {
			candidate := fmt.Sprintf("v1.%d", minor)
			if talosMinorSupported(candidate) {
				maxTalosMinorCache = candidate
			}
		}
	})

	return maxTalosMinorCache
}

func talosTargetSupported(version string) bool {
	ceiling := maxSupportedTalosMinor()
	if ceiling == "" || version == "" {
		return true
	}

	return semver.Compare(semver.MajorMinor(version), ceiling) <= 0
}

func (r *UpgradePlanReconciler) autoBumpTargets(ctx context.Context, plan *upgradev1alpha1.UpgradePlan) (bool, error) {
	if inFlightNode(plan) != nil || plan.Status.Kubernetes.Phase.InFlight() {
		return false, nil
	}

	if name, _ := failedTarget(plan); name != "" {
		return false, nil
	}

	talosNext := nextVersion(plan.Status.AvailableTalosVersions, plan.Status.LowestTalosVersion())
	talosWhy := ""

	if !talosTargetSupported(talosNext) {
		talosWhy = fmt.Sprintf("talos %s is newer than this controller build supports (max %s.x); "+
			"update the controller (talos machinery dependency) to upgrade further", talosNext, maxSupportedTalosMinor())
	}

	current := plan.Status.Kubernetes.CurrentVersion
	kubernetesNext := nextVersion(plan.Status.AvailableKubernetesVersions, current)
	kubernetesWhy := ""

	if !kubernetesPathSupported(current, kubernetesNext) {
		kubernetesWhy = fmt.Sprintf("kubernetes %s -> %s is not supported by this controller build; "+
			"update the controller (go-kubernetes dependency) to upgrade further", current, kubernetesNext)
	}

	if talosWhy == "" && pairSupported(talosNext, current) != "" {
		talosNext = ""
	}

	headingTo := plan.Spec.TalosVersion
	if talosNext != "" && talosWhy == "" && semver.Compare(talosNext, headingTo) > 0 {
		headingTo = talosNext
	}

	if kubernetesWhy == "" && kubernetesNext != "" && fitsTalos(kubernetesNext, append(runningTalos(plan), headingTo)) != "" {
		kubernetesNext = ""
	}

	changed := r.bumpTarget(plan, "talos", &plan.Spec.TalosVersion, talosNext, talosWhy, &r.lastSkippedTalosTarget)
	changed = r.bumpTarget(plan, "kubernetes", &plan.Spec.KubernetesVersion, kubernetesNext, kubernetesWhy,
		&r.lastSkippedKubernetesTarget) || changed

	if !changed {
		return false, nil
	}

	if err := r.Update(ctx, plan); err != nil {
		return false, fmt.Errorf("update spec with auto-upgrade targets: %w", err)
	}

	return true, nil
}

func (r *UpgradePlanReconciler) bumpTarget(
	plan *upgradev1alpha1.UpgradePlan, what string, target *string, next, unsupported string, warned *string,
) bool {
	switch {
	case next == "":
		return false
	case unsupported != "":
		if *warned != next {
			*warned = next

			r.event(plan, corev1.EventTypeWarning, "AutoUpgradeTargetSkipped", unsupported)
		}

		return false
	case semver.Compare(next, *target) <= 0:
		return false
	}

	r.event(plan, corev1.EventTypeNormal, "AutoUpgradeTarget",
		fmt.Sprintf("channel %s: %s target advanced from %s to %s", plan.Spec.Channel, what, *target, next))

	*target = next

	return true
}

func kubernetesPathSupported(from, to string) bool {
	if from == "" || to == "" {
		return false
	}

	path, err := upgrade.NewPath(from, to)
	if err != nil {
		return false
	}

	return path.IsSupported()
}

func unsupportedKubernetesPath(from, to string) string {
	if minorGap(from, to) > 1 {
		return fmt.Sprintf("kubernetes %s -> %s skips a minor; Kubernetes upgrades one minor at a time, "+
			"set the target to a %s release first", from, to, nextMinor(from))
	}

	return fmt.Sprintf("kubernetes %s -> %s is not supported by this controller build; "+
		"update the controller (go-kubernetes dependency)", from, to)
}

func minorGap(from, to string) int {
	fm, fn, ok1 := majorMinor(from)
	tm, tn, ok2 := majorMinor(to)

	if !ok1 || !ok2 || fm != tm {
		return 0
	}

	return tn - fn
}

func nextMinor(v string) string {
	major, minor, ok := majorMinor(v)
	if !ok {
		return ""
	}

	return fmt.Sprintf("v%d.%d", major, minor+1)
}

func majorMinor(v string) (major, minor int, ok bool) {
	if !semver.IsValid(v) {
		return 0, 0, false
	}

	_, err := fmt.Sscanf(semver.MajorMinor(v), "v%d.%d", &major, &minor)

	return major, minor, err == nil
}

func nextVersion(available []string, current string) string {
	if current == "" {
		return ""
	}

	adjacent := nextMinor(current)

	latestOfCurrentMinor := ""
	latestOfNextMinor := ""

	for _, v := range available {
		if semver.Compare(v, current) <= 0 {
			continue
		}

		switch semver.MajorMinor(v) {
		case semver.MajorMinor(current):
			if latestOfCurrentMinor == "" || semver.Compare(v, latestOfCurrentMinor) > 0 {
				latestOfCurrentMinor = v
			}
		case adjacent:
			if latestOfNextMinor == "" || semver.Compare(v, latestOfNextMinor) > 0 {
				latestOfNextMinor = v
			}
		}
	}

	if latestOfCurrentMinor != "" {
		return latestOfCurrentMinor
	}

	return latestOfNextMinor
}

func kubernetesUpgradePending(plan *upgradev1alpha1.UpgradePlan) bool {
	desired := plan.Spec.KubernetesVersion
	current := plan.Status.Kubernetes.CurrentVersion

	if desired == "" || current == "" {
		return false
	}

	return semver.Compare(desired, current) > 0
}

func pairSupported(talos, kubernetes string) string {
	if talos == "" || kubernetes == "" {
		return fmt.Sprintf("cannot check talos %q with kubernetes %q: version unknown", talos, kubernetes)
	}

	t, err := compatibility.ParseTalosVersion(&machine.VersionInfo{Tag: talos})
	if err != nil {
		return err.Error()
	}

	k, err := compatibility.ParseKubernetesVersion(kubernetes)
	if err != nil {
		return err.Error()
	}

	if err := k.SupportedWith(t); err != nil {
		return err.Error()
	}

	return ""
}

func fitsTalos(kubernetes string, talos []string) string {
	for _, v := range talos {
		if v == "" {
			continue
		}

		if why := pairSupported(v, kubernetes); why != "" {
			return why
		}
	}

	return ""
}

func runningTalos(plan *upgradev1alpha1.UpgradePlan) []string {
	var versions []string

	for _, n := range plan.Status.Nodes {
		if n.CurrentVersion != "" {
			versions = append(versions, n.CurrentVersion)
		}
	}

	return versions
}

// Kubernetes goes first (nil) when the Talos target cannot run the current
// Kubernetes but the Kubernetes target fits the Talos running now.
func orderTalosAndKubernetes(plan *upgradev1alpha1.UpgradePlan, next *upgradev1alpha1.NodeStatus) *upgradev1alpha1.NodeStatus {
	if next == nil || !kubernetesUpgradePending(plan) {
		return next
	}

	if pairSupported(plan.Spec.TalosVersion, plan.Status.Kubernetes.CurrentVersion) != "" &&
		fitsTalos(plan.Spec.KubernetesVersion, runningTalos(plan)) == "" {
		return nil
	}

	return next
}

func compatibilityGate(plan *upgradev1alpha1.UpgradePlan, next *upgradev1alpha1.NodeStatus) string {
	current := plan.Status.Kubernetes.CurrentVersion

	if next != nil {
		if why := pairSupported(plan.Spec.TalosVersion, current); why != "" {
			return fmt.Sprintf("talos %s cannot run the cluster's kubernetes %s (%s); "+
				"set spec.kubernetesVersion to a version both support so it goes first", plan.Spec.TalosVersion, current, why)
		}

		return ""
	}

	if why := fitsTalos(plan.Spec.KubernetesVersion, runningTalos(plan)); why != "" {
		return fmt.Sprintf("kubernetes %s cannot run on the cluster's talos (%s); "+
			"upgrade talos first or lower spec.kubernetesVersion", plan.Spec.KubernetesVersion, why)
	}

	return ""
}
