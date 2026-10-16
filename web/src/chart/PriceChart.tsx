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

/**
 * Live chart of one symbol from the FIX stream: candles of the mid, bid and
 * ask lines, dashed min/max, and markers at fills from the drop copies, so
 * slippage against the feed is visible (section 11.3).
 */
export function PriceChart({ symbol, min, max, tickSize, intervals = [1, 5, 60] }: Props) {
  const el = useRef<HTMLDivElement>(null);
  const [interval, setCandleSec] = useState(intervals[0]);
  const [last, setLast] = useState<Quote>();
  const [rejected, setRejected] = useState<string>();
  const ticks = useRef<Quote[]>([]);
  const api = useRef<{
    chart: IChartApi;
    candles: ISeriesApi<"Candlestick">;
    bid: ISeriesApi<"Line">;
    ask: ISeriesApi<"Line">;
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
      timeScale: { timeVisible: true, secondsVisible: interval < 60 },
    });
    const candles = chart.addSeries(CandlestickSeries, {
      upColor: css("--buy"),
      downColor: css("--sell"),
      wickUpColor: css("--buy"),
      wickDownColor: css("--sell"),
      borderVisible: false,
      priceFormat: priceFormat(tickSize),
    });
    const line = { lineWidth: 1 as const, priceLineVisible: false, lastValueVisible: false, priceFormat: priceFormat(tickSize) };
    const bid = chart.addSeries(LineSeries, { ...line, color: css("--buy") });
    const ask = chart.addSeries(LineSeries, { ...line, color: css("--sell") });
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
  }, [symbol, interval, tickSize]);

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
  }, [min, max, symbol, interval]);

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
  }, [reports, symbol, interval]);

  function draw(q: Quote) {
    const a = api.current;
    const m = mid(q);
    if (!a || m === undefined || q.time < a.lastTime) return; // series need increasing time
    a.lastTime = q.time;
    const sec = Math.floor(q.time / 1000) as UTCTimestamp;
    const c = a.builder.add(q.time, m);
    a.candles.update({ ...c, time: c.time as UTCTimestamp });
    if (q.bid !== undefined) a.bid.update({ time: sec, value: q.bid });
    if (q.ask !== undefined) a.ask.update({ time: sec, value: q.ask });
  }

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
          {intervals.map((i) => (
            <button key={i} className={i === interval ? "primary" : ""} onClick={() => setCandleSec(i)}>
              {i < 60 ? `${i}s` : `${i / 60}m`}
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
