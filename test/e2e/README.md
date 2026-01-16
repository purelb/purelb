# PureLB E2E Tests

End-to-end functional tests for PureLB.

## Test Suites

| Directory | Description |
|-----------|-------------|
| [local/](local/) | Tests for local IP allocation mode (LocalPool) |

## Running Tests

Each test suite has its own README with specific instructions. Generally:

```bash
cd <test-directory>
./<test-script>.sh
```

## Adding New Tests

When adding new test suites:

1. Create a subdirectory under `test/e2e/` for the feature being tested
2. Include a README.md documenting the tests
3. Include any required Kubernetes manifests
4. Ensure the test script cleans up after itself
