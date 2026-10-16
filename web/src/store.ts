import { useSyncExternalStore } from "react";

/** A tiny observable value for React's useSyncExternalStore. */
export class Store<T> {
  private value: T;
  private readonly listeners = new Set<() => void>();

  constructor(value: T) {
    this.value = value;
  }

  get = () => this.value;

  set(next: T | ((prev: T) => T)) {
    this.value = typeof next === "function" ? (next as (p: T) => T)(this.value) : next;
    this.listeners.forEach((l) => l());
  }

  subscribe = (l: () => void) => {
    this.listeners.add(l);
    return () => this.listeners.delete(l);
  };
}

export function useStore<T>(s: Store<T>): T {
  return useSyncExternalStore(s.subscribe, s.get);
}
