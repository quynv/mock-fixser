# mockfix

A simulated FIX 4.4 server written in Go. It streams prices for configured symbols, accepts orders and answers with ExecutionReports according to predefined rules. See [the design document](./Mock%20FIX%20Server%20—%20Design%20Document.md) for the full specification.

Status: phase 0 (setup). The server binary starts and stops but does not yet accept FIX sessions.

## Requirements

- Go 1.26.9 (pinned in `.go-version` for goenv)
- Docker (optional)

## Layout

| Path | Contents |
| --- | --- |
| `cmd/mockfix/` | Server entry point |
| `internal/` | Server packages (`fixserver`, and later `config`, `marketdata`, `order`, ...) |
| `spec/FIX44.xml` | quickfixgo FIX44 dictionary plus U1 MirrorLog, U2 MonitorControl and tags 5001–5006 |
| `fix/` | Go packages generated from `spec/FIX44.xml` (`fix44`, `field`, `tag`, `enum`); do not edit by hand |

## Commands

```sh
make gen     # regenerate fix/ after editing spec/FIX44.xml
make build   # static binary in bin/mockfix
make test    # go test -race ./...
make docker  # build the mockfix image
```

Run with `bin/mockfix --config configs/mock.yaml`. Flags: `--log-level`, `--no-ui`, `--version`.
