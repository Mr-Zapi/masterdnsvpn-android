package com.masterdnsvpn.client

import android.content.Context
import org.json.JSONArray
import org.json.JSONObject
import java.io.BufferedReader
import java.io.File
import java.io.FileInputStream
import java.io.FileOutputStream
import java.io.InputStream
import java.io.InputStreamReader
import java.net.HttpURLConnection
import java.net.URL
import java.util.zip.GZIPInputStream
import java.util.zip.GZIPOutputStream

/**
 * Downloads public DNS server lists from multiple providers, merges them and
 * stores the last successful, de-duplicated copy gzip-compressed on disk. The
 * cache lets the scanner keep working when the providers are unreachable.
 *
 * Providers:
 *  - https://public-dns.info  (full world list; CSV, JSON or TXT)
 *  - https://publicdnsserver.com (per-country plain text, e.g. russia.txt)
 *
 * Each provider is cached on its own first, so a failed refresh of one provider
 * never destroys the previously downloaded data. The merged cache is rebuilt
 * from whichever provider files are present.
 */
class PublicDnsRepository private constructor(context: Context) {

    private val dir = File(context.filesDir, CACHE_DIR)
    private val providerDir = File(dir, PROVIDER_DIR)
    private val cacheFile = File(dir, CACHE_FILE)
    private val metaFile = File(dir, META_FILE)

    data class Meta(
        val fetchedAt: Long,
        val total: Int,
        val countries: Map<String, Int>,
        val source: String
    )

    data class Entry(val ip: String, val country: String)

    /**
     * Downloads and caches all providers, then rebuilds the merged list. Returns
     * the merged metadata, or a failure if nothing could be fetched/merged. The
     * existing cache is left untouched on failure.
     */
    fun refreshAll(): Result<Meta> {
        dir.mkdirs()
        providerDir.mkdirs()

        for (provider in providers()) {
            refreshProvider(provider)
        }

        // Rebuild from whatever provider files exist (old and/or newly fetched),
        // so a provider outage falls back to the previous data.
        val meta = rebuildMergedCache()
        if (meta == null) {
            return Result.failure(IllegalStateException("could not fetch public DNS list"))
        }
        return Result.success(meta)
    }

    fun hasCache(): Boolean = cacheFile.exists() && cacheFile.length() > 0

    fun cachedMeta(): Meta? {
        if (!metaFile.exists()) return null
        return try {
            val obj = JSONObject(metaFile.readText())
            val countriesObj = obj.optJSONObject("countries") ?: JSONObject()
            val countries = HashMap<String, Int>()
            val keys = countriesObj.keys()
            while (keys.hasNext()) {
                val key = keys.next()
                countries[key] = countriesObj.optInt(key)
            }
            Meta(
                fetchedAt = obj.optLong("fetchedAt"),
                total = obj.optInt("total"),
                countries = countries,
                source = obj.optString("source")
            )
        } catch (_: Exception) {
            null
        }
    }

    fun availableCountries(): List<String> {
        val meta = cachedMeta() ?: return emptyList()
        return meta.countries.keys.sorted()
    }

    /**
     * Reads the merged cached list and returns resolver candidates ("ip", port
     * 53 is implied by the Go client). [country] of null/blank/"ALL" returns
     * every server. Duplicates across providers are already removed.
     */
    fun loadCandidates(country: String?): List<String> {
        if (!hasCache()) return emptyList()
        val want = (country ?: "ALL").trim().uppercase()
        val all = want.isEmpty() || want == ALL_COUNTRIES

        val out = ArrayList<String>()
        val seen = HashSet<String>()
        openGzReader(cacheFile).use { reader ->
            reader.forEachLine { line ->
                if (line.isBlank()) return@forEachLine
                val tab = line.indexOf('\t')
                if (tab <= 0) return@forEachLine
                val ip = line.substring(0, tab).trim()
                val cc = if (tab + 1 < line.length) line.substring(tab + 1).trim() else ""
                if (ip.isEmpty()) return@forEachLine
                if (!all && !cc.equals(want, ignoreCase = true)) return@forEachLine
                if (seen.add(ip)) out.add(ip)
            }
        }
        return out
    }

    fun cacheAgeMillis(): Long {
        val meta = cachedMeta() ?: return -1
        return System.currentTimeMillis() - meta.fetchedAt
    }

    // ------------------------------------------------------------------------

    private data class Provider(
        val id: String,
        val candidates: List<Pair<String, String>>,
        val forcedCountry: String = ""
    )

    private fun providers(): List<Provider> {
        val list = ArrayList<Provider>()
        list.add(
            Provider(
                id = "publicdns",
                candidates = listOf(
                    FULL_CSV_URL to "csv",
                    FULL_JSON_URL to "json",
                    FULL_TXT_URL to "txt"
                )
            )
        )
        for ((cc, slug) in PUBLICDNSSERVER_COUNTRIES) {
            list.add(
                Provider(
                    id = "publicdnsserver_$slug",
                    candidates = listOf("$PUBLICDNSSERVER_DOWNLOAD/$slug.txt" to "txt"),
                    forcedCountry = cc
                )
            )
        }
        return list
    }

