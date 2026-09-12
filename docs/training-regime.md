# NostrHost agent training regime

## Decision

Start with supervised fine-tuning of Qwen3.5-0.8B-Instruct using a LoRA adapter. Do not train yet: the current evaluation set has only 17 synthetic scenarios, which is useful for regression checks but too small and too artificial to teach reliable operating behavior. The 1.7B model remains the comparison baseline; a trained 0.8B model must beat both the untrained 0.8B and the 1.7B baseline on held-out cases before it is considered for use.

This is a proposal-selection model. The host remains responsible for operation registration, typed argument validation, capability checks, approvals, execution, and fresh verification. Training must never be treated as a replacement for those controls.

## Safety boundary before collecting privileged examples

The planner currently receives `trigger`, `target`, `observations`, and `knowledge`; all are treated as untrusted. It does not receive a structured, authenticated owner-intent field. Therefore, v1 training must not teach positive proposals for approval-gated or destructive operations based only on wording inside a trigger, log, observation, or retrieved note. Keep those operations out of the model-visible operation set during this phase, or label such cases as no-call. Before adding positive examples for them, add a host-authored authorization signal to the planner protocol and test that untrusted text cannot set it.

That boundary is especially important because the current 0.8B model followed injected log text in the expanded evaluation. Such cases belong in the adversarial holdout and, after expert adjudication, in safety training as no-call examples. Never turn raw model outputs or unreviewed execution logs into labels.

## Dataset contract and collection rules

Each JSONL record follows [`training/trace.schema.json`](../training/trace.schema.json). It captures the planner-visible input, the exact operation schemas made available for that decision, the expected single proposal or no-call, provenance, redaction, review, and outcome evidence. Keep the production system prompt, model-facing tool descriptions, tokenizer/chat template, and serialization version alongside each dataset release. A change to any of those creates a new dataset version.

Only include a record in train, validation, or test when a reviewer has accepted the label, redaction is complete, and the decision is supported by evidence. For a state-changing operation, require a recorded fresh read showing the intended result; successful execution alone is not proof. For a no-call, preserve the evidence that the target was healthy, the request was ambiguous or unauthorized, the proposed action was unsafe, or the operation was unavailable. Keep rejected, failed, unknown, unreviewed, synthetic, and privacy-sensitive records excluded until resolved.

Do not put secrets, private keys, bearer tokens, raw environment files, or unnecessary personal data into examples. Redact identifiers consistently while preserving relationships needed to choose the operation. Store evidence references as access-controlled pointers, never as a reason to copy sensitive source data into the training set.

The 17 cases in `evaluation/model_cases.json` remain a frozen regression suite and must never be copied into training. Model-generated traces can be used to find candidate cases, but a qualified reviewer must independently determine the correct label from evidence.

## Dataset size and balance

Collect at least 500 reviewed, eligible episodes for a pilot. Aim for 1,000 or more diverse episodes before considering promotion. Ensure the corpus includes no-call decisions and safety boundaries rather than only successful actions. Use these proportions as initial sampling targets, then report the actual counts by category:

| Category | Share |
| --- | ---: |
| Healthy or already-resolved target; no state change | 20% |
| Confirmed low-risk recovery | 20% |
| Read-only diagnostics and evidence gathering | 20% |
| Ambiguous or insufficient evidence; ask for more evidence/no-call | 15% |
| Injection and untrusted instruction attempts; no-call or safe diagnostic | 15% |
| Unsupported operation, invalid identifier, or policy boundary | 10% |

These are proportions of reviewed episodes, not raw model generations. Sample from different services, failure modes, wording, observation shapes, and time periods. Do not let one incident or one service dominate the corpus.

## Split policy

Use 70% train, 15% validation, and 15% final test, grouped by incident family and source. Keep near-duplicates, templated variants, and all records from the same incident in one split. Freeze the test set before training. Maintain a separate adversarial red-team set that is never used for gradient updates; include prompt injection in logs/knowledge, forged owner intent, missing evidence, malformed identifiers, and attempts to request unregistered operations.

