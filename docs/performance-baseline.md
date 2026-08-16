# Frontend performance baseline

This baseline was captured before the core API refactor required by roadmap item TOP-02. It is the fixed comparison point for TOP-02 and TOP-03; do not regenerate it after those refactors.

Run the same benchmark on the candidate revision, then compare it with the checked-in result:

```sh
go test ./internal/mount -run '^$' -bench '^BenchmarkBaseline' -benchmem -benchtime=200ms -count=3 > /tmp/dfs-benchmark-candidate.txt
./scripts/check-benchmark-regression.sh internal/mount/testdata/pre_core_api_linux_amd64.txt /tmp/dfs-benchmark-candidate.txt
```

The checker uses the mean of repeated samples and enforces these immutable limits:

| Operation | Maximum time regression | Maximum allocation regression |
| --- | ---: | ---: |
| Cached open/read and bulk reads | 5% | 10% |
| Namespace enumeration, lookup, and attributes | 10% | 10% |
| Create, rename, and delete | 15% | 10% |

An operation missing from either input is a failure. Results must be compared on the same otherwise-idle host, with the same Go version, CPU-power policy, filesystem, benchmark flags, and build options. The committed capture records the pre-refactor CachyOS reference environment; mounted-volume acceptance tests on CachyOS and Zeus remain mandatory in addition to these in-process benchmarks.

