# Training pilot tools

These scripts support the separate, synthetic VM-lab pilot described in [`../docs/training-regime.md`](../docs/training-regime.md). They do not collect production traces, execute operations, or make an adapter eligible for deployment.

The dataset builder reads the signed and redacted exports from `/tmp/nostrhost-agent-lab` and validates every row against `trace.schema.json` before writing JSONL. Keep the output, source captures, model weights, and adapter outside Git:

```sh
python3 training/build_vm_lab_dataset.py \
  --output /tmp/nostrhost-agent-lab/vm-lab-pilot.jsonl
```

With the isolated training environment and pinned base weights available, run the one-batch memory/serialization smoke test first, then the short pilot:

```sh
/tmp/nostrhost-agent-training-venv/bin/python training/train_vm_lab_lora.py \
  --lab-pilot \
  --dataset /tmp/nostrhost-agent-lab/vm-lab-pilot.jsonl \
  --output-dir /tmp/nostrhost-agent-lab/lora-smoke \
  --smoke-only

/tmp/nostrhost-agent-training-venv/bin/python training/train_vm_lab_lora.py \
  --lab-pilot \
  --dataset /tmp/nostrhost-agent-lab/vm-lab-pilot.jsonl \
  --output-dir /tmp/nostrhost-agent-lab/lora-run \
  --epochs 3
```

Run the deterministic comparison without dispatching any proposals:

```sh
/tmp/nostrhost-agent-training-venv/bin/python training/evaluate_vm_lab_lora.py \
  --lab-dataset /tmp/nostrhost-agent-lab/vm-lab-pilot.jsonl \
  --schema-registry /tmp/nostrhost-agent-lab/agent-operation-schemas.json \
  --frozen-suite evaluation/model_cases.json \
  --adapter-dir /tmp/nostrhost-agent-lab/lora-run/adapter \
  --output /tmp/nostrhost-agent-lab/lora-run/evaluation.json
```

The trainer refuses non-lab rows, non-synthetic provenance, and positive operations outside the verified `service.status`/`service.restart` subset. The evaluator keeps `lab_test` out of training and model selection and also checks the frozen regression suite. A passing script run is not a promotion decision; use the gates and current rejection in the training-regime document.
