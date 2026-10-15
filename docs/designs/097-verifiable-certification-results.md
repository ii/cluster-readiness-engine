# ADR-097: Verifiable Certification Results

> **Status:** Ideation. Written in the ii fork to start a conversation. Not submitted upstream, not approved by the NVCRE maintainers, and not an issue-first proposal yet.

## Context

NVCRE calls its output a certification. The README says it "certifies GPU clusters before production workloads run on them" and is for teams that "bring up, validate, or resell GPU clusters". A certificate gets read by someone other than the person who ran it: a buyer, an auditor, a platform team two quarters later. That reader has three questions the report cannot answer today.

**What was the bar?** ADR-000 puts pass/fail criteria out of scope on purpose: NVCRE does not prescribe "specific burn-in workloads or pass/fail criteria. Those decisions belong to subject matter experts". That is the right boundary, and this record keeps it. The consequence is that the bar is whatever the operator set. [`first-certification.md:139`](../getting-started/first-certification.md?plain=1#L139) says it plainly: "Pass and fail are informational unless you set thresholds. No thresholds ship by default." The report does not record which bar was used. `CertReport` ([`pkg/report/report.go:53-68`](../../pkg/report/report.go#L53-L68)) carries name, platform, GPU, node counts, exclusions, categories, failed nodes and the result. It does not carry the NVCRE version, the catalog version, or the thresholds the categories were judged against. Two reports that both say `PASSED` may have been judged against different bars, or against none, and nothing in either file says which.

**Is this the file the run produced?** [ADR-090](090-component-result-sharding.md) (proposed) writes a results bundle with a `manifest.json` carrying a sha256 per file, and its reader refuses a bundle whose digests do not match. That catches corruption and partial writes, which is what it is for. It does not bind the bundle to whoever ran the certification. Anyone who can edit a file can edit the manifest. [ADR-074](074-supply-chain-attestation.md) makes the same point about release artifacts: "A checksum served from the same origin as the artifact it describes proves nothing against an adversary who can write to that origin". The project already applies that principle to what users install. This record asks what applying it to what NVCRE reports would look like.

**Where does it go, and who checks it?** The maintainers decided on [#397](https://github.com/NVIDIA/cluster-readiness-engine/issues/397) and [#426](https://github.com/NVIDIA/cluster-readiness-engine/issues/426) that NVCRE owns report *generation*, not report *delivery*: "A lightweight POST is still delivery". This record agrees and does not reopen it. Whatever receives and checks certification results lives outside this repository.

### The precedent

Certified Kubernetes has worked this way since 2017 and has never needed cryptography to do it:

| Step | Kubernetes conformance | NVCRE today |
|---|---|---|
| Run a versioned suite | `hydrophone --conformance-image registry.k8s.io/conformance:v1.37.1`. The suite is an image the release team publishes per version. | `kubectl nvcre certification run`. The catalog is compiled into the controller ([#244](https://github.com/NVIDIA/cluster-readiness-engine/issues/244) proposes versioning it separately). |
| Leave evidence | `e2e.log`, `junit_01.xml`, `PRODUCT.yaml` in a PR to `cncf/k8s-conformance` | `--results-file` JSON; ADR-090 bundle (proposed) |
| Check it | [`verify-conformance`](https://github.com/kubernetes-sigs/verify-conformance) reads a Gherkin feature file, labels the PR, and a person reviews it | Nothing |

The trust in that model comes from the bar being versioned and public, the evidence being plain files in a public PR, and the checks being readable sentences. [AICR ADR-007](https://github.com/NVIDIA/aicr/blob/main/docs/design/007-recipe-evidence.md) names this exact kind of evidence: "Human-readable, not cryptographically signed; trust comes from the submission process, not the artifact itself." AICR then goes one step further for recipe evidence, with a signed in-toto statement, a pointer file in git, and a signer allowlist.

## Decision

Three changes, all inside NVCRE's existing scope of report generation. None of them chooses a bar, delivers a report, or adds a network dependency to a run.

### 1. Every result records the bar it was judged against

`CertReport` gains a `judgedAgainst` block, written once per report:

```go
type JudgedAgainst struct {
    // CLIVersion is the nvcrectl build that assembled the report.
    // ControllerVersion is the manager build that ran the workloads.
    CLIVersion        string `json:"cliVersion"`
    ControllerVersion string `json:"controllerVersion,omitempty"`
    // Catalog identifies the catalog the categories came from: "embedded"
    // today, an OCI reference once #244 lands, plus a content digest.
    Catalog CatalogRef `json:"catalog"`
    // Thresholds is a digest over the resolved threshold set across all
    // categories, and false in ThresholdsSet when no category had any.
    ThresholdsDigest string `json:"thresholdsDigest,omitempty"`
    ThresholdsSet    bool   `json:"thresholdsSet"`
}
```

Each `CategoryReport` also gains the resolved threshold expressions it was evaluated against (the same strings ADR-071 already propagates into failure reasons), so a reader can see the bar without decoding the Certification spec.

The `Result` field keeps its four values. Changing `PASSED` would break every consumer that switches on it. The human card adds one line under the result, in the same spirit as `INCOMPLETE` in [#71](https://github.com/NVIDIA/cluster-readiness-engine/issues/71): describe what was checked, and don't invent a verdict.

```
│  Result:       PASSED                                          │
│  Judged by:    no thresholds set (informational)               │
```

NVCRE still does not decide what the bar is. It only writes down the one it was given.

### 2. The ADR-090 bundle carries an unsigned in-toto statement

When `--results-bundle` writes `manifest.json`, it also writes `statement.json`: an in-toto Statement whose subject is the manifest's digest and whose predicate is the `judgedAgainst` block plus the certification's identity (name, namespace, cluster UID, start and end time). NVCRE does **not** sign it. Signing happens outside the run, by whoever submits the result, with tools that already exist:

- `cosign attest-blob` against `statement.json`, or
- AICR's `aicr evidence publish`, if this becomes one more evidence kind beside `cncf-conformance` and `recipe-test-attestation`. ADR-007 says new kinds "plug in without further top-level CLI growth".

Keeping the signer out of the run path matters for air-gapped clusters ([#243](https://github.com/NVIDIA/cluster-readiness-engine/issues/243)), where Fulcio and Rekor are unreachable, and it keeps NVCRE from owning an identity model.

### 3. The submission contract is written down as a feature file

[`test/features/verify-nvcre.feature`](../../test/features/verify-nvcre.feature) states what a certification submission has to satisfy, in the same Gherkin shape as `verify-conformance`'s own feature file: one scenario per check, the message a checker would post when it fails, then the steps. It is a contract for whoever builds a checker, and the place to argue about what "complete" means. In this repository it can also run in CI against golden bundles, which the project's golden-file convention already supports. A scenario that fails there is a report NVCRE produces that no checker could accept.

The checker, the registry, and the review process live outside NVCRE.

## Implementation

A sketch only.

- **`judgedAgainst`.** `CLIVersion` comes from the version `nvcrectl` already carries ([`pkg/certification/certification.go:756`](../../pkg/certification/certification.go#L756)). `ControllerVersion` is the one piece that needs the controller to stamp its build on the Certification status, which is a CRD change and goes through the significant-change process. `Catalog.Digest` is a sha256 over the embedded catalog entries, computed at build time next to the `go:embed` in `pkg/catalog`. The digest is only meaningful if the controller and CLI embed the same catalog, which is another reason to want #244. `ThresholdsDigest` is a sha256 over the resolved threshold map (category, key, expression), sorted, in canonical JSON. All three are filled in `pkg/report` when the report is assembled. Apart from `ControllerVersion`, no CRD change is needed. The threshold set is already on the Workflow spec ([ADR-021](021-performance-threshold-enforcement.md)).
- **`statement.json`.** Written after `manifest.json`, with the same tmp-then-rename discipline ADR-090 requires. The predicate type URI is a maintainer decision, not something to settle in a fork.
- **Feature file.** Steps backed by [godog](https://github.com/cucumber/godog), the runner `verify-conformance` uses, reading a bundle directory. Golden bundles under `test/features/testdata/`.

## Rationale

The bar stays with subject-matter experts, as ADR-000 intends. What changes is that the result says which bar, so two results can be compared or told apart. That is what turns a report into a certificate a second party can read.

Signing outside the run follows from ADR-074's own reasoning: the integrity of an artifact has to come from somewhere other than the place that wrote it. It also lets NVCRE reuse AICR's verifier instead of growing a second one.

Writing the checks as Gherkin follows the precedent. The Kubernetes bot's checks have been plain sentences since its 2022 rewrite, which made them easy to review and argue about.

## Consequences

- Reports grow by a few hundred bytes.
- Consumers that parse the JSON see a new top-level field. Additive, so existing parsers are unaffected.
- The build has to compute and embed a catalog digest. A catalog change without a version bump becomes visible as a digest change, which is the point.
- `statement.json` adds one file to the ADR-090 bundle and one entry to its manifest.
- The feature file is a second source of truth about the bundle's shape, and it has to change when ADR-090's schema does. Running it in CI against golden bundles keeps the two honest.

## Alternatives Considered

**Ship default thresholds.** Rejected. ADR-000 puts pass/fail criteria with subject-matter experts, and a default would turn NVCRE's opinion into everyone's bar. Kai's #244 (a versioned catalog as its own artifact) is the right place for calibrated bars to live, owned by whoever owns them.

**Sign inside the controller.** Rejected. It needs network access to Sigstore during a run, breaks air-gapped installs, and makes NVCRE responsible for an identity model it has no reason to own.

**Own delivery and the registry.** Rejected on the reasoning in #397 and #426.

**Change `Result` to `PASSED_NO_THRESHOLDS`.** Rejected. It breaks consumers, and a missing bar is not a different verdict, only a missing explanation.

**Do nothing.** Every report stays readable only by the person who ran it.

## Notes

This record was written from outside the project, by people who ran the tooling for Kubernetes conformance for several years (APISnoop, the conformance test backlog, and the bot that checks submissions). It describes an approach. It is not a request to implement anything, and every decision in it belongs to the maintainers.

## References

- [ADR-000](000-adr.md): pass/fail criteria as a non-goal
- [ADR-021](021-performance-threshold-enforcement.md): threshold enforcement
- [ADR-071](071-threshold-reason-propagation.md): threshold reasons in report surfaces
- [ADR-074](074-supply-chain-attestation.md): supply chain artifact and verification contract
- [ADR-090](090-component-result-sharding.md): sharded component results and the report bundle
- [#71](https://github.com/NVIDIA/cluster-readiness-engine/issues/71) `INCOMPLETE`; [#244](https://github.com/NVIDIA/cluster-readiness-engine/issues/244) versioned catalog; [#397](https://github.com/NVIDIA/cluster-readiness-engine/issues/397), [#426](https://github.com/NVIDIA/cluster-readiness-engine/issues/426) delivery; [#446](https://github.com/NVIDIA/cluster-readiness-engine/issues/446) JUnit output
- [AICR ADR-007: Verifiable Recipe Test Evidence](https://github.com/NVIDIA/aicr/blob/main/docs/design/007-recipe-evidence.md)
- [cncf/k8s-conformance instructions](https://github.com/cncf/k8s-conformance/blob/master/instructions.md), [verify-conformance feature file](https://github.com/kubernetes-sigs/verify-conformance/blob/main/kodata/features/verify-conformance.feature), [Hydrophone](https://github.com/kubernetes-sigs/hydrophone)
