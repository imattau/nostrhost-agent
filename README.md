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
- Subscribe to recent NostrHost system, service, backup, and security notices;
  only server-signed warning/critical events become fixed-vocabulary triggers,
  and free-form notice summaries are discarded.
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
- Retrieve relevant examples from recent audit history only when an operation
  completed and passed its configured fresh-read verification; the live
  journal is re-read for each cycle, and sensitive fields are redacted.
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
local corpus and the latest positively verified operation traces, using
lexical search by default. Assist, maintain, and autonomous modes may also use
a separate local OpenAI-compatible embeddings endpoint for semantic ranking;
the lexical ranking remains the fallback if that endpoint is unavailable. The
semantic pass considers at most 512 documents per cycle and caches their
vectors in memory. Embedding failures fall back to lexical ranking with
exponential retry backoff. Configure the optional settings like this:

```json
"embeddings": {
  "base_url": "http://127.0.0.1:8081/v1",
  "model": "local-embedding-model"
}
```

For llama.cpp, use a dedicated embedding model with embedding mode and a
non-`none` pooling type, then point these settings at its loopback server.
Operators provide
service-manager packaging and configure operation-specific verification rules.

## Offline community contribution review

Operators can prepare one explicitly selected, completed planner cycle as a
local review candidate. The exporter reads the audit journal read-only, keeps
the exact model-visible operation schemas, applies conservative redaction, and
writes a new mode-`0600` JSON file. It has no upload command, network client, or
training credentials. The candidate is not a training example: the model's
proposal is labeled as observed behavior and `expected_decision` remains null
until a human reviewer supplies an independently supported label.

```sh
go run ./cmd/nostrhost-agent-export \
  --journal /var/lib/nostrhost-agent/audit.jsonl \
  --cycle-id CYCLE_ID \
  --output ./review-candidate.json
```

Review the candidate locally before sharing it. Redaction is best-effort and
cannot guarantee that a host-specific identifier or sensitive detail was
removed; edit or discard the file if anything looks identifying. The exporter
refuses insecure journal permissions, symlinks, incomplete/corrupt journals,
unfinalized cycles, and existing output paths. Only after review should a
maintainer convert a candidate to the versioned training trace format in
[`training/trace.schema.json`](training/trace.schema.json), assign provenance
and split, and accept it into a dataset. Community collection is opt-in and
offline by default; no operator data is automatically sent to the project.

## Development

```sh
go test ./...
go build ./cmd/nostrhost-agent
go build ./cmd/nostrhost-agent-export
```

Run the deterministic recovery and safety evaluation with
`go test ./agent -run '^TestFaultEvaluation$' -v`; see
[`docs/fault-evaluation.md`](docs/fault-evaluation.md) for its scope and
current scenarios.

The planner-only local model selection suite is available through
`go run ./cmd/nostrhost-agent-eval --model <local-model-id>`; candidate models,
hardware constraints, and measurement rules are in
[`docs/model-selection.md`](docs/model-selection.md).

The daemon accepts a strict JSON config through `--config` (default
`/etc/nostrhost-agent/config.json`). Store it with mode `0600`; the loader
rejects symlinks, group/other permissions, unknown fields, and files over
256 KiB. Intervals and relay result timeouts use Go duration strings such as
`"6h"` and `"90s"`. Observe mode does not require an inference endpoint. For
maintain/autonomous mode, configure a loopback inference endpoint and a fresh
read verification rule for each operation the agent may execute.
An observe-only starting point is provided in
[`examples/config.observe.example.json`](examples/config.observe.example.json);
replace both key placeholders before starting the daemon. That example listens
for notice events from the trusted server identity; set `listen_for_events` to
`false` to use periodic maintenance only. `event_lookback` controls the recent
history requested at startup (default `5m`, maximum `24h`). Notice content is
reduced to a fixed event-kind trigger and an optional validated target; the
summary and other free-form fields are never sent to the planner.

## License

AGPL-3.0-or-later
