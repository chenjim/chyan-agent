// Small formatting/escaping helpers shared by views and tests.

/** HTML-escape a raw string (safe for innerHTML interpolation). */
export function esc(s: string): string {
  return s
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

/** Format an ISO timestamp as local "MM-DD HH:mm". */
export function fmtTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const p = (n: number) => String(n).padStart(2, '0');
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

/** Human file size: 1.2 KB / 3.4 MB. */
export function fmtSize(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(1)} ${units[i]}`;
}

const STATUS_LABEL: Record<string, string> = {
  pending: '等待中',
  running: '运行中',
  done: '已完成',
  failed: '失败',
};

export function taskStatusLabel(status: string): string {
  return STATUS_LABEL[status] ?? status;
}

/** Compact token count: 850 / 12.3k / 1.2M. */
export function fmtTokens(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0';
  if (n < 1000) return String(Math.round(n));
  if (n < 1_000_000) {
    const v = n / 1000;
    return v < 100 ? `${v.toFixed(1)}k` : `${Math.round(v)}k`;
  }
  return `${(n / 1_000_000).toFixed(1)}M`;
}

/** USD cost with enough precision for small per-turn amounts. */
export function fmtCost(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '$0';
  return n < 0.01 ? `$${n.toFixed(4)}` : `$${n.toFixed(2)}`;
}

/** Turn duration from milliseconds: 900ms / 1.2s / 1m 05s. */
export function fmtDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '0s';
  if (ms < 1000) return `${Math.round(ms)}ms`;
  const sec = ms / 1000;
  if (sec < 60) return sec < 10 ? `${sec.toFixed(1)}s` : `${Math.round(sec)}s`;
  const m = Math.floor(sec / 60);
  return `${m}m ${String(Math.round(sec % 60)).padStart(2, '0')}s`;
}
