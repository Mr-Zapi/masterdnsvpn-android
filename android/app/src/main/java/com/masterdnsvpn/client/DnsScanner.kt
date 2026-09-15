package com.masterdnsvpn.client

import android.os.Handler
import android.os.Looper
import mobile.Mobile
import org.json.JSONArray
import org.json.JSONObject
import java.util.concurrent.ExecutorService
import java.util.concurrent.Executors
import java.util.concurrent.Future
import java.util.concurrent.atomic.AtomicBoolean

/**
 * Runs the native MasterDnsVPN resolver scan (Mobile.testResolvers) on a
 * background thread and reports progress/results on the main thread.
 *
 * The scan validates resolvers with the real tunnel protocol, so it needs a
 * valid base64 config and a public-dns.info/cached candidate list.
 */
class DnsScanner {

    data class ScanResult(
        val resolver: String,
        val domain: String,
        val ok: Boolean,
        val rttMs: Double,
        val upMtu: Int,
        val downMtu: Int,
        val reason: String
    )

    data class Report(
        val tested: Int,
        val ok: Int,
        val cancelled: Boolean,
        val results: List<ScanResult>
    ) {
        fun okResolvers(): List<String> =
            results.filter { it.ok }.sortedBy { it.rttMs }.map { it.resolver }
    }

    data class Progress(
        val tested: Int,
        val ok: Int,
        val running: Boolean,
        val phase: Int,
        val mtuDone: Int,
        val mtuTotal: Int
    )

    interface Listener {
        fun onProgress(progress: Progress)
        fun onFinished(report: Report)
        fun onError(message: String)
    }

    private val executor: ExecutorService = Executors.newSingleThreadExecutor { r ->
        Thread(r, "dns-scanner").apply { isDaemon = true }
    }
    private val main = Handler(Looper.getMainLooper())
    private val running = AtomicBoolean(false)
    private var job: Future<*>? = null

    private val progressPoller = object : Runnable {
        override fun run() {
            if (!running.get()) return
            try {
                listener?.onProgress(parseProgress(Mobile.scanProgress()))
            } catch (_: Throwable) {
                // Ignore transient progress errors.
            }
            if (running.get()) main.postDelayed(this, PROGRESS_INTERVAL_MS)
        }
    }

    @Volatile
    private var listener: Listener? = null

    fun isRunning(): Boolean = running.get()

    fun start(
        filesDir: String,
        configB64: String,
        candidates: List<String>,
        maxPps: Int,
        timeoutSec: Double,
        fullMtu: Boolean,
        listener: Listener
    ) {
        if (!running.compareAndSet(false, true)) {
            listener.onError("A scan is already running")
            return
        }
        this.listener = listener

        val candidatesText = candidates.joinToString("\n")
        main.post { listener.onProgress(Progress(0, 0, true, 1, 0, 0)) }
        main.post(progressPoller)

        job = executor.submit {
            try {
                val json = Mobile.testResolvers(
                    filesDir,
                    configB64,
                    candidatesText,
                    maxPps.toLong(),
                    timeoutSec,
                    fullMtu
                )
                val report = parseReport(json)
                running.set(false)
                main.post { listener.onFinished(report) }
            } catch (e: Throwable) {
                running.set(false)
                main.post { listener.onError(e.message ?: "scan failed") }
            } finally {
                this.listener = null
            }
        }
    }

    fun stop() {
        try {
            Mobile.cancelScan()
        } catch (_: Throwable) {
            // Ignore.
        }
    }

    fun shutdown() {
        stop()
        job?.cancel(true)
        executor.shutdownNow()
    }

    private fun parseProgress(json: String): Progress {
        val obj = JSONObject(json)
        return Progress(
            tested = obj.optInt("tested"),
            ok = obj.optInt("ok"),
            running = obj.optBoolean("running"),
            phase = obj.optInt("phase"),
            mtuDone = obj.optInt("mtuDone"),
            mtuTotal = obj.optInt("mtuTotal")
        )
    }

    private fun parseReport(json: String): Report {
        val root = JSONObject(json)
        val arr = root.optJSONArray("results") ?: JSONArray()
        val results = ArrayList<ScanResult>(arr.length())
        for (i in 0 until arr.length()) {
            val obj = arr.optJSONObject(i) ?: continue
            results.add(
                ScanResult(
                    resolver = obj.optString("resolver"),
                    domain = obj.optString("domain"),
                    ok = obj.optBoolean("ok"),
                    rttMs = obj.optDouble("rttMs", 0.0),
                    upMtu = obj.optInt("upMtu"),
                    downMtu = obj.optInt("downMtu"),
                    reason = obj.optString("reason")
                )
            )
        }
        return Report(
            tested = root.optInt("tested"),
            ok = root.optInt("ok"),
            cancelled = root.optBoolean("cancelled"),
            results = results
        )
    }

    companion object {
        private const val PROGRESS_INTERVAL_MS = 400L
    }
}
