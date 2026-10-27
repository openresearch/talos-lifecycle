package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	talosruntime "github.com/siderolabs/talos/pkg/machinery/resources/runtime"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestUntilWindow(t *testing.T) {
	at := func(hhmm string) time.Time {
		parsed, err := time.Parse("15:04", hhmm)
		if err != nil {
			t.Fatalf("bad test time %q: %v", hhmm, err)
		}

		return parsed
	}

	overnight := &upgradev1alpha1.UpgradeWindow{Start: "20:00", End: "05:00"}
	daytime := &upgradev1alpha1.UpgradeWindow{Start: "09:00", End: "17:00"}

	cases := []struct {
		name   string
		now    string
		window *upgradev1alpha1.UpgradeWindow
		want   time.Duration
	}{
		{"inside overnight before midnight", "22:00", overnight, 0},
		{"inside overnight after midnight", "03:00", overnight, 0},
		{"outside overnight", "12:00", overnight, 8 * time.Hour},
		{"just after overnight window", "05:00", overnight, 15 * time.Hour},
		{"inside daytime", "12:00", daytime, 0},
		{"before daytime", "07:30", daytime, 90 * time.Minute},
		{"after daytime wraps to next day", "20:00", daytime, 13 * time.Hour},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := untilWindow(at(tc.now), tc.window); err != nil || got != tc.want {
				t.Errorf("untilWindow(%s) = %s, want %s", tc.now, got, tc.want)
			}
		})
	}
}

func TestCooldownRemaining(t *testing.T) {
	now := time.Now()
	cooldown := int64(60)
	plan := &upgradev1alpha1.UpgradePlan{
		Spec:   upgradev1alpha1.UpgradePlanSpec{CooldownMinutes: &cooldown},
		Status: upgradev1alpha1.UpgradePlanStatus{LastCompletedAt: &metav1.Time{Time: now.Add(-30 * time.Minute)}},
	}

	if remaining := cooldownRemaining(now, plan); remaining != 30*time.Minute {
		t.Errorf("cooldownRemaining = %s, want 30m", remaining)
	}

	zero := int64(0)

	plan.Spec.CooldownMinutes = &zero
	if remaining := cooldownRemaining(now, plan); remaining != 0 {
		t.Errorf("cooldownRemaining with cooldown 0 = %s, want 0", remaining)
	}

	plan.Spec.CooldownMinutes = &cooldown

	plan.Status.LastCompletedAt = &metav1.Time{Time: now.Add(-2 * time.Hour)}
	if remaining := cooldownRemaining(now, plan); remaining != 0 {
		t.Errorf("cooldownRemaining after expiry = %s, want 0", remaining)
	}

	plan.Status.LastCompletedAt = nil
	if remaining := cooldownRemaining(now, plan); remaining != 0 {
		t.Errorf("cooldownRemaining without a completed upgrade = %s, want 0", remaining)
	}
}

func TestNextTalosNode(t *testing.T) {
	plan := &upgradev1alpha1.UpgradePlan{
		Spec: upgradev1alpha1.UpgradePlanSpec{TalosVersion: "v1.10.2"},
		Status: upgradev1alpha1.UpgradePlanStatus{
			Nodes: []upgradev1alpha1.NodeStatus{
				{Name: "worker-1", Role: "worker", CurrentVersion: "v1.9.5", Phase: upgradev1alpha1.NodePhasePending},
				{Name: "cp-2", Role: "control-plane", CurrentVersion: "v1.10.1", Phase: upgradev1alpha1.NodePhasePending},
				{Name: "cp-1", Role: "control-plane", CurrentVersion: "v1.10.0", Phase: upgradev1alpha1.NodePhasePending},
				{Name: "done", Role: "worker", CurrentVersion: "v1.10.2", Phase: upgradev1alpha1.NodePhaseSucceeded},
			},
		},
	}

	if next := nextTalosNode(plan); next == nil || next.Name != "cp-1" {
		t.Fatalf("nextTalosNode = %v, want cp-1", next)
	}

	plan.Status.Nodes[1].Phase = upgradev1alpha1.NodePhaseSucceeded
	plan.Status.Nodes[2].Phase = upgradev1alpha1.NodePhaseSucceeded

	if next := nextTalosNode(plan); next == nil || next.Name != "worker-1" {
		t.Fatalf("nextTalosNode = %v, want worker-1", next)
	}

	plan.Spec.TalosVersion = ""
	if next := nextTalosNode(plan); next != nil {
		t.Fatalf("nextTalosNode without target = %v, want nil", next)
	}
}

