import { describe, it, expect } from 'vitest';
import { parsePluginConfigResponse } from './static/js/plugin-config-utils.js';

describe('parsePluginConfigResponse', () => {

    it('returns null for null input', () => {
        expect(parsePluginConfigResponse(null)).toBeNull();
    });

    it('returns null for undefined input', () => {
        expect(parsePluginConfigResponse(undefined)).toBeNull();
    });

    it('returns none providers for empty config object', () => {
        const r = parsePluginConfigResponse({});
        expect(r.embedProvider).toBe('none');
        expect(r.enrichProvider).toBe('none');
        expect(r.embedOllamaModel).toBeNull();
        expect(r.embedUrl).toBeNull();
        expect(r.embedApiKey).toBeNull();
        expect(r.enrichOllamaModel).toBeNull();
        expect(r.enrichModel).toBeNull();
        expect(r.enrichApiKey).toBeNull();
    });

    it('maps empty-string provider to none', () => {
        const r = parsePluginConfigResponse({ embed_provider: '', enrich_provider: '' });
        expect(r.embedProvider).toBe('none');
        expect(r.enrichProvider).toBe('none');
    });

    // ── Embed section ────────────────────────────────────────────────────────

    it('parses ollama embed URL to model name', () => {
        const r = parsePluginConfigResponse({
            embed_provider: 'ollama',
            embed_url: 'ollama://localhost:11434/nomic-embed-text',
        });
        expect(r.embedProvider).toBe('ollama');
        expect(r.embedOllamaModel).toBe('nomic-embed-text');
        expect(r.embedUrl).toBeNull();
    });

    it('parses openai embed with custom https base URL', () => {
        const r = parsePluginConfigResponse({
            embed_provider: 'openai',
            embed_url: 'https://api.openai.com/v1',
            embed_api_key: 'sk-test-key',
        });
        expect(r.embedProvider).toBe('openai');
        expect(r.embedUrl).toBe('https://api.openai.com/v1');
        expect(r.embedApiKey).toBe('sk-test-key');
        expect(r.embedOllamaModel).toBeNull();
    });

    it('parses openai embed with http base URL', () => {
        const r = parsePluginConfigResponse({
            embed_provider: 'openai',
            embed_url: 'http://localhost:8080/v1',
        });
        expect(r.embedUrl).toBe('http://localhost:8080/v1');
    });

    it('parses voyage embed (api key only, no embed_url)', () => {
        const r = parsePluginConfigResponse({
            embed_provider: 'voyage',
            embed_url: '',
            embed_api_key: 'pa-voyage-key',
        });
        expect(r.embedProvider).toBe('voyage');
        expect(r.embedApiKey).toBe('pa-voyage-key');
        expect(r.embedUrl).toBeNull();
        expect(r.embedOllamaModel).toBeNull();
    });

    // ── Enrich section ───────────────────────────────────────────────────────

    it('parses anthropic enrich URL to model name', () => {
        const r = parsePluginConfigResponse({
            enrich_provider: 'anthropic',
            enrich_url: 'anthropic://claude-haiku-4-5-20251001',
            enrich_api_key: 'sk-ant-test',
        });
        expect(r.enrichProvider).toBe('anthropic');
        expect(r.enrichModel).toBe('claude-haiku-4-5-20251001');
        expect(r.enrichApiKey).toBe('sk-ant-test');
        expect(r.enrichOllamaModel).toBeNull();
    });

    it('parses openai enrich URL to model name', () => {
        const r = parsePluginConfigResponse({
            enrich_provider: 'openai',
            enrich_url: 'openai://gpt-4o-mini',
            enrich_api_key: 'sk-openai-test',
        });
        expect(r.enrichProvider).toBe('openai');
        expect(r.enrichModel).toBe('gpt-4o-mini');
        expect(r.enrichApiKey).toBe('sk-openai-test');
    });

    it('parses ollama enrich URL to model name', () => {
        const r = parsePluginConfigResponse({
            enrich_provider: 'ollama',
            enrich_url: 'ollama://localhost:11434/llama3.2',
        });
        expect(r.enrichProvider).toBe('ollama');
        expect(r.enrichOllamaModel).toBe('llama3.2');
        expect(r.enrichModel).toBeNull();
    });

    // ── Provider guard tests ─────────────────────────────────────────────────

    it('does not parse ollama embed URL when embed_provider is not ollama', () => {
        const r = parsePluginConfigResponse({
            embed_provider: 'openai',
            embed_url: 'ollama://localhost:11434/stale-model',
        });
        expect(r.embedProvider).toBe('openai');
        expect(r.embedOllamaModel).toBeNull();  // guard prevents stale parse
        expect(r.embedUrl).toBeNull();           // not http/https either
    });

    it('does not parse anthropic enrich URL when enrich_provider is not anthropic', () => {
        const r = parsePluginConfigResponse({
            enrich_provider: 'openai',
            enrich_url: 'anthropic://claude-haiku-4-5-20251001',
        });
        expect(r.enrichProvider).toBe('openai');
        expect(r.enrichModel).toBeNull();
    });

    it('does not parse openai enrich URL when enrich_provider is not openai', () => {
        const r = parsePluginConfigResponse({
            enrich_provider: 'anthropic',
            enrich_url: 'openai://gpt-4o-mini',
        });
        expect(r.enrichProvider).toBe('anthropic');
        expect(r.enrichModel).toBeNull();
    });

    // ── Full round-trip scenarios ────────────────────────────────────────────

    it('full anthropic enrich + ollama embed round-trip', () => {
        const r = parsePluginConfigResponse({
            embed_provider: 'ollama',
            embed_url: 'ollama://localhost:11434/nomic-embed-text',
            embed_api_key: '',
            enrich_provider: 'anthropic',
            enrich_url: 'anthropic://claude-haiku-4-5-20251001',
            enrich_api_key: 'sk-ant-real-key',
        });
        expect(r.embedProvider).toBe('ollama');
        expect(r.embedOllamaModel).toBe('nomic-embed-text');
        expect(r.enrichProvider).toBe('anthropic');
        expect(r.enrichModel).toBe('claude-haiku-4-5-20251001');
        expect(r.enrichApiKey).toBe('sk-ant-real-key');
    });
});
