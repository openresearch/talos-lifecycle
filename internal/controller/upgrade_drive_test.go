package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	talosruntime "github.com/siderolabs/talos/pkg/machinery/resources/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
)

type fakeTalos struct {
	version       string
	versionErr    error
	stage         talosruntime.MachineStage
	upgradeErr    error
	upgrades      []string
	duringUpgrade func()
	rollbacks     int

	healthy   bool
	healthErr error

	controlPlane     string
	rollouts         []string
	kubernetesErr    error
	duringKubernetes func()
}

func (f *fakeTalos) Version(context.Context, string) (string, error) { return f.version, f.versionErr }

func (f *fakeTalos) Stage(context.Context, string) (talosruntime.MachineStage, error) {
	return f.stage, nil
}

func (f *fakeTalos) Upgrade(_ context.Context, _, image string) error {
	f.upgrades = append(f.upgrades, image)
	if f.duringUpgrade != nil {
		f.duringUpgrade()
	}

	return f.upgradeErr
}

func (f *fakeTalos) Rollback(context.Context, string) error {
	f.rollbacks++

	return nil
}

func (f *fakeTalos) Health(context.Context, client.Reader) (bool, string, error) {
	return f.healthy, "fake says unhealthy", f.healthErr
}

func (f *fakeTalos) KubernetesVersion(context.Context, string) (string, error) {
	return f.controlPlane, nil
}

func (f *fakeTalos) UpgradeKubernetes(_ context.Context, _, from, _ string) error {
	f.rollouts = append(f.rollouts, from)
	if f.duringKubernetes != nil {
		f.duringKubernetes()
	}

	return f.kubernetesErr
}

func upgradeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	if err := upgradev1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}

	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}

	return s
}

func readyNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
}

func planWithNodeIn(phase upgradev1alpha1.NodePhase) *upgradev1alpha1.UpgradePlan {
	plan := &upgradev1alpha1.UpgradePlan{
		ObjectMeta: metav1.ObjectMeta{Name: upgradev1alpha1.UpgradePlanName},
		Spec:       upgradev1alpha1.UpgradePlanSpec{TalosVersion: talos1142, InstallerImageBase: "example.com/installer"},
	}
	plan.Status.Nodes = []upgradev1alpha1.NodeStatus{{
		Name: testWorkerName, Address: "10.0.0.10", Role: upgradev1alpha1.RoleWorker,
		CurrentVersion: "v1.14.0", TargetVersion: talos1142,
		Phase: phase, LastTransition: metav1.NewTime(time.Now().Add(-time.Hour)),
	}}

	return plan
}

