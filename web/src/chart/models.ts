// Browser copies of the price models of section 6.1 for the Symbols
// preview. They show the shape of the parameters being edited; they are
// not expected to match the server number for number (section 11.3).

export type ModelParams = {
  model: string;
  initial_price: number;
  min: number;
  max: number;
  tick_size: number;
  volatility?: number;
  drift?: number;
  sine?: { amplitude?: number; period_ms?: number };
  intervalMs: number;
  seed?: number;
};

/** mulberry32: small seeded PRNG. */
function rng(seed: number) {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

function normal(r: () => number) {
  const u = Math.max(r(), 1e-12);
  return Math.sqrt(-2 * Math.log(u)) * Math.cos(2 * Math.PI * r());
}

function reflect(p: number, lo: number, hi: number) {
  for (let i = 0; i < 4 && (p > hi || p < lo); i++) {
    if (p > hi) p = 2 * hi - p;
    if (p < lo) p = 2 * lo - p;
  }
  return Math.min(Math.max(p, lo), hi);
}

/** Simulates n ticks; returns mids. Returns [] when params are unusable. */
export function simulate(p: ModelParams, n: number): number[] {
  if (!(p.max > p.min) || !(p.tick_size > 0) || !(p.intervalMs > 0)) return [];
  const r = rng(p.seed ?? 1);
  const dt = p.intervalMs / 1000;
  const vol = p.volatility ?? 0;
  const out: number[] = [];
  let price = Math.min(Math.max(p.initial_price, p.min), p.max);
  for (let i = 1; i <= n; i++) {
    switch (p.model) {
      case "gbm":
        price *= Math.exp(((p.drift ?? 0) - (vol * vol) / 2) * dt + vol * Math.sqrt(dt) * normal(r));
        break;
      case "sine": {
        const period = (p.sine?.period_ms ?? 0) / 1000;
        const amp = p.sine?.amplitude ?? 0;
        price = p.initial_price + (period > 0 ? amp * Math.sin((2 * Math.PI * i * dt) / period) : 0) + (r() * 2 - 1) * vol;
        break;
      }
      default: // random_walk
        price += (r() * 2 - 1) * (vol || p.tick_size);
    }
    price = Math.round(reflect(price, p.min, p.max) / p.tick_size) * p.tick_size;
    out.push(price);
  }
  return out;
}
