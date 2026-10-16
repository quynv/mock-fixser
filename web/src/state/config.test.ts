import { describe, expect, it } from "vitest";
import { parseDocument } from "yaml";

// The draft edits rely on yaml Documents keeping comments and flow style.
describe("YAML document edits", () => {
  const text = `# header comment
version: 3
symbols:
  # main pair
  - { symbol: EURUSD, min: 1.07, max: 1.1, speed: { ticks_per_second: 5 } }
order_rules:
  - name: a # keep me
    priority: 1
`;

  it("sets nested values in place", () => {
    const doc = parseDocument(text);
    doc.setIn(["symbols", 0, "max"], 1.2);
    doc.setIn(["symbols", 0, "speed", "jitter_ms"], 50);
    doc.setIn(["order_rules", 0, "priority"], 10);
    const out = doc.toString({ lineWidth: 0 });
    expect(out).toContain("# header comment");
    expect(out).toContain("# main pair");
    expect(out).toContain("# keep me");
    expect(out).toContain("max: 1.2");
    expect(out).toContain("jitter_ms: 50");
    expect(out).toContain("priority: 10");
  });

  it("adds and deletes list items", () => {
    const doc = parseDocument(text);
    doc.addIn(["symbols"], { symbol: "GBPUSD", min: 1.2, max: 1.4 });
    doc.deleteIn(["symbols", 0]);
    const js = doc.toJS() as { symbols: { symbol: string }[] };
    expect(js.symbols.map((s) => s.symbol)).toEqual(["GBPUSD"]);
  });
});
