# mockfix

A simulated FIX 4.4 server written in Go. It streams prices for configured symbols, accepts orders and answers with ExecutionReports according to predefined rules. See [the design document](./docs/Design%20Document.md) for the full specification.

Status: phase 1 (core FIX and config). The server accepts FIX 4.4 logons for the sessions in the YAML config and hot-reloads the config. Application messages are answered with BusinessMessageReject until the market data (phase 2) and order (phase 3) engines land.

## Requirements

- Go 1.26.9 (pinned in `.go-version` for goenv)
- Docker (optional)

## Layout

| Path | Contents |
| --- | --- |
| `cmd/mockfix/` | Server entry point |
| `internal/config/` | Config model, YAML loading, validation, hot reload, versioning |
| `internal/fixserver/` | Client and monitor acceptors, MsgType router |
| `schema/config.schema.json` | JSON Schema of the config, embedded in the binary |
| `configs/mock.yaml` | Sample config (design doc, section 9.1) |
| `data/` | Replay files, e.g. `data/aapl.csv` |
| `spec/FIX44.xml` | quickfixgo FIX44 dictionary plus U1 MirrorLog, U2 MonitorControl and tags 5001–5006 |
| `fix/` | Go packages generated from `spec/FIX44.xml` (`fix44`, `field`, `tag`, `enum`); not committed, created by `make gen` |

## Commands

The FIX packages in `fix/` are generated, not committed. `make build`, `make test` and `make vet` generate them first (only when `spec/FIX44.xml` changed). Without make, run `go generate ./fix` once after cloning.

```sh
make gen     # generate fix/ from spec/FIX44.xml
make build   # static binary in bin/mockfix
make test    # go test -race ./...
make docker  # build the mockfix image
```

Run with `bin/mockfix --config configs/mock.yaml`. Flags: `--log-level`, `--no-ui`, `--version`.

## Configuration

`configs/mock.yaml` is the single source of truth. On startup an invalid config exits with code 2 and prints every error with its line number:

```
invalid config:
  line 25: symbols[0].initial_price: initial_price (1.085) must be within [min, max] = [1.07, 1]
```

While running, edits to the file are picked up after a 200 ms debounce. A valid change gets the next version number, the previous version is saved to `configs/history/vN.yaml` (latest 20 kept), and the engines apply it. An invalid change is logged and the running config stays.

Client sessions can be added or removed without a restart: removed sessions receive a Logout and are disconnected. Changing `listen` addresses, `data_dictionary` or the monitor session needs a restart; the log reports these under `restart_required`.
