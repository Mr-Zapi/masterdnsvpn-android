package com.masterdnsvpn.client

import android.content.Context
import android.os.Bundle
import android.view.View
import android.widget.ArrayAdapter
import android.widget.Button
import android.widget.CheckBox
import android.widget.EditText
import android.widget.ListView
import android.widget.ProgressBar
import android.widget.Spinner
import android.widget.TextView
import android.widget.Toast
import androidx.appcompat.app.AppCompatActivity

/**
 * Scans public DNS servers for resolvers that can carry the MasterDnsVPN
 * protocol, then lets the user save the working ones as a named list.
 */
class ScanActivity : AppCompatActivity(), DnsScanner.Listener {

    private lateinit var repo: PublicDnsRepository
    private lateinit var store: DnsListStore

    private lateinit var summaryText: TextView
    private lateinit var countrySpinner: Spinner
    private lateinit var fullMtuCheck: CheckBox
    private lateinit var packetsPerSecondInput: EditText
    private lateinit var maxSaveInput: EditText
    private lateinit var scanProgressBar: ProgressBar
    private lateinit var startButton: Button
    private lateinit var cancelButton: Button
    private lateinit var progressText: TextView
    private lateinit var resultsView: ListView
    private lateinit var saveButton: Button

    private val scanner = DnsScanner()
    private val prefs by lazy { getSharedPreferences("masterdnsvpn", Context.MODE_PRIVATE) }
    private val resultsAdapter by lazy { ArrayAdapter<String>(this, android.R.layout.simple_list_item_1) }

    private var lastReport: DnsScanner.Report? = null
    private var candidateCount = 0

