package com.masterdnsvpn.client

import android.Manifest
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.VpnService
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.view.LayoutInflater
import android.view.ViewGroup
import android.widget.EditText
import android.widget.FrameLayout
import android.widget.ImageView
import android.widget.LinearLayout
import android.widget.TextView
import android.widget.Toast
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AlertDialog
import androidx.appcompat.app.AppCompatActivity
import androidx.appcompat.widget.SwitchCompat
import androidx.core.content.ContextCompat
import androidx.core.view.ViewCompat
import androidx.core.view.WindowInsetsCompat
import androidx.core.view.updatePadding
import android.graphics.Rect
import mobile.Mobile

class MainActivity : AppCompatActivity() {

    private lateinit var configInput: EditText
    private lateinit var powerButton: FrameLayout
    private lateinit var powerIcon: ImageView
    private lateinit var glow: android.view.View
    private lateinit var statusText: TextView
    private lateinit var hintText: TextView
    private lateinit var listsContainer: LinearLayout
    private lateinit var addListButton: TextView
    private lateinit var resolverSummary: TextView
    private lateinit var settingsButton: ImageView

    private val prefs by lazy { getSharedPreferences("masterdnsvpn", Context.MODE_PRIVATE) }
    private val store by lazy { ResolverStore(prefs) }
    private val settingsStore by lazy { SettingsStore(prefs) }
    private val ui = Handler(Looper.getMainLooper())

    private lateinit var lists: MutableList<ResolverList>
    private lateinit var settings: TunnelSettings

    // True from the moment the user taps connect until the core reports running.
    @Volatile
    private var connecting = false

    private val poller = object : Runnable {
        override fun run() {
            refreshUi()
            ui.postDelayed(this, 800)
        }
    }

    // Android 13+ needs POST_NOTIFICATIONS at runtime; without it the
    // foreground-service notification is silently never shown, which looks
    // exactly like "the tunnel never reports connected".
    private val notificationPermissionLauncher =
        registerForActivityResult(ActivityResultContracts.RequestPermission()) { }

