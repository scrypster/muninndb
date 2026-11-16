import {
  MuninnConnectionError,
  MuninnTimeoutError,
  errorFromStatus,
} from "./errors.js";
import { streamSse } from "./sse.js";
import type {
  ActivateOptions,
  ActivateResponse,
  AssociationItem,
  BatchWriteResponse,
  ConsolidateOptions,
  ConsolidateResponse,
  ContradictionsResponse,
  DecideOptions,
  DecideResponse,
  Engram,
  EvolveResponse,
  ExplainOptions,
  ExplainResponse,
  HealthResponse,
  LinkOptions,
  ListDeletedResponse,
  ListEngramsResponse,
  MuninnClientOptions,
  RestoreResponse,
  RetryEnrichResponse,
  SessionResponse,
  SetStateResponse,
  SseEvent,
  StatsResponse,
  TraverseOptions,
  TraverseResponse,
  VaultsResponse,
  WriteOptions,
  WriteResponse,
} from "./types.js";
import { stripTrailingSlashes } from "./url.js";

const DEFAULT_BASE_URL = "http://localhost:8476";
const DEFAULT_TIMEOUT = 30_000;
const DEFAULT_MAX_RETRIES = 3;
const DEFAULT_RETRY_BACKOFF = 500;
const DEFAULT_VAULT = "default";

type HttpMethod = "GET" | "POST" | "PUT" | "DELETE";

interface RequestOptions {
  method: HttpMethod;
  path: string;
  body?: unknown;
  query?: Record<string, string | number | boolean | undefined>;
  /** Skip automatic retry for this request. */
  noRetry?: boolean;
}

/**
 * Official TypeScript client for MuninnDB.
 *
 * Requires Node.js 18+ (uses native `fetch`).
 */
export class MuninnClient {
  private readonly baseUrl: string;
  private readonly token: string;
  private readonly timeout: number;
  private readonly maxRetries: number;
  private readonly retryBackoff: number;
  private readonly defaultVault: string;
  private activeAbortControllers = new Set<AbortController>();

  constructor(options: MuninnClientOptions) {
    this.baseUrl = stripTrailingSlashes(options.baseUrl ?? DEFAULT_BASE_URL);
    this.token = options.token;
    this.timeout = options.timeout ?? DEFAULT_TIMEOUT;
    this.maxRetries = options.maxRetries ?? DEFAULT_MAX_RETRIES;
    this.retryBackoff = options.retryBackoff ?? DEFAULT_RETRY_BACKOFF;
    this.defaultVault = options.defaultVault ?? DEFAULT_VAULT;
  }

  // -----------------------------------------------------------------------
  // Core CRUD
  // -----------------------------------------------------------------------

  /** Write a single engram. */
  async write(options: WriteOptions): Promise<WriteResponse> {
    const vault = options.vault ?? this.defaultVault;
    const body = { ...options, vault };
    return this.request<WriteResponse>({
      method: "POST",
      path: "/api/engrams",
      body,
      query: { vault },
    });
  }

  /** Write a batch of engrams (max 50). */
  async writeBatch(
    vault: string,
    engrams: WriteOptions[],
  ): Promise<BatchWriteResponse> {
    return this.request<BatchWriteResponse>({
      method: "POST",
      path: "/api/engrams/batch",
      body: {
        engrams: engrams.map((e) => ({ ...e, vault: e.vault ?? vault })),
      },
      query: { vault },
    });
  }

  /** Semantic recall / activation query. */
  async activate(options: ActivateOptions): Promise<ActivateResponse> {
    const vault = options.vault ?? this.defaultVault;
    const body = { ...options, vault };
    return this.request<ActivateResponse>({
      method: "POST",
      path: "/api/activate",
      body,
      query: { vault },
    });
  }

  /** Read a single engram by ID. */
  async read(id: string, vault?: string): Promise<Engram> {
    return this.request<Engram>({
      method: "GET",
      path: `/api/engrams/${encodeURIComponent(id)}`,
      query: { vault: vault ?? this.defaultVault },
    });
  }

  /** Soft-delete an engram. */
  async forget(id: string, vault?: string): Promise<void> {
    await this.request<unknown>({
      method: "DELETE",
      path: `/api/engrams/${encodeURIComponent(id)}`,
      query: { vault: vault ?? this.defaultVault },
    });
  }

  /** Create an association between two engrams. */
  async link(options: LinkOptions): Promise<void> {
    const vault = options.vault ?? this.defaultVault;
    const body = { ...options, vault };
    await this.request<unknown>({ method: "POST", path: "/api/link", body, query: { vault } });
  }

  // -----------------------------------------------------------------------
  // Extended operations
  // -----------------------------------------------------------------------

  /** Evolve (update) an engram's content. */
  async evolve(
    id: string,
    newContent: string,
    reason: string,
    vault?: string,
  ): Promise<EvolveResponse> {
    return this.request<EvolveResponse>({
      method: "POST",
      path: `/api/engrams/${encodeURIComponent(id)}/evolve`,
      body: { new_content: newContent, reason },
      query: { vault: vault ?? this.defaultVault },
    });
  }

