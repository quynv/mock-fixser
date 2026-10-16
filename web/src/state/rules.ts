// Converts order rule `match` sections to editable condition rows and back
// (design doc, section 7.2), and computes action timelines.

export type Cond = { field: string; op: string; value: string; tag?: string };

export const STRING_FIELDS = ["symbol", "account", "cl_ord_id", "comp_id", "list_id"];
export const NUMBER_FIELDS = ["qty", "price", "list_seq_no"];
export const NUMBER_OPS = ["gt", "gte", "lt", "lte", "eq"];
export const FIELDS = [...STRING_FIELDS, "side", "ord_type", "time_in_force", ...NUMBER_FIELDS, "tags", "in_list", "probability"];
export const ORD_TYPES = ["MARKET", "LIMIT", "STOP", "STOP_LIMIT"];
export const TIFS = ["DAY", "GTC", "IOC", "FOK"];

export function opsFor(field: string): string[] {
  if (NUMBER_FIELDS.includes(field)) return NUMBER_OPS;
  if (field === "side" || field === "ord_type" || field === "time_in_force") return ["eq", "in"];
  if (field === "probability" || field === "in_list") return ["eq"];
  return ["eq", "in", "regex"];
}

function strValue(v: unknown): { op: string; value: string } {
  if (Array.isArray(v)) return { op: "in", value: v.join(", ") };
  if (v && typeof v === "object" && "regex" in v) return { op: "regex", value: String((v as { regex: unknown }).regex) };
  return { op: "eq", value: String(v) };
}

export function fromMatch(match: Record<string, unknown> | undefined): Cond[] {
  const rows: Cond[] = [];
  for (const [field, v] of Object.entries(match ?? {})) {
    if (NUMBER_FIELDS.includes(field)) {
      if (v && typeof v === "object") {
        for (const [op, n] of Object.entries(v)) rows.push({ field, op, value: String(n) });
      } else {
        rows.push({ field, op: "eq", value: String(v) });
      }
    } else if (field === "tags") {
      for (const [tag, tv] of Object.entries((v as Record<string, unknown>) ?? {})) rows.push({ field, tag, ...strValue(tv) });
    } else {
      rows.push({ field, ...strValue(v) });
    }
  }
  return rows;
}

function parseStr(op: string, value: string): unknown {
  if (op === "in") return value.split(",").map((s) => s.trim()).filter(Boolean);
  if (op === "regex") return { regex: value };
  return value;
}

const num = (s: string) => (s.trim() === "" || Number.isNaN(Number(s)) ? s : Number(s));

export function toMatch(rows: Cond[]): Record<string, unknown> {
  const m: Record<string, unknown> = {};
  for (const r of rows) {
    if (NUMBER_FIELDS.includes(r.field)) {
      const prev = m[r.field];
      if (r.op === "eq" && prev === undefined) {
        m[r.field] = num(r.value);
      } else {
        const obj: Record<string, unknown> =
          prev && typeof prev === "object" ? (prev as Record<string, unknown>) : prev === undefined ? {} : { eq: prev };
        obj[r.op] = num(r.value);
        m[r.field] = obj;
      }
    } else if (r.field === "tags") {
      const tags = (m.tags as Record<string, unknown>) ?? {};
      tags[r.tag ?? ""] = parseStr(r.op, r.value);
      m.tags = tags;
    } else if (r.field === "in_list") {
      m.in_list = r.value === "true";
    } else if (r.field === "probability") {
      m.probability = num(r.value);
    } else {
      m[r.field] = parseStr(r.op, r.value);
    }
  }
  return m;
}

/** Short text for a condition chip. */
export function condLabel(c: Cond): string {
  const sym: Record<string, string> = { eq: "=", gt: ">", gte: "≥", lt: "<", lte: "≤", in: "∈", regex: "~" };
  const name = c.field === "tags" ? `tag ${c.tag}` : c.field;
  return `${name} ${sym[c.op] ?? c.op} ${c.op === "in" ? `[${c.value}]` : c.value}`;
}

export type Action = { type: string } & Record<string, unknown>;
export type TimelineStep = { label: string; at: number };

/** Cumulative times of an action chain; partial_fill with count repeats. */
export function timeline(actions: Action[] | undefined): { steps: TimelineStep[]; total: number } {
  const steps: TimelineStep[] = [];
  let at = 0;
  for (const a of actions ?? []) {
    const delay = Number(a.delay_ms ?? 0) || 0;
    const count = a.type === "partial_fill" ? Math.max(Number(a.count ?? 1) || 1, 1) : 1;
    for (let i = 0; i < count; i++) {
      at += delay;
      steps.push({ label: count > 1 ? `${a.type} ${i + 1}/${count}` : a.type, at });
    }
  }
  return { steps, total: at };
}
