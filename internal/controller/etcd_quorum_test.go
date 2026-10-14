package controller

import (
	"testing"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
)

func countMembers(resp *machineapi.EtcdMemberListResponse) (voters, learners int) {
	for _, message := range resp.GetMessages() {
		for _, member := range message.GetMembers() {
			if member.GetIsLearner() {
				learners++

				continue
			}

			voters++
		}
	}

	return voters, learners
}

func members(specs ...bool) *machineapi.EtcdMemberListResponse {
	list := make([]*machineapi.EtcdMember, 0, len(specs))
	for _, isLearner := range specs {
		list = append(list, &machineapi.EtcdMember{IsLearner: isLearner})
	}

	return &machineapi.EtcdMemberListResponse{
		Messages: []*machineapi.EtcdMembers{{Members: list}},
	}
}

func TestEtcdMemberTally(t *testing.T) {
	for _, tc := range []struct {
		name         string
		resp         *machineapi.EtcdMemberListResponse
		wantVoters   int
		wantLearners int
		wantQuorum   bool
	}{
		{"healthy three", members(false, false, false), 3, 0, true},
		{
			name: "one voter one learner", resp: members(false, true),
			wantVoters: 1, wantLearners: 1, wantQuorum: false,
		},
		{"two voters", members(false, false), 2, 0, false},
		{"single node", members(false), 1, 0, false},
		{"five voters", members(false, false, false, false, false), 5, 0, true},
		{"three voters plus learner", members(false, false, false, true), 3, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			voters, learners := countMembers(tc.resp)
			if voters != tc.wantVoters || learners != tc.wantLearners {
				t.Errorf("tally = %d voters/%d learners, want %d/%d",
					voters, learners, tc.wantVoters, tc.wantLearners)
			}

			if got := voters >= 3; got != tc.wantQuorum {
				t.Errorf("quorum-safe = %v, want %v (voters=%d)", got, tc.wantQuorum, voters)
			}
		})
	}
}
