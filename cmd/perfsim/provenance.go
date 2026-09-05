package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"runtime/debug"
)

type buildProvenance struct {
	Revision         string `json:"revision,omitempty"`
	Modified         *bool  `json:"modified,omitempty"`
	ExecutableSHA256 string `json:"executable_sha256,omitempty"`
}

func provenance() buildProvenance {
	var p buildProvenance
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				p.Revision = setting.Value
			case "vcs.modified":
				p.Modified = new(setting.Value == "true")
			}
		}
	}
	if path, err := os.Executable(); err == nil {
		if f, err := os.Open(path); err == nil {
			defer f.Close()
			h := sha256.New()
			if _, err := io.Copy(h, f); err == nil {
				p.ExecutableSHA256 = fmt.Sprintf("%x", h.Sum(nil))
			}
		}
	}
	return p
}
