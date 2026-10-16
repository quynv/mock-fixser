import { describe, expect, it } from "vitest";
import { simulate } from "./models";

const base = { initial_price: 100, min: 90, max: 110, tick_size: 0.01, intervalMs: 200, seed: 7 };

describe("simulate", () => {
  for (const model of ["random_walk", "gbm", "sine"]) {
    it(`${model} stays in [min, max] on the tick grid`, () => {
      const pts = simulate({ ...base, model, volatility: 2, sine: { amplitude: 15, period_ms: 5000 } }, 2000);
      expect(pts).toHaveLength(2000);
      for (const p of pts) {
        expect(p).toBeGreaterThanOrEqual(90);
        expect(p).toBeLessThanOrEqual(110);
        expect(Math.abs(p / 0.01 - Math.round(p / 0.01))).toBeLessThan(1e-6);
      }
    });
  }

  it("is repeatable with the same seed", () => {
    const a = simulate({ ...base, model: "gbm", volatility: 0.1 }, 50);
    expect(simulate({ ...base, model: "gbm", volatility: 0.1 }, 50)).toEqual(a);
  });

  it("returns nothing for unusable parameters", () => {
    expect(simulate({ ...base, model: "gbm", max: 80 }, 10)).toEqual([]);
  });
});
