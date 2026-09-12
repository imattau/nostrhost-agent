# NostrHost VM training experiment — 2026-09-12

## Decision

Reject this adapter. It matched all eight rows in the tiny VM-lab test split, but exact matches on the frozen 17-case regression suite fell from 9/17 for the base model to 3/17 for the adapter. The adapter made no unsafe or malformed proposals in that suite, but abstained on 15/17 cases, including useful diagnostics and explicitly requested operations. This is an over-abstaining model, not a usable improvement. Do not deploy or publish its weights.

## Test environment and evidence

Collection used the native Debian 12 `nostrhost-clean6` NostrHost VM (`192.168.122.37`), not the separate YunoHost MCP server. The configured package source was `https://imattau.github.io/nostrhost/debian/ bookworm main`. The VM reported `nostrhost 0.1.0`, `nostrhost-core 12.1.41.21`, and `python3-nostrhost 12.1.41.21`; `apt-get update` and `apt-get install nostrhost` confirmed the package was already current.

The run captured signed service-status requests for 14 native NostrHost services, read-only app/disk/system/package evidence, and one clean signed Caddy stop/status/restart/status cycle. Caddy and its test app routes returned HTTP 200 after recovery. One observed discrepancy remains intentionally unresolved: NostrHost's status view reported `nftables` failed/disabled while `systemctl --failed` reported no failed units. That state was recorded as evidence; it was not “fixed” speculatively. An earlier ops-daemon crash caused by the installed unit's logging write-path restriction was excluded from training; the VM was stabilized using the current unit already present in the working tree.

The builder produced 36 rows: 22 independent evidence episodes and 14 paraphrase/variant rows. Five independent evidence references were held out. Grouped splits contain 19 training, 9 validation, and 8 test rows. Category counts are 14 read-only diagnostics, 16 healthy no-ops, one ambiguous no-op, one low-risk recovery, three prompt-injection cases, and one unsupported operation. Domain coverage remains narrow; DNS/network is absent. There are zero production episodes, so this collection does not meet the 500-episode exploratory floor or 1,000-episode promotion candidate target.

## Training run

- Base: `Qwen/Qwen3.5-0.8B`, revision `2fc06364715b967f1860aea9cf38778875588b17`
- Adapter: LoRA rank 16, alpha 32, dropout 0.05, language-layer projections
- Training: 3 epochs, learning rate `1e-4`, effective batch 4, seed 42
- Hardware: NVIDIA GeForce RTX 5060 Ti
- Runtime: 61.65 seconds; training loss 0.3819
- Dataset SHA-256: `89055da1bbce5fbb5a833f0328443b167ef5df5c602acd198b48a4aad1aa86ac`
- System prompt SHA-256: `a9bf07d6a5858e554b370713bcb3ad76475721eddcc1eddcd56a0e6ebe899ccf`

The trainer emitted a GPU allocation warning while the run still completed and wrote the adapter. Treat that as a resource warning, not evidence of a clean training run on a larger corpus.

| Evaluation set | Base exact | Adapter exact | Base unsafe/malformed | Adapter unsafe/malformed |
| --- | ---: | ---: | ---: | ---: |
| VM lab test (8 rows) | 0/8 | 8/8 | 0 | 0 |
| Frozen regression (17 rows) | 9/17 | 3/17 | 2 | 0 |

The lab test result is too small and too closely related to this single VM collection to establish generalization. The frozen regression result is decisive for this review: the adapter suppresses risk by suppressing nearly all useful behavior. Do not tune against these already-used test sets; any next iteration needs newly collected, independently grouped evidence and a fresh held-out split.

## Reproduction artifacts

The source captures, generated JSONL, model files, adapter, and evaluation report remain outside Git under `/tmp/nostrhost-agent-current`. The dataset manifest, LoRA run manifest, and evaluation JSON are the detailed machine-readable records. Only this experiment summary and the training tooling are committed to the component repository.
