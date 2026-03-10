/**
 * plugin-config-utils.js — pure plugin config parsing utilities.
 *
 * Loaded as a <script type="module"> so it can be imported by Vitest with ES
 * module syntax. The globalThis assignment at the bottom exposes MuninnPluginCfg
 * as a browser global so the non-module app.js can call it after Alpine init.
 *
 * Execution order in index.html:
 *   1. app.js  (sync/blocking — defines Alpine data component, does NOT call methods)
 *   2. plugin-config-utils.js  (module — sets globalThis.MuninnPluginCfg)
 *   3. alpine.js  (defer — initializes Alpine; methods are now callable)
 *
 * Because Alpine.js is deferred and runs after step 2, MuninnPluginCfg is always
 * defined before any component method (e.g. loadSavedPluginConfig) is invoked.
 */

/**
 * Parse a GET /api/admin/plugin-config response into Alpine-ready plugin state.
 *
 * Returns an object where null fields mean "leave Alpine default unchanged".
 * Provider fields always return a string ('none' when empty/absent).
 *
 * URL decoding mirrors the save conventions in savePluginConfig (app.js):
 *   embed_url  "ollama://localhost:11434/{model}" → embedOllamaModel
 *   embed_url  "http(s)://..."                   → embedUrl (custom base URL)
 *   enrich_url "ollama://localhost:11434/{model}" → enrichOllamaModel
 *   enrich_url "anthropic://{model}"              → enrichModel
 *   enrich_url "openai://{model}"                 → enrichModel
 *
 * @param {object|null} data - raw API response object
 * @returns {object|null} parsed state, or null when data is falsy
 */
export function parsePluginConfigResponse(data) {
    if (!data) return null;

    const result = {
        embedProvider:     data.embed_provider  || 'none',
        embedOllamaModel:  null,
        embedUrl:          null,
        embedApiKey:       data.embed_api_key   || null,
        enrichProvider:    data.enrich_provider || 'none',
        enrichOllamaModel: null,
        enrichModel:       null,
        enrichApiKey:      data.enrich_api_key  || null,
    };

    // ── Embed URL parsing ─────────────────────────────────────────────────────
    const embedUrl = data.embed_url || '';
    if (result.embedProvider === 'ollama' && embedUrl.startsWith('ollama://')) {
        // "ollama://localhost:11434/nomic-embed-text" → "nomic-embed-text"
        const model = embedUrl.split('/').pop();
        if (model) result.embedOllamaModel = model;
    } else if (embedUrl.startsWith('http://') || embedUrl.startsWith('https://')) {
        result.embedUrl = embedUrl;
    }

    // ── Enrich URL parsing ────────────────────────────────────────────────────
    const enrichUrl = data.enrich_url || '';
    if (result.enrichProvider === 'ollama' && enrichUrl.startsWith('ollama://')) {
        // "ollama://localhost:11434/llama3.2" → "llama3.2"
        const model = enrichUrl.split('/').pop();
        if (model) result.enrichOllamaModel = model;
    } else if (result.enrichProvider === 'anthropic' && enrichUrl.startsWith('anthropic://')) {
        // "anthropic://claude-haiku-4-5-20251001" → "claude-haiku-4-5-20251001"
        const model = enrichUrl.replace('anthropic://', '');
        if (model) result.enrichModel = model;
    } else if (result.enrichProvider === 'openai' && enrichUrl.startsWith('openai://')) {
        // "openai://gpt-4o-mini" → "gpt-4o-mini"
        const model = enrichUrl.replace('openai://', '');
        if (model) result.enrichModel = model;
    }

    return result;
}

// Expose as a browser global so the non-module app.js can access it after Alpine
// initializes. Module scripts execute before Alpine's deferred init, so this is
// always set by the time any component method runs.
globalThis.MuninnPluginCfg = { parsePluginConfigResponse };