func TestUpgradeLoopAsksTheNodeBeforeTheBudget(t *testing.T) {
	ctx := context.Background()

	t.Run("Upgrading, node at target", func(t *testing.T) {
		plan := planWithNodeIn(upgradev1alpha1.NodePhaseUpgrading)
		r := &UpgradePlanReconciler{talos: &fakeTalos{version: talos1142}}

		if _, err := r.driveNodeUpgrade(ctx, plan, &plan.Status.Nodes[0]); err != nil {
			t.Fatal(err)
		}

		if got := plan.Status.Nodes[0].Phase; got != upgradev1alpha1.NodePhaseVerifying {
			t.Errorf("phase = %s (%s), want Verifying: the node runs the target", got, plan.Status.Nodes[0].Message)
		}
	})

	t.Run("Upgrading, call dropped", func(t *testing.T) {
		plan := planWithNodeIn(upgradev1alpha1.NodePhaseUpgrading)
		r := &UpgradePlanReconciler{talos: &fakeTalos{version: "v1.14.0", stage: talosruntime.MachineStageRunning}}

		if _, err := r.driveNodeUpgrade(ctx, plan, &plan.Status.Nodes[0]); err != nil {
			t.Fatal(err)
		}

		if n := plan.Status.Nodes[0]; n.Phase != upgradev1alpha1.NodePhasePending || n.Resends != 1 {
			t.Errorf("phase = %s, resends %d, want Pending to resend once", n.Phase, n.Resends)
		}
	})

	t.Run("Upgrading, node unreachable", func(t *testing.T) {
		plan := planWithNodeIn(upgradev1alpha1.NodePhaseUpgrading)
		r := &UpgradePlanReconciler{talos: &fakeTalos{versionErr: errors.New("unreachable")}}

		if _, err := r.driveNodeUpgrade(ctx, plan, &plan.Status.Nodes[0]); err != nil {
			t.Fatal(err)
		}

		if got := plan.Status.Nodes[0].Phase; got != upgradev1alpha1.NodePhaseRollingBack {
			t.Errorf("phase = %s, want RollingBack: the budget still bounds a node that does not answer", got)
		}
	})

	t.Run("HealthChecking, cluster healthy", func(t *testing.T) {
		plan := planWithNodeIn(upgradev1alpha1.NodePhaseHealthChecking)
		r := &UpgradePlanReconciler{talos: &fakeTalos{healthy: true}}

		if _, err := r.driveNodeUpgrade(ctx, plan, &plan.Status.Nodes[0]); err != nil {
			t.Fatal(err)
		}

		if got := plan.Status.Nodes[0].Phase; got != upgradev1alpha1.NodePhaseSucceeded {
			t.Errorf("phase = %s (%s), want Succeeded: the cluster is healthy", got, plan.Status.Nodes[0].Message)
		}
	})

	t.Run("HealthChecking, cluster unhealthy", func(t *testing.T) {
		plan := planWithNodeIn(upgradev1alpha1.NodePhaseHealthChecking)
		r := &UpgradePlanReconciler{talos: &fakeTalos{}}

		if _, err := r.driveNodeUpgrade(ctx, plan, &plan.Status.Nodes[0]); err != nil {
			t.Fatal(err)
		}

		if got := plan.Status.Nodes[0].Phase; got != upgradev1alpha1.NodePhaseRollingBack {
			t.Errorf("phase = %s, want RollingBack after the budget", got)
		}
	})

	t.Run("Kubernetes HealthChecking, cluster healthy", func(t *testing.T) {
		plan := planWithNodeIn(upgradev1alpha1.NodePhaseSucceeded)
		plan.Status.Kubernetes = upgradev1alpha1.KubernetesStatus{
			Phase: upgradev1alpha1.NodePhaseHealthChecking, TargetVersion: k8s1359,
			LastTransition: metav1.NewTime(time.Now().Add(-time.Hour)),
		}
		r := &UpgradePlanReconciler{talos: &fakeTalos{healthy: true}}

		if _, err := r.driveKubernetesHealthCheck(ctx, plan); err != nil {
			t.Fatal(err)
		}

		if got := plan.Status.Kubernetes.Phase; got != upgradev1alpha1.NodePhaseSucceeded {
			t.Errorf("kubernetes phase = %s (%s), want Succeeded: the cluster is healthy", got, plan.Status.Kubernetes.Message)
		}
	})

	t.Run("RollingBack, node on the target", func(t *testing.T) {
		plan := planWithNodeIn(upgradev1alpha1.NodePhaseRollingBack)
		plan.Status.Nodes[0].LastTransition = metav1.Now()
		talos := &fakeTalos{version: talos1142}
		c := fake.NewClientBuilder().WithScheme(upgradeScheme(t)).WithObjects(plan).WithStatusSubresource(plan).Build()
		r := &UpgradePlanReconciler{Client: c, talos: talos}

		status := plan.Status

		if err := c.Get(ctx, client.ObjectKeyFromObject(plan), plan); err != nil {
			t.Fatal(err)
		}

		plan.Status = status

		for range 3 {
			if _, err := r.driveRollback(ctx, plan, &plan.Status.Nodes[0]); err != nil {
				t.Fatal(err)
			}
		}

		if talos.rollbacks != 1 || !plan.Status.Nodes[0].RollbackIssued {
			t.Errorf("rollbacks = %d, issued = %v; want exactly one", talos.rollbacks, plan.Status.Nodes[0].RollbackIssued)
		}
	})

	t.Run("RollingBack, issued and done", func(t *testing.T) {
		plan := planWithNodeIn(upgradev1alpha1.NodePhaseRollingBack)
		plan.Status.Nodes[0].RollbackIssued = true
		r := &UpgradePlanReconciler{
			Client: fake.NewClientBuilder().WithScheme(upgradeScheme(t)).WithObjects(readyNode(testWorkerName)).Build(),
			talos:  &fakeTalos{version: "v1.14.0"},
		}

		if _, err := r.driveRollback(ctx, plan, &plan.Status.Nodes[0]); err != nil {
			t.Fatal(err)
		}

		if msg := plan.Status.Nodes[0].Message; !strings.Contains(msg, "rolled back to v1.14.0") {
			t.Errorf("message = %q, want the completed rollback, not a timeout", msg)
		}
	})
}

