package kept

// Part retains one source's contribution and version.
type Part[T any] struct {
	ID, Fingerprint string
	Group           string
	Present         bool
	Value           T
}

func LoadParts[T any](disk *Disk, slot, version string) map[string]Part[T] {
	parts := map[string]Part[T]{}
	if data, ok := disk.Get(slot, version); ok {
		if rows, ok := DecodeRows[Part[T]](data); ok {
			for _, row := range rows {
				parts[row.ID] = row
			}
		}
	}
	return parts
}

func StaleParts[T any](parts map[string]Part[T], versions map[string]string) map[string]bool {
	changed := map[string]bool{}
	for id, version := range versions {
		part, ok := parts[id]
		if !ok || version == "" || version != part.Fingerprint {
			changed[id] = true
		}
	}
	for id := range parts {
		if _, ok := versions[id]; !ok {
			changed[id] = true
		}
	}
	return changed
}

func SaveParts[T any](disk *Disk, slot, version string, rows []Part[T], size func(T) int64) {
	if data, err := EncodeRows(rows, 128<<20, func(part Part[T]) int64 {
		return int64(len(part.ID)+len(part.Fingerprint)+len(part.Group)) + size(part.Value)
	}); err == nil {
		disk.Put(slot, version, data)
	}
}
