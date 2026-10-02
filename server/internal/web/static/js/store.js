import { useEffect, useState } from '../vendor/hooks.module.js';

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

// useStore re-renders the component whenever the store changes.
export function useStore(store, select = (s) => s) {
  const [, force] = useState(0);
  useEffect(() => store.subscribe(() => force((n) => n + 1)), [store]);
  return select(store.get());
}
