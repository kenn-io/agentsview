#!/bin/sh
set -eu
service=$1
port=$2
shift 2

case "$service" in
    postgres)
        export TEST_PG_PUBLISH=127.0.0.1::5432
        export TEST_CH_HTTP_PUBLISH=
        export TEST_CH_NATIVE_PUBLISH= ;;
    clickhouse)
        export TEST_PG_PUBLISH=
        export TEST_CH_HTTP_PUBLISH=127.0.0.1::8123
        export TEST_CH_NATIVE_PUBLISH=127.0.0.1::9000 ;;
esac
docker compose -p "$TEST_SERVICE_LEASE" -f docker-compose.test.yml -f docker-compose.test-lease.yml up -d --wait "$service"
address=$(docker compose -p "$TEST_SERVICE_LEASE" -f docker-compose.test.yml -f docker-compose.test-lease.yml port "$service" "$port")
pg_url=
ch_url=
case "$service" in
    postgres) pg_url="postgres://agentsview_test:agentsview_test_password@${address}/agentsview_test?sslmode=disable" ;;
    clickhouse) ch_url="clickhouse://${address}/default" ;;
esac
# Override values passed to the parent Make on its command line.
exec "$@" "TEST_SERVICE_LEASE=$TEST_SERVICE_LEASE" "TEST_PG_URL=$pg_url" "TEST_CLICKHOUSE_URL=$ch_url" "TEST_PG_STANDBY_URL="
