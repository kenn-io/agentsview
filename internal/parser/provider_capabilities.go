package parser

func withWatchRootPlanningCapability(caps Capabilities) Capabilities {
	caps.Source.WatchRoots = CapabilitySupported
	return caps
}

// withPollMetadataScan declares that the provider's WatchPlan names every file
// whose change fallback polling acts on. See SourceCapabilities.PollMetadataScan.
func withPollMetadataScan(caps Capabilities) Capabilities {
	caps.Source.PollMetadataScan = CapabilitySupported
	return caps
}

func jsonlFileProviderSourceCapabilities() SourceCapabilities {
	return SourceCapabilities{
		DiscoverSources:      CapabilitySupported,
		WatchSources:         CapabilitySupported,
		ClassifyChangedPath:  CapabilitySupported,
		FindSource:           CapabilitySupported,
		CompositeFingerprint: CapabilitySupported,
		MultiSessionSource:   CapabilityNotApplicable,
		PerSessionErrors:     CapabilityNotApplicable,
		ExcludedSessions:     CapabilityNotApplicable,
		ForceReplaceOnParse:  CapabilityNotApplicable,
	}
}
