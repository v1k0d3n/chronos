import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import type { ResourceSnapshot } from './types';
import { RESOURCE_SNAPSHOT_GVK } from './types';

// useSnapshot fetches a single ResourceSnapshot by name within a namespace.
// Passing an empty name skips the watch (React hooks must run unconditionally),
// so the drawer can call it for both the before and after snapshots even when
// one is absent (create has no before, delete has no after).
export const useSnapshot = (
  name: string | undefined,
  namespace: string | undefined,
): ResourceSnapshot | undefined => {
  const [data] = useK8sWatchResource<ResourceSnapshot>(
    name && namespace
      ? { groupVersionKind: RESOURCE_SNAPSHOT_GVK, name, namespace }
      : null,
  );
  return data?.metadata ? data : undefined;
};
