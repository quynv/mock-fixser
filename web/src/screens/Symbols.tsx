import { useMemo, useState } from "react";
import { api, HttpError, type SymbolState } from "../api";
import { simulate } from "../chart/models";
import { PreviewChart, PriceChart } from "../chart/PriceChart";
import { Field, Input, Panel, Select, toNum, usePoll } from "../components/ui";
import { go } from "../router";
import { draftJS, editDraft, itemIndex, notify, setAt, useConfig } from "../state/config";

type Sym = Record<string, any>; // eslint-disable-line @typescript-eslint/no-explicit-any

const MODELS = ["random_walk", "gbm", "sine", "replay"];

export function Symbols({ selected }: { selected?: string }) {
  const cfg = useConfig();
  const data = draftJS(cfg.draft);
  const symbols = ((data.symbols as Sym[] | undefined) ?? []).filter(Boolean);
  const runtime = usePoll(api.symbols, 2000).data ?? [];
  const live = new Map(runtime.map((r) => [r.symbol, r.runtime]));
  const [adding, setAdding] = useState("");
  const current = selected ?? symbols[0]?.symbol;

  const add = () => {
    const name = adding.trim().toUpperCase();
    if (!name) return;
    if (symbols.some((s) => s.symbol === name)) {
      notify("error", `${name} already exists`);
      return;
    }
    editDraft((doc) =>
      doc.addIn(["symbols"], {
        symbol: name, initial_price: 100, min: 90, max: 110, tick_size: 0.01, spread: 0.02,
        model: "random_walk", speed: { ticks_per_second: 2 },
      }),
    );
    setAdding("");
    go("symbols", name);
  };

  return (
    <div className="split">
      <Panel title="Symbols">
        <ul className="list">
          {symbols.map((s) => (
            <li key={s.symbol} className={s.symbol === current ? "selected" : ""} onClick={() => go("symbols", s.symbol)}>
              <span>{s.symbol}</span>
              <span className="muted">
                {live.get(s.symbol)?.paused ? "paused" : live.has(s.symbol) ? (s.model ?? "random_walk") : "not applied"}
              </span>
            </li>
          ))}
        </ul>
        <div className="row" style={{ marginTop: 8 }}>
          <button className="runtime" title="Send QuoteCancel (Z) for all quotes to every subscribed client" onClick={() => void quoteCancel([])}>
            ⚡ Cancel all quotes
          </button>
        </div>
        <div className="row" style={{ marginTop: 8 }}>
          <input placeholder="new symbol" value={adding} onChange={(e) => setAdding(e.target.value)} onKeyDown={(e) => e.key === "Enter" && add()} />
          <button onClick={add}>Add</button>
        </div>
      </Panel>
      {current && symbols.some((s) => s.symbol === current) ? (
        <SymbolEditor
          key={current}
          sym={symbols.find((s) => s.symbol === current)!}
          index={itemIndex(data, "symbols", "symbol", current)}
          runtime={live.get(current)}
          applied={live.has(current)}
        />
      ) : (
        <p className="muted">{symbols.length ? "Select a symbol." : "No symbols yet. Add one."}</p>
      )}
    </div>
  );
}

