import { describe, it, expect } from 'vitest';
import { parseIntOrNaN } from './formNumbers';

describe('parseIntOrNaN', () => {
  it('parses a plain base-10 integer, trimming surrounding whitespace', () => {
    expect(parseIntOrNaN('10')).toBe(10);
    expect(parseIntOrNaN('  42  ')).toBe(42);
    expect(parseIntOrNaN('-3')).toBe(-3);
    expect(parseIntOrNaN('0')).toBe(0);
  });

  it('returns NaN for blank input', () => {
    expect(parseIntOrNaN('')).toBeNaN();
    expect(parseIntOrNaN('   ')).toBeNaN();
  });

  it('returns NaN for non-numeric or non-integer input (no lenient prefix parse)', () => {
    expect(parseIntOrNaN('x')).toBeNaN();
    expect(parseIntOrNaN('12x')).toBeNaN();
    expect(parseIntOrNaN('1.5')).toBeNaN();
    expect(parseIntOrNaN('1e3')).toBeNaN();
  });
});