    // When set, the scan is a "rescan" of an existing list: the save dialog is
    // pre-filled with that list's name and saving updates the list in place.
    private var rescanListId: String? = null

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_scan)

        repo = PublicDnsRepository.get(this)
        store = DnsListStore.get(this)

        summaryText = findViewById(R.id.summaryText)
        countrySpinner = findViewById(R.id.countrySpinner)
        fullMtuCheck = findViewById(R.id.fullMtuCheck)
        packetsPerSecondInput = findViewById(R.id.packetsPerSecondInput)
        maxSaveInput = findViewById(R.id.maxSaveInput)
        scanProgressBar = findViewById(R.id.scanProgressBar)
        startButton = findViewById(R.id.startButton)
        cancelButton = findViewById(R.id.cancelButton)
        progressText = findViewById(R.id.progressText)
        resultsView = findViewById(R.id.resultsView)
        saveButton = findViewById(R.id.saveButton)

        rescanListId = intent.getStringExtra(EXTRA_LIST_ID)

        findViewById<TextView>(R.id.backButton).setOnClickListener { finish() }
        resultsView.adapter = resultsAdapter
        cancelButton.isEnabled = false
        saveButton.isEnabled = false

        packetsPerSecondInput.setText(prefs.getString(PREF_MAX_PPS, DEFAULT_MAX_PPS.toString()))
        maxSaveInput.setText(prefs.getString(PREF_MAX_SAVE, DEFAULT_MAX_SAVE.toString()))
        fullMtuCheck.isChecked = prefs.getBoolean(PREF_FULL_MTU, true)

        store.get(rescanListId)?.let { list ->
            saveButton.text = "Save results to \"${list.name}\""
        }

        startButton.setOnClickListener { startScan() }
        cancelButton.setOnClickListener {
            scanner.stop()
            progressText.text = "Stopping..."
        }
        saveButton.setOnClickListener { promptSave() }

        loadCountries()
    }

    private fun loadCountries() {
        val countries = ArrayList<String>()
        countries.add(PublicDnsRepository.ALL_COUNTRIES)
        countries.addAll(repo.availableCountries())

        val adapter = ArrayAdapter(this, android.R.layout.simple_spinner_item, countries)
        adapter.setDropDownViewResource(android.R.layout.simple_spinner_dropdown_item)
        countrySpinner.adapter = adapter

        updateSummary()
        if (countries.size <= 1) {
            progressText.text = "No public DNS cache yet. Tap \"Refresh public list\" on the lists screen."
        }
    }

    private fun updateSummary() {
        val meta = repo.cachedMeta()
        summaryText.text = if (meta == null) {
            "No cached public DNS list"
        } else {
            "Cache: ${meta.total} servers from ${meta.countries.size} countries"
        }
    }

    private fun startScan() {
        val config = prefs.getString("config_b64", "").orEmpty()
        if (config.isBlank()) {
            Toast.makeText(this, "Paste your base64 config on the main screen first", Toast.LENGTH_LONG).show()
            return
        }

        val country = countrySpinner.selectedItem?.toString()
        val candidates = repo.loadCandidates(country)
        if (candidates.isEmpty()) {
            Toast.makeText(
                this,
                "No candidates. Refresh the public DNS list first or try the cached list.",
                Toast.LENGTH_LONG
            ).show()
            return
        }

        lastReport = null
        resultsAdapter.clear()
        saveButton.isEnabled = false
        startButton.isEnabled = false
        cancelButton.isEnabled = true
        candidateCount = candidates.size
        progressText.text = "Phase 1/2: finding live servers \u2022 0/${candidates.size}"
        scanProgressBar.max = candidates.size
        scanProgressBar.progress = 0

        val maxPps = packetsPerSecondInput.text.toString().trim().toIntOrNull()
            ?.coerceIn(MIN_MAX_PPS, MAX_MAX_PPS) ?: DEFAULT_MAX_PPS
        val maxSave = maxSaveInput.text.toString().trim().toIntOrNull()?.coerceAtLeast(0) ?: 0
        prefs.edit()
            .putString(PREF_MAX_PPS, maxPps.toString())
            .putString(PREF_MAX_SAVE, maxSave.toString())
            .putBoolean(PREF_FULL_MTU, fullMtuCheck.isChecked)
            .apply()

        scanner.start(
            filesDir = filesDir.absolutePath,
            configB64 = config,
            candidates = candidates,
            maxPps = maxPps,
            timeoutSec = DEFAULT_TIMEOUT_SEC,
            fullMtu = fullMtuCheck.isChecked,
            listener = this
        )
    }

    override fun onProgress(progress: DnsScanner.Progress) {
        if (progress.phase == 2) {
            scanProgressBar.max = progress.mtuTotal.coerceAtLeast(1)
            scanProgressBar.progress = progress.mtuDone
            progressText.text =
                "Phase 2/2: MTU on live servers \u2022 ${progress.mtuDone}/${progress.mtuTotal}"
        } else {
            scanProgressBar.max = candidateCount.coerceAtLeast(1)
            scanProgressBar.progress = progress.tested
            progressText.text =
                "Phase 1/2: finding live servers \u2022 tested ${progress.tested} \u2022 live ${progress.ok}"
        }
    }

    override fun onFinished(report: DnsScanner.Report) {
        lastReport = report
        startButton.isEnabled = true
        cancelButton.isEnabled = false
        scanProgressBar.progress = scanProgressBar.max

        val ok = report.results.filter { it.ok }.sortedBy { it.rttMs }
        resultsAdapter.clear()
        for (r in ok.take(MAX_DISPLAY)) {
            val detail = if (r.upMtu > 0) "  up ${r.upMtu}/${r.downMtu}" else ""
            resultsAdapter.add("${r.resolver}  \u2014  ${r.rttMs.toInt()} ms$detail")
        }
        if (ok.size > MAX_DISPLAY) {
            resultsAdapter.add("... and ${ok.size - MAX_DISPLAY} more")
        }
        resultsAdapter.notifyDataSetChanged()

        val prefix = if (report.cancelled) "Stopped. " else ""
        progressText.text = "${prefix}Tested ${report.tested}. Working: ${ok.size}"
        saveButton.isEnabled = ok.isNotEmpty()
    }

    override fun onError(message: String) {
        startButton.isEnabled = true
        cancelButton.isEnabled = false
        progressText.text = "Scan failed: $message"
        Toast.makeText(this, message, Toast.LENGTH_LONG).show()
    }

    private fun promptSave() {
        val report = lastReport ?: return
        val working = report.okResolvers()
        if (working.isEmpty()) {
            Toast.makeText(this, "No working resolvers to save", Toast.LENGTH_SHORT).show()
            return
        }

        // 0 (default) saves every working resolver; otherwise the fastest N.
        val maxSave = maxSaveInput.text.toString().trim().toIntOrNull()?.coerceAtLeast(0) ?: 0
        val toSave = if (maxSave in 1 until working.size) working.take(maxSave) else working

        val existing = store.get(rescanListId)
        val defaultName = existing?.name
            ?: "Scan ${countrySpinner.selectedItem?.toString() ?: PublicDnsRepository.ALL_COUNTRIES}"
        val input = android.widget.EditText(this).apply {
            setText(defaultName)
            setSelection(defaultName.length)
        }
        val title = if (toSave.size < working.size) {
            "Save ${toSave.size} of ${working.size} resolvers"
        } else {
            "Save ${toSave.size} resolvers"
        }
        android.app.AlertDialog.Builder(this)
            .setTitle(title)
            .setView(input)
            .setPositiveButton("Save") { _, _ ->
                val name = input.text.toString()
                if (existing != null) {
                    store.updateServers(existing.id, toSave)
                    store.rename(existing.id, name)
                    Toast.makeText(this, "Updated \"$name\"", Toast.LENGTH_LONG).show()
                } else {
                    store.create(name, toSave, DnsList.SOURCE_SCAN)
                    Toast.makeText(this, "Saved. Activate it from the DNS lists screen.", Toast.LENGTH_LONG).show()
                }
                saveButton.isEnabled = false
            }
            .setNegativeButton("Cancel", null)
            .show()
    }

    override fun onDestroy() {
        scanner.shutdown()
        super.onDestroy()
    }

    companion object {
        const val EXTRA_LIST_ID = "list_id"

        private const val DEFAULT_MAX_PPS = 500
        private const val MIN_MAX_PPS = 1
        private const val MAX_MAX_PPS = 100_000
        private const val DEFAULT_TIMEOUT_SEC = 5.0
        private const val PREF_MAX_PPS = "scan_max_pps"
        private const val PREF_MAX_SAVE = "scan_max_save"
        private const val PREF_FULL_MTU = "scan_full_mtu"
        private const val DEFAULT_MAX_SAVE = 0
        private const val MAX_DISPLAY = 100
    }
}
