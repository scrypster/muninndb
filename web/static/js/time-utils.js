/**
 * time-utils.js — pure created_at unit normalization helpers.
 *
 * The REST API returns created_at in INCONSISTENT units depending on the
 * endpoint, because the backend response types are shared with the MCP and
 * gRPC transports and cannot be changed for the Web UI alone:
 *
 *   • /api/engrams and /api/session      use .Unix()      → SECONDS
 *   • /api/activate and /api/engrams/{id} use .UnixNano() → NANOSECONDS
 *
 * The Web UI normalizes every created_at to epoch MILLISECONDS at each mapping
 * site so the rest of the frontend can treat the value uniformly and templates
 * can render it with `new Date(createdAt)` (no `* 1000`). When the input is
 * falsy (null / undefined / 0) the result stays 0 so the
 * `createdAt ? … : 'unknown'` template branches still work.
 *
 * Loaded as a <script type="module"> so it can be imported by Vitest with ES
 * module syntax. The globalThis assignment at the bottom exposes MuninnTime as a
 * browser global so the non-module app.js can call it after Alpine init.
 */

/**
 * Convert an epoch-seconds value to epoch-milliseconds.
 * Falsy input (null / undefined / 0) is preserved as 0.
 * @param {number|null|undefined} value
 * @returns {number}
 */
export function secondsToMillis(value) {
    return value ? value * 1000 : 0;
}

/**
 * Convert an epoch-nanoseconds value to epoch-milliseconds.
 * Falsy input (null / undefined / 0) is preserved as 0.
 * @param {number|null|undefined} value
 * @returns {number}
 */
export function nanosToMillis(value) {
    return value ? Math.floor(value / 1e6) : 0;
}

// Expose as a browser global so the non-module app.js can access it after Alpine
// initializes. Module scripts execute before Alpine's deferred init, so this is
// always set by the time any component method runs.
globalThis.MuninnTime = { secondsToMillis, nanosToMillis };
