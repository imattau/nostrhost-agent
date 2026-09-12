# nostrhost-agent

The native local administrator for NostrHost. The model is an unprivileged
planner; this module owns typed operations, autonomy policy, execution
boundaries, and auditable administration traces.

The current increment establishes the deterministic policy boundary, a local
OpenAI-compatible planner, and the NostrHost operation relay adapter. Machine
changes still run through NostrHost's authoritative operation daemon; all
agent-side effects are behind typed interfaces.

## Initial scope

- Describe operations using stable names and required capabilities.
- Publish strict JSON argument schemas and reject missing, mistyped, or
  unknown arguments before requesting approval or dispatching an operation;
  operation arguments are capped at 64 KiB.
- Evaluate proposals under `observe`, `assist`, `maintain`, or `autonomous`
  autonomy levels.
- Run a bounded Observe → Diagnose → Plan → Execute → Verify cycle through
  injected host interfaces; each cycle handles at most one proposal so the
  next action uses fresh observations.
- Publish signed kind-2200 operation requests to the local relay and accept
  only correlated, signature-verified kind-2204 results from the configured
  server identity.
- Retrieve bounded lexical matches from an operator-selected local corpus;
  record the source document hashes in the administration trace.
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
The relay adapter only allows loopback relay URLs and uses the agent identity
for NIP-42 authentication; NostrHost's operation daemon remains authoritative
for per-operation authorization and execution. The local planner only emits
typed proposals and cannot execute operations. A persistent index, semantic
embeddings, verified-history ingestion, and the resident service lifecycle are
subsequent increments; the current retrieval index is in-memory lexical search.

## Development

```sh
go test ./...
```

## License

AGPL-3.0-or-later
