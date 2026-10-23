package vector

import (
	"iter"

	"go.kenn.io/agentsview/internal/db"
)

// UnitDocument is a store-independent embedding document, including its stable
// identity and content revision. Deleted units retain their identity for removal.
type UnitDocument struct {
	DocKey      string
	SessionID   string
	SourceUUID  string
	Ordinal     int
	OrdinalEnd  int
	Subordinate bool
	Offsets     []db.UnitOffset
	Content     string
	ContentHash string
	Deleted     bool
}

// UnitDocuments maps a complete ordered unit stream to documents without
// buffering it. Occurrences are counted per (session, source UUID), across
// unit kinds, just as DocKey requires. Each iteration starts fresh counters.
func UnitDocuments(units iter.Seq[db.EmbeddableUnit]) iter.Seq[UnitDocument] {
	return func(yield func(UnitDocument) bool) {
		occurrences := make(map[[2]string]int)
		for u := range units {
			occurrence := 1
			if u.SourceUUID != "" {
				key := [2]string{u.SessionID, u.SourceUUID}
				occurrences[key]++
				occurrence = occurrences[key]
			}
			if !yield(UnitDocument{
				DocKey:    DocKey(u.Kind, u.SessionID, u.SourceUUID, u.Ordinal, occurrence),
				SessionID: u.SessionID, SourceUUID: u.SourceUUID,
				Ordinal: u.Ordinal, OrdinalEnd: u.OrdinalEnd, Subordinate: u.Subordinate,
				Offsets: u.Offsets, Content: u.Content, ContentHash: contentHash(u.Content), Deleted: u.Deleted,
			}) {
				return
			}
		}
	}
}