  /** Consolidate (merge) multiple engrams into one. */
  async consolidate(
    options: ConsolidateOptions,
  ): Promise<ConsolidateResponse> {
    const vault = options.vault ?? this.defaultVault;
    const body = { ...options, vault };
    return this.request<ConsolidateResponse>({
      method: "POST",
      path: "/api/consolidate",
      body,
      query: { vault },
    });
  }

  /** Record a decision. */
  async decide(options: DecideOptions): Promise<DecideResponse> {
    const vault = options.vault ?? this.defaultVault;
    const body = { ...options, vault };
    return this.request<DecideResponse>({
      method: "POST",
      path: "/api/decide",
      body,
      query: { vault },
    });
  }

  /** Restore a soft-deleted engram. */
  async restore(id: string, vault?: string): Promise<RestoreResponse> {
    return this.request<RestoreResponse>({
      method: "POST",
      path: `/api/engrams/${encodeURIComponent(id)}/restore`,
      query: { vault: vault ?? this.defaultVault },
    });
  }

  /** Traverse the association graph from a starting engram. */
  async traverse(options: TraverseOptions): Promise<TraverseResponse> {
    const vault = options.vault ?? this.defaultVault;
    const body = { ...options, vault };
    return this.request<TraverseResponse>({
      method: "POST",
      path: "/api/traverse",
      body,
      query: { vault },
    });
  }

  /** Get a scoring breakdown for an engram against a query. */
  async explain(options: ExplainOptions): Promise<ExplainResponse> {
    const vault = options.vault ?? this.defaultVault;
    const body = { ...options, vault };
    return this.request<ExplainResponse>({
      method: "POST",
      path: "/api/explain",
      body,
      query: { vault },
    });
  }

  /** Transition an engram's lifecycle state. */
  async setState(
    id: string,
    state: string,
    reason?: string,
    vault?: string,
  ): Promise<SetStateResponse> {
    return this.request<SetStateResponse>({
      method: "PUT",
      path: `/api/engrams/${encodeURIComponent(id)}/state`,
      body: { state, reason },
      query: { vault: vault ?? this.defaultVault },
    });
  }

  /** List soft-deleted engrams. */
  async listDeleted(
    vault?: string,
    limit?: number,
  ): Promise<ListDeletedResponse> {
    return this.request<ListDeletedResponse>({
      method: "GET",
      path: "/api/deleted",
      query: { vault: vault ?? this.defaultVault, limit },
    });
  }

  /** Re-queue enrichment plugins for an engram. */
  async retryEnrich(
    id: string,
    vault?: string,
  ): Promise<RetryEnrichResponse> {
    return this.request<RetryEnrichResponse>({
      method: "POST",
      path: `/api/engrams/${encodeURIComponent(id)}/retry-enrich`,
      query: { vault: vault ?? this.defaultVault },
    });
  }

  /** List detected contradictions in a vault. */
  async contradictions(vault?: string): Promise<ContradictionsResponse> {
    return this.request<ContradictionsResponse>({
      method: "GET",
      path: "/api/contradictions",
      query: { vault: vault ?? this.defaultVault },
    });
  }

  /** Get the usage guide text for a vault. */
  async guide(vault?: string): Promise<string> {
    const res = await this.request<{ guide: string }>({
      method: "GET",
      path: "/api/guide",
      query: { vault: vault ?? this.defaultVault },
    });
    return res.guide;
  }

  // -----------------------------------------------------------------------
  // Query & List
  // -----------------------------------------------------------------------

  /** Get vault statistics. */
  async stats(vault?: string): Promise<StatsResponse> {
    return this.request<StatsResponse>({
      method: "GET",
      path: "/api/stats",
      query: { vault: vault ?? this.defaultVault },
    });
  }

  /** List engrams with pagination. */
  async listEngrams(
    vault?: string,
    limit?: number,
    offset?: number,
  ): Promise<ListEngramsResponse> {
    return this.request<ListEngramsResponse>({
      method: "GET",
      path: "/api/engrams",
      query: { vault: vault ?? this.defaultVault, limit, offset },
    });
  }

  /** Get associations (links) for an engram. */
  async getLinks(id: string, vault?: string): Promise<AssociationItem[]> {
    const res = await this.request<{ links: AssociationItem[] }>({
      method: "GET",
      path: `/api/engrams/${encodeURIComponent(id)}/links`,
      query: { vault: vault ?? this.defaultVault },
    });
    return res.links;
  }

  /** List all vaults. */
  async listVaults(): Promise<string[]> {
    const res = await this.request<VaultsResponse>({
      method: "GET",
      path: "/api/vaults",
    });
    return res.vaults;
  }

  /** Get session activity for a vault. */
  async session(
    vault?: string,
    since?: string,
    limit?: number,
    offset?: number,
  ): Promise<SessionResponse> {
    return this.request<SessionResponse>({
      method: "GET",
      path: "/api/session",
      query: { vault: vault ?? this.defaultVault, since, limit, offset },
    });
  }

