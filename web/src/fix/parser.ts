// A small FIX parser for frames from /ws: one message per frame, fields
// separated by | (design doc, section 11.4). Names, enums and repeating
// groups come from dict.json, generated from spec/FIX44.xml at build time.
import dictJSON from "./dict.json";

type FieldDef = { n: string; t: string; e?: Record<string, string> };
type GroupDef = { d: number; m: number[] };
type Dict = {
  fields: Record<string, FieldDef>;
  messages: Record<string, string>;
  groups: Record<string, Record<string, GroupDef>>;
};

const dict = dictJSON as Dict;
const memberSets = new Map<GroupDef, Set<number>>();

export type Field = { tag: number; value: string };

export class FixMessage {
  readonly fields: Field[];
  readonly raw: string;

  constructor(raw: string, fields: Field[]) {
    this.raw = raw;
    this.fields = fields;
  }

  get msgType(): string {
    return this.get(35) ?? "";
  }

  /** First value of a tag at the top level. */
  get(tag: number): string | undefined {
    return this.fields.find((f) => f.tag === tag)?.value;
  }

  num(tag: number): number | undefined {
    const v = this.get(tag);
    return v === undefined ? undefined : Number(v);
  }

  /** Entries of the repeating group counted by countTag. */
  group(countTag: number): FixGroupEntry[] {
    return splitGroup(this.msgType, this.fields, countTag);
  }
}

export class FixGroupEntry {
  readonly fields: Field[];
  private readonly msgType: string;

  constructor(msgType: string, fields: Field[]) {
    this.msgType = msgType;
    this.fields = fields;
  }

  get(tag: number): string | undefined {
    return this.fields.find((f) => f.tag === tag)?.value;
  }

  num(tag: number): number | undefined {
    const v = this.get(tag);
    return v === undefined ? undefined : Number(v);
  }

  group(countTag: number): FixGroupEntry[] {
    return splitGroup(this.msgType, this.fields, countTag);
  }
}

export function parseFix(raw: string, sep = "|"): FixMessage {
  const fields: Field[] = [];
  for (const part of raw.split(sep)) {
    const i = part.indexOf("=");
    if (i <= 0) continue;
    const tag = Number(part.slice(0, i));
    if (!Number.isInteger(tag)) continue;
    fields.push({ tag, value: part.slice(i + 1) });
  }
  return new FixMessage(raw, fields);
}

function splitGroup(msgType: string, fields: Field[], countTag: number): FixGroupEntry[] {
  const start = fields.findIndex((f) => f.tag === countTag);
  if (start < 0) return [];
  const def = dict.groups[msgType]?.[String(countTag)];
  const count = Number(fields[start].value);
  const entries: FixGroupEntry[] = [];
  if (!def) return entries;
  let members = memberSets.get(def);
  if (!members) {
    members = new Set(def.m);
    memberSets.set(def, members);
  }
  let cur: Field[] | null = null;
  for (let i = start + 1; i < fields.length; i++) {
    const f = fields[i];
    if (f.tag === def.d) {
      if (entries.length === count) break;
      cur = [f];
      entries.push(new FixGroupEntry(msgType, cur));
    } else if (cur && members.has(f.tag)) {
      cur.push(f);
    } else {
      break; // first field after the group
    }
  }
  return entries;
}

export function fieldName(tag: number): string {
  return dict.fields[String(tag)]?.n ?? String(tag);
}

export function enumName(tag: number, value: string): string | undefined {
  return dict.fields[String(tag)]?.e?.[value];
}

export function messageName(msgType: string): string {
  return dict.messages[msgType] ?? msgType;
}

export type Mirror = { session: string; dir: string; time: string; raw: string; outer: FixMessage };

/**
 * Splits a U1 MirrorLog frame. Its MirrorRawMsg (5003) is a whole FIX
 * message whose separators are also |, so it is cut out by position (it
 * runs up to MirrorTime, 5004) before the outer fields are parsed.
 */
export function parseMirror(frame: string): Mirror {
  const start = frame.indexOf("|5003=");
  const end = frame.indexOf("|5004=", start + 1);
  if (start < 0 || end < 0) {
    const m = parseFix(frame);
    return { session: m.get(5001) ?? "", dir: m.get(5002) ?? "", time: m.get(5004) ?? "", raw: m.get(5003) ?? "", outer: m };
  }
  const raw = frame.slice(start + 6, end).replace(/\|$/, "");
  const outer = parseFix(frame.slice(0, start) + frame.slice(end));
  return { session: outer.get(5001) ?? "", dir: outer.get(5002) ?? "", time: outer.get(5004) ?? "", raw, outer };
}
