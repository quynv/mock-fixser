import { lazy, Suspense, useEffect, useState } from "react";
import { stream } from "./fix/stream";
import { href, useRoute } from "./router";
import { Dashboard } from "./screens/Dashboard";
import { FixLog } from "./screens/FixLog";
import { Orders } from "./screens/Orders";
import { Rules } from "./screens/Rules";
import { Sessions } from "./screens/Sessions";
import { Symbols } from "./screens/Symbols";
import {
  applyDraft,
  discardDraft,
  isUnsaved,
  loadConfig,
  notify,
  useConfig,
  watchRemote,
} from "./state/config";
import { useStore } from "./store";

// Monaco is large; load it only when the YAML screen opens.
const YamlScreen = lazy(() => import("./screens/YamlScreen"));

const NAV = [
  ["dashboard", "Dashboard"],
  ["symbols", "Symbols"],
  ["rules", "Order rules"],
  ["sessions", "Sessions"],
  ["orders", "Orders"],
  ["log", "FIX log"],
  ["yaml", "YAML"],
] as const;

export function App() {
  const route = useRoute();
  const cfg = useConfig();
  const unsaved = isUnsaved(cfg);
  const ws = useStore(stream.state);

  useEffect(() => {
    stream.start();
    loadConfig().catch((e) => notify("error", `Cannot load the config: ${e}`));
    return watchRemote();
  }, []);

  // Leaving with unsaved edits asks for confirmation (section 11.5).
  useEffect(() => {
    if (!unsaved) return;
    const warn = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [unsaved]);

  const page = route[0] ?? "dashboard";
  const errors = cfg.errors.length;

  return (
    <div className="app">
      <header className="topbar">
        <span className="brand">mockfix</span>
        <nav className="nav">
          {NAV.map(([id, label]) => (
            <a key={id} href={href(id)} className={page === id ? "active" : ""}>
              {label}
            </a>
          ))}
        </nav>
        <div className="cfg-status" data-testid="config-status">
          <span className="badge">v{cfg.version}</span>
          {!cfg.loaded ? (
            <span className="muted">loading…</span>
          ) : unsaved ? (
            <span className="badge unsaved">unsaved</span>
          ) : (
            <span className="badge ok">applied</span>
          )}
          {errors > 0 && <span className="badge err">{errors} error{errors > 1 ? "s" : ""}</span>}
          {cfg.validating && <span className="muted">validating…</span>}
          {unsaved && <button onClick={discardDraft}>Discard</button>}
          {/* Apply stays clickable with errors and returns them (section 11.5). */}
          <button className="primary" disabled={!unsaved || cfg.applying} onClick={() => void applyDraft()}>
            {cfg.applying ? "Applying…" : "Apply"}
          </button>
        </div>
      </header>

      <main>
        {cfg.remoteVersion !== undefined && (
          <div className="banner">
            Version {cfg.remoteVersion} was applied elsewhere while you have unsaved edits.
            <button onClick={() => void loadConfig()}>Reload (discard my edits)</button>
          </div>
        )}
        {page === "dashboard" && <Dashboard />}
        {page === "symbols" && <Symbols selected={route[1]} />}
        {page === "rules" && <Rules selected={route[1]} />}
        {page === "sessions" && <Sessions />}
        {page === "orders" && <Orders selected={route[1]} />}
        {page === "log" && <FixLog />}
        {page === "yaml" && (
          <Suspense fallback={<p className="muted">Loading editor…</p>}>
            <YamlScreen />
          </Suspense>
        )}
      </main>

      <footer className="statusbar">
        <span>
          <span className={`dot ${ws}`} />
          gateway {ws === "open" ? "connected" : ws}
        </span>
        <span>config v{cfg.version}</span>
        <span className="muted">Charts by TradingView Lightweight Charts</span>
      </footer>
      <Toast />
    </div>
  );
}

function Toast() {
  const { notice } = useConfig();
  const [shown, setShown] = useState(notice);
  useEffect(() => {
    setShown(notice);
    if (!notice) return;
    const id = setTimeout(() => setShown(undefined), notice.kind === "error" ? 8000 : 4000);
    return () => clearTimeout(id);
  }, [notice]);
  if (!shown) return null;
  return (
    <div className={`toast ${shown.kind}`} role="status" onClick={() => setShown(undefined)}>
      {shown.text}
    </div>
  );
}