func TestRefusedUpgradeWaitsWithAReason(t *testing.T) {
	ctx := context.Background()

	plan := planWithNodeIn(upgradev1alpha1.NodePhasePending)
	plan.Status.Kubernetes.CurrentVersion = k8s1359
	c := fake.NewClientBuilder().WithScheme(upgradeScheme(t)).
		WithObjects(plan, readyNode(testWorkerName)).
		WithStatusSubresource(&upgradev1alpha1.UpgradePlan{}).Build()
	talos := &fakeTalos{upgradeErr: errors.New("rpc error: code = Unknown desc = failed to pull image: not found")}
	r := &UpgradePlanReconciler{Client: c, talos: talos}

	pass := func() *upgradev1alpha1.UpgradePlan {
		t.Helper()

		var current upgradev1alpha1.UpgradePlan
		if err := c.Get(ctx, client.ObjectKeyFromObject(plan), &current); err != nil {
			t.Fatal(err)
		}

		if _, err := r.step(ctx, &current); err != nil {
			t.Fatalf("step returned %v, want a gate: the operator has to fix the target or the registry", err)
		}

		if err := c.Get(ctx, client.ObjectKeyFromObject(plan), &current); err != nil {
			t.Fatal(err)
		}

		return &current
	}

	stored := pass()
	if stored.Status.Phase != upgradev1alpha1.PlanPhaseWaiting || stored.Status.Reason != reasonUpgradeRefused {
		t.Errorf("plan = %s/%s, want Waiting/%s", stored.Status.Phase, stored.Status.Reason, reasonUpgradeRefused)
	}

	if n := stored.Status.Nodes[0]; n.Phase != upgradev1alpha1.NodePhasePending || !strings.Contains(n.Message, "failed to pull image") {
		t.Errorf("node = %s (%s), want Pending with the refusal", n.Phase, n.Message)
	}

	if len(talos.upgrades) != 1 {
		t.Fatalf("upgrade calls = %d, want 1", len(talos.upgrades))
	}

	if stored = pass(); len(talos.upgrades) != 1 || stored.Status.Reason != reasonUpgradeRefused {
		t.Errorf("calls = %d, reason %s on the next pass, want 1 and still %s", len(talos.upgrades), stored.Status.Reason, reasonUpgradeRefused)
	}

	stored.Status.Nodes[0].LastTransition = metav1.NewTime(time.Now().Add(-upgradeRefusedRetry - time.Minute))
	if err := c.Status().Update(ctx, stored); err != nil {
		t.Fatal(err)
	}

	if pass(); len(talos.upgrades) != 2 {
		t.Errorf("calls = %d after the retry interval, want 2", len(talos.upgrades))
	}
}

