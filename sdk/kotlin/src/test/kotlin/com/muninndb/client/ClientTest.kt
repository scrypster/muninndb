package com.muninndb.client

import kotlinx.coroutines.flow.take
import kotlinx.coroutines.flow.toList
import kotlinx.coroutines.runBlocking
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.After
import org.junit.Before
import org.junit.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertTrue

class ClientTest {
    private lateinit var server: MockWebServer
    private lateinit var client: MuninnClient

    @Before
    fun setUp() {
        server = MockWebServer()
        server.start()
        client = MuninnClient(
            baseUrl = server.url("/").toString().trimEnd('/'),
            token = "test-token"
        )
    }

    @After
    fun tearDown() {
        server.shutdown()
    }

    // ── Write ────────────────────────────────────────────────────────

    @Test
    fun testWrite() = runBlocking {
        server.enqueue(MockResponse()
            .setBody("""{"id":"01ARZ3NDEK","created_at":1700000000}""")
            .addHeader("Content-Type", "application/json"))

        val result = client.write(WriteOptions(concept = "test", content = "content"))
        assertEquals("01ARZ3NDEK", result.id)
        assertEquals(1700000000L, result.createdAt)

        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertEquals("/api/engrams", req.path)
        assertEquals("Bearer test-token", req.getHeader("Authorization"))

        val body = client.gson.fromJson(req.body.readUtf8(), WriteOptions::class.java)
        assertEquals("test", body.concept)
        assertEquals("content", body.content)
    }

    // ── WriteBatch ───────────────────────────────────────────────────

    @Test
    fun testWriteBatch() = runBlocking {
        server.enqueue(MockResponse()
            .setBody("""{"results":[{"index":0,"id":"id1","status":"ok","error":null},{"index":1,"id":"id2","status":"ok","error":null}]}""")
            .addHeader("Content-Type", "application/json"))

        val result = client.writeBatch("v1", listOf(
            WriteOptions(concept = "a", content = "aaa"),
            WriteOptions(concept = "b", content = "bbb")
        ))
        assertEquals(2, result.results.size)
        assertEquals("id1", result.results[0].id)
        assertEquals("id2", result.results[1].id)

        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertEquals("/api/engrams/batch", req.path)
    }

    // ── Read ─────────────────────────────────────────────────────────

    @Test
    fun testRead() = runBlocking {
        server.enqueue(MockResponse()
            .setBody("""{"id":"abc","concept":"c","content":"body","confidence":0.9,"relevance":0.8,"stability":0.7,"access_count":3,"tags":["t"],"state":0,"created_at":100,"updated_at":200,"last_access":300,"summary":null,"key_points":null,"memory_type":1,"type_label":"episodic","embed_dim":null}""")
            .addHeader("Content-Type", "application/json"))

        val engram = client.read("abc", vault = "v1")
        assertEquals("abc", engram.id)
        assertEquals("c", engram.concept)
        assertEquals(0.9, engram.confidence)
        assertEquals(1, engram.memoryType)

        val req = server.takeRequest()
        assertEquals("GET", req.method)
        assertTrue(req.path!!.startsWith("/api/engrams/abc"))
        assertTrue(req.path!!.contains("vault=v1"))
    }

    // ── Forget (soft) ────────────────────────────────────────────────

    @Test
    fun testForgetSoft() = runBlocking {
        server.enqueue(MockResponse().setResponseCode(204))

        client.forget("abc", vault = "v1")

        val req = server.takeRequest()
        assertEquals("DELETE", req.method)
        assertTrue(req.path!!.startsWith("/api/engrams/abc"))
        assertTrue(req.path!!.contains("vault=v1"))
    }

    // ── Forget (hard) ────────────────────────────────────────────────

    @Test
    fun testForgetHard() = runBlocking {
        server.enqueue(MockResponse().setResponseCode(204))

        client.forget("abc", vault = "v1", hard = true)

        val req = server.takeRequest()
        assertEquals("DELETE", req.method)
        assertTrue(req.path!!.startsWith("/api/engrams/abc"))
        assertTrue(req.path!!.contains("hard=true"))
        assertTrue(req.path!!.contains("vault=v1"))
    }

