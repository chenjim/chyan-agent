// @vitest-environment happy-dom
import { describe, expect, it } from 'vitest';

import { brandMarkSvg } from './brand';

describe('brandMarkSvg', () => {
  it('draws the mark with vectors only, never a text glyph', () => {
    const svg = brandMarkSvg('a');
    expect(svg).toContain('<svg');
    expect(svg).toContain('viewBox="0 0 64 64"');
    expect(svg).not.toContain('<text');
    // Bubble + prompt are real paths so the favicon stays crisp at 16px.
    expect(svg).toContain('stroke-linejoin="round"');
    expect(svg.match(/<path/g)?.length).toBeGreaterThanOrEqual(3);
  });

  it('namespaces the gradient id per instance', () => {
    const rail = brandMarkSvg('rail');
    const login = brandMarkSvg('login');
    expect(rail).toContain('id="brand-g-rail"');
    expect(rail).toContain('url(#brand-g-rail)');
    expect(login).toContain('id="brand-g-login"');
    expect(rail).not.toContain('brand-g-login');
  });
});