function SymbolEditor({ sym, index, runtime, applied }: { sym: Sym; index: number; runtime?: SymbolState; applied: boolean }) {
  const p = `symbols[${index}]`;
  const at = (...keys: string[]) => ["symbols", index, ...keys];
  const set = (keys: string[], v: unknown) => setAt(at(...keys), v);
  const [preview, setPreview] = useState(false);
  const model = sym.model ?? "random_walk";
  const speedMode = sym.speed?.interval_ms !== undefined ? "interval_ms" : "ticks_per_second";
  const slip = sym.slippage?.type ?? "none";

  const remove = () => {
    if (!confirm(`Delete ${sym.symbol} from the draft?`)) return;
    editDraft((doc) => doc.deleteIn(["symbols", index]));
    go("symbols");
  };

  const intervalMs = sym.speed?.interval_ms ?? (sym.speed?.ticks_per_second ? 1000 / sym.speed.ticks_per_second : 500);
  const points = useMemo(
    () =>
      preview
        ? simulate(
            {
              model, initial_price: sym.initial_price, min: sym.min, max: sym.max, tick_size: sym.tick_size,
              volatility: sym.volatility, drift: sym.drift, sine: sym.sine, intervalMs, seed: sym.seed,
            },
            600,
          )
        : [],
    [preview, model, sym, intervalMs],
  );

  return (
    <div className="grid">
      {applied && (
        <Panel title="Live">
          <PriceChart symbol={sym.symbol} min={sym.min} max={sym.max} tickSize={sym.tick_size} />
          <RuntimeCommands symbol={sym.symbol} runtime={runtime} />
        </Panel>
      )}
      <Panel
        title={`${sym.symbol}${applied ? "" : " (not applied yet)"}`}
        actions={
          <button className="danger" onClick={remove}>
            Delete
          </button>
        }
      >
        <div className="form">
          <Field label="Initial price" path={`${p}.initial_price`}>
            <Input type="number" value={sym.initial_price} onCommit={(v) => set(["initial_price"], toNum(v))} />
          </Field>
          <Field label="Min" path={`${p}.min`}>
            <Input type="number" value={sym.min} onCommit={(v) => set(["min"], toNum(v))} />
          </Field>
          <Field label="Max" path={`${p}.max`}>
            <Input type="number" value={sym.max} onCommit={(v) => set(["max"], toNum(v))} />
          </Field>
          <Field label="Tick size" path={`${p}.tick_size`}>
            <Input type="number" value={sym.tick_size} onCommit={(v) => set(["tick_size"], toNum(v))} />
          </Field>
          <Field label="Spread" path={`${p}.spread`} hint="multiple of the tick size">
            <Input type="number" value={sym.spread} onCommit={(v) => set(["spread"], toNum(v))} />
          </Field>
          <Field label="Depth levels" path={`${p}.depth_levels`}>
            <Input type="number" value={sym.depth_levels} placeholder="1" onCommit={(v) => set(["depth_levels"], toNum(v))} />
          </Field>
          <Field label="Update mode" path={`${p}.update_mode`}>
            <Select value={sym.update_mode ?? ""} allowEmpty="incremental (default)" options={["incremental", "snapshot"]} onChange={(v) => set(["update_mode"], v)} />
          </Field>
          <Field label="Quote condition (276)" path={`${p}.quote_condition`} hint="e.g. A open, B closed; empty: not sent">
            <Input value={sym.quote_condition} placeholder="not sent" onCommit={(v) => set(["quote_condition"], v.trim().toUpperCase())} />
          </Field>
          <Field label="Seed" path={`${p}.seed`} hint="empty: random each start">
            <Input type="number" value={sym.seed} onCommit={(v) => set(["seed"], toNum(v))} />
          </Field>
        </div>

        <h4>Price model</h4>
        <div className="form">
          <Field label="Model" path={`${p}.model`}>
            <Select value={model} options={MODELS} onChange={(v) => set(["model"], v)} />
          </Field>
          <Field
            label="Volatility"
            path={`${p}.volatility`}
            hint={model === "gbm" ? "σ per √second" : model === "sine" ? "noise" : "max step (price)"}
          >
            <Input type="number" value={sym.volatility} onCommit={(v) => set(["volatility"], toNum(v))} />
          </Field>
          {model === "gbm" && (
            <Field label="Drift" path={`${p}.drift`} hint="μ per second">
              <Input type="number" value={sym.drift} onCommit={(v) => set(["drift"], toNum(v))} />
            </Field>
          )}
          {model === "sine" && (
            <>
              <Field label="Amplitude" path={`${p}.sine`}>
                <Input type="number" value={sym.sine?.amplitude} onCommit={(v) => set(["sine", "amplitude"], toNum(v))} />
              </Field>
              <Field label="Period (ms)" path={`${p}.sine.period_ms`}>
                <Input type="number" value={sym.sine?.period_ms} onCommit={(v) => set(["sine", "period_ms"], toNum(v))} />
              </Field>
            </>
          )}
          {model === "replay" && (
            <>
              <Field label="Replay file" path={`${p}.replay`}>
                <Input value={sym.replay?.file} placeholder="data/aapl.csv" onCommit={(v) => set(["replay", "file"], v)} />
              </Field>
              <Field label="Loop" path={`${p}.replay.loop`}>
                <input type="checkbox" checked={!!sym.replay?.loop} onChange={(e) => set(["replay", "loop"], e.target.checked)} />
              </Field>
            </>
          )}
        </div>

        <h4>Speed</h4>
        <div className="form">
          <Field label="Speed" path={`${p}.speed`}>
            <Select
              value={speedMode}
              options={["ticks_per_second", "interval_ms"]}
              onChange={(v) =>
                editDraft((doc) => {
                  const cur = doc.getIn(at("speed", speedMode)) as number | undefined;
                  doc.deleteIn(at("speed", speedMode));
                  const conv = cur ? (v === "interval_ms" ? Math.round(1000 / cur) : Math.round((1000 / cur) * 100) / 100) : 1;
                  doc.setIn(at("speed", v), conv);
                })
              }
            />
          </Field>
          <Field label={speedMode === "interval_ms" ? "Interval (ms)" : "Ticks per second"} path={`${p}.speed.${speedMode}`}>
            <Input type="number" value={sym.speed?.[speedMode]} onCommit={(v) => set(["speed", speedMode], toNum(v))} />
          </Field>
          <Field label="Jitter (ms)" path={`${p}.speed.jitter_ms`}>
            <Input type="number" value={sym.speed?.jitter_ms} onCommit={(v) => set(["speed", "jitter_ms"], toNum(v))} />
          </Field>
        </div>

        <h4>Slippage</h4>
        <div className="form">
          <Field label="Type" path={`${p}.slippage`}>
            <Select
              value={slip}
              options={["none", "fixed", "percent", "random"]}
              onChange={(v) => setAt(at("slippage"), v === "none" ? undefined : { type: v })}
            />
          </Field>
          {slip === "fixed" && (
            <Field label="Ticks" path={`${p}.slippage.ticks`}>
              <Input type="number" value={sym.slippage?.ticks} onCommit={(v) => set(["slippage", "ticks"], toNum(v))} />
            </Field>
          )}
          {slip === "percent" && (
            <Field label="Percent" path={`${p}.slippage.value`}>
              <Input type="number" value={sym.slippage?.value} onCommit={(v) => set(["slippage", "value"], toNum(v))} />
            </Field>
          )}
          {slip === "random" && (
            <>
              <Field label="Min ticks" path={`${p}.slippage.min_ticks`}>
                <Input type="number" value={sym.slippage?.min_ticks} onCommit={(v) => set(["slippage", "min_ticks"], toNum(v))} />
              </Field>
              <Field label="Max ticks" path={`${p}.slippage.max_ticks`}>
                <Input type="number" value={sym.slippage?.max_ticks} onCommit={(v) => set(["slippage", "max_ticks"], toNum(v))} />
              </Field>
            </>
          )}
        </div>
      </Panel>

      <Panel
        title="Preview"
        actions={<button onClick={() => setPreview(!preview)}>{preview ? "Hide" : "Run preview"}</button>}
      >
        {preview ? (
          points.length ? (
            <>
              <PreviewChart points={points} intervalMs={intervalMs} min={sym.min} max={sym.max} />
              <p className="muted">
                Simulated in the browser from the edited parameters: shows the shape only, not the server's exact prices.
              </p>
            </>
          ) : (
            <p className="muted">
              {model === "replay" ? "Replay follows its file; no preview." : "Fix the parameters to see a preview."}
            </p>
          )
        ) : (
          <p className="muted">Try the edited parameters before Apply.</p>
        )}
      </Panel>

    </div>
  );
}

