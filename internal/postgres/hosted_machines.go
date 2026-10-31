package postgres

import (
	"context"
	"fmt"
)

// A raw session can have several contributing devices. Keep that relation at
// read time instead of assigning the shared transcript to an arbitrary device.
var hostedMachineCatalogSQL = hostedMachineRowsSQL("(SELECT * FROM sessions WHERE tenant_id=$1)")

func (h *HostedStore) GetMachines(ctx context.Context, excludeOneShot, excludeAutomated bool) ([]string, error) {
	query := `SELECT DISTINCT m.machine FROM (` + hostedMachineCatalogSQL + `) m
 JOIN sessions s ON s.id=m.session_id WHERE s.deleted_at IS NULL`
	query += machineVisibilitySQL("s.", excludeOneShot, excludeAutomated)
	query += " ORDER BY m.machine"
	return h.machineIDs(ctx, query, h.tenant)
}

func (h *HostedStore) GetMatchingMachineIDs(ctx context.Context, candidates []string) ([]string, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	// As in ordinary archives, retain exact selectors for trashed sessions.
	// Old hosted URLs used the physical "hosted" key. Keep exact selectors
	// working without advertising that compatibility key as a source device.
	return h.machineIDs(ctx, `SELECT DISTINCT machine FROM (
 `+hostedMachineCatalogSQL+` UNION ALL SELECT id,project,machine FROM sessions WHERE provenance_kind='raw' AND tenant_id=$1) m
 WHERE machine=ANY($2) ORDER BY machine`, h.tenant, candidates)
}

func (h *HostedStore) machineIDs(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := h.physical.pg.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading hosted machines: %w", err)
	}
	defer rows.Close()
	machines := []string{}
	for rows.Next() {
		var machine string
		if err := rows.Scan(&machine); err != nil {
			return nil, err
		}
		machines = append(machines, machine)
	}
	return machines, rows.Err()
}

func (h *HostedStore) GetMachineLabels(ctx context.Context) (map[string]string, error) {
	labels, err := h.physical.GetMachineLabels(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := h.physical.pg.QueryContext(ctx, `SELECT `+hostedRawMachineKeySQL+`,raw_machine_source.display_name
 FROM raw_devices raw_machine_source WHERE raw_machine_source.tenant_id=$1`, h.tenant)
	if err != nil {
		return nil, fmt.Errorf("reading hosted machine labels: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, label string
		if err := rows.Scan(&id, &label); err != nil {
			return nil, err
		}
		labels[id] = label
	}
	return labels, rows.Err()
}

type hostedMachineRef struct {
	session string
	field   *[]string
}

// sourceMachines records physical coordinates before mapIDs replaces them with
// public aliases. All result shapes use one bounded batch read, including search.
func (r *hostedRefs) sourceMachines(session string, field *[]string) {
	r.machines = append(r.machines, hostedMachineRef{session: session, field: field})
}

func (r *hostedRefs) mapSourceMachines(ctx context.Context, h *HostedStore) error {
	if len(r.machines) == 0 {
		return nil
	}
	fields := make(map[string][]*[]string, len(r.machines))
	ids := make([]string, 0, len(r.machines))
	for _, ref := range r.machines {
		*ref.field = nil
		if _, ok := fields[ref.session]; !ok {
			ids = append(ids, ref.session)
		}
		fields[ref.session] = append(fields[ref.session], ref.field)
	}
	rows, err := h.physical.pg.QueryContext(ctx, `SELECT DISTINCT raw_machine_source.session_id,
 `+hostedRawMachineKeySQL+` AS machine
 FROM (`+hostedRawMachineSourceSQL+`) raw_machine_source
 WHERE raw_machine_source.tenant_id=$1 AND raw_machine_source.session_id=ANY($2)
 ORDER BY raw_machine_source.session_id,machine`, h.tenant, ids)
	if err != nil {
		return fmt.Errorf("reading hosted session source machines: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var session, machine string
		if err := rows.Scan(&session, &machine); err != nil {
			return err
		}
		for _, field := range fields[session] {
			*field = append(*field, machine)
		}
	}
	return rows.Err()
}
