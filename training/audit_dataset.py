#!/usr/bin/env python3
"""Validate trace rows and audit independent episodes, category mix, and leakage."""
from __future__ import annotations

import argparse
import hashlib
import json
from collections import Counter, defaultdict
from pathlib import Path
from typing import Any

import jsonschema


def stable_hash(value: Any) -> str:
    raw = json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
    return hashlib.sha256(raw.encode()).hexdigest()


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("dataset", type=Path)
    ap.add_argument("--schema", type=Path, default=Path(__file__).with_name("trace.schema.json"))
    ap.add_argument("--plan", type=Path, default=Path(__file__).with_name("acquisition-plan.json"))
    ap.add_argument("--enforce-minimum", action="store_true", help="fail unless the promotion candidate floor and holdout floor are met")
    args = ap.parse_args()

    schema = json.loads(args.schema.read_text())
    plan = json.loads(args.plan.read_text())
    validator = jsonschema.Draft202012Validator(schema)
    rows = [json.loads(line) for line in args.dataset.read_text().splitlines() if line.strip()]
    if not rows:
        raise SystemExit("empty dataset")
    seen_ids: set[str] = set()
    family_splits: dict[str, set[str]] = defaultdict(set)
    source_splits: dict[str, set[str]] = defaultdict(set)
    input_splits: dict[str, set[str]] = defaultdict(set)
    source_families: dict[str, set[str]] = defaultdict(set)
    source_variants: Counter[str] = Counter()
    categories: Counter[str] = Counter()
    domains: Counter[str] = Counter()
    splits: Counter[str] = Counter()
    production_sources: set[str] = set()
    lab_sources: set[str] = set()
    errors: list[str] = []

    for i, row in enumerate(rows, 1):
        for error in validator.iter_errors(row):
            errors.append(f"row {i} {'.'.join(map(str, error.absolute_path))}: {error.message}")
        rid = row.get("id")
        if rid in seen_ids:
            errors.append(f"duplicate row id: {rid}")
        seen_ids.add(rid)
        family, split = row.get("incident_family"), row.get("split")
        source = row.get("provenance", {}).get("source_ref")
        family_splits[family].add(split)
        source_splits[source].add(split)
        source_families[source].add(family)
        source_variants[source] += 1
        input_splits[stable_hash(row.get("planner_input"))].add(split)
        categories[row.get("category")] += 1
        domains[row.get("domain")] += 1
        splits[split] += 1
        if row.get("provenance", {}).get("synthetic"):
            lab_sources.add(source)
        else:
            production_sources.add(source)

    for family, family_split_set in family_splits.items():
        if len(family_split_set) > 1:
            errors.append(f"incident family crosses splits: {family} -> {sorted(family_split_set)}")
    for source, source_split_set in source_splits.items():
        if len(source_split_set) > 1:
            errors.append(f"evidence source crosses splits: {source} -> {sorted(source_split_set)}")
    for input_hash, input_split_set in input_splits.items():
        if len(input_split_set) > 1:
            errors.append(f"identical planner input leaks across splits: {input_hash}")
    for source, families in source_families.items():
        if len(families) > 1:
            errors.append(f"one evidence source was assigned multiple incident families: {source}")

    independent = len(production_sources | lab_sources)
    heldout = len({r["provenance"]["source_ref"] for r in rows if r["split"] in {"test", "lab_test"}})
    production_independent = len(production_sources)
    category_deficits = {
        name: max(0, target - categories.get(name, 0))
        for name, target in plan["category_targets_at_1000"].items()
    }
    domain_deficits = {
        name: max(0, target - domains.get(name, 0))
        for name, target in plan["domain_coverage_targets_at_1000"].items()
    }
    summary = {
        "rows": len(rows),
        "independent_evidence_episodes": independent,
        "production_evidence_episodes": production_independent,
        "synthetic_lab_evidence_episodes": len(lab_sources),
        "heldout_evidence_episodes": heldout,
        "category_counts": dict(categories),
        "category_target_deficits_at_1000": category_deficits,
        "domain_counts": dict(domains),
        "domain_target_deficits_at_1000": domain_deficits,
        "split_row_counts": dict(splits),
        "incident_families": len(family_splits),
        "prompt_variant_rows": sum(n - 1 for n in source_variants.values() if n > 1),
        "pilot_floor_met": independent >= plan["pilot_floor"],
        "promotion_candidate_floor_met": independent >= plan["promotion_candidate_floor"],
        "promotion_heldout_floor_met": heldout >= plan["promotion_test_floor"],
        "leakage_or_schema_errors": errors,
    }
    print(json.dumps(summary, indent=2))
    if errors:
        raise SystemExit(2)
    if args.enforce_minimum and not (summary["promotion_candidate_floor_met"] and summary["promotion_heldout_floor_met"]):
        raise SystemExit(3)


if __name__ == "__main__":
    main()
