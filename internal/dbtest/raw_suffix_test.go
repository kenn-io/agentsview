package dbtest

import "testing"

func TestRawSuffixSQLite(t *testing.T) {
	d := OpenTestDB(t)
	SeedRawSuffix(t, d)
	TrashRawSuffix(t, d)
	AssertRawSuffix(t, d)
}
