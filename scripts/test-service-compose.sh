#!/bin/sh
set -eu
service=$1
port=$2
shift 2

export TEST_PG_PUBLISH=127.0.0.1::5432
export TEST_CH_HTTP_PUBLISH=127.0.0.1::8123
export TEST_CH_NATIVE_PUBLISH=127.0.0.1::9000
docker compose -p "$TEST_SERVICE_LEASE" -f docker-compose.test.yml -f docker-compose.test-lease.yml up -d --wait "$service"
address=$(docker compose -p "$TEST_SERVICE_LEASE" -f docker-compose.test.yml -f docker-compose.test-lease.yml port "$service" "$port")
case "$service" in
    postgres) url="TEST_PG_URL=postgres://agentsview_test:agentsview_test_password@${address}/agentsview_test?sslmode=disable"; other=TEST_CLICKHOUSE_URL= ;;
    clickhouse) url="TEST_CLICKHOUSE_URL=clickhouse://${address}/default"; other=TEST_PG_URL= ;;
esac
export "$url"
# Override values passed to the parent Make on its command line.
exec "$@" "TEST_SERVICE_LEASE=$TEST_SERVICE_LEASE" "$url" "$other"
