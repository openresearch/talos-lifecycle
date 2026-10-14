// Package versions writes the available Talos and Kubernetes versions into the
// UpgradePlan status. It never writes the spec.
package versions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/mod/semver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	upgradev1alpha1 "github.com/openresearch/talos-lifecycle/api/v1alpha1"
)

const (
	defaultTalosReleasesURL      = "https://api.github.com/repos/siderolabs/talos/releases"
	defaultKubernetesReleasesURL = "https://api.github.com/repos/kubernetes/kubernetes/releases"

	pollInterval          = 24 * time.Hour
	retryInterval         = 15 * time.Minute
	baselineRetryInterval = time.Minute
	fetchTimeout          = time.Minute
	maxFeedBytes          = 16 << 20
	maxFeedPages          = 10
)

type release struct {
	TagName    string `json:"tag_name"`
	Prerelease bool   `json:"prerelease"`
	Draft      bool   `json:"draft"`
}

type Poller struct {
	Client client.Client

	talosURL      string
	kubernetesURL string
}

var _ manager.LeaderElectionRunnable = (*Poller)(nil)

func (p *Poller) NeedLeaderElection() bool { return true }

func (p *Poller) Start(ctx context.Context) error {
	registerMetrics()

	log := log.FromContext(ctx).WithName("version-poller")

	p.talosURL = defaultTalosReleasesURL
	if url := os.Getenv("TALOS_RELEASE_URL"); url != "" {
		p.talosURL = url
	}

	p.kubernetesURL = defaultKubernetesReleasesURL
	if url := os.Getenv("KUBERNETES_RELEASE_URL"); url != "" {
		p.kubernetesURL = url
	}

	log.Info("starting", "talosURL", p.talosURL, "kubernetesURL", p.kubernetesURL)

	for {
		interval := pollInterval

		switch err := p.poll(ctx); {
		case errors.Is(err, errBaselineUnknown):
			log.Info("waiting for the cluster's versions before publishing")

			interval = baselineRetryInterval
		case err != nil:
			log.Error(err, "poll failed")
			metricPollErrors.Inc()

			interval = retryInterval
		default:
			metricPollLastSuccess.SetToCurrentTime()
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

func (p *Poller) poll(ctx context.Context) error {
	talosVersions, talosErr := fetchStableVersions(ctx, p.talosURL)
	kubernetesVersions, kubernetesErr := fetchStableVersions(ctx, p.kubernetesURL)

	if talosErr != nil && kubernetesErr != nil {
		return fmt.Errorf("fetch talos releases: %w; fetch kubernetes releases: %w", talosErr, kubernetesErr)
	}

	if err := p.publish(ctx, talosVersions, talosErr == nil, kubernetesVersions, kubernetesErr == nil); err != nil {
		return err
	}

	if talosErr != nil {
		return fmt.Errorf("fetch talos releases: %w", talosErr)
	}

	if kubernetesErr != nil {
		return fmt.Errorf("fetch kubernetes releases: %w", kubernetesErr)
	}

	return nil
}

// errBaselineUnknown: a list not filtered against the cluster's version would
// offer the wrong next step, so nothing is published until it is known.
var errBaselineUnknown = errors.New("cluster versions not observed yet")

func (p *Poller) publish(
	ctx context.Context,
	talosVersions []string, talosOK bool,
	kubernetesVersions []string, kubernetesOK bool,
) error {
	log := log.FromContext(ctx).WithName("version-poller")

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var plan upgradev1alpha1.UpgradePlan

		err := p.Client.Get(ctx, types.NamespacedName{Name: upgradev1alpha1.UpgradePlanName}, &plan)
		if apierrors.IsNotFound(err) {
			plan = upgradev1alpha1.UpgradePlan{
				ObjectMeta: metav1.ObjectMeta{Name: upgradev1alpha1.UpgradePlanName},
			}
			if err := p.Client.Create(ctx, &plan); err != nil {
				return fmt.Errorf("create upgrade plan: %w", err)
			}

			log.Info("created empty UpgradePlan singleton")
		} else if err != nil {
			return fmt.Errorf("get upgrade plan: %w", err)
		}

		talos, kubernetes := plan.Status.LowestTalosVersion(), plan.Status.Kubernetes.CurrentVersion
		if talos == "" || kubernetes == "" {
			return errBaselineUnknown
		}

		if talosOK {
			plan.Status.AvailableTalosVersions = newerThan(talosVersions, talos)
		}

		if kubernetesOK {
			plan.Status.AvailableKubernetesVersions = newerThan(kubernetesVersions, kubernetes)
		}

		return p.Client.Status().Update(ctx, &plan)
	})
}

// fetchStableVersions follows rel="next": a GitHub page holds only 30 releases.
func fetchStableVersions(ctx context.Context, url string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	var versions []string

	for page := 0; url != "" && page < maxFeedPages; page++ {
		releases, next, err := fetchPage(ctx, url)
		if err != nil {
			return nil, err
		}

		for _, r := range releases {
			if r.Prerelease || r.Draft || !semver.IsValid(r.TagName) {
				continue
			}

			versions = append(versions, r.TagName)
		}

		url = next
	}

	semver.Sort(versions)

	return versions, nil
}

func fetchPage(ctx context.Context, url string) ([]release, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("request %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("request %s: unexpected status %s", url, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes))
	if err != nil {
		return nil, "", fmt.Errorf("read response: %w", err)
	}

	var releases []release
	if err := json.Unmarshal(body, &releases); err != nil {
		return nil, "", fmt.Errorf("unmarshal releases: %w", err)
	}

	return releases, nextLink(resp.Header.Get("Link")), nil
}

func nextLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		target, params, ok := strings.Cut(part, ";")
		if ok && strings.Contains(params, `rel="next"`) {
			return strings.Trim(strings.TrimSpace(target), "<>")
		}
	}

	return ""
}

func newerThan(versions []string, current string) []string {
	if current == "" {
		return versions
	}

	var newer []string

	for _, v := range versions {
		if semver.Compare(v, current) > 0 {
			newer = append(newer, v)
		}
	}

	return newer
}
