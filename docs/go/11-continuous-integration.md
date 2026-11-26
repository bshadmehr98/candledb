# 11. Continuous integration

The previous chapters showed how to write the code and its tests. This one
is about the machine that runs those tests for you: on every push, on three
operating systems, before anyone can forget.

## What you will learn

- What continuous integration (CI) is, and what a GitHub Actions workflow file contains.
- Why every CI job in this project is a `make` target.
- What each job checks, and which kind of mistake only that job finds.
- How to run a job on your own machine, and how to read a failing run.
- Why CI can fail when nobody changed the code, and what to do about it.
- Three real failures from this project's first CI runs, and their fixes.

## The feature

Every push to `main` and every pull request starts two workflows on
GitHub's machines:

| Workflow | File | Runs |
|---|---|---|
| CI | [`.github/workflows/ci.yml`](../../.github/workflows/ci.yml) | on every push and pull request |
| Security | [`.github/workflows/security.yml`](../../.github/workflows/security.yml) | on every push and pull request, and every Monday |

A third file, [`.github/dependabot.yml`](../../.github/dependabot.yml),
asks GitHub to open a pull request whenever a dependency has a newer
version. CI then runs on that pull request, so the update arrives already
tested.

Nothing is published by these workflows. They only answer the question
"is this commit good?". Publishing is [chapter 12](12-releasing.md).

## Where the code is

- [`.github/workflows/ci.yml`](../../.github/workflows/ci.yml): the seven CI jobs.
- [`.github/workflows/security.yml`](../../.github/workflows/security.yml): vulnerability scans and CodeQL.
- [`.github/dependabot.yml`](../../.github/dependabot.yml): dependency updates.
- [`Makefile`](../../Makefile): the commands the jobs run.
- [`scripts/docker-smoke.sh`](../../scripts/docker-smoke.sh): the container test.
- [`sdk/js/scripts/check-pack.mjs`](../../sdk/js/scripts/check-pack.mjs): the npm package test.
- [`test/docs/docs_test.go`](../../test/docs/docs_test.go): the test that checks these documents.
- [`test/integration`](../../test/integration): the tests that drive the whole server over HTTP.

## A workflow file, piece by piece

A workflow is a YAML file. The top of `ci.yml`:

```yaml
name: CI

on:
  push:
    branches: [main]
  pull_request:

permissions:
  contents: read

concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: true
```

- `on` says what starts the workflow: a push to `main`, or any pull request.
- `permissions` says what the workflow may do to the repository. It only
  needs to read the code, so that is all it gets. If a step were ever
  compromised, it could not push commits or publish anything.
- `concurrency` cancels a run when a newer push to the same branch makes it
  pointless.

Below that come the jobs. Each job starts on a fresh virtual machine,
checks the code out, installs Go, and runs commands:

```yaml
  coverage:
    name: Coverage
    runs-on: ubuntu-latest
    timeout-minutes: 10
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: ${{ env.GO_VERSION }}
          check-latest: true
      - name: Total coverage meets the minimum
        run: make cover-check
```

- `uses` runs a published action, a reusable step someone else wrote.
  `actions/checkout` fetches the repository; `actions/setup-go` installs Go.
- `run` runs a shell command. A step fails if its command exits with a
  status other than zero, and a failed step fails the job.
- `timeout-minutes` stops a job that hangs. Without it a stuck test would
  occupy a machine for six hours.

Jobs run in parallel and do not share files. That is why every job begins
with the same two or three steps.

## Every job is a make target

Look at the `run` lines in `ci.yml`: almost all of them are `make
something`. The workflow file says *when* and *where* to run; the
[`Makefile`](../../Makefile) says *what*.

The reason is practical. When a job fails, you want to reproduce the
failure on your own machine, and "run `make cover-check`" is something you
can do. A check that exists only as twenty lines of YAML can only be
debugged by pushing commits and waiting.

```sh
make check          # everything CI runs, except the container job
make docker-smoke   # the container job (needs Docker)
```

## What each job checks

### Static checks

Checks that read the code without running it.