    private fun providerFile(id: String) = File(providerDir, "$id.tsv.gz")

    private fun refreshProvider(provider: Provider): Boolean {
        val dest = providerFile(provider.id)
        for ((url, kind) in provider.candidates) {
            val ok = try {
                downloadAndNormalize(url, kind, provider.forcedCountry, dest)
            } catch (_: Exception) {
                false
            }
            if (ok) return true
        }
        return false
    }

    private fun downloadAndNormalize(url: String, kind: String, forcedCountry: String, dest: File): Boolean {
        val tmp = File(dest.parentFile, "${dest.name}.tmp")
        var total = 0

        val connection = (URL(url).openConnection() as HttpURLConnection).apply {
            connectTimeout = 20_000
            readTimeout = 60_000
            requestMethod = "GET"
            instanceFollowRedirects = true
            setRequestProperty("User-Agent", USER_AGENT)
        }
        try {
            if (connection.responseCode != HttpURLConnection.HTTP_OK) return false
            connection.inputStream.use { input ->
                GZIPOutputStream(FileOutputStream(tmp)).buffered().use { gz ->
                    val writer = gz.writer()
                    total = when (kind) {
                        "csv" -> normalizeCsv(input, writer, forcedCountry)
                        "txt" -> normalizeTxt(input, writer, forcedCountry)
                        else -> normalizeJson(input, writer, forcedCountry)
                    }
                    writer.flush()
                }
            }
        } finally {
            connection.disconnect()
        }

        if (total <= 0) {
            tmp.delete()
            return false
        }

        if (dest.exists()) dest.delete()
        if (!tmp.renameTo(dest)) {
            tmp.copyTo(dest, overwrite = true)
            tmp.delete()
        }
        return true
    }

    private fun normalizeCsv(input: InputStream, writer: java.io.Writer, forcedCountry: String): Int {
        var ipIndex = 0
        var ccIndex = 4
        var errorIndex = 7
        var total = 0
        val reader = BufferedReader(InputStreamReader(input, Charsets.UTF_8))
        var first = true
        reader.forEachLine { raw ->
            if (raw.isBlank()) return@forEachLine
            val fields = splitCsvLine(raw)
            if (first) {
                first = false
                val header = fields.map { it.trim().lowercase() }
                val hIp = header.indexOfFirst { it == "ip" || it == "ip_address" }
                val hCc = header.indexOfFirst { it == "country_id" || it == "country_code" }
                val hErr = header.indexOfFirst { it == "error" }
                if (hIp >= 0 && hCc >= 0) {
                    ipIndex = hIp
                    ccIndex = hCc
                    errorIndex = hErr
                    return@forEachLine
                }
            }
            if (fields.size <= ipIndex) return@forEachLine
            val ip = fields[ipIndex].trim()
            val cc = if (ccIndex in fields.indices) fields[ccIndex].trim().uppercase() else ""
            val error = if (errorIndex >= 0 && errorIndex in fields.indices) fields[errorIndex].trim() else ""
            if (isUsable(ip, error)) {
                writeEntry(writer, ip, cc.ifEmpty { forcedCountry })
                total++
            }
        }
        return total
    }

    private fun normalizeJson(input: InputStream, writer: java.io.Writer, forcedCountry: String): Int {
        val entries = parseJson(input.bufferedReader(Charsets.UTF_8).readText())
        var total = 0
        for (entry in entries) {
            writeEntry(writer, entry.ip, entry.country.ifEmpty { forcedCountry })
            total++
        }
        return total
    }

    private fun normalizeTxt(input: InputStream, writer: java.io.Writer, forcedCountry: String): Int {
        var total = 0
        input.bufferedReader(Charsets.UTF_8).forEachLine { raw ->
            val value = raw.trim().substringBefore(',').trim()
            if (value.isEmpty() || value.startsWith("#")) return@forEachLine

            // Strip an IPv4 ":port" suffix, but keep IPv6 addresses intact.
            var ip = value
            val firstColon = ip.indexOf(':')
            val lastColon = ip.lastIndexOf(':')
            if (firstColon > 0 && firstColon == lastColon) {
                val port = ip.substring(lastColon + 1)
                if (port.isNotEmpty() && port.all { it.isDigit() }) {
                    ip = ip.substring(0, lastColon)
                }
            }
            ip = ip.trim()
            if (ip.isEmpty()) return@forEachLine
            writeEntry(writer, ip, forcedCountry)
            total++
        }
        return total
    }

    /** Parses a public-dns.info JSON array (per-country file or full dump). */
    fun parseJson(text: String): List<Entry> {
        val out = ArrayList<Entry>()
        val array = try {
            JSONArray(text)
        } catch (_: Exception) {
            return out
        }
        for (i in 0 until array.length()) {
            val obj = array.optJSONObject(i) ?: continue
            val ip = obj.optString("ip").ifBlank { obj.optString("ip_address") }.trim()
            val cc = obj.optString("country_id").ifBlank { obj.optString("country_code") }
                .trim().uppercase()
            val error = obj.optString("error").trim()
            if (isUsable(ip, error)) out.add(Entry(ip, cc))
        }
        return out
    }

