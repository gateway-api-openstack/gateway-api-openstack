# Architecture decision records

Architecture decision records (ADRs) record decisions that change the project's public or safety contract or are expensive to reverse.
Each record explains the problem, alternatives, chosen boundary, and required evidence.
Issues and pull requests can carry the detailed discussion.
Acceptance records project approval, not implementation quality, release readiness, or upstream endorsement.

## When an ADR is required

Write an ADR before changing:

- the OpenStack ownership boundary or immutable identity
- the writer and resource mapping for a Gateway graph
- member mode, credentials, or provider capability semantics
- the controller name, module ownership, or API domain
- a public CRD, flag, annotation, report format, or compatibility policy
- shared mutation of an object owned by another controller or the operator
- certificate identity and lifecycle
- adoption, deletion recovery, or finalizer guarantees

A cleanup that preserves these contracts does not need an ADR.
When the boundary is unclear, state the design question before implementing it.

## Process

1. Open a design issue describing the problem, current behavior, constraints, and alternatives.
2. Copy [the template](0000-template.md) to the next available four-digit number and choose a descriptive name.
3. Open a pull request with status `Proposed` and link the design issue.
4. Collect implementation, operator, and upstream feedback, keeping the proposal open for at least five working days unless it corrects an obvious mistake or the owner explicitly approves a scoped exception.
5. Record the approver and set the status to `Accepted` when the decision and its safety, compatibility, and evidence requirements are clear.
6. Merge implementation separately and validate it against the accepted decision.

For the Phase 3 development change, the repository owner directed implementation followed by documentation and excluded Phase 2 migration compatibility.
ADRs 0001 and 0002 record that scoped approval without claiming a public review issue or independent consensus.
Their release evidence remains a separate gate.
ADR 0003 records the owner's subsequent repository transfer and explicit choice of the project organization's API domain.

Use one of these statuses:

- `Proposed` while the decision is under review
- `Accepted` after project approval
- `Rejected` when the proposal will not be pursued
- `Superseded` when a later ADR replaces it

Do not silently change an accepted decision to make its history look current.
Use a new ADR for a changed contract and link both records.
Clarifications and implementation evidence may be added without changing the original decision.

## Index

| ADR | Status | Decision |
| --- | --- | --- |
| [0001](0001-gateway-graph-writer.md) | Accepted | Use one writer and durable route fragments for each Gateway graph. |
| [0002](0002-class-configuration.md) | Accepted, identity superseded by 0003 | Define class configuration, frozen Gateway topology, and worker NodePort eligibility. |
| [0003](0003-project-organization.md) | Accepted | Use the project organization for the repository, Go module, API group, and default controller identity. |
