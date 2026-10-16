import { describe, expect, it } from "vitest";
import { fixTime } from "./candles";

describe("fixTime", () => {
  it("parses FIX UTC timestamps", () => {
    expect(fixTime("20261016-03:12:45.123")).toBe(Date.UTC(2026, 9, 16, 3, 12, 45, 123));
    expect(fixTime("20261016-03:12:45.123456")).toBe(Date.UTC(2026, 9, 16, 3, 12, 45, 123));
    expect(fixTime("20261016-03:12:45")).toBe(Date.UTC(2026, 9, 16, 3, 12, 45));
    expect(fixTime("bad")).toBeUndefined();
  });
});
