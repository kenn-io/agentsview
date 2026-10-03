package db_test

import (
	"testing"

	"go.kenn.io/agentsview/internal/dbtest"
)

func TestNativePalette(t *testing.T) {
	archive := dbtest.OpenTestDB(t)
	dbtest.SeedNativePalette(t, archive)
	dbtest.CheckNativePalette(t, archive)
}
