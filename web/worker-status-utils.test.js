import { describe, expect, it } from 'vitest';
import { mapWorkerResponse } from './static/js/worker-status-utils.js';

describe('mapWorkerResponse', () => {
    it.each([
        ['dormant', 0, 'Dormant', true],
        ['idle', 0, 'Idle', true],
        ['active', 8, 'Active', true],
        ['stopped', 8, 'Stopped', true],
        ['disabled', 0, 'Disabled', false],
    ])('renders %s authoritatively with processed=%i', (status, processed, label, enabled) => {
        const [row] = mapWorkerResponse({
            hebbian: { enabled, status, processed, errors: 2 },
        });

        expect(row).toMatchObject({
            name: 'Hebbian Learning', status, statusLabel: label, enabled, processed, errors: 2,
        });
    });

    it('degrades an unknown future server state safely', () => {
        const [row] = mapWorkerResponse({
            hebbian: { enabled: true, status: 'hibernating', processed: 0 },
        });

        expect(row).toMatchObject({ enabled: true, status: 'unknown', statusLabel: 'Unknown' });
    });

    it('does not infer status from processed or a nonexistent running field', () => {
        const [row] = mapWorkerResponse({
            hebbian: { enabled: true, status: 'idle', processed: 0, running: true },
        });

        expect(row.statusLabel).toBe('Idle');
    });

    it('only creates rows for real API worker fields', () => {
        const rows = mapWorkerResponse({
            hebbian: { enabled: true, status: 'dormant', processed: 0 },
            decay: { enabled: true, status: 'active', processed: 1 },
            future_worker: { enabled: true, status: 'active', processed: 1 },
        });

        expect(rows.map((row) => row.name)).toEqual(['Hebbian Learning']);
        expect(rows.some((row) => row.name === 'Temporal Scoring')).toBe(false);
    });

    it('does not invent rows when the response omits worker fields', () => {
        expect(mapWorkerResponse({})).toEqual([]);
        expect(mapWorkerResponse(null)).toEqual([]);
    });

    it('gives stopped and disabled workers distinct badge styles', () => {
        const [stopped] = mapWorkerResponse({
            hebbian: { enabled: true, status: 'stopped', processed: 0 },
        });
        const [disabled] = mapWorkerResponse({
            hebbian: { enabled: false, status: 'disabled', processed: 0 },
        });

        expect(stopped.badgeClass).toBe('badge-stopped');
        expect(disabled.badgeClass).toBe('badge-disabled');
        expect(stopped.badgeClass).not.toBe(disabled.badgeClass);
    });
});
