import * as React from 'react';
import {
  Alert,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  DrawerActions,
  DrawerCloseButton,
  DrawerHead,
  DrawerPanelBody,
  DrawerPanelContent,
  Title,
} from '@patternfly/react-core';
import type { ChangeEvent } from '../types';
import { useSessionSize } from '../useSessionSize';
import { ConfidenceBadge, RiskBadge, VerbBadge } from './badges';

export const ChangeDetailDrawer: React.FC<{
  event: ChangeEvent;
  onClose: () => void;
}> = ({ event, onClose }) => {
  const { spec } = event;
  const t = spec.target;
  const [width, onResize] = useSessionSize('chronos.summary.width', '460px');

  return (
    <DrawerPanelContent
      isResizable
      defaultSize={width}
      minSize="320px"
      onResize={(_e, px) => { onResize(px); }}
    >
      <DrawerHead>
        <Title headingLevel="h2" size="lg">
          {t.kind}/{t.name}
        </Title>
        <div className="chronos-drawer-badges">
          <VerbBadge verb={spec.verb} />
          <RiskBadge risk={spec.riskLevel} />
          <ConfidenceBadge
            confidence={spec.actor.confidence}
            shared={spec.actor.shared}
          />
        </div>
        <DrawerActions>
          <DrawerCloseButton onClick={onClose} />
        </DrawerActions>
      </DrawerHead>

      <DrawerPanelBody>
        <DescriptionList isHorizontal isCompact>
          <DescriptionListGroup>
            <DescriptionListTerm>When</DescriptionListTerm>
            <DescriptionListDescription>
              {new Date(spec.observedAt).toLocaleString()}
            </DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Actor</DescriptionListTerm>
            <DescriptionListDescription>
              {spec.actor.username || 'unknown'}
              {spec.actor.sourceIP ? ` · ${spec.actor.sourceIP}` : ''}
              {spec.actor.userAgent ? ` · ${spec.actor.userAgent}` : ''}
            </DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Namespace</DescriptionListTerm>
            <DescriptionListDescription>
              {t.namespace || 'cluster-scoped'}
            </DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Source</DescriptionListTerm>
            <DescriptionListDescription>{spec.source}</DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Changed fields</DescriptionListTerm>
            <DescriptionListDescription>
              {spec.changedFields?.length ?? 0}
            </DescriptionListDescription>
          </DescriptionListGroup>
        </DescriptionList>

        {spec.redacted && (
          <Alert
            variant="info"
            isInline
            title="Sensitive values were redacted at capture"
            className="chronos-drawer-section"
          >
            Snapshot values for this object are stored as hashes, never in
            plaintext. A revert may require re-supplying secret values.
          </Alert>
        )}

        <div className="chronos-drawer-hint chronos-drawer-section">
          Compare versions, download manifests, and revert from the diff panel
          below.
        </div>
      </DrawerPanelBody>
    </DrawerPanelContent>
  );
};
