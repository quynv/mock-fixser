# mockfix

A simulated FIX 4.4 server written in Go. It streams prices for configured symbols, accepts orders and answers with ExecutionReports according to predefined rules. See [the design document](./docs/Design%20Document.md) for the full specification.

Status: phase 3 (orders), the MVP gate: mockfix runs entirely from YAML. Clients get market data (V/W/X/Y) and orders (D/F/G/H) answered by the rule engine, with drop copies to the monitor session. Order lists (E/K/M/q), the REST API and the Web UI come in later phases.

## Requirements

- Go 1.26.9 (pinned in `.go-version` for goenv)
- Docker (optional)

## Layout

| Path | Contents |
| --- | --- |
| `cmd/mockfix/` | Server entry point |
| `internal/config/` | Config model, YAML loading, validation, hot reload, versioning |
| `internal/fixserver/` | Client and monitor acceptors, MsgType router |
| `internal/marketdata/` | Price models, scheduler, V/W/X/Y subscriptions, conflating and batching publisher |
| `internal/order/` | Order state, rule engine, action chains, slippage, D/F/G/H handlers, drop copy |
| `internal/sched/` | Single-goroutine scheduler for delayed actions |
| `internal/app/` | Wiring and startup/shutdown order; end-to-end tests |
| `internal/clock/` | Real and fake clocks for time-based tests |
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

## Market data

Each symbol ticks at `speed.ticks_per_second` or `speed.interval_ms`, with optional `jitter_ms`. After every model step the mid is reflected into `[min, max]` and rounded to `tick_size`; bid is mid − spread/2 rounded down to a tick and ask is bid + spread. The same `seed` and config give the same price sequence.

| Model | Parameters |
| --- | --- |
| `random_walk` | `volatility`: maximum step in price units (default one tick) |
| `gbm` | `volatility`: σ per √second, `drift`: μ per second |
| `sine` | `sine: { amplitude, period_ms }` around `initial_price`, `volatility`: uniform noise |
| `replay` | `replay: { file, loop }`; CSV with a header naming `bid` and `ask` (or `mid`), one row per tick |

A V with `SubscriptionRequestType=1` gets one W per symbol, then X on every tick (W if `update_mode: snapshot` or `MDUpdateType=0`). `MarketDepth=1` sends top of book; other values send up to `depth_levels`. Unknown symbols and duplicate MDReqIDs get Y. A slow session only receives the latest tick per symbol (conflation), so it never slows down the others. `market_data.batching` groups ticks of several symbols into one X (design doc, section 6.6), globally or per session.

## Orders

Only Market orders (`OrdType=1`) are accepted; other types get `OrdRejReason=11` before any rule runs, as do unknown symbols (`1`), non-positive quantities (`13`) and duplicate ClOrdIDs (`6`).

A new order is matched once against `order_rules` in ascending `priority`; the first match decides its action chain, otherwise `default_action` (built-in default: ack, then fill). Conditions in `match` are ANDed:

| Field | Forms |
| --- | --- |
| `symbol`, `account`, `cl_ord_id`, `comp_id` | value, list, `{ regex: "..." }` |
| `side` | `BUY` or `SELL`, or a list |
| `ord_type` | `MARKET` (the only supported order type) |
| `qty`, `price` | number, or `{ gt, gte, lt, lte, eq }` |
| `tags` | `{ "9001": VIP }`, each value in any string form |
| `probability` | `0.2`: a stable 20% of orders, decided by hashing the rule name, CompID and ClOrdID |

Actions run at their cumulative `delay_ms`: `ack`, `fill` (remaining quantity), `partial_fill` (`ratio` or `qty`, optional `count` fills `delay_ms` apart), `reject` (`reason`, `text`), `cancel`, `expire`, `ignore`, `business_reject`. Fills use `price_mode: market` (ask for buys, bid for sells, plus the symbol's slippage, always against the client) or `price_mode: fixed` with `price`. Slippage types: `fixed` (`ticks`), `percent` (`value`), `random` (`min_ticks`..`max_ticks`, seeded from the symbol `seed`).

Rules with `message_type: F` or `G` take exactly one action: `ack` (accept, optionally after `delay_ms`), `reject` (OrderCancelReject with `reason`) or `ignore`. Without a matching rule a cancel or replace is accepted at once. A cancel drops the order's pending actions; a replace may change only `OrderQty`, and the remaining actions continue with the new quantity. Orders in flight keep their action chain when the rules are reloaded.

With `server.monitor.drop_copy: true`, every ExecutionReport is copied to the monitor session with tag 5001 naming the source session.

`internal/order/testdata/order_flow.golden` holds the exact message sequence of a scripted order flow on a fake clock; after an intended change run `go test ./internal/order -run Golden -update` and review the diff.
