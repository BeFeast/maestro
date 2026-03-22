## Test-First Development Workflow

Before writing any implementation code, follow this test-first sequence:

### Step 1: Identify assertions
Read the issue description and extract concrete, testable requirements. Each requirement
becomes an assertion that your tests will verify.

### Step 2: Write failing tests
For each assertion, write a test that:
- Describes the expected behavior clearly in the test name
- Fails before implementation (proving it actually tests something)
- Is deterministic and machine-checkable (no manual verification needed)

### Step 3: Implement until tests pass
Write the minimum code necessary to make each test pass. Do not write implementation
code that is not exercised by a test.

### Step 4: Refactor
Only after all tests pass, clean up the implementation. Re-run tests after refactoring.

### Why test-first?
- Prevents implementation-led tests that just confirm what was written
- Catches regressions — tests encode the contract, not the implementation
- Forces clear thinking about requirements before coding

---

## Validation Evidence in PR

When creating the PR, include validation evidence in the description:

```
## Validation
- [ ] Tests written before implementation
- [ ] All new tests pass
- [ ] All existing tests still pass
- [ ] Build compiles without errors
- [ ] Lint/format clean
```

If a VALIDATION.md file exists in the worktree, read it first and use it as your
validation checklist. Report which assertions passed in the PR body.
