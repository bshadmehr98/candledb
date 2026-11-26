# 12. Releasing

[Chapter 11](11-continuous-integration.md) was about checking every commit.
This one is about the rarer event of handing a version to other people: a
binary they can download, an image they can run, a package they can install.

## What you will learn

- What a release is, and what a version number promises.
- How one git tag becomes three published things.
- How Go modules are released (it is less than you expect).
- How to build a container image for two CPU architectures without emulation.
- How a workflow gets permission to publish, and how to give it as little as possible.
- How to rehearse a release without publishing anything.

## The feature

Pushing a tag such as `v0.1.0` starts
[`.github/workflows/release.yml`](../../.github/workflows/release.yml),
which runs all of CI and then publishes:

| What | Where | How people get it |
|---|---|---|
| Server binaries for six platforms | A GitHub release | Download an archive from the repository's Releases page |
| Container image for amd64 and arm64 | GitHub's container registry | `docker pull ghcr.io/bshadmehr98/candledb` |
| JavaScript client | npm | `npm install candledb-client` |
| Go module | Nowhere: the tag is the release | `go install github.com/bshadmehr98/candledb/cmd/candledb@latest` |

All four carry the same version number, taken from the tag.

## Where the code is

- [`.github/workflows/release.yml`](../../.github/workflows/release.yml): the workflow.
- [`scripts/build-release.sh`](../../scripts/build-release.sh): builds the binary archives.
- [`Dockerfile`](../../Dockerfile): the image, built for two architectures.
- [`sdk/js/package.json`](../../sdk/js/package.json): what npm publishes.
- [`LICENSE`](../../LICENSE): the terms everything is published under.

## Version numbers

The project uses *semantic versioning*: `MAJOR.MINOR.PATCH`.

- **PATCH** (0.1.0 to 0.1.1): fixes only. Safe to take without reading anything.
- **MINOR** (0.1.1 to 0.2.0): new features; existing code keeps working.
- **MAJOR** (1.4.0 to 2.0.0): something existing code relied on has changed.

Two special cases:

- While the major version is 0, anything may change in any release. That
  is the honest label for a young project.
- A suffix marks a *prerelease*: `0.2.0-rc.1` comes before `0.2.0`. The
  workflow publishes prereleases, but does not make them the default: the
  image tag `latest` and the npm tag `latest` stay on the last full release.

