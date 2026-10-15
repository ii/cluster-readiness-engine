# SPDX-License-Identifier: Apache-2.0
#
# Ideation for ADR-097 (docs/designs/097-verifiable-certification-results.md).
# The submission contract for an NVCRE certification result, written in the
# same shape as kubernetes-sigs/verify-conformance's feature file: one scenario
# per check, the message a checker posts when it fails, then the steps.

Feature: verify NVCRE certification submission PR

  Scenario: every targeted node has a verdict
    some targeted nodes have no verdict, or a failed node has no reason

    Given a "results.json" file
    Then every targeted node is listed as passed or failed
    And every failed node has a reason
    And the result is not "PASSED" when a targeted node was skipped

  Scenario: submission contains all required files
    there seem to be some required files missing

    Given <file> is included in its file list
    Then <file> is not empty
    And <file> is valid <type>

    Examples:
      | file              | type   |
      | "results.json"    | "json" |
      | "manifest.json"   | "json" |
      | "truncation.json" | "json" |
      | "statement.json"  | "json" |
      | "PRODUCT.yaml"    | "yaml" |

  Scenario: lost evidence is reported, not dropped
    some results could not be read and the submission does not say so

    Given a "truncation.json" file
    Then every partial read is listed with its component
    And no partial read is counted as zero rows

  Scenario: the manifest matches the files
    a file in the submission does not match its digest in manifest.json

    Given a "manifest.json" file
    Then every file listed has a matching sha256
    And the file count matches the files in the PR

  Scenario: the statement is signed by whoever submitted it
    the submission is not signed, or the signer is not on the allowlist

    Given a "statement.json" file
    Then its subject is the digest of "manifest.json"
    And it carries a signature
    And the signing identity is on the signer allowlist

  Scenario: PRODUCT.yaml says whose cluster this is
    it appears that PRODUCT.yaml does not contain all the required fields

    Given a "PRODUCT.yaml" file
    Then the yaml file "PRODUCT.yaml" contains the required and non-empty <field>

    Examples:
      | field      |
      | "provider" |
      | "product"  |
      | "platform" |
      | "gpu"      |
      | "nodes"    |

  Scenario: the submission names its bar
    results.json does not say which catalog and thresholds it was judged against

    Given a "results.json" file
    Then judgedAgainst names the catalog and its digest
    And judgedAgainst names the digest of the threshold set
    And judgedAgainst says whether any thresholds were set

  Scenario: thresholds were enforced
    the result is PASSED but no thresholds were set

    Given a "results.json" file
    Then the threshold set is not empty
    And every category was evaluated against it

  Scenario: all required categories are present
    it appears that some categories are missing from the submission

    Given a "junit.xml" file
    Then every required category for <gpu> on <platform> is present
    And no required category was skipped
