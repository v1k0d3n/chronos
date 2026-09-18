import * as React from 'react';
import { Label } from '@patternfly/react-core';
import type {
  AttributionConfidence,
  ChangeVerb,
  RiskLevel,
} from '../types';

// PatternFly Label colors are a fixed union; we cast because our mapping is
// intentional and stable.
type LabelColor = React.ComponentProps<typeof Label>['color'];

export const RiskBadge: React.FC<{ risk?: RiskLevel }> = ({ risk }) => {
  if (!risk) {
    return null;
  }
  const color: LabelColor =
    risk === 'critical' || risk === 'high'
      ? 'red'
      : risk === 'medium'
      ? 'orange'
      : 'grey';
  return (
    <Label color={color} isCompact>
      {risk}
    </Label>
  );
};

export const VerbBadge: React.FC<{ verb: ChangeVerb }> = ({ verb }) => {
  const color: LabelColor =
    verb === 'create' ? 'green' : verb === 'delete' ? 'red' : 'blue';
  return (
    <Label color={color} isCompact>
      {verb}
    </Label>
  );
};

export const ConfidenceBadge: React.FC<{
  confidence: AttributionConfidence;
  shared?: boolean;
}> = ({ confidence, shared }) => {
  const color: LabelColor =
    confidence === 'verified'
      ? 'green'
      : confidence === 'partial'
      ? 'orange'
      : 'red';
  // A shared/privileged account (e.g. kube:admin) is an attribution blind spot
  // even when the change is otherwise verified, so always flag it.
  const text = shared ? `${confidence} · shared` : confidence;
  return (
    <Label color={color} isCompact>
      {text}
    </Label>
  );
};
