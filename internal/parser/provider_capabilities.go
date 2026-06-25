package parser

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
