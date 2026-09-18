import * as React from 'react';
import * as yaml from 'js-yaml';
import {
  Button,
  DrawerActions,
  DrawerCloseButton,
  DrawerHead,
  DrawerPanelBody,
  DrawerPanelContent,
  Switch,
  Title,
} from '@patternfly/react-core';
import type { ChangeEvent } from '../types';
import { buildVersions, findVersionKey } from '../objectHistory';
import { useSnapshot } from '../useSnapshot';
import { useSessionSize } from '../useSessionSize';
import { changedPaths } from '../changedPaths';
import { ChangeDiff } from './ChangeDiff';
import { VersionSelect } from './VersionSelect';
import type { OwnerRef} from './RevertModal';
import { RevertModal } from './RevertModal';

const tokenFor = (field: string): string => {
  const noIndex = field.replace(/\[\d+\]/g, '');
  const segments = noIndex.split('.').filter(Boolean);
  return segments[segments.length - 1] || field;
};

const downloadManifest = (
  name: string,
  content?: Record<string, unknown>,
): void => {
  if (!content) {
    return;
  }
  const blob = new Blob([yaml.dump(content, { noRefs: true })], {
    type: 'text/yaml',
  });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = `${name}.yaml`;
  a.click();
  URL.revokeObjectURL(url);
};

const shortTime = (iso?: string): string =>
  iso
    ? new Date(iso).toLocaleString(undefined, {
        month: 'short',
        day: 'numeric',
        hour: '2-digit',
        minute: '2-digit',
      })
    : '';

