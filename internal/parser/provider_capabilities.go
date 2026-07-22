package parser

func withWatchRootPlanningCapability(caps Capabilities) Capabilities {
	caps.Source.WatchRoots = CapabilitySupported
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
