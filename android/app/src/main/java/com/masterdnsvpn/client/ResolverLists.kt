package com.masterdnsvpn.client

import android.content.SharedPreferences
import org.json.JSONArray
import org.json.JSONObject

/**
 * One named group of DNS resolvers. Only entries from lists with [active] set
 * are handed to the Go core on connect.
 */
data class ResolverList(
    val id: String,
    var name: String,
    var entries: MutableList<String>,
    var active: Boolean
)

/**
 * Persists resolver lists as JSON in SharedPreferences.
 *
 * Nothing secret lives here: the encryption key stays inside the user's base64
 * config and is never stored or seeded by this class.
 */
class ResolverStore(private val prefs: SharedPreferences) {

    companion object {
        private const val KEY = "resolver_lists"
        private const val LEGACY_KEY = "resolvers"

        /** Homogeneous Yandex set - measured as the fastest working group. */
        private val SEED = listOf(
            "77.88.8.8:53", "77.88.8.1:53", "77.88.8.88:53",
            "77.88.8.2:53", "77.88.8.7:53", "77.88.8.3:53"
        )
    }

    fun load(): MutableList<ResolverList> {
        val raw = prefs.getString(KEY, null)
        if (raw.isNullOrBlank()) return migrate()
        return try {
            val parsed = parse(raw)
            if (parsed.isEmpty()) migrate() else parsed
        } catch (e: Exception) {
            migrate()
        }
    }

    fun save(lists: List<ResolverList>) {
        val arr = JSONArray()
        for (l in lists) {
            val o = JSONObject()
            o.put("id", l.id)
            o.put("name", l.name)
            o.put("active", l.active)
            val e = JSONArray()
            for (entry in l.entries) e.put(entry)
            o.put("entries", e)
            arr.put(o)
        }
        prefs.edit().putString(KEY, arr.toString()).apply()
    }

    /** Entries of every active list, de-duplicated, order preserved. */
    fun activeEntries(lists: List<ResolverList>): List<String> =
        lists.filter { it.active }.flatMap { it.entries }.distinct()

    fun activeResolversText(lists: List<ResolverList>): String =
        activeEntries(lists).joinToString("\n")

    /** Accepts newline- or comma-separated input; drops blanks and # comments. */
    fun parseEntries(text: String): MutableList<String> =
        text.split('\n', ',')
            .map { it.trim() }
            .filter { it.isNotEmpty() && !it.startsWith("#") }
            .toMutableList()

    fun newId(): String = System.currentTimeMillis().toString() + "-" + (0..9999).random()

    private fun parse(raw: String): MutableList<ResolverList> {
        val arr = JSONArray(raw)
        val out = mutableListOf<ResolverList>()
        for (i in 0 until arr.length()) {
            val o = arr.getJSONObject(i)
            val e = o.optJSONArray("entries") ?: JSONArray()
            val entries = mutableListOf<String>()
            for (j in 0 until e.length()) entries.add(e.getString(j))
            out.add(
                ResolverList(
                    id = o.optString("id", newId()),
                    name = o.optString("name", "List"),
                    entries = entries,
                    active = o.optBoolean("active", true)
                )
            )
        }
        return out
    }

    /** First run, or corrupted store: import the old flat field, else seed. */
    private fun migrate(): MutableList<ResolverList> {
        val legacy = parseEntries(prefs.getString(LEGACY_KEY, "") ?: "")
        val lists = mutableListOf(
            if (legacy.isNotEmpty()) {
                ResolverList(newId(), "Imported", legacy, true)
            } else {
                ResolverList(newId(), "Yandex", SEED.toMutableList(), true)
            }
        )
        save(lists)
        return lists
    }
}
