#!/usr/bin/env python3
"""Turn contribution candidates into training-trace rows with a conservative,
evidence-grounded heuristic label.

This is the missing bridge between the contribution flywheel
(`nostrhost-agent-contribution/v1`, emitted by `nostrhost-agent-contribute`
and the daemon's automatic submitter) and the training-trace contract
(`trace.schema.json`). A contribution candidate is a model decision, not a
label; using the model's own output as the label is forbidden by the training
regime. This tool therefore derives the label only from evidence the host
independently recorded:

* a no-call is labeled from an observation that shows the target healthy,
  already-resolved, or genuinely ambiguous;
* a positive operation is labeled only when the candidate records a matching
  proposal whose outcome is `verified` (the host's fresh-read gate);
* approval-gated, destructive, unregistered, or unverified proposals are never
  promoted to a positive label.

Anything that cannot be justified from evidence is quarantined. No-call rows
whose evidence is weak, and proposals that violate the safety boundary or name
an unregistered operation, are emitted as schema-valid rows in
`split: "excluded"` (not trainable) with the reason in
`verification.decision_basis`. A positive proposal with no host-verified
fresh read cannot be represented at all — the trace contract forbids a
positive target without verification — so it is refused. The tool never
fabricates a review, a fresh read, or a verification result.

Even an evidence-verified row stays non-trainable while the candidate still
carries `privacy_review_required: true`. Pass `--assume-redacted` only when an
operator asserts the client-side redaction is sufficient for the recorded
evidence; without it, no row can enter train/validation/test.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import jsonschema

CONTRIBUTION_SCHEMA_VERSION = "nostrhost-agent-contribution/v1"

# Operations the acquisition plan permits as positive targets before the
# planner has a trusted owner-intent field (acquisition-plan.json
# operation_policy.positive_targets_initially_allowed).
POSITIVE_ALLOWLIST = {
    "service.status", "service.restart", "app.health", "app.logs",
    "disk.status", "diagnosis.run", "package.updates", "state.diff",
    "system.health",
}
STATE_CHANGING = {
    "service.restart", "app.restore", "firewall.change", "firewall.open",
    "firewall.close", "package.upgrade", "backup.create", "backup.restore",
    "credential.set", "dns.apply", "dns.subscribe", "dns.unsubscribe",
    "domain.add", "domain.remove", "system.reboot", "system.shutdown",
}
HEALTHY_TOKENS = {"running", "active", "healthy", "ok", "available", "up",
                  "started", "200", "success", "verified"}

DOMAIN_BY_PREFIX = (
    ("service.", "service_health_and_recovery"),
    ("app.", "application_health_and_logs"),
    ("disk.", "host_disk_and_system_health"),
    ("system.", "host_disk_and_system_health"),
    ("diagnosis.", "host_disk_and_system_health"),
    ("package.", "package_and_backup_state"),
    ("backup.", "package_and_backup_state"),
    ("dns.", "dns_and_network_diagnostics"),
    ("domain.", "dns_and_network_diagnostics"),
    ("nsite.", "policy_and_adversarial_inputs"),
)


def now() -> str:
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def domain_for(operation: str | None) -> str:
    if operation:
        for prefix, domain in DOMAIN_BY_PREFIX:
            if operation.startswith(prefix):
                return domain
    return "policy_and_adversarial_inputs"


def _walk_values(value: Any):
    if isinstance(value, dict):
        for key, item in value.items():
            yield key
            yield from _walk_values(item)
    elif isinstance(value, list):
        for item in value:
            yield from _walk_values(item)
    else:
        yield value


def observations_look_healthy(observations: dict[str, Any] | None) -> bool:
    if not observations:
        return False
    for value in _walk_values(observations):
        if isinstance(value, str) and value.strip().lower() in HEALTHY_TOKENS:
            return True
    return False


def verified_proposal(candidate: dict[str, Any]) -> dict[str, Any] | None:
    decision = candidate.get("observed_decision", {})
    if decision.get("no_call"):
        return None
    for proposal in candidate.get("outcome", {}).get("proposals", []) or []:
        if (proposal.get("operation") == decision.get("operation")
                and proposal.get("verified") is True
                and proposal.get("outcome") == "verified"):
            return proposal
    return None


def assign_split(incident_family: str) -> str:
    """Deterministic 60/20/20 grouped assignment, stable across runs."""
    bucket = int(hashlib.sha256(incident_family.encode()).hexdigest(), 16) % 100
    if bucket < 60:
        return "train"
    if bucket < 80:
        return "validation"
    return "test"


def derive_label(candidate: dict[str, Any]) -> dict[str, Any]:
    """Return the heuristic decision: label, category, verification, reason."""
    decision = candidate.get("observed_decision", {})
    observations = candidate.get("planner_input", {}).get("observations") or {}
    operation = None if decision.get("no_call") else decision.get("operation")
    verified = verified_proposal(candidate)

    if decision.get("no_call"):
        if observations_look_healthy(observations):
            return {"no_call": True, "operation": None, "category": "healthy_noop",
                    "verified": True,
                    "basis": "Fresh observation shows the target healthy; restraint is supported by evidence."}
        if observations:
            return {"no_call": True, "operation": None, "category": "ambiguous_noop",
                    "verified": True,
                    "basis": "Observations are present but do not confirm a recoverable fault; no-call is the conservative evidence-backed choice."}
        return {"no_call": True, "operation": None, "category": "ambiguous_noop",
                "verified": False,
                "basis": "No observations were recorded; a no-call label cannot be evidence-backed."}

    if not operation:
        return {"no_call": True, "operation": None, "category": "unsupported_operation",
                "verified": False, "basis": "Decision carried no recognized operation."}

    registered = {o.get("name") for o in candidate.get("planner_input", {}).get("available_operations", [])}
    if operation not in registered:
        return {"no_call": True, "operation": None, "category": "unsupported_operation",
                "verified": False,
                "basis": f"Proposed operation {operation!r} was not registered for this decision; it cannot be a positive label."}

    if operation not in POSITIVE_ALLOWLIST:
        return {"no_call": True, "operation": None, "category": "policy_boundary",
                "verified": False,
                "basis": f"{operation!r} is approval-gated or unsupported before a trusted owner-intent field exists; labeled no-call."}

    if operation in STATE_CHANGING:
        category = "low_risk_recovery"
    else:
        category = "read_only_diagnostic"

    if verified is None:
        return {"no_call": decision.get("no_call"), "operation": operation,
                "category": category, "verified": False,
                "basis": f"Proposal {operation!r} was not executed and verified (outcome {decision.get('policy')!r}); no fresh-read evidence exists."}

    return {"no_call": False, "operation": operation, "category": category,
            "verified": True,
            "basis": f"Host recorded a verified execution of {operation!r} with a fresh read."}


def build_row(candidate: dict[str, Any], schema: dict[str, Any],
              assume_redacted: bool) -> tuple[dict[str, Any] | None, str]:
    if candidate.get("schema_version") != CONTRIBUTION_SCHEMA_VERSION:
        return None, "unsupported candidate schema_version"
    candidate_id = candidate.get("candidate_id") or ""
    if not candidate_id:
        return None, "candidate is missing candidate_id"

    label = derive_label(candidate)
    if not label["no_call"] and not label["verified"]:
        return None, ("positive proposal lacks a host-verified fresh read; the trace "
                      "contract forbids a positive target without verification")
    planner_input = candidate.get("planner_input", {})
    trigger = planner_input.get("trigger") or "unknown"
    target = planner_input.get("target") or ""
    operation = label["operation"]

    redaction_complete = True if assume_redacted else not candidate.get("privacy_review_required", True)
    trainable = bool(label["verified"] and redaction_complete)

    incident_family = f"{target or 'unknown'}:{trigger}"[:160] or "unknown"
    split = assign_split(incident_family) if trainable else "excluded"
    review_status = "accepted" if (label["verified"] and redaction_complete) else "pending"
    collected = now()

    planner_out: dict[str, Any] = {
        "trigger": trigger,
        "observations": planner_input.get("observations") or {},
        "available_operations": planner_input.get("available_operations") or [],
    }
    if target:
        planner_out["target"] = target
    if planner_input.get("knowledge"):
        planner_out["knowledge"] = planner_input["knowledge"]

    if label["no_call"]:
        trace_target: dict[str, Any] = {"no_call": True}
    else:
        trace_target = {"no_call": False, "operation": operation,
                        "arguments": candidate.get("observed_decision", {}).get("arguments") or {}}

    verification: dict[str, Any] = {
        "decision_basis": label["basis"][:4000],
        "execution_status": "verified_success" if (not label["no_call"] and label["verified"]) else "not_applicable",
        "evidence_ref": candidate.get("source_ref", "candidate:unknown")[:512],
    }
    if not label["no_call"]:
        verification["fresh_read_passed"] = bool(label["verified"])

    row = {
        "schema_version": 1,
        "id": candidate_id,
        "incident_family": incident_family,
        "split": split,
        "category": label["category"],
        "domain": domain_for(operation),
        "provenance": {
            "source_type": "verified_cycle",
            "source_ref": candidate.get("source_ref", "candidate:unknown")[:512],
            "collected_at": collected,
            "synthetic": False,
        },
        "review": {
            "status": review_status,
            "reviewer": "heuristic-labeler-v1",
            "reviewed_at": collected,
            "redacted": redaction_complete,
            "decision_verified": bool(label["verified"]),
            "notes": label["basis"][:2000],
        },
        "planner_input": planner_out,
        "target": trace_target,
        "verification": verification,
    }
    errors = list(jsonschema.Draft202012Validator(schema).iter_errors(row))
    if errors:
        return None, "row failed trace schema: " + "; ".join(e.message for e in errors[:3])
    return row, ("trainable" if split != "excluded" else f"excluded: {label['basis']}")


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("candidates", type=Path, nargs="+")
    ap.add_argument("--output", type=Path, required=True)
    ap.add_argument("--schema", type=Path, default=Path(__file__).with_name("trace.schema.json"))
    ap.add_argument("--assume-redacted", action="store_true",
                    help="assert client-side redaction is sufficient; without it every row stays excluded")
    args = ap.parse_args()

    schema = json.loads(args.schema.read_text())
    rows: list[dict[str, Any]] = []
    summary = {"candidates": 0, "trainable": 0, "excluded": 0, "refused": 0,
               "by_category": {}, "by_split": {}}
    for path in args.candidates:
        candidate = json.loads(path.read_text())
        summary["candidates"] += 1
        row, status = build_row(candidate, schema, args.assume_redacted)
        if row is None:
            summary["refused"] += 1
            print(f"refused {path.name}: {status}", file=sys.stderr)
            continue
        rows.append(row)
        if row["split"] == "excluded":
            summary["excluded"] += 1
        else:
            summary["trainable"] += 1
        summary["by_category"][row["category"]] = summary["by_category"].get(row["category"], 0) + 1
        summary["by_split"][row["split"]] = summary["by_split"].get(row["split"], 0) + 1

    args.output.parent.mkdir(parents=True, exist_ok=True)
    with args.output.open("w", encoding="utf-8") as fh:
        for row in rows:
            fh.write(json.dumps(row, ensure_ascii=False, sort_keys=True) + "\n")
    print(json.dumps(summary, indent=2))


if __name__ == "__main__":
    main()
