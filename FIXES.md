# MuninnDB — Community Fixes

Fixes contributed by [@To3Knee](https://github.com/To3Knee).
Each fix is self-contained and can be applied independently.
Tracked in branch: `fix/live-feed-entity-graph-vault`

---

## Fix 1 — Live Feed: Alpine crash from malformed SSE events

**File:** `web/static/js/app.js`
**Function:** `_handleLiveMessage()` (~line 521)

### Problem
The Live Feed SSE handler pushes `memory_added` events into the `liveFeed` 
array without validating that `msg.data.id` exists. When the server sends a 
malformed event (missing `id`), the item is added with `id: undefined`.

Alpine.js uses `item.id` as the key for `x-for` DOM diffing. An `undefined` 
key corrupts Alpine's internal DOM anchor tracking, producing:

```
Alpine Warning: Duplicate key on x-for
<template x-for="item in liveFeed" :key="item.id">

Alpine Expression Error: can't access property "after", v is undefined
Expression: "liveFeed"
```

This crash cascades — once Alpine's reactivity system is corrupted, **all 
other components stop working**, including Entity Graph and the Vault Selector.

### Root Cause
`undefined === undefined` is `true` in JavaScript. Once one item with 
`id: undefined` is in the array, the dedup check blocks all future malformed 
messages. But the ONE item with `id: undefined` remains in the array, 
poisoning Alpine's key tracking on every re-render.

### Fix
Add a guard at the top of the `memory_added` branch to skip any event where 
`msg.data` or `msg.data.id` is missing, and log a warning so the issue is 
visible in the browser console.

```diff
 } else if (msg.type === 'memory_added') {
+  // Guard: skip malformed events missing required fields.
+  if (!msg.data || !msg.data.id) {
+    console.warn('[muninn] live feed received memory_added with missing id — skipping', msg.data);
+    return;
+  }
   if (!this.liveFeed.some(item => item.id === msg.data.id)) {
     const next = [msg.data, ...this.liveFeed];
     this.liveFeed = next.length > 20 ? next.slice(0, 20) : next;
   }
 }
```

### Verification
1. Open browser DevTools → Console
2. Push memories via MCP
3. Confirm no Alpine duplicate key warnings
4. Confirm Live Feed populates correctly
5. Confirm Entity Graph loads after Live Feed is stable

---

## Fix 2 — Vault Selector: vault list not refreshing when modal opens

**File:** `web/static/js/app.js`
**Function:** `$watch('vaultModalOpen')` (~line 309)

### Problem
When the vault picker modal opens, only `loadVaultStats()` is called — not 
`loadVaults()`. The vault list is only fetched at login/auth-check. Any vault 
created after page load will not appear in the dropdown until a full page 
reload.

### Root Cause
Missing `loadVaults()` call in the `vaultModalOpen` watcher.

### Fix
```diff
 this.$watch('vaultModalOpen', (open) => {
-  if (open) this.loadVaultStats();
+  if (open) {
+    this.loadVaults();
+    this.loadVaultStats();
+  }
 });
```

### Verification
1. Log into MuninnDB UI
2. Create a new vault via Settings
3. Open the vault picker modal
4. Confirm the new vault appears **without** a page reload

---

## Fix 3 — SSE onmessage: silent error swallowing

**File:** `web/static/js/app.js`
**Function:** `connectLive()` SSE onmessage handler (~line 498)

### Problem
The SSE `onmessage` catch block silently swallows all errors with `catch (_) {}`.
Malformed events, JSON parse failures, and handler exceptions produce no 
output — making these bugs invisible in production.

### Fix
```diff
 es.onmessage = (e) => {
   try {
     const msg = JSON.parse(e.data);
     this._handleLiveMessage(msg);
-  } catch (_) {}
+  } catch (err) {
+    console.warn('[muninn] failed to process live event:', err, e.data);
+  }
 };
```

### Verification
Open browser DevTools → Console. Any future SSE processing errors will now 
appear as warnings prefixed with `[muninn]` instead of failing silently.

---

## Applying These Fixes

### Option A — Rebuild Docker image (recommended)
```bash
git clone https://github.com/To3Knee/muninndb
cd muninndb
git checkout fix/live-feed-entity-graph-vault
docker build -t muninndb-fixed .
docker stop muninndb
docker run -d --name muninndb-fixed \
  -p 8474:8474 -p 8475:8475 -p 8476:8476 -p 8750:8750 \
  -v muninndb_data:/data \
  --env-file .env \
  muninndb-fixed
```

### Option B — Hot patch via --dev flag (no rebuild)
```bash
# Copy web directory to host
mkdir -p /opt/muninndb/web
# Copy the fixed app.js into the web/static/js/ path
# Restart container with --dev flag and volume mount
docker stop muninndb
docker run -d --name muninndb \
  -p 8474:8474 -p 8475:8475 -p 8476:8476 -p 8750:8750 \
  -v muninndb_data:/data \
  -v /opt/muninndb/web:/usr/local/bin/web \
  --env-file .env \
  ghcr.io/scrypster/muninndb:latest \
  --daemon --data /data --dev
```
