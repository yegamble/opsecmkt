"""Documentation that cites a Go test must cite one that exists (A-496).

A checklist line that names TestSomething is a claim that the test proves the
line. When a test is renamed or deleted (A-448 removed 146 of them) the claim
silently rots, so this check fails the operations job instead.

Scope, deliberately narrow so frozen history is not rewritten:

* docs/acceptance.md: only ticked items (`- [x]` and their indented
  continuation lines). An unticked line is a plan or a recorded gap, not a claim.
* docs/implementation-status.md: every line.
* docs/evidence/ and docs/verification.md are dated records and are not read.

No database, no Go toolchain: the Go side is read with a regular expression.
"""

import re
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
ACCEPTANCE = ROOT / "docs" / "acceptance.md"
STATUS = ROOT / "docs" / "implementation-status.md"

GO_FUNC = re.compile(r"^func ((?:Test|Fuzz)\w+)", re.M)
DOC_NAME = re.compile(r"\b((?:Test|Fuzz)[A-Z]\w*)(\*?)")

# Names that appear in the scoped documents but are not top-level Go test
# functions. Each entry needs a reason; an empty set is the goal.
SPEC_NAME = re.compile(r"\b([\w-]+\.spec\.ts)\b")

ALLOWLIST = {
    # (none: scripts and Python tests are cited by file or `test_` method name,
    # which the pattern above does not match)
}


def go_test_names():
    names = set()
    for base in ("internal", "cmd"):
        for path in sorted((ROOT / base).rglob("*_test.go")):
            names.update(GO_FUNC.findall(path.read_text(encoding="utf-8")))
    return names


def ticked_items(text):
    """Yield the text of each `- [x]` item with its indented continuation."""
    item = None
    for line in text.splitlines():
        if line.startswith("- [x]"):
            if item is not None:
                yield "\n".join(item)
            item = [line]
        elif item is not None and line.startswith("  ") and line.strip():
            item.append(line)
        else:
            if item is not None:
                yield "\n".join(item)
            item = None
    if item is not None:
        yield "\n".join(item)


def missing_names(text, defined):
    """Return sorted cited names that no Go test function defines.

    `TestFoo*` cites a family and is satisfied by any defined name with that
    prefix.
    """
    missing = set()
    for name, star in DOC_NAME.findall(text):
        if name in ALLOWLIST:
            continue
        if star:
            if not any(d.startswith(name) for d in defined):
                missing.add(name + "*")
        elif name not in defined:
            missing.add(name)
    return sorted(missing)


def missing_specs(text, existing):
    """Return sorted cited Playwright spec files that tests/e2e does not contain."""
    return sorted({name for name in SPEC_NAME.findall(text) if name not in existing})


def acceptance_claims(text):
    return "\n".join(ticked_items(text))


class DocTestNames(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.defined = go_test_names()

    def test_go_tests_were_found(self):
        # A wrong working directory or a moved tree must not make the lint pass
        # by finding nothing to compare against.
        self.assertGreater(len(self.defined), 500)

    def test_ticked_acceptance_lines_cite_existing_tests(self):
        claims = acceptance_claims(ACCEPTANCE.read_text(encoding="utf-8"))
        self.assertTrue(claims, "no ticked acceptance lines found")
        missing = missing_names(claims, self.defined)
        self.assertEqual(
            missing,
            [],
            "docs/acceptance.md ticked lines cite tests with no func in internal/ or cmd/: "
            + ", ".join(missing),
        )

    def test_ticked_acceptance_lines_cite_existing_browser_specs(self):
        # A browser spec that was renamed or deleted (wallet-journey.spec.ts went with the pooled wallet) is the same rot.
        claims = acceptance_claims(ACCEPTANCE.read_text(encoding="utf-8"))
        existing = {path.name for path in (ROOT / "tests" / "e2e").glob("*.spec.ts")}
        self.assertGreater(len(existing), 10)
        missing = missing_specs(claims, existing)
        self.assertEqual(
            missing,
            [],
            "docs/acceptance.md ticked lines cite browser specs that tests/e2e does not contain: " + ", ".join(missing),
        )

    def test_implementation_status_cites_existing_tests(self):
        missing = missing_names(STATUS.read_text(encoding="utf-8"), self.defined)
        self.assertEqual(
            missing,
            [],
            "docs/implementation-status.md cites tests with no func in internal/ or cmd/: "
            + ", ".join(missing),
        )


class DocTestNamesSelfTest(unittest.TestCase):
    """The lint must fail on an invented name and ignore what it should."""

    DEFINED = {"TestReal", "FuzzReal", "TestRealFamilyA"}

    def test_invented_name_in_ticked_line_fails(self):
        doc = "- [x] A claim (`TestReal`, `TestInventedByA496`).\n"
        self.assertEqual(
            missing_names(acceptance_claims(doc), self.DEFINED), ["TestInventedByA496"]
        )

    def test_invented_fuzz_name_fails(self):
        doc = "- [x] A claim (`FuzzInventedByA496`).\n"
        self.assertEqual(
            missing_names(acceptance_claims(doc), self.DEFINED), ["FuzzInventedByA496"]
        )

    def test_continuation_lines_of_a_ticked_item_count(self):
        doc = "- [x] A claim\n  (`TestInventedByA496`).\n\nProse after.\n"
        self.assertEqual(
            missing_names(acceptance_claims(doc), self.DEFINED), ["TestInventedByA496"]
        )

    def test_unticked_lines_and_prose_are_not_claims(self):
        doc = (
            "- [ ] A plan (`TestInventedByA496`).\n"
            "Prose naming TestAlsoInvented.\n"
            "- [x] Proven (`TestReal`).\n"
            "- [ ] Another plan\n  (`TestInventedByA496`).\n"
        )
        self.assertEqual(missing_names(acceptance_claims(doc), self.DEFINED), [])

    def test_real_names_prefixes_and_words_pass(self):
        text = "`TestReal`, `FuzzReal` seeds, `TestRealFamily*`, Test network, TestRun"
        self.assertEqual(missing_names(text, self.DEFINED | {"TestRun"}), [])

    def test_family_prefix_without_members_fails(self):
        self.assertEqual(missing_names("`TestNoSuchFamily*`", self.DEFINED), ["TestNoSuchFamily*"])

    def test_missing_browser_spec_in_ticked_line_fails(self):
        doc = "- [x] Browser: `db-real.spec.ts`, `wallet-journey.spec.ts`.\n- [ ] Plan: `gone.spec.ts`.\n"
        self.assertEqual(missing_specs(acceptance_claims(doc), {"db-real.spec.ts"}), ["wallet-journey.spec.ts"])

    def test_go_func_pattern_ignores_methods_and_helpers(self):
        src = "func TestA(t *testing.T) {}\nfunc (e *env) TestB() {}\nfunc helper() {}\nfunc FuzzC(f *testing.F) {}\n"
        self.assertEqual(sorted(GO_FUNC.findall(src)), ["FuzzC", "TestA"])


if __name__ == "__main__":
    unittest.main()
