import * as React from 'react';
import type { ChangeEvent } from '../types';
import { RiskBadge, VerbBadge, ConfidenceBadge } from './badges';

const dayLabel = (iso: string): string =>
  new Date(iso).toLocaleDateString(undefined, {
    weekday: 'short',
    month: 'short',
    day: 'numeric',
  });

const timeLabel = (iso: string): string =>
  new Date(iso).toLocaleTimeString(undefined, {
    hour: '2-digit',
    minute: '2-digit',
  });

interface DayGroup {
  day: string;
  events: ChangeEvent[];
}

const groupByDay = (events: ChangeEvent[]): DayGroup[] => {
  const sorted = [...events].sort(
    (a, b) =>
      new Date(b.spec.observedAt).getTime() -
      new Date(a.spec.observedAt).getTime(),
  );
  const groups: DayGroup[] = [];
  let current: DayGroup | undefined;
  for (const e of sorted) {
    const day = dayLabel(e.spec.observedAt);
    if (current?.day !== day) {
      current = { day, events: [] };
      groups.push(current);
    }
    current.events.push(e);
  }
  return groups;
};

export const TimelineView: React.FC<{
  events: ChangeEvent[];
  selectedUid?: string;
  onSelect: (e: ChangeEvent) => void;
}> = ({ events, selectedUid, onSelect }) => {
  const groups = React.useMemo(() => groupByDay(events), [events]);

  return (
    <div className="chronos-timeline">
      {groups.map((group) => (
        <div key={group.day}>
          <div className="chronos-day-label">{group.day}</div>
          <div className="chronos-feed">
            {group.events.map((e) => {
              const t = e.spec.target;
              const selected = e.metadata?.uid === selectedUid;
              return (
                <button
                  type="button"
                  key={e.metadata?.uid}
                  className={`chronos-row${selected ? ' chronos-row--selected' : ''}`}
                  onClick={() => { onSelect(e); }}
                >
                  <div className="chronos-row-main">
                    <span className="chronos-time">
                      {timeLabel(e.spec.observedAt)}
                    </span>
                    <VerbBadge verb={e.spec.verb} />
                    <span className="chronos-target">
                      {t.kind}/{t.name}
                    </span>
                    {t.namespace && (
                      <span className="chronos-ns">{t.namespace}</span>
                    )}
                    <span className="chronos-spacer" />
                    <RiskBadge risk={e.spec.riskLevel} />
                  </div>
                  <div className="chronos-row-sub">
                    <ConfidenceBadge
                      confidence={e.spec.actor.confidence}
                      shared={e.spec.actor.shared}
                    />
                    <span className="chronos-actor">
                      {e.spec.actor.username || 'unknown'}
                    </span>
                    {e.spec.summary && (
                      <span className="chronos-summary">{e.spec.summary}</span>
                    )}
                    {e.spec.redacted && (
                      <span className="chronos-redacted">redacted</span>
                    )}
                  </div>
                </button>
              );
            })}
          </div>
        </div>
      ))}
    </div>
  );
};
