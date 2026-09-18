# Rejected-adapter validation — 2026-09-18

## Decision

**The rejection stands and is stronger than originally recorded.** The
2026-09-12 Qwen3.5-0.8B LoRA pilot was re-run end to end from the retained
artifacts. It reproduces the original metrics exactly, and this review corrects
the failure description: the adapter does not merely abstain on 15 of 17 frozen
cases — it predicts no-call on **17 of 17**. Its three "correct" frozen answers
are precisely the three cases whose expected label is no-call. Outside the narrow
distribution it memorized, the adapter is a near-constant no-call emitter.

The root cause is positive-operation coverage collapse, not a training bug: the
corpus demonstrates only two distinct positive operations (14× `service.status`,
1× `service.restart`), and 13 of those 14 share one identical trigger template,
while the frozen suite requires nine distinct positive operations, eight of which
never appear as a target anywhere in training.

## Reproduction

Environment: `transformers 5.13.0`, `peft 0.20.0`, `trl 1.13.0`, `torch 2.13.0+cu130`,
RTL 5060 Ti; base weights and training venv retained under `/tmp/nostrhost-agent-lab`
and `/tmp/nostrhost-agent-training-venv`.

Artifact integrity confirmed before running: dataset SHA-256
`89055da1bbce5fbb5a833f0328443b167ef5df5c602acd198b48a4aad1aa86ac` matches
`current-nostrhost-pilot.manifest.json` and the run manifest.

| Step | Recorded (2026-09-12) | Reproduced (2026-09-18) |
| --- | ---: | ---: |
| train_loss | 0.38188 | 0.38114 |
| train_runtime (s) | 61.65 | 60.50 |
| final eval_loss | — | 0.002012 |
| lab_test base / adapter exact | 0/8 / 8/8 | 0/8 / 8/8 |
| frozen base exact (unsafe) | 9/17 (2) | 9/17 (2) |
| frozen adapter exact (unsafe) | 3/17 (0) | 3/17 (0) |

The freeze-smoke test (finite loss, adapter save/reload) passed; trainable
parameters 10,523,136 / 863,509,056 (1.22%).

## Root cause

1. **Positive-operation coverage collapse.** Only two operations are ever a
   positive target:

   | Operation | positive rows | distinct trigger phrasings |
   | --- | ---: | ---: |
   | `service.status` | 14 | 14 (13 identical: "Read the current status of the managed service X.") |
   | `service.restart` | 1 | 1 |

   The frozen suite requires nine positive operations. Eight of them are never
   demonstrated as a target: `app.health`, `app.logs`, `backup.create`,
   `diagnosis.run`, `disk.status`, `package.updates`, `package.upgrade`,
   `system.health`. The model cannot emit an operation it was never taught to
   emit, so it falls back to its most frequent completion, the no-call string.

2. **Memorization, not generalization.** Final eval_loss `0.002` on 9 validation
   rows: the adapter fits the tiny split almost perfectly, which is why lab_test
   is 8/8 while the untouched frozen suite collapses. The lab_test split is only
   5 held-out evidence sources and shares nearly all surface forms with train.

3. **Class imbalance reinforces abstention.** Even at the row level, no-call
   (21 rows) outnumbers the positive targets (15 rows), and the positive targets
   collapse to a single template, so the cheapest loss-minimizing strategy is to
   abstain unless the exact `service.*` phrasing appears.

4. **The "safe" result is vacuous.** The adapter's `unsafe_or_malformed = 0` on
   the frozen suite is achieved only because it proposes nothing. There is no
   evidence it would avoid harm while acting.

## Findings beyond the adapter

- **The frozen regression suite was not actually frozen.** Commit `7a351bf`
  (2026-09-16) regenerated the operation registry and edited
  `evaluation/model_cases.json`: `system.health` became `system.status`, and it
  added `service.history`, `app.upgrade`, `backup.restore`, `firewall.open`, and
  `updates.check`. The evaluator can no longer even load the current suite
  against the 2026-09-12 registry (`ValueError: unknown operations
  ['system.status']`). The recorded baseline is only reproducible with the suite
  as of `61b3bc0`, recovered via
  `git show 61b3bc0:evaluation/model_cases.json`. `evaluation/model_cases-v1.json`
  is stale for the same reason.