    // ── Activate ─────────────────────────────────────────────────────

    @Test
    fun testActivate() = runBlocking {
        server.enqueue(MockResponse()
            .setBody("""{"query_id":"q1","total_found":1,"activations":[{"id":"e1","concept":"c","content":"body","summary":null,"score":0.95,"confidence":0.9,"why":"match","hop_path":null,"dormant":false,"created_at":100,"last_access":200,"access_count":1,"relevance":0.8}],"latency_ms":12.5,"brief":null}""")
            .addHeader("Content-Type", "application/json"))

        val result = client.activate(ActivateOptions(context = listOf("query"), maxResults = 5))
        assertEquals("q1", result.queryId)
        assertEquals(1, result.totalFound)
        assertEquals(0.95, result.activations[0].score)

        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertEquals("/api/activate", req.path)
    }

    // ── Link ─────────────────────────────────────────────────────────

    @Test
    fun testLink() = runBlocking {
        server.enqueue(MockResponse().setResponseCode(200).setBody("{}").addHeader("Content-Type", "application/json"))

        client.link(LinkOptions(sourceId = "a", targetId = "b", relType = 1, weight = 0.5))

        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertEquals("/api/link", req.path)
        val body = req.body.readUtf8()
        assertTrue(body.contains("\"source_id\":\"a\""))
        assertTrue(body.contains("\"target_id\":\"b\""))
    }

    // ── Traverse ─────────────────────────────────────────────────────

    @Test
    fun testTraverse() = runBlocking {
        server.enqueue(MockResponse()
            .setBody("""{"nodes":[{"id":"n1","concept":"c","hop_dist":0,"summary":null}],"edges":[{"from_id":"n1","to_id":"n2","rel_type":"assoc","weight":0.8}],"total_reachable":2,"query_ms":5.0}""")
            .addHeader("Content-Type", "application/json"))

        val result = client.traverse(TraverseOptions(startId = "n1"))
        assertEquals(1, result.nodes.size)
        assertEquals(1, result.edges.size)
        assertEquals(2, result.totalReachable)

        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertEquals("/api/traverse", req.path)
    }

    // ── Evolve ───────────────────────────────────────────────────────

    @Test
    fun testEvolve() = runBlocking {
        server.enqueue(MockResponse()
            .setBody("""{"id":"e1"}""")
            .addHeader("Content-Type", "application/json"))

        val result = client.evolve("e1", newContent = "updated", reason = "correction")
        assertEquals("e1", result.id)

        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertTrue(req.path!!.startsWith("/api/engrams/e1/evolve"))
    }

    // ── Consolidate ──────────────────────────────────────────────────

    @Test
    fun testConsolidate() = runBlocking {
        server.enqueue(MockResponse()
            .setBody("""{"id":"merged1","archived":["a","b"],"warnings":null}""")
            .addHeader("Content-Type", "application/json"))

        val result = client.consolidate(listOf("a", "b"), mergedContent = "combined")
        assertEquals("merged1", result.id)
        assertEquals(listOf("a", "b"), result.archived)

        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertEquals("/api/consolidate", req.path)
    }

    // ── Decide ───────────────────────────────────────────────────────

    @Test
    fun testDecide() = runBlocking {
        server.enqueue(MockResponse()
            .setBody("""{"id":"d1","warnings":null}""")
            .addHeader("Content-Type", "application/json"))

        val result = client.decide(DecideOptions(
            decision = "chose A",
            rationale = "because",
            alternatives = listOf("B", "C")
        ))
        assertEquals("d1", result.id)

        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertEquals("/api/decide", req.path)
    }

    // ── Restore ──────────────────────────────────────────────────────

    @Test
    fun testRestore() = runBlocking {
        server.enqueue(MockResponse()
            .setBody("""{"id":"r1","concept":"restored","restored":true,"state":"active"}""")
            .addHeader("Content-Type", "application/json"))

        val result = client.restore("r1")
        assertEquals("r1", result.id)
        assertTrue(result.restored)

        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertTrue(req.path!!.contains("/api/engrams/r1/restore"))
    }

    // ── Explain ──────────────────────────────────────────────────────

