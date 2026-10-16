// REST client for /api (design doc, section 10).

export type FieldError = { path: string; line?: number; message: string };
export type ApiError = { code: string; message: string; details: FieldError[] };
export type Diff = { sections: string[]; restart_required?: string[] };
export type ApplyResult = { version: number; diff: Diff };

export class HttpError extends Error {
  readonly status: number;
  readonly body: ApiError;
  constructor(status: number, body: ApiError) {
    super(body.message || `HTTP ${status}`);
    this.status = status;
    this.body = body;
  }
}

async function request<T>(method: string, path: string, body?: string, headers: Record<string, string> = {}): Promise<T> {
  const res = await fetch(path, { method, body, headers });
  const text = await res.text();
  const data = text && (res.headers.get("Content-Type") ?? "").includes("json") ? JSON.parse(text) : text;
  if (!res.ok) {
    const err: ApiError =
      typeof data === "object" ? data : { code: String(res.status), message: String(data), details: [] };
    throw new HttpError(res.status, { ...err, details: err.details ?? [] });
  }
  return data as T;
}

const json = { "Content-Type": "application/json" };
const yaml = { "Content-Type": "application/yaml" };

export const api = {
  configYAML: async () => {
    const res = await fetch("/api/config.yaml");
    return { text: await res.text(), version: Number((res.headers.get("ETag") ?? "0").replaceAll('"', "")) };
  },
  configVersion: async () => {
    const res = await fetch("/api/config", { method: "GET" });
    await res.body?.cancel();
    return Number((res.headers.get("ETag") ?? "0").replaceAll('"', ""));
  },
  validate: (text: string) => request<{ valid: boolean; diff: Diff }>("POST", "/api/config/validate", text, yaml),
  apply: (text: string, version: number) =>
    request<ApplyResult>("PUT", "/api/config", text, { ...yaml, "If-Match": `"${version}"` }),
  history: () => request<{ version: number; saved_at: string }[]>("GET", "/api/config/history"),
  rollback: (version: number) => request<ApplyResult>("POST", `/api/config/rollback/${version}`),
  symbols: () => request<SymbolView[]>("GET", "/api/symbols"),
  pause: (s: string) => request<SymbolState>("POST", `/api/symbols/${encodeURIComponent(s)}/pause`),
  resume: (s: string) => request<SymbolState>("POST", `/api/symbols/${encodeURIComponent(s)}/resume`),
  setPrice: (s: string, mid: number) =>
    request<SymbolState>("POST", `/api/symbols/${encodeURIComponent(s)}/price`, JSON.stringify({ mid }), json),
  spike: (s: string, percent: number, direction: "up" | "down") =>
    request<SymbolState>("POST", `/api/symbols/${encodeURIComponent(s)}/spike`, JSON.stringify({ percent, direction }), json),
  quoteCancel: (symbols: string[] = []) =>
    request<{ sessions: string[]; subscriptions_canceled: number }>("POST", "/api/quote-cancel", JSON.stringify({ symbols }), json),
  testRule: (order: Record<string, unknown>) =>
    request<RuleTestResult>("POST", "/api/rules/test", JSON.stringify(order), json),
  sessions: () => request<SessionInfo[]>("GET", "/api/sessions"),
  logout: (id: string) => request<unknown>("POST", `/api/sessions/${encodeURIComponent(id)}/logout`),
  fault: (id: string, type: "drop" | "seq_gap" | "heartbeat_miss") =>
    request<unknown>("POST", `/api/sessions/${encodeURIComponent(id)}/fault`, JSON.stringify({ type }), json),
  orders: (q: Record<string, string>) =>
    request<OrderView[]>("GET", `/api/orders?${new URLSearchParams(Object.entries(q).filter(([, v]) => v))}`),
  metrics: async () => (await fetch("/metrics")).text(),
};

export type SymbolState = { symbol: string; paused: boolean; bid: number; ask: number; mid: number; seq: number };
export type SymbolView = { symbol: string; runtime?: SymbolState } & Record<string, unknown>;
export type RuleTestStep = {
  type: string;
  delay_ms: number;
  at_ms: number;
  ratio?: string;
  qty?: string;
  count?: number;
  price_mode?: string;
  price?: string;
  reason?: number;
  text?: string;
};
export type RuleTestResult = { rule: string; actions: RuleTestStep[]; total_ms: number };
export type SessionInfo = {
  id: string;
  kind: "client" | "monitor";
  version: string;
  sender_comp_id: string;
  target_comp_id: string;
  logged_on: boolean;
  next_sender_seq?: number;
  next_target_seq?: number;
  remote?: string;
  faults?: Record<string, unknown>;
};
export type OrderView = {
  order_id: string;
  session: string;
  cl_ord_id: string;
  orig_cl_ord_id?: string;
  list_id?: string;
  symbol: string;
  side: string;
  order_qty: string;
  cum_qty: string;
  leaves_qty: string;
  avg_px: string;
  status: string;
  rule?: string;
  fills: { ExecID: string; Qty: string; Px: string; Time: string }[];
  created_at: string;
};

/** Sums every sample of a metric in Prometheus text format. */
export function metricSum(text: string, name: string): number {
  let sum = 0;
  for (const line of text.split("\n")) {
    if (line.startsWith(name + "{") || line.startsWith(name + " ")) {
      sum += Number(line.slice(line.lastIndexOf(" ") + 1)) || 0;
    }
  }
  return sum;
}