func TestNextVersion(t *testing.T) {
	available := []string{"v1.9.5", "v1.9.7", "v1.10.0", "v1.10.2", "v1.11.0", "v1.11.3"}

	cases := []struct {
		name    string
		current string
		want    string
	}{
		{"latest patch of current minor first", "v1.9.4", "v1.9.7"},
		{"then latest patch of next minor", "v1.9.7", "v1.10.2"},
		{"never skips a minor to the newest release", "v1.10.0", "v1.10.2"},
		{"next minor at its latest patch", "v1.10.2", "v1.11.3"},
		{"nothing newer available", "v1.11.3", ""},
		{"unknown current version: do not guess", "", ""},
	}

	gappy := []string{"v1.9.7", "v1.11.3", "v2.0.0"}
	if got := nextVersion(gappy, "v1.9.7"); got != "" {
		t.Errorf("gap in the feed: nextVersion = %q, want none (v1.10 missing)", got)
	}

	if got := nextVersion(gappy, "v1.11.3"); got != "" {
		t.Errorf("major bump: nextVersion = %q, want none", got)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextVersion(available, tc.current); got != tc.want {
				t.Errorf("nextVersion(%q) = %q, want %q", tc.current, got, tc.want)
			}
		})
	}
}

func TestLowestTalosVersion(t *testing.T) {
	plan := &upgradev1alpha1.UpgradePlan{
		Status: upgradev1alpha1.UpgradePlanStatus{
			Nodes: []upgradev1alpha1.NodeStatus{
				{Name: "cp-1", CurrentVersion: "v1.10.1"},
				{Name: "worker-1", CurrentVersion: "v1.9.5"},
				{Name: "worker-2", CurrentVersion: ""},
			},
		},
	}

	if got := plan.Status.LowestTalosVersion(); got != "v1.9.5" {
		t.Errorf("LowestTalosVersion = %s, want v1.9.5", got)
	}

	plan.Status.Nodes = nil
	if got := plan.Status.LowestTalosVersion(); got != "" {
		t.Errorf("LowestTalosVersion with no nodes = %s, want empty", got)
	}
}

func TestSetPhase(t *testing.T) {
	r := &UpgradePlanReconciler{}
	plan := &upgradev1alpha1.UpgradePlan{}

	r.setPhase(plan, upgradev1alpha1.PlanPhaseIdle, reasonUpToDate, "converged")

	if plan.Status.Phase != upgradev1alpha1.PlanPhaseIdle || plan.Status.Reason != reasonUpToDate {
		t.Fatalf("phase = %s/%s, want Idle/UpToDate", plan.Status.Phase, plan.Status.Reason)
	}

	r.setPhase(plan, upgradev1alpha1.PlanPhaseFrozen, reasonUpgradeFailed, "node w1 failed")

	if plan.Status.Phase != upgradev1alpha1.PlanPhaseFrozen || plan.Status.Message != "node w1 failed" {
		t.Fatalf("phase = %s, want Frozen with message", plan.Status.Phase)
	}
}

