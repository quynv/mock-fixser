// The browser side of /ws (design doc, section 8.3): one WebSocket that
// carries raw FIX. Subscriptions are reference counted per symbol, the
// connection reconnects on its own, and drop copies are kept for the
// Orders and Dashboard screens.
import { Store } from "../store";
import { FixMessage, parseFix } from "./parser";

export type ConnState = "connecting" | "open" | "closed";
type Handler = (m: FixMessage) => void;

const MAX_REPORTS = 5000;

class FixStream {
  readonly state = new Store<ConnState>("closed");
  /** Drop copies (8 and N) since the page loaded, oldest first. */
  readonly reports = new Store<FixMessage[]>([]);

  private ws?: WebSocket;
  private retry = 0;
  private seq = 0;
  private readonly symbols = new Map<string, { id: string; handlers: Set<Handler> }>();
  private readonly logHandlers = new Set<Handler>();
  private readonly rejectHandlers = new Set<(text: string) => void>();
  private started = false;

  start() {
    if (this.started) return;
    this.started = true;
    this.connect();
  }

  private connect() {
    const url = `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/ws`;
    this.state.set("connecting");
    const ws = new WebSocket(url);
    this.ws = ws;
    ws.onopen = () => {
      this.retry = 0;
      this.state.set("open");
      for (const [symbol, s] of this.symbols) this.send(`35=V|262=${s.id}|263=1|55=${symbol}`);
      if (this.logHandlers.size > 0) this.send("35=U2|5005=log|5006=on");
    };
    ws.onmessage = (ev) => this.dispatch(parseFix(String(ev.data)));
    ws.onclose = () => {
      this.state.set("closed");
      const delay = Math.min(1000 * 2 ** this.retry++, 10000);
      setTimeout(() => this.connect(), delay);
    };
  }

  private send(frame: string) {
    if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(frame);
  }

  private dispatch(m: FixMessage) {
    switch (m.msgType) {
      case "W":
      case "X":
      case "Y": {
        const id = m.get(262);
        for (const s of this.symbols.values()) {
          if (s.id === id) s.handlers.forEach((h) => h(m));
        }
        break;
      }
      case "8":
      case "N":
        this.reports.set((prev) => {
          const next = prev.length >= MAX_REPORTS ? prev.slice(prev.length - MAX_REPORTS + 1) : prev.slice();
          next.push(m);
          return next;
        });
        break;
      case "U1":
        this.logHandlers.forEach((h) => h(m));
        break;
      case "j":
        this.rejectHandlers.forEach((h) => h(m.get(58) ?? "rejected"));
        break;
    }
  }

  /** Receives W/X/Y for a symbol until the returned function is called. */
  subscribe(symbol: string, h: Handler): () => void {
    let s = this.symbols.get(symbol);
    if (!s) {
      s = { id: `ui-${++this.seq}`, handlers: new Set() };
      this.symbols.set(symbol, s);
      this.send(`35=V|262=${s.id}|263=1|55=${symbol}`);
    }
    s.handlers.add(h);
    return () => {
      const cur = this.symbols.get(symbol);
      if (!cur) return;
      cur.handlers.delete(h);
      if (cur.handlers.size === 0) {
        this.symbols.delete(symbol);
        this.send(`35=V|262=${cur.id}|263=2`);
      }
    };
  }

  /** Receives U1 log messages; the server sends them only while someone listens. */
  onLog(h: Handler): () => void {
    this.logHandlers.add(h);
    if (this.logHandlers.size === 1) this.send("35=U2|5005=log|5006=on");
    return () => {
      this.logHandlers.delete(h);
      if (this.logHandlers.size === 0) this.send("35=U2|5005=log|5006=off");
    };
  }

  onReject(h: (text: string) => void): () => void {
    this.rejectHandlers.add(h);
    return () => this.rejectHandlers.delete(h);
  }
}

export const stream = new FixStream();
