import * as React from 'react';
import { Button } from '@patternfly/react-core';
import type { ChangeEvent, RiskLevel } from '../types';
import { RiskBadge, VerbBadge } from './badges';

const RISK_ORDER: Record<RiskLevel, number> = {
  low: 1,
  medium: 2,
  high: 3,
  critical: 4,
};

const higher = (a: RiskLevel | undefined, b: RiskLevel | undefined): RiskLevel | undefined => {
  if (!a) {
    return b;
  }
  if (!b) {
    return a;
  }
  return RISK_ORDER[a] >= RISK_ORDER[b] ? a : b;
};

interface DayCell {
  count: number;
  maxRisk?: RiskLevel;
}

const WEEKDAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];

export const CalendarView: React.FC<{
  events: ChangeEvent[];
  onSelect: (e: ChangeEvent) => void;
}> = ({ events, onSelect }) => {
  // Anchor the grid on the month of the most recent change, else today.
  const anchor = React.useMemo(() => {
    if (events.length === 0) {
      return new Date();
    }
    const latest = events.reduce((max, e) =>
      new Date(e.spec.observedAt) > new Date(max.spec.observedAt) ? e : max,
    );
    return new Date(latest.spec.observedAt);
  }, [events]);

  const [viewDate, setViewDate] = React.useState<Date>(
    () => new Date(anchor.getFullYear(), anchor.getMonth(), 1),
  );
  const [selectedDay, setSelectedDay] = React.useState<number | undefined>();
  const year = viewDate.getFullYear();
  const month = viewDate.getMonth();

  const goToMonth = (delta: number): void => {
    setSelectedDay(undefined);
    setViewDate(new Date(year, month + delta, 1));
  };
  const jumpToLatest = (): void => {
    setSelectedDay(undefined);
    setViewDate(new Date(anchor.getFullYear(), anchor.getMonth(), 1));
  };

  const cells = React.useMemo(() => {
    const map: Record<number, DayCell> = {};
    for (const e of events) {
      const d = new Date(e.spec.observedAt);
      if (d.getFullYear() !== year || d.getMonth() !== month) {
        continue;
      }
      const day = d.getDate();
      const existing = map[day] || { count: 0 };
      map[day] = {
        count: existing.count + 1,
        maxRisk: higher(existing.maxRisk, e.spec.riskLevel),
      };
    }
    return map;
  }, [events, year, month]);

  const firstWeekday = new Date(year, month, 1).getDay();
  const daysInMonth = new Date(year, month + 1, 0).getDate();
  const monthLabel = viewDate.toLocaleDateString(undefined, {
    month: 'long',
    year: 'numeric',
  });
  const anchorMonthLabel = anchor.toLocaleDateString(undefined, {
    month: 'short',
    year: 'numeric',
  });
  const onLatestMonth =
    year === anchor.getFullYear() && month === anchor.getMonth();

  const dayEvents = React.useMemo(() => {
    if (!selectedDay) {
      return [];
    }
    return events
      .filter((e) => {
        const d = new Date(e.spec.observedAt);
        return (
          d.getFullYear() === year &&
          d.getMonth() === month &&
          d.getDate() === selectedDay
        );
      })
      .sort(
        (a, b) =>
          new Date(b.spec.observedAt).getTime() -
          new Date(a.spec.observedAt).getTime(),
      );
  }, [events, selectedDay, year, month]);

  return (
    <div>
      <div className="chronos-cal-header">
        <Button
          variant="plain"
          aria-label="Previous month"
          onClick={() => { goToMonth(-1); }}
        >
          ‹
        </Button>
        <span className="chronos-cal-month">{monthLabel}</span>
        <Button
          variant="plain"
          aria-label="Next month"
          onClick={() => { goToMonth(1); }}
        >
          ›
        </Button>
        {!onLatestMonth && (
          <Button variant="link" isInline onClick={jumpToLatest}>
            Jump to latest ({anchorMonthLabel})
          </Button>
        )}
      </div>
      <div className="chronos-cal-grid">
        {WEEKDAYS.map((w) => (
          <div key={w} className="chronos-cal-weekday">
            {w}
          </div>
        ))}
        {Array.from({ length: firstWeekday }).map((_, i) => (
          <div key={`blank-${i}`} />
        ))}
        {Array.from({ length: daysInMonth }).map((_, i) => {
          const day = i + 1;
          const cell = cells[day];
          const risk = cell?.maxRisk;
          const cls = risk ? `chronos-cal-cell chronos-risk-${risk}` : 'chronos-cal-cell';
          return (
            <button
              type="button"
              key={day}
              className={`${cls}${selectedDay === day ? ' chronos-cal-cell--selected' : ''}`}
              onClick={() => { setSelectedDay(day); }}
            >
              <span className="chronos-cal-daynum">{day}</span>
              {cell && (
                <span className="chronos-cal-count">
                  {cell.count} {cell.count === 1 ? 'change' : 'changes'}
                </span>
              )}
            </button>
          );
        })}
      </div>

      <div className="chronos-cal-legend">
        <span>Highest risk that day:</span>
        <span className="chronos-swatch chronos-risk-low" /> low
        <span className="chronos-swatch chronos-risk-medium" /> medium
        <span className="chronos-swatch chronos-risk-high" /> high
        <span className="chronos-swatch chronos-risk-critical" /> critical
      </div>

      {selectedDay && (
        <div className="chronos-cal-daypanel">
          <div className="chronos-day-label">
            {monthLabel.split(' ')[0]} {selectedDay} · {dayEvents.length}{' '}
            {dayEvents.length === 1 ? 'change' : 'changes'}
          </div>
          {dayEvents.map((e) => (
            <button
              type="button"
              key={e.metadata?.uid}
              className="chronos-row"
              onClick={() => { onSelect(e); }}
            >
              <div className="chronos-row-main">
                <VerbBadge verb={e.spec.verb} />
                <span className="chronos-target">
                  {e.spec.target.kind}/{e.spec.target.name}
                </span>
                <span className="chronos-spacer" />
                <RiskBadge risk={e.spec.riskLevel} />
              </div>
            </button>
          ))}
        </div>
      )}
    </div>
  );
};