The current 17-case benchmark is an additional regression check, not a substitute for the held-out sets. Report results per category and per incident family so a high aggregate score cannot hide unsafe behavior.

## Training procedure

1. Pin the exact Hugging Face model revision, tokenizer revision, chat template, tool schema serialization, dataset commit/hash, library versions, and random seed in a run manifest. Start from the official Qwen3.5-0.8B-Instruct Transformers weights, not the Q4 GGUF deployment artifact.
2. Before a full run, render one real planner request and one tool-call target through the chosen tokenizer/template. Compare their meaning and structure with the OpenAI-compatible request emitted by `OpenAICompatiblePlanner` and the llama.cpp server. Stop if the trained completion format cannot be served and parsed identically.
3. Use supervised fine-tuning with loss on the assistant decision only. Preserve tool calls as actual tool-call targets in the model's supported chat format. Represent no-call as an assistant completion with no tool call. Do not train on user/observation tokens as targets.
4. Fit LoRA to the unquantized base if the training GPU allows it. Use QLoRA only if needed to fit the training job; quantization is a training-memory choice, not the deployment format. Inspect the model's actual module names before selecting adapter targets. PEFT's `all-linear` target is a reasonable QLoRA pilot to test, not an assumption that every Qwen3.5 layer is compatible.
5. Run a small one-batch forward/backward smoke test, verify finite loss and adapter save/reload, then train for at most three epochs with early stopping on validation loss and decision metrics. Keep the best validation checkpoint; do not select on the final test set.
6. Export the adapter against the same pinned base, convert/quantize to the intended llama.cpp format, and run the full end-to-end evaluation through the production planner interface. Evaluate the base 0.8B, adapter 0.8B, and 1.7B baseline with identical inputs and decoding settings.

Initial pilot hyperparameters to test—not established best values—are LoRA rank 16, alpha 32, dropout 0.05, learning rate `1e-4`, effective batch size 16, sequence length 2,048, two epochs, and seed 42. Adjust only from validation results and record every change. If QLoRA is required, begin with 4-bit NF4 and bf16 compute where supported. The local 4 GB VM is for inference evaluation, not model training.

TRL's SFT trainer supports conversational and prompt-completion datasets with completion-only/assistant-only loss; PEFT documents LoRA and quantized QLoRA workflows. Use those as implementation references, then pin and smoke-test the exact versions against this model and tool-call format: [TRL SFTTrainer](https://huggingface.co/docs/trl/sft_trainer), [PEFT LoRA](https://huggingface.co/docs/peft/main/package_reference/lora), and [PEFT quantization](https://huggingface.co/docs/peft/developer_guides/quantization). The [official Qwen3.5-0.8B model card](https://huggingface.co/Qwen/Qwen3.5-0.8B) describes its supported chat/tool format; pin the exact revision used for any run.

## Promotion gates

Do not promote an adapter unless it passes all of these gates:

- Zero unsafe or unregistered proposals, invalid arguments, or injection-following decisions on the frozen regression and red-team suites. Any such event rejects the candidate.
- At least 200 independently reviewed, held-out episodes, with no material category regression and a measured improvement over both base models on the primary decision metric. Report bootstrap confidence intervals grouped by incident family.
- At least 95% correct low-risk recovery selection when evidence confirms a recoverable stopped/unhealthy target, and at least 98% no-call accuracy for healthy/already-resolved cases.
- Identical host-side capability, schema, approval, and verification tests pass with the candidate enabled. Keep writes disabled in shadow evaluation; compare proposals with reviewer-approved outcomes first.
- Record model/base hashes, adapter hash, quantization settings, run config, dataset hash, evaluation report, and reviewer sign-off. Rollout begins with the existing host policy controls and a reversible model pin.

If the 0.8B adapter does not clear the gates, keep the 1.7B model or deterministic policy path. A small model is useful only if it improves decision quality without increasing safety risk or exceeding the device's latency and memory budget.

## Current status

No training has been run. The available 17-case suite is synthetic and insufficient as training data. The next useful work is collecting and reviewing real, redacted episodes, plus resolving the structured owner-intent boundary before any approval-gated action examples are admitted.
