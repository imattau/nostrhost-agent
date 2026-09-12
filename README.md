# nostrhost-agent

The native local administrator for NostrHost. The model is an unprivileged
planner; this module owns typed operations, autonomy policy, execution
boundaries, and auditable administration traces.

The first increment deliberately contains no model runtime and performs no
system changes. It establishes a deterministic policy boundary that a future
planner and the NostrHost operation executor can share.

## Initial scope

- Describe operations using stable names and required capabilities.
- Publish strict JSON argument schemas and reject missing, mistyped, or
  unknown arguments before requesting approval or dispatching an operation.
- Evaluate proposals under `observe`, `assist`, `maintain`, or `autonomous`
  autonomy levels.
- Run a bounded Observe → Diagnose → Plan → Execute → Verify cycle through
  injected host interfaces; signed approval checks and audit persistence sit
  on the execution path.
- Require the executor to re-check authorization and validate operation
  arguments; a policy decision is not an execution credential.
- Record observations, proposals, policy outcomes, execution results, and
  verification in a structured cycle trace.
- Deny unknown operations and destructive operations by default.
- Save policy decisions before approval or execution, and save an `executing`
  trace state before dispatching an operation.
- Redact common credential-shaped fields and operation-declared sensitive
  arguments from persisted traces.

There is no shell operation. The agent cannot change its own capabilities or
disable audit. The host must provide typed operation adapters, a verifier that
re-reads current health, and an approval gate that checks signed owner approval.
Relay persistence, concrete NostrHost adapters, retrieval, and inference are
subsequent increments.

## Development

```sh
go test ./...
```

## License

AGPL-3.0-or-later
