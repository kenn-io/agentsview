package main

import "github.com/spf13/cobra"

func bindExportProfile(command *cobra.Command) *SyncConfig {
	cfg := new(SyncConfig)
	flags := command.Flags()
	flags.StringVar(&cfg.CPUProfile, "cpuprofile", "",
		"Write CPU profile to file (developer use)")
	flags.StringVar(&cfg.MemProfile, "memprofile", "",
		"Write memory profile to file (developer use)")
	flags.StringVar(&cfg.Trace, "trace", "",
		"Write runtime trace to file (developer use)")
	for _, name := range []string{"cpuprofile", "memprofile", "trace"} {
		if err := flags.MarkHidden(name); err != nil {
			panic(err)
		}
	}
	return cfg
}
