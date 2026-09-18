// changedPaths computes the leaf field paths that differ between two object
// versions, so the TOC works for any pair the user selects (not just the
// original change). It mirrors the operator's diff normalization: server-managed
// noise (status, resourceVersion, managedFields, ...) is ignored.

const IGNORED_META = new Set([
  'resourceVersion',
  'generation',
  'managedFields',
  'creationTimestamp',
  'uid',
  'selfLink',
]);

type Obj = Record<string, unknown>;

const normalize = (obj?: Obj): Obj | undefined => {
  if (!obj) {
    return undefined;
  }
  const out: Obj = {};
  for (const [k, v] of Object.entries(obj)) {
    if (k === 'status') {
      continue;
    }
    if (k === 'metadata' && v && typeof v === 'object') {
      const meta: Obj = {};
      for (const [mk, mv] of Object.entries(v as Obj)) {
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

const isObject = (v: unknown): v is Obj =>
  typeof v === 'object' && v !== null && !Array.isArray(v);

const join = (prefix: string, key: string): string =>
  prefix ? `${prefix}.${key}` : key;

const walk = (path: string, a: unknown, b: unknown, out: string[]): void => {
  if (a === b) {
    return;
  }
  if (isObject(a) || isObject(b)) {
    const am = (isObject(a) ? a : {});
    const bm = (isObject(b) ? b : {});
    const keys = new Set([...Object.keys(am), ...Object.keys(bm)]);
    for (const key of keys) {
      walk(join(path, key), am[key], bm[key], out);
    }
    return;
  }
  if (Array.isArray(a) || Array.isArray(b)) {
    const aa = Array.isArray(a) ? a : [];
    const ba = Array.isArray(b) ? b : [];
    const n = Math.max(aa.length, ba.length);
    for (let i = 0; i < n; i += 1) {
      walk(`${path}[${i}]`, aa[i], ba[i], out);
    }
    return;
  }
  if (JSON.stringify(a) !== JSON.stringify(b)) {
    out.push(path);
  }
};

export const changedPaths = (before?: Obj, after?: Obj): string[] => {
  const out: string[] = [];
  walk('', normalize(before), normalize(after), out);
  return out.sort();
};