    @Test
    fun testExplain() = runBlocking {
        server.enqueue(MockResponse()
            .setBody("""{"engram_id":"e1","concept":"c","final_score":0.85,"components":{"full_text_relevance":0.7,"semantic_similarity":0.8,"decay_factor":0.95,"hebbian_boost":1.1,"access_frequency":0.5,"confidence":0.9},"fts_matches":["word"],"assoc_path":[],"would_return":true,"threshold":0.3}""")
            .addHeader("Content-Type", "application/json"))

        val result = client.explain(ExplainOptions(engramId = "e1", query = listOf("word")))
        assertEquals("e1", result.engramId)
        assertEquals(0.85, result.finalScore)
        assertTrue(result.wouldReturn)

        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertEquals("/api/explain", req.path)
    }

    // ── Set State ────────────────────────────────────────────────────

    @Test
    fun testSetState() = runBlocking {
        server.enqueue(MockResponse()
            .setBody("""{"id":"s1","state":"dormant","updated":true}""")
            .addHeader("Content-Type", "application/json"))

        val result = client.setState("s1", state = "dormant", reason = "inactive")
        assertEquals("s1", result.id)
        assertEquals("dormant", result.state)
        assertTrue(result.updated)

        val req = server.takeRequest()
        assertEquals("PUT", req.method)
        assertTrue(req.path!!.contains("/api/engrams/s1/state"))
    }

    // ── List Deleted ─────────────────────────────────────────────────

    @Test
    fun testListDeleted() = runBlocking {
        server.enqueue(MockResponse()
            .setBody("""{"deleted":[{"id":"d1","concept":"gone","deleted_at":100,"recoverable_until":200,"tags":null}],"count":1}""")
            .addHeader("Content-Type", "application/json"))

        val result = client.listDeleted(limit = 10)
        assertEquals(1, result.count)
        assertEquals("d1", result.deleted[0].id)

        val req = server.takeRequest()
        assertEquals("GET", req.method)
        assertTrue(req.path!!.startsWith("/api/deleted"))
    }

    // ── Retry Enrich ─────────────────────────────────────────────────

    @Test
    fun testRetryEnrich() = runBlocking {
        server.enqueue(MockResponse()
            .setBody("""{"engram_id":"e1","plugins_queued":["summary"],"already_complete":["entities"],"note":null}""")
            .addHeader("Content-Type", "application/json"))

        val result = client.retryEnrich("e1")
        assertEquals("e1", result.engramId)
        assertEquals(listOf("summary"), result.pluginsQueued)

        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertTrue(req.path!!.contains("/api/engrams/e1/retry-enrich"))
    }

    // ── Contradictions ───────────────────────────────────────────────

    @Test
    fun testContradictions() = runBlocking {
        server.enqueue(MockResponse()
            .setBody("""{"contradictions":[{"id_a":"a1","concept_a":"ca","id_b":"b1","concept_b":"cb","detected_at":500}]}""")
            .addHeader("Content-Type", "application/json"))

        val result = client.contradictions()
        assertEquals(1, result.contradictions.size)
        assertEquals("a1", result.contradictions[0].idA)

        val req = server.takeRequest()
        assertEquals("GET", req.method)
        assertTrue(req.path!!.startsWith("/api/contradictions"))
    }

    // ── Guide ────────────────────────────────────────────────────────

    @Test
    fun testGuide() = runBlocking {
        server.enqueue(MockResponse()
            .setBody("""{"guide":"Use activate to search."}""")
            .addHeader("Content-Type", "application/json"))

        val result = client.guide()
        assertEquals("Use activate to search.", result)

        val req = server.takeRequest()
        assertEquals("GET", req.method)
        assertTrue(req.path!!.startsWith("/api/guide"))
    }

    // ── Stats ────────────────────────────────────────────────────────

    @Test
    fun testStats() = runBlocking {
        server.enqueue(MockResponse()
            .setBody("""{"engram_count":42,"vault_count":2,"index_size":1024,"storage_bytes":4096,"coherence":null}""")
            .addHeader("Content-Type", "application/json"))

        val result = client.stats()
        assertEquals(42L, result.engramCount)
        assertEquals(2, result.vaultCount)

        val req = server.takeRequest()
        assertEquals("GET", req.method)
        assertTrue(req.path!!.startsWith("/api/stats"))
    }

