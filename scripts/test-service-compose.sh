#!/bin/sh
set -eu
service=$1
port=$2
shift 2

cleanup() {
    status=$?
    trap - EXIT HUP INT TERM
    if ! docker compose -p "$TEST_SERVICE_LEASE" -f docker-compose.test.yml -f docker-compose.test-lease.yml down -v --remove-orphans; then
        echo 'cleanup failed; scheduled cleanup will retry' >&2
    fi
    exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
export TEST_PG_PUBLISH=127.0.0.1::5432
export TEST_CH_HTTP_PUBLISH=127.0.0.1::8123
export TEST_CH_NATIVE_PUBLISH=127.0.0.1::9000
docker compose -p "$TEST_SERVICE_LEASE" -f docker-compose.test.yml -f docker-compose.test-lease.yml up -d --wait "$service"
address=$(docker compose -p "$TEST_SERVICE_LEASE" -f docker-compose.test.yml -f docker-compose.test-lease.yml port "$service" "$port")
case "$service" in
    postgres) export TEST_PG_URL="postgres://agentsview_test:agentsview_test_password@${address}/agentsview_test?sslmode=disable" ;;
    clickhouse) export TEST_CLICKHOUSE_URL="clickhouse://${address}/default" ;;
esac
"$@"
