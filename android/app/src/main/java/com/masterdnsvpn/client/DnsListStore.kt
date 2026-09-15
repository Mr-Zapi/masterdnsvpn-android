package com.masterdnsvpn.client

import android.content.Context
import org.json.JSONArray
import org.json.JSONObject
import java.io.File
import java.util.UUID
import java.util.concurrent.Executors

/**
 * Persists named DNS lists (and the active selection) as JSON in app storage.
 * Uses the platform org.json API so the app needs no extra dependencies.
 */
class DnsListStore private constructor(context: Context) {

    private val file = File(context.filesDir, FILE_NAME)
    private val lists = ArrayList<DnsList>()
    private var activeId: String? = null
    private var loaded = false

    // Persistence runs off the UI thread; writes are serialized and use a
    // snapshot of the in-memory state taken while holding the lock.
    private val ioExecutor = Executors.newSingleThreadExecutor { r ->
        Thread(r, "dns-list-store").apply { isDaemon = true }
    }

    @Synchronized
    fun all(): List<DnsList> {
        ensureLoaded()
        return ArrayList(lists)
    }

    @Synchronized
    fun get(id: String?): DnsList? {
        ensureLoaded()
        if (id == null) return null
        return lists.firstOrNull { it.id == id }
    }

    @Synchronized
    fun active(): DnsList? {
        ensureLoaded()
        return lists.firstOrNull { it.id == activeId } ?: lists.firstOrNull()
    }

    @Synchronized
    fun activeId(): String? {
        ensureLoaded()
        return active()?.id
    }

    @Synchronized
    fun setActive(id: String) {
        ensureLoaded()
        if (lists.any { it.id == id }) {
            activeId = id
            persist()
        }
    }

    @Synchronized
    fun create(name: String, servers: List<String>, source: String = DnsList.SOURCE_MANUAL): DnsList {
        ensureLoaded()
        val list = DnsList(
            id = UUID.randomUUID().toString(),
            name = uniqueName(name),
            servers = normalizeServers(servers),
            source = source,
            updatedAt = System.currentTimeMillis()
        )
        lists.add(list)
        if (activeId == null) activeId = list.id
        persist()
        return list
    }

    @Synchronized
    fun rename(id: String, newName: String): Boolean {
        ensureLoaded()
        val index = lists.indexOfFirst { it.id == id }
        if (index < 0) return false
        val clean = newName.trim()
        if (clean.isEmpty()) return false
        lists[index] = lists[index].copy(name = uniqueName(clean, id), updatedAt = System.currentTimeMillis())
        persist()
        return true
    }

    @Synchronized
    fun updateServers(id: String, servers: List<String>): Boolean {
        ensureLoaded()
        val index = lists.indexOfFirst { it.id == id }
        if (index < 0) return false
        lists[index] = lists[index].copy(
            servers = normalizeServers(servers),
            updatedAt = System.currentTimeMillis()
        )
        persist()
        return true
    }

    @Synchronized
    fun duplicate(id: String): DnsList? {
        ensureLoaded()
        val source = lists.firstOrNull { it.id == id } ?: return null
        return create("${source.name} (copy)", source.servers, source.source)
    }

    @Synchronized
    fun delete(id: String): Boolean {
        ensureLoaded()
        val removed = lists.removeAll { it.id == id }
        if (!removed) return false
        if (activeId == id) activeId = lists.firstOrNull()?.id
        persist()
        return true
    }

    /**
     * Seeds the store from the legacy resolvers text (the old single text field)
     * when no lists exist yet. Returns true when a list was created.
     */
    @Synchronized
    fun seedFromLegacy(legacyResolvers: String, defaultServers: List<String>): Boolean {
        ensureLoaded()
        if (lists.isNotEmpty()) return false

        val fromLegacy = normalizeServers(legacyResolvers.split('\n', ',', ' ', '\t'))
        val servers = if (fromLegacy.isNotEmpty()) fromLegacy else normalizeServers(defaultServers)
        if (servers.isEmpty()) return false
        val source = if (fromLegacy.isNotEmpty()) DnsList.SOURCE_LEGACY else DnsList.SOURCE_MANUAL
        create("Default", servers, source)
        return true
    }

    fun serversText(list: DnsList?): String {
        if (list == null) return ""
        return list.servers.joinToString("\n")
    }

    private fun normalizeServers(input: List<String>): List<String> {
        val seen = LinkedHashSet<String>()
        for (raw in input) {
            val value = raw.trim()
            if (value.isNotEmpty()) seen.add(value)
        }
        return ArrayList(seen)
    }

    private fun uniqueName(candidate: String, selfId: String? = null): String {
        val base = candidate.trim().ifEmpty { "Unnamed" }
        if (lists.none { it.name.equals(base, ignoreCase = true) && it.id != selfId }) return base
        var counter = 2
        while (lists.any { it.name.equals("$base ($counter)", ignoreCase = true) && it.id != selfId }) {
            counter++
        }
        return "$base ($counter)"
    }

    private fun ensureLoaded() {
        if (loaded) return
        loaded = true
        if (!file.exists()) return
        try {
            val root = JSONObject(file.readText())
            activeId = root.optString("activeListId").takeIf { it.isNotBlank() }
            val arr = root.optJSONArray("lists") ?: JSONArray()
            for (i in 0 until arr.length()) {
                val obj = arr.optJSONObject(i) ?: continue
                lists.add(DnsList.fromJson(obj))
            }
        } catch (_: Exception) {
            lists.clear()
            activeId = null
        }
    }

    private fun persist() {
        val json = try {
            JSONObject().apply {
                put("activeListId", activeId ?: JSONObject.NULL)
                put("lists", JSONArray().apply { lists.forEach { put(it.toJson()) } })
            }.toString()
        } catch (_: Exception) {
            return
        }
        ioExecutor.execute {
            try {
                file.writeText(json)
            } catch (_: Exception) {
                // Best-effort persistence; in-memory state remains valid for the session.
            }
        }
    }

    companion object {
        private const val FILE_NAME = "dns_lists.json"

        @Volatile
        private var instance: DnsListStore? = null

        fun get(context: Context): DnsListStore {
            return instance ?: synchronized(this) {
                instance ?: DnsListStore(context.applicationContext).also { instance = it }
            }
        }
    }
}
