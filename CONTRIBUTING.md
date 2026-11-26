# Contributing

Thank you for wanting to improve candledb. This project is both a database
and a codebase people learn Go from, so a change is judged on two things:
whether it is correct, and whether a beginner can still read the result.

## Getting set up

You need Go 1.27 or newer, Node 20 or newer, and Docker for one optional
check.

```sh
make build          # the server, into bin/candledb
make check          # everything CI runs, except the container test
make docker-smoke   # the container test
make browser-test   # the todo example in three browsers
make help           # all targets
```

## Making a change

1. Open an issue first for anything larger than a small fix, so the
   approach can be agreed before you spend time on it.
2. Create a branch and make the change. Add or update tests at the lowest
   layer that can catch a mistake in it;
   [docs/architecture/04-testing-strategy.md](docs/architecture/04-testing-strategy.md)
   says which layer that is.
3. If the change affects behaviour described in `docs/`, update the
   documents in the same pull request. Code excerpts in the docs are copied
   from the source and must still match it.
4. Run `make check`.
5. Open a pull request. CI runs on it automatically;
   [docs/go/11-continuous-integration.md](docs/go/11-continuous-integration.md)
   explains each check and how to read a failure.

## Conventions

- Code is formatted with `gofmt` and passes `go vet` and `staticcheck`.
- The core (`internal/domain`, `internal/rules`, `internal/app`) imports
  no adapter. See
  [docs/architecture/01-hexagonal.md](docs/architecture/01-hexagonal.md).
- New dependencies need a good reason. The server has three.
- Comments explain why, in full sentences, for a reader who is new to Go.
- Commit and pull request titles are used to generate release notes, so
  write them for someone reading a list of changes.

## Releases

Maintainers release by pushing a version tag;
[docs/go/12-releasing.md](docs/go/12-releasing.md) describes the process.

## License

By contributing you agree that your contribution is licensed under the
[MIT license](LICENSE).
