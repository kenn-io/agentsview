package pricing

import (
	"crypto/sha256"
	"sync"
)

// storedGenAIDocument keeps the last GenAI Prices document a store parsed.
// Every usage read prices with the stored document, which is several hundred
// KB of JSON, and parsing it costs about 5 ms and 3.5 MB of allocations, while
// the document changes only when a pricing refresh or push replaces it. One
// entry is enough: a process reads one stored document at a time.
var storedGenAIDocument struct {
	mu        sync.Mutex
	parsed    bool
	sum       [sha256.Size]byte
	version   string
	sourceRef string
	document  GenAIDocument
}

// CachedGenAIDocument returns the parsed document whose JSON has SHA-256 sum,
// if ParseStoredGenAIDocument parsed it last. A store whose database can hash
// the JSON uses this to skip reading the JSON at all. The sum covers the exact
// bytes, so a document with reordered keys or different whitespace misses.
func CachedGenAIDocument(
	sum [sha256.Size]byte, version, sourceRef string,
) (GenAIDocument, bool) {
	cache := &storedGenAIDocument
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if !cache.parsed || cache.sum != sum ||
		cache.version != version || cache.sourceRef != sourceRef {
		return GenAIDocument{}, false
	}
	return cache.document, true
}

// ParseStoredGenAIDocument is ParseGenAIDocument for a document read from
// storage. It reuses the last parse when data hashes to the same SHA-256 and
// the version and source ref match, and remembers a new parse otherwise. The
// returned document shares its catalog with other callers, which is safe:
// GenAIPrices is read-only apart from its concurrent-safe lookup memos.
func ParseStoredGenAIDocument(
	data []byte, version, sourceRef string,
) (GenAIDocument, error) {
	sum := sha256.Sum256(data)
	if document, ok := CachedGenAIDocument(sum, version, sourceRef); ok {
		return document, nil
	}
	// Parse outside the lock: a concurrent miss parses twice at worst, and
	// no caller waits behind another's parse.
	document, err := ParseGenAIDocument(data, version, sourceRef)
	if err != nil {
		return GenAIDocument{}, err
	}
	cache := &storedGenAIDocument
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.parsed = true
	cache.sum, cache.version, cache.sourceRef = sum, version, sourceRef
	cache.document = document
	return document, nil
}
