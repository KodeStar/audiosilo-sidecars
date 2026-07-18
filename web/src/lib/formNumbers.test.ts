import { describe, it, expect } from 'vitest';
import { parseFloatOrNaN, parseIntOrNaN } from './formNumbers';

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

describe('parseFloatOrNaN', () => {
  it('parses integers and decimals, trimming whitespace', () => {
    expect(parseFloatOrNaN('75')).toBe(75);
    expect(parseFloatOrNaN('  120.5 ')).toBe(120.5);
    expect(parseFloatOrNaN('0')).toBe(0);
    expect(parseFloatOrNaN('-3.25')).toBe(-3.25);
  });

  it('returns NaN for blank or non-numeric input (no lenient prefix parse)', () => {
    expect(parseFloatOrNaN('')).toBeNaN();
    expect(parseFloatOrNaN('   ')).toBeNaN();
    expect(parseFloatOrNaN('x')).toBeNaN();
    expect(parseFloatOrNaN('12x')).toBeNaN();
    expect(parseFloatOrNaN('1.2.3')).toBeNaN();
    expect(parseFloatOrNaN('1e3')).toBeNaN();
  });
});
