package versions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
)

func TestFetchStableVersions(t *testing.T) {
	releases := []release{
		{TagName: "v1.10.0"},
		{TagName: "v1.10.1-beta.0", Prerelease: true},
		{TagName: "v1.9.5"},
		{TagName: "v1.11.0", Draft: true},
		{TagName: "not-a-version"},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewEncoder(w).Encode(releases); err != nil {
			t.Errorf("encode releases: %v", err)
		}
	}))
	defer server.Close()

	versions, err := fetchStableVersions(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("fetchStableVersions: %v", err)
	}

	want := []string{"v1.9.5", "v1.10.0"}
	if len(versions) != len(want) {
		t.Fatalf("versions = %v, want %v", versions, want)
	}

	for i := range want {
		if versions[i] != want[i] {
			t.Errorf("versions[%d] = %s, want %s", i, versions[i], want[i])
		}
	}
}

func TestFetchFollowsNextPages(t *testing.T) {
	pages := map[string][]release{
		"":  {{TagName: "v1.33.1"}, {TagName: "v1.33.0"}},
		"2": {{TagName: "v1.32.4"}},
		"3": {{TagName: "v1.31.9"}},
	}

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if next := map[string]string{"": "2", "2": "3"}[page]; next != "" {
			w.Header().Set("Link", fmt.Sprintf(`<%s/releases?page=%s>; rel="next", <%s/releases?page=3>; rel="last"`,
				server.URL, next, server.URL))
		}

		if err := json.NewEncoder(w).Encode(pages[page]); err != nil {
			t.Errorf("encode releases: %v", err)
		}
	}))
	defer server.Close()

	versions, err := fetchStableVersions(context.Background(), server.URL+"/releases")
	if err != nil {
		t.Fatalf("fetchStableVersions: %v", err)
	}

	want := fmt.Sprint([]string{"v1.31.9", "v1.32.4", "v1.33.0", "v1.33.1"})
	if got := fmt.Sprint(versions); got != want {
		t.Errorf("versions = %s, want %s", got, want)
	}
}

func TestNewerThan(t *testing.T) {
	versions := []string{"v1.9.5", "v1.10.0", "v1.10.2"}

	if got := newerThan(versions, "v1.10.0"); len(got) != 1 || got[0] != "v1.10.2" {
		t.Errorf("newerThan(v1.10.0) = %v, want [v1.10.2]", got)
	}

	if got := newerThan(versions, ""); len(got) != len(versions) {
		t.Errorf("newerThan(\"\") = %v, want the full list", got)
	}
}

func TestPublishWaitsForTheClusterVersions(t *testing.T) {
	ctx := context.Background()

	scheme := runtime.NewScheme()
	if err := upgradev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	plan := &upgradev1alpha1.UpgradePlan{ObjectMeta: metav1.ObjectMeta{Name: upgradev1alpha1.UpgradePlanName}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(plan).WithStatusSubresource(plan).Build()
	p := &Poller{Client: c}

	talos := []string{"v1.12.3", "v1.12.12", "v1.13.0", "v1.13.11", "v1.14.0", "v1.14.1", "v1.14.2"}
	kubernetes := []string{"v1.31.0", "v1.31.14", "v1.32.0"}

	if err := p.publish(ctx, talos, true, kubernetes, true); !errors.Is(err, errBaselineUnknown) {
		t.Fatalf("publish before any node version = %v, want errBaselineUnknown", err)
	}

	if err := c.Get(ctx, client.ObjectKeyFromObject(plan), plan); err != nil {
		t.Fatal(err)
	}

	if len(plan.Status.AvailableTalosVersions) != 0 {
		t.Fatalf("published %v without a baseline", plan.Status.AvailableTalosVersions)
	}

	plan.Status.Nodes = []upgradev1alpha1.NodeStatus{{Name: "cp-0", CurrentVersion: "v1.12.3"}}
	plan.Status.Kubernetes.CurrentVersion = "v1.31.0"

	if err := c.Status().Update(ctx, plan); err != nil {
		t.Fatal(err)
	}

	if err := p.publish(ctx, talos, true, kubernetes, true); err != nil {
		t.Fatal(err)
	}

	if err := c.Get(ctx, client.ObjectKeyFromObject(plan), plan); err != nil {
		t.Fatal(err)
	}

	if got := fmt.Sprint(plan.Status.AvailableTalosVersions); got != fmt.Sprint(talos[1:]) {
		t.Errorf("talos = %s, want %v", got, talos[1:])
	}

	if got := fmt.Sprint(plan.Status.AvailableKubernetesVersions); got != fmt.Sprint(kubernetes[1:]) {
		t.Errorf("kubernetes = %s, want %v", got, kubernetes[1:])
	}
}
