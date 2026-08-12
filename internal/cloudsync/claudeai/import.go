package claudeai

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	cacheManifestName = "index.json"
	syncStateName     = "sync-state.json"
)

// SyncState is the durable, credential-free progress record for a browser
// import. It is deliberately separate from the cache manifest so a stopped
// scan can safely restart from offset zero and skip unchanged details.
type SyncState struct {
	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Scanned   int       `json:"scanned"`
	Changed   int       `json:"changed"`
	Fetched   int       `json:"fetched"`
	Imported  int       `json:"imported"`
	Skipped   int       `json:"skipped"`
	Failed    int       `json:"failed"`
}

// BrowserImportPlan tells the authenticated browser which summary details
// actually need fetching. Summary timestamps are compared only to the local
// content manifest; no credential is accepted or persisted.
type BrowserImportPlan struct {
	ChangedIDs []string  `json:"changed_ids"`
	Unchanged  int       `json:"unchanged"`
	State      SyncState `json:"state"`
}

func syncStatePath(root string) string { return filepath.Join(root, syncStateName) }

func ReadSyncState(root string) (SyncState, error) {
	data, err := os.ReadFile(syncStatePath(root))
	if os.IsNotExist(err) {
		return SyncState{}, nil
	}
	if err != nil {
		return SyncState{}, fmt.Errorf("read Claude sync state: %w", err)
	}
	var state SyncState
	if err := json.Unmarshal(data, &state); err != nil {
		return SyncState{}, fmt.Errorf("decode Claude sync state: %w", err)
	}
	return state, nil
}

func writeSyncState(root string, state SyncState) error {
	state.UpdatedAt = time.Now().UTC()
	return atomicWriteJSON(syncStatePath(root), state)
}

// PlanBrowserImport compares summary markers to the local cache before the
// browser requests details. A fresh scan always begins at zero after restart;
// that is safe because this plan eliminates already-cached unchanged details.
func PlanBrowserImport(cacheRoot string, summaries []json.RawMessage) (BrowserImportPlan, error) {
	return PlanBrowserImportWithForce(cacheRoot, summaries, false)
}

// PlanBrowserImportWithForce requests every supplied conversation detail when
// force is true. It is used for explicit repair imports after users have
// permanently deleted previously imported Claude.ai sessions.
func PlanBrowserImportWithForce(cacheRoot string, summaries []json.RawMessage, force bool) (BrowserImportPlan, error) {
	if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
		return BrowserImportPlan{}, fmt.Errorf("create Claude cache: %w", err)
	}
	manifest, err := readManifest(filepath.Join(cacheRoot, cacheManifestName))
	if err != nil {
		return BrowserImportPlan{}, err
	}
	state, err := ReadSyncState(cacheRoot)
	if err != nil {
		return BrowserImportPlan{}, err
	}
	if state.Status != "running" {
		state = SyncState{Status: "running", StartedAt: time.Now().UTC()}
	}
	// Always serialise an array so desktop clients can safely call includes
	// when a page has no changed conversations.
	plan := BrowserImportPlan{ChangedIDs: []string{}, State: state}
	for _, summary := range summaries {
		id, updatedAt, err := conversationMarker(summary)
		if err != nil {
			state.Failed++
			continue
		}
		state.Scanned++
		if !force && manifest[id] == updatedAt && fileExists(conversationCachePath(cacheRoot, id)) {
			plan.Unchanged++
			state.Skipped++
			continue
		}
		plan.ChangedIDs = append(plan.ChangedIDs, id)
		state.Changed++
	}
	plan.State = state
	if err := writeSyncState(cacheRoot, state); err != nil {
		return BrowserImportPlan{}, err
	}
	return plan, nil
}

// RecordBrowserImportProgress persists completed ingestion progress. The
// caller supplies importer counts after the database transaction succeeds.
func CompleteBrowserImport(cacheRoot string) (SyncState, error) {
	return RecordBrowserImportProgress(cacheRoot, 0, 0, 0, true)
}

