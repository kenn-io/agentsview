package git

// Git for Windows reports zero for st_dev during repository discovery.
func repoRootDevice(_ string) (uint64, error) {
	return 0, nil
}
