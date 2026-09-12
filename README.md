# nostrhost-agent

The native local administrator for NostrHost. The model is an unprivileged
planner; this module owns typed operations, autonomy policy, execution
boundaries, and auditable administration traces.

The current increment establishes the deterministic policy boundary, a local
OpenAI-compatible planner, and the NostrHost operation relay adapter. Machine
changes still run through NostrHost's authoritative operation daemon; all
agent-side effects are behind typed interfaces. `NewResidentRuntime` wires the
local planner, Nostr operation observer/executor, optional knowledge corpus,
fresh-read verification rules, audit journal, and cycle service into one owned
lifecycle.

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
- Verify configured actions by repeating a registered read-only operation and
  comparing a selected result field against the expected value.
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
disable audit. The NostrHost operation daemon remains authoritative for
per-operation authorization, request-bound signed owner approval, and machine
changes. The agent's loopback relay adapter uses the agent identity for NIP-42
authentication. Approval-gated requests are audited locally before dispatch;
the agent waits for the daemon's correlated result after approval and
execution. The local planner only emits typed proposals and cannot execute
operations. The current index is rebuilt in memory from an operator-managed
local corpus and uses lexical search; semantic embeddings and verified-history
ingestion are subsequent increments. Operators provide service-manager
packaging and configure operation-specific verification rules.

## Development

```sh
go test ./...
go build ./cmd/nostrhost-agent
```

The daemon accepts a strict JSON config through `--config` (default
`/etc/nostrhost-agent/config.json`). Store it with mode `0600`; the loader
rejects symlinks, group/other permissions, unknown fields, and files over
256 KiB. Intervals and relay result timeouts use Go duration strings such as
`"6h"` and `"90s"`. Observe mode does not require an inference endpoint. For
maintain/autonomous mode, configure a loopback inference endpoint and a fresh
read verification rule for each operation the agent may execute.
An observe-only starting point is provided in
[`examples/config.observe.example.json`](examples/config.observe.example.json);
replace both key placeholders before starting the daemon.

## License

AGPL-3.0-or-later
