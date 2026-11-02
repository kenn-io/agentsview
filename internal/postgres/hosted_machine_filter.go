package postgres

import "strings"

// Device identities are public enrollment identifiers, never credentials. Keep
// their machine keys separate from legacy installation identities.
const hostedRawMachinePrefix = "raw-device:"

// Enrollment permits UTF-8 identifiers, including CSV separators. Machine
// selectors must remain opaque, injective keys that round-trip CSV filters.
const hostedRawMachineKeySQL = `'` + hostedRawMachinePrefix + `' || encode(convert_to(raw_machine_source.device_id,'UTF8'),'hex')`

// Membership follows the currently selected content cohort, including retained
// contributions, rather than every historical capture. Effective exclusions must
// match materializeGroup so an excluded branch cannot supply visible metadata.
var hostedRawMachineSourceSQL = `SELECT machine_branch.tenant_id,
 machine_branch.session_id, machine_source.device_id
 FROM raw_session_branches machine_branch
 JOIN raw_source_projections machine_source
   ON machine_source.tenant_id=machine_branch.tenant_id
  AND machine_source.source_id=machine_branch.source_id
 WHERE machine_branch.active
 AND NOT COALESCE(` + rawBranchExclusionSQL("machine_branch", "c.tenant_id=machine_branch.tenant_id AND ") + `,false)`

// Both aggregate readers materialize their eligible sessions once, then count
// source identities separately so multiple contributors never multiply totals.
// filtered_sessions supplies id, tenant_id, project, machine and provenance_kind.
var hostedFilteredMachinesCTESQL = `,
 filtered_source_machines AS (` + hostedMachineRowsSQL("filtered_sessions") + `)`

func hostedMachineRowsSQL(eligibleSessions string) string {
	return `SELECT filtered.id AS session_id, filtered.project, ` + hostedRawMachineKeySQL + ` AS machine
   FROM ` + eligibleSessions + ` filtered
   JOIN (` + hostedRawMachineSourceSQL + `) raw_machine_source
     ON raw_machine_source.tenant_id=filtered.tenant_id AND raw_machine_source.session_id=filtered.id
   WHERE filtered.provenance_kind='raw'
   UNION ALL
   SELECT id AS session_id,project,machine FROM ` + eligibleSessions + ` WHERE provenance_kind='legacy'`
}

func hostedMachinePredicate(sessionIDExpr, machineExpr string, placeholders []string) string {
	values := "(" + strings.Join(placeholders, ", ") + ")"
	return "(" + machineExpr + " IN " + values +
		" OR EXISTS (SELECT 1 FROM (" + hostedRawMachineSourceSQL +
		") raw_machine_source WHERE raw_machine_source.session_id=" + sessionIDExpr +
		" AND " + hostedRawMachineKeySQL + " IN " + values + "))"
}
