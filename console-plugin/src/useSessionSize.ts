import * as React from 'react';

const read = (key: string, initial: string): string => {
  try {
    return sessionStorage.getItem(key) || initial;
  } catch {
    return initial;
  }
};

// useSessionSize keeps a resizable panel's size sticky for the browser-tab
// session. Critically, onResize ONLY persists to sessionStorage — it never
// calls setState, so resizing does not re-render the panel. (An earlier version
// re-rendered on resize, which fed PatternFly's resize callback back into
// itself and thrashed the drawer.) The stored size is read once on mount, so a
// remounted panel (e.g. when the selection changes) starts where the user left
// it, giving a consistent, non-flickering divider.
export const useSessionSize = (
  key: string,
  initial: string,
): [string, (px: number) => void] => {
  const initialSize = React.useMemo(() => read(key, initial), [key, initial]);

  const onResize = React.useCallback(
    (px: number) => {
      try {
        sessionStorage.setItem(key, `${Math.round(px)}px`);
      } catch {
        // sessionStorage may be unavailable; the current drag still applies.
      }
    },
    [key],
  );

  return [initialSize, onResize];
};
