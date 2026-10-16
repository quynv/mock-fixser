import { useEffect, useState } from "react";

/** Hash routes: #/symbols/EURUSD -> ["symbols", "EURUSD"]. */
export function useRoute(): string[] {
  const read = () => location.hash.replace(/^#\/?/, "").split("/").filter(Boolean).map(decodeURIComponent);
  const [route, setRoute] = useState(read);
  useEffect(() => {
    const on = () => setRoute(read());
    window.addEventListener("hashchange", on);
    return () => window.removeEventListener("hashchange", on);
  }, []);
  return route;
}

export function go(...parts: string[]) {
  location.hash = "/" + parts.map(encodeURIComponent).join("/");
}

export const href = (...parts: string[]) => "#/" + parts.map(encodeURIComponent).join("/");
