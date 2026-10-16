import {
  CandlestickSeries,
  ColorType,
  createChart,
  createSeriesMarkers,
  LineSeries,
  LineStyle,
  type IChartApi,
  type IPriceLine,
  type ISeriesApi,
  type ISeriesMarkersPluginApi,
  type SeriesMarker,
  type Time,
  type UTCTimestamp,
} from "lightweight-charts";
import { useEffect, useRef, useState } from "react";
import type { FixMessage } from "../fix/parser";
import { stream } from "../fix/stream";
import { useStore } from "../store";
import { CandleBuilder, fixTime, mid, quotesFrom, type Quote } from "./candles";

const css = (name: string) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();

type Props = { symbol: string; min?: number; max?: number; tickSize?: number; intervals?: number[] };

/** Price axis format from the tick size, e.g. 0.00001 -> 5 decimals. */
function priceFormat(tick?: number) {
  if (!tick || !(tick > 0)) return undefined;
  const decimals = Math.max(0, Math.min(10, Math.ceil(-Math.log10(tick) - 1e-9)));
  return { type: "price" as const, precision: decimals, minMove: tick };
}

const HISTORY = 20000;
const LINES_KEY = "mockfix.chart.bidAsk";

function readLinesPref(): boolean {
  try {
    return localStorage.getItem(LINES_KEY) === "1";
  } catch {
    return false;
  }
}

/**
 * Live chart of one symbol from the FIX stream: one candle of the mid per
 * selected pace, updated in place by every tick until the next pace starts;
 * optional bid and ask lines on the same time points; dashed min/max; and
 * markers at fill prices from the drop copies (section 11.3).
 */
