package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/export"
)

func identityObs(
	root, remote, remoteName string,
) export.ProjectIdentityObservation {
	return export.ProjectIdentityObservation{
		Project:       "proj",
		Machine:       "m1",
		RootPath:      root,
		GitRemote:     remote,
		GitRemoteName: remoteName,
		ObservedAt:    time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestPlanProjectIdentityObservationSync(t *testing.T) {
	tests := []struct {
		name          string
		observations  []export.ProjectIdentityObservation
		wantReal      []string // GitRemoteName markers, in order
		wantAmbiguous []string // RootPath markers, in order
		wantFallbacks []string // RootPath markers, in order
		wantRoots     []string // RootPath markers, in order
	}{
		{
			name: "real shadows fallback regardless of order",
			observations: []export.ProjectIdentityObservation{
				identityObs("/a", "", ""),
				identityObs("/a", "git@x:a.git", "origin"),
				identityObs("/b", "git@x:b.git", "origin"),
				identityObs("/b", "", ""),
			},
			wantReal:  []string{"origin", "origin"},
			wantRoots: []string{"/a", "/b"},
		},
		{
			name: "ambiguous survives alongside real remote",
			observations: func() []export.ProjectIdentityObservation {
				ambiguous := identityObs("/a", "", "")
				ambiguous.RemoteResolution = export.ProjectResolutionAmbiguous
				ambiguous.RemoteCandidateCount = 2
				return []export.ProjectIdentityObservation{
					identityObs("/a", "git@x:a.git", "origin"),
					ambiguous,
				}
			}(),
			wantReal:      []string{"origin"},
			wantAmbiguous: []string{"/a"},
			wantRoots:     []string{"/a"},
		},
		{
			name: "fallback without real survives to the pg check",
			observations: []export.ProjectIdentityObservation{
				identityObs("/a", "", ""),
				identityObs("/b", "git@x:b.git", "origin"),
			},
			wantReal:      []string{"origin"},
			wantFallbacks: []string{"/a"},
			wantRoots:     []string{"/b"},
		},
		{
			name: "last observation per conflict key wins",
			observations: []export.ProjectIdentityObservation{
				identityObs("/a", "git@x:a.git", "old"),
				identityObs("/a", "git@x:a.git", "new"),
				identityObs("/a", "git@y:a.git", "other"),
			},
			wantReal:  []string{"new", "other"},
			wantRoots: []string{"/a"},
		},
		{
			name: "empty input plans nothing",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := planProjectIdentityObservationSync(tt.observations)

			var gotReal []string
			for _, obs := range plan.realRemote {
				gotReal = append(gotReal, obs.GitRemoteName)
			}
			assert.Equal(t, tt.wantReal, gotReal, "real remote observations")

			var gotAmbiguous []string
			for _, obs := range plan.ambiguous {
				gotAmbiguous = append(gotAmbiguous, obs.RootPath)
			}
			assert.Equal(t, tt.wantAmbiguous, gotAmbiguous, "ambiguous observations")

			var gotFallbacks []string
			for _, obs := range plan.fallbacks {
				gotFallbacks = append(gotFallbacks, obs.RootPath)
			}
			assert.Equal(t, tt.wantFallbacks, gotFallbacks, "fallbacks")

			var gotRoots []string
			for _, root := range plan.realRoots {
				gotRoots = append(gotRoots, root.rootPath)
			}
			assert.Equal(t, tt.wantRoots, gotRoots, "real roots")
		})
	}
}
