package parser

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type traeLayoutState uint8

const (
	traeLayoutSupported traeLayoutState = iota
	traeLayoutValidEmpty
	traeLayoutIncomplete
	traeLayoutUnsupported
)

func classifyTraeLayout(root string, snapshot traeSessionSnapshot) traeLayoutState {
	if len(snapshot.records) > 0 {
		return traeLayoutSupported
	}
	if snapshot.malformed {
		return traeLayoutIncomplete
	}
	if snapshot.authoritative && snapshot.complete {
		return traeLayoutValidEmpty
	}
	if traeEncryptedModularData(root) {
		return traeLayoutUnsupported
	}
	return traeLayoutIncomplete
}

// TraeEncryptedLayoutDetected reports the measured modern Trae layout for a
// configured User root. Files outside the expected profile shape stay quiet.
func TraeEncryptedLayoutDetected(root string) bool {
	dbs := traeDBs(root)
	if len(dbs) == 0 {
		return false
	}
	foundUnsupported := false
	for _, db := range dbs {
		snapshot, err := traeLoadSessionSnapshot(db.path)
		if err != nil {
			continue
		}
		switch classifyTraeLayout(root, snapshot) {
		case traeLayoutSupported:
			return false
		case traeLayoutUnsupported:
			foundUnsupported = true
		}
	}
	return foundUnsupported
}

func traeEncryptedModularData(root string) bool {
	profile := filepath.Clean(root)
	if filepath.Base(profile) != "User" {
		return false
	}
	product := strings.ToLower(filepath.Base(filepath.Dir(profile)))
	if product != "trae" && product != "trae cn" && product != "trae solo cn" {
		return false
	}
	path := filepath.Join(filepath.Dir(profile), "ModularData", "ai-agent", "database.db")
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	header := make([]byte, len(sqliteHeaderMagic))
	if _, err := io.ReadFull(file, header); err != nil {
		return false
	}
	return !bytes.Equal(header, sqliteHeaderMagic)
}
