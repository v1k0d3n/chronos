import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import type { ChangeEvent } from './types';
import { CHANGE_EVENT_GVK } from './types';

// useChangeEvents is the single data-access seam for the timeline. Today it
// reads ChangeEvents directly via the console's k8s API (works for cluster
// admins). The Chronos security model requires that multi-tenant access go
// through a SAR-filtering backend instead — when that lands, only this hook
// changes; the view components stay the same.
export const useChangeEvents = (): {
  events: ChangeEvent[];
  loaded: boolean;
  error: unknown;
} => {
  const watched: [ChangeEvent[] | undefined, boolean, unknown] = useK8sWatchResource<ChangeEvent[]>({
    groupVersionKind: CHANGE_EVENT_GVK,
    isList: true,
  });
  const [events, loaded, error] = watched;
  return { events: events ?? [], loaded, error };
};