export function PriceChart({ symbol, min, max, tickSize, intervals = [1, 5, 15, 60, 300] }: Props) {
  const el = useRef<HTMLDivElement>(null);
  const [interval, setCandleSec] = useState(intervals[0]);
  const [showLines, setShowLines] = useState(readLinesPref);
  const [last, setLast] = useState<Quote>();
  const [rejected, setRejected] = useState<string>();
  const ticks = useRef<Quote[]>([]);
  const api = useRef<{
    chart: IChartApi;
    candles: ISeriesApi<"Candlestick">;
    bid?: ISeriesApi<"Line">;
    ask?: ISeriesApi<"Line">;
    markers: ISeriesMarkersPluginApi<Time>;
    lines: IPriceLine[];
    builder: CandleBuilder;
    lastTime: number;
  }>(null);
  const reports = useStore(stream.reports);

  // Chart setup; rebuilt when the symbol or the candle interval changes.
  useEffect(() => {
    if (!el.current) return;
    const chart = createChart(el.current, {
      autoSize: true,
      layout: { background: { type: ColorType.Solid, color: css("--panel") }, textColor: css("--muted") },
      grid: { vertLines: { color: css("--border") }, horzLines: { color: css("--border") } },
      timeScale: { timeVisible: true, secondsVisible: interval < 60, barSpacing: 12, rightOffset: 4 },
    });
    const candles = chart.addSeries(CandlestickSeries, {
      upColor: css("--buy"),
      downColor: css("--sell"),
      wickUpColor: css("--buy"),
      wickDownColor: css("--sell"),
      borderVisible: false,
      priceFormat: priceFormat(tickSize),
    });
    // Bid and ask are optional and share the candles' time points.
    const line = { lineWidth: 1 as const, priceLineVisible: false, lastValueVisible: false, priceFormat: priceFormat(tickSize) };
    const bid = showLines ? chart.addSeries(LineSeries, { ...line, color: css("--buy") }) : undefined;
    const ask = showLines ? chart.addSeries(LineSeries, { ...line, color: css("--sell") }) : undefined;
    api.current = {
      chart, candles, bid, ask, lines: [],
      markers: createSeriesMarkers(candles, []),
      builder: new CandleBuilder(interval),
      lastTime: 0,
    };
    for (const q of ticks.current) draw(q);
    return () => {
      chart.remove();
      api.current = null;
    };
  }, [symbol, interval, tickSize, showLines]);

  // Dashed min/max lines.
  useEffect(() => {
    const a = api.current;
    if (!a) return;
    a.lines.forEach((l) => a.candles.removePriceLine(l));
    a.lines = [];
    for (const [price, title] of [[min, "min"], [max, "max"]] as const) {
      if (price !== undefined && Number.isFinite(price)) {
        a.lines.push(a.candles.createPriceLine({ price, title, color: css("--muted"), lineStyle: LineStyle.Dashed, lineWidth: 1 }));
      }
    }
  }, [min, max, symbol, interval, showLines]);

  // Live data.
  useEffect(() => {
    ticks.current = [];
    setRejected(undefined);
    return stream.subscribe(symbol, (m: FixMessage) => {
      if (m.msgType === "Y") {
        setRejected(m.get(58) ?? "rejected");
        return;
      }
      const time = fixTime(m.get(52)) ?? Date.now();
      for (const q of quotesFrom(m, time)) {
        if (q.symbol !== symbol) continue;
        const prev = ticks.current.at(-1);
        const full = { ...q, bid: q.bid ?? prev?.bid, ask: q.ask ?? prev?.ask };
        ticks.current.push(full);
        if (ticks.current.length > HISTORY) ticks.current.splice(0, ticks.current.length - HISTORY);
        draw(full);
        setLast(full);
      }
    });
  }, [symbol]);

  // Fill markers from drop copies.
  useEffect(() => {
    const a = api.current;
    if (!a) return;
    const markers: SeriesMarker<Time>[] = [];
    for (const r of reports) {
      if (r.msgType !== "8" || r.get(150) !== "F" || r.get(55) !== symbol) continue;
      const t = fixTime(r.get(60)) ?? fixTime(r.get(52));
      const px = r.num(31);
      if (t === undefined || px === undefined) continue;
      const buy = r.get(54) === "1" || r.get(54) === "3";
      // At the fill price, so slippage against the bid/ask lines shows.
      markers.push({
        time: (Math.floor(t / 1000 / interval) * interval) as UTCTimestamp,
        position: buy ? "atPriceBottom" : "atPriceTop",
        price: px,
        color: buy ? css("--buy") : css("--sell"),
        shape: buy ? "arrowUp" : "arrowDown",
        size: 0.6,
      });
    }
    markers.sort((x, y) => Number(x.time) - Number(y.time));
    a.markers.setMarkers(markers);
  }, [reports, symbol, interval, showLines]);

  function draw(q: Quote) {
    const a = api.current;
    const m = mid(q);
    if (!a || m === undefined || q.time < a.lastTime) return; // series need increasing time
    a.lastTime = q.time;
    // Same bucket: the current candle moves; a new bucket starts a candle.
    const c = a.builder.add(q.time, m);
    const time = c.time as UTCTimestamp;
    a.candles.update({ ...c, time });
    if (q.bid !== undefined) a.bid?.update({ time, value: q.bid });
    if (q.ask !== undefined) a.ask?.update({ time, value: q.ask });
  }

  const toggleLines = () => {
    const next = !showLines;
    setShowLines(next);
    try {
      localStorage.setItem(LINES_KEY, next ? "1" : "0");
    } catch {
      /* private mode: the choice lasts for this page only */
    }
  };

  const label = (i: number) => (i < 60 ? `${i}s` : `${i / 60}m`);

  return (
    <div>
      <div className="row" style={{ justifyContent: "space-between", marginBottom: 6 }}>
        <span className="mono">
          {last ? (
            <>
              <span className="buy">{last.bid}</span> / <span className="sell">{last.ask}</span>
            </>
          ) : rejected ? (
            <span className="err-text">{rejected}</span>
          ) : (
            <span className="muted">waiting for prices…</span>
          )}
        </span>
        <span className="row">
          <label className="row muted" style={{ gap: 4 }}>
            <input type="checkbox" checked={showLines} onChange={toggleLines} />
            Bid/Ask lines
          </label>
          <span className="muted">Candle</span>
          {intervals.map((i) => (
            <button key={i} className={i === interval ? "primary" : ""} onClick={() => setCandleSec(i)} aria-pressed={i === interval}>
              {label(i)}
            </button>
          ))}
        </span>
      </div>
      <div className="chart" ref={el} />
    </div>
  );
}

/** Line chart of a simulated preview (section 11.3). */
export function PreviewChart({ points, intervalMs, min, max }: { points: number[]; intervalMs: number; min?: number; max?: number }) {
  const el = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!el.current) return;
    const chart = createChart(el.current, {
      autoSize: true,
      layout: { background: { type: ColorType.Solid, color: css("--panel") }, textColor: css("--muted") },
      grid: { vertLines: { color: css("--border") }, horzLines: { color: css("--border") } },
      timeScale: { timeVisible: true, secondsVisible: true },
    });
    const series = chart.addSeries(LineSeries, { color: css("--accent"), lineWidth: 1 });
    const start = Math.floor(Date.now() / 1000);
    // One point per second at most: the chart's time unit is a second.
    const step = Math.max(intervalMs / 1000, 1);
    series.setData(points.map((v, i) => ({ time: (start + Math.floor(i * step)) as UTCTimestamp, value: v })).filter(
      (p, i, arr) => i === 0 || p.time !== arr[i - 1].time,
    ));
    for (const [price, title] of [[min, "min"], [max, "max"]] as const) {
      if (price !== undefined && Number.isFinite(price)) {
        series.createPriceLine({ price, title, color: css("--muted"), lineStyle: LineStyle.Dashed, lineWidth: 1 });
      }
    }
    chart.timeScale().fitContent();
    return () => chart.remove();
  }, [points, intervalMs, min, max]);
  return <div className="chart small" ref={el} />;
}
