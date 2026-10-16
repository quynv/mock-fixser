import { describe, expect, it } from "vitest";
import { condLabel, fromMatch, timeline, toMatch } from "./rules";

describe("match conversion", () => {
  const match = {
    symbol: "AAPL",
    side: ["BUY", "SELL"],
    cl_ord_id: { regex: "^TIMEOUT-" },
    qty: { gt: 1000, lte: 5000 },
    price: 10,
    tags: { "9001": "VIP" },
    in_list: true,
    probability: 0.2,
  };

  it("round-trips", () => {
    expect(toMatch(fromMatch(match))).toEqual(match);
  });

  it("labels conditions", () => {
    const rows = fromMatch(match);
    expect(rows.map(condLabel)).toContain("qty > 1000");
    expect(rows.map(condLabel)).toContain("tag 9001 = VIP");
    expect(rows.map(condLabel)).toContain("side ∈ [BUY, SELL]");
  });
});

describe("timeline", () => {
  it("adds delays cumulatively and splits partial fills", () => {
    const t = timeline([
      { type: "ack", delay_ms: 50 },
      { type: "partial_fill", ratio: 0.4, count: 2, delay_ms: 300 },
      { type: "fill", delay_ms: 800 },
    ]);
    expect(t.steps.map((s) => s.at)).toEqual([50, 350, 650, 1450]);
    expect(t.total).toBe(1450);
  });
});
