// app.js — MuninnDB Alpine.js application
// Alpine.js is loaded from vendor (with defer) AFTER this file.
// alpine:init fires before Alpine initializes DOM — correct hook for Alpine.data()

document.addEventListener('alpine:init', () => {
  Alpine.data('muninnApp', () => ({
    // ── State ──────────────────────────────────────────────────────────────
    currentView: 'dashboard',
    vault: localStorage.getItem('muninnVault') || 'default',
    vaults: ['default'],
    vaultStats: {},
    vaultModalOpen: false,
    vaultPickerSearch: '',
    newVaultModal: { show: false, name: '', error: '', loading: false, collision: null },
    isDarkMode: localStorage.getItem('muninnTheme') !== 'light',
    liveConnected: false,
    appVersion: '',

    // Dashboard
    stats: { engramCount: 0, vaultCount: 0, storageBytes: 0, indexSize: 0 },
    workerStats: [],
    liveFeed: [],
    _activityChart: null,
    _prevEngramCount: 0,
    _prevVaultCount: 0,

    // Memories
    memories: [],
    totalMemories: 0,
    searchQuery: '',
    searchMode: 'balanced',
    page: 0,
    memoriesLoading: false,
    memoryFilters: { sort: 'created', tags: '', state: '', minConf: 0, maxConf: 0 },
    selectedMemory: null,
    showNewMemoryModal: false,
    newMemoryForm: { concept: '', content: '', tagsRaw: '', confidence: 0.8 },
    confirmForgetId: null,

    // Edit/Evolve
    editingMemory: false,
    editMemoryForm: { content: '', reason: '' },
    editMemorySaving: false,

    // Tag editing
    editingTags: false,
    editTagsValue: '',
    editTagsSaving: false,

    // Link modal
    linkModal: { show: false, sourceId: '', targetId: '', relType: 5, weight: 0.8 },

    // Explain modal
    explainModal: { show: false, data: null, loading: false },

    // Multi-select / consolidate
    multiSelectMode: false,
    selectedMemoryIds: [],
    consolidateModal: { show: false, mergedContent: '' },

    // Decide modal
    decideModal: { show: false, decision: '', rationale: '', alternatives: '', evidenceIds: '' },

    // Graph
    graphLoaded: false,
    graphTab: 'memory',
    _cy: null,
    entityGraphLoaded: false,
    entityGraphStatus: '',
    _entityCy: null,

    // Session
    sessionRange: '24h',
    sessionEntries: [],

    // Cluster
    clusterEnabled: null,  // null=unknown, true=enabled, false=disabled
    clusterNodes: [],
    clusterHealth: null,
    clusterCCS: null,
    clusterEvents: [],
    _clusterNodesInterval: null,
    _clusterCCSInterval: null,

    // Enable cluster form
    clusterEnableForm: { role: 'primary', bindAddr: '', clusterSecret: '', cortexAddr: '' },
    clusterEnableLoading: false,
    clusterEnableError: null,
    clusterEnableProgress: [],
    showEnableClusterConfirm: false,

    // Sub-tab nav
    clusterSubTab: 'overview',

    // Security posture
    clusterSecurityPosture: null,

    // Management tab
    showAddNodeModal: false,
    addNodeForm: { addr: '', token: '' },
    addNodeTesting: false,
    addNodeTestResult: null,
    addNodeLoading: false,
    addNodeError: null,
    addNodeProgress: [],
    showRemoveNodeModal: false,
    removeNodeTarget: null,
    removeNodeDrain: true,
    removeNodeLoading: false,
    removeNodeError: null,
    showFailoverModal: false,
    failoverTarget: '',
    failoverLoading: false,
    failoverError: null,
    failoverProgress: [],
    clusterFeed: [],
    clusterFeedPaused: false,
    _clusterFeedSSE: null,

    // Settings tab
    clusterToken: null,
    clusterTokenLoading: false,
    clusterTokenCopied: false,
    showRegenerateTokenConfirm: false,
    clusterSettings: { heartbeat_ms: 1000, sdown_beats: 3, ccs_interval_seconds: 30, reconcile_on_heal: true },
    clusterSettingsSaving: false,
    clusterSettingsSaved: false,

    // Notifications
    notifications: [],
    _notifId: 0,

    // Auth
    isAuthenticated: false,
    showSignOutConfirm: false,
    loginForm: { username: '', password: '' },
    loginError: '',
    changePassForm: { username: 'root', newPassword: '', confirmPassword: '' },
    changePassError: '',
    changePassSuccess: false,

    // Observability
    obs: null,
    _obsInterval: null,

    // Contradictions
    contradictions: [],
    contradictionsLoaded: false,
    memoriesSubTab: 'list', // 'list' | 'contradictions'

    // Backup
    backupLoading: false,

    // Keyboard shortcuts help
    showShortcutsHelp: false,

    // Settings
    settingsTab: 'connect', // 'connect' | 'vault' | 'plugins' | 'keys' | 'admin'
    embedStatus: null,       // loaded from GET /api/admin/embed/status
    mcpInfo: null,           // loaded from GET /api/admin/mcp-info
    connectCopied: false,    // feedback for copy button
    connectPlatform: (() => {
        const p = (navigator.userAgent || '').toLowerCase();
        if (p.includes('win')) return 'windows';
        if (p.includes('linux')) return 'linux';
        return 'macos'; // default
    })(),
    apiKeys: [],
    apiKeyForm: { vault: '', label: '', mode: 'full' },
    apiKeyToken: null,
    apiKeyError: '',
    apiKeyLoading: false,
    plugins: [],
    cogWorkerStats: null,

    // Plasticity (vault cognitive pipeline config)
    plasticityForm: {
      preset: 'default',
      showAdvanced: false,
      hebbianEnabled: true,
      temporalEnabled: true,
      hopDepth: null,
      semanticWeight: null,
      ftsWeight: null,
      relevanceFloor: null,
      temporalHalflife: null,
      recallMode: 'balanced',
    },
    plasticitySaving: false,
    plasticitySaveOk: false,
    plasticitySaveErr: '',

    // Plugin configuration wizard state
    pluginCfg: {
      embedProvider: 'none',  // 'none' | 'ollama' | 'openai' | 'voyage'
      embedOllamaModel: 'nomic-embed-text',
      embedApiKey: '',
      embedUrl: '',           // custom base URL for openai-compatible endpoints
      embedShowForm: false,
      embedSaved: false,
      embedError: '',
      enrichProvider: 'none', // 'none' | 'ollama' | 'openai' | 'anthropic'
      enrichOllamaModel: 'llama3.2',
      enrichModel: 'claude-haiku-4-5-20251001',
      enrichApiKey: '',
      enrichShowForm: false,
      enrichSaved: false,
      enrichError: '',
      ollamaModels: [],
      ollamaEmbedModels: [],
      ollamaDetected: null,   // null=unchecked, true=running, false=not found
      ollamaChecking: false,
    },

    // Vault actions
    vaultActionModal: { show: false, action: '', vault: '', confirmText: '', memCount: 0 },
    cloneModal: { show: false, source: '', newName: '' },
    mergeModal: { show: false, source: '', target: '', deleteSource: false },
    importModal: { show: false, vaultName: '', file: null, resetMeta: false },
    activeJob: null,
    jobPollInterval: null,
    vaultExporting: false,
    reindexing: false,

    // Sidebar
    sidebarExpanded: localStorage.getItem('muninnSidebar') === 'expanded',

    // SSE
    _es: null,
    _esRetries: 0,

    // ── Lifecycle ──────────────────────────────────────────────────────────
    async init() {
      // Apply theme to html element
      if (!this.isDarkMode) {
        document.documentElement.classList.add('light');
      } else {
        document.documentElement.classList.remove('light');
      }

      // Hash-based routing
      const onHash = () => {
        const hash = location.hash.replace(/^#\/?/, '') || 'dashboard';
        const parts = hash.split('/');
        const raw = parts[0];
        // Only use known views
        const known = ['dashboard', 'memories', 'graph', 'session', 'settings', 'logs', 'cluster'];
        this.currentView = known.includes(raw) ? raw : 'dashboard';

        // Parse settings sub-tab if entering settings view
        if (raw === 'settings' && parts[1]) {
          const validTabs = ['connect', 'vault', 'plugins', 'keys', 'admin'];
          if (validTabs.includes(parts[1])) {
            this.settingsTab = parts[1];
          }
        }

        this._onViewEnter(this.currentView);
      };
      window.addEventListener('hashchange', onHash);
      onHash();

      // Keyboard shortcuts
      document.addEventListener('keydown', (e) => {
        // Ignore when typing in an input/textarea/select
        const tag = (e.target.tagName || '').toLowerCase();
        const inField = tag === 'input' || tag === 'textarea' || tag === 'select' || e.target.isContentEditable;

        if (e.key === 'Escape') {
          // Close any open modal/panel
          if (this.showNewMemoryModal)  { this.showNewMemoryModal = false; return; }
          if (this.explainModal.show)   { this.closeExplainModal(); return; }
          if (this.consolidateModal.show) { this.consolidateModal.show = false; return; }
          if (this.decideModal.show)    { this.decideModal.show = false; return; }
          if (this.selectedMemory)      { this.selectedMemory = null; return; }
          if (this.confirmForgetId)     { this.confirmForgetId = null; return; }
          if (this.showSignOutConfirm)  { this.showSignOutConfirm = false; return; }
          if (this.vaultActionModal.show) { this.vaultActionModal.show = false; return; }
          if (this.cloneModal.show)     { this.cloneModal.show = false; return; }
          if (this.mergeModal.show)     { this.mergeModal.show = false; return; }
          if (this.importModal.show)    { this.importModal.show = false; return; }
          if (this.showShortcutsHelp)   { this.showShortcutsHelp = false; return; }
        }

        if (inField) return;

        if (e.key === '/' && this.currentView === 'memories') {
          e.preventDefault();
          const input = document.getElementById('memory-search-input');
          if (input) input.focus();
        } else if (e.key === 'n' && this.currentView === 'memories') {
          e.preventDefault();
          this.showNewMemoryModal = true;
        } else if (e.key === '?') {
          e.preventDefault();
          this.showShortcutsHelp = !this.showShortcutsHelp;
        }
      });

      // Fetch version from public health endpoint
      try {
        const h = await fetch('/api/health').then(r => r.json());
        this.appVersion = h.version || '';
      } catch (_) {}

      // Load initial data (gated on auth check)
      await this.checkAuth();

      // Fetch vault engram counts whenever the vault picker modal opens.
      this.$watch('vaultModalOpen', (open) => {
        if (open) this.loadVaultStats();
      });
    },

    // ── Auth ───────────────────────────────────────────────────────────────
    async checkAuth() {
      try {
        await this.apiCall('/api/auth/check');
        this.isAuthenticated = true;
        this.loadVaults();
        this.loadWorkerStats();
        setInterval(() => this.loadWorkerStats(), 10000);
        this.connectLive();
      } catch (_) {
        this.isAuthenticated = false;
        history.replaceState(null, '', location.pathname);
      }
    },

    async login() {
      this.loginError = '';
      try {
        await this.apiCall('/api/auth/login', {
          method: 'POST',
          body: JSON.stringify(this.loginForm),
        });
        this.isAuthenticated = true;
        this.loginForm = { username: '', password: '' };
        this.loadVaults();
        this.connectLive();
        this.navigateTo('dashboard');
      } catch (err) {
        this.loginError = 'Invalid username or password';
      }
    },

    async logout() {
      await this.apiCall('/api/auth/logout', { method: 'POST' }).catch(() => {});
      this.isAuthenticated = false;
      history.replaceState(null, '', location.pathname);
    },

    async changePassword() {
      this.changePassError = '';
      this.changePassSuccess = false;
      if (this.changePassForm.newPassword !== this.changePassForm.confirmPassword) {
        this.changePassError = 'Passwords do not match.';
        return;
      }
      try {
        await this.apiCall('/api/admin/password', {
          method: 'PUT',
          body: JSON.stringify({
            username: this.changePassForm.username,
            new_password: this.changePassForm.newPassword,
          }),
        });
        this.changePassSuccess = true;
        this.changePassForm.newPassword = '';
        this.changePassForm.confirmPassword = '';
      } catch (err) {
        this.changePassError = 'Failed to update password. Check the username and try again.';
      }
    },

    _onViewEnter(view) {
      // Stop observability polling when leaving the tab
      if (this._obsInterval) {
        clearInterval(this._obsInterval);
        this._obsInterval = null;
      }

      if (view === 'dashboard') {
        this.loadStats();
        // Chart init happens after DOM renders
        this.$nextTick(() => this._initChart());
      } else if (view === 'memories') {
        this.page = 0;
        this.loadMemories();
        this.loadContradictions();
      } else if (view === 'session') {
        this.loadSession();
      } else if (view === 'observability') {
        this.loadObservability();
        this._obsInterval = setInterval(() => this.loadObservability(), 5000);
      } else if (view === 'settings') {
        // Check current hash to determine which sub-tab to activate
        const hash = location.hash.replace(/^#\/?/, '');
        const parts = hash.split('/');
        if (parts[0] === 'settings' && parts[1]) {
          const validTabs = ['connect', 'vault', 'plugins', 'keys', 'admin'];
          if (validTabs.includes(parts[1])) {
            this.settingsTab = parts[1];
          }
        }

        // Load data based on current sub-tab
        if (this.settingsTab === 'connect') {
          this.loadMCPInfo();
        } else if (this.settingsTab === 'vault') {
          this.loadEmbedStatus();
          this.loadWorkers();
          this.loadPlasticity();
        } else if (this.settingsTab === 'plugins') {
          this.loadPlugins();
          this.loadEmbedStatus();
          this.probeOllama();
        } else if (this.settingsTab === 'keys') {
          this.loadApiKeys();
          this.loadVaults();
        }
        // Admin tab doesn't need special loading

        // Always load these for settings
        this.loadVaults();
      } else if (view === 'graph') {
        // Clear graph state on vault change so stale nodes from the previous
        // vault are not shown. User must click Load Graph again for the new vault.
        if (this._cy) { this._cy.destroy(); this._cy = null; }
        if (this._entityCy) { this._entityCy.destroy(); this._entityCy = null; }
        this.graphLoaded = false;
        this.entityGraphLoaded = false;
      }
      if (view === 'cluster') {
        this.loadClusterDashboard();
      } else {
        this.stopClusterFeed();
      }
    },

    navigateTo(view) {
      location.hash = '/' + view;
    },

    toggleTheme() {
      this.isDarkMode = !this.isDarkMode;
      if (this.isDarkMode) {
        document.documentElement.classList.remove('light');
        localStorage.setItem('muninnTheme', 'dark');
      } else {
        document.documentElement.classList.add('light');
        localStorage.setItem('muninnTheme', 'light');
      }
    },

    onVaultChange() {
      localStorage.setItem('muninnVault', this.vault);
      this._onViewEnter(this.currentView);
    },

    pickVault(v) {
      this.vault = v;
      this.vaultModalOpen = false;
      this.vaultPickerSearch = '';
      this.onVaultChange();
    },

    toggleSidebar() {
      this.sidebarExpanded = !this.sidebarExpanded;
      localStorage.setItem('muninnSidebar', this.sidebarExpanded ? 'expanded' : 'collapsed');
    },

    // ── Server-Sent Events ─────────────────────────────────────────────────
    connectLive() {
      if (this._es) {
        this._es.close();
        this._es = null;
      }

      window._muninnSSE = new EventSource('/events');
      const es = window._muninnSSE;

      es.onopen = () => {
        this.liveConnected = true;
        this._esRetries = 0;
      };

      es.onerror = () => {
        this.liveConnected = false;
        // EventSource auto-reconnects, but we track state
        es.close();
        this._es = null;
        window._muninnSSE = null;
        const delay = Math.min(500 * Math.pow(1.5, this._esRetries), 30000);
        this._esRetries++;
        setTimeout(() => this.connectLive(), delay);
      };

      es.onmessage = (e) => {
        try {
          const msg = JSON.parse(e.data);
          this._handleLiveMessage(msg);
        } catch (_) {}
      };

      this._es = es;
    },

    _handleLiveMessage(msg) {
      if (msg.type === 'stats_update') {
        const newCount = msg.data.engramCount || 0;

        // Count-diff: if engrams increased, fetch newest as live feed entry
        if (this._prevEngramCount > 0 && newCount > this._prevEngramCount) {
          this._fetchNewestEngram();
        }

        // Vault count-diff: refresh vault list when a vault is added or removed.
        // Guard with > 0 on first message (learn current count without triggering a reload).
        const newVaultCount = msg.data.vaultCount || 0;
        if (this._prevVaultCount > 0 && newVaultCount !== this._prevVaultCount) {
          this.loadVaults();
        }
        this._prevVaultCount = newVaultCount;

        // Re-fetch stats scoped to the selected vault instead of using
        // the global broadcast values.
        this.loadStats();
      } else if (msg.type === 'memory_added') {
        this.liveFeed.unshift(msg.data);
        if (this.liveFeed.length > 20) this.liveFeed.pop();
      }
    },

    async _fetchNewestEngram() {
      try {
        const data = await this.apiCall(
          '/api/engrams?vault=' + encodeURIComponent(this.vault) + '&limit=1&offset=0'
        );
        const e = (data.engrams || [])[0];
        if (e) {
          this.liveFeed.unshift({
            id: e.id,
            concept: e.concept,
            vault: e.vault || this.vault,
            createdAt: e.created_at,
          });
          if (this.liveFeed.length > 20) this.liveFeed.pop();
        }
      } catch (_) {}
    },

    // ── API helpers ────────────────────────────────────────────────────────
    async apiCall(url, opts = {}) {
      const res = await fetch(url, {
        headers: { 'Content-Type': 'application/json', ...(opts.headers || {}) },
        ...opts,
      });
      if (!res.ok) {
        const text = await res.text().catch(() => res.statusText);
        throw new Error(res.status + ': ' + text);
      }
      return res.json();
    },

    // ── Dashboard ──────────────────────────────────────────────────────────
    async loadStats() {
      try {
        const data = await this.apiCall('/api/stats?vault=' + encodeURIComponent(this.vault));
        this.stats = {
          engramCount:  data.engram_count   || data.engramCount  || 0,
          vaultCount:   data.vault_count    || data.vaultCount   || 0,
          storageBytes: data.storage_bytes  || data.storageBytes || 0,
          indexSize:    data.index_size     || data.indexSize    || 0,
        };
        this._prevEngramCount = this.stats.engramCount;
      } catch (err) {
        this.addNotification('error', 'Stats: ' + err.message);
      }
    },

    async loadWorkerStats() {
      try {
        const data = await this.apiCall('/api/workers');
        this.workerStats = [
          { name: 'Temporal',    state: data.decay?.state      ?? 0 },
          { name: 'Hebbian',     state: data.hebbian?.state    ?? 0 },
          { name: 'Contradict',  state: data.contradict?.state ?? 0 },
          { name: 'Confidence',  state: data.confidence?.state ?? 0 },
        ];
      } catch (_) {}
    },

    workerStateName(state) {
      return ['Active', 'Idle', 'Dormant'][state] ?? 'Unknown';
    },

    workerStateBadge(state) {
      const classes = ['badge-active', 'badge-idle', 'badge-dormant'];
      return classes[state] ?? 'badge-idle';
    },

    async loadVaults() {
      try {
        const data = await this.apiCall('/api/vaults');
        this.vaults = Array.isArray(data) ? data.slice().sort((a, b) => a.localeCompare(b)) : ['default'];
        if (!this.vaults.includes(this.vault)) {
          this.vault = this.vaults[0] || 'default';
          localStorage.setItem('muninnVault', this.vault);
          // Vault changed — refresh current view so charts/lists use the new vault.
          this.$nextTick(() => this._onViewEnter(this.currentView));
        }
        // Keep API key form vault in sync with available vaults
        if (!this.apiKeyForm.vault || !this.vaults.includes(this.apiKeyForm.vault)) {
          this.apiKeyForm.vault = this.vault;
        }
      } catch (_) {
        this.vaults = ['default'];
      }
    },

    async loadVaultStats() {
      try {
        const data = await this.apiCall('/api/vaults/stats');
        if (Array.isArray(data)) {
          const map = {};
          data.forEach(s => { map[s.name] = s.engram_count; });
          this.vaultStats = map;
        }
      } catch (_) {
        // Non-critical — swallow silently
      }
    },

    _initChart() {
      const canvas = document.getElementById('activityChart');
      if (!canvas) return;
      if (this._activityChart) {
        this._activityChart.destroy();
        this._activityChart = null;
      }

      // Last 7 days labels
      const labels = [];
      const now = new Date();
      for (let i = 6; i >= 0; i--) {
        const d = new Date(now);
        d.setDate(d.getDate() - i);
        labels.push(d.toLocaleDateString('en-US', { month: 'short', day: 'numeric' }));
      }

      // Fetch session data for 7 days
      const since = new Date(Date.now() - 7 * 86400 * 1000).toISOString();
      this.apiCall(
        '/api/session?vault=' + encodeURIComponent(this.vault) +
        '&since=' + encodeURIComponent(since) + '&limit=500'
      ).then(resp => {
        const entries = resp.entries || (Array.isArray(resp) ? resp : []);
        const counts = new Array(7).fill(0);
        entries.forEach(e => {
          if (!e.created_at) return;
          const diffMs = now - new Date(e.created_at * 1000);
          const diffDays = Math.floor(diffMs / 86400000);
          const idx = 6 - diffDays;
          if (idx >= 0 && idx < 7) counts[idx]++;
        });

        this._activityChart = new Chart(canvas, {
          type: 'bar',
          data: {
            labels,
            datasets: [{
              label: 'Engrams written',
              data: counts,
              backgroundColor: 'rgba(6,182,212,0.5)',
              borderColor: '#06b6d4',
              borderWidth: 1,
              borderRadius: 4,
            }],
          },
          options: {
            responsive: true,
            plugins: { legend: { display: false } },
            scales: {
              x: {
                grid: { color: 'rgba(42,42,74,0.5)' },
                ticks: { color: '#94a3b8' },
              },
              y: {
                grid: { color: 'rgba(42,42,74,0.5)' },
                ticks: { color: '#94a3b8', stepSize: 1 },
                beginAtZero: true,
              },
            },
          },
        });
      }).catch(() => {});
    },

    formatBytes(bytes) {
      if (!bytes) return '0 B';
      const units = ['B', 'KB', 'MB', 'GB'];
      let i = 0, n = +bytes;
      while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
      return n.toFixed(1) + ' ' + units[i];
    },

    formatUptime(seconds) {
      if (!seconds) return '0s';
      const d = Math.floor(seconds / 86400);
      const h = Math.floor((seconds % 86400) / 3600);
      const m = Math.floor((seconds % 3600) / 60);
      if (d > 0) return d + 'd ' + h + 'h';
      if (h > 0) return h + 'h ' + m + 'm';
      return m + 'm';
    },

    // ── Memories ───────────────────────────────────────────────────────────
    async loadMemories() {
      this.memoriesLoading = true;
      try {
        const offset = this.page * 20;
        let url = '/api/engrams?vault=' + encodeURIComponent(this.vault) +
          '&limit=20&offset=' + offset;
        const f = this.memoryFilters;
        if (f.sort && f.sort !== 'created') url += '&sort=' + encodeURIComponent(f.sort);
        if (f.tags && f.tags.trim()) url += '&tags=' + encodeURIComponent(f.tags.trim());
        if (f.state && f.state.trim()) url += '&state=' + encodeURIComponent(f.state.trim());
        if (f.minConf > 0) url += '&min_confidence=' + f.minConf;
        if (f.maxConf > 0) url += '&max_confidence=' + f.maxConf;
        const data = await this.apiCall(url);
        this.memories = data.engrams || [];
        this.totalMemories = data.total || 0;
      } catch (err) {
        this.addNotification('error', 'Load failed: ' + err.message);
      } finally {
        this.memoriesLoading = false;
      }
    },

    async searchMemories() {
      if (!this.searchQuery.trim()) {
        this.page = 0;
        this.loadMemories();
        return;
      }
      this.memoriesLoading = true;
      try {
        // ActivateRequest uses context:[]string, max_results:int
        const body = {
            context: [this.searchQuery.trim()],
            vault: this.vault,
            max_results: 20,
        };
        if (this.searchMode && this.searchMode !== 'balanced') {
            body.mode = this.searchMode;
        }
        const data = await this.apiCall('/api/activate', {
          method: 'POST',
          body: JSON.stringify(body),
        });
        // ActivateResponse has activations: [{id, concept, content, confidence, score}]
        const items = data.activations || data.results || [];
        this.memories = items.map(a => ({
          id: a.id,
          concept: a.concept,
          content: a.content,
          confidence: a.confidence || a.score || 0,
          vault: this.vault,
          createdAt: a.created_at || 0,
        }));
        this.totalMemories = this.memories.length;
        this.page = 0;
      } catch (err) {
        this.addNotification('error', 'Search failed: ' + err.message);
      } finally {
        this.memoriesLoading = false;
      }
    },

    async loadContradictions() {
      try {
        const data = await this.apiCall('/api/contradictions?vault=' + encodeURIComponent(this.vault));
        this.contradictions = data.contradictions || [];
        this.contradictionsLoaded = true;
      } catch (_) {
        this.contradictions = [];
        this.contradictionsLoaded = true;
      }
    },

    async resolveContradiction(idA, idB, action) {
      const vault = this.vault;
      try {
        if (action === 'keep_a') {
          // A supersedes B; archive B
          await fetch('/api/link', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ source_id: idA, target_id: idB, rel_type: 4, weight: 1.0, vault }),
          });
          await fetch('/api/engrams/' + encodeURIComponent(idB) + '/state', {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ vault, state: 'archived' }),
          });
          await fetch('/api/admin/contradictions/resolve', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ vault, id_a: idA, id_b: idB }),
          });
        } else if (action === 'keep_b') {
          // B supersedes A; archive A
          await fetch('/api/link', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ source_id: idB, target_id: idA, rel_type: 4, weight: 1.0, vault }),
          });
          await fetch('/api/engrams/' + encodeURIComponent(idA) + '/state', {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ vault, state: 'archived' }),
          });
          await fetch('/api/admin/contradictions/resolve', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ vault, id_a: idA, id_b: idB }),
          });
        } else if (action === 'dismiss') {
          await fetch('/api/admin/contradictions/resolve', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ vault, id_a: idA, id_b: idB }),
          });
        } else if (action === 'merge') {
          // Open consolidate modal pre-filled with both IDs
          this.multiSelectMode = true;
          this.selectedMemoryIds = [idA, idB];
          this.consolidateModal = { show: true, mergedContent: '(Merge contradicting memories here)' };
          return; // don't reload contradictions yet
        }
        this.addNotification('success', 'Contradiction resolved');
      } catch (err) {
        this.addNotification('error', 'Resolve failed: ' + err.message);
      }
      this.loadContradictions();
    },

    async openMemory(m) {
      // Session entries only have id/concept/createdAt — fetch full engram if content is missing.
      if (!m.content && m.id) {
        try {
          const resp = await fetch('/api/engrams/' + encodeURIComponent(m.id) + '?vault=' + encodeURIComponent(this.vault));
          if (resp.ok) {
            const full = await resp.json();
            m = { ...m, ...full };
          }
        } catch (e) { /* fall through with partial data */ }
      }
      this.selectedMemory = m;
      // Navigate to memories view and update URL
      if (this.currentView !== 'memories') {
        this.navigateTo('memories');
      }
    },

    selectedMemoryIndex() {
      if (!this.selectedMemory || !this.memories.length) return -1;
      return this.memories.findIndex(m => m.id === this.selectedMemory.id);
    },

    navigateMemory(delta) {
      const idx = this.selectedMemoryIndex();
      if (idx === -1) return;
      const next = idx + delta;
      if (next < 0 || next >= this.memories.length) return;
      this.selectedMemory = this.memories[next];
    },

    forgetMemory(id) {
      this.confirmForgetId = id;
    },

    async doForget() {
      const id = this.confirmForgetId;
      this.confirmForgetId = null;
      try {
        await this.apiCall(
          '/api/engrams/' + encodeURIComponent(id) + '?vault=' + encodeURIComponent(this.vault),
          { method: 'DELETE' }
        );
        this.addNotification('success', 'Memory forgotten');
        if (this.selectedMemory && this.selectedMemory.id === id) {
          this.selectedMemory = null;
        }
        await this.loadMemories();
      } catch (err) {
        this.addNotification('error', 'Forget failed: ' + err.message);
      }
    },

    async createMemory(form) {
      const tags = form.tagsRaw
        ? form.tagsRaw.split(',').map(t => t.trim()).filter(Boolean)
        : [];
      try {
        // POST /api/engrams → WriteRequest: { concept, content, tags, vault, confidence }
        await this.apiCall('/api/engrams', {
          method: 'POST',
          body: JSON.stringify({
            concept: form.concept,
            content: form.content,
            tags,
            vault: this.vault,
            confidence: parseFloat(form.confidence) || 0.8,
          }),
        });
        this.showNewMemoryModal = false;
        this.newMemoryForm = { concept: '', content: '', tagsRaw: '', confidence: 0.8 };
        this.addNotification('success', 'Memory created');
        await this.loadMemories();
      } catch (err) {
        this.addNotification('error', 'Create failed: ' + err.message);
      }
    },

    // ── Edit / Evolve ─────────────────────────────────────────────────────
    startEditMemory() {
      this.editingMemory = true;
      this.editMemoryForm.content = this.selectedMemory ? this.selectedMemory.content : '';
      this.editMemoryForm.reason = '';
    },

    cancelEditMemory() {
      this.editingMemory = false;
      this.editMemoryForm = { content: '', reason: '' };
    },

    async saveEditMemory() {
      if (!this.selectedMemory) return;
      if (!this.editMemoryForm.content.trim()) {
        this.addNotification('error', 'Content cannot be empty');
        return;
      }
      if (!this.editMemoryForm.reason.trim()) {
        this.addNotification('error', 'Reason is required');
        return;
      }
      this.editMemorySaving = true;
      try {
        const resp = await this.apiCall(
          '/api/engrams/' + encodeURIComponent(this.selectedMemory.id) + '/evolve?vault=' + encodeURIComponent(this.vault),
          {
            method: 'POST',
            body: JSON.stringify({
              new_content: this.editMemoryForm.content,
              reason: this.editMemoryForm.reason,
            }),
          }
        );
        this.selectedMemory = { ...this.selectedMemory, content: this.editMemoryForm.content };
        this.editingMemory = false;
        this.editMemoryForm = { content: '', reason: '' };
        this.addNotification('success', 'Memory updated');
        // Refresh the list so the new content shows there too
        await this.loadMemories();
      } catch (err) {
        this.addNotification('error', 'Evolve failed: ' + err.message);
      } finally {
        this.editMemorySaving = false;
      }
    },

    // ── Tag editing ────────────────────────────────────────────────────────
    startEditTags() {
      if (!this.selectedMemory) return;
      this.editTagsValue = (this.selectedMemory.tags || []).join(', ');
      this.editingTags = true;
    },

    cancelEditTags() {
      this.editingTags = false;
      this.editTagsValue = '';
    },

    async saveTags() {
      if (!this.selectedMemory) return;
      const tags = this.editTagsValue
        .split(',')
        .map(t => t.trim())
        .filter(Boolean);
      this.editTagsSaving = true;
      try {
        const resp = await this.apiCall(
          '/api/engrams/' + encodeURIComponent(this.selectedMemory.id) + '/tags',
          {
            method: 'PUT',
            body: JSON.stringify({ vault: this.vault, tags }),
          }
        );
        this.selectedMemory = { ...this.selectedMemory, tags: resp.tags };
        // Refresh list so tag chips update there too.
        const idx = this.memories.findIndex(m => m.id === this.selectedMemory.id);
        if (idx !== -1) {
          this.memories[idx] = { ...this.memories[idx], tags: resp.tags };
        }
        this.editingTags = false;
        this.editTagsValue = '';
        this.addNotification('success', 'Tags updated');
      } catch (err) {
        this.addNotification('error', 'Tag update failed: ' + err.message);
      } finally {
        this.editTagsSaving = false;
      }
    },

    // ── Link creation ──────────────────────────────────────────────────────
    openLinkModal(sourceId) {
      this.linkModal = { show: true, sourceId: sourceId, targetId: '', relType: 5, weight: 0.8 };
    },

    closeLinkModal() {
      this.linkModal = { show: false, sourceId: '', targetId: '', relType: 5, weight: 0.8 };
    },

    async createLink() {
      if (!this.linkModal.targetId.trim()) {
        this.addNotification('error', 'Target ID is required');
        return;
      }
      try {
        await this.apiCall('/api/link', {
          method: 'POST',
          body: JSON.stringify({
            source_id: this.linkModal.sourceId,
            target_id: this.linkModal.targetId.trim(),
            rel_type: parseInt(this.linkModal.relType, 10),
            weight: parseFloat(this.linkModal.weight),
            vault: this.vault,
          }),
        });
        this.closeLinkModal();
        this.addNotification('success', 'Association created');
      } catch (err) {
        this.addNotification('error', 'Link failed: ' + err.message);
      }
    },

    // ── Create vault ───────────────────────────────────────────────────────
    createVault() {
      this.newVaultModal = { show: true, name: '', error: '', loading: false, collision: null };
    },

    async submitNewVault(force) {
      const name = this.newVaultModal.name.trim();
      if (!name) return;
      const valid = /^[a-z0-9_-]{1,64}$/.test(name);
      if (!valid) {
        this.newVaultModal.error = 'Lowercase letters, digits, hyphens, underscores only (1-64 chars)';
        return;
      }
      this.newVaultModal.loading = true;
      this.newVaultModal.error = '';
      this.newVaultModal.collision = null;
      try {
        const url = force ? '/api/admin/vaults/config?force=true' : '/api/admin/vaults/config';
        const r = await fetch(url, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name }),
        });
        if (r.status === 409) {
          const data = await r.json().catch(() => null);
          if (data && data.code === 'VAULT_NAME_COLLISION') {
            this.newVaultModal.collision = data;
            this.newVaultModal.loading = false;
            return;
          }
          const text = await r.text().catch(() => r.statusText);
          throw new Error(r.status + ': ' + text);
        }
        if (!r.ok) {
          const text = await r.text().catch(() => r.statusText);
          throw new Error(r.status + ': ' + text);
        }
        await fetch('/api/hello', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ version: '1', vault: name }),
        }).catch(() => {});
        this.vault = name;
        localStorage.setItem('muninnVault', name);
        await this.loadVaults();
        this.newVaultModal.loading = false;
        this.newVaultModal.show = false;
        this.addNotification('success', 'Vault "' + name + '" created');
      } catch (err) {
        this.newVaultModal.error = err.message;
        this.newVaultModal.loading = false;
      }
    },

    // ── Graph ──────────────────────────────────────────────────────────────
    graphShowOrphans: false,

    async loadGraph() {
      this.addNotification('info', 'Loading graph…');
      try {
        // Use GET /api/engrams for node listing
        const data = await this.apiCall(
          '/api/engrams?vault=' + encodeURIComponent(this.vault) + '&limit=50&offset=0'
        );
        const engrams = data.engrams || [];
        if (!engrams.length) {
          this.addNotification('error', 'No engrams to graph');
          return;
        }

        // Load all engram links in a single batch call (replaces N+1 pattern).
        const nodeIdSet = new Set(engrams.map(e => e.id));
        const batchResp = await this.apiCall('/api/engrams/links/batch', {
          method: 'POST',
          body: JSON.stringify({
            ids: engrams.map(e => e.id),
            vault: this.vault,
          }),
        });
        const linksMap = batchResp.links || {};
        const edgeSet = new Set();
        const edges = [];
        const connectedNodeIds = new Set();
        for (const [srcId, srcLinks] of Object.entries(linksMap)) {
          for (const l of (srcLinks || [])) {
            const edgeId = srcId + '-' + l.target_id;
            if (nodeIdSet.has(l.target_id) && !edgeSet.has(edgeId)) {
              edgeSet.add(edgeId);
              edges.push({
                data: {
                  id: edgeId,
                  source: srcId,
                  target: l.target_id,
                  weight: l.weight || 0.5,
                },
              });
              connectedNodeIds.add(srcId);
              connectedNodeIds.add(l.target_id);
            }
          }
        }

        // Filter orphan nodes (nodes with 0 edges) unless showOrphans is toggled on
        const showOrphans = this.graphShowOrphans;
        const filteredEngrams = showOrphans
          ? engrams
          : engrams.filter(e => connectedNodeIds.has(e.id));

        // If no connected nodes, fall back to all engrams so the graph isn't empty
        const nodesToRender = filteredEngrams.length > 0 ? filteredEngrams : engrams;

        // Build node elements
        const nodeElements = nodesToRender.map(e => ({
          data: {
            id: e.id,
            label: e.concept || e.id.slice(0, 8),
            size: connectedNodeIds.has(e.id) ? 20 + (e.confidence || 0.5) * 20 : 12,
            color: !connectedNodeIds.has(e.id) ? '#64748b'
                 : (e.confidence || 0) > 0.7 ? '#06b6d4'
                 : (e.confidence || 0) > 0.4 ? '#a855f7' : '#eab308',
            orphan: !connectedNodeIds.has(e.id),
            snippet: (e.content || '').slice(0, 80),
          },
        }));

        const elements = [...nodeElements, ...edges];

        // Init or reinit Cytoscape
        if (this._cy) { this._cy.destroy(); this._cy = null; }
        this._cy = cytoscape({
          container: document.getElementById('cy'),
          elements,
          style: [
            {
              selector: 'node',
              style: {
                'background-color': 'data(color)',
                'width': 'data(size)',
                'height': 'data(size)',
                'label': 'data(label)',
                'color': '#e2e8f0',
                'font-size': '11px',
                'text-valign': 'bottom',
                'text-margin-y': '6px',
                'text-wrap': 'wrap',
                'text-max-width': '100px',
                'border-width': 2,
                'border-color': 'rgba(255,255,255,0.1)',
              },
            },
            {
              selector: 'node[?orphan]',
              style: {
                'opacity': 0.45,
                'border-style': 'dashed',
              },
            },
            {
              selector: 'edge',
              style: {
                'line-color': 'rgba(168,85,247,0.4)',
                'width': 2,
                'curve-style': 'bezier',
                'opacity': 0,
              },
            },
            {
              selector: 'node:selected',
              style: { 'border-width': 3, 'border-color': '#06b6d4' },
            },
          ],
          layout: { name: 'fcose', animate: true, animationDuration: 600 },
          wheelSensitivity: 0.3,
        });

        // Fade edges in after nodes settle into position (fcose layout: 600ms).
        // cy.one() fires once and removes itself — does not re-trigger on layout re-runs.
        this._cy.one('layoutstop', () => {
          this._cy.edges().animate({
            style: { opacity: 0.6 },
            duration: 250,
            easing: 'ease-in-out',
          });
        });

        this._cy.on('tap', 'node', (evt) => {
          const node = evt.target;
          this.addNotification(
            'info',
            node.data('label') + ': ' + (node.data('snippet') || '(no content)')
          );
        });

        this.graphLoaded = true;
        const orphanCount = engrams.length - connectedNodeIds.size;
        const msg = 'Graph loaded (' + nodesToRender.length + ' nodes' +
          (orphanCount > 0 && !showOrphans ? ', ' + orphanCount + ' orphans hidden' : '') + ')';
        this.addNotification('success', msg);
      } catch (err) {
        this.addNotification('error', 'Graph failed: ' + err.message);
      }
    },

    graphZoomIn() {
      if (this._cy) { this._cy.zoom(this._cy.zoom() * 1.25); this._cy.center(); }
    },
    graphZoomOut() {
      if (this._cy) { this._cy.zoom(this._cy.zoom() * 0.8); this._cy.center(); }
    },
    graphFit() {
      if (this._cy) { this._cy.fit(); }
    },

    // ── Entity Graph ───────────────────────────────────────────────────────
    async loadEntityGraph() {
      this.entityGraphStatus = 'Loading entity graph…';
      try {
        // Call the REST endpoint directly instead of the MCP server.
        // The previous approach fetched from http://127.0.0.1:8750/mcp which
        // is unreachable from remote browsers (127.0.0.1 resolves to the
        // browser's own loopback, not the server's).
        const data = await this.apiCall(
          '/api/admin/entity-graph?vault=' + encodeURIComponent(this.vault) + '&include_engrams=true'
        );

        const nodes = [];
        const edges = [];
        const nodeIdSet = new Set();

        (data.nodes || []).forEach(n => {
          const entityType = (n.type || 'other').toLowerCase();
          nodeIdSet.add(n.id);
          nodes.push({
            id: n.id,
            label: n.id,
            type: entityType,
            title: n.id + ' (' + entityType + ')',
            shape: 'dot',
            size: 16,
            color: this.getEntityTypeColor(entityType),
            font: { size: 11, color: '#e2e8f0' },
            borderWidth: 2,
            borderWidthSelected: 3,
            borderColor: 'rgba(255,255,255,0.2)'
          });
        });

        (data.edges || []).forEach(e => {
          if (nodeIdSet.has(e.from) && nodeIdSet.has(e.to)) {
            edges.push({
              from: e.from,
              to: e.to,
              label: e.rel_type,
              arrows: 'to',
              color: 'rgba(168,85,247,0.4)',
              font: { size: 10, color: '#ccc' },
              width: Math.max(1, (e.weight || 0.5) * 3),
              smooth: { type: 'continuous' }
            });
          }
        });

        if (nodes.length === 0) {
          this.entityGraphStatus = 'No entities found in vault';
          return;
        }

        // Reinit or destroy existing graph
        if (this._entityCy) { this._entityCy.destroy(); this._entityCy = null; }

        // Create vis.Network-style data structure for Cytoscape
        const elements = nodes.concat(edges);

        this._entityCy = cytoscape({
          container: document.getElementById('entity-cy'),
          elements: elements,
          style: [
            {
              selector: 'node',
              style: {
                'background-color': 'data(color)',
                'width': 'data(size)',
                'height': 'data(size)',
                'label': 'data(label)',
                'color': 'data(font.color)',
                'font-size': 'data(font.size)',
                'text-valign': 'center',
                'text-halign': 'center',
                'border-width': 'data(borderWidth)',
                'border-color': 'data(borderColor)',
                'text-background': true,
                'text-background-color': 'rgba(0,0,0,0.5)',
                'text-background-padding': '2px',
                'text-background-shape': 'roundrectangle',
                'text-wrap': 'wrap',
                'text-max-width': '80px'
              }
            },
            {
              selector: 'node:selected',
              style: {
                'border-width': 'data(borderWidthSelected)',
                'border-color': '#06b6d4'
              }
            },
            {
              selector: 'edge',
              style: {
                'line-color': 'data(color)',
                'width': 'data(width)',
                'curve-style': 'bezier',
                'opacity': 0.7,
                'label': 'data(label)',
                'color': 'data(font.color)',
                'font-size': 'data(font.size)',
                'text-background': true,
                'text-background-color': 'rgba(0,0,0,0.5)',
                'text-background-padding': '2px',
                'text-background-shape': 'roundrectangle'
              }
            }
          ],
          layout: {
            name: 'fcose',
            animate: true,
            animationDuration: 600,
            animationEasing: 'ease-out'
          },
          wheelSensitivity: 0.3
        });

        // Add click handler to show entity info
        this._entityCy.on('tap', 'node', (evt) => {
          const node = evt.target;
          this.addNotification('info', node.data('label') + ' (' + node.data('type') + ')');
        });

        this.entityGraphLoaded = true;
        this.entityGraphStatus = 'Loaded ' + nodes.length + ' entities, ' + edges.length + ' relationships';
        this.addNotification('success', this.entityGraphStatus);
      } catch (err) {
        this.entityGraphStatus = 'Error: ' + err.message;
        this.addNotification('error', 'Entity graph failed: ' + err.message);
      }
    },

    getEntityTypeColor(entityType) {
      const colors = {
        'person': '#3b82f6',           // blue
        'organization': '#8b5cf6',     // purple
        'technology': '#10b981',       // emerald
        'project': '#f59e0b',          // amber
        'location': '#ec4899',         // pink
        'concept': '#6366f1',          // indigo
        'tool': '#14b8a6',             // teal
        'database': '#8b5cf6',         // purple
        'service': '#06b6d4',          // cyan
        'framework': '#10b981',        // emerald
        'language': '#f59e0b',         // amber
        'product': '#ef4444',          // red
        'event': '#84cc16',            // lime
        'other': '#64748b'             // slate
      };
      return colors[entityType] || colors['other'];
    },

    entityGraphZoomIn() {
      if (this._entityCy) { this._entityCy.zoom(this._entityCy.zoom() * 1.25); this._entityCy.center(); }
    },

    entityGraphZoomOut() {
      if (this._entityCy) { this._entityCy.zoom(this._entityCy.zoom() * 0.8); this._entityCy.center(); }
    },

    entityGraphFit() {
      if (this._entityCy) { this._entityCy.fit(); }
    },

    // ── Session ────────────────────────────────────────────────────────────
    async loadSession() {
      const ranges = { '24h': 24, '7d': 168, '30d': 720 };
      const hours = ranges[this.sessionRange] || 24;
      const since = new Date(Date.now() - hours * 3600 * 1000).toISOString();
      try {
        const data = await this.apiCall(
          '/api/session?vault=' + encodeURIComponent(this.vault) +
          '&since=' + encodeURIComponent(since) + '&limit=100'
        );
        // GetSessionResponse has { entries: [] } or raw array
        this.sessionEntries = data.entries || (Array.isArray(data) ? data : []);
      } catch (err) {
        this.addNotification('error', 'Session: ' + err.message);
      }
    },

    // ── Backup ─────────────────────────────────────────────────────────────
    async triggerBackup() {
      this.backupLoading = true;
      try {
        const ts = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);
        const outputDir = './backups/muninn-backup-' + ts;
        const data = await this.apiCall('/api/admin/backup', {
          method: 'POST',
          body: JSON.stringify({ output_dir: outputDir }),
        });
        this.addNotification('success', 'Backup complete: ' + data.output_dir + ' (' + data.elapsed + ')');
      } catch (err) {
        this.addNotification('error', 'Backup failed: ' + err.message);
      } finally {
        this.backupLoading = false;
      }
    },

    // ── Observability ─────────────────────────────────────────────────────
    async loadObservability() {
      try {
        this.obs = await this.apiCall('/api/admin/observability');
      } catch (e) {
        console.error('Failed to load observability:', e);
      }
    },

    // ── Settings ───────────────────────────────────────────────────────────
    async loadEmbedStatus() {
      try {
        this.embedStatus = await this.apiCall('/api/admin/embed/status');
        // Reflect the active provider in the plugin config UI (local is default, not a plugin choice)
        const p = this.embedStatus?.provider;
        if (p && p !== 'none' && p !== 'local') {
          this.pluginCfg.embedProvider = p;
        }
      } catch (_) {
        // Non-fatal: embedStatus stays null, UI shows fallback
        this.embedStatus = null;
      }
    },

    async loadMCPInfo() {
      try {
        this.mcpInfo = await this.apiCall('/api/admin/mcp-info');
      } catch (_) {
        // Fallback to defaults if endpoint not available
        this.mcpInfo = { url: 'http://localhost:8750/mcp', token_configured: false };
      }
    },

    async loadApiKeys() {
        try {
            const data = await this.apiCall('/api/admin/keys?vault=' + encodeURIComponent(this.vault));
            this.apiKeys = Array.isArray(data?.keys) ? data.keys : [];
        } catch (e) {
            this.apiKeys = [];
        }
    },
    async createApiKey() {
        this.apiKeyError = '';
        if (!this.apiKeyForm.vault || !this.apiKeyForm.label) {
            this.apiKeyError = 'Vault and label are required.';
            return;
        }
        this.apiKeyLoading = true;
        try {
            const data = await this.apiCall('/api/admin/keys', {
                method: 'POST',
                body: JSON.stringify(this.apiKeyForm),
            });
            this.apiKeyToken = data?.token || null;
            this.apiKeyForm = { vault: this.vault, label: '', mode: 'full' };
            await this.loadApiKeys();
        } catch (e) {
            this.apiKeyError = e.message || 'Failed to create key.';
        } finally {
            this.apiKeyLoading = false;
        }
    },
    async revokeApiKey(id) {
        if (!confirm('Revoke this API key? This cannot be undone.')) return;
        try {
            await this.apiCall('/api/admin/keys/' + id + '?vault=' + encodeURIComponent(this.vault), { method: 'DELETE' });
            await this.loadApiKeys();
        } catch (e) {
            this.addNotification('error', 'Failed to revoke key: ' + (e.message || 'unknown error'));
        }
    },
    async loadPlugins() {
        try {
            const data = await this.apiCall('/api/admin/plugins');
            this.plugins = Array.isArray(data) ? data : [];
        } catch (e) {
            this.plugins = [];
        }
    },
    async loadSavedPluginConfig() {
        try {
            const data = await this.apiCall('/api/admin/plugin-config');
            const embedUrl = data.embed_url || '';
            // Only populate the Base URL field when it's an HTTP/HTTPS URL.
            // ollama:// and other scheme URLs encode a model name, not a base URL.
            if (embedUrl.startsWith('http://') || embedUrl.startsWith('https://')) {
                this.pluginCfg.embedUrl = embedUrl;
            }
        } catch (_) {
            // Non-critical — leave embedUrl at default
        }
    },
    async loadWorkers() {
        try {
            this.cogWorkerStats = await this.apiCall('/api/workers');
        } catch (e) {
            this.cogWorkerStats = null;
        }
    },
    workerRows() {
        const ws = this.cogWorkerStats || {};
        const toRow = (name, stats) => ({
            name,
            active: stats && (stats.processed > 0 || stats.running),
            processed: stats ? (stats.processed ?? '—') : '—',
        });
        return [
            toRow('Hebbian Learning', ws.hebbian),
            toRow('Temporal Scoring', ws.decay),
            toRow('Contradiction Detection', ws.contradict),
            toRow('Confidence Updates', ws.confidence),
        ];
    },

    async loadPlasticity() {
        if (!this.isAuthenticated) return;
        try {
            this.plasticitySaveErr = '';
            const data = await this.apiCall(
                '/api/admin/vault/' + encodeURIComponent(this.vault) + '/plasticity'
            );
            const cfg = data.config || {};
            this.plasticityForm.preset         = cfg.preset || 'default';
            this.plasticityForm.hebbianEnabled = data.resolved?.hebbian_enabled ?? true;
            this.plasticityForm.temporalEnabled   = data.resolved?.temporal_enabled   ?? true;
            this.plasticityForm.hopDepth       = cfg.hop_depth       ?? null;
            this.plasticityForm.semanticWeight = cfg.semantic_weight ?? null;
            this.plasticityForm.ftsWeight      = cfg.fts_weight      ?? null;
            this.plasticityForm.relevanceFloor     = cfg.relevance_floor     ?? null;
            this.plasticityForm.temporalHalflife = cfg.temporal_halflife ?? null;
            this.plasticityForm.recallMode = cfg.recall_mode || data.resolved?.recall_mode || 'balanced';
        } catch (err) {
            console.error('loadPlasticity error:', err);
            this.plasticitySaveErr = 'Failed to load Plasticity settings';
        }
    },
    onPlasticityPresetChange() {
        this.plasticityForm.hopDepth       = null;
        this.plasticityForm.semanticWeight = null;
        this.plasticityForm.ftsWeight      = null;
        this.plasticityForm.relevanceFloor     = null;
        this.plasticityForm.temporalHalflife = null;
        this.plasticityForm.hebbianEnabled = true;
        this.plasticityForm.temporalEnabled   = true;
        this._updatePlasticityChart();
    },
    _plasticityData: {
        'default':         [0.30, 0.40, 0.50, 0.70, 0.60],
        'reference':       [1.00, 0.35, 0.375, 0.80, 0.70],
        'scratchpad':      [0.05, 0.00, 0.00, 0.70, 0.60],
        'knowledge-graph': [0.70, 1.00, 1.00, 0.75, 0.50],
    },
    _plasticityColors: {
        'default':         { border: '#6366f1', bg: 'rgba(99,102,241,0.35)' },
        'reference':       { border: '#10b981', bg: 'rgba(16,185,129,0.35)' },
        'scratchpad':      { border: '#f59e0b', bg: 'rgba(245,158,11,0.35)' },
        'knowledge-graph': { border: '#ec4899', bg: 'rgba(236,72,153,0.35)' },
    },
    initPlasticityChart() {
        const canvas = document.getElementById('plasticityChart');
        if (!canvas) return;
        const existing = Chart.getChart(canvas);
        if (existing) existing.destroy();
        const p = this.plasticityForm.preset;
        const c = this._plasticityColors[p];
        new Chart(canvas, {
            type: 'radar',
            data: {
                labels: ['Memory Lifespan', 'Associative Learning', 'Graph Depth', 'Semantic Match', 'FTS Relevance'],
                datasets: [{
                    data: this._plasticityData[p],
                    borderColor: c.border,
                    backgroundColor: c.bg,
                    borderWidth: 2.5,
                    pointRadius: 5,
                    pointBackgroundColor: c.border,
                }],
            },
            options: {
                responsive: true,
                maintainAspectRatio: true,
                animation: { duration: 300 },
                scales: { r: {
                    min: 0, max: 1,
                    ticks: { display: false },
                    grid: { color: this.isDarkMode ? 'rgba(255,255,255,0.08)' : 'rgba(0,0,0,0.08)' },
                    angleLines: { color: this.isDarkMode ? 'rgba(255,255,255,0.08)' : 'rgba(0,0,0,0.08)' },
                    pointLabels: { color: this.isDarkMode ? '#9ca3af' : '#6b7280', font: { size: 11 } },
                }},
                plugins: { legend: { display: false } },
            },
        });
    },
    _updatePlasticityChart() {
        const canvas = document.getElementById('plasticityChart');
        if (!canvas) return;
        const chart = Chart.getChart(canvas);
        if (!chart) return;
        const ds = chart.data.datasets[0];

        if (this.plasticityForm.showAdvanced) {
            // Compute chart shape from override values, falling back to base preset
            const p    = this.plasticityForm.preset || 'default';
            const base = this._plasticityData[p] || this._plasticityData['default'];
            const f    = this.plasticityForm;
            // dimensions: [Memory Lifespan, Associative Learning, Graph Depth, Semantic Match, FTS Relevance]
            const lifespan = f.relevanceFloor     != null ? Math.min(1, Math.max(0, f.relevanceFloor))     : base[0];
            const assoc    = f.hebbianEnabled
                ? (f.temporalHalflife != null ? Math.min(1, f.temporalHalflife / 60) : base[1])
                : 0;
            const depth    = f.hopDepth       != null ? Math.min(1, f.hopDepth / 8)                : base[2];
            const semW     = f.semanticWeight != null ? Math.min(1, Math.max(0, f.semanticWeight)) : base[3];
            const ftsW     = f.ftsWeight      != null ? Math.min(1, Math.max(0, f.ftsWeight))      : base[4];
            ds.data             = [lifespan, assoc, depth, semW, ftsW];
            ds.borderColor      = '#94a3b8';
            ds.backgroundColor  = 'rgba(148,163,184,0.35)';
            ds.pointBackgroundColor = '#94a3b8';
        } else {
            const p = this.plasticityForm.preset;
            const c = this._plasticityColors[p];
            ds.data             = this._plasticityData[p];
            ds.borderColor      = c.border;
            ds.backgroundColor  = c.bg;
            ds.pointBackgroundColor = c.border;
        }
        chart.update();
    },
    plasticityPresetDescription(preset) {
        const d = {
            'default':         'General-purpose. Temporal scoring on, Hebbian on, 2-hop BFS. Balanced weights.',
            'reference':       'Documentation and facts. Temporal scoring OFF — memories persist indefinitely.',
            'scratchpad':      'Ephemeral drafts. Aggressive fading (7-day halflife, 0.01 floor). No Hebbian, no hops.',
            'knowledge-graph': 'Dense interlinked concepts. 4-hop BFS, slow fading (60-day halflife).',
        };
        return d[preset] || '';
    },
    async savePlasticity() {
        this.plasticitySaving = true;
        this.plasticitySaveOk = false;
        this.plasticitySaveErr = '';
        try {
            const payload = { version: 1, preset: this.plasticityForm.preset };
            payload.recall_mode = this.plasticityForm.recallMode;
            if (this.plasticityForm.showAdvanced) {
                if (this.plasticityForm.hopDepth       !== null) payload.hop_depth       = this.plasticityForm.hopDepth;
                if (this.plasticityForm.semanticWeight !== null) payload.semantic_weight = this.plasticityForm.semanticWeight;
                if (this.plasticityForm.ftsWeight      !== null) payload.fts_weight      = this.plasticityForm.ftsWeight;
                if (this.plasticityForm.relevanceFloor     !== null) payload.relevance_floor     = this.plasticityForm.relevanceFloor;
                if (this.plasticityForm.temporalHalflife !== null) payload.temporal_halflife = this.plasticityForm.temporalHalflife;
                payload.hebbian_enabled = this.plasticityForm.hebbianEnabled;
                payload.temporal_enabled   = this.plasticityForm.temporalEnabled;
            }
            await this.apiCall(
                '/api/admin/vault/' + encodeURIComponent(this.vault) + '/plasticity',
                { method: 'PUT', body: JSON.stringify(payload) }
            );
            await this.loadPlasticity();
            this.plasticitySaveOk = true;
            setTimeout(() => { this.plasticitySaveOk = false; }, 3000);
        } catch (err) {
            this.plasticitySaveErr = err.message;
            setTimeout(() => { this.plasticitySaveErr = ''; }, 5000);
        } finally {
            this.plasticitySaving = false;
        }
    },

    async copyToClipboard(text) {
      try {
        await navigator.clipboard.writeText(text);
        this.connectCopied = true;
        setTimeout(() => { this.connectCopied = false; }, 2000);
        this.addNotification('success', 'Copied to clipboard');
      } catch (_) {
        this.addNotification('error', 'Copy failed — select and copy manually');
      }
    },

    // ── API key expiry display ──────────────────────────────────────────────
    formatKeyExpiry(expiresAt) {
      if (!expiresAt) return 'Never';
      const exp = new Date(expiresAt);
      const now = new Date();
      const diffMs = exp - now;
      if (diffMs <= 0) return 'Expired';
      const diffDays = Math.round(diffMs / 86400000);
      if (diffDays === 0) return 'Today';
      if (diffDays === 1) return 'Tomorrow';
      if (diffDays < 30) return 'in ' + diffDays + ' days';
      if (diffDays < 365) return 'in ' + Math.round(diffDays / 30) + ' months';
      return exp.toLocaleDateString();
    },

    // ── Confidence helpers ─────────────────────────────────────────────────
    // Thresholds are defined once here and used in templates.
    confLabel(v) {
      const CONFIDENCE_HIGH = 0.7;
      const CONFIDENCE_MED  = 0.4;
      if (v >= CONFIDENCE_HIGH) return 'High';
      if (v >= CONFIDENCE_MED)  return 'Med';
      return 'Low';
    },

    confLabelClass(v) {
      const CONFIDENCE_HIGH = 0.7;
      const CONFIDENCE_MED  = 0.4;
      if (v >= CONFIDENCE_HIGH) return 'badge-active';
      if (v >= CONFIDENCE_MED)  return 'badge-warning';
      return 'badge-dormant';
    },

    // Returns the progress percentage (0-100) for the embed progress bar.
    embedProgressPct() {
      if (!this.embedStatus) return 0;
      const total = this.embedStatus.total_count;
      const embedded = this.embedStatus.embedded_count;
      if (total <= 0 || embedded < 0) return 0;
      return Math.min(100, Math.round((embedded / total) * 100));
    },

    // ── Cluster ────────────────────────────────────────────────────────────
    async loadClusterDashboard() {
      // Clear any existing intervals and SSE feed before setting up new ones
      if (this._clusterNodesInterval) {
        clearInterval(this._clusterNodesInterval);
        this._clusterNodesInterval = null;
      }
      if (this._clusterCCSInterval) {
        clearInterval(this._clusterCCSInterval);
        this._clusterCCSInterval = null;
      }
      this.stopClusterFeed();

      await this._loadClusterInfo();

      if (this.clusterEnabled) {
        this._clusterNodesInterval = setInterval(() => this._loadClusterNodes(), 5000);
        this._clusterCCSInterval = setInterval(() => this._loadClusterCCS(), 30000);
      }
    },

    async _loadClusterInfo() {
      try {
        const info = await this.apiCall('/v1/cluster/info');
        // If cluster is disabled, info has { enabled: false }
        if (info.enabled === false) {
          this.clusterEnabled = false;
          return;
        }
        this.clusterEnabled = true;
        try {
          const secResp = await fetch('/api/admin/cluster/token', { credentials: 'same-origin' });
          if (secResp.ok) this.clusterSecurityPosture = await secResp.json();
        } catch (_) {}
        await Promise.all([
          this._loadClusterNodes(),
          this._loadClusterHealth(),
          this._loadClusterCCS(),
        ]);
      } catch (_) {
        this.clusterEnabled = false;
      }
    },

    async _loadClusterNodes() {
      try {
        const data = await this.apiCall('/v1/cluster/nodes');
        const health = await this.apiCall('/v1/cluster/health');
        const cortexId = health.is_leader ? health.role : null;
        const prevEpoch = this.clusterHealth ? this.clusterHealth.epoch : null;
        const newEpoch = health.epoch || 0;

        this.clusterNodes = (data.nodes || []).map(n => ({
          nodeId: n.node_id,
          role: n.role,
          status: this._nodeStatus(n, health),
          epoch: newEpoch,
          lag: n.last_seq,
        }));

        this.clusterHealth = health;

        // Detect epoch change → record failover event
        if (prevEpoch !== null && newEpoch !== prevEpoch && newEpoch > 0) {
          this._recordFailoverEvent(newEpoch, health);
        }
      } catch (_) {}
    },

    async _loadClusterHealth() {
      try {
        this.clusterHealth = await this.apiCall('/v1/cluster/health');
      } catch (_) {}
    },

    async _loadClusterCCS() {
      try {
        this.clusterCCS = await this.apiCall('/v1/cluster/cognitive/consistency');
      } catch (_) {}
    },

    _nodeStatus(node, health) {
      if (!health) return 'unknown';
      if (health.status === 'down') return 'down';
      if (node.role === 'primary' || node.role === 'cortex') return 'healthy';
      const lag = node.last_seq || 0;
      if (lag >= 10000) return 'down';
      if (lag >= 1000) return 'degraded';
      return 'healthy';
    },

    _recordFailoverEvent(epoch, health) {
      const stored = JSON.parse(localStorage.getItem('muninnClusterEvents') || '[]');
      const ts = new Date().toISOString();
      const cortexId = health.cortex_id || health.node_id || 'unknown';
      const entry = {
        ts,
        epoch,
        cortexId,
        label: '[' + ts + '] Epoch ' + epoch + ': ' + cortexId + ' became Cortex',
      };
      stored.unshift(entry);
      const trimmed = stored.slice(0, 10);
      localStorage.setItem('muninnClusterEvents', JSON.stringify(trimmed));
      this.clusterEvents = trimmed;
    },

    loadClusterEvents() {
      this.clusterEvents = JSON.parse(localStorage.getItem('muninnClusterEvents') || '[]');
    },

    async enableCluster() {
      this.clusterEnableLoading = true;
      this.clusterEnableError = null;
      this.clusterEnableProgress = ['Validating settings...'];
      try {
        const resp = await fetch('/api/admin/cluster/enable', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          credentials: 'same-origin',
          body: JSON.stringify({
            role: this.clusterEnableForm.role,
            bind_addr: this.clusterEnableForm.bindAddr,
            cluster_secret: this.clusterEnableForm.clusterSecret,
            cortex_addr: this.clusterEnableForm.cortexAddr,
          })
        });
        if (!resp.ok) {
          const err = await resp.json().catch(() => ({ error: 'Enable failed' }));
          throw new Error(err.error || 'Enable failed');
        }
        this.clusterEnableProgress = ['Initializing TLS...', 'Generating join token...', 'Starting heartbeat...'];
        await this._loadClusterInfo();
        this.clusterEnableProgress = [...this.clusterEnableProgress, 'Cluster active \u2713'];
      } catch (e) {
        this.clusterEnableError = e.message;
      } finally {
        this.clusterEnableLoading = false;
      }
    },

    async testNodeReachability() {
      this.addNodeTesting = true;
      this.addNodeTestResult = null;
      try {
        const resp = await fetch('/api/admin/cluster/nodes/test', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          credentials: 'same-origin',
          body: JSON.stringify({ addr: this.addNodeForm.addr })
        });
        const data = await resp.json();
        this.addNodeTestResult = data;
      } catch (e) {
        this.addNodeTestResult = { reachable: false, error: e.message };
      } finally {
        this.addNodeTesting = false;
      }
    },

    async addNode() {
      this.addNodeLoading = true;
      this.addNodeError = null;
      this.addNodeProgress = ['Validating token...'];
      try {
        const resp = await fetch('/api/admin/cluster/nodes', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          credentials: 'same-origin',
          body: JSON.stringify({ addr: this.addNodeForm.addr, token: this.addNodeForm.token })
        });
        if (!resp.ok) {
          const err = await resp.json().catch(() => ({ error: 'Add node failed' }));
          throw new Error(err.error || 'Add node failed');
        }
        this.addNodeProgress = ['Registering peer...', 'Waiting for join handshake...', 'Node added \u2713'];
        await new Promise(r => setTimeout(r, 1200));
        this.showAddNodeModal = false;
        this.addNodeForm = { addr: '', token: '' };
        this.addNodeProgress = [];
      } catch (e) {
        this.addNodeError = e.message;
      } finally {
        this.addNodeLoading = false;
      }
    },

    async removeNode() {
      if (!this.removeNodeTarget) return;
      this.removeNodeLoading = true;
      this.removeNodeError = null;
      try {
        const drain = this.removeNodeDrain ? '?drain=true' : '';
        const resp = await fetch(`/api/admin/cluster/nodes/${this.removeNodeTarget.nodeId}${drain}`, {
          method: 'DELETE',
          credentials: 'same-origin',
        });
        if (!resp.ok) {
          const err = await resp.json().catch(() => ({ error: 'Remove failed' }));
          throw new Error(err.error || 'Remove failed');
        }
        this.showRemoveNodeModal = false;
        this.removeNodeTarget = null;
        await this._loadClusterNodes();
      } catch (e) {
        this.removeNodeError = e.message;
      } finally {
        this.removeNodeLoading = false;
      }
    },

    async triggerFailover() {
      this.failoverLoading = true;
      this.failoverError = null;
      this.failoverProgress = ['Sending handoff request...'];
      try {
        const resp = await fetch('/api/admin/cluster/failover', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          credentials: 'same-origin',
          body: JSON.stringify({ target_node_id: this.failoverTarget })
        });
        if (!resp.ok) {
          const err = await resp.json().catch(() => ({ error: 'Failover failed' }));
          throw new Error(err.error || 'Failover failed');
        }
        this.failoverProgress = ['Sending handoff request...', 'New Cortex elected...', 'Handoff acknowledged...', 'Complete \u2713'];
        await new Promise(r => setTimeout(r, 1500));
        this.showFailoverModal = false;
        this.failoverProgress = [];
      } catch (e) {
        this.failoverError = e.message;
      } finally {
        this.failoverLoading = false;
      }
    },

    startClusterFeed() {
      if (this._clusterFeedSSE) return;
      const es = new EventSource('/api/admin/cluster/events');
      es.addEventListener('entry', (e) => {
        if (this.clusterFeedPaused) return;
        try {
          const data = JSON.parse(e.data);
          this.clusterFeed.unshift({ ...data, ts: new Date().toLocaleTimeString() });
          if (this.clusterFeed.length > 200) this.clusterFeed.pop();
        } catch (_) {}
      });
      this._clusterFeedSSE = es;
    },

    stopClusterFeed() {
      if (this._clusterFeedSSE) {
        this._clusterFeedSSE.close();
        this._clusterFeedSSE = null;
      }
    },

    async loadClusterToken() {
      this.clusterTokenLoading = true;
      try {
        const resp = await fetch('/api/admin/cluster/token', { credentials: 'same-origin' });
        if (resp.ok) this.clusterToken = await resp.json();
      } catch (_) {}
      finally { this.clusterTokenLoading = false; }
    },

    async regenerateToken() {
      this.showRegenerateTokenConfirm = false;
      try {
        const resp = await fetch('/api/admin/cluster/token/regenerate', {
          method: 'POST',
          credentials: 'same-origin',
        });
        if (resp.ok) this.clusterToken = await resp.json();
      } catch (_) {}
    },

    copyToken() {
      if (!this.clusterToken?.token) return;
      navigator.clipboard.writeText(this.clusterToken.token).catch(() => {});
      this.clusterTokenCopied = true;
      setTimeout(() => { this.clusterTokenCopied = false; }, 2000);
    },

    async saveClusterSettings() {
      this.clusterSettingsSaving = true;
      this.clusterSettingsSaved = false;
      try {
        const resp = await fetch('/api/admin/cluster/settings', {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          credentials: 'same-origin',
          body: JSON.stringify(this.clusterSettings)
        });
        if (resp.ok) {
          this.clusterSettingsSaved = true;
          setTimeout(() => { this.clusterSettingsSaved = false; }, 2500);
        }
      } catch (_) {}
      finally { this.clusterSettingsSaving = false; }
    },

    async rotateTLS() {
      try {
        const resp = await fetch('/api/admin/cluster/tls/rotate', {
          method: 'POST',
          credentials: 'same-origin',
        });
        if (!resp.ok) {
          this.addNotification('error', 'TLS rotation failed');
        } else {
          this.addNotification('success', 'TLS certificate rotated');
        }
      } catch (_) {
        this.addNotification('error', 'TLS rotation failed');
      }
    },

    clusterBannerClass() {
      if (!this.clusterHealth) return 'cluster-banner-unknown';
      const s = this.clusterHealth.status;
      if (s === 'ok') return 'cluster-banner-ok';
      if (s === 'degraded') return 'cluster-banner-degraded';
      return 'cluster-banner-down';
    },

    clusterBannerText() {
      if (!this.clusterHealth) return 'Cluster status unknown';
      const s = this.clusterHealth.status;
      const n = this.clusterNodes.length;
      if (s === 'ok') return 'Cluster healthy \u2014 ' + n + ' node' + (n !== 1 ? 's' : '');
      if (s === 'degraded') return 'Cluster degraded \u2014 check replication lag';
      return 'Cluster down \u2014 no quorum';
    },

    ccsScore() {
      if (!this.clusterCCS) return 0;
      return Math.round((this.clusterCCS.score || 0) * 100);
    },

    ccsBarColor() {
      const pct = this.ccsScore();
      if (pct >= 99) return '#22c55e';
      if (pct >= 90) return '#eab308';
      return '#ef4444';
    },

    nodeStatusBadgeClass(status) {
      if (status === 'healthy') return 'badge-active';
      if (status === 'degraded') return 'badge-warning';
      return 'badge-dormant';
    },

    // ── Notifications ──────────────────────────────────────────────────────
    addNotification(type, msg) {
      const id = ++this._notifId;
      this.notifications.push({ id, type, msg });
      const timeout = type === 'error' ? 6000 : 4000;
      setTimeout(() => this.removeNotification(id), timeout);
    },

    removeNotification(id) {
      this.notifications = this.notifications.filter(n => n.id !== id);
    },

    // ── Plugin config save ───────────────────────────────────────────────────
    async savePluginConfig(section) {
      const c = this.pluginCfg;
      const savedKey = section + 'Saved';
      const errorKey = section + 'Error';
      c[savedKey] = false;
      c[errorKey] = '';

      // Build payload from current pluginCfg state.
      const payload = {
        embed_provider: c.embedProvider === 'none' ? '' : c.embedProvider,
        embed_url: c.embedProvider === 'ollama' ? `ollama://localhost:11434/${c.embedOllamaModel}` : (c.embedProvider === 'openai' && c.embedUrl ? c.embedUrl : ''),
        embed_api_key: (c.embedProvider === 'openai' || c.embedProvider === 'voyage') ? c.embedApiKey : '',
        enrich_provider: c.enrichProvider === 'none' ? '' : c.enrichProvider,
        enrich_url: c.enrichProvider === 'ollama'
          ? `ollama://localhost:11434/${c.enrichOllamaModel}`
          : c.enrichProvider === 'openai' ? 'openai://gpt-4o-mini'
          : c.enrichProvider === 'anthropic' ? `anthropic://${c.enrichModel}`
          : '',
        enrich_api_key: (c.enrichProvider === 'openai' || c.enrichProvider === 'anthropic') ? c.enrichApiKey : '',
      };

      try {
        await this.apiCall('/api/admin/plugin-config', { method: 'PUT', body: JSON.stringify(payload) });
        c[savedKey] = true;
        setTimeout(() => { c[savedKey] = false; }, 4000);
        if (section === 'embed') c.embedShowForm = false;
        if (section === 'enrich') c.enrichShowForm = false;
      } catch (e) {
        c[errorKey] = e?.message || 'Save failed';
        setTimeout(() => { c[errorKey] = ''; }, 5000);
      }
    },

    async reembedVault() {
      if (!confirm(`Re-embed vault "${this.vault}"?\n\nThis clears all embeddings and lets the RetroactiveProcessor re-embed every engram with the current model.\n\nThe vault stays queryable during migration (with degraded recall).`)) return;
      try {
        const data = await this.apiCall('/api/admin/vaults/' + encodeURIComponent(this.vault) + '/reembed', { method: 'POST' });
        this.addNotification('success', `Re-embed started (job ${data.job_id}). Monitor via Embed Status.`);
        // Refresh embed status to show progress.
        this.loadEmbedStatus();
      } catch (e) {
        this.addNotification('error', 'Re-embed failed: ' + (e?.message || 'unknown error'));
      }
    },

    // ── Vault actions ──────────────────────────────────────────────────────
    openVaultAction(action) {
      this.vaultActionModal = {
        show: true,
        action,
        vault: this.vault,
        confirmText: '',
        memCount: this.stats?.engramCount || 0,
      };
    },

    async confirmVaultAction() {
      const { action, vault } = this.vaultActionModal;
      this.vaultActionModal.show = false;
      const method = action === 'delete' ? 'DELETE' : 'POST';
      const path = action === 'delete'
        ? '/api/admin/vaults/' + encodeURIComponent(vault)
        : '/api/admin/vaults/' + encodeURIComponent(vault) + '/clear';
      const headers = { 'Content-Type': 'application/json' };
      if (vault === 'default') {
        headers['X-Allow-Default'] = 'true';
      }
      try {
        const r = await fetch(path, { method, headers });
        if (r.ok) {
          if (action === 'delete') {
            await this.loadVaults();
            if (this.vault === vault) {
              this.vault = this.vaults?.[0] || '';
              localStorage.setItem('muninnVault', this.vault);
            }
            this.addNotification('success', 'Vault deleted');
          } else {
            this.addNotification('success', 'Memories cleared');
          }
        } else if (r.status === 401) {
          this.addNotification('error', 'Not authenticated');
        } else if (r.status === 409) {
          this.addNotification('error', 'Protected vault — cannot modify default');
        } else {
          this.addNotification('error', 'Error: ' + r.status);
        }
      } catch (e) {
        this.addNotification('error', 'Network error');
      }
    },

    // ── Rename ─────────────────────────────────────────────────────────────
    openVaultRename() {
      const newName = prompt('Enter new name for vault "' + this.vault + '":');
      if (!newName || newName === this.vault) return;
      this.renameVault(newName);
    },

    async renameVault(newName, force) {
      try {
        const url = '/api/admin/vaults/' + encodeURIComponent(this.vault) + '/rename' + (force ? '?force=true' : '');
        const r = await fetch(url, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ new_name: newName }),
        });
        if (r.status === 409) {
          const data = await r.json().catch(() => null);
          if (data && data.code === 'VAULT_NAME_COLLISION') {
            const proceed = confirm(
              'A vault named "' + data.conflict + '" already exists with a similar name.\n\nCreate "' + newName + '" anyway?'
            );
            if (proceed) {
              this.renameVault(newName, true);
            }
            return;
          }
        }
        if (!r.ok) {
          const err = await r.json().catch(() => null);
          const msg = err && err.error && err.error.message ? err.error.message : 'HTTP ' + r.status;
          this.addNotification('error', 'Rename failed: ' + msg);
          return;
        }
        this.vault = newName;
        this.loadVaults();
        this.addNotification('success', 'Vault renamed to "' + newName + '"');
      } catch (e) {
        this.addNotification('error', 'Rename failed: ' + e.message);
      }
    },

    // ── Clone / Merge ───────────────────────────────────────────────────────
    openVaultClone() {
      if (this.activeJob && this.activeJob.status === 'running') {
        this.addNotification('warning', 'A clone or merge job is still in progress.');
        return;
      }
      this.cloneModal = { show: true, source: this.vault, newName: '' };
      this.clearActiveJob();
    },

    openVaultMerge() {
      if (this.activeJob && this.activeJob.status === 'running') {
        this.addNotification('warning', 'A clone or merge job is still in progress.');
        return;
      }
      this.mergeModal = { show: true, source: this.vault, target: '', deleteSource: false };
      this.clearActiveJob();
    },

    async startClone() {
      if (!this.cloneModal.newName) return;
      const r = await fetch(
        '/api/admin/vaults/' + encodeURIComponent(this.cloneModal.source) + '/clone',
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ new_name: this.cloneModal.newName }),
        }
      );
      if (!r.ok) {
        this.addNotification('error', 'Clone failed: ' + r.status);
        return;
      }
      const { job_id } = await r.json();
      this.startJobPolling(job_id, this.cloneModal.source, () => {
        this.loadVaults();
        this.cloneModal.show = false;
        this.addNotification('success', 'Vault cloned successfully');
      });
    },

    async startMerge() {
      if (!this.mergeModal.target) return;
      const r = await fetch(
        '/api/admin/vaults/' + encodeURIComponent(this.mergeModal.source) + '/merge-into',
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ target: this.mergeModal.target, delete_source: this.mergeModal.deleteSource }),
        }
      );
      if (!r.ok) {
        this.addNotification('error', 'Merge failed: ' + r.status);
        return;
      }
      const { job_id } = await r.json();
      this.startJobPolling(job_id, this.mergeModal.source, () => {
        this.loadVaults();
        this.mergeModal.show = false;
        this.addNotification('success', 'Vaults merged successfully');
      });
    },

    startJobPolling(jobId, vaultName, onComplete) {
      this.clearActiveJob();
      this.activeJob = { status: 'running', pct: 0, phase: 'copying', copy_current: 0, copy_total: 0 };
      this.jobPollInterval = setInterval(async () => {
        try {
          const s = await fetch(
            '/api/admin/vaults/' + encodeURIComponent(vaultName) + '/job-status?job_id=' + jobId
          );
          if (!s.ok) return;
          const snap = await s.json();
          this.activeJob = snap;
          if (snap.status !== 'running') {
            this.clearActiveJob();
            if (snap.status === 'done') {
              onComplete();
            } else {
              this.addNotification('error', 'Job failed: ' + (snap.error || 'unknown'));
            }
          }
        } catch (e) {
          // network hiccup — keep polling
        }
      }, 1000);
    },

    clearActiveJob() {
      if (this.jobPollInterval) {
        clearInterval(this.jobPollInterval);
        this.jobPollInterval = null;
      }
      this.activeJob = null;
    },

    // ── Vault export ───────────────────────────────────────────────────────
    async exportVault() {
      this.vaultExporting = true;
      try {
        const res = await fetch('/api/admin/vaults/' + encodeURIComponent(this.vault) + '/export');
        if (!res.ok) {
          const text = await res.text().catch(() => res.statusText);
          throw new Error(res.status + ': ' + text);
        }
        const blob = await res.blob();
        const url = URL.createObjectURL(blob);
        const a = document.createElement('a');
        a.href = url;
        a.download = this.vault + '.muninn';
        document.body.appendChild(a);
        a.click();
        a.remove();
        URL.revokeObjectURL(url);
        this.addNotification('success', 'Vault exported: ' + this.vault + '.muninn');
      } catch (e) {
        this.addNotification('error', 'Export failed: ' + (e?.message || 'unknown error'));
      } finally {
        this.vaultExporting = false;
      }
    },

    // ── Vault import ───────────────────────────────────────────────────────
    openImportModal() {
      this.importModal = { show: true, vaultName: '', file: null, resetMeta: false };
    },

    async startImport() {
      if (!this.importModal.vaultName || !this.importModal.file) return;
      const params = new URLSearchParams({
        vault: this.importModal.vaultName,
        reset_metadata: this.importModal.resetMeta ? 'true' : 'false',
      });
      try {
        const res = await fetch('/api/admin/vaults/import?' + params.toString(), {
          method: 'POST',
          headers: { 'Content-Type': 'application/octet-stream' },
          body: this.importModal.file,
        });
        if (!res.ok) {
          const text = await res.text().catch(() => res.statusText);
          throw new Error(res.status + ': ' + text);
        }
        const data = await res.json();
        const jobId = data.job_id;
        this.startJobPolling(jobId, this.importModal.vaultName, () => {
          this.loadVaults();
          this.importModal.show = false;
          this.addNotification('success', 'Vault imported successfully');
        });
      } catch (e) {
        this.addNotification('error', 'Import failed: ' + (e?.message || 'unknown error'));
      }
    },

    // ── FTS reindex ────────────────────────────────────────────────────────
    async reindexFTS() {
      if (!confirm('Reindex full-text search for vault "' + this.vault + '"?\n\nThis rebuilds the FTS index for all engrams. The vault stays queryable during reindex.')) return;
      this.reindexing = true;
      try {
        const res = await fetch(
          '/api/admin/vaults/' + encodeURIComponent(this.vault) + '/reindex-fts',
          { method: 'POST' }
        );
        if (!res.ok) {
          const text = await res.text().catch(() => res.statusText);
          throw new Error(res.status + ': ' + text);
        }
        const data = await res.json();
        this.addNotification('success', 'FTS reindex complete — ' + (data.engrams_reindexed || 0) + ' engrams reindexed');
      } catch (e) {
        this.addNotification('error', 'Reindex failed: ' + (e?.message || 'unknown error'));
      } finally {
        this.reindexing = false;
      }
    },

    // ── Lifecycle state ────────────────────────────────────────────────────
    async updateLifecycleState(id, state) {
      try {
        const res = await fetch('/api/engrams/' + encodeURIComponent(id) + '/state', {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ vault: this.vault, state }),
        });
        if (!res.ok) {
          const text = await res.text().catch(() => res.statusText);
          throw new Error(res.status + ': ' + text);
        }
        if (this.selectedMemory && this.selectedMemory.id === id) {
          this.selectedMemory = { ...this.selectedMemory, state };
        }
        this.addNotification('success', 'Lifecycle state updated to ' + state);
      } catch (e) {
        this.addNotification('error', 'State update failed: ' + (e?.message || 'unknown error'));
      }
    },

    async probeOllama() {
      if (this.pluginCfg.ollamaChecking) return;
      this.pluginCfg.ollamaChecking = true;
      try {
        const r = await fetch('http://localhost:11434/api/tags', { signal: AbortSignal.timeout(3000) });
        if (r.ok) {
          const data = await r.json();
          const models = (data.models || []).map(m => m.name);
          this.pluginCfg.ollamaModels = models;
          this.pluginCfg.ollamaEmbedModels = models.filter(m => m.toLowerCase().includes('embed'));
          this.pluginCfg.ollamaDetected = true;
          if (models.length) {
            const embedList = this.pluginCfg.ollamaEmbedModels.length
              ? this.pluginCfg.ollamaEmbedModels : models;
            if (!embedList.includes(this.pluginCfg.embedOllamaModel)) {
              this.pluginCfg.embedOllamaModel = embedList[0];
            }
            const llmList = models.filter(m => !m.toLowerCase().includes('embed'));
            const enrichList = llmList.length ? llmList : models;
            if (!enrichList.includes(this.pluginCfg.enrichOllamaModel)) {
              this.pluginCfg.enrichOllamaModel = enrichList[0];
            }
          }
        } else {
          this.pluginCfg.ollamaDetected = false;
        }
      } catch {
        this.pluginCfg.ollamaDetected = false;
      }
      this.pluginCfg.ollamaChecking = false;
    },

    // ── Explain Score ──────────────────────────────────────────────────────
    async explainScore(engramId) {
      if (!this.searchQuery.trim()) return;
      this.explainModal = { show: true, data: null, loading: true };
      try {
        const data = await this.apiCall('/api/explain', {
          method: 'POST',
          body: JSON.stringify({
            vault: this.vault,
            engram_id: engramId,
            query: [this.searchQuery.trim()],
          }),
        });
        this.explainModal = { show: true, data, loading: false };
      } catch (err) {
        this.explainModal = { show: false, data: null, loading: false };
        this.addNotification('error', 'Explain failed: ' + err.message);
      }
    },

    closeExplainModal() {
      this.explainModal = { show: false, data: null, loading: false };
    },

    // ── Multi-select / Consolidate ─────────────────────────────────────────
    toggleMultiSelect() {
      this.multiSelectMode = !this.multiSelectMode;
      if (!this.multiSelectMode) {
        this.selectedMemoryIds = [];
      }
    },

    toggleMemorySelection(id) {
      const idx = this.selectedMemoryIds.indexOf(id);
      if (idx === -1) {
        this.selectedMemoryIds.push(id);
      } else {
        this.selectedMemoryIds.splice(idx, 1);
      }
    },

    openConsolidate() {
      if (this.selectedMemoryIds.length < 2) {
        this.addNotification('error', 'Select at least 2 memories to consolidate');
        return;
      }
      // Pre-fill with combined content from selected memories
      const selected = this.memories.filter(m => this.selectedMemoryIds.includes(m.id));
      const combined = selected.map(m => (m.concept ? '[' + m.concept + ']\n' : '') + m.content).join('\n\n---\n\n');
      this.consolidateModal = { show: true, mergedContent: combined };
    },

    async submitConsolidate() {
      if (!this.consolidateModal.mergedContent.trim()) {
        this.addNotification('error', 'Merged content cannot be empty');
        return;
      }
      try {
        const data = await this.apiCall('/api/consolidate', {
          method: 'POST',
          body: JSON.stringify({
            vault: this.vault,
            ids: this.selectedMemoryIds,
            merged_content: this.consolidateModal.mergedContent.trim(),
          }),
        });
        this.consolidateModal = { show: false, mergedContent: '' };
        this.selectedMemoryIds = [];
        this.multiSelectMode = false;
        this.addNotification('success', 'Memories consolidated (new ID: ' + data.id.slice(0, 8) + '…)');
        await this.loadMemories();
      } catch (err) {
        this.addNotification('error', 'Consolidate failed: ' + err.message);
      }
    },

    // ── Decide ─────────────────────────────────────────────────────────────
    openDecideModal() {
      const evidenceIds = this.selectedMemoryIds.length > 0
        ? this.selectedMemoryIds.join('\n')
        : '';
      this.decideModal = { show: true, decision: '', rationale: '', alternatives: '', evidenceIds };
    },

    async submitDecide() {
      if (!this.decideModal.decision.trim()) {
        this.addNotification('error', 'Decision text is required');
        return;
      }
      const alternatives = this.decideModal.alternatives
        .split('\n')
        .map(s => s.trim())
        .filter(Boolean);
      const evidenceIds = this.decideModal.evidenceIds
        .split('\n')
        .map(s => s.trim())
        .filter(Boolean);
      try {
        const data = await this.apiCall('/api/decide', {
          method: 'POST',
          body: JSON.stringify({
            vault: this.vault,
            decision: this.decideModal.decision.trim(),
            rationale: this.decideModal.rationale.trim(),
            alternatives,
            evidence_ids: evidenceIds,
          }),
        });
        this.decideModal = { show: false, decision: '', rationale: '', alternatives: '', evidenceIds: '' };
        this.addNotification('success', 'Decision recorded (ID: ' + data.id.slice(0, 8) + '…)');
        await this.loadMemories();
      } catch (err) {
        this.addNotification('error', 'Decide failed: ' + err.message);
      }
    },
  }));
});