func TestSyncNodePhase(t *testing.T) {
	n := &upgradev1alpha1.NodeStatus{Name: "n1", CurrentVersion: "v1.10.2"}

	syncNodePhase(n, "v1.10.2")

	if n.Phase != upgradev1alpha1.NodePhaseSucceeded {
		t.Errorf("phase = %s, want Succeeded", n.Phase)
	}

	syncNodePhase(n, "v1.10.3")

	if n.Phase != upgradev1alpha1.NodePhasePending {
		t.Errorf("phase after new target = %s, want Pending", n.Phase)
	}

	n.Phase = upgradev1alpha1.NodePhaseFailed
	syncNodePhase(n, "v1.10.3")

	if n.Phase != upgradev1alpha1.NodePhaseFailed {
		t.Errorf("failed phase = %s, want sticky Failed", n.Phase)
	}

	n.CurrentVersion = "v1.10.3"
	syncNodePhase(n, "v1.10.3")

	if n.Phase != upgradev1alpha1.NodePhaseFailed {
		t.Errorf("failed-at-target phase = %s, want sticky Failed", n.Phase)
	}

	above := &upgradev1alpha1.NodeStatus{Name: "n2", CurrentVersion: "v1.14.1", Phase: upgradev1alpha1.NodePhaseSucceeded}
	syncNodePhase(above, "v1.13.10")

	if above.Phase != upgradev1alpha1.NodePhaseSucceeded {
		t.Errorf("node above target: phase = %s, want Succeeded", above.Phase)
	}

	n.Phase = upgradev1alpha1.NodePhaseUpgrading
	syncNodePhase(n, "v1.10.3")

	if n.Phase != upgradev1alpha1.NodePhaseUpgrading {
		t.Errorf("in-flight phase = %s, want Upgrading", n.Phase)
	}

	n.Phase = upgradev1alpha1.NodePhaseHealthChecking
	syncNodePhase(n, "v1.10.3")

	if n.Phase != upgradev1alpha1.NodePhaseHealthChecking {
		t.Errorf("in-flight phase = %s, want HealthChecking", n.Phase)
	}
}

func TestKubernetesPathSupported(t *testing.T) {
	if !kubernetesPathSupported("v1.35.0", "v1.35.6") {
		t.Error("1.35->1.35 should be supported")
	}

	if !kubernetesPathSupported("v1.35.6", "v1.36.2") {
		t.Error("1.35->1.36 should be supported")
	}

	if kubernetesPathSupported("v1.34.0", "v1.36.0") {
		t.Error("1.34->1.36 must not be supported (skips a minor)")
	}

	if kubernetesPathSupported("v1.99.0", "v1.99.1") {
		t.Error("unknown future minor must not be supported")
	}

	if kubernetesPathSupported("", "v1.35.0") {
		t.Error("empty current version must not be supported")
	}
}

func TestFailNodeRollbackDecision(t *testing.T) {
	r := &UpgradePlanReconciler{}
	enabled, disabled := true, false

	for _, autoRollback := range []*bool{nil, &enabled} {
		plan := &upgradev1alpha1.UpgradePlan{
			Spec: upgradev1alpha1.UpgradePlanSpec{TalosVersion: "v1.10.2", AutoRollback: autoRollback},
		}
		node := &upgradev1alpha1.NodeStatus{Name: "n1", Phase: upgradev1alpha1.NodePhaseUpgrading}
		r.failNode(plan, node, "timeout")

		if node.Phase != upgradev1alpha1.NodePhaseRollingBack {
			t.Errorf("autoRollback=%v: phase = %s, want RollingBack", autoRollback, node.Phase)
		}
	}

	plan := &upgradev1alpha1.UpgradePlan{
		Spec: upgradev1alpha1.UpgradePlanSpec{TalosVersion: "v1.10.2", AutoRollback: &disabled},
	}
	node := &upgradev1alpha1.NodeStatus{Name: "n1", Phase: upgradev1alpha1.NodePhaseUpgrading}
	r.failNode(plan, node, "timeout")

	if node.Phase != upgradev1alpha1.NodePhaseFailed {
		t.Errorf("autoRollback=false: phase = %s, want Failed", node.Phase)
	}

	node = &upgradev1alpha1.NodeStatus{
		Name:           "n1",
		Phase:          upgradev1alpha1.NodePhaseRollingBack,
		RollbackIssued: true,
	}
	planDefault := &upgradev1alpha1.UpgradePlan{
		Spec: upgradev1alpha1.UpgradePlanSpec{TalosVersion: "v1.10.2"},
	}
	r.failNode(planDefault, node, "rollback timed out")

	if node.Phase != upgradev1alpha1.NodePhaseFailed {
		t.Errorf("failure during rollback: phase = %s, want Failed", node.Phase)
	}
}

