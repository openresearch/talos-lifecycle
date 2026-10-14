package main

import "testing"

func TestParseControllers(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{in: "upgrade", want: []string{"upgrade"}},
		{in: "config", want: []string{"config"}},
		{in: "upgrade,config", want: []string{"upgrade", "config"}},
		{in: " config , upgrade ", want: []string{"upgrade", "config"}},
		{in: "", wantErr: true},
		{in: "nonsense", wantErr: true},
		{in: "upgrade,typo", wantErr: true},
	} {
		got, err := parseControllers(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: expected an error", tc.in)
			}

			continue
		}

		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}

		if len(got) != len(tc.want) {
			t.Errorf("%q: got %d controllers, want %d", tc.in, len(got), len(tc.want))
		}

		for _, name := range tc.want {
			if !got[name] {
				t.Errorf("%q: %s not enabled", tc.in, name)
			}
		}
	}
}

func TestLeaderElectionIDs(t *testing.T) {
	const upgradeLease = "15e33b50.openresearch.com"

	up := leaderElectionID(map[string]bool{"upgrade": true})
	cfg := leaderElectionID(map[string]bool{"config": true})
	both := leaderElectionID(map[string]bool{"upgrade": true, "config": true})

	if up != upgradeLease || both != upgradeLease {
		t.Errorf("anything running the upgrade loop must keep %q, got upgrade=%q both=%q", upgradeLease, up, both)
	}

	if cfg == upgradeLease {
		t.Error("the config loop must not share the upgrade lease")
	}
}