/** QuoteCancel (Z): the clients must subscribe again to get prices. */
async function quoteCancel(symbols: string[]) {
  try {
    const r = await api.quoteCancel(symbols);
    notify(
      r.sessions.length ? "ok" : "info",
      r.sessions.length
        ? `QuoteCancel sent to ${r.sessions.length} session(s): ${r.sessions.join(", ")}`
        : "QuoteCancel: no client session is subscribed",
    );
  } catch (e) {
    notify("error", e instanceof HttpError ? e.body.message : String(e));
  }
}

/** Runtime commands: immediate, not saved to the YAML (sections 6.5, 11.5). */
function RuntimeCommands({ symbol, runtime }: { symbol: string; runtime?: SymbolState }) {
  const [price, setPrice] = useState("");
  const [pct, setPct] = useState("1");
  const run = async (f: () => Promise<SymbolState>, what: string) => {
    try {
      const st = await f();
      notify("ok", `${symbol}: ${what} (mid ${st.mid})`);
    } catch (e) {
      notify("error", e instanceof HttpError ? e.body.message : String(e));
    }
  };
  return (
    <div className="runtime-box" style={{ marginTop: 12 }}>
      <div className="row">
        <strong>⚡ Runtime</strong>
        <span className="note">Takes effect now; not saved to the YAML.</span>
      </div>
      <div className="row" style={{ marginTop: 8 }}>
        {runtime?.paused ? (
          <button className="runtime" onClick={() => void run(() => api.resume(symbol), "resumed")}>
            ▶ Resume
          </button>
        ) : (
          <button className="runtime" onClick={() => void run(() => api.pause(symbol), "paused")}>
            ⏸ Pause
          </button>
        )}
        <input type="number" step="any" placeholder="mid" value={price} onChange={(e) => setPrice(e.target.value)} style={{ width: 110 }} />
        <button className="runtime" disabled={!price} onClick={() => void run(() => api.setPrice(symbol, Number(price)), `price set to ${price}`)}>
          Set price
        </button>
        <input type="number" step="any" value={pct} onChange={(e) => setPct(e.target.value)} style={{ width: 70 }} />
        <span>%</span>
        <button className="runtime" onClick={() => void run(() => api.spike(symbol, Number(pct), "up"), `spike +${pct}%`)}>
          Spike ↑
        </button>
        <button className="runtime" onClick={() => void run(() => api.spike(symbol, Number(pct), "down"), `spike −${pct}%`)}>
          Spike ↓
        </button>
        <button className="runtime" title="Send QuoteCancel (Z) for this symbol and drop its subscriptions" onClick={() => void quoteCancel([symbol])}>
          Quote cancel
        </button>
      </div>
    </div>
  );
}
