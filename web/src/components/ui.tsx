import { useEffect, useState, type ReactNode } from "react";
import type { FieldError } from "../api";
import { errorsAt, useConfig } from "../state/config";

/** Errors for path, shown next to a field (section 11.5). */
export function useFieldErrors(path: string): FieldError[] {
  return errorsAt(useConfig().errors, path);
}

type FieldProps = { label: string; path: string; children: ReactNode; hint?: string };

export function Field({ label, path, children, hint }: FieldProps) {
  const errs = useFieldErrors(path);
  // The label wraps only its text and the control, so the control's
  // accessible name stays the label; errors and hints sit next to it.
  return (
    <div className={`field${errs.length ? " has-err" : ""}`}>
      <label>
        <span className="label">{label}</span>
        {children}
      </label>
      {errs.map((e, i) => (
        <span className="err" role="alert" key={i}>
          {e.message}
        </span>
      ))}
      {!errs.length && hint && <span className="muted">{hint}</span>}
    </div>
  );
}

/**
 * A text or number input that commits on blur or Enter, so typing does not
 * rewrite the YAML on every key press.
 */
export function Input({
  value,
  onCommit,
  type = "text",
  placeholder,
  disabled,
}: {
  value: unknown;
  onCommit: (v: string) => void;
  type?: "text" | "number";
  placeholder?: string;
  disabled?: boolean;
}) {
  const shown = value === undefined || value === null ? "" : String(value);
  const [text, setText] = useState(shown);
  useEffect(() => setText(shown), [shown]);
  const commit = () => {
    if (text !== shown) onCommit(text);
  };
  return (
    <input
      type={type}
      step="any"
      value={text}
      placeholder={placeholder}
      disabled={disabled}
      onChange={(e) => setText(e.target.value)}
      onBlur={commit}
      onKeyDown={(e) => e.key === "Enter" && commit()}
    />
  );
}

export function Select({
  value,
  options,
  onChange,
  allowEmpty,
}: {
  value: unknown;
  options: string[];
  onChange: (v: string) => void;
  allowEmpty?: string;
}) {
  return (
    <select value={value === undefined ? "" : String(value)} onChange={(e) => onChange(e.target.value)}>
      {allowEmpty !== undefined && <option value="">{allowEmpty}</option>}
      {options.map((o) => (
        <option key={o} value={o}>
          {o}
        </option>
      ))}
    </select>
  );
}

/** Parses a number field: "" clears it. */
export const toNum = (s: string) => (s.trim() === "" ? undefined : Number(s));

export function Panel({ title, actions, children }: { title: ReactNode; actions?: ReactNode; children: ReactNode }) {
  return (
    <section className="panel">
      <header>
        <span>{title}</span>
        {actions && <span className="row">{actions}</span>}
      </header>
      <div className="body">{children}</div>
    </section>
  );
}

export function usePoll<T>(fn: () => Promise<T>, ms: number, deps: unknown[] = []): { data?: T; error?: string } {
  const [state, setState] = useState<{ data?: T; error?: string }>({});
  useEffect(() => {
    let alive = true;
    const run = () =>
      fn().then(
        (data) => alive && setState({ data }),
        (e) => alive && setState((s) => ({ ...s, error: String(e) })),
      );
    run();
    const id = setInterval(run, ms);
    return () => {
      alive = false;
      clearInterval(id);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
  return state;
}
