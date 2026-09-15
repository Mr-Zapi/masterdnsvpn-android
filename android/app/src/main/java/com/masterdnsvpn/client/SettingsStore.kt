package com.masterdnsvpn.client

import android.content.SharedPreferences
import android.util.Base64
import org.json.JSONObject

/**
 * Tunnel tuning the user can change from the UI.
 *
 * Window size and batch size are deliberately left out: they were measured not
 * to bind (or are clamped by the server), so exposing them would only invite
 * people to slow themselves down.
 */
data class TunnelSettings(
    /**
     * Outstanding tunnel queries. Download throughput is
     * (queries/sec x bytes-per-answer) and queries/sec = depth / RTT, so this
     * is the main lever: 192 measured ~14-20 Mbit/s, 320 measured ~40 Mbit/s.
     * When [adaptive] is on this acts as the ceiling instead of a fixed value.
     */
    var depth: Int = DEFAULT_DEPTH,
    var adaptive: Boolean = true,
    /** 1 on a clean link; 2-3 trades bandwidth for resilience on a lossy one. */
    var duplication: Int = 1,
    /** ZSTD both ways. Measured +29% on compressible payloads, free otherwise. */
    var compression: Boolean = true,
    /**
     * RX/TX worker count (each owns its own UDP socket). The client derives the
     * processor count from this, so that one is not exposed separately.
     */
    var workers: Int = DEFAULT_WORKERS,
    /**
     * Floor for the download-MTU probe. The session's download MTU is the
     * MINIMUM across active resolvers, so one resolver that truncates large DNS
     * answers drags the whole tunnel down to its size. Raising this floor makes
     * the probe REJECT such resolvers instead of adapting to the worst one.
     */
    var minDownloadMtu: Int = DEFAULT_MIN_DOWNLOAD_MTU,
) {
    companion object {
        const val DEFAULT_DEPTH = 320
        const val MIN_DEPTH = 16
        const val MAX_DEPTH = 512
        const val MIN_DUPLICATION = 1
        const val MAX_DUPLICATION = 10
        const val DEFAULT_WORKERS = 8
        const val MIN_WORKERS = 1
        const val MAX_WORKERS = 128
        const val DEFAULT_MIN_DOWNLOAD_MTU = 1024
        const val MIN_MIN_DOWNLOAD_MTU = 100
        const val MAX_MIN_DOWNLOAD_MTU = 3500
    }
}

class SettingsStore(private val prefs: SharedPreferences) {

    fun load(): TunnelSettings = TunnelSettings(
        depth = clampDepth(prefs.getInt(KEY_DEPTH, TunnelSettings.DEFAULT_DEPTH)),
        adaptive = prefs.getBoolean(KEY_ADAPTIVE, true),
        duplication = clampDuplication(prefs.getInt(KEY_DUPLICATION, 1)),
        compression = prefs.getBoolean(KEY_COMPRESSION, true),
        workers = clampWorkers(prefs.getInt(KEY_WORKERS, TunnelSettings.DEFAULT_WORKERS)),
        minDownloadMtu = clampMinDownloadMtu(
            prefs.getInt(KEY_MIN_DOWNLOAD_MTU, TunnelSettings.DEFAULT_MIN_DOWNLOAD_MTU)
        ),
    )

    fun save(s: TunnelSettings) {
        prefs.edit()
            .putInt(KEY_DEPTH, clampDepth(s.depth))
            .putBoolean(KEY_ADAPTIVE, s.adaptive)
            .putInt(KEY_DUPLICATION, clampDuplication(s.duplication))
            .putBoolean(KEY_COMPRESSION, s.compression)
            .putInt(KEY_WORKERS, clampWorkers(s.workers))
            .putInt(KEY_MIN_DOWNLOAD_MTU, clampMinDownloadMtu(s.minDownloadMtu))
            .apply()
    }

    fun clampDepth(v: Int): Int =
        v.coerceIn(TunnelSettings.MIN_DEPTH, TunnelSettings.MAX_DEPTH)

    fun clampDuplication(v: Int): Int =
        v.coerceIn(TunnelSettings.MIN_DUPLICATION, TunnelSettings.MAX_DUPLICATION)

    fun clampWorkers(v: Int): Int =
        v.coerceIn(TunnelSettings.MIN_WORKERS, TunnelSettings.MAX_WORKERS)

    fun clampMinDownloadMtu(v: Int): Int =
        v.coerceIn(TunnelSettings.MIN_MIN_DOWNLOAD_MTU, TunnelSettings.MAX_MIN_DOWNLOAD_MTU)

    /**
     * Merges [settings] into the user's base64 JSON config and re-encodes it.
     *
     * The pasted config only has to carry identity (domain, key, encryption
     * method); everything tunable is overwritten from the UI here, so changing
     * a knob never means re-pasting a base64 blob.
     *
     * Returns the input unchanged if it is not decodable JSON - the Go core
     * will report the real error, which is a better failure than silently
     * connecting with something the user did not paste.
     */
    fun applyTo(configB64: String, settings: TunnelSettings): String {
        val trimmed = configB64.trim()
        if (trimmed.isEmpty()) return configB64

        return try {
            val json = JSONObject(String(Base64.decode(trimmed, Base64.DEFAULT), Charsets.UTF_8))

            json.put("PING_INFLIGHT_TARGET", clampDepth(settings.depth))
            json.put("PING_INFLIGHT_ADAPTIVE", settings.adaptive)
            json.put("PACKET_DUPLICATION_COUNT", clampDuplication(settings.duplication))
            json.put("RX_TX_WORKERS", clampWorkers(settings.workers))
            json.put("MIN_DOWNLOAD_MTU", clampMinDownloadMtu(settings.minDownloadMtu))
            json.put("MAX_DOWNLOAD_MTU", 4096)

            val compression = if (settings.compression) 1 else 0
            json.put("UPLOAD_COMPRESSION_TYPE", compression)
            json.put("DOWNLOAD_COMPRESSION_TYPE", compression)

            Base64.encodeToString(json.toString().toByteArray(Charsets.UTF_8), Base64.NO_WRAP)
        } catch (e: Exception) {
            configB64
        }
    }

    private companion object {
        const val KEY_DEPTH = "tune_depth"
        const val KEY_ADAPTIVE = "tune_adaptive"
        const val KEY_DUPLICATION = "tune_duplication"
        const val KEY_COMPRESSION = "tune_compression"
        const val KEY_WORKERS = "tune_workers"
        const val KEY_MIN_DOWNLOAD_MTU = "tune_min_download_mtu"
    }
}
