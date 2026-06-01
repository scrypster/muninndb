import { describe, it, expect } from 'vitest';
import { secondsToMillis, nanosToMillis } from './static/js/time-utils.js';

describe('secondsToMillis', () => {
    it('converts epoch seconds to milliseconds', () => {
        // 2025-01-01T00:00:00Z = 1735689600 s
        expect(secondsToMillis(1735689600)).toBe(1735689600000);
    });

    it('returns 0 for zero', () => {
        expect(secondsToMillis(0)).toBe(0);
    });

    it('returns 0 for null', () => {
        expect(secondsToMillis(null)).toBe(0);
    });

    it('returns 0 for undefined', () => {
        expect(secondsToMillis(undefined)).toBe(0);
    });

    it('produces a valid Date (not NaN)', () => {
        const d = new Date(secondsToMillis(1735689600));
        expect(Number.isNaN(d.getTime())).toBe(false);
        expect(d.toISOString()).toBe('2025-01-01T00:00:00.000Z');
    });

    it('swallows NaN input to 0 (not NaN)', () => {
        expect(secondsToMillis(NaN)).toBe(0);
    });

    it('passes through negative input and produces a valid pre-epoch Date', () => {
        const result = secondsToMillis(-1);
        expect(result).toBe(-1000);
        const d = new Date(result);
        expect(Number.isNaN(d.getTime())).toBe(false);
    });
});

describe('nanosToMillis', () => {
    it('converts epoch nanoseconds to milliseconds (floored)', () => {
        // 2025-01-01T00:00:00Z = 1735689600000000000 ns
        expect(nanosToMillis(1735689600000000000)).toBe(1735689600000);
    });

    it('floors sub-millisecond precision', () => {
        // 1_000_999_999 ns = 1000.999999 ms → floor → 1000
        expect(nanosToMillis(1000999999)).toBe(1000);
    });

    it('returns 0 for zero', () => {
        expect(nanosToMillis(0)).toBe(0);
    });

    it('returns 0 for null', () => {
        expect(nanosToMillis(null)).toBe(0);
    });

    it('returns 0 for undefined', () => {
        expect(nanosToMillis(undefined)).toBe(0);
    });

    it('produces a valid Date (not NaN)', () => {
        const d = new Date(nanosToMillis(1735689600000000000));
        expect(Number.isNaN(d.getTime())).toBe(false);
        expect(d.toISOString()).toBe('2025-01-01T00:00:00.000Z');
    });

    it('is falsy for zero so the `nanosToMillis(...) || fallback` chain falls through', () => {
        // Mirrors the /api/engrams/{id} mapping site:
        //   nanosToMillis(full.created_at) || m.createdAt
        // A zero nanos value must be falsy so the fallback (m.createdAt) wins.
        expect(nanosToMillis(0)).toBe(0);
        expect(nanosToMillis(0) || 1700000000000).toBe(1700000000000);
    });

    it('swallows NaN input to 0 (not NaN)', () => {
        expect(nanosToMillis(NaN)).toBe(0);
    });

    it('passes through negative input and produces a valid pre-epoch Date', () => {
        const result = nanosToMillis(-1000000);
        expect(result).toBe(-1);
        const d = new Date(result);
        expect(Number.isNaN(d.getTime())).toBe(false);
    });
});