func TestResolveInstallerImageOverride(t *testing.T) {
	r := &UpgradePlanReconciler{}
	plan := &upgradev1alpha1.UpgradePlan{
		Spec: upgradev1alpha1.UpgradePlanSpec{
			TalosVersion:       "v1.10.2",
			InstallerImageBase: "registry.example.com/custom/installer",
		},
	}
	node := &upgradev1alpha1.NodeStatus{Name: "worker-1", Address: "10.0.0.10"}

	image, err := r.resolveInstallerImage(context.Background(), plan, node)
	if err != nil {
		t.Fatalf("resolveInstallerImage: %v", err)
	}

	if want := "registry.example.com/custom/installer:v1.10.2"; image != want {
		t.Errorf("image = %q, want %q", image, want)
	}
}

func TestNodeImageFactoryBase(t *testing.T) {
	cases := []struct {
		node NodeImage
		want string
	}{
		{NodeImage{Schematic: "abc", Platform: "nocloud"}, "factory.talos.dev/nocloud-installer/abc"},
		{NodeImage{Schematic: "abc", Platform: "hcloud"}, "factory.talos.dev/hcloud-installer/abc"},
		{NodeImage{Schematic: "abc", Platform: "metal", SecureBoot: true}, "factory.talos.dev/metal-installer-secureboot/abc"},
		{NodeImage{Platform: "metal"}, "factory.talos.dev/metal-installer/" + vanillaSchematic},
	}

	for _, c := range cases {
		if got := c.node.FactoryBase(); got != c.want {
			t.Errorf("%+v: FactoryBase() = %q, want %q", c.node, got, c.want)
		}
	}
}

func TestInFlightNode(t *testing.T) {
	plan := &upgradev1alpha1.UpgradePlan{
		Status: upgradev1alpha1.UpgradePlanStatus{
			Nodes: []upgradev1alpha1.NodeStatus{
				{Name: "a", Phase: upgradev1alpha1.NodePhaseSucceeded},
				{Name: "b", Phase: upgradev1alpha1.NodePhaseHealthChecking},
				{Name: "c", Phase: upgradev1alpha1.NodePhasePending},
			},
		},
	}

	if got := inFlightNode(plan); got == nil || got.Name != "b" {
		t.Fatalf("inFlightNode = %v, want node b (HealthChecking counts as in-flight)", got)
	}

	plan.Status.Nodes[1].Phase = upgradev1alpha1.NodePhaseFailed
	if got := inFlightNode(plan); got != nil {
		t.Fatalf("inFlightNode = %v, want nil (Failed is not in-flight)", got)
	}
}

func TestUnsupportedKubernetesPath(t *testing.T) {
	got := unsupportedKubernetesPath("v1.35.0", "v1.37.1")
	if !strings.Contains(got, "skips a minor") || !strings.Contains(got, "v1.36") {
		t.Errorf("two-minor jump: %q, want it to name the skip and v1.36", got)
	}

	if kubernetesPathSupported("v1.35.0", "v1.37.1") {
		t.Error("1.35 -> 1.37 must not be supported")
	}

	if got := unsupportedKubernetesPath("v1.35.0", "v1.36.0"); !strings.Contains(got, "controller build") {
		t.Errorf("adjacent minor: %q, want it to point at the controller build", got)
	}
}

func TestMinorGap(t *testing.T) {
	for _, tc := range []struct {
		from, to string
		want     int
	}{
		{"v1.35.0", "v1.36.4", 1},
		{"v1.35.0", "v1.37.1", 2},
		{"v1.35.0", "v1.35.9", 0},
		{"v1.35.0", "v2.0.0", 0},
		{"garbage", "v1.36.0", 0},
	} {
		if got := minorGap(tc.from, tc.to); got != tc.want {
			t.Errorf("minorGap(%s, %s) = %d, want %d", tc.from, tc.to, got, tc.want)
		}
	}
}

