package db

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/config"
)

// MachineLabelKeyPrefix identifies display labels in archive and mirror metadata.
const MachineLabelKeyPrefix = "machine_label:"

// MachineAliasKeyPrefix identifies proven former archive owners.
const MachineAliasKeyPrefix = "machine_alias:"

// GetMachineLabels returns explicitly recorded labels, keyed by machine identity.
func (db *DB) GetMachineLabels(ctx context.Context) (map[string]string, error) {
	return db.getMachineMetadata(ctx, MachineLabelKeyPrefix)
}

// GetMachineAliases returns former machine keys and their canonical identities.
func (db *DB) GetMachineAliases(ctx context.Context) (map[string]string, error) {
	return db.getMachineMetadata(ctx, MachineAliasKeyPrefix)
}

func (db *DB) getMachineMetadata(ctx context.Context, prefix string) (map[string]string, error) {
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT key, value FROM pg_sync_state
		WHERE key LIKE ? ESCAPE '\'`, strings.ReplaceAll(prefix, "_", "\\_")+"%")
	if err != nil {
		return nil, fmt.Errorf("reading machine metadata: %w", err)
	}
	defer rows.Close()
	metadata := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		metadata[strings.TrimPrefix(key, prefix)] = value
	}
	return metadata, rows.Err()
}

// CanonicalMachineFilter resolves recorded aliases in a comma-separated filter.
func CanonicalMachineFilter(machine string, aliases map[string]string) string {
	machines := strings.Split(machine, ",")
	for i, key := range machines {
		if canonical, ok := aliases[strings.TrimSpace(key)]; ok {
			machines[i] = canonical
		}
	}
	return strings.Join(machines, ",")
}

// ResolveMachineAliases resolves aliases without requiring sessions for a key.
func ResolveMachineAliases(ctx context.Context, store Store, machine string) (string, error) {
	if machine == "" {
		return "", nil
	}
	aliases, err := machineFilterAliases(ctx, store)
	if err != nil {
		return "", err
	}
	return CanonicalMachineFilter(machine, aliases), nil
}

func machineFilterAliases(ctx context.Context, store Store) (map[string]string, error) {
	aliases, err := store.GetMachineAliases(ctx)
	if err != nil {
		return nil, err
	}
	return localMachineAlias(ctx, store, aliases)
}

func localMachineAlias(ctx context.Context, store Store, aliases map[string]string) (map[string]string, error) {
	if local, ok := store.(*DB); ok {
		var identity string
		err := local.getReader().QueryRowContext(ctx, `SELECT COALESCE(
			(SELECT value FROM pg_sync_state WHERE key = ?), '')`, artifactLocalInstallationStateKey).Scan(&identity)
		if err != nil {
			return nil, fmt.Errorf("reading local installation identity: %w", err)
		}
		if identity != "" {
			aliases["local"] = identity
		}
	}
	return aliases, nil
}

// MachineFilterError reports an unknown or ambiguous machine selector.
type MachineFilterError struct {
	Value      string
	Candidates []string
	Known      []string
}

func (e *MachineFilterError) Error() string {
	known := strings.Join(e.Known, ", ")
	if len(e.Candidates) > 1 {
		return fmt.Sprintf("ambiguous machine %q; matching IDs: %s; known machines: %s", e.Value, strings.Join(e.Candidates, ", "), known)
	}
	hint := ""
	if strings.Contains(e.Value, ",") {
		hint = "; comma-separated filters take machine IDs or aliases"
	}
	return fmt.Sprintf("unknown machine %q%s; known machines: %s", e.Value, hint, known)
}

// ResolveMachineFilter accepts aliases, exact IDs, and unique display labels.
func ResolveMachineFilter(ctx context.Context, store Store, machine string) (string, error) {
	machine = strings.TrimSpace(machine)
	if machine == "" {
		return "", nil
	}
	labels, err := store.GetMachineLabels(ctx)
	if err != nil {
		return "", err
	}
	aliases, err := machineFilterAliases(ctx, store)
	if err != nil {
		return "", err
	}
	return ResolveMachineFilterWithMetadata(ctx, store, machine, labels, aliases)
}

// knownMachineIDs lists identities named by metadata.
func knownMachineIDs(labels, aliases map[string]string) map[string]bool {
	known := make(map[string]bool, len(labels)+len(aliases))
	for id := range labels {
		known[id] = true
	}
	for _, id := range aliases {
		known[id] = true
	}
	return known
}

// keepMachineList reports whether every part resolves as an ID.
func keepMachineList(parts []string, known map[string]bool) bool {
	return len(parts) > 0 && !slices.ContainsFunc(parts, func(id string) bool { return !known[id] })
}

// ResolveMachineFilterWithMetadata reuses labels and aliases already read for a catalog response.
func ResolveMachineFilterWithMetadata(ctx context.Context, store Store, machine string, labels, aliases map[string]string) (string, error) {
	machine = strings.TrimSpace(machine)
	parts := splitCSV(CanonicalMachineFilter(machine, aliases))
	known := knownMachineIDs(labels, aliases)
	// Confirm identities absent from metadata against retained session rows.
	var unconfirmed []string
	for _, id := range parts {
		if known[id] {
			continue
		}
		if config.ValidateInstallationID(id) == nil {
			known[id] = true
			continue
		}
		unconfirmed = append(unconfirmed, id)
	}
	if len(unconfirmed) > 0 {
		ids, err := store.GetMatchingMachineIDs(ctx, unconfirmed)
		if err != nil {
			return "", err
		}
		for _, id := range ids {
			known[id] = true
		}
	}
	if keepMachineList(parts, known) {
		return strings.Join(parts, ","), nil
	}
	var candidates []string
	for id, label := range labels {
		if strings.TrimSpace(label) == machine {
			candidates = append(candidates, CanonicalMachineFilter(id, aliases))
		}
	}
	slices.Sort(candidates)
	candidates = slices.Compact(candidates)
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	names, err := store.GetMachines(ctx, false, false)
	if err != nil {
		return "", err
	}
	for _, id := range aliases {
		names = append(names, id)
	}
	for id := range labels {
		names = append(names, id)
	}
	for i, id := range names {
		if label := labels[id]; label != "" {
			names[i] = label
		}
	}
	slices.Sort(names)
	return "", &MachineFilterError{Value: machine, Candidates: candidates, Known: slices.Compact(names)}
}