func FailBrowserImport(cacheRoot string) (SyncState, error) {
	state, err := ReadSyncState(cacheRoot)
	if err != nil {
		return SyncState{}, err
	}
	state.Status = "failed"
	state.Failed++
	if err := writeSyncState(cacheRoot, state); err != nil {
		return SyncState{}, err
	}
	return state, nil
}

func RecordBrowserImportProgress(cacheRoot string, fetched, imported, failed int, finished bool) (SyncState, error) {
	state, err := ReadSyncState(cacheRoot)
	if err != nil {
		return SyncState{}, err
	}
	state.Fetched += fetched
	state.Imported += imported
	state.Failed += failed
	if finished {
		state.Status = "completed"
	}
	if err := writeSyncState(cacheRoot, state); err != nil {
		return SyncState{}, err
	}
	return state, nil
}

// PreparedImport contains a Claude.ai export-compatible JSON payload. Call
// Commit only after that payload was successfully ingested; otherwise the next
// sync will refetch changed conversations rather than falsely marking them
// current.
type PreparedImport struct {
	ExportJSON   []byte
	Downloaded   int
	Unchanged    int
	manifest     map[string]string
	manifestPath string
}

// BrowserConversation is one authenticated browser fetch. The server receives
// this data only after Tauri fetched it from the isolated Claude webview; it
// never contains browser credentials.
type BrowserConversation struct {
	Summary      json.RawMessage `json:"summary"`
	Conversation json.RawMessage `json:"conversation"`
}

// PrepareBrowserImport updates the content cache from browser-fetched
// conversations and produces the same export-compatible payload as
// Browser imports deliberately have no HTTP or Keychain dependency.
func PrepareBrowserImport(cacheRoot string, conversations []BrowserConversation) (PreparedImport, error) {
	return PrepareBrowserImportWithForce(cacheRoot, conversations, false)
}

// PrepareBrowserImportWithForce saves and imports only the supplied batch.
// The durable cache is never replayed wholesale, avoiding quadratic imports.
func PrepareBrowserImportWithForce(cacheRoot string, conversations []BrowserConversation, force bool) (PreparedImport, error) {
	if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
		return PreparedImport{}, fmt.Errorf("create Claude cache: %w", err)
	}
	manifestPath := filepath.Join(cacheRoot, cacheManifestName)
	manifest, err := readManifest(manifestPath)
	if err != nil {
		return PreparedImport{}, err
	}
	result := PreparedImport{manifest: manifest, manifestPath: manifestPath}
	batch := make([]json.RawMessage, 0, len(conversations))
	for _, item := range conversations {
		id, updatedAt, err := conversationMarker(item.Summary)
		if err != nil {
			return PreparedImport{}, fmt.Errorf("invalid Claude conversation summary: %w", err)
		}
		path := conversationCachePath(cacheRoot, id)
		if !force && manifest[id] == updatedAt && fileExists(path) {
			result.Unchanged++
			continue
		}
		conversation, err := mergeSummary(item.Conversation, item.Summary)
		if err != nil {
			return PreparedImport{}, fmt.Errorf("normalise Claude conversation %s: %w", id, err)
		}
		if err := atomicWriteJSON(path, json.RawMessage(conversation)); err != nil {
			return PreparedImport{}, fmt.Errorf("cache Claude conversation %s: %w", id, err)
		}
		manifest[id] = updatedAt
		result.Downloaded++
		normalized, err := normalizeConversation(conversation)
		if err != nil {
			return PreparedImport{}, fmt.Errorf("normalise Claude import conversation %s: %w", id, err)
		}
		batch = append(batch, normalized)
	}
	payload, err := json.Marshal(batch)
	if err != nil {
		return PreparedImport{}, fmt.Errorf("encode Claude import batch: %w", err)
	}
	result.ExportJSON = payload
	return result, nil
}

// Commit records the remote update timestamps after a successful archive write.
func (p PreparedImport) Commit() error {
	if p.manifestPath == "" {
		return nil
	}
	return atomicWriteJSON(p.manifestPath, p.manifest)
}

