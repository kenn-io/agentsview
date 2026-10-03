package rawcheckpoint

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

var (
	ErrBackfillConflict   = errors.New("rawcheckpoint: backfill selection or state conflict")
	ErrBackfillIncomplete = errors.New("rawcheckpoint: backfill incomplete")
)

type BackfillSelection struct {
	Provider         parser.AgentType
	ConfiguredRootID string
	ProjectPath      string
}

// BackfillRoot retains provider metadata needed to reproduce raw manifest paths.
type BackfillRoot struct {
	ConfiguredRoot
	ProjectPath string
}

// BackfillEntry is one configured root entry as written in the user's
// configuration, before the provider resolves it to capture roots.
type BackfillEntry struct {
	Provider parser.AgentType
	Path     string
}

type BackfillRunSpec struct {
	RunID, DeviceID, Destination string
	Providers                    []parser.AgentType
	Roots                        []BackfillSelection
	Entries                      []BackfillEntry
}
type BackfillProgress struct {
	RunID        string           `json:"run_id"`
	Discovery    string           `json:"discovery"`
	Captured     int64            `json:"captured"`
	Acknowledged int64            `json:"acknowledged"`
	Pending      int64            `json:"pending"`
	Watermark    int64            `json:"watermark"`
	Failures     map[string]int64 `json:"failures"`
	Complete     bool             `json:"complete"`
}
type BackfillMember struct {
	RunID                                  string
	Source                                 SourceIdentity
	Ordinal                                int64
	CaptureID, Status, ManifestID, Receipt string
	Generation                             int64
	ErrorClass                             string
}
type BackfillPassResult struct {
	Complete                       bool
	Changed, Unsupported, Degraded int64
	ErrorClass                     string
}