    // ── List Engrams ─────────────────────────────────────────────────

    @Test
    fun testListEngrams() = runBlocking {
        server.enqueue(MockResponse()
            .setBody("""{"engrams":[{"id":"e1","concept":"c","content":"body","confidence":0.9,"tags":null,"vault":"default","created_at":100,"embed_dim":null}],"total":1,"limit":20,"offset":0}""")
            .addHeader("Content-Type", "application/json"))

        val result = client.listEngrams(limit = 20)
        assertEquals(1, result.engrams.size)
        assertEquals("e1", result.engrams[0].id)

        val req = server.takeRequest()
        assertEquals("GET", req.method)
        assertTrue(req.path!!.startsWith("/api/engrams"))
    }

    // ── Get Links ────────────────────────────────────────────────────

    @Test
    fun testGetLinks() = runBlocking {
        server.enqueue(MockResponse()
            .setBody("""{"links":[{"target_id":"t1","rel_type":1,"weight":0.8,"co_activation_count":3,"restored_at":null}]}""")
            .addHeader("Content-Type", "application/json"))

        val result = client.getLinks("e1")
        assertEquals(1, result.size)
        assertEquals("t1", result[0].targetId)
        assertEquals(1, result[0].relType)

        val req = server.takeRequest()
        assertEquals("GET", req.method)
        assertTrue(req.path!!.contains("/api/engrams/e1/links"))
    }

    // ── List Vaults ──────────────────────────────────────────────────

    @Test
    fun testListVaults() = runBlocking {
        server.enqueue(MockResponse()
            .setBody("""{"vaults":["default","work","personal"]}""")
            .addHeader("Content-Type", "application/json"))

        val result = client.listVaults()
        assertEquals(listOf("default", "work", "personal"), result)

        val req = server.takeRequest()
        assertEquals("GET", req.method)
        assertEquals("/api/vaults", req.path)
    }

    // ── Session ──────────────────────────────────────────────────────

    @Test
    fun testSession() = runBlocking {
        server.enqueue(MockResponse()
            .setBody("""{"entries":[{"id":"s1","concept":"c","content":"body","created_at":100}],"total":1,"offset":0,"limit":50}""")
            .addHeader("Content-Type", "application/json"))

        val result = client.session(limit = 50)
        assertEquals(1, result.entries.size)
        assertEquals("s1", result.entries[0].id)

        val req = server.takeRequest()
        assertEquals("GET", req.method)
        assertTrue(req.path!!.startsWith("/api/session"))
    }

    // ── Health ───────────────────────────────────────────────────────

    @Test
    fun testHealth() = runBlocking {
        server.enqueue(MockResponse()
            .setBody("""{"status":"ok","version":"0.9.0","uptime_seconds":3600,"db_writable":true}""")
            .addHeader("Content-Type", "application/json"))

        val result = client.health()
        assertEquals("ok", result.status)
        assertEquals("0.9.0", result.version)
        assertTrue(result.dbWritable)

        val req = server.takeRequest()
        assertEquals("GET", req.method)
        assertEquals("/api/health", req.path)
    }

    // ── Error mapping ────────────────────────────────────────────────

    @Test
    fun testError401() = runBlocking {
        server.enqueue(MockResponse().setResponseCode(401).setBody("unauthorized"))

        assertFailsWith<MuninnException.Unauthorized> {
            client.health()
        }
    }

    @Test
    fun testError404() = runBlocking {
        server.enqueue(MockResponse().setResponseCode(404).setBody("not found"))

        assertFailsWith<MuninnException.NotFound> {
            client.read("missing")
        }
    }

    @Test
    fun testError409() = runBlocking {
        server.enqueue(MockResponse().setResponseCode(409).setBody("conflict"))

        assertFailsWith<MuninnException.Conflict> {
            client.write(WriteOptions(concept = "dup", content = "dup"))
        }
    }

    @Test
    fun testError400() = runBlocking {
        server.enqueue(MockResponse().setResponseCode(400).setBody("bad request"))

        assertFailsWith<MuninnException.Validation> {
            client.write(WriteOptions(concept = "", content = ""))
        }
    }

