# nostrhost-agent

The native local administrator for NostrHost. The model is an unprivileged
planner; this module owns typed operations, autonomy policy, execution
boundaries, and auditable administration traces.

The first increment deliberately contains no model runtime and performs no
system changes. It establishes a deterministic policy boundary that a future
planner and the NostrHost operation executor can share.

## Initial scope

- Describe operations using stable names and required capabilities.
- Evaluate proposals under `observe`, `assist`, `maintain`, or `autonomous`
  autonomy levels.
- Require the executor to re-check authorization and validate operation
  arguments; a policy decision is not an execution credential.
- Record observations, proposals, policy outcomes, execution results, and
  verification in a structured cycle trace.
- Deny unknown operations and destructive operations by default.

There is no shell operation. The agent cannot change its own capabilities or
disable audit. Relay persistence, signed owner approvals, operation adapters,
retrieval, and inference are subsequent increments.

## Development

```sh
go test ./...
```

## License

AGPL-3.0-or-later
