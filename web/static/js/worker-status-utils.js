/**
 * Pure /api/workers response mapping shared by both Admin worker views.
 *
 * The server owns lifecycle meaning through the symbolic `status` and
 * `enabled` fields. This module deliberately does not interpret the legacy
 * numeric `state`, processed counters, or nonexistent client-side flags.
 */

const WORKERS = [
    ['hebbian', 'Hebbian Learning'],
    ['contradict', 'Contradiction Detection'],
    ['confidence', 'Confidence Updates'],
];

const PRESENTATION = {
    active:   { label: 'Active',   badgeClass: 'badge-active',  color: '#10b981' },
    idle:     { label: 'Idle',     badgeClass: 'badge-idle',    color: '#f59e0b' },
    dormant:  { label: 'Dormant',  badgeClass: 'badge-dormant', color: '#9ca3af' },
    stopped:  { label: 'Stopped',  badgeClass: 'badge-stopped',  color: '#ef4444' },
    disabled: { label: 'Disabled', badgeClass: 'badge-disabled', color: '#64748b' },
    unknown:  { label: 'Unknown',  badgeClass: 'badge-idle',    color: '#f59e0b' },
};

function normalizeStatus(stats) {
    if (!stats || stats.enabled === false || stats.status === 'disabled') return 'disabled';
    return Object.prototype.hasOwnProperty.call(PRESENTATION, stats.status) ? stats.status : 'unknown';
}

/**
 * Map only real, declared worker fields present in an /api/workers response.
 * Extra fields (including the historical synthetic `decay`) are ignored.
 */
export function mapWorkerResponse(response) {
    if (!response || typeof response !== 'object') return [];

    return WORKERS.flatMap(([key, name]) => {
        if (!Object.prototype.hasOwnProperty.call(response, key)) return [];
        const stats = response[key];
        const status = normalizeStatus(stats);
        const presentation = PRESENTATION[status];
        return [{
            name,
            enabled: status !== 'disabled',
            status,
            statusLabel: presentation.label,
            badgeClass: presentation.badgeClass,
            color: presentation.color,
            state: stats?.state ?? 0, // retained for consumers; never interpreted here
            processed: stats?.processed ?? 0,
            batches: stats?.batches ?? 0,
            errors: stats?.errors ?? 0,
            dropped: stats?.dropped ?? 0,
            lastRun: stats?.lastRun ?? 0,
            effectiveWait: stats?.effectiveWait ?? 0,
        }];
    });
}

globalThis.MuninnWorkers = { mapWorkerResponse };
