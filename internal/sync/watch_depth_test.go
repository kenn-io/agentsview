package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMergeWatchDepth(t *testing.T) {
	tests := []struct {
		name       string
		recursiveA bool
		depthA     int
		recursiveB bool
		depthB     int
		want       int
	}{
		{"both limited keeps deeper", true, 1, true, 3, 3},
		{"unlimited recursive wins", true, 1, true, 0, 0},
		{"shallow plan does not limit", true, 2, false, 0, 2},
		{"shallow plan depth ignored", false, 5, true, 1, 1},
		{"neither recursive", false, 1, false, 2, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, MergeWatchDepth(
				tt.recursiveA, tt.depthA, tt.recursiveB, tt.depthB,
			))
		})
	}
}

func TestMergeExtraDirectories(t *testing.T) {
	assert.Nil(t, MergeExtraDirectories(nil, nil))
	assert.Equal(t, []string{"*/logs"}, MergeExtraDirectories([]string{"*/logs"}, nil))
	assert.Equal(t, []string{"*/logs", "*/other"}, MergeExtraDirectories(
		[]string{"*/logs"}, []string{"*/other", "*/logs", ""},
	))
}
