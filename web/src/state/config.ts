// The config draft (design doc, sections 11.1 and 11.5). The draft is the
// YAML text itself: form screens edit it through a YAML Document so comments
// and layout survive, the YAML screen edits the text directly, and nothing
// reaches the server until Apply. Validation runs on the server, the single
// source of truth, and returns paths and line numbers.
import { Document, parseDocument } from "yaml";
import { api, HttpError, type FieldError } from "../api";
import { Store, useStore } from "../store";

export type Notice = { kind: "ok" | "error" | "info"; text: string; at: number };

export type ConfigState = {
  loaded: boolean;
  serverText: string;
  version: number;
  draft: string;
  errors: FieldError[];
  validating: boolean;
  /** Version applied elsewhere while the draft has unsaved edits. */
  remoteVersion?: number;
  applying: boolean;
  notice?: Notice;
};

export const configStore = new Store<ConfigState>({
  loaded: false,
  serverText: "",
  version: 0,
  draft: "",
  errors: [],
  validating: false,
  applying: false,
});

export const useConfig = () => useStore(configStore);
export const isUnsaved = (s: ConfigState) => s.loaded && s.draft !== s.serverText;

export function notify(kind: Notice["kind"], text: string) {
  configStore.set((s) => ({ ...s, notice: { kind, text, at: Date.now() } }));
}

export async function loadConfig() {
  const { text, version } = await api.configYAML();
  configStore.set((s) => ({ ...s, loaded: true, serverText: text, draft: text, version, errors: [], remoteVersion: undefined }));
}

let validateTimer: ReturnType<typeof setTimeout> | undefined;
let validateSeq = 0;

export function setDraft(text: string) {
  configStore.set((s) => ({ ...s, draft: text }));
  clearTimeout(validateTimer);
  validateTimer = setTimeout(() => void validateDraft(), 300);
}

async function validateDraft() {
  const seq = ++validateSeq;
  const { draft, serverText } = configStore.get();
  if (draft === serverText) {
    configStore.set((s) => ({ ...s, errors: [], validating: false }));
    return;
  }
  configStore.set((s) => ({ ...s, validating: true }));
  let errors: FieldError[] = [];
  try {
    await api.validate(draft);
  } catch (e) {
    errors = e instanceof HttpError ? e.body.details : [{ path: "", message: String(e) }];
    if (errors.length === 0 && e instanceof HttpError) errors = [{ path: "", message: e.message }];
  }
  if (seq === validateSeq) configStore.set((s) => ({ ...s, errors, validating: false }));
}

/** Parses the draft; errors in hand-edited YAML are reported, not thrown. */
export function draftDoc(text = configStore.get().draft): Document {
  return parseDocument(text, { keepSourceTokens: false });
}

let jsCache: { text: string; value: unknown } | undefined;

/** The draft as plain data, for reading in forms. */
export function draftJS<T = Record<string, unknown>>(text: string): T {
  if (jsCache?.text !== text) {
    const doc = draftDoc(text);
    jsCache = { text, value: doc.errors.length ? {} : (doc.toJS() ?? {}) };
  }
  return jsCache.value as T;
}

/** Edits the draft through its YAML Document, keeping comments. */
export function editDraft(fn: (doc: Document) => void) {
  const doc = draftDoc();
  if (doc.errors.length) {
    notify("error", "The YAML has syntax errors; fix them on the YAML screen first.");
    return;
  }
  fn(doc);
  setDraft(doc.toString({ lineWidth: 0 }));
}

/** Sets (or, for undefined/"", deletes) a value at a path in the draft. */
export function setAt(path: (string | number)[], value: unknown) {
  editDraft((doc) => {
    if (value === undefined || value === "" || (typeof value === "number" && Number.isNaN(value))) {
      doc.deleteIn(path);
    } else {
      doc.setIn(path, value);
    }
  });
}

/** Index of the item of a top-level list whose idField equals id, or -1. */
export function itemIndex(data: Record<string, unknown>, list: string, idField: string, id: string): number {
  const items = (data[list] as Record<string, unknown>[] | undefined) ?? [];
  return items.findIndex((it) => it?.[idField] === id);
}

export function discardDraft() {
  configStore.set((s) => ({ ...s, draft: s.serverText, errors: [] }));
}

export async function applyDraft() {
  const s = configStore.get();
  if (!isUnsaved(s) || s.applying) return;
  configStore.set((c) => ({ ...c, applying: true }));
  try {
    const res = await api.apply(s.draft, s.version);
    await loadConfig();
    const restart = res.diff.restart_required?.length ? ` · restart required for ${res.diff.restart_required.join(", ")}` : "";
    notify("ok", `Applied version ${res.version}${restart}`);
  } catch (e) {
    if (e instanceof HttpError && e.status === 409) {
      configStore.set((c) => ({ ...c, remoteVersion: c.version + 1 }));
      notify("error", "Someone else applied a newer version. Reload it, then redo your edits.");
    } else if (e instanceof HttpError && e.status === 422) {
      configStore.set((c) => ({ ...c, errors: e.body.details }));
      notify("error", `Not applied: ${e.body.details.length} validation error(s).`);
    } else {
      notify("error", `Not applied: ${String(e instanceof Error ? e.message : e)}`);
    }
  } finally {
    configStore.set((c) => ({ ...c, applying: false }));
  }
}

/** Polls the running version: reloads when clean, warns when there are unsaved edits. */
export function watchRemote(intervalMs = 3000) {
  const tick = async () => {
    const s = configStore.get();
    if (!s.loaded || s.applying) return;
    try {
      const v = await api.configVersion();
      if (v === s.version) return;
      if (!isUnsaved(s)) {
        await loadConfig();
        notify("info", `Config changed elsewhere; loaded version ${v}.`);
      } else if (s.remoteVersion !== v) {
        configStore.set((c) => ({ ...c, remoteVersion: v }));
      }
    } catch {
      /* server unreachable; the status bar shows it */
    }
  };
  const id = setInterval(tick, intervalMs);
  return () => clearInterval(id);
}

/** Errors for a path (e.g. symbols[0].max), including those below it. */
export function errorsAt(errors: FieldError[], path: string): FieldError[] {
  return errors.filter((e) => e.path === path || e.path.startsWith(path + ".") || e.path.startsWith(path + "["));
}