export const DiffPanel: React.FC<{
  event: ChangeEvent;
  events: ChangeEvent[];
  onClose: () => void;
}> = ({ event, events, onClose }) => {
  const ns = event.metadata?.namespace;
  const target = event.spec.target;

  // Oldest first (initial state at the top of the dropdowns).
  const versions = React.useMemo(
    () => buildVersions(events, target),
    [events, target],
  );

  // Default the comparison to the clicked change's before -> after.
  const defaults = React.useMemo(() => {
    const toKey =
      event.spec.verb === 'delete'
        ? versions.find((v) => v.deleted)?.key
        : findVersionKey(versions, event.spec.afterSnapshot);
    let fromKey = findVersionKey(versions, event.spec.beforeSnapshot);
    if (!fromKey && toKey) {
      const idx = versions.findIndex((v) => v.key === toKey);
      fromKey = idx > 0 ? versions[idx - 1].key : toKey;
    }
    return { fromKey: fromKey || toKey, toKey: toKey || fromKey };
  }, [versions, event]);

  // A manual selection applies only to the defaults it was made against; when
  // the event or versions change, the new defaults take over without an
  // effect having to reset anything.
  const [picked, setPicked] = React.useState<{
    base: typeof defaults;
    fromKey: string | undefined;
    toKey: string | undefined;
  } | null>(null);
  const current = picked && picked.base === defaults ? picked : defaults;
  const fromKey = current.fromKey;
  const toKey = current.toKey;
  const setFromKey = (k: string): void => { setPicked({ base: defaults, fromKey: k, toKey }); };
  const setToKey = (k: string): void => { setPicked({ base: defaults, fromKey, toKey: k }); };

  const fromV = versions.find((v) => v.key === fromKey);
  const toV = versions.find((v) => v.key === toKey);
  const fromSnap = useSnapshot(fromV?.snapshot, ns);
  const toSnap = useSnapshot(toV?.snapshot, ns);
  const beforeContent = fromSnap?.spec.content;
  const afterContent = toSnap?.spec.content;
  const loading =
    (Boolean(fromV?.snapshot) && !fromSnap) ||
    (Boolean(toV?.snapshot) && !toSnap);

  const fields = React.useMemo(
    () => changedPaths(beforeContent, afterContent),
    [beforeContent, afterContent],
  );

  const [changedOnly, setChangedOnly] = React.useState(false);
  const [token, setToken] = React.useState<string | undefined>();
  const [seq, setSeq] = React.useState(0);
  const onField = (f: string): void => {
    setToken(tokenFor(f));
    setSeq((s) => s + 1);
  };

  const [height, onResize] = useSessionSize('chronos.diff.height', '340px');

  const [revertOpen, setRevertOpen] = React.useState(false);
  const ownerRef: OwnerRef | undefined = React.useMemo(() => {
    const meta = beforeContent?.metadata as
      | { ownerReferences?: { kind: string; name: string; controller?: boolean }[] }
      | undefined;
    const owner = meta?.ownerReferences?.find((r) => r.controller);
    return owner ? { kind: owner.kind, name: owner.name } : undefined;
  }, [beforeContent]);

  return (
    <DrawerPanelContent
      className="chronos-diff-panelcontent"
      isResizable
      defaultSize={height}
      minSize="140px"
      onResize={(_e, px) => { onResize(px); }}
    >
      <DrawerHead>
        <div className="chronos-diffpanel-head">
          <Title headingLevel="h3" size="md">
            History · {target.kind}/{target.name}
          </Title>
          <Switch
            id="chronos-changed-only"
            label="Changed only"
            isChecked={changedOnly}
            onChange={(_e, v) => { setChangedOnly(v); }}
          />
        </div>
        <DrawerActions>
          <DrawerCloseButton onClick={onClose} />
        </DrawerActions>
      </DrawerHead>

      <DrawerPanelBody className="chronos-diff-panelbody">
        <div className="chronos-version-bar">
          <VersionSelect
            label="from"
            versions={versions}
            selectedKey={fromKey}
            onChange={setFromKey}
          />
          <span className="chronos-version-arrow" aria-hidden="true">
            →
          </span>
          <VersionSelect
            label="to"
            versions={versions}
            selectedKey={toKey}
            onChange={setToKey}
          />
          <span className="chronos-version-spacer" />
          <Button
            variant="secondary"
            size="sm"
            isDisabled={!beforeContent}
            onClick={() =>
              { downloadManifest(
                `${target.kind}-${target.name}-${shortTime(fromV?.time)}`.toLowerCase(),
                beforeContent,
              ); }
            }
          >
            Download from
          </Button>
          <Button
            variant="secondary"
            size="sm"
            isDisabled={!afterContent}
            onClick={() =>
              { downloadManifest(
                `${target.kind}-${target.name}-${shortTime(toV?.time)}`.toLowerCase(),
                afterContent,
              ); }
            }
          >
            Download to
          </Button>
          <Button
            variant="secondary"
            size="sm"
            isDisabled={!fromV?.snapshot}
            onClick={() => { setRevertOpen(true); }}
          >
            Revert to {shortTime(fromV?.time)}
          </Button>
        </div>

        <div className="chronos-diffpanel-body">
          <div className="chronos-toc">
            <div className="chronos-toc-title">Changed fields</div>
            {fields.length === 0 ? (
              <div className="chronos-diff-empty">
                {beforeContent || afterContent
                  ? 'No differences'
                  : 'Select two versions'}
              </div>
            ) : (
              fields.map((f) => (
                <button
                  type="button"
                  key={f}
                  className="chronos-toc-item"
                  onClick={() => { onField(f); }}
                  title="Jump to this change in the diff"
                >
                  <code>{f}</code>
                </button>
              ))
            )}
          </div>
          <div className="chronos-diffpanel-diff">
            <ChangeDiff
              before={beforeContent}
              after={afterContent}
              loading={loading}
              changedOnly={changedOnly}
              highlightToken={token}
              highlightSeq={seq}
            />
          </div>
        </div>
      </DrawerPanelBody>

      <RevertModal
        isOpen={revertOpen}
        target={target}
        namespace={ns}
        version={fromV}
        redacted={fromSnap?.spec.redacted}
        ownerRef={ownerRef}
        onClose={() => { setRevertOpen(false); }}
      />
    </DrawerPanelContent>
  );
};
