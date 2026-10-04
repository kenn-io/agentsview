package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestFrictionReadsAreReadOnly(t *testing.T) {
	s := &Store{}
	calls := map[string]func() error{
		"findings": func() error {
			_, _, err := s.ListFrictionFindings(t.Context(), db.FrictionFindingFilter{})
			return err
		},
		"digests": func() error {
			_, err := s.ListFrictionDigests(t.Context(), "", "")
			return err
		},
		"patterns": func() error {
			_, _, err := s.ListFrictionPatterns(t.Context(), db.FrictionPatternFilter{})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, call(), db.ErrReadOnly)
		})
	}
}