- **Serving-format assumption is unverified.** Production `OpenAICompatiblePlanner`
  consumes structured `choices[].message.tool_calls[].function` (see
  `agent/llm_planner.go`), while the trainer and evaluator serialize and parse
  the raw Qwen `<tool_call><function=…>` text via `apply_chat_template`.
  Training-regime §Training step 2 requires proving the served model emits and
  parses identically; that check was never recorded. No-call as bare text
  ("No operation is warranted.") maps to an empty `tool_calls` array, which is
  consistent, but the positive path is still an assumption.
- **The collection flywheel produces no trainable rows yet.** All seven
  canonical contribution candidates on Hugging Face
  (`0xx0lostcause0xx0/nostrhost-agent`) are `outcome: proposal_only` with
  `verified: null` (six propose `service.status`, one is no-call),
  `expected_decision: null`, and `privacy_review_required: true`. A new
  evidence-grounded labeler (`training/label_candidate.py`) refuses six and
  quarantines the seventh:

  | Mode | trainable | excluded | refused |
  | --- | ---: | ---: | ---: |
  | default (privacy review required) | 0 | 1 | 6 |
  | `--assume-redacted` | 1 | 0 | 6 |

  The six refusals are positive proposals with no host-verified fresh read; the
  trace contract (`trace.schema.json`) forbids a positive target without
  verification, so they cannot be represented at all. Automated/heuristic
  labeling therefore cannot currently produce a training row from real
  contributions.

## What a retry would require (none attempted here)

A retry on the current corpus would fail for the same reason; the fix is data,
not hyperparameters. Minimum changes before the next run:

1. Broad positive-operation coverage: every allowlisted operation must appear as
   a positive target with several distinct trigger phrasings and observation
   shapes, not one template.
2. Independent episodes per operation, grouped by incident family, with a
   genuinely frozen held-out suite (pin the exact `model_cases.json` revision
   and stop editing it).
3. Evidence-carrying contributions: the candidate must record the host's
   fresh-read verification so the labeler can accept a positive; extend
   `ContributionCandidate` with a verification/evidence field or a matching
   `outcome.proposals[].verified` set by the executor.
4. The trusted owner-intent field before any approval-gated positive is taught.

None of this is satisfied by the current 22-episode synthetic corpus or the
seven unverified production candidates, which remain far below the 500-episode
pilot floor in `training/acquisition-plan.json`.

## Reproduce

```sh
VENV=/tmp/nostrhost-agent-training-venv/bin/python
CUR=/tmp/nostrhost-agent-current
cd libs/nostrhost-agent

$VENV training/audit_dataset.py "$CUR/lab/current-nostrhost-pilot.jsonl"

$VENV training/train_vm_lab_lora.py --lab-pilot \
  --dataset "$CUR/lab/current-nostrhost-pilot.jsonl" \
  --output-dir /tmp/opencode/repro/lora-run --epochs 3

git show 61b3bc0:evaluation/model_cases.json > /tmp/opencode/model_cases-0912.json
$VENV training/evaluate_vm_lab_lora.py \
  --lab-dataset "$CUR/lab/current-nostrhost-pilot.jsonl" \
  --schema-registry "$CUR/lab/agent-operation-schemas.json" \
  --frozen-suite /tmp/opencode/model_cases-0912.json \
  --adapter-dir /tmp/opencode/repro/lora-run/adapter \
  --output /tmp/opencode/repro/evaluation-0912suite.json

$VENV -m pytest training/test_label_candidate.py -q

$VENV training/label_candidate.py /tmp/opencode/hf-candidates/*.json \
  --output /tmp/opencode/labeled.jsonl
```

Raw captures, adapter weights, and evaluation JSON remain outside Git under
`/tmp`. The adapter stays undeployed.