The tag is the only place the version is written. The workflow passes it to
the Go build with `-ldflags "-X main.version=..."`
([chapter 10](10-production.md#building-the-binary)), to the image build
as an argument, and into `package.json` just before publishing:

```yaml
      - name: Set the package version from the tag
        run: npm version "${{ needs.version.outputs.version }}" --no-git-tag-version --allow-same-version
```

A version kept in three files will one day disagree with itself.

## Releasing a Go module

Go has no package registry to upload to. A module's versions are the git
tags of its repository. When someone runs

```sh
go install github.com/bshadmehr98/candledb/cmd/candledb@latest
```

the `go` command asks a proxy, `proxy.golang.org`, which fetches the
repository, finds the highest version tag, and serves that source. The
proxy keeps its copy forever, and a public checksum database records its
hash.

Two consequences:

- The module path in [`go.mod`](../../go.mod) must be where the repository
  really lives. That is why it is `github.com/bshadmehr98/candledb` and not
  just `candledb`.
- **A published tag cannot be changed.** If you move `v0.1.0` to a
  different commit, people get either the old code from the proxy or a
  checksum error. A mistake in a release is fixed by releasing `v0.1.1`.

## The workflow

```
            ┌─ binaries ── GitHub release
version ─┐  │
         ├──┼─ image ───── ghcr.io
verify ──┘  │
            └─ npm ─────── npmjs.com
```

`version` turns the tag into a version number and checks its form.
`verify` runs every CI job:

```yaml
  verify:
    name: CI
    uses: ./.github/workflows/ci.yml
```

`uses` on a job calls another workflow as if it were a function. `ci.yml`
allows this with `workflow_call` in its `on` section. The three publishing
jobs say `needs: [version, verify]`, so nothing is published from a commit
that fails a test.

The publishing jobs are independent of each other. If one fails (say, npm
is unreachable), the other two still complete, and the failed job can be
re-run on its own from the run's page.

### Binaries

`make release-build` runs
[`scripts/build-release.sh`](../../scripts/build-release.sh), which loops
over six platforms and for each one compiles the server, puts it in an
archive with the licence and README, and finally writes `checksums.txt`.
Someone who downloads an archive can check it:

```sh
sha256sum -c checksums.txt --ignore-missing
```

`gh release create` then creates the release and attaches the files.
`--generate-notes` writes the list of changes from the pull requests and
commits since the previous tag.

### The container image

The image is built for `linux/amd64` and `linux/arm64`, so the same name
works on an ordinary server, a Raspberry Pi and an Apple-silicon laptop.

The slow way to build for another architecture is to emulate its CPU. The
[`Dockerfile`](../../Dockerfile) avoids that:

```dockerfile
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
```

```dockerfile
ARG TARGETOS TARGETARCH
```

```dockerfile
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /candledb ./cmd/candledb
```

The build stage always runs on the machine's own architecture
(`BUILDPLATFORM`) and asks the Go compiler to produce a binary for the
target. The final stage only copies files, which needs no CPU of any kind.
This works because the server is pure Go with `CGO_ENABLED=0`: one more
reason a SQLite driver written in Go was chosen
([chapter 6](06-sql-and-encoding.md)).

### The npm package

The job installs dependencies with `npm ci` (exactly what
`package-lock.json` says, unlike `npm install`), compiles the TypeScript,
runs the same package check as CI, and publishes.

`--provenance` makes npm record which repository, commit and workflow
built the package, and shows that on the package's page. It is evidence
that the published files came from this source and not from someone's
laptop.

## Permission to publish

A workflow that can publish is a target: whoever can change what it runs
can ship code to your users. The workflow is built to limit that.

```yaml
permissions:
  contents: read
```

is the default for every job, and each publishing job adds one thing:

| Job | Extra permission | Why |
|---|---|---|
| binaries | `contents: write` | to create the release |
| image | `packages: write` | to push to the registry |
| npm | `id-token: write` | to prove its identity for provenance |

The GitHub release and the image are published with the token GitHub
creates for each run, which expires when the run ends. Nothing has to be
stored.

npm is a different company, so it needs a secret: an access token created
on npmjs.com and saved in the repository as `NPM_TOKEN` (Settings, then
Secrets and variables, then Actions). The workflow reads it only in the
one step that publishes:

```yaml
        env:
          NODE_AUTH_TOKEN: ${{ secrets.NPM_TOKEN }}
```

Secrets are not passed to workflows started from a fork's pull request,
and GitHub masks them in logs. If the secret is missing, the job fails with
a message saying so, and the other two publish normally.

## Rehearsing

Because a release cannot be taken back, the workflow can be run without
releasing. Start it by hand from the Actions tab, or with:

```sh
gh workflow run release.yml
```

Started this way the version is `0.0.0-rehearsal`, every artefact is
built, and every step that publishes is skipped:

```yaml
        if: needs.version.outputs.publish == 'true'
```

The archives can be downloaded from the run's page to try.

## Making a release

1. Make sure `main` is green.
2. Choose the version by the rules above.
3. Tag and push:

   ```sh
   git tag v0.1.0
   git push origin v0.1.0
   ```

4. Watch the run: `gh run watch`.
5. Check the results: the Releases page, `docker pull`, `npm view candledb-client`.

The first time, two things need doing by hand: add the `NPM_TOKEN` secret,
and, after the first image is pushed, set the package's visibility to
public on GitHub (new packages there start private).

## What a release does not do

- **Sign the binaries.** There are checksums, which detect a corrupted
  download, but nothing that proves who built the archives. macOS and
  Windows will warn about unsigned programs.
- **Publish to Docker Hub**, Homebrew or any Linux distribution.
- **Keep a hand-written changelog.** The release notes are generated from
  commit and pull request titles, which makes those titles worth writing well.
- **Deploy anything.** Nothing running anywhere is updated by a release.

## Try it

Build the release archives locally and run one:

```sh
make release-build VERSION=v0.0.0-test
ls release/
tar -xzf release/candledb_0.0.0-test_linux_amd64.tar.gz -C /tmp
/tmp/candledb_0.0.0-test_linux_amd64/candledb version
```

Use the archive for your own system in the last two commands. The output
is `0.0.0-test`: the version went from the command line, through the
script, into the binary.

Build the image for both architectures (needs Docker with buildx):

```sh
docker buildx build --platform linux/amd64,linux/arm64 --build-arg VERSION=test .
```

See what npm would publish:

```sh
make sdk-pack
cd sdk/js && npm pack --dry-run
```

## Exercise: say which commit a binary was built from

`candledb version` prints the version. For a bug report it would help to
also know the exact commit.

- Add a second variable next to `version` in
  [`cmd/candledb/main.go`](../../cmd/candledb/main.go), named `commit`,
  with the default `"unknown"`.
- Print it from the `version` command, for example `0.1.0 (3f2a91c)`.
- Set it with a second `-X` flag in the [`Makefile`](../../Makefile),
  [`scripts/build-release.sh`](../../scripts/build-release.sh) and the
  [`Dockerfile`](../../Dockerfile). `git rev-parse --short HEAD` prints
  the commit. The Docker build has no `.git` directory (look at
  [`.dockerignore`](../../.dockerignore)), so there it has to arrive as a
  build argument.
- Update the test of the `version` command in
  [`cmd/candledb/main_test.go`](../../cmd/candledb/main_test.go).

**How to know it works.** `make build && ./bin/candledb version` prints
your current commit; `make check` and `make docker-smoke` pass; a binary
built with plain `go build ./cmd/candledb` prints `dev (unknown)`.

**Stretch.** Go already embeds the commit in every binary built from a git
checkout. Read it with `debug.ReadBuildInfo()` from `runtime/debug`
(look for the setting named `vcs.revision`) and drop the `-X` flag for it.
Find out in which of the three builds that does not work, and why.

## Further reading

- [Semantic Versioning](https://semver.org)
- [Publishing a Go module](https://go.dev/doc/modules/publishing) and [Module version numbering](https://go.dev/doc/modules/version-numbers)
- [Reusing workflows](https://docs.github.com/en/actions/using-workflows/reusing-workflows)
- [Controlling permissions for GITHUB_TOKEN](https://docs.github.com/en/actions/security-guides/automatic-token-authentication)
- [Docker: multi-platform builds](https://docs.docker.com/build/building/multi-platform/)
- [npm: generating provenance statements](https://docs.npmjs.com/generating-provenance-statements)

You have now read every part of the server, the checks that guard it and
the way it reaches its users. The architecture track looks at the same
code from above: why it is divided the way it is.

Next: [Architecture: the hexagon](../architecture/01-hexagonal.md)
