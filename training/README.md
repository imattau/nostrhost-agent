# Training pilot tools

These scripts support the separate, synthetic VM-lab pilot described in [`../docs/training-regime.md`](../docs/training-regime.md). They do not collect production traces, execute operations, or make an adapter eligible for deployment.

The collection target is defined in [`acquisition-plan.json`](acquisition-plan.json): 500 independent evidence episodes for an exploratory pilot, then 1,000 total with at least 200 independent held-out episodes before considering promotion. Count evidence episodes, not paraphrases. Use `audit_dataset.py` to validate schemas and detect split leakage before training:

```sh
python3 training/audit_dataset.py \
  /tmp/nostrhost-agent-current/lab/current-nostrhost-pilot.jsonl
```

Add `--enforce-minimum` only after the promotion-sized dataset is assembled. The current run has 36 rows but only 22 independent evidence episodes, so it is far below the pilot floor.

The dataset builder reads signed and redacted exports from `/tmp/nostrhost-agent-current` and validates every row against `trace.schema.json` before writing JSONL. Keep the output, source captures, model weights, and adapter outside Git:

```sh
python3 training/build_vm_lab_dataset.py \
  --output /tmp/nostrhost-agent-current/lab/current-nostrhost-pilot.jsonl
```

With the isolated training environment and pinned base weights available, run the one-batch memory/serialization smoke test first, then the short pilot:

```sh
/tmp/nostrhost-agent-training-venv/bin/python training/train_vm_lab_lora.py \
  --lab-pilot \
  --dataset /tmp/nostrhost-agent-current/lab/current-nostrhost-pilot.jsonl \
  --output-dir /tmp/nostrhost-agent-current/lora-smoke \
  --smoke-only

/tmp/nostrhost-agent-training-venv/bin/python training/train_vm_lab_lora.py \
  --lab-pilot \
  --dataset /tmp/nostrhost-agent-current/lab/current-nostrhost-pilot.jsonl \
  --output-dir /tmp/nostrhost-agent-current/lora-run \
  --epochs 3
```

Run the deterministic comparison without dispatching any proposals:

```sh
/tmp/nostrhost-agent-training-venv/bin/python training/evaluate_vm_lab_lora.py \
  --lab-dataset /tmp/nostrhost-agent-current/lab/current-nostrhost-pilot.jsonl \
  --schema-registry /tmp/nostrhost-agent-current/agent-operation-schemas.json \
  --frozen-suite evaluation/model_cases.json \
  --adapter-dir /tmp/nostrhost-agent-current/lora-run/adapter \
  --output /tmp/nostrhost-agent-current/lora-run/evaluation.json
```

The trainer refuses non-lab rows, non-synthetic provenance, and positive operations outside the verified operations registered in the acquisition plan. The evaluator keeps `lab_test` out of training and model selection and also checks the frozen regression suite. The run record and rejection rationale are in [`experiment-2026-09-12-current-nostrhost.md`](experiment-2026-09-12-current-nostrhost.md). A passing script run is not a promotion decision; use the gates and current rejection in the training-regime document.

## Labeling contributions (production bridge)

[`label_candidate.py`](label_candidate.py) converts a
`nostrhost-agent-contribution/v1` candidate into a `trace.schema.json` row with
a conservative, evidence-grounded heuristic label: a no-call is labeled from an
observation showing the target healthy or genuinely ambiguous, and a positive
operation is labeled only when the candidate records a matching proposal whose
outcome is `verified`. Approval-gated, unregistered, or unverified proposals are
never promoted to a positive label; an unverified positive is refused because the
trace contract forbids a positive target without verification. Rows stay
non-trainable (`split: "excluded"`) while the candidate still carries
`privacy_review_required: true`; `--assume-redacted` is only for an operator who
asserts client-side redaction is sufficient.

```sh
python3 training/label_candidate.py candidate-*.json --output /tmp/labeled.jsonl
python3 -m pytest training/test_label_candidate.py -q
```

With the current contribution corpus this yields zero trainable rows: all seven
canonical candidates are `proposal_only` with `verified: null`. See
[`validation-2026-09-18-rejected-adapter.md`](validation-2026-09-18-rejected-adapter.md).
