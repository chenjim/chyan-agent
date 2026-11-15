import { describe, expect, it } from 'vitest';
import { chip, emptyState, errorState, skeleton } from './render';

describe('skeleton', () => {
  it('emits the requested number of placeholder rows', () => {
    expect(skeleton()).toMatch(/^<div class="skel"><\/div>(<div class="skel"><\/div>){2}$/);
    expect(skeleton(0)).toBe('');
    expect(skeleton(5).match(/class="skel"/g)).toHaveLength(5);
  });
});

describe('emptyState / errorState', () => {
  it('escapes the empty-state text', () => {
    expect(emptyState('<none>')).toBe(
      '<div class="empty"><div class="empty-title">&lt;none&gt;</div></div>',
    );
  });

  it('renders an optional hint below the title', () => {
    expect(emptyState('空', '<hint>')).toBe(
      '<div class="empty"><div class="empty-title">空</div>' +
        '<div class="empty-hint">&lt;hint&gt;</div></div>',
    );
  });

  it('renders Error messages escaped', () => {
    expect(errorState(new Error('<boom>'))).toContain('加载失败：&lt;boom&gt;');
  });

  it('stringifies non-Error values', () => {
    expect(errorState('plain')).toContain('加载失败：plain');
    expect(errorState({ code: 1 })).toContain('加载失败：[object Object]');
  });
});

describe('chip', () => {
  it('escapes the label and appends the optional class', () => {
    expect(chip('a<b')).toBe('<span class="chip ">a&lt;b</span>');
    expect(chip('x', 'mono')).toBe('<span class="chip mono">x</span>');
  });
});
