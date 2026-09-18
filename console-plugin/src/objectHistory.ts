import type { ChangeEvent, TargetObjectReference } from './types';

// A single point in an object's recorded history — a snapshot the user can view,
// compare against, download, or revert to.
export interface Version {
  key: string;
  // ResourceSnapshot name to fetch content from; undefined for a "deleted" marker.
  snapshot?: string;
  time: string;
  label: string;
  verb: string;
  deleted?: boolean;
}

const sameTarget = (a: TargetObjectReference, b: TargetObjectReference): boolean =>
  a.kind === b.kind &&
  a.name === b.name &&
  (a.namespace || '') === (b.namespace || '');

// buildVersions derives an object's version timeline (oldest → newest) from all
// ChangeEvents that target it. Each change contributes its "after" state; the
// earliest change also contributes its "before" state as the initial version.
export const buildVersions = (
  events: ChangeEvent[],
  target: TargetObjectReference,
): Version[] => {
  const matches = events
    .filter((e) => sameTarget(e.spec.target, target))
    .sort(
      (a, b) =>
        new Date(a.spec.observedAt).getTime() -
        new Date(b.spec.observedAt).getTime(),
    );

  const versions: Version[] = [];
  if (matches.length > 0 && matches[0].spec.beforeSnapshot) {
    versions.push({
      key: `initial-${matches[0].spec.beforeSnapshot}`,
      snapshot: matches[0].spec.beforeSnapshot,
      time: matches[0].spec.observedAt,
      label: 'initial state',
      verb: '—',
    });
  }
  matches.forEach((e) => {
    if (e.spec.verb === 'delete') {
      versions.push({
        key: `deleted-${e.metadata?.uid || e.spec.observedAt}`,
        time: e.spec.observedAt,
        label: 'deleted',
        verb: 'delete',
        deleted: true,
      });
    } else if (e.spec.afterSnapshot) {
      versions.push({
        key: e.spec.afterSnapshot,
        snapshot: e.spec.afterSnapshot,
        time: e.spec.observedAt,
        label: e.spec.summary || e.spec.verb,
        verb: e.spec.verb,
      });
    }
  });
  return versions;
};

// findVersionKey returns the version key for a given snapshot name.
export const findVersionKey = (
  versions: Version[],
  snapshot?: string,
): string | undefined =>
  snapshot ? versions.find((v) => v.snapshot === snapshot)?.key : undefined;