// BeginBackfill creates or verifies an immutable device, destination and root set.
// Paths live only in the private selection snapshot, never progress or errors.
func (s *Store) BeginBackfill(ctx context.Context, spec BackfillRunSpec) (BackfillProgress, error) {
	if !backfillToken(spec.RunID) || spec.DeviceID == "" || len(spec.Providers) == 0 {
		return BackfillProgress{}, ErrBackfillConflict
	}
	destination, err := normalizeDestination(spec.Destination)
	if err != nil {
		return BackfillProgress{}, ErrBackfillConflict
	}
	spec.Destination = destination
	spec.Providers = slices.Clone(spec.Providers)
	slices.Sort(spec.Providers)
	spec.Providers = slices.Compact(spec.Providers)
	// Root and entry order is part of the run: it decides which root owns a
	// file under overlapping roots, so only providers are put in a fixed order.
	spec.Roots = uniqueInProviderOrder(spec.Roots, func(root BackfillSelection) parser.AgentType { return root.Provider })
	spec.Entries = uniqueInProviderOrder(spec.Entries, func(entry BackfillEntry) parser.AgentType { return entry.Provider })
	for _, entry := range spec.Entries {
		if !slices.Contains(spec.Providers, entry.Provider) {
			return BackfillProgress{}, ErrBackfillConflict
		}
	}
	err = s.withImmediateWrite(ctx, "begin backfill", func(conn *sql.Conn) error {
		if err := requireConfiguredDeviceConn(ctx, conn, spec.DeviceID); err != nil {
			return err
		}
		if err := ensureDestinationConn(ctx, conn, spec.Destination); err != nil {
			return err
		}
		type rootSelection struct {
			BackfillSelection
			LocalRoot string
		}
		selected := make([]rootSelection, 0, len(spec.Roots))
		for _, root := range spec.Roots {
			if !slices.Contains(spec.Providers, root.Provider) {
				return ErrBackfillConflict
			}
			var path string
			if err := conn.QueryRowContext(ctx, `SELECT local_root FROM configured_roots WHERE id=? AND provider=?`, root.ConfiguredRootID, string(root.Provider)).Scan(&path); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrBackfillConflict
				}
				return err
			}
			selected = append(selected, rootSelection{root, path})
		}
		// Root IDs keep the snapshot unique when invalid UTF-8 in a path is replaced.
		selection, err := json.Marshal(struct {
			Providers []parser.AgentType
			Roots     []rootSelection
			Entries   []BackfillEntry
		}{spec.Providers, selected, spec.Entries}, jsontext.AllowInvalidUTF8(true))
		if err != nil {
			return ErrBackfillConflict
		}
		var device, dest, stored string
		err = conn.QueryRowContext(ctx, `SELECT device_id,destination,selection FROM backfill_runs WHERE run_id=?`, spec.RunID).Scan(&device, &dest, &stored)
		if err == nil {
			if device != spec.DeviceID || dest != spec.Destination || stored != string(selection) {
				return ErrBackfillConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err = conn.ExecContext(ctx, `INSERT INTO backfill_runs(run_id,device_id,destination,selection) VALUES(?,?,?,?)`, spec.RunID, spec.DeviceID, spec.Destination, string(selection)); err != nil {
			return err
		}
		for _, provider := range spec.Providers {
			if provider == "" {
				return ErrBackfillConflict
			}
			if _, err = conn.ExecContext(ctx, `INSERT INTO backfill_providers(run_id,provider) VALUES(?,?)`, spec.RunID, string(provider)); err != nil {
				return err
			}
		}
		for ordinal, root := range selected {
			if _, err = conn.ExecContext(ctx, `INSERT INTO backfill_roots(run_id,provider,configured_root_id,local_root,project_path,ordinal) VALUES(?,?,?,?,?,?)`, spec.RunID, string(root.Provider), root.ConfiguredRootID, root.LocalRoot, root.ProjectPath, ordinal); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return BackfillProgress{}, err
	}
	return s.BackfillProgress(ctx, spec.RunID)
}

func uniqueInProviderOrder[T comparable](items []T, provider func(T) parser.AgentType) []T {
	unique := make([]T, 0, len(items))
	for _, item := range items {
		if !slices.Contains(unique, item) {
			unique = append(unique, item)
		}
	}
	slices.SortStableFunc(unique, func(a, b T) int {
		return strings.Compare(string(provider(a)), string(provider(b)))
	})
	return unique
}

func backfillToken(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

func requireBackfillConn(ctx context.Context, conn *sql.Conn, runID string) (string, error) {
	var device, state string
	if err := conn.QueryRowContext(ctx, `SELECT device_id,discovery FROM backfill_runs WHERE run_id=?`, runID).Scan(&device, &state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrBackfillConflict
		}
		return "", err
	}
	return state, requireConfiguredDeviceConn(ctx, conn, device)
}

// BackfillProgress reports durable aggregates, with no local source identifiers.
func (s *Store) BackfillProgress(ctx context.Context, runID string) (BackfillProgress, error) {
	var p BackfillProgress
	err := s.withImmediateWrite(ctx, "backfill progress", func(conn *sql.Conn) error {
		if _, err := requireBackfillConn(ctx, conn, runID); err != nil {
			return err
		}
		var invalid int64
		var class string
		if err := conn.QueryRowContext(ctx, `SELECT run_id,discovery,captured,acknowledged,invalidated,watermark,complete,error_class FROM backfill_runs WHERE run_id=?`, runID).Scan(&p.RunID, &p.Discovery, &p.Captured, &p.Acknowledged, &invalid, &p.Watermark, &p.Complete, &class); err != nil {
			return err
		}
		p.Pending = p.Captured - p.Acknowledged
		p.Failures = map[string]int64{}
		if class != "" {
			p.Failures[class]++
		}
		// A capture_lost run class restates its invalidated members; count them once.
		if invalid > 0 {
			p.Failures["capture_lost"] = invalid
		}
		rows, err := conn.QueryContext(ctx, `SELECT changed,unsupported,degraded,error_class FROM backfill_providers WHERE run_id=?`, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var changed, unsupported, degraded int64
			var class string
			if err := rows.Scan(&changed, &unsupported, &degraded, &class); err != nil {
				return err
			}
			for k, v := range map[string]int64{"source_changed": changed, "unsupported": unsupported, "capacity": degraded} {
				if v > 0 {
					p.Failures[k] += v
				}
			}
			if class != "" {
				p.Failures[class]++
			}
		}
		return rows.Err()
	})
	return p, err
}

// BackfillSource looks up the immutable binding; invalidated bindings remain found.
func (s *Store) BackfillSource(ctx context.Context, runID string, source SourceIdentity) (BackfillMember, bool, error) {
	var member BackfillMember
	found := false
	err := s.withImmediateWrite(ctx, "backfill source", func(conn *sql.Conn) error {
		if _, err := requireBackfillConn(ctx, conn, runID); err != nil {
			return err
		}
		member.RunID = runID
		member.Source = source
		err := conn.QueryRowContext(ctx, `SELECT ordinal,capture_id,status,manifest_id,receipt,generation,error_class FROM backfill_members WHERE run_id=? AND provider=? AND configured_root_id=? AND source_key=?`, runID, string(source.Provider), source.ConfiguredRootID, source.SourceKey).Scan(&member.Ordinal, &member.CaptureID, &member.Status, &member.ManifestID, &member.Receipt, &member.Generation, &member.ErrorClass)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	return member, found, err
}

// BackfillProviderComplete allows a resumed coordinator to skip completed passes.
func (s *Store) BackfillProviderComplete(ctx context.Context, runID string, provider parser.AgentType) (bool, error) {
	var complete bool
	err := s.db.QueryRowContext(ctx, `SELECT complete FROM backfill_providers WHERE run_id=? AND provider=?`, runID, string(provider)).Scan(&complete)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrBackfillConflict
	}
	return complete, err
}

// FinishBackfillProvider replaces an unfinished provider's prior attempt outcome.
// Complete passes cannot be reset. A successful retry explicitly clears failures.
func (s *Store) FinishBackfillProvider(ctx context.Context, runID string, provider parser.AgentType, pass BackfillPassResult) error {
	if pass.Changed < 0 || pass.Unsupported < 0 || pass.Degraded < 0 || !validBackfillFailure(pass.ErrorClass) {
		return ErrBackfillConflict
	}
	complete := pass.Complete && pass.Changed == 0 && pass.Unsupported == 0 && pass.Degraded == 0 && pass.ErrorClass == ""
	if !complete && pass.Changed == 0 && pass.Unsupported == 0 && pass.Degraded == 0 && pass.ErrorClass == "" {
		pass.ErrorClass = "discovery_incomplete"
	}
	return s.withImmediateWrite(ctx, "finish backfill provider", func(conn *sql.Conn) error {
		state, err := requireBackfillConn(ctx, conn, runID)
		if err != nil {
			return err
		}
		if state != "open" {
			return ErrBackfillConflict
		}
		if complete {
			var roots int
			if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM backfill_roots WHERE run_id=? AND provider=?`, runID, string(provider)).Scan(&roots); err != nil {
				return err
			}
			if roots == 0 {
				return ErrBackfillIncomplete
			}
		}
		result, err := conn.ExecContext(ctx, `UPDATE backfill_providers SET complete=?,changed=?,unsupported=?,degraded=?,error_class=? WHERE run_id=? AND provider=? AND complete=0`, complete, pass.Changed, pass.Unsupported, pass.Degraded, pass.ErrorClass, runID, string(provider))
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrBackfillConflict
		}
		return nil
	})
}

// RecordBackfillFailure stores only a closed vocabulary; empty clears a retry error.
func (s *Store) RecordBackfillFailure(ctx context.Context, runID, class string) error {
	if !validBackfillFailure(class) {
		return ErrBackfillConflict
	}
	return s.withImmediateWrite(ctx, "backfill failure", func(conn *sql.Conn) error {
		if _, err := requireBackfillConn(ctx, conn, runID); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, `UPDATE backfill_runs SET error_class=? WHERE run_id=? AND complete=0`, class, runID)
		return err
	})
}

func validBackfillFailure(class string) bool {
	switch class {
	case "", "discovery_incomplete", "source_changed", "unsupported", "capacity", "capture", "upload", "deferred", "rejected", "cancelled", "root_unavailable", "capture_lost":
		return true
	}
	return false
}

func (s *Store) SealBackfill(ctx context.Context, runID string) (BackfillProgress, error) {
	err := s.withImmediateWrite(ctx, "seal backfill", func(conn *sql.Conn) error {
		state, err := requireBackfillConn(ctx, conn, runID)
		if err != nil {
			return err
		}
		if state == "sealed" {
			return nil
		}
		var unfinished, invalid int64
		var class string
		if err := conn.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM backfill_providers WHERE run_id=? AND complete=0),invalidated,error_class FROM backfill_runs WHERE run_id=?`, runID, runID).Scan(&unfinished, &invalid, &class); err != nil {
			return err
		}
		if unfinished != 0 || invalid != 0 || class != "" {
			return ErrBackfillIncomplete
		}
		_, err = conn.ExecContext(ctx, `UPDATE backfill_runs SET discovery='sealed',watermark=captured WHERE run_id=?`, runID)
		return err
	})
	if err != nil {
		return BackfillProgress{}, err
	}
	return s.BackfillProgress(ctx, runID)
}

func (s *Store) CompleteBackfill(ctx context.Context, runID string) (BackfillProgress, error) {
	err := s.withImmediateWrite(ctx, "complete backfill", func(conn *sql.Conn) error {
		state, err := requireBackfillConn(ctx, conn, runID)
		if err != nil {
			return err
		}
		if state != "sealed" {
			return ErrBackfillIncomplete
		}
		var pending, invalid int64
		var class string
		if err := conn.QueryRowContext(ctx, `SELECT watermark-acknowledged,invalidated,error_class FROM backfill_runs WHERE run_id=?`, runID).Scan(&pending, &invalid, &class); err != nil {
			return err
		}
		if pending != 0 || invalid != 0 || class != "" {
			return ErrBackfillIncomplete
		}
		_, err = conn.ExecContext(ctx, `UPDATE backfill_runs SET complete=1 WHERE run_id=?`, runID)
		return err
	})
	if err != nil {
		return BackfillProgress{}, err
	}
	return s.BackfillProgress(ctx, runID)
}

func bindBackfillCaptureConn(ctx context.Context, conn *sql.Conn, runID string, source SourceIdentity, captureID string) error {
	if runID == "" {
		return nil
	}
	state, err := requireBackfillConn(ctx, conn, runID)
	if err != nil {
		return err
	}
	if state != "open" || captureID == "" {
		return ErrBackfillConflict
	}
	var selected int
	err = conn.QueryRowContext(ctx, `SELECT count(*) FROM backfill_roots AS r JOIN backfill_providers AS p ON p.run_id=r.run_id AND p.provider=r.provider JOIN configured_roots AS c ON c.id=r.configured_root_id AND c.provider=r.provider AND c.local_root=r.local_root WHERE r.run_id=? AND r.provider=? AND r.configured_root_id=? AND p.complete=0`, runID, string(source.Provider), source.ConfiguredRootID).Scan(&selected)
	if err != nil {
		return err
	}
	if selected != 1 {
		return ErrBackfillConflict
	}
	var exists int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM backfill_members WHERE run_id=? AND provider=? AND configured_root_id=? AND source_key=?`, runID, string(source.Provider), source.ConfiguredRootID, source.SourceKey).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return ErrBackfillConflict
	}
	var pending int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM outbox_generations WHERE capture_id=? AND provider=? AND configured_root_id=? AND source_key=? AND state IN ('queued','finalized')`, captureID, string(source.Provider), source.ConfiguredRootID, source.SourceKey).Scan(&pending); err != nil {
		return err
	}
	status := "pending"
	var manifest, receipt string
	var generation int64
	if pending == 0 {
		if err := conn.QueryRowContext(ctx, `SELECT head_manifest_id,head_receipt,head_generation FROM raw_sources WHERE provider=? AND configured_root_id=? AND source_key=? AND head_capture_id=?`, string(source.Provider), source.ConfiguredRootID, source.SourceKey, captureID).Scan(&manifest, &receipt, &generation); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrBackfillConflict
			}
			return err
		}
		if manifest == "" || receipt == "" || generation <= 0 {
			return ErrBackfillConflict
		}
		status = "acknowledged"
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO backfill_members(run_id,provider,configured_root_id,source_key,ordinal,capture_id,status,manifest_id,receipt,generation) SELECT run_id,?,?,?,captured+1,?,?,?,?,? FROM backfill_runs WHERE run_id=?`, string(source.Provider), source.ConfiguredRootID, source.SourceKey, captureID, status, manifest, receipt, generation, runID)
	return err
}

const backfillPendingClosure = `WITH RECURSIVE backfill_pending(capture_id) AS (
 SELECT capture_id FROM backfill_members WHERE run_id=? AND status='pending'
 UNION
 SELECT g.predecessor_capture_id FROM outbox_generations AS g
 JOIN backfill_pending AS b ON b.capture_id=g.capture_id
 WHERE g.predecessor_capture_id IS NOT NULL
) `

// BackfillRoots returns the private immutable root selection for one provider.
func (s *Store) BackfillRoots(ctx context.Context, runID string, provider parser.AgentType) ([]BackfillRoot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT configured_root_id,local_root,project_path FROM backfill_roots WHERE run_id=? AND provider=? ORDER BY ordinal`, runID, string(provider))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	roots := []BackfillRoot{}
	for rows.Next() {
		root := BackfillRoot{Provider: provider}
		if err := rows.Scan(&root.ID, &root.LocalPath, &root.ProjectPath); err != nil {
			return nil, err
		}
		roots = append(roots, root)
	}
	return roots, rows.Err()
}

// BackfillRejected reports whether a pending capture of the run, or one it
// waits on, was permanently rejected by the server.
func (s *Store) BackfillRejected(ctx context.Context, runID string) (bool, error) {
	var blocked int
	err := s.db.QueryRowContext(ctx, backfillPendingClosure+`SELECT count(*) FROM outbox_generations
 WHERE blocked = 1 AND capture_id IN (SELECT capture_id FROM backfill_pending)`, runID).Scan(&blocked)
	return blocked > 0, err
}
