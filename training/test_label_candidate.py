"""Tests for the contribution-candidate -> training-trace heuristic labeler."""
from __future__ import annotations

import json
from pathlib import Path

import jsonschema

from label_candidate import build_row, derive_label

SCHEMA = json.loads((Path(__file__).with_name("trace.schema.json")).read_text())
OP_STATUS = {"name": "service.status", "description": "read status", "args_schema": {"type": "object"}}
OP_FIREWALL = {"name": "firewall.open", "description": "open port", "args_schema": {"type": "object"}}


def candidate(*, no_call: bool, operation: str | None = None, observations=None,
              operations=None, proposals=None) -> dict:
    decision = {"no_call": no_call, "policy": "proposal_only"}
    if operation:
        decision["operation"] = operation
        decision["arguments"] = {"name": "web"}
    return {
        "schema_version": "nostrhost-agent-contribution/v1",
        "candidate_id": "candidate-abcdef0123456789abcd",
        "source_ref": "sha256:" + "a" * 64,
        "review_status": "submitted",
        "privacy_review_required": True,
        "redactions_applied": 1,
        "planner_input": {
            "trigger": "scheduled_maintenance",
            "observations": observations or {},
            "available_operations": operations if operations is not None else [OP_STATUS],
        },
        "observed_decision": decision,
        "outcome": {"cycle_result": "completed", "proposals": proposals or []},
    }


def test_healthy_noop_is_evidence_backed():
    label = derive_label(candidate(no_call=True,
                                   observations={"service.status": {"web": {"status": "running"}}}))
    assert label["category"] == "healthy_noop"
    assert label["no_call"] is True
    assert label["verified"] is True


def test_no_observations_is_not_verified():
    label = derive_label(candidate(no_call=True, observations={}))
    assert label["verified"] is False
    row, status = build_row(candidate(no_call=True, observations={}), SCHEMA, assume_redacted=True)
    assert row is None or row["split"] == "excluded"


def test_unverified_positive_is_refused():
    c = candidate(no_call=False, operation="service.status")
    row, reason = build_row(c, SCHEMA, assume_redacted=True)
    assert row is None
    assert "verified fresh read" in reason


def test_verified_positive_becomes_trainable():
    proposals = [{"operation": "service.status", "outcome": "verified", "verified": True}]
    c = candidate(no_call=False, operation="service.status", proposals=proposals)
    row, status = build_row(c, SCHEMA, assume_redacted=True)
    assert row is not None
    assert row["split"] in {"train", "validation", "test"}
    assert row["review"]["decision_verified"] is True
    assert row["verification"]["execution_status"] == "verified_success"
    jsonschema.Draft202012Validator(SCHEMA).validate(row)


def test_privacy_review_required_blocks_training():
    proposals = [{"operation": "service.status", "outcome": "verified", "verified": True}]
    c = candidate(no_call=False, operation="service.status", proposals=proposals)
    row, _ = build_row(c, SCHEMA, assume_redacted=False)
    assert row is not None
    assert row["split"] == "excluded"


def test_approval_gated_positive_is_policy_boundary():
    c = candidate(no_call=False, operation="firewall.open",
                  operations=[OP_FIREWALL],
                  observations={"service.status": {"web": {"status": "running"}}})
    label = derive_label(c)
    assert label["category"] == "policy_boundary"
    assert label["no_call"] is True
    assert label["verified"] is False


def test_unregistered_positive_is_unsupported():
    c = candidate(no_call=False, operation="service.status", operations=[OP_FIREWALL])
    label = derive_label(c)
    assert label["category"] == "unsupported_operation"
    assert label["no_call"] is True
