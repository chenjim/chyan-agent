import { describe, expect, it } from 'vitest';
import { esc, fmtCost, fmtDuration, fmtSize, fmtTime, fmtTokens, taskStatusLabel } from './format';

describe('esc', () => {
  it('escapes every HTML metacharacter', () => {
    expect(esc(`<a href="x">'&'</a>`)).toBe(
      '&lt;a href=&quot;x&quot;&gt;&#39;&amp;&#39;&lt;/a&gt;',
    );
  });

  it('leaves plain text untouched', () => {
    expect(esc('plain text 123')).toBe('plain text 123');
  });
});

describe('fmtTime', () => {
  it('formats an ISO timestamp as MM-DD HH:mm', () => {
    // Local time depends on TZ; assert the shape instead of the wall clock.
    expect(fmtTime('2024-03-04T05:06:07Z')).toMatch(/^\d{2}-\d{2} \d{2}:\d{2}$/);
  });

  it('returns the raw input when unparsable', () => {
    expect(fmtTime('not-a-date')).toBe('not-a-date');
    expect(fmtTime('')).toBe('');
  });
});

describe('fmtSize', () => {
  it('uses bytes below 1 KiB', () => {
    expect(fmtSize(0)).toBe('0 B');
    expect(fmtSize(1023)).toBe('1023 B');
  });

  it('scales through KB/MB/GB/TB', () => {
    expect(fmtSize(1024)).toBe('1.0 KB');
    expect(fmtSize(1536)).toBe('1.5 KB');
    expect(fmtSize(1024 * 1024)).toBe('1.0 MB');
    expect(fmtSize(1024 ** 3)).toBe('1.0 GB');
    expect(fmtSize(1024 ** 4)).toBe('1.0 TB');
    // Beyond TB the value keeps growing in TB units.
    expect(fmtSize(1024 ** 5)).toBe('1024.0 TB');
  });
});

describe('taskStatusLabel', () => {
  it('maps known statuses to Chinese labels', () => {
    expect(taskStatusLabel('pending')).toBe('等待中');
    expect(taskStatusLabel('running')).toBe('运行中');
    expect(taskStatusLabel('done')).toBe('已完成');
    expect(taskStatusLabel('failed')).toBe('失败');
  });

  it('passes unknown statuses through', () => {
    expect(taskStatusLabel('interrupted')).toBe('interrupted');
  });
});

describe('usage formatting', () => {
  it('compacts token counts', () => {
    expect(fmtTokens(0)).toBe('0');
    expect(fmtTokens(850)).toBe('850');
    expect(fmtTokens(12345)).toBe('12.3k');
    expect(fmtTokens(999999)).toBe('1000k');
    expect(fmtTokens(1200000)).toBe('1.2M');
  });

  it('formats small costs with four decimals', () => {
    expect(fmtCost(0)).toBe('$0');
    expect(fmtCost(0.00042)).toBe('$0.0004');
    expect(fmtCost(0.1234)).toBe('$0.12');
  });

  it('formats turn durations', () => {
    expect(fmtDuration(0)).toBe('0s');
    expect(fmtDuration(900)).toBe('900ms');
    expect(fmtDuration(1200)).toBe('1.2s');
    expect(fmtDuration(65000)).toBe('1m 05s');
  });
});
