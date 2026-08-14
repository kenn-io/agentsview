# Daemon dual-stack port collision design

## Problem

When AgentsView is configured with `host = "0.0.0.0"`, Go can bind an IPv6-only
wildcard listener even when another process already owns the requested port on
IPv4. AgentsView then publishes an IPv4 loopback runtime endpoint and uses a
bare TCP connection as its readiness signal. The readiness check can connect to
the unrelated IPv4 process, while daemon identity probes continue to reject
that process. An attached `agentsview daemon restart` then waits indefinitely
after the real server has started on IPv6.

## Design

Port selection will treat a wildcard port as unavailable when either the IPv4
or IPv6 wildcard address is occupied. The existing next-port fallback remains
the user-visible behavior: a collision on port 8080 selects port 8081 and emits
the existing fallback message.

Backend readiness will verify an AgentsView HTTP endpoint instead of accepting
any successful TCP connection. The probe will use the configured authentication
token and require the daemon ping contract. An unrelated HTTP server therefore
cannot make startup publish a runtime record. Managed Caddy readiness remains a
generic TCP check because Caddy does not implement the AgentsView daemon
protocol.

This is a focused fix rather than an atomic listener-handoff refactor. A process
can still claim a selected port between the availability check and the server
bind, but the server then fails startup or the identity-aware readiness check
rejects the wrong listener. It cannot publish a healthy runtime for another
process.

## Compatibility

The CLI, configuration, runtime record, HTTP API, and database formats do not
change. Normal listeners keep their configured ports. Only a split IPv4/IPv6
collision changes behavior, and that case now follows the existing documented
next-port fallback. No compatibility adapter, migration, or version bump is
needed.

## Tests

- Occupy a port on IPv4 and verify wildcard port selection skips it.
- When IPv6 is available, occupy a port on IPv6 and verify wildcard port
  selection skips it.
- Verify backend readiness rejects an unrelated HTTP listener.
- Verify backend readiness accepts an authenticated AgentsView daemon endpoint.

