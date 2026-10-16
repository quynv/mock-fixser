import { describe, expect, it } from "vitest";
import { enumName, fieldName, messageName, parseFix, parseMirror } from "./parser";

const x =
  "8=FIX.4.4|9=200|35=X|34=12|49=MOCKSRV|56=UI_MONITOR|262=chart|268=4|" +
  "279=1|269=0|55=EURUSD|270=1.08499|271=300|290=1|" +
  "279=1|269=1|55=EURUSD|270=1.08509|271=200|290=1|" +
  "279=1|269=0|55=GBPUSD|270=1.27|271=100|290=1|" +
  "279=1|269=1|55=GBPUSD|270=1.2701|271=100|290=1|10=123|";

describe("parseFix", () => {
  it("reads top-level fields", () => {
    const m = parseFix(x);
    expect(m.msgType).toBe("X");
    expect(m.get(262)).toBe("chart");
    expect(m.num(34)).toBe(12);
  });

  it("splits repeating groups by the message's delimiter", () => {
    const g = parseFix(x).group(268);
    expect(g).toHaveLength(4);
    expect(g[0].get(55)).toBe("EURUSD");
    expect(g[1].num(270)).toBe(1.08509);
    expect(g[3].get(55)).toBe("GBPUSD");
    expect(g[3].get(10)).toBeUndefined(); // CheckSum is not part of the entry
  });

  it("uses the W delimiter for snapshots", () => {
    const w = parseFix("35=W|262=a|55=AAPL|268=2|269=0|270=229.99|271=1|269=1|270=230.01|271=1|");
    const g = w.group(268);
    expect(g.map((e) => e.get(269))).toEqual(["0", "1"]);
  });

  it("handles nested groups in ListStatus", () => {
    const n = parseFix("35=N|66=L1|429=1|82=1|431=2|83=1|68=2|73=2|11=A|14=0|39=0|151=10|84=0|6=0|11=B|14=0|39=0|151=5|84=0|6=0|");
    expect(n.group(73).map((e) => e.get(11))).toEqual(["A", "B"]);
  });

  it("names tags, enums and messages", () => {
    expect(fieldName(270)).toBe("MDEntryPx");
    expect(fieldName(5003)).toBe("MirrorRawMsg");
    expect(enumName(150, "F")).toBe("TRADE");
    expect(messageName("U1")).toBe("MirrorLog");
    expect(fieldName(99999)).toBe("99999");
  });
});

describe("parseMirror", () => {
  it("keeps the mirrored message whole", () => {
    const frame =
      "8=FIX.4.4|9=180|35=U1|34=7|49=MOCKSRV|52=20261016-03:57:30.612|56=UI_MONITOR|" +
      "5001=FIX.4.4:MOCKSRV->CLIENT1|5002=I|5003=8=FIX.4.4|9=120|35=D|34=3|11=C1|55=EURUSD|10=123|" +
      "5004=20261016-03:57:30.612965|10=200|";
    const m = parseMirror(frame);
    expect(m.session).toBe("FIX.4.4:MOCKSRV->CLIENT1");
    expect(m.dir).toBe("I");
    expect(m.time).toBe("20261016-03:57:30.612965");
    expect(m.raw).toBe("8=FIX.4.4|9=120|35=D|34=3|11=C1|55=EURUSD|10=123");
    expect(parseFix(m.raw).get(11)).toBe("C1");
  });
});