func kubernetesRolloutPlan(t *testing.T) (*upgradev1alpha1.UpgradePlan, client.Client) {
	t.Helper()

	plan := &upgradev1alpha1.UpgradePlan{
		ObjectMeta: metav1.ObjectMeta{Name: upgradev1alpha1.UpgradePlanName},
		Spec:       upgradev1alpha1.UpgradePlanSpec{KubernetesVersion: k8s1359},
	}
	plan.Status.Phase = upgradev1alpha1.PlanPhaseUpgrading
	plan.Status.Nodes = []upgradev1alpha1.NodeStatus{{Name: "control-plane-0", Address: "10.0.0.1",
		Role: upgradev1alpha1.RoleControlPlane, Phase: upgradev1alpha1.NodePhaseSucceeded}}
	plan.Status.Kubernetes = upgradev1alpha1.KubernetesStatus{
		Phase: upgradev1alpha1.NodePhaseUpgrading, TargetVersion: k8s1359, LastTransition: metav1.Now(),
	}

	cp := readyNode("control-plane-0")
	cp.Labels = map[string]string{"node-role.kubernetes.io/control-plane": ""}
	cp.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.1"}}

	c := fake.NewClientBuilder().WithScheme(upgradeScheme(t)).WithObjects(plan, cp).
		WithStatusSubresource(&upgradev1alpha1.UpgradePlan{}).Build()

	return plan, c
}

func TestFailedKubernetesRolloutSurvivesAConcurrentWrite(t *testing.T) {
	ctx := context.Background()
	plan, c := kubernetesRolloutPlan(t)

	talos := &fakeTalos{controlPlane: "v1.34.2",
		kubernetesErr: fmt.Errorf("%w: upgrade to %s: kube-apiserver did not become ready", errKubernetesUpgradeFailed, k8s1359)}
	talos.duringKubernetes = func() {
		var other upgradev1alpha1.UpgradePlan
		if err := c.Get(ctx, client.ObjectKeyFromObject(plan), &other); err != nil {
			t.Fatal(err)
		}

		other.Status.AvailableKubernetesVersions = []string{"v1.36.0"}
		if err := c.Status().Update(ctx, &other); err != nil {
			t.Fatal(err)
		}
	}
	r := &UpgradePlanReconciler{Client: c, talos: talos}

	if _, err := r.Reconcile(ctx, clusterRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var stored upgradev1alpha1.UpgradePlan
	if err := c.Get(ctx, client.ObjectKeyFromObject(plan), &stored); err != nil {
		t.Fatal(err)
	}

	if got := stored.Status.Kubernetes.Phase; got != upgradev1alpha1.NodePhaseFailed {
		t.Errorf("stored kubernetes phase = %s, want Failed: the next pass would run the rollout again", got)
	}
}

func TestInterruptedKubernetesRolloutResumes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	plan, c := kubernetesRolloutPlan(t)

	talos := &fakeTalos{controlPlane: "v1.34.2",
		kubernetesErr: fmt.Errorf("%w: upgrade to %s: %w", errKubernetesUpgradeFailed, k8s1359, context.Canceled)}
	talos.duringKubernetes = cancel
	r := &UpgradePlanReconciler{Client: c, talos: talos}

	if _, err := r.driveKubernetesUpgrade(ctx, plan); err == nil {
		t.Error("driveKubernetesUpgrade returned no error for an interrupted rollout")
	}

	if got := plan.Status.Kubernetes.Phase; got != upgradev1alpha1.NodePhaseUpgrading {
		t.Errorf("kubernetes phase = %s, want Upgrading so the next leader resumes", got)
	}
}

