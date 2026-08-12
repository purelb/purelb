# Remote-mode fixtures

The remote-mode functional suite (`test-remote-allocation.sh`) has been
migrated to pytest: see [../py/tests/test_remote.py](../py/tests/test_remote.py).
It was removed only after `scripts/e2e-dualrun.sh --suite remote` reported
every one of its 162 assertions agreeing with a passing pytest counterpart.

## Running the migrated suite

    cd ../py
    .venv/bin/pytest tests/test_remote.py --context <your-context>

Remote mode puts the address on the `kube-lb0` dummy interface on EVERY
node, because it is reached by routing rather than by ARP. Two consequences
shape the tests: the pool deliberately sits outside every node subnet, so a
node cannot reach a remote VIP and reachability is checked from a POD; and
`ExternalTrafficPolicy: Local` is honoured here (it is overridden for local
pools), so most of the module is about which nodes hold endpoints.