func TestNextTalosNodeSkipsNodesAboveTarget(t *testing.T) {
	plan := &upgradev1alpha1.UpgradePlan{
		Spec: upgradev1alpha1.UpgradePlanSpec{TalosVersion: "v1.13.12"},
		Status: upgradev1alpha1.UpgradePlanStatus{Nodes: []upgradev1alpha1.NodeStatus{
			{Name: "manual-0", CurrentVersion: "v1.14.4", Phase: upgradev1alpha1.NodePhasePending},
			{Name: testWorkerName, CurrentVersion: "v1.13.5", Phase: upgradev1alpha1.NodePhasePending},
			{Name: "manual-1", CurrentVersion: "v1.14.4", Phase: upgradev1alpha1.NodePhasePending},
		}},
	}

	if next := nextTalosNode(plan); next == nil || next.Name != testWorkerName {
		t.Fatalf("nextTalosNode = %v, want worker-0", next)
	}

	plan.Status.Nodes[1].CurrentVersion = "v1.13.12"
	if next := nextTalosNode(plan); next != nil {
		t.Errorf("nextTalosNode = %s, want nil once only nodes above the target remain", next.Name)
	}
}

func TestEtcdQuorumFailsClosedWithoutAddresses(t *testing.T) {
	r := &UpgradePlanReconciler{}
	plan := &upgradev1alpha1.UpgradePlan{Status: upgradev1alpha1.UpgradePlanStatus{Nodes: []upgradev1alpha1.NodeStatus{
		{Name: "cp-0", Role: upgradev1alpha1.RoleControlPlane},
	}}}

	gate, err := r.etcdQuorumReady(context.Background(), plan)
	if err != nil {
		t.Fatalf("etcdQuorumReady: %v", err)
	}

	if gate.passed() {
		t.Error("an unchecked quorum must block the upgrade")
	}
}

func TestNodeTargetIgnoresSpecChanges(t *testing.T) {
	plan := &upgradev1alpha1.UpgradePlan{Spec: upgradev1alpha1.UpgradePlanSpec{TalosVersion: "v1.13.10"}}

	if got := nodeTarget(plan, &upgradev1alpha1.NodeStatus{TargetVersion: talos1142}); got != talos1142 {
		t.Errorf("nodeTarget = %q, want the version the node was sent", got)
	}

	if got := nodeTarget(plan, &upgradev1alpha1.NodeStatus{}); got != "v1.13.10" {
		t.Errorf("nodeTarget without a record = %q, want the spec", got)
	}
}

func TestSelfEvictionNeedsPodName(t *testing.T) {
	t.Setenv("POD_NAME", "")

	pod, err := (&UpgradePlanReconciler{}).ownPodOn(context.Background(), "worker-0")
	if err != nil || pod != nil {
		t.Errorf("pod=%v err=%v, want neither without POD_NAME", pod, err)
	}
}

func TestBumpTarget(t *testing.T) {
	r := &UpgradePlanReconciler{}
	plan := &upgradev1alpha1.UpgradePlan{}
	target, warned := "v1.14.1", ""

	if r.bumpTarget(plan, "talos", &target, talos1142, "", &warned) != true || target != talos1142 {
		t.Errorf("supported newer: target %q, want %s", target, talos1142)
	}

	if r.bumpTarget(plan, "talos", &target, talos1142, "", &warned) {
		t.Error("same version must not count as a change")
	}

	if r.bumpTarget(plan, "talos", &target, "v1.15.0", "unsupported", &warned) || target != talos1142 {
		t.Errorf("unsupported: target %q, want unchanged", target)
	}

	if warned != "v1.15.0" {
		t.Errorf("warned = %q, want the skipped target recorded once", warned)
	}

	if r.bumpTarget(plan, "talos", &target, "", "", &warned) {
		t.Error("no talos1142 version must not count as a change")
	}
}

