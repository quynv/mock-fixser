import { DiffEditor, Editor, loader, type OnMount } from "@monaco-editor/react";
import EditorWorker from "monaco-esm/editor/editor.worker.js?worker";
import type * as Monaco from "monaco-editor";
import { monaco } from "../monaco";
import { useEffect, useRef, useState } from "react";
import { api, HttpError } from "../api";
import { Panel } from "../components/ui";
import { loadConfig, notify, setDraft, useConfig } from "../state/config";

// Bundle Monaco instead of loading it from a CDN: mockfix runs offline.
self.MonacoEnvironment = { getWorker: () => new EditorWorker() };
loader.config({ monaco });

const dark = () => window.matchMedia("(prefers-color-scheme: dark)").matches;

/** YAML editor with server validation markers, diff and history (section 11.2). */
export default function YamlScreen() {
  const cfg = useConfig();
  const [diff, setDiff] = useState(false);
  const editorRef = useRef<Monaco.editor.IStandaloneCodeEditor>(null);

  const onMount: OnMount = (editor) => {
    editorRef.current = editor;
  };

  // Validation errors become editor markers on their YAML lines.
  useEffect(() => {
    const model = editorRef.current?.getModel();
    if (!model) return;
    monaco.editor.setModelMarkers(
      model,
      "mockfix",
      cfg.errors.map((e) => {
        const line = Math.max(e.line ?? 1, 1);
        return {
          severity: monaco.MarkerSeverity.Error,
          message: e.path ? `${e.path}: ${e.message}` : e.message,
          startLineNumber: line,
          endLineNumber: line,
          startColumn: 1,
          endColumn: model.getLineMaxColumn(Math.min(line, model.getLineCount())),
        };
      }),
    );
  }, [cfg.errors, diff]);

  return (
    <div className="grid" style={{ gridTemplateColumns: "minmax(0, 1fr) 260px", alignItems: "start" }}>
      <Panel
        title="config YAML"
        actions={
          <button onClick={() => setDiff(!diff)}>{diff ? "Edit" : "Diff with running config"}</button>
        }
      >
        <div style={{ height: "calc(100vh - 200px)", minHeight: 400 }}>
          {diff ? (
            <DiffEditor
              original={cfg.serverText}
              modified={cfg.draft}
              language="yaml"
              theme={dark() ? "vs-dark" : "vs"}
              options={{ readOnly: true, renderSideBySide: true, minimap: { enabled: false } }}
            />
          ) : (
            <Editor
              value={cfg.draft}
              language="yaml"
              theme={dark() ? "vs-dark" : "vs"}
              onMount={onMount}
              onChange={(v) => setDraft(v ?? "")}
              options={{ minimap: { enabled: false }, tabSize: 2, scrollBeyondLastLine: false, fontSize: 13 }}
            />
          )}
        </div>
        {cfg.errors.length > 0 && (
          <ul>
            {cfg.errors.map((e, i) => (
              <li key={i} className="err-text">
                <button className="link" onClick={() => editorRef.current?.revealLineInCenter(e.line ?? 1)}>
                  line {e.line ?? "?"}
                </button>{" "}
                {e.path}: {e.message}
              </li>
            ))}
          </ul>
        )}
      </Panel>
      <History version={cfg.version} />
    </div>
  );
}

function History({ version }: { version: number }) {
  const [items, setItems] = useState<{ version: number; saved_at: string }[]>([]);
  useEffect(() => {
    api.history().then(setItems, () => setItems([]));
  }, [version]);
  const rollback = async (v: number) => {
    if (!confirm(`Roll back to version ${v}? It is applied as a new version.`)) return;
    try {
      const res = await api.rollback(v);
      await loadConfig();
      notify("ok", `Rolled back: version ${v} is now version ${res.version}`);
    } catch (e) {
      notify("error", e instanceof HttpError ? e.body.message : String(e));
    }
  };
  return (
    <Panel title="History">
      <p className="muted">Running: version {version}</p>
      {items.length === 0 && <p className="muted">No earlier versions.</p>}
      <ul className="list">
        {items.map((h) => (
          <li key={h.version}>
            <span>
              v{h.version} <span className="muted">{new Date(h.saved_at).toLocaleString()}</span>
            </span>
            <button onClick={() => void rollback(h.version)}>Roll back</button>
          </li>
        ))}
      </ul>
    </Panel>
  );
}
