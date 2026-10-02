import { useEffect, useRef, useState } from '../vendor/hooks.module.js';

// createStore holds app-wide state (player, toasts) outside the component tree.
export function createStore(initial) {
  let state = initial;
  const subs = new Set();
  return {
    get: () => state,
    set(patch) {
      state = { ...state, ...(typeof patch === 'function' ? patch(state) : patch) };
      subs.forEach((f) => f(state));
    },
    subscribe(f) {
      subs.add(f);
      return () => subs.delete(f);
    },
  };
}

// useStore re-renders the component whenever the selected part of the store changes
// (by default the whole state, so on every change).
export function useStore(store, select = (s) => s) {
  const [, force] = useState(0);
  const value = select(store.get());
  const latest = useRef();
  latest.current = { select, value };
  useEffect(() => store.subscribe((s) => {
    if (!Object.is(latest.current.select(s), latest.current.value)) force((n) => n + 1);
  }), [store]);
  return value;
}
