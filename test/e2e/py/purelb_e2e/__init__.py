"""PureLB end-to-end test harness."""

# The namespace holding the echo backend every suite exercises. Defined
# here rather than per test module so the modules do not import constants
# from one another -- tests/ is not a package, and cross-test imports make
# collection order matter.
TEST_NAMESPACE = "test"
