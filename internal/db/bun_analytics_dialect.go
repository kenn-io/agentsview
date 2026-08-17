package db

// BunAnalyticsDialect supplies only the SQL expressions that genuinely vary
// between engines. BunStore owns the surrounding CTEs, joins, predicates,
// aggregates, ordering, and limits. Operands and granularities are
// application-owned SQL; runtime values remain Bun arguments in the returned
// fragments.
type BunAnalyticsDialect interface {
	LocalTimestamp(operand, timezone string) BunSQLFragment
	Date(operand BunSQLFragment) BunSQLFragment
	Bucket(operand BunSQLFragment, granularity string) BunSQLFragment
	Hour(operand BunSQLFragment) BunSQLFragment
	ISOWeekday(operand BunSQLFragment) BunSQLFragment
	DurationSeconds(start, end BunSQLFragment) BunSQLFragment
}

type sqliteBunAnalyticsDialect struct{}
type postgresBunAnalyticsDialect struct{}
type duckBunAnalyticsDialect struct{}

func SQLiteBunAnalyticsDialect() BunAnalyticsDialect   { return sqliteBunAnalyticsDialect{} }
func PostgresBunAnalyticsDialect() BunAnalyticsDialect { return postgresBunAnalyticsDialect{} }
func DuckDBBunAnalyticsDialect() BunAnalyticsDialect   { return duckBunAnalyticsDialect{} }

func analyticsGranularity(value string) string {
	switch value {
	case "week", "month":
		return value
	default:
		return "day"
	}
}

func (sqliteBunAnalyticsDialect) LocalTimestamp(
	operand, timezone string,
) BunSQLFragment {
	return BunSQL("agentsview_local_timestamp("+operand+", ?)", timezone)
}

func (sqliteBunAnalyticsDialect) Date(operand BunSQLFragment) BunSQLFragment {
	return BunSQL("SUBSTR("+operand.SQL+", 1, 10)", operand.Args...)
}

func (sqliteBunAnalyticsDialect) Bucket(
	operand BunSQLFragment, granularity string,
) BunSQLFragment {
	switch analyticsGranularity(granularity) {
	case "week":
		return BunSQL("DATE("+operand.SQL+
			", '-' || ((CAST(STRFTIME('%w', "+operand.SQL+
			") AS INTEGER) + 6) % 7) || ' days')", append(
			append([]any(nil), operand.Args...), operand.Args...,
		)...)
	case "month":
		return BunSQL("SUBSTR("+operand.SQL+", 1, 7) || '-01'", operand.Args...)
	default:
		return BunSQL("SUBSTR("+operand.SQL+", 1, 10)", operand.Args...)
	}
}

func (sqliteBunAnalyticsDialect) Hour(operand BunSQLFragment) BunSQLFragment {
	return BunSQL("CAST(SUBSTR("+operand.SQL+", 12, 2) AS INTEGER)", operand.Args...)
}

func (sqliteBunAnalyticsDialect) ISOWeekday(operand BunSQLFragment) BunSQLFragment {
	return BunSQL("((CAST(STRFTIME('%w', "+operand.SQL+
		") AS INTEGER) + 6) % 7)", operand.Args...)
}

func (sqliteBunAnalyticsDialect) DurationSeconds(
	start, end BunSQLFragment,
) BunSQLFragment {
	return BunSQL("((JULIANDAY("+end.SQL+") - JULIANDAY("+start.SQL+
		")) * 86400.0)", append(append([]any(nil), end.Args...), start.Args...)...)
}

func (postgresBunAnalyticsDialect) LocalTimestamp(
	operand, timezone string,
) BunSQLFragment {
	return BunSQL("("+operand+" AT TIME ZONE ?)", timezone)
}

func (postgresBunAnalyticsDialect) Date(operand BunSQLFragment) BunSQLFragment {
	return BunSQL("TO_CHAR("+operand.SQL+", 'YYYY-MM-DD')", operand.Args...)
}

func (postgresBunAnalyticsDialect) Bucket(
	operand BunSQLFragment, granularity string,
) BunSQLFragment {
	switch analyticsGranularity(granularity) {
	case "week":
		return BunSQL("TO_CHAR(DATE_TRUNC('week', "+operand.SQL+
			"), 'YYYY-MM-DD')", operand.Args...)
	case "month":
		return BunSQL("TO_CHAR(DATE_TRUNC('month', "+operand.SQL+
			"), 'YYYY-MM-DD')", operand.Args...)
	default:
		return BunSQL("TO_CHAR("+operand.SQL+", 'YYYY-MM-DD')", operand.Args...)
	}
}

func (postgresBunAnalyticsDialect) Hour(operand BunSQLFragment) BunSQLFragment {
	return BunSQL("CAST(EXTRACT(HOUR FROM "+operand.SQL+") AS INTEGER)", operand.Args...)
}

func (postgresBunAnalyticsDialect) ISOWeekday(operand BunSQLFragment) BunSQLFragment {
	return BunSQL("CAST(EXTRACT(ISODOW FROM "+operand.SQL+") AS INTEGER) - 1", operand.Args...)
}

func (postgresBunAnalyticsDialect) DurationSeconds(
	start, end BunSQLFragment,
) BunSQLFragment {
	return BunSQL("EXTRACT(EPOCH FROM ("+end.SQL+" - "+start.SQL+"))",
		append(append([]any(nil), end.Args...), start.Args...)...)
}

func (duckBunAnalyticsDialect) LocalTimestamp(
	operand, timezone string,
) BunSQLFragment {
	return BunSQL("TIMEZONE(?, TIMEZONE('UTC', "+operand+"))", timezone)
}

func (duckBunAnalyticsDialect) Date(operand BunSQLFragment) BunSQLFragment {
	return BunSQL("STRFTIME("+operand.SQL+", '%Y-%m-%d')", operand.Args...)
}

func (duckBunAnalyticsDialect) Bucket(
	operand BunSQLFragment, granularity string,
) BunSQLFragment {
	switch analyticsGranularity(granularity) {
	case "week":
		return BunSQL("STRFTIME(DATE_TRUNC('week', "+operand.SQL+
			"), '%Y-%m-%d')", operand.Args...)
	case "month":
		return BunSQL("STRFTIME(DATE_TRUNC('month', "+operand.SQL+
			"), '%Y-%m-%d')", operand.Args...)
	default:
		return BunSQL("STRFTIME("+operand.SQL+", '%Y-%m-%d')", operand.Args...)
	}
}

func (duckBunAnalyticsDialect) Hour(operand BunSQLFragment) BunSQLFragment {
	return BunSQL("CAST(DATE_PART('hour', "+operand.SQL+") AS INTEGER)", operand.Args...)
}

func (duckBunAnalyticsDialect) ISOWeekday(operand BunSQLFragment) BunSQLFragment {
	return BunSQL("CAST(DATE_PART('isodow', "+operand.SQL+") AS INTEGER) - 1", operand.Args...)
}

func (duckBunAnalyticsDialect) DurationSeconds(
	start, end BunSQLFragment,
) BunSQLFragment {
	return BunSQL("EPOCH("+end.SQL+" - "+start.SQL+")",
		append(append([]any(nil), end.Args...), start.Args...)...)
}
