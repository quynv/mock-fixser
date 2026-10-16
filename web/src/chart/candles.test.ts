import { describe, expect, it } from "vitest";
import { parseFix } from "../fix/parser";
import { CandleBuilder, quotesFrom } from "./candles";

describe("quotesFrom", () => {
  it("takes level 1 bid and ask per symbol", () => {
    const x = parseFix(
      "35=X|262=a|268=4|279=1|269=0|55=A|270=10|290=1|279=1|269=0|55=A|270=9.9|290=2|279=1|269=1|55=A|270=10.2|290=1|279=1|269=0|55=B|270=5|290=1|",
    );
    expect(quotesFrom(x, 1)).toEqual([
      { symbol: "A", bid: 10, ask: 10.2, time: 1 },
      { symbol: "B", bid: 5, time: 1 },
    ]);
  });

  it("uses the W symbol when entries carry none", () => {
    const w = parseFix("35=W|262=a|55=AAPL|268=2|269=0|270=229.99|269=1|270=230.01|");
    expect(quotesFrom(w, 1)).toEqual([{ symbol: "AAPL", bid: 229.99, ask: 230.01, time: 1 }]);
  });
});

describe("CandleBuilder", () => {
  it("aggregates into buckets", () => {
    const b = new CandleBuilder(5);
    b.add(10_000, 1);
    b.add(11_000, 3);
    expect(b.add(14_999, 2)).toEqual({ time: 10, open: 1, high: 3, low: 1, close: 2 });
    expect(b.add(15_000, 4)).toEqual({ time: 15, open: 4, high: 4, low: 4, close: 4 });
  });
});
