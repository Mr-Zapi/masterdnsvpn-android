package com.masterdnsvpn.client

import org.json.JSONArray
import org.json.JSONObject
import java.util.UUID

/**
 * A named, saved set of DNS resolvers. Lists are created manually, by scanning
 * public DNS servers, or by importing the legacy resolver text.
 */
data class DnsList(
    val id: String,
    val name: String,
    val servers: List<String>,
    val source: String,
    val updatedAt: Long
) {
    fun toJson(): JSONObject = JSONObject().apply {
        put("id", id)
        put("name", name)
        put("servers", JSONArray(servers))
        put("source", source)
        put("updatedAt", updatedAt)
    }

    companion object {
        const val SOURCE_MANUAL = "manual"
        const val SOURCE_SCAN = "scan"
        const val SOURCE_PUBLIC = "public"
        const val SOURCE_LEGACY = "legacy"

        fun fromJson(obj: JSONObject): DnsList {
            val serversArr = obj.optJSONArray("servers") ?: JSONArray()
            val servers = ArrayList<String>(serversArr.length())
            for (i in 0 until serversArr.length()) {
                val value = serversArr.optString(i).trim()
                if (value.isNotEmpty()) servers.add(value)
            }
            return DnsList(
                id = obj.optString("id").ifBlank { UUID.randomUUID().toString() },
                name = obj.optString("name", "Unnamed"),
                servers = servers,
                source = obj.optString("source", SOURCE_MANUAL),
                updatedAt = obj.optLong("updatedAt", System.currentTimeMillis())
            )
        }
    }
}