    private val vpnPermissionLauncher =
        registerForActivityResult(ActivityResultContracts.StartActivityForResult()) { result ->
            if (result.resultCode == RESULT_OK) {
                startVpn()
            } else {
                connecting = false
                refreshUi()
                Toast.makeText(this, "VPN permission denied", Toast.LENGTH_SHORT).show()
            }
        }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_main)

        configInput = findViewById(R.id.configInput)
        powerButton = findViewById(R.id.powerButton)
        powerIcon = findViewById(R.id.powerIcon)
        glow = findViewById(R.id.glow)
        statusText = findViewById(R.id.statusText)
        hintText = findViewById(R.id.hintText)
        listsContainer = findViewById(R.id.resolverListsContainer)
        addListButton = findViewById(R.id.addListButton)
        resolverSummary = findViewById(R.id.resolverSummary)
        settingsButton = findViewById(R.id.settingsButton)

        // targetSdk 35 is edge-to-edge, where windowSoftInputMode="adjustResize"
        // no longer shrinks the window - the keyboard just covers the content.
        // Pad the scroll container by the IME height instead so the focused
        // field can be scrolled above it.
        val rootScroll = findViewById<android.view.View>(R.id.rootScroll)
        ViewCompat.setOnApplyWindowInsetsListener(rootScroll) { v, insets ->
            val ime = insets.getInsets(WindowInsetsCompat.Type.ime()).bottom
            val bars = insets.getInsets(WindowInsetsCompat.Type.systemBars()).bottom
            v.updatePadding(bottom = maxOf(ime, bars))
            insets
        }

        configInput.setText(prefs.getString("config_b64", ""))

        // Bring the field fully into view once the keyboard has opened.
        // requestRectangleOnScreen walks up to the scrolling parent, which is
        // what makes this correct for a field nested several layouts deep:
        // v.bottom alone is relative to the card, not to the scroll content.
        configInput.setOnFocusChangeListener { v, hasFocus ->
            if (hasFocus) {
                v.postDelayed({
                    v.requestRectangleOnScreen(Rect(0, 0, v.width, v.height), false)
                }, 250)
            }
        }

        lists = store.load()
        renderLists()

        settings = settingsStore.load()

        requestNotificationPermissionIfNeeded()

        settingsButton.setOnClickListener { showSettingsDialog() }
        addListButton.setOnClickListener { showListDialog(null) }
        powerButton.setOnClickListener { onPowerTapped() }
    }

    override fun onResume() {
        super.onResume()
        connecting = false
        ui.post(poller)
    }

    override fun onPause() {
        super.onPause()
        ui.removeCallbacks(poller)
        saveConfig()
    }

    private fun requestNotificationPermissionIfNeeded() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU) return
        val granted = ContextCompat.checkSelfPermission(
            this, Manifest.permission.POST_NOTIFICATIONS
        ) == PackageManager.PERMISSION_GRANTED
        if (!granted) {
            notificationPermissionLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
        }
    }

    // ------------------------------------------------------------- settings

    /**
     * Tuning lives behind the gear so the main screen stays a single button.
     * The controls exist only while the dialog is open, so everything is read
     * back and persisted when it is confirmed - the activity never holds
     * references to them.
     */
    private fun showSettingsDialog() {
        val view = LayoutInflater.from(this).inflate(R.layout.dialog_settings, null)

        val depthInput = view.findViewById<EditText>(R.id.depthInput)
        val adaptiveSwitch = view.findViewById<SwitchCompat>(R.id.adaptiveSwitch)
        val adaptiveHint = view.findViewById<TextView>(R.id.adaptiveHint)
        val minDownloadMtuInput = view.findViewById<EditText>(R.id.minDownloadMtuInput)
        val duplicationInput = view.findViewById<EditText>(R.id.duplicationInput)
        val workersInput = view.findViewById<EditText>(R.id.workersInput)
        val compressionSwitch = view.findViewById<SwitchCompat>(R.id.compressionSwitch)

        depthInput.setText(settings.depth.toString())
        minDownloadMtuInput.setText(settings.minDownloadMtu.toString())
        duplicationInput.setText(settings.duplication.toString())
        workersInput.setText(settings.workers.toString())
        adaptiveSwitch.isChecked = settings.adaptive
        compressionSwitch.isChecked = settings.compression

        fun renderAdaptiveHint(checked: Boolean) {
            adaptiveHint.text = if (checked) {
                "Значение выше — потолок"
            } else {
                "Значение выше — фиксировано"
            }
        }
        renderAdaptiveHint(adaptiveSwitch.isChecked)
        adaptiveSwitch.setOnCheckedChangeListener { _, checked -> renderAdaptiveHint(checked) }

        AlertDialog.Builder(this)
            .setTitle("Настройки туннеля")
            .setView(view)
            .setNegativeButton("Отмена", null)
            .setPositiveButton("Сохранить") { _, _ ->
                settings.depth = settingsStore.clampDepth(
                    depthInput.text.toString().trim().toIntOrNull() ?: settings.depth
                )
                settings.minDownloadMtu = settingsStore.clampMinDownloadMtu(
                    minDownloadMtuInput.text.toString().trim().toIntOrNull()
                        ?: settings.minDownloadMtu
                )
                settings.duplication = settingsStore.clampDuplication(
                    duplicationInput.text.toString().trim().toIntOrNull() ?: settings.duplication
                )
                settings.workers = settingsStore.clampWorkers(
                    workersInput.text.toString().trim().toIntOrNull() ?: settings.workers
                )
                settings.adaptive = adaptiveSwitch.isChecked
                settings.compression = compressionSwitch.isChecked

                settingsStore.save(settings)
                Toast.makeText(this, "Применится при следующем подключении", Toast.LENGTH_SHORT)
                    .show()
            }
            .show()
    }

    // ---------------------------------------------------------------- lists

    private fun renderLists() {
        listsContainer.removeAllViews()
        val inflater = LayoutInflater.from(this)

        for (list in lists) {
            val row = inflater.inflate(R.layout.item_resolver_list, listsContainer, false)
            val name = row.findViewById<TextView>(R.id.listName)
            val meta = row.findViewById<TextView>(R.id.listMeta)
            val active = row.findViewById<SwitchCompat>(R.id.listActive)
            val delete = row.findViewById<TextView>(R.id.listDelete)
            val body = row.findViewById<ViewGroup>(R.id.listRowBody)

            name.text = list.name
            meta.text = "${list.entries.size} IP"

            // Assign the state before wiring the listener so re-rendering rows
            // does not fire a spurious toggle.
            active.setOnCheckedChangeListener(null)
            active.isChecked = list.active
            active.setOnCheckedChangeListener { _, checked ->
                list.active = checked
                store.save(lists)
                updateSummary()
            }

            body.setOnClickListener { showListDialog(list) }
            delete.setOnClickListener { confirmDelete(list) }

            listsContainer.addView(row)
        }

        updateSummary()
    }

    private fun updateSummary() {
        val count = store.activeEntries(lists).size
        resolverSummary.text = if (count == 1) "1 active resolver" else "$count active resolvers"
    }

    /** [existing] null means "create a new list". */
    private fun showListDialog(existing: ResolverList?) {
        val view = LayoutInflater.from(this).inflate(R.layout.dialog_resolver_list, null)
        val nameField = view.findViewById<EditText>(R.id.dialogListName)
        val entriesField = view.findViewById<EditText>(R.id.dialogListEntries)

        if (existing != null) {
            nameField.setText(existing.name)
            entriesField.setText(existing.entries.joinToString("\n"))
        }

        AlertDialog.Builder(this)
            .setTitle(if (existing == null) "New list" else "Edit list")
            .setView(view)
            .setNegativeButton("Cancel", null)
            .setPositiveButton("Save") { _, _ ->
                val newName = nameField.text.toString().trim().ifBlank { "List" }
                val newEntries = store.parseEntries(entriesField.text.toString())

                if (newEntries.isEmpty()) {
                    Toast.makeText(this, "Add at least one IP", Toast.LENGTH_SHORT).show()
                    return@setPositiveButton
                }

                if (existing == null) {
                    lists.add(ResolverList(store.newId(), newName, newEntries, true))
                } else {
                    existing.name = newName
                    existing.entries = newEntries
                }

                store.save(lists)
                renderLists()
            }
            .show()
    }

    private fun confirmDelete(list: ResolverList) {
        AlertDialog.Builder(this)
            .setTitle("Delete \"${list.name}\"?")
            .setNegativeButton("Cancel", null)
            .setPositiveButton("Delete") { _, _ ->
                lists.remove(list)
                store.save(lists)
                renderLists()
            }
            .show()
    }

    // -------------------------------------------------------------- connect

    private fun onPowerTapped() {
        if (isRunning()) {
            connecting = false
            val intent = Intent(this, MasterDnsVpnService::class.java)
            intent.action = MasterDnsVpnService.ACTION_DISCONNECT
            startService(intent)
        } else {
            saveConfig()

            if (configInput.text.toString().isBlank()) {
                Toast.makeText(this, "Paste your base64 config first", Toast.LENGTH_SHORT).show()
                return
            }
            if (store.activeEntries(lists).isEmpty()) {
                Toast.makeText(this, "Enable at least one resolver list", Toast.LENGTH_SHORT).show()
                return
            }

            connecting = true
            refreshUi()
            val intent = VpnService.prepare(this)
            if (intent != null) vpnPermissionLauncher.launch(intent) else startVpn()
        }
    }

    private fun saveConfig() {
        prefs.edit().putString("config_b64", configInput.text.toString().trim()).apply()
    }

    private fun startVpn() {
        // The pasted config only carries identity; the tuning knobs are merged
        // in here so changing one never means re-pasting a base64 blob.
        val merged = settingsStore.applyTo(configInput.text.toString().trim(), settings)

        val intent = Intent(this, MasterDnsVpnService::class.java)
        intent.action = MasterDnsVpnService.ACTION_CONNECT
        intent.putExtra(MasterDnsVpnService.EXTRA_CONFIG_B64, merged)
        intent.putExtra(MasterDnsVpnService.EXTRA_RESOLVERS, store.activeResolversText(lists))
        startForegroundService(intent)
        refreshUi()
    }

    private fun isRunning(): Boolean = try {
        Mobile.isRunning()
    } catch (e: Exception) {
        false
    }

    private fun refreshUi() {
        val running = isRunning()
        if (running) connecting = false

        val accent = ContextCompat.getColor(this, R.color.accent)
        val secondary = ContextCompat.getColor(this, R.color.text_secondary)

        when {
            running -> {
                powerButton.setBackgroundResource(R.drawable.bg_power_on)
                powerIcon.setColorFilter(accent)
                glow.animate().alpha(0.5f).setDuration(400).start()
                statusText.text = "Connected"
                hintText.text = "Tap to disconnect"
            }
            connecting -> {
                powerButton.setBackgroundResource(R.drawable.bg_power_off)
                powerIcon.setColorFilter(accent)
                glow.animate().alpha(0.25f).setDuration(400).start()
                statusText.text = "Connecting…"
                hintText.text = "Establishing tunnel"
            }
            else -> {
                powerButton.setBackgroundResource(R.drawable.bg_power_off)
                powerIcon.setColorFilter(secondary)
                glow.animate().alpha(0f).setDuration(400).start()
                statusText.text = "Disconnected"
                hintText.text = "Tap to connect"
            }
        }
    }
}
