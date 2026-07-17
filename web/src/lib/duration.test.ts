import { describe, it, expect } from 'vitest';
import { formatDuration, formatEta } from './duration';

describe('formatDuration', () => {
  it('renders seconds under a minute', () => {
    expect(formatDuration(45)).toBe('45s');
    expect(formatDuration(0.4)).toBe('0s'); // rounds below 1 -> 0
    expect(formatDuration(59.6)).toBe('1m'); // rounds up to 60s -> 1m
  });

  it('renders minutes + seconds, dropping a zero seconds part', () => {
    expect(formatDuration(320)).toBe('5m 20s');
    expect(formatDuration(300)).toBe('5m');
  });

  it('renders hours + minutes, dropping a zero minutes part', () => {
    expect(formatDuration(7800)).toBe('2h 10m');
    expect(formatDuration(7200)).toBe('2h');
  });

  it('renders just under an hour as minutes + seconds (no hour rollover)', () => {
    expect(formatDuration(3599)).toBe('59m 59s');
  });

  it('rounds the hour-branch leftover to whole minutes', () => {
    // 7199s -> round(119.98) = 120 min -> 2h exactly (no "1h 60m").
    expect(formatDuration(7199)).toBe('2h');
  });

  it('renders zero / negative / non-finite as 0s', () => {
    expect(formatDuration(0)).toBe('0s');
    expect(formatDuration(-10)).toBe('0s');
    expect(formatDuration(NaN)).toBe('0s');
  });
});

describe('formatEta', () => {
  it('renders under a minute as <1m', () => {
    expect(formatEta(0)).toBe('<1m');
    expect(formatEta(30)).toBe('<1m');
    expect(formatEta(59)).toBe('<1m');
  });

  it('rounds to whole minutes under an hour', () => {
    expect(formatEta(2400)).toBe('~40m');
    expect(formatEta(90)).toBe('~2m'); // round(1.5) = 2
  });

  it('promotes a value that rounds up to 60 minutes into the hours form', () => {
    expect(formatEta(3569)).toBe('~59m'); // round(59.48) = 59
    expect(formatEta(3599)).toBe('~1h'); // round(59.98) = 60 -> ~1h, not ~60m
    expect(formatEta(3600)).toBe('~1h');
  });

  it('renders hours + minutes, dropping a zero minutes part', () => {
    expect(formatEta(7800)).toBe('~2h 10m');
    expect(formatEta(7200)).toBe('~2h');
  });

  it('renders negative / non-finite as <1m', () => {
    expect(formatEta(-5)).toBe('<1m');
    expect(formatEta(NaN)).toBe('<1m');
  });
});