    @Test
    fun testError500() = runBlocking {
        // Enqueue maxRetries + 1 responses for retry exhaustion
        repeat(client.maxRetries + 1) {
            server.enqueue(MockResponse().setResponseCode(500).setBody("server error"))
        }

        assertFailsWith<MuninnException.ServerError> {
            client.health()
        }
    }

    @Test
    fun testErrorMessageExtractedFromNestedJson() = runBlocking {
        server.enqueue(MockResponse()
            .setResponseCode(404)
            .setBody("""{"error":{"code":1004,"message":"engram not found"}}""")
            .addHeader("Content-Type", "application/json"))

        val ex = assertFailsWith<MuninnException.NotFound> {
            client.read("missing")
        }
        assertEquals("engram not found", ex.message)
    }

    // ── Subscribe (SSE) ──────────────────────────────────────────────

    @Test
    fun testSubscribe() = runBlocking {
        // MockWebServer delivers the full body; OkHttp reads it line-by-line via source
        val sseBody = "data: {\"type\":\"write\",\"id\":\"e1\"}\n\n" +
                      "data: {\"type\":\"write\",\"id\":\"e2\"}\n\n"
        server.enqueue(MockResponse()
            .setBody(sseBody)
            .addHeader("Content-Type", "text/event-stream"))

        val events = client.subscribe(vault = "myVault").take(2).toList()

        assertEquals(2, events.size)
        assertEquals("message", events[0].type)
        assertTrue(events[0].rawData.contains("e1"))
        assertTrue(events[1].rawData.contains("e2"))

        val req = server.takeRequest()
        assertEquals("GET", req.method)
        assertTrue(req.path!!.startsWith("/api/subscribe"))
        assertTrue(req.path!!.contains("vault=myVault"))
        assertTrue(req.path!!.contains("push_on_write=true"))
        assertEquals("Bearer test-token", req.getHeader("Authorization"))
        assertEquals("text/event-stream", req.getHeader("Accept"))
    }

    @Test
    fun testSubscribeEventTypeField() = runBlocking {
        val sseBody = "event: engram_written\ndata: {\"id\":\"e1\"}\n\n" +
                      "data: {\"id\":\"e2\"}\n\n"
        server.enqueue(MockResponse()
            .setBody(sseBody)
            .addHeader("Content-Type", "text/event-stream"))

        val events = client.subscribe().take(2).toList()

        assertEquals("engram_written", events[0].type)
        assertEquals("message", events[1].type)  // default when no event: line
    }

    @Test
    fun testSubscribeWithThreshold() = runBlocking {
        server.enqueue(MockResponse()
            .setBody("data: {\"type\":\"write\"}\n\n")
            .addHeader("Content-Type", "text/event-stream"))

        client.subscribe(vault = "default", threshold = 0.5).take(1).toList()

        val req = server.takeRequest()
        assertTrue(req.path!!.contains("threshold=0.5"))
    }

    // ── Auth header ──────────────────────────────────────────────────

    @Test
    fun testNoAuthHeaderWhenTokenEmpty() = runBlocking {
        server.enqueue(MockResponse()
            .setBody("""{"status":"ok","version":"0.9.0","uptime_seconds":0,"db_writable":true}""")
            .addHeader("Content-Type", "application/json"))

        val noAuthClient = MuninnClient(
            baseUrl = server.url("/").toString().trimEnd('/'),
            token = ""
        )
        noAuthClient.health()

        val req = server.takeRequest()
        assertEquals(null, req.getHeader("Authorization"))
    }

    // ── Default vault ────────────────────────────────────────────────

    @Test
    fun testDefaultVaultUsed() = runBlocking {
        server.enqueue(MockResponse()
            .setBody("""{"id":"abc","concept":"c","content":"body","confidence":0.9,"relevance":0.8,"stability":0.7,"access_count":0,"tags":null,"state":0,"created_at":0,"updated_at":0,"last_access":0,"summary":null,"key_points":null,"memory_type":0,"type_label":null,"embed_dim":null}""")
            .addHeader("Content-Type", "application/json"))

        client.read("abc")

        val req = server.takeRequest()
        assertTrue(req.path!!.contains("vault=default"))
    }
}