  // -----------------------------------------------------------------------
  // Streaming & Health
  // -----------------------------------------------------------------------

  /**
   * Subscribe to real-time SSE events for a vault.
   *
   * Returns an `AsyncIterable` that yields {@link SseEvent} objects.
   * The connection is kept alive until the iterable is broken out of or
   * {@link close} is called.
   */
  subscribe(vault?: string, pushOnWrite = true, threshold?: number): AsyncIterable<SseEvent> {
    const v = vault ?? this.defaultVault;
    let url = `${this.baseUrl}/api/subscribe?vault=${encodeURIComponent(v)}&push_on_write=${pushOnWrite}`;
    if (threshold !== undefined) {
      url += `&threshold=${threshold}`;
    }
    const ac = new AbortController();
    this.activeAbortControllers.add(ac);

    const headers: Record<string, string> = {
      Authorization: `Bearer ${this.token}`,
      Accept: "text/event-stream",
    };

    const doFetch = async () => {
      const res = await fetch(url, { headers, signal: ac.signal });
      if (!res.ok || !res.body) {
        throw errorFromStatus(res.status, await this.safeJson(res));
      }
      return res.body;
    };

    const self = this;
    async function* gen(): AsyncGenerator<SseEvent, void, undefined> {
      try {
        const body = await doFetch();
        yield* streamSse(body);
      } finally {
        ac.abort();
        self.activeAbortControllers.delete(ac);
      }
    }

    return gen();
  }

  /** Health check. */
  async health(): Promise<HealthResponse> {
    return this.request<HealthResponse>({
      method: "GET",
      path: "/api/health",
      noRetry: true,
    });
  }

  // -----------------------------------------------------------------------
  // Lifecycle
  // -----------------------------------------------------------------------

  /** Abort all in-flight requests and SSE subscriptions. */
  close(): void {
    for (const ac of this.activeAbortControllers) {
      ac.abort();
    }
    this.activeAbortControllers.clear();
  }

  // -----------------------------------------------------------------------
  // Internal helpers
  // -----------------------------------------------------------------------

  private async request<T>(opts: RequestOptions): Promise<T> {
    const url = this.buildUrl(opts.path, opts.query);
    const headers: Record<string, string> = {
      Authorization: `Bearer ${this.token}`,
    };

    let bodyStr: string | undefined;
    if (opts.body !== undefined) {
      headers["Content-Type"] = "application/json";
      bodyStr = JSON.stringify(opts.body);
    }

    const attempts = opts.noRetry ? 1 : this.maxRetries + 1;

    for (let attempt = 0; attempt < attempts; attempt++) {
      const ac = new AbortController();
      this.activeAbortControllers.add(ac);
      const timer = setTimeout(() => ac.abort(), this.timeout);

      try {
        const res = await fetch(url, {
          method: opts.method,
          headers,
          body: bodyStr,
          signal: ac.signal,
        });

        if (res.ok) {
          const text = await res.text();
          if (text.length === 0) return undefined as unknown as T;
          return JSON.parse(text) as T;
        }

        const errorBody = await this.safeJson(res);

        // Only retry on 429 or 5xx transient errors.
        if (
          attempt < attempts - 1 &&
          (res.status === 429 || res.status >= 500)
        ) {
          await this.backoff(attempt);
          continue;
        }

        throw errorFromStatus(res.status, errorBody);
      } catch (err: unknown) {
        if (err instanceof DOMException && err.name === "AbortError") {
          if (attempt < attempts - 1) {
            await this.backoff(attempt);
            continue;
          }
          throw new MuninnTimeoutError();
        }

        // Network-level failures.
        if (
          err instanceof TypeError ||
          (err instanceof Error && err.message === "fetch failed")
        ) {
          if (attempt < attempts - 1) {
            await this.backoff(attempt);
            continue;
          }
          throw new MuninnConnectionError(
            (err as Error).message ?? "Connection failed",
            err,
          );
        }

        throw err;
      } finally {
        clearTimeout(timer);
        this.activeAbortControllers.delete(ac);
      }
    }

    // Unreachable, but satisfies the compiler.
    throw new MuninnConnectionError("All retry attempts exhausted");
  }

  private buildUrl(
    path: string,
    query?: Record<string, string | number | boolean | undefined>,
  ): string {
    const url = new URL(path, this.baseUrl);
    if (query) {
      for (const [k, v] of Object.entries(query)) {
        if (v !== undefined) url.searchParams.set(k, String(v));
      }
    }
    return url.toString();
  }

  private async backoff(attempt: number): Promise<void> {
    const base = this.retryBackoff * 2 ** attempt;
    const jitter = Math.random() * base * 0.5;
    await new Promise((resolve) => setTimeout(resolve, base + jitter));
  }

  private async safeJson(res: Response): Promise<unknown> {
    try {
      return await res.json();
    } catch {
      return undefined;
    }
  }
}
