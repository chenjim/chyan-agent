// Tests for diff classification and safe HTML rendering.

import { describe, expect, it } from 'vitest';
import { classifyDiffLine, renderDiffHtml, splitDiff } from './diff';
import { esc } from './format';

describe('classifyDiffLine', () => {
  it('classifies unified diff markers', () => {
    expect(classifyDiffLine('+added').kind).toBe('add');
    expect(classifyDiffLine('-removed').kind).toBe('del');
    expect(classifyDiffLine('@@ -1,2 +1,3 @@').kind).toBe('hunk');
    expect(classifyDiffLine('+++ b/a.go').kind).toBe('meta');
    expect(classifyDiffLine('--- a/a.go').kind).toBe('meta');
    expect(classifyDiffLine('diff --git a/x b/x').kind).toBe('meta');
    expect(classifyDiffLine('index 123..456').kind).toBe('meta');
    expect(classifyDiffLine(' context').kind).toBe('ctx');
    expect(classifyDiffLine('').kind).toBe('ctx');
  });

  it('keeps original text', () => {
    expect(classifyDiffLine('+a <b>').text).toBe('+a <b>');
  });
});

describe('splitDiff', () => {
  it('splits on LF and CRLF', () => {
    expect(splitDiff('+a\n-b').map((l) => l.kind)).toEqual(['add', 'del']);
    expect(splitDiff('+a\r\n-b').map((l) => l.kind)).toEqual(['add', 'del']);
  });
});

describe('renderDiffHtml', () => {
  it('escapes HTML in diff text', () => {
    const html = renderDiffHtml('+<script>alert(1)</script>', esc);
    expect(html).not.toContain('<script>');
    expect(html).toContain('&lt;script&gt;');
  });

  it('escapes quotes and ampersands', () => {
    const html = renderDiffHtml(`-'x' & "y"`, esc);
    expect(html).toContain('&#39;x&#39;');
    expect(html).toContain('&quot;y&quot;');
    expect(html).toContain('&amp;');
  });

  it('wraps lines with kind classes', () => {
    const html = renderDiffHtml('@@ -1 +1 @@\n+x\n-y\n z', esc);
    expect(html).toContain('diff-hunk');
    expect(html).toContain('diff-add');
    expect(html).toContain('diff-del');
    expect(html).toContain('diff-ctx');
  });

  it('renders placeholder for empty diff', () => {
    expect(renderDiffHtml('', esc)).toContain('无 diff 预览');
    expect(renderDiffHtml('', esc)).not.toContain('diff-line');
  });

  it('renders blank line as non-breaking space', () => {
    const html = renderDiffHtml('\n', esc);
    expect(html).toContain('&nbsp;');
  });
});
