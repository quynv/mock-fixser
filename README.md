# mockfix

A simulated FIX 4.4 server written in Go. It streams prices for configured symbols, accepts orders and answers with ExecutionReports according to predefined rules. See [the design document](./docs/Design%20Document.md) for the full specification.

Status: phase 5 (Web UI). Everything of phases 1–4 plus the Web UI at `http://localhost:8080/`, embedded in the binary.

## Requirements

- Go 1.26.9 (pinned in `.go-version` for goenv)
- Node.js 22+ and npm, to build the Web UI (`make build-go` builds without it)
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
| `web/` | React + TypeScript Web UI (Vite); `web/dist` is embedded with `go:embed` |
| `cmd/webdict/` | Generates the browser's FIX dictionary from `spec/FIX44.xml` |
| `fix/` | Go packages generated from `spec/FIX44.xml` (`fix44`, `field`, `tag`, `enum`); not committed, created by `make gen` |

## Commands

The FIX packages in `fix/` are generated, not committed. `make build`, `make test` and `make vet` generate them first (only when `spec/FIX44.xml` changed). Without make, run `go generate ./fix` once after cloning.

```sh
make gen        # generate fix/ from spec/FIX44.xml
make build      # build the Web UI, then a static binary in bin/mockfix with the UI embedded
make build-go   # binary only (no Node.js), with whatever is in web/dist
make test       # go test -race ./...
make test-web   # UI unit tests (Vitest)
make e2e        # Playwright test of the Apply flow against a real mockfix
make docker     # build the mockfix image
```

For UI development, run mockfix, then `cd web && npm run dev`: Vite serves the UI with hot reload and proxies `/api`, `/ws` and `/metrics` to `http://localhost:8080` (override with `MOCKFIX_URL`). Before the first `npm run dev`, generate the dictionary with `make web/src/fix/dict.json`. The first `make e2e` needs `cd web && npx playwright install chromium`.

## Run locally

### Binary

```sh
make build                                   # UI + binary (or make build-go without Node.js)
bin/mockfix --config configs/mock.yaml
```

Run it from the repository root: the sample config refers to `spec/FIX44.xml` and `data/aapl.csv` by relative paths. Flags: `--config`, `--log-level` (debug, info, warn, error), `--no-ui` (no HTTP server or gateway, for CI), `--pprof` (serve `/debug/pprof` on the HTTP port), `--version`.

### Docker

```sh
make docker                                  # builds mockfix:<git version>
docker tag mockfix:$(git describe --tags --always --dirty) mockfix:latest
docker run -d --name mockfix \
  -p 9876:9876 -p 8080:8080 \
  -v "$PWD/configs:/app/configs" \
  -v "$PWD/data:/app/data" \
  mockfix:latest
```

The image reads `/app/configs/mock.yaml`; `data/` holds replay files. Port 9877 (the monitor session) is not published: only the built-in gateway uses it. Applying changes from the UI or the API writes to the mounted `configs/mock.yaml` and creates `configs/history/`; mount a copy of `configs/` to keep the repository file unchanged.

```sh
docker logs -f mockfix                       # logs
docker restart mockfix                       # apply changes reported as restart_required
docker stop mockfix && docker rm mockfix     # stop and remove
```

### Endpoints

| What | Where |
| --- | --- |
| FIX acceptor (clients) | `localhost:9876`, sessions `MOCKSRV` ↔ `CLIENT1`, `CLIENT2` in the sample config |
| Web UI | http://localhost:8080 |
| REST API | http://localhost:8080/api/ (e.g. `/api/sessions`, `/api/config.yaml`) |
| Health, metrics | http://localhost:8080/healthz, http://localhost:8080/metrics |

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

OrdType 1 (Market), 2 (Limit), 3 (Stop) and 4 (Stop limit) are accepted, with TimeInForce 0 (Day, the default), 1 (GTC), 3 (IOC) or 4 (FOK). Other types and times in force get `OrdRejReason=11` before any rule runs; so do unknown symbols (`1`), non-positive quantities (`13`) and duplicate ClOrdIDs (`6`). Limit orders need `Price`, stop orders `StopPx` (`99` otherwise).

A new order is matched once against `order_rules` in ascending `priority`; the first match decides its action chain, otherwise `default_action` (built-in default: ack, then fill). Conditions in `match` are ANDed:

| Field | Forms |
| --- | --- |
| `symbol`, `account`, `cl_ord_id`, `comp_id` | value, list, `{ regex: "..." }` |
| `side` | `BUY` or `SELL`, or a list |
| `ord_type` | `MARKET`, `LIMIT`, `STOP`, `STOP_LIMIT`, or a list |
| `time_in_force` | `DAY`, `GTC`, `IOC`, `FOK`, or a list (no TimeInForce is `DAY`) |
| `qty`, `price` | number, or `{ gt, gte, lt, lte, eq }` |
| `tags` | `{ "9001": VIP }`, each value in any string form |
| `probability` | `0.2`: a stable 20% of orders, decided by hashing the rule name, CompID and ClOrdID |
| `in_list` | `true` for orders that arrived in a NewOrderList |
| `list_id` | value, list, `{ regex: "..." }` |
| `list_seq_no` | number, or `{ gt, gte, lt, lte, eq }` (list orders only) |

Actions run at their cumulative `delay_ms`: `ack`, `fill` (remaining quantity), `partial_fill` (`ratio` or `qty`, optional `count` fills `delay_ms` apart), `reject` (`reason`, `text`), `cancel`, `expire`, `ignore`, `business_reject`. Fill prices (`price_mode`, section 7.4):