func TestAutoBumpTargets(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := upgradev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	plan := &upgradev1alpha1.UpgradePlan{
		ObjectMeta: metav1.ObjectMeta{Name: upgradev1alpha1.UpgradePlanName},
		Spec:       upgradev1alpha1.UpgradePlanSpec{Channel: upgradev1alpha1.ChannelLatest, TalosVersion: "v1.14.1"},
		Status: upgradev1alpha1.UpgradePlanStatus{
			AvailableTalosVersions: []string{talos1142, "v1.14.3"},
			Kubernetes:             upgradev1alpha1.KubernetesStatus{CurrentVersion: "v1.32.0"},
			Nodes:                  []upgradev1alpha1.NodeStatus{{Name: "n", CurrentVersion: "v1.14.1", Phase: upgradev1alpha1.NodePhaseSucceeded}},
		},
	}
	r := &UpgradePlanReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(plan).Build()}

	bumped, err := r.autoBumpTargets(context.Background(), plan)
	if err != nil || !bumped || plan.Spec.TalosVersion != "v1.14.3" {
		t.Fatalf("bumped=%v err=%v target=%q, want v1.14.3 (latest patch of the minor)", bumped, err, plan.Spec.TalosVersion)
	}

	plan.Status.Nodes[0].Phase = upgradev1alpha1.NodePhaseFailed
	plan.Status.AvailableTalosVersions = []string{"v1.14.4"}

	if bumped, _ := r.autoBumpTargets(context.Background(), plan); bumped {
		t.Error("a frozen plan must not get a new target")
	}
}

func TestPairSupported(t *testing.T) {
	for _, tc := range []struct {
		talos, kubernetes string
		ok                bool
	}{
		{"v1.12.3", "v1.31.4", true},
		{"v1.13.5", "v1.31.4", true},
		{talos1142, "v1.31.4", false},
		{"v1.12.3", "v1.36.0", false},
		{"", "v1.31.4", false},
	} {
		if got := pairSupported(tc.talos, tc.kubernetes) == ""; got != tc.ok {
			t.Errorf("pairSupported(%s, %s) ok=%v, want %v", tc.talos, tc.kubernetes, got, tc.ok)
		}
	}
}

func compatPlan(talosTarget, k8sTarget, k8sCurrent string, running ...string) *upgradev1alpha1.UpgradePlan {
	plan := &upgradev1alpha1.UpgradePlan{
		Spec: upgradev1alpha1.UpgradePlanSpec{TalosVersion: talosTarget, KubernetesVersion: k8sTarget},
		Status: upgradev1alpha1.UpgradePlanStatus{
			Kubernetes: upgradev1alpha1.KubernetesStatus{CurrentVersion: k8sCurrent},
		},
	}
	for i, v := range running {
		plan.Status.Nodes = append(plan.Status.Nodes, upgradev1alpha1.NodeStatus{
			Name: fmt.Sprintf("n%d", i), CurrentVersion: v, Phase: upgradev1alpha1.NodePhasePending,
		})
	}

	return plan
}

func TestCompatibilityGateAndOrder(t *testing.T) {
	plan := compatPlan(talos1142, "v1.31.4", "v1.31.4", "v1.13.5")
	next := orderTalosAndKubernetes(plan, &plan.Status.Nodes[0])

	if why := compatibilityGate(plan, next); !strings.Contains(why, "spec.kubernetesVersion") {
		t.Errorf("gate = %q, want it to ask for a Kubernetes target", why)
	}

	plan = compatPlan(talos1142, "v1.32.3", "v1.31.4", "v1.13.5")

	next = orderTalosAndKubernetes(plan, &plan.Status.Nodes[0])
	if next != nil {
		t.Fatalf("next = %s, want Kubernetes first", next.Name)
	}

	if why := compatibilityGate(plan, next); why != "" {
		t.Errorf("gate = %q, want Kubernetes 1.32 on Talos 1.13 to pass", why)
	}

	plan = compatPlan("v1.12.3", "v1.36.0", "v1.35.0", "v1.12.3")
	if why := compatibilityGate(plan, nil); !strings.Contains(why, "upgrade talos first") {
		t.Errorf("gate = %q, want it to ask for a Talos upgrade first", why)
	}

	plan = compatPlan("v1.13.5", "v1.32.3", "v1.31.4", "v1.12.3")
	next = orderTalosAndKubernetes(plan, &plan.Status.Nodes[0])

	if next == nil || compatibilityGate(plan, next) != "" {
		t.Error("Talos 1.13 on Kubernetes 1.31 should go first and pass")
	}
}