func readManifest(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return make(map[string]string), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Claude cache index: %w", err)
	}
	manifest := make(map[string]string)
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("decode Claude cache index: %w", err)
	}
	return manifest, nil
}

func conversationMarker(raw json.RawMessage) (string, string, error) {
	var summary struct {
		UUID      string `json:"uuid"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.Unmarshal(raw, &summary); err != nil {
		return "", "", err
	}
	if !safeConversationID(summary.UUID) {
		return "", "", fmt.Errorf("invalid conversation UUID")
	}
	return summary.UUID, summary.UpdatedAt, nil
}

func mergeSummary(detail, summary json.RawMessage) (json.RawMessage, error) {
	var conversation map[string]json.RawMessage
	if err := json.Unmarshal(detail, &conversation); err != nil {
		return nil, err
	}
	var listed map[string]json.RawMessage
	if err := json.Unmarshal(summary, &listed); err != nil {
		return nil, err
	}
	for _, key := range []string{"uuid", "name", "created_at", "updated_at"} {
		if len(conversation[key]) == 0 || string(conversation[key]) == `""` || string(conversation[key]) == "null" {
			conversation[key] = listed[key]
		}
	}
	return json.Marshal(conversation)
}

// normalizeConversation preserves the fields consumed by the existing Claude
// export parser. Private API payloads include regenerated branches; retain the
// active leaf only so a transcript does not show competing replies twice.
func normalizeConversation(raw json.RawMessage) (json.RawMessage, error) {
	var conversation struct {
		UUID        string            `json:"uuid"`
		Name        string            `json:"name"`
		CreatedAt   string            `json:"created_at"`
		UpdatedAt   string            `json:"updated_at"`
		CurrentLeaf string            `json:"current_leaf_message_uuid"`
		Messages    []json.RawMessage `json:"chat_messages"`
	}
	if err := json.Unmarshal(raw, &conversation); err != nil {
		return nil, err
	}
	if !safeConversationID(conversation.UUID) {
		return nil, fmt.Errorf("missing conversation UUID")
	}
	if conversation.CreatedAt == "" || conversation.UpdatedAt == "" {
		return nil, fmt.Errorf("missing conversation timestamps")
	}
	messages, err := activeMessages(conversation.Messages, conversation.CurrentLeaf)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		UUID      string            `json:"uuid"`
		Name      string            `json:"name"`
		CreatedAt string            `json:"created_at"`
		UpdatedAt string            `json:"updated_at"`
		Messages  []json.RawMessage `json:"chat_messages"`
	}{conversation.UUID, conversation.Name, conversation.CreatedAt, conversation.UpdatedAt, messages})
}

func activeMessages(messages []json.RawMessage, leaf string) ([]json.RawMessage, error) {
	if leaf == "" {
		return messages, nil
	}
	type node struct {
		UUID   string `json:"uuid"`
		Parent string `json:"parent_message_uuid"`
		Raw    json.RawMessage
	}
	byID := make(map[string]node, len(messages))
	for _, raw := range messages {
		var item node
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		item.Raw = raw
		if item.UUID != "" {
			byID[item.UUID] = item
		}
	}
	chain := make([]json.RawMessage, 0, len(messages))
	seen := make(map[string]struct{})
	for cursor := leaf; cursor != ""; {
		item, ok := byID[cursor]
		if !ok {
			return messages, nil
		}
		if _, exists := seen[cursor]; exists {
			return nil, fmt.Errorf("conversation message tree contains a cycle")
		}
		seen[cursor] = struct{}{}
		chain = append(chain, item.Raw)
		cursor = item.Parent
	}
	for left, right := 0, len(chain)-1; left < right; left, right = left+1, right-1 {
		chain[left], chain[right] = chain[right], chain[left]
	}
	return chain, nil
}

func conversationCachePath(root, id string) string {
	return filepath.Join(root, "conversations", id+".json")
}

func safeConversationID(id string) bool {
	return id != "" && !strings.ContainsAny(id, `/\\`) && id != "." && id != ".."
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func atomicWriteJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
