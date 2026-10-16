# mockfix

A simulated FIX 4.4 server written in Go. It streams prices for configured symbols, accepts orders and answers with ExecutionReports according to predefined rules. See [the design document](./docs/Design%20Document.md) for the full specification.

Status: phase 4 (monitor, gateway, REST). Everything of phases 1–3 plus the Admin REST API, runtime commands, Prometheus metrics, the U1 log mirror and the FIX gateway with its WebSocket hub at `/ws`. The Web UI comes in phase 5.

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
| `internal/order/` | Order state, rule engine, action chains, slippage, D/F/G/H, order lists E/K/M, mass cancel q, drop copy |
| `internal/sched/` | Single-goroutine scheduler for delayed actions |
| `internal/monitor/` | U1 log mirror to the monitor session, U2 control |
| `internal/gateway/` | UI_MONITOR initiator and WebSocket hub (`/ws`) |
| `internal/admin/` | REST API, `/healthz`, `/metrics` |
| `internal/metrics/` | Prometheus metrics |
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
| `in_list` | `true` for orders that arrived in a NewOrderList |
| `list_id` | value, list, `{ regex: "..." }` |
| `list_seq_no` | number, or `{ gt, gte, lt, lte, eq }` (list orders only) |

Actions run at their cumulative `delay_ms`: `ack`, `fill` (remaining quantity), `partial_fill` (`ratio` or `qty`, optional `count` fills `delay_ms` apart), `reject` (`reason`, `text`), `cancel`, `expire`, `ignore`, `business_reject`. Fills use `price_mode: market` (ask for buys, bid for sells, plus the symbol's slippage, always against the client) or `price_mode: fixed` with `price`. Slippage types: `fixed` (`ticks`), `percent` (`value`), `random` (`min_ticks`..`max_ticks`, seeded from the symbol `seed`).

Rules with `message_type: F` or `G` take exactly one action: `ack` (accept, optionally after `delay_ms`), `reject` (OrderCancelReject with `reason`) or `ignore`. Without a matching rule a cancel or replace is accepted at once. A cancel drops the order's pending actions; a replace may change only `OrderQty`, and the remaining actions continue with the new quantity. Orders in flight keep their action chain when the rules are reloaded.

With `server.monitor.drop_copy: true`, every ExecutionReport is copied to the monitor session with tag 5001 naming the source session.

### Order lists and mass cancel

A NewOrderList (E) is checked as a whole first: TotNoOrders must match the entries, ClOrdIDs must be unique in the list and the session, the ListID must be new in the session, and the list may hold at most `order_lists.max_orders_per_list` orders. A failing list gets ListStatus (N) with `ListOrderStatus=7` and `ListStatusText`, and no order is created. Then `list_rules` run (match `tot_no_orders`, `list_id`, `comp_id`; action `reject_list` with `text`, or `delay_ack` with `delay_ms`), the N Ack is sent (`ack: list_status`), and each order goes through `order_rules` like a D.

`report` chooses how list orders report: `per_order_er` (ExecutionReports carrying ListID), `list_status` (an aggregated N every `list_status_interval_ms`, only when something changed) or `both`. With `final_list_status` (default true) an N with `ListStatusType=5` follows once every order is closed. Responses to a client's own F, G, H, K or q are always ExecutionReports. ListCancelRequest (K) cancels the list's open orders and answers with N; ListStatusRequest (M) returns the current N. `order_lists.enabled: false` answers E, K and M with BusinessMessageReject.

OrderMassCancelRequest (q) supports `MassCancelRequestType` 1 (by symbol) and 7 (all orders of the session); it answers with OrderMassCancelReport (r) listing the affected orders, then an ExecutionReport per canceled order. Other types are rejected in r.

`internal/order/testdata/order_flow.golden` holds the exact message sequence of a scripted order flow on a fake clock; after an intended change run `go test ./internal/order -run Golden -update` and review the diff.

## Admin API and WebSocket

The HTTP server (`server.http.listen`, default `:8080`) serves the REST API under `/api`, `/ws`, `/healthz` and `/metrics`; `--no-ui` skips it and the gateway. Errors are `{ code, message, details[] }`, with validation details carrying the path and YAML line.

| Method | Path | |
| --- | --- | --- |
| GET | `/api/config`, `/api/config.yaml` | Running config; `ETag` is the version |
| PUT | `/api/config` | Replace the config (YAML, or JSON with `Content-Type: application/json`); requires `If-Match: "<version>"`, 409 on a stale version |
| POST | `/api/config/validate` | Validate only |
| GET | `/api/config/history` | Saved versions |
| POST | `/api/config/rollback/{version}` | Apply a saved version as a new version |
| GET | `/api/schema` | JSON Schema of the config |
| GET, POST | `/api/symbols` | List (with runtime state), add |
| GET, PUT, DELETE | `/api/symbols/{symbol}` | View, edit, delete |
| POST | `/api/symbols/{symbol}/pause`, `/resume` | Runtime: stop or restart ticks |
| POST | `/api/symbols/{symbol}/price` | Runtime: `{ "mid": 1.09 }` |
| POST | `/api/symbols/{symbol}/spike` | Runtime: `{ "percent": 2, "direction": "up" }` |
| GET, POST | `/api/rules` | List by priority, add |
| PUT, DELETE | `/api/rules/{name}` | Edit (may rename), delete |
| PUT | `/api/rules/order` | `{ "order": ["a", "b"] }`: priorities 10, 20, ... |
| POST | `/api/rules/test` | Sample order → matching rule and its action chain with cumulative times |
| GET | `/api/sessions` | Sessions, logon state, sequence numbers |
| POST | `/api/sessions/{id}/logout` | `{id}` is the full session ID or the client CompID |
| GET | `/api/orders?session=&symbol=&status=&limit=` | `status`: `open`, `closed` or a FIX OrdStatus |

Partial edits (symbols, rules) change only that part of the YAML file, keeping comments elsewhere, and go through the same validate-and-swap path, so each creates a new version; `If-Match` is optional for them. Runtime commands take effect at once and are not written to the YAML.

`/ws` carries raw FIX, one message per text frame with SOH replaced by `|`. The browser may send only `35=V|262=<id>|263=1|55=<symbol>` (subscribe; several `55` allowed), `35=V|262=<id>|263=2` (unsubscribe) and `35=U2|5005=log|5006=on|off` (U1 log mirror, off by default). Tabs watching the same symbol share one upstream subscription, and a new tab first receives the last `history_ticks` frames. Drop copies (8, N) go to every tab. A tab that cannot keep up is closed with code 1008.
