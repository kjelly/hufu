# Shared CLI test binary: issue #27

## Scope and implementation

The shared helper was already introduced in `7c544e4b` and is present in the
comparison base `402d97e2`. This follow-up makes its cache independently testable,
returns an empty path on failure, preserves the wrapped build error and complete
compiler output, adds a Windows executable suffix, and verifies its lifecycle.
It does not attribute the previously merged optimization to this follow-up.

`buildProcessContractBinary` delegates to `buildTestBinary`. The latter has no
platform build tag, builds `./cmd/hufu` from the module root, and uses ordinary
`go build -p 2` without race or coverage instrumentation. The PTY harness and its
callers retain their Linux tags; portable process tests can use the same helper.
A `sync.Once` caches both success and failure, and `os.MkdirTemp` creates a private
process-owned directory. `TestMain` removes that directory after `m.Run`, including
parallel tests and their cleanups. No consumer's `t.TempDir` owns the binary.
Existing HOME/XDG/config/workspace/env fixtures and process/PTY assertions remain
unchanged. Existing decisionrt and MCP re-exec tests retain their exit contracts.

Regression tests exercise 24 concurrent callers for both successful and failed
real builds of a tiny fixture module. Changing the fixture afterward must not
cause another build. Failed calls retain the identical error, its `exec.ExitError`
cause and compiler diagnostic, and return no path. Two concurrent test processes
verify distinct roots, survival across sequential consumers and consumer cleanup,
and `TestMain` removal on both success and failure. These fixture builds are
separate from the single real hufu CLI build per test process.

## Controlled comparison

The before tree is an archive of `402d97e2` with **only the two build helpers**
restored from issue #27's `8f65a57c`: restore `buildProcessContractBinary`, including
its `runtime` import; insert the `testBinaryOnce`/`testBinaryPath` declarations and
`buildTestBinary` block into `pty_e2e_test.go`; remove `test_binary_test.go`.
Normalize both old build commands to `go build -p 2` so parallelism is identical.
All remaining production code, test cases and assertions match the after tree.
This is a controlled helper comparison, **not** a whole-revision benchmark of
`8f65a57c` against current main, nor a full-project baseline for #26.

The measured set contains the 31 top-level tests directly calling either helper
in the comparison base. Generate its exact filter before running either tree:

```python
from pathlib import Path
import re
names = []
for path in Path('cmd/hufu').glob('*test.go'):
    if path.name.startswith('test_binary'):
        continue
    for block in re.split(r'(?=^func Test)', path.read_text(), flags=re.M)[1:]:
        if re.search(r'build(?:Test|ProcessContract)Binary\(t\)', block):
            names.append(re.match(r'func (Test\w+)', block)[1])
Path('work/filter.txt').write_text('^(' + '|'.join(sorted(names)) + ')$')
```

Run from each source root, with the same generated filter:

```sh
go test -json -short -count=1 -timeout=10m \
  -exec /absolute/path/to/test-exec -run "$(cat /absolute/path/to/filter.txt)" ./cmd/hufu
```

`test-exec` prepends a private directory containing a `go` forwarding wrapper to
PATH, then execs the test executable. This is necessary because `go test` itself
prepends GOROOT/bin to the test process's PATH. The forwarding wrapper records
each `go build` argv to a per-run log, then execs the real Go binary without
changing arguments. Count log lines, not compiler invocations or output events.
Each run receives its own short private TMPDIR, preventing the historical fixed
`hufu-e2e-test` name from affecting other runs. Shared caches and GOTMPDIR are
preserved. The outer invocation's wall time uses Python `time.perf_counter`;
JSONL package/test elapsed values are not summed.

Environment: Go 1.27.2, Linux amd64 (6.18.55), AMD EPYC 9V74, CPU quota 4 CPUs,
GOMAXPROCS=5, CGO_ENABLED=1, GOFLAGS=-mod=readonly, GOTMPDIR unset,
GOCACHE=/workspace/tooling/go-cache, GOMODCACHE=/workspace/tooling/gopath/pkg/mod.
Both parent tests and nested CLI binaries are ordinary non-race builds.
One unmeasured warm-up precedes three fresh (`-count=1`) observations per tree;
this measures warm build cache, not cold compilation or test-result replay.

| Helper implementation | Wall seconds (three observations) | Median / range | Nested CLI builds per run |
| --- | --- | --- | --- |
| Historical two-helper algorithm | 59.322, 59.559, 59.573 | 59.559 / 59.322–59.573 | 15, 15, 15 |
| Shared helper with this follow-up | 15.631, 16.054, 15.716 | 15.716 / 15.631–16.054 | 1, 1, 1 |

All six observations exited successfully: 31 top-level tests passed, with no skips.
The complete parent/subtest name-and-outcome sets also matched across all six
runs. Median wall time fell by about 73.6% for this controlled helper comparison;
this is a local observation, not a general CI speedup prediction.

The case set and outcomes must match in all measured runs. New cache/lifetime
regressions are deliberately excluded from this timing filter; they add fixture
builds and are validated separately. No speedup against the already-shared
`402d97e2` helper is claimed.

## Correctness checks

Passed on Go 1.27.2:

- `go test -count=1 -run '^TestCLIBinary' ./cmd/hufu` (0.494s).
- `go test -race -short -count=1 -timeout 20m ./cmd/hufu` (277.987s).
- `CGO_ENABLED=0 go test -count=1 -timeout 20m ./cmd/hufu` (38.476s).
- All 31 measured top-level cases, each launched alone in a new test process
  from a separately compiled `go test -c` executable, with `-test.short`, its
  exact `-test.run=^Name$` and `-test.timeout=5m`: 31/31 passed.
- `GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go test -c ./cmd/hufu`:
  compilation passed; native Darwin execution was not performed on this runner.
- `golangci-lint run`: 0 issues.

The race invocation instruments the test harness; the nested CLI build remains
an ordinary binary, as before. CGO-free execution is recorded separately.
Raw JSONL, per-run build argv logs and validation logs remain in ignored `work/`.
