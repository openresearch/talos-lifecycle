package versions

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
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

func TestCapUnfiltered(t *testing.T) {
	versions := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		versions = append(versions, fmt.Sprintf("v1.%d.0", i))
	}

	capped := capUnfiltered(versions, "")
	if len(capped) != maxUnfilteredVersions {
		t.Fatalf("capUnfiltered length = %d, want %d", len(capped), maxUnfilteredVersions)
	}

	if capped[len(capped)-1] != "v1.29.0" {
		t.Errorf("newest capped version = %s, want v1.29.0", capped[len(capped)-1])
	}

	if got := capUnfiltered(versions, "v1.0.0"); len(got) != len(versions) {
		t.Errorf("capUnfiltered with baseline trimmed the list to %d entries", len(got))
	}
}
