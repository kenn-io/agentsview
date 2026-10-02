// ABOUTME: tests for the import CLI summary: refused conversations
// ABOUTME: are grouped by reason next to the error count.
package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/importer"
)

func TestFormatImportSummaryRefusalReasons(t *testing.T) {
	stats := importer.ImportStats{
		Imported: 1,
		Errors:   3,
		Refusals: []importer.ImportRefusal{
			{SessionID: "chatgpt:a", Reason: importer.RefusalDiverged},
			{SessionID: "chatgpt:b", Reason: importer.RefusalTransient},
			{SessionID: "chatgpt:c", Reason: importer.RefusalDiverged},
		},
	}
	assert.Equal(t,
		"\rDone: 1 processed (1 new)\n  3 errors (2 diverged, 1 transient)\n",
		formatImportSummary(stats),
	)
	assert.Equal(t,
		"\rDone: 0 processed\n  2 errors\n",
		formatImportSummary(importer.ImportStats{Errors: 2}),
	)
}