func TestAutoBumpWalksInStep(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := upgradev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	plan := compatPlan("v1.13.5", "v1.31.4", "v1.31.4", "v1.13.5")
	plan.Name = upgradev1alpha1.UpgradePlanName
	plan.Spec.Channel = upgradev1alpha1.ChannelLatest
	plan.Status.Nodes[0].Phase = upgradev1alpha1.NodePhaseSucceeded
	plan.Status.AvailableTalosVersions = []string{talos1142}
	plan.Status.AvailableKubernetesVersions = []string{"v1.32.3"}
	r := &UpgradePlanReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(plan).Build()}

	if _, err := r.autoBumpTargets(context.Background(), plan); err != nil {
		t.Fatal(err)
	}

	if plan.Spec.TalosVersion != "v1.13.5" || plan.Spec.KubernetesVersion != "v1.32.3" {
		t.Errorf("targets talos=%s kubernetes=%s, want Kubernetes 1.32 first and Talos held at 1.13",
			plan.Spec.TalosVersion, plan.Spec.KubernetesVersion)
	}

	plan.Status.Kubernetes.CurrentVersion = "v1.32.3"
	if _, err := r.autoBumpTargets(context.Background(), plan); err != nil {
		t.Fatal(err)
	}

	if plan.Spec.TalosVersion != talos1142 {
		t.Errorf("talos target = %s, want v1.14.2 once Kubernetes is 1.32", plan.Spec.TalosVersion)
	}
}

func TestBlockedMetricAndEvent(t *testing.T) {
	r := &UpgradePlanReconciler{}
	plan := &upgradev1alpha1.UpgradePlan{}

	r.setPhase(plan, upgradev1alpha1.PlanPhaseWaiting, reasonIncompatibleVersions, "talos 1.14 cannot run kubernetes 1.31")

	if got := testutil.ToFloat64(metricPlanBlocked.WithLabelValues(reasonIncompatibleVersions)); got != 1 {
		t.Errorf("blocked{IncompatibleVersions} = %v, want 1", got)
	}

	r.setPhase(plan, upgradev1alpha1.PlanPhaseWaiting, reasonCoolingDown, "cooling down")

	if got := testutil.ToFloat64(metricPlanBlocked.WithLabelValues(reasonIncompatibleVersions)); got != 0 {
		t.Errorf("blocked{IncompatibleVersions} = %v after a normal wait, want 0", got)
	}
}

const talos1142 = "v1.14.2"

func TestUpgradeNeverStarted(t *testing.T) {
	for _, tc := range []struct {
		stage talosruntime.MachineStage
		since time.Duration
		want  bool
	}{
		{talosruntime.MachineStageRunning, 5 * time.Minute, true},
		{talosruntime.MachineStageRunning, 30 * time.Second, false},
		{talosruntime.MachineStageUpgrading, 5 * time.Minute, false},
		{talosruntime.MachineStageRebooting, 5 * time.Minute, false},
		{talosruntime.MachineStageBooting, 5 * time.Minute, false},
	} {
		if got := upgradeNeverStarted(tc.stage, tc.since); got != tc.want {
			t.Errorf("upgradeNeverStarted(%s, %s) = %v, want %v", tc.stage, tc.since, got, tc.want)
		}
	}
}

func TestAutoBumpWaitsForKubernetesHealthCheck(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := upgradev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	plan := compatPlan(talos1142, k8s1359, k8s1359, talos1142)
	plan.Name = upgradev1alpha1.UpgradePlanName
	plan.Spec.Channel = upgradev1alpha1.ChannelLatest
	plan.Status.Nodes[0].Phase = upgradev1alpha1.NodePhaseSucceeded
	plan.Status.Kubernetes.Phase = upgradev1alpha1.NodePhaseHealthChecking
	plan.Status.AvailableKubernetesVersions = []string{"v1.36.5"}
	r := &UpgradePlanReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(plan).Build()}

	if bumped, _ := r.autoBumpTargets(context.Background(), plan); bumped || plan.Spec.KubernetesVersion != k8s1359 {
		t.Errorf("bumped=%v target=%s during the Kubernetes health check, want no change", bumped, plan.Spec.KubernetesVersion)
	}
}

