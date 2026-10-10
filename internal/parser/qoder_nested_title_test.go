package parser

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQoderTitleUsesMostSpecificRootOwner(t *testing.T) {
	for _, localChild := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmtNestedTitleCase(localChild, reverse), func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				t.Setenv("USERPROFILE", home)
				parent := filepath.Join(home, ".qoder", "projects")
				child := filepath.Join(parent, "nested")
				writeQoderTitleTestSession(t, child, qoderTitleTestID)
				title := "local application title"
				writeQoderTitleDB(t, filepath.Join(home, "Library", "Application Support", qoderIntlClientApp, "main.sqlite"), map[string]*string{qoderTitleTestID: &title})
				owners := map[string]string{parent: "local", child: "foreign"}
				if localChild {
					owners[parent], owners[child] = "foreign", "local"
				}
				roots := []string{parent, child}
				if reverse {
					roots = []string{child, parent}
				}
				provider, ok := NewProvider(AgentQoder, ProviderConfig{Roots: roots, Machine: "local", SourceMachines: owners})
				require.True(t, ok)
				result := qoderTitleTestParse(t, provider, child)[0]
				assert.Equal(t, localChild, result.Result.Session.SessionNamePresent)
				if localChild {
					assert.Equal(t, title, result.Result.Session.SessionName)
				} else {
					assert.Empty(t, result.Result.Session.SessionName)
				}
				assert.Equal(t, DataVersionCurrent, result.DataVersion)
			})
		}
	}
}

func fmtNestedTitleCase(localChild, reverse bool) string {
	name := "foreign child"
	if localChild {
		name = "local child"
	}
	if reverse {
		name += " reversed roots"
	}
	return name
}
