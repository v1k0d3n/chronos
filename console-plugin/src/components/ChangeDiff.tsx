import * as React from 'react';
import * as yaml from 'js-yaml';
import { diffLines } from 'diff';

// Server-managed noise stripped before diffing so the diff shows only
// user-meaningful changes (mirrors the operator's diff normalization).
const IGNORED_META = new Set([
  'resourceVersion',
  'generation',
  'managedFields',
  'creationTimestamp',
  'uid',
  'selfLink',
]);

const clean = (
  obj?: Record<string, unknown>,
): Record<string, unknown> | undefined => {
  if (!obj) {
    return undefined;
  }
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(obj)) {
    if (k === 'status') {
      continue;
    }
    if (k === 'metadata' && v && typeof v === 'object') {
      const meta: Record<string, unknown> = {};
      for (const [mk, mv] of Object.entries(v as Record<string, unknown>)) {
        if (!IGNORED_META.has(mk)) {
          meta[mk] = mv;
        }
      }
      out[k] = meta;
      continue;
    }
    out[k] = v;
  }
  return out;
};

const toYaml = (obj?: Record<string, unknown>): string => {
  const cleaned = clean(obj);
  if (!cleaned) {
    return '';
  }
  try {
    return yaml.dump(cleaned, { sortKeys: true, noRefs: true, lineWidth: 120 });
  } catch {
    return '';
  }
};

type LineType = 'add' | 'del' | 'ctx';
interface DiffLine {
  text: string;
  type: LineType;
  num: number;
}

const buildLines = (
  before?: Record<string, unknown>,
  after?: Record<string, unknown>,
): DiffLine[] => {
  const parts = diffLines(toYaml(before), toYaml(after));
  const lines: DiffLine[] = [];
  let n = 0;
  for (const part of parts) {
    const type: LineType = part.added ? 'add' : part.removed ? 'del' : 'ctx';
    const rows = part.value.replace(/\n$/, '').split('\n');
    for (const row of rows) {
      n += 1;
      lines.push({ text: row, type, num: n });
    }
  }
  return lines;
};

const CONTEXT = 2;
type Row = { kind: 'line'; line: DiffLine } | { kind: 'gap'; count: number };

// collapseUnchanged keeps only changed lines plus a few lines of context,
// replacing long unchanged runs with a gap marker (GitHub-style).
const collapseUnchanged = (lines: DiffLine[]): Row[] => {
  const keep = new Array(lines.length).fill(false);
  lines.forEach((l, i) => {
    if (l.type !== 'ctx') {
      for (
        let j = Math.max(0, i - CONTEXT);
        j <= Math.min(lines.length - 1, i + CONTEXT);
        j += 1
      ) {
        keep[j] = true;
      }
    }
  });
  const rows: Row[] = [];
  let gap = 0;
  for (let i = 0; i < lines.length; i += 1) {
    if (keep[i]) {
      if (gap > 0) {
        rows.push({ kind: 'gap', count: gap });
        gap = 0;
      }
      rows.push({ kind: 'line', line: lines[i] });
    } else {
      gap += 1;
    }
  }
  if (gap > 0) {
    rows.push({ kind: 'gap', count: gap });
  }
  return rows;
};

export const ChangeDiff: React.FC<{
  before?: Record<string, unknown>;
  after?: Record<string, unknown>;
  loading?: boolean;
  changedOnly?: boolean;
  highlightToken?: string;
  highlightSeq?: number;
}> = ({ before, after, loading, changedOnly, highlightToken, highlightSeq }) => {
  const lines = React.useMemo(
    () => buildLines(before, after),
    [before, after],
  );
  const rows = React.useMemo<Row[]>(
    () =>
      changedOnly
        ? collapseUnchanged(lines)
        : lines.map((line) => ({ kind: 'line', line })),
    [lines, changedOnly],
  );

  const refs = React.useRef<(HTMLDivElement | null)[]>([]);

  // The row to flash is derived from the highlight request; state only
  // records that a given request has finished flashing. Nothing is set
  // synchronously inside the effect.
  const target = React.useMemo(() => {
    if (!highlightToken) {
      return null;
    }
    const tok = highlightToken.toLowerCase();
    const idx = rows.findIndex(
      (r) =>
        r.kind === 'line' &&
        r.line.type !== 'ctx' &&
        r.line.text.toLowerCase().includes(tok),
    );
    return idx >= 0 ? { idx, seq: highlightSeq } : null;
  }, [rows, highlightToken, highlightSeq]);
  const [doneSeq, setDoneSeq] = React.useState<number | undefined>(undefined);
  const flash = target && doneSeq !== target.seq ? target.idx : null;

  React.useEffect(() => {
    if (!target) {
      return undefined;
    }
    refs.current[target.idx]?.scrollIntoView({ block: 'center', behavior: 'smooth' });
    const timer = window.setTimeout(() => { setDoneSeq(target.seq); }, 1600);
    return () => { window.clearTimeout(timer); };
  }, [target]);

  if (loading) {
    return <div className="chronos-diff-empty">Loading snapshots…</div>;
  }
  if (!before && !after) {
    return (
      <div className="chronos-diff-empty">
        No stored snapshot content for this change.
      </div>
    );
  }

  return (
    <pre className="chronos-diff" aria-label="Object diff">
      {rows.map((row, i) => {
        if (row.kind === 'gap') {
          return (
            <div key={`gap-${i}`} className="chronos-diff-gap">
              ⋯ {row.count} unchanged {row.count === 1 ? 'line' : 'lines'}
            </div>
          );
        }
        const { line } = row;
        return (
          <div
            key={i}
            ref={(el) => {
              refs.current[i] = el;
            }}
            className={`chronos-diff-line chronos-diff-${line.type}${
              flash === i ? ' chronos-diff-flash' : ''
            }`}
          >
            <span className="chronos-diff-num" aria-hidden="true">
              {line.num}
            </span>
            <span className="chronos-diff-gutter" aria-hidden="true">
              {line.type === 'add' ? '+' : line.type === 'del' ? '-' : ' '}
            </span>
            <span className="chronos-diff-text">{line.text || ' '}</span>
          </div>
        );
      })}
    </pre>
  );
};
