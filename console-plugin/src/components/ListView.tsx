import * as React from 'react';
import { Table, Thead, Tr, Th, Tbody, Td } from '@patternfly/react-table';
import type { ChangeEvent } from '../types';
import { RiskBadge, VerbBadge, ConfidenceBadge } from './badges';

const when = (iso: string): string => new Date(iso).toLocaleString();

export const ListView: React.FC<{
  events: ChangeEvent[];
  selectedUid?: string;
  onSelect: (e: ChangeEvent) => void;
}> = ({ events, selectedUid, onSelect }) => {
  const sorted = React.useMemo(
    () =>
      [...events].sort(
        (a, b) =>
          new Date(b.spec.observedAt).getTime() -
          new Date(a.spec.observedAt).getTime(),
      ),
    [events],
  );

  return (
    <Table aria-label="Change events" variant="compact">
      <Thead>
        <Tr>
          <Th>When</Th>
          <Th>Verb</Th>
          <Th>Kind</Th>
          <Th>Target</Th>
          <Th>Namespace</Th>
          <Th>Actor</Th>
          <Th>Confidence</Th>
          <Th>Risk</Th>
        </Tr>
      </Thead>
      <Tbody>
        {sorted.map((e) => (
          <Tr
            key={e.metadata?.uid}
            isClickable
            isRowSelected={e.metadata?.uid === selectedUid}
            onRowClick={() => { onSelect(e); }}
          >
            <Td dataLabel="When">{when(e.spec.observedAt)}</Td>
            <Td dataLabel="Verb">
              <VerbBadge verb={e.spec.verb} />
            </Td>
            <Td dataLabel="Kind">{e.spec.target.kind}</Td>
            <Td dataLabel="Target">{e.spec.target.name}</Td>
            <Td dataLabel="Namespace">{e.spec.target.namespace || '—'}</Td>
            <Td dataLabel="Actor">{e.spec.actor.username || 'unknown'}</Td>
            <Td dataLabel="Confidence">
              <ConfidenceBadge
                confidence={e.spec.actor.confidence}
                shared={e.spec.actor.shared}
              />
            </Td>
            <Td dataLabel="Risk">
              <RiskBadge risk={e.spec.riskLevel} />
            </Td>
          </Tr>
        ))}
      </Tbody>
    </Table>
  );
};