| Step | Command | What it finds |
|---|---|---|
| Formatting and `go vet` | `make lint` | Unformatted files; constructs that compile but are almost certainly wrong, such as a `Printf` whose arguments do not match its format string |
| Tidy module files | `make tidy-check` | A dependency that is imported but not in `go.mod`, or listed but no longer used |
| staticcheck | `make staticcheck` | A wider set of mistakes: unused code, deprecated functions, comparisons that are always true |
| Known vulnerabilities | `make vuln` | Functions this code calls that have a published vulnerability |
| Example rules | `make rules-check` | A syntax error in [`examples/todo/candledb.rules`](../../examples/todo/candledb.rules) |
| Workflow files | `actionlint` | Mistakes in the workflow files themselves |

`staticcheck` and `govulncheck` are fetched with `go run package@latest`,
so nothing has to be installed first, on the CI machine or on yours.

`go mod tidy -diff` prints the changes `go mod tidy` would make and exits
with an error if there are any. It changes nothing, which is what a check
should do.

### Tests on three operating systems

```yaml
    strategy:
      fail-fast: false
      matrix:
        os: [ubuntu-latest, macos-latest, windows-latest]
```

A `matrix` runs the same job once per value. `fail-fast: false` lets the
other two finish when one fails, so that you learn in one run whether a
failure is specific to one system.

The job has two steps:

| Step | Command | Which tests |
|---|---|---|
| Unit, contract and property tests | `make test-unit` | Every package under `internal/` and `cmd/` |
| Integration tests | `make test-integration` | [`test/integration`](../../test/integration), which starts the real server and talks to it over HTTP, and [`test/docs`](../../test/docs), which checks these documents |

Both run with the race detector ([chapter 9](09-concurrency.md)).
[Architecture: testing strategy](../architecture/04-testing-strategy.md)
describes what each kind of test is for.

`go test ./...` would run all of them in one command. They are two steps
so that the summary of a failed run says at a glance whether the mistake is
inside one package or only shows up when everything is assembled.