    /**
     * Merges every cached provider file into a single de-duplicated cache,
     * keeping the first country seen for each IP.
     */
    private fun rebuildMergedCache(): Meta? {
        val files = providerDir.listFiles()
            ?.filter { it.isFile && it.name.endsWith(".tsv.gz") }
            ?: emptyList()
        if (files.isEmpty()) return null

        val merged = LinkedHashMap<String, String>()
        val sourceIds = ArrayList<String>()
        for (file in files) {
            sourceIds.add(file.name.removeSuffix(".tsv.gz"))
            try {
                openGzReader(file).use { reader ->
                    reader.forEachLine { line ->
                        if (line.isBlank()) return@forEachLine
                        val tab = line.indexOf('\t')
                        if (tab <= 0) return@forEachLine
                        val ip = line.substring(0, tab).trim()
                        val cc = if (tab + 1 < line.length) line.substring(tab + 1).trim() else ""
                        if (ip.isNotEmpty() && !merged.containsKey(ip)) merged[ip] = cc
                    }
                }
            } catch (_: Exception) {
                // Skip an unreadable provider file but keep the others.
            }
        }
        if (merged.isEmpty()) return null

        val countries = HashMap<String, Int>()
        val tmp = File(dir, "$CACHE_FILE.tmp")
        GZIPOutputStream(FileOutputStream(tmp)).buffered().use { gz ->
            val writer = gz.writer()
            for ((ip, cc) in merged) {
                writeEntry(writer, ip, cc)
                val key = cc.ifEmpty { "??" }
                countries[key] = (countries[key] ?: 0) + 1
            }
            writer.flush()
        }

        if (cacheFile.exists()) cacheFile.delete()
        if (!tmp.renameTo(cacheFile)) {
            tmp.copyTo(cacheFile, overwrite = true)
            tmp.delete()
        }

        val meta = Meta(
            fetchedAt = System.currentTimeMillis(),
            total = merged.size,
            countries = countries,
            source = sourceIds.joinToString(",")
        )
        persistMeta(meta)
        return meta
    }

    private fun writeEntry(writer: java.io.Writer, ip: String, cc: String) {
        writer.write(ip)
        writer.write('\t'.code)
        writer.write(cc)
        writer.write('\n'.code)
    }

    private fun isUsable(ip: String, error: String): Boolean {
        if (ip.isEmpty() || error.isNotEmpty()) return false
        // Keep only things that look like an IPv4 or IPv6 address.
        return ip.count { it == '.' } == 3 || ip.contains(':')
    }

    private fun openGzReader(file: File): BufferedReader {
        return GZIPInputStream(FileInputStream(file)).bufferedReader(Charsets.UTF_8)
    }

    private fun persistMeta(meta: Meta) {
        val countriesObj = JSONObject()
        for ((cc, count) in meta.countries) countriesObj.put(cc, count)
        val obj = JSONObject().apply {
            put("fetchedAt", meta.fetchedAt)
            put("total", meta.total)
            put("source", meta.source)
            put("countries", countriesObj)
        }
        metaFile.writeText(obj.toString())
    }

    private fun splitCsvLine(line: String): List<String> {
        val fields = ArrayList<String>()
        val current = StringBuilder()
        var inQuotes = false
        var i = 0
        while (i < line.length) {
            val c = line[i]
            when {
                c == '"' -> {
                    if (inQuotes && i + 1 < line.length && line[i + 1] == '"') {
                        current.append('"')
                        i++
                    } else {
                        inQuotes = !inQuotes
                    }
                }
                c == ',' && !inQuotes -> {
                    fields.add(current.toString())
                    current.setLength(0)
                }
                else -> current.append(c)
            }
            i++
        }
        fields.add(current.toString())
        return fields
    }

    companion object {
        private const val CACHE_DIR = "public_dns"
        private const val PROVIDER_DIR = "providers"
        private const val CACHE_FILE = "nameservers.tsv.gz"
        private const val META_FILE = "meta.json"
        private const val USER_AGENT = "MasterDnsVPN-Android"

        private const val FULL_CSV_URL = "https://public-dns.info/nameservers.csv"
        private const val FULL_JSON_URL = "https://public-dns.info/nameservers.json"
        private const val FULL_TXT_URL = "https://public-dns.info/nameservers.txt"

        private const val PUBLICDNSSERVER_DOWNLOAD = "https://publicdnsserver.com/download"

        // publicdnsserver.com country slug -> ISO country code used by the cache.
        private val PUBLICDNSSERVER_COUNTRIES = linkedMapOf(
            "RU" to "russia"
        )

        const val ALL_COUNTRIES = "ALL"

        @Volatile
        private var instance: PublicDnsRepository? = null

        fun get(context: Context): PublicDnsRepository {
            return instance ?: synchronized(this) {
                instance ?: PublicDnsRepository(context.applicationContext).also { instance = it }
            }
        }
    }
}
