# nostrhost-agent

The native local administrator for NostrHost. The model is an unprivileged
planner; this module owns typed operations, autonomy policy, execution
boundaries, and auditable administration traces.

The current increment establishes the deterministic policy boundary, a local
OpenAI-compatible planner, and the NostrHost operation relay adapter. Machine
changes still run through NostrHost's authoritative operation daemon; all
agent-side effects are behind typed interfaces. `NewResidentRuntime` wires the
local planner, Nostr operation observer/executor, optional knowledge corpus,
audit journal, and cycle service into one owned lifecycle.

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
- Run the cycle engine as a resident service with reactive triggers, optional
  periodic maintenance, sequential execution, and fail-closed shutdown.
- Build structured observations from explicitly selected read-only NostrHost
  operations; failed reads are represented without exposing adapter errors.
- Publish signed kind-2200 operation requests to the local relay and accept
  only correlated, signature-verified kind-2204 results from the configured
  server identity.
- Retrieve bounded lexical matches from an operator-selected local corpus;
  load and save that corpus as a private local JSON file, and record source
  document hashes in the administration trace.
- Require the executor to re-check authorization and validate operation
  arguments; a policy decision is not an execution credential.
- Record observations, proposals, policy outcomes, execution results, and
  verification in a structured cycle trace.
- Persist each cycle checkpoint to a private, append-only local JSONL journal;
  recover an incomplete final record after process interruption.
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
for per-operation authorization, request-bound signed owner approval, and
execution. Approval-gated requests are audited locally before dispatch and
wait for the daemon's correlated result. The local planner only emits
typed proposals and cannot execute operations. The current index is rebuilt in
memory from an operator-managed local corpus and uses lexical search; semantic
embeddings and verified-history ingestion are subsequent increments. Host
integrators provide the concrete observers, audit sink, approval verifier, and
service manager for their NostrHost installation.

## Development

```sh
go test ./...
```

## License

AGPL-3.0-or-later