func TestSelfEvictionIntentBeforeTheEviction(t *testing.T) {
	t.Setenv("POD_NAME", "controller")
	t.Setenv("POD_NAMESPACE", "talos-lifecycle")

	for _, refuse := range []bool{false, true} {
		ctx := context.Background()
		plan := planWithNodeIn(upgradev1alpha1.NodePhasePending)
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "controller", Namespace: "talos-lifecycle"},
			Spec:       corev1.PodSpec{NodeName: testWorkerName},
		}

		var storedAtEviction *bool

		c := fake.NewClientBuilder().WithScheme(upgradeScheme(t)).WithObjects(plan, pod).
			WithStatusSubresource(&upgradev1alpha1.UpgradePlan{}).
			WithInterceptorFuncs(interceptor.Funcs{SubResourceCreate: func(ctx context.Context, c client.Client, sub string,
				obj, subObj client.Object, opts ...client.SubResourceCreateOption,
			) error {
				var stored upgradev1alpha1.UpgradePlan
				if err := c.Get(ctx, client.ObjectKeyFromObject(plan), &stored); err != nil {
					return err
				}
				storedAtEviction = &stored.Status.Nodes[0].SelfEvictionRequested
				if refuse {
					return errors.New("cannot evict pod as it would violate the pod's disruption budget")
				}

				return c.SubResource(sub).Create(ctx, obj, subObj, opts...)
			}}).Build()
		talos := &fakeTalos{}
		r := &UpgradePlanReconciler{Client: c, talos: talos}

		var current upgradev1alpha1.UpgradePlan
		if err := c.Get(ctx, client.ObjectKeyFromObject(plan), &current); err != nil {
			t.Fatal(err)
		}

		if _, err := r.startNodeUpgrade(ctx, &current, &current.Status.Nodes[0]); err != nil {
			t.Fatalf("refuse=%v: startNodeUpgrade: %v", refuse, err)
		}

		switch {
		case storedAtEviction == nil:
			t.Fatalf("refuse=%v: no eviction was attempted", refuse)
		case !*storedAtEviction:
			t.Errorf("refuse=%v: SelfEvictionRequested was not stored before the eviction", refuse)
		case len(talos.upgrades) != 0:
			t.Errorf("refuse=%v: upgrade sent from the node being upgraded", refuse)
		}

		if refuse && (current.Status.Nodes[0].SelfEvictionRequested || current.Status.Reason != reasonWaitingForEviction) {
			t.Errorf("refused eviction: marker=%v reason=%s, want the marker cleared and %s",
				current.Status.Nodes[0].SelfEvictionRequested, current.Status.Reason, reasonWaitingForEviction)
		}
	}
}

func TestKubernetesResumeFinishesTheRollout(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		controlPlane string
		wantRollout  bool
	}{
		{k8s1359, true},
		{"v1.35.10", false},
		{"v1.34.2", true},
	} {
		plan, c := kubernetesRolloutPlan(t)
		talos := &fakeTalos{controlPlane: tc.controlPlane}
		r := &UpgradePlanReconciler{Client: c, talos: talos}

		if _, err := r.driveKubernetesUpgrade(ctx, plan); err != nil {
			t.Fatalf("control plane %s: %v", tc.controlPlane, err)
		}

		if got := len(talos.rollouts) == 1; got != tc.wantRollout {
			t.Errorf("control plane %s: rollouts %v, want a rollout: %v", tc.controlPlane, talos.rollouts, tc.wantRollout)
		}

		if got := plan.Status.Kubernetes.Phase; got != upgradev1alpha1.NodePhaseHealthChecking {
			t.Errorf("control plane %s: phase = %s, want HealthChecking", tc.controlPlane, got)
		}
	}
}

func TestInterruptedUpgradeCallKeepsUpgrading(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	plan := planWithNodeIn(upgradev1alpha1.NodePhasePending)
	c := fake.NewClientBuilder().WithScheme(upgradeScheme(t)).WithObjects(plan).
		WithStatusSubresource(&upgradev1alpha1.UpgradePlan{}).Build()
	talos := &fakeTalos{upgradeErr: status.Error(codes.Canceled, context.Canceled.Error()), duringUpgrade: cancel}
	r := &UpgradePlanReconciler{Client: c, talos: talos}

	var current upgradev1alpha1.UpgradePlan
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(plan), &current); err != nil {
		t.Fatal(err)
	}

	if _, err := r.startNodeUpgrade(ctx, &current, &current.Status.Nodes[0]); err == nil {
		t.Error("startNodeUpgrade returned no error for an interrupted call")
	}

	var stored upgradev1alpha1.UpgradePlan
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(plan), &stored); err != nil {
		t.Fatal(err)
	}

	if got := stored.Status.Nodes[0].Phase; got != upgradev1alpha1.NodePhaseUpgrading {
		t.Errorf("stored phase = %s (%s), want Upgrading: the call may have reached the node", got, stored.Status.Nodes[0].Message)
	}
}

