// Turns market data frames into top-of-book quotes and quotes into candles
// (design doc, section 11.3).
import type { FixMessage } from "../fix/parser";

export type Quote = { symbol: string; bid?: number; ask?: number; time: number };

/** Top-of-book quotes (level 1) from a W or X, one per symbol. */
export function quotesFrom(m: FixMessage, now = Date.now()): Quote[] {
  const bySymbol = new Map<string, Quote>();
  const top = m.get(55);
  for (const e of m.group(268)) {
    const pos = e.num(290) ?? 1;
    if (pos !== 1) continue;
    const symbol = e.get(55) ?? top;
    const px = e.num(270);
    if (!symbol || px === undefined) continue;
    const q = bySymbol.get(symbol) ?? { symbol, time: now };
    if (e.get(269) === "0") q.bid = px;
    if (e.get(269) === "1") q.ask = px;
    bySymbol.set(symbol, q);
  }
  return [...bySymbol.values()];
}

export type Candle = { time: number; open: number; high: number; low: number; close: number };

/** Builds candles of intervalSec seconds; time is the bucket start in seconds. */
export class CandleBuilder {
  private readonly intervalSec: number;
  private current?: Candle;

  constructor(intervalSec: number) {
    this.intervalSec = intervalSec;
  }

  /** Adds a price at timeMs; returns the candle to draw (new or updated). */
  add(timeMs: number, price: number): Candle {
    const bucket = Math.floor(timeMs / 1000 / this.intervalSec) * this.intervalSec;
    const c = this.current;
    if (!c || bucket > c.time) {
      this.current = { time: bucket, open: price, high: price, low: price, close: price };
    } else {
      c.high = Math.max(c.high, price);
      c.low = Math.min(c.low, price);
      c.close = price;
    }
    return { ...this.current! };
  }
}

export function mid(q: Quote): number | undefined {
  if (q.bid !== undefined && q.ask !== undefined) return (q.bid + q.ask) / 2;
  return q.bid ?? q.ask;
}

/** FIX UTCTimestamp (20261016-03:12:45.123) to epoch ms. */
export function fixTime(v: string | undefined): number | undefined {
  const m = v?.match(/^(\d{4})(\d{2})(\d{2})-(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?$/);
  if (!m) return undefined;
  const ms = m[7] ? Number(m[7].slice(0, 3).padEnd(3, "0")) : 0;
  return Date.UTC(+m[1], +m[2] - 1, +m[3], +m[4], +m[5], +m[6], ms);
}