func TestKubernetesTargetIgnoresSpecChanges(t *testing.T) {
	plan := &upgradev1alpha1.UpgradePlan{Spec: upgradev1alpha1.UpgradePlanSpec{KubernetesVersion: "v1.36.5"}}
	plan.Status.Kubernetes.TargetVersion = k8s1359

	if got := kubernetesTarget(plan); got != k8s1359 {
		t.Errorf("kubernetesTarget = %s, want the version the upgrade was started for", got)
	}

	plan.Status.Kubernetes.TargetVersion = ""
	if got := kubernetesTarget(plan); got != "v1.36.5" {
		t.Errorf("kubernetesTarget without a record = %s, want the spec", got)
	}
}

const k8s1359 = "v1.35.9"

func TestCooldownSurvivesANewTarget(t *testing.T) {
	two := int64(2)
	now := time.Now()
	plan := &upgradev1alpha1.UpgradePlan{
		Spec: upgradev1alpha1.UpgradePlanSpec{CooldownMinutes: &two},
		Status: upgradev1alpha1.UpgradePlanStatus{
			LastCompletedAt: &metav1.Time{Time: now.Add(-30 * time.Second)},
			Nodes: []upgradev1alpha1.NodeStatus{
				{Name: "cp-0", Phase: upgradev1alpha1.NodePhasePending, StartedAt: metav1.NewTime(now.Add(-5 * time.Minute))},
			},
		},
	}

	if got := cooldownRemaining(now, plan); got != 90*time.Second {
		t.Errorf("cooldown after a new target = %s, want 1m30s", got)
	}

	plan.Status.LastCompletedAt = nil
	plan.Status.Nodes[0].Phase = upgradev1alpha1.NodePhaseSucceeded
	plan.Status.Nodes[0].LastTransition = metav1.NewTime(now)

	if got := cooldownRemaining(now, plan); got != 0 {
		t.Errorf("cooldown without a completed upgrade = %s, want 0", got)
	}
}

func TestVerifyAsksTheNodeBeforeTheBudget(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := upgradev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	ready := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-0"},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
	r := &UpgradePlanReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(ready).Build()}

	plan := &upgradev1alpha1.UpgradePlan{Spec: upgradev1alpha1.UpgradePlanSpec{TalosVersion: talos1142}}
	plan.Status.Nodes = []upgradev1alpha1.NodeStatus{{
		Name: "worker-0", Address: "10.0.0.10", CurrentVersion: talos1142, TargetVersion: talos1142,
		Phase: upgradev1alpha1.NodePhaseVerifying, LastTransition: metav1.NewTime(time.Now().Add(-time.Hour)),
	}}
	node := &plan.Status.Nodes[0]

	if _, err := r.driveNodeUpgrade(context.Background(), plan, node); err != nil {
		t.Fatalf("driveNodeUpgrade: %v", err)
	}

	if node.Phase != upgradev1alpha1.NodePhaseHealthChecking {
		t.Errorf("phase = %q (%s), want HealthChecking: the node is at target and Ready", node.Phase, node.Message)
	}
}

func TestObserveKeepsVanishedNodeInFlightOrFailed(t *testing.T) {
	for _, phase := range []upgradev1alpha1.NodePhase{upgradev1alpha1.NodePhaseUpgrading, upgradev1alpha1.NodePhaseFailed} {
		t.Run(string(phase), func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := upgradev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}

			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}

			live := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}
			r := &UpgradePlanReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(live).Build()}

			plan := &upgradev1alpha1.UpgradePlan{Spec: upgradev1alpha1.UpgradePlanSpec{TalosVersion: talos1142}}
			plan.Status.Nodes = []upgradev1alpha1.NodeStatus{
				{Name: "worker-0", Address: "10.0.0.10", CurrentVersion: "v1.14.0", Phase: phase},
				{Name: "worker-1", CurrentVersion: "v1.14.0", Phase: upgradev1alpha1.NodePhasePending},
			}

			if err := r.observeNodes(context.Background(), plan); err != nil {
				t.Fatalf("observeNodes: %v", err)
			}

			if len(plan.Status.Nodes) != 2 || plan.Status.Nodes[0].Phase != phase {
				t.Fatalf("nodes = %+v, want worker-0 kept as %s", plan.Status.Nodes, phase)
			}
		})
	}
}