func heldWorker(hold string) *corev1.Node {
	n := readyNode(testWorkerName)
	n.Annotations = map[string]string{upgradev1alpha1.HoldAnnotation: hold}
	n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.10"}}
	n.Status.NodeInfo.KubeletVersion = "v1.34.2"

	return n
}

func reconcileAndGet(t *testing.T, r *UpgradePlanReconciler) *upgradev1alpha1.UpgradePlan {
	t.Helper()

	if _, err := r.Reconcile(context.Background(), clusterRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var stored upgradev1alpha1.UpgradePlan
	if err := r.Get(context.Background(), clusterRequest().NamespacedName, &stored); err != nil {
		t.Fatal(err)
	}

	return &stored
}

func TestHoldAnnotationStopsAndReleasesTheTalosUpgrade(t *testing.T) {
	ctx := context.Background()

	plan := &upgradev1alpha1.UpgradePlan{
		ObjectMeta: metav1.ObjectMeta{Name: upgradev1alpha1.UpgradePlanName},
		Spec:       upgradev1alpha1.UpgradePlanSpec{TalosVersion: talos1142, InstallerImageBase: "example.com/installer"},
	}
	node := heldWorker("true")
	c := fake.NewClientBuilder().WithScheme(upgradeScheme(t)).WithObjects(plan, node).
		WithStatusSubresource(&upgradev1alpha1.UpgradePlan{}).Build()
	talos := &fakeTalos{version: "v1.14.0", healthy: true}
	r := &UpgradePlanReconciler{Client: c, talos: talos}

	stored := reconcileAndGet(t, r)
	if stored.Status.Phase != upgradev1alpha1.PlanPhaseWaiting || stored.Status.Reason != reasonNodeHeld {
		t.Fatalf("plan = %s/%s (%s), want Waiting/%s", stored.Status.Phase, stored.Status.Reason, stored.Status.Message, reasonNodeHeld)
	}

	if !stored.Status.Nodes[0].Held || len(talos.upgrades) != 0 {
		t.Fatalf("held = %t, upgrade calls = %d, want true and 0", stored.Status.Nodes[0].Held, len(talos.upgrades))
	}

	node.Annotations[upgradev1alpha1.HoldAnnotation] = "false"
	if err := c.Update(ctx, node); err != nil {
		t.Fatal(err)
	}

	stored = reconcileAndGet(t, r)
	if len(talos.upgrades) != 1 || stored.Status.Nodes[0].Phase != upgradev1alpha1.NodePhaseUpgrading {
		t.Errorf("upgrade calls = %d, node %s (%s), want 1 and Upgrading once released",
			len(talos.upgrades), stored.Status.Nodes[0].Phase, stored.Status.Nodes[0].Message)
	}

	if stored.Status.Nodes[0].Held {
		t.Error("held = true after the annotation was set to false")
	}
}

func TestHoldAnnotationStopsTheKubernetesUpgrade(t *testing.T) {
	plan := &upgradev1alpha1.UpgradePlan{
		ObjectMeta: metav1.ObjectMeta{Name: upgradev1alpha1.UpgradePlanName},
		Spec:       upgradev1alpha1.UpgradePlanSpec{KubernetesVersion: k8s1359},
	}
	c := fake.NewClientBuilder().WithScheme(upgradeScheme(t)).WithObjects(plan, heldWorker("true")).
		WithStatusSubresource(&upgradev1alpha1.UpgradePlan{}).Build()
	talos := &fakeTalos{version: talos1142, controlPlane: "v1.34.2", healthy: true}
	r := &UpgradePlanReconciler{Client: c, talos: talos}

	stored := reconcileAndGet(t, r)
	if stored.Status.Reason != reasonNodeHeld || len(talos.rollouts) != 0 {
		t.Errorf("plan = %s/%s (%s), rollouts = %d, want Waiting/%s and none",
			stored.Status.Phase, stored.Status.Reason, stored.Status.Message, len(talos.rollouts), reasonNodeHeld)
	}
}