Why three systems, when the server will mostly run on Linux? Because Go
code that touches files, signals or paths can behave differently on each,
and people develop on all three. The first Windows run found exactly such a
difference; see [below](#three-failures-from-the-first-runs).

### Coverage

`make cover-check` runs the tests with a coverage profile and fails if the
total is below `COVER_MIN`, which the `Makefile` sets to 85 percent. The
total at the time of writing is 89 percent.

The minimum is a floor, not a goal. Its job is to notice the pull request
that adds three hundred lines and no tests. A coverage number says which
lines ran, not whether anything checked their results;
[chapter 3](03-testing.md#coverage) explains the difference.

The job also uploads `coverage.out` as an *artifact*: a file attached to
the run that you can download from its page.

### Fuzzing and benchmarks

- `make fuzz` feeds generated input to the rules parser for 30 seconds
  ([chapter 3](03-testing.md#fuzzing)). Thirty seconds will not find a
  subtle bug, but it finds a crash introduced by yesterday's change.
- `make bench-smoke` runs every benchmark exactly once (`-benchtime 1x`).
  It measures nothing. Benchmarks are code that the ordinary tests never
  execute, so without this step they would rot unnoticed until the day
  somebody needs them.

CI machines are shared and their speed varies from run to run, which is
why this job does not compare timings. Measure performance on a quiet
machine ([chapter 10](10-production.md#benchmarks-and-profiles)).

### Builds for every platform

`make cross` compiles the server for Linux, macOS and Windows, each on
amd64 and arm64. Go cross-compiles without extra tools
([chapter 10](10-production.md#building-the-binary)), so this takes
seconds. It catches code that uses something one platform does not have.

Compiling is weaker than testing: this job proves that
`windows/arm64` builds, not that it works.

### JavaScript client

The client's tests start the real server binary and talk to it, so this
job installs both Go and Node. A matrix runs it on Node 20, 22 and 24: the
oldest version the client supports, and the two after it.

`make sdk-pack` then asks npm which files it *would* publish and checks
the list: the compiled client must be there, and sources and tests must
not. Nothing is uploaded. Without this, a mistake in the `files` field of
`package.json` would be discovered by the first person to install an empty
package.

### Container image

`make docker-smoke` runs [`scripts/docker-smoke.sh`](../../scripts/docker-smoke.sh),
which:

1. builds the image from the [`Dockerfile`](../../Dockerfile);
2. starts a container and waits for `/readyz`;
3. checks that the process is not running as root;
4. writes a document with the admin key and reads it back;
5. checks that a request without credentials is refused;
6. stops the container, checks that the server exited cleanly, starts a
   new one on the same volume, and reads the document again.

This is called a *smoke test*: switch it on and see whether smoke comes
out. It is deliberately shallow. The Go tests already cover the server's
behaviour; this covers what they cannot see, namely whether the image
contains a working binary, listens on the right port, can write to its
data directory as an unprivileged user, and shuts down on the signal
Docker sends.

### The security workflow

| Job | What it does |
|---|---|
| Go vulnerabilities | `make vuln` again |
| npm vulnerabilities | `npm audit` on the client's development dependencies |
| CodeQL | GitHub's analyser, which follows data through the program looking for patterns such as unsanitised input reaching a file path; findings appear in the repository's Security tab |

These also run on a schedule:

```yaml
on:
  schedule:
    - cron: "17 5 * * 1" # Mondays, 05:17 UTC
  workflow_dispatch:
```

`workflow_dispatch` adds a button to the Actions tab for starting the
workflow by hand.

## CI can fail when nobody changed anything

Most checks depend only on the code: the same commit gives the same
result. Vulnerability checks are different. They compare the code against
a database that grows every week. A commit that passed on Monday can fail
on Friday because somebody found a bug in a library it uses.

That is the reason for the weekly schedule. Without it, a project that
nobody pushes to would never learn about a new vulnerability.

It is also the reason for this setting, used by every Go job:

```yaml
      - uses: actions/setup-go@v5
        with:
          go-version: ${{ env.GO_VERSION }}
          check-latest: true
```

`GO_VERSION` is `1.27.x`, and `check-latest` makes the action install the
newest patch release of 1.27 instead of one it has cached. Fixes for
vulnerabilities in the standard library arrive in patch releases.

## Three failures from the first runs

These are real. Each one is a kind of failure you will meet again.

### 1. A vulnerability in the standard library

The very first run failed in the vulnerability check:

```
Vulnerability #1: GO-2026-6859
    HTTP/1 server crash due to MaxBytesReader header race in net/http
  Standard library
    Found in: net/http@go1.27.2
    Fixed in: net/http@go1.27.4
```

Nothing in the repository was wrong. The workflow installed the exact Go
version written in `go.mod`, 1.27.2, and two vulnerabilities fixed in
later patch releases had been published. The same check had passed on the
author's machine a few hours earlier.

The fix was the `check-latest` setting above. The `go` line in `go.mod`
states the *minimum* version the code needs; it is not a good choice for
the version to build releases with.

### 2. A test that assumed Unix

The first Windows run failed here:

```
--- FAIL: TestFilesArePrivate (0.06s)
    store_test.go:90: test.db has permissions 666, want 600
```

The test checks that the database file, which contains password hashes,
is readable only by its owner. Windows has no owner/group/other
permission bits. Go reports a synthetic value there, and `os.Chmod` can
only toggle read-only.

The code was not wrong and neither was the test; the property does not
exist on that system. The test now skips itself:

```go
	if runtime.GOOS == "windows" {
		// Windows has no owner/group/other permission bits; access there is
		// governed by the ACL the file inherits from its directory.
		t.Skip("Unix file permissions do not exist on Windows")
	}
```

`t.Skip` is honest in a way that deleting the assertion would not be: the
test output says the check did not run, and why. The operations guide now
tells Windows users to protect the data directory themselves.

### 3. A flaky test

On Node 20 only, one client test failed with `1 !== 2`. It counted the
server's open listen sessions, opened one, and expected the count to rise
by one. But an earlier test had closed its stream a moment before, and the
server had not noticed yet. The count went down by one and up by one at
the same time.

The test passed on Node 22 and 24 in the same run, and had passed locally
dozens of times. That is what a *flaky* test looks like: correct code, a
test whose result depends on timing.

The fix was to wait until the count settles at zero before starting,
instead of trusting the first reading.
[Chapter 3](03-testing.md#waiting-without-sleeping) covers the technique.
The wrong fix would have been to press "re-run" until it went green. A
flaky test that is tolerated teaches everyone to ignore red runs, and then
the suite stops protecting anything.

## Reading a failed run

On GitHub, open the repository's **Actions** tab, click the run, click the
red job, and the failed step is already expanded.

With the [`gh`](https://cli.github.com) command-line tool:

```sh
gh run list --limit 5          # recent runs and their results
gh run view <id>               # jobs of one run
gh run view <id> --log-failed  # the output of the failed steps only
gh run watch <id>              # follow a run in progress
```

Then:

1. Find the step that failed, and the `make` target it ran.
2. Run that target locally.
3. If it passes locally, look at what differs: the operating system, the
   Go or Node version, or timing. The three failures above were one of
   each.

## What is not in CI

- **Publishing.** These two workflows publish nothing. A third one does,
  when a version tag is pushed; it is the subject of
  [chapter 12](12-releasing.md).
- **Browsers.** The client is tested in Node. The example app is not
  opened in a browser by any test.
- **Performance.** Benchmarks run once, to prove they work. Nothing
  compares their timings with an earlier commit.
- **Long runs and large data.** Nothing runs for hours or with millions of
  documents.
- **Branch protection.** CI reports a result; it does not stop anyone
  from pushing to `main`. Requiring the checks to pass before a pull
  request can be merged is a repository setting on GitHub.

## Try it

Run the whole pipeline on your machine:

```sh
make check
make docker-smoke
```

Then break things on purpose, one at a time, and watch the right check
fail. Undo each change with `git checkout .` before the next.

| Change | Run | What you should see |
|---|---|---|
| Add spaces before a `}` in any `.go` file | `make lint` | `these files need gofmt:` and the file name |
| Rename `docs/go/02-errors.md` | `go test ./test/docs` | one line for every link that pointed at it |
| Delete a test file from `internal/rules` | `make cover-check` | the total drops; lower `COVER_MIN` on the command line (`make cover-check COVER_MIN=95`) to see the failure message without deleting anything |
| Change `CMD ["serve"]` to `CMD ["version"]` in the `Dockerfile` | `make docker-smoke` | `the server did not become ready`, followed by the container's log |
| Remove `"dist"` from `files` in `sdk/js/package.json` | `make sdk-pack` | a complaint about what the package would contain |

## Exercise: a check for forgotten debugging output

The project logs through `log/slog` and never prints with `fmt.Println`
in the server's packages. Nothing enforces that. Add a check that does.

- Write a test in a new package `test/hygiene` that walks `internal/` and
  fails if a non-test `.go` file contains `fmt.Println(` or `fmt.Printf(`.
  [`test/docs/docs_test.go`](../../test/docs/docs_test.go) shows how to
  walk the repository from a test.
- Decide what to do about `cmd/candledb`, where printing to the terminal
  is the correct behaviour of `check-rules` and `version`.
- Check that `make test-integration` picks the new package up without any
  change to the `Makefile` or the workflow, and work out why.

**How to know it works.** `go test ./test/hygiene` passes on the
unmodified code. Add `fmt.Println("here")` to any function in
`internal/app` and it fails, naming the file.

**Stretch.** Searching for text finds `fmt.Println(` inside comments and
strings too. Parse the files with `go/parser` and inspect call expressions
with `go/ast` instead. That is a small version of what `go vet` and
`staticcheck` do.

## Further reading

- [GitHub Actions: understanding workflows](https://docs.github.com/en/actions/using-workflows/about-workflows)
- [Workflow syntax reference](https://docs.github.com/en/actions/using-workflows/workflow-syntax-for-github-actions)
- [actions/setup-go](https://github.com/actions/setup-go)
- [Go toolchains](https://go.dev/doc/toolchain), on what the `go` line in `go.mod` means
- [Vulnerability Management for Go](https://go.dev/blog/vuln)
- [staticcheck](https://staticcheck.dev) and [actionlint](https://github.com/rhysd/actionlint)
- [About Dependabot version updates](https://docs.github.com/en/code-security/dependabot/dependabot-version-updates/about-dependabot-version-updates)

Next: [Releasing](12-releasing.md)