| price_mode | Fill price |
| --- | --- |
| `market` (default) | Ask for buys, bid for sells, plus the symbol's slippage (always against the client). A limit order fills only once the market reaches its limit, and never worse than the limit. |
| `market_driven` | Same as `market`; named for rules that wait on limit orders. |
| `limit_immediate` | The order's limit price at once, no slippage (market orders use the market price). |
| `fixed` | The rule's `price`, no slippage. |

A fill step that cannot execute (limit not reached, stop not triggered) waits and is retried on every tick of the symbol. A stop order triggers when the ask (buy) or bid (sell) reaches `StopPx`, then acts as a market or limit order. IOC orders cancel whatever cannot fill when a fill step runs and what the chain leaves open; FOK orders fill completely at once or are canceled (a `partial_fill` step fills everything). A replace may change OrderQty, and Price or StopPx of limit and stop orders; the OrdType cannot change. Day orders do not expire at the end of the day. Slippage types: `fixed` (`ticks`), `percent` (`value`), `random` (`min_ticks`..`max_ticks`, seeded from the symbol `seed`).

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

## Web UI

Screens: Dashboard, Symbols, Order rules, Sessions, Orders, FIX log and YAML. All realtime data reaches the browser over FIX through `/ws`, so the UI sees exactly what clients receive.

Edits only change a draft; nothing reaches the server until **Apply**. The draft is the YAML text itself: the forms change it through a YAML document, so comments and layout in `configs/mock.yaml` survive, and the YAML screen edits the same text in Monaco. The server validates the draft as you type; errors show next to the field and as editor markers, and Apply returns them too. If someone else applies a version meanwhile, a banner offers to reload instead of overwriting. Runtime commands (pause, set price, spike) are styled apart and are not saved to the YAML.

- **Symbols**: live chart (Lightweight Charts) with candles of the mid at 1s, 5s or 1m, bid and ask lines, dashed min/max and markers at fill prices from the drop copies; an editor for every symbol parameter; a browser-side preview of the edited price model (shape only, not the server's exact numbers).
- **Order rules**: drag to reorder priorities, conditions as chips, action chains as timelines with total latency, `default_action`, list rules, and a rule tester that runs a sample order against the draft.
- **FIX log**: the U1 mirror (turned on while the screen is open), filters by session, direction and MsgType, search, pause, and a tag-by-tag view with field and enum names.
- **YAML**: the draft in Monaco, a diff against the running config, and version history with rollback.

## Session faults

Per client session, `faults` simulates problems from each logon (design doc, section 5.4):

```yaml
sessions:
  - sender_comp_id: MOCKSRV
    target_comp_id: CLIENT2
    faults:
      reject_logon: { text: "Try later" }       # answer Logon with Logout
      drop_after: { messages: 100 }             # or { seconds: 30 }: close the TCP connection
      seq_gap: { every: 500 }                   # skip a MsgSeqNum after every 500 application messages sent
      heartbeat_miss: { after_seconds: 10 }     # stop heartbeats, also in answer to TestRequest
```

`POST /api/sessions/{id}/fault` with `{"type": "drop" | "seq_gap" | "heartbeat_miss"}` triggers one at once on a logged-on session; the Sessions screen has buttons for them.

## Load test

`cmd/loadgen` runs the load test of design doc section 13: client sessions that subscribe to every symbol and send orders at a fixed rate. It reports order latency (D to the first ExecutionReport, network included), market data received, and the server's tick rate, conflation, CPU and memory from `/metrics`.

```sh
go run ./cmd/loadgen -gen-config /tmp/load.yaml -sessions 10 -symbols 200 -tps 25
bin/mockfix --config /tmp/load.yaml --log-level warn &
go run ./cmd/loadgen -sessions 10 -symbols 200 -orders 100 -duration 30s
```

On an Apple M3 Pro (11 cores), 10 sessions × 200 symbols × 25 ticks/s with 100 orders/s, without batching. The round trip includes the load generator itself, which parses 5,000 market data messages per second and session before it reads each report:

| | Result | Target (section 2.2) |
| --- | --- | --- |
| Ticks generated | 5,087/s | ≥ 5,000/s |
| Market data delivered | 50,850 messages/s (every tick to every session), 0 conflated | |
| Order handling in the server (`mockfix_order_latency_seconds`) | p99 ≤ 0.1 ms | p99 < 5 ms |
| Order round trip, D to first ExecutionReport, measured by the client | p50 0.22–0.25 ms, p99 3.6–5.6 ms over several runs | |
| Server CPU | 2.4 cores | |
| Server memory | 121 MiB | |

Each session sends through flow control: at most one application message waits for the network, orders go before market data, and a session that stops reading only has its own ticks conflated. Market data and U1 messages are not kept for resend; a ResendRequest gets a gap fill for them.

## FIX versions

Sessions use `server.fix.version` (FIX.4.4) unless they set their own `version`: `FIX.4.2` or `FIX.5.0SP2` (over FIXT.1.1, DefaultApplVerID 9).

```yaml
sessions:
  - { sender_comp_id: MOCKSRV, target_comp_id: CLIENT42, version: FIX.4.2 }
  - { sender_comp_id: MOCKSRV, target_comp_id: CLIENT50, version: FIX.5.0SP2 }
```

Their dictionaries are read next to `data_dictionary`: `spec/FIX42.xml`, and `spec/FIXT11.xml` with `spec/FIX50SP2.xml`. The engines work on the same messages for every version; FIX 4.2 ExecutionReports carry ExecTransType, report fills with ExecType 1 or 2 and answer OrderStatusRequest with ExecTransType 3. Drop copies and the UI stay on FIX 4.4. Sessions of another version than `server.fix.version` are created when the acceptor starts, so adding one or changing a session's version needs a restart (reported as `restart_required`).
