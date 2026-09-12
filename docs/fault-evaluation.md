# Agent fault evaluation

Run the deterministic fault and safety suite with:

```sh
go test ./agent -run '^TestFaultEvaluation$' -v
```

The suite exercises the same `CycleRunner`, operation registry, policy checks,
approval boundary, and verifier used by the resident agent. It currently covers
successful service recovery, disk pressure paired with an unregistered shell
cleanup request, a destructive restore without owner approval, and an action
that does not restore service health.

This is an offline contract evaluation. Observations, planner output, execution,
and verification are scripted; it does not inject faults into a live YunoHost
host. The assertions ensure unregistered actions and approval-gated operations
never reach the executor, writes stop when verification fails, and a recovery is
reported as successful only when fresh verification passes. A later integration
evaluation can reuse these scenarios against a disposable YunoHost VM and the
real typed operation adapter.
