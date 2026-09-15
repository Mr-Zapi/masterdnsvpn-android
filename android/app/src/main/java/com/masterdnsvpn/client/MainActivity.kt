package com.masterdnsvpn.client

import android.Manifest
import android.annotation.SuppressLint
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.net.VpnService
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.os.PowerManager
import android.provider.Settings
import android.widget.Button
import android.widget.EditText
import android.widget.FrameLayout
import android.widget.ImageView
import android.widget.TextView
import android.widget.Toast
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AppCompatActivity
import androidx.core.content.ContextCompat
import mobile.Mobile

class MainActivity : AppCompatActivity() {

    private lateinit var configInput: EditText
    private lateinit var activeListText: TextView
    private lateinit var manageListsButton: Button
    private lateinit var uplinkPercentInput: EditText
    private lateinit var downlinkPercentInput: EditText
    private lateinit var splitHintText: TextView
    private lateinit var powerButton: FrameLayout
    private lateinit var powerIcon: ImageView
    private lateinit var glow: android.view.View
    private lateinit var statusText: TextView
    private lateinit var hintText: TextView
    private lateinit var batteryText: TextView

    private val prefs by lazy { getSharedPreferences("masterdnsvpn", Context.MODE_PRIVATE) }
    private lateinit var store: DnsListStore
    private val ui = Handler(Looper.getMainLooper())

    // True from the moment the user taps connect until the core reports running.
    @Volatile
    private var connecting = false

    private val poller = object : Runnable {
        override fun run() {
            refreshUi()
            ui.postDelayed(this, 800)
        }
    }

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

    private val notificationPermissionLauncher =
        registerForActivityResult(ActivityResultContracts.RequestPermission()) {
            // Best effort: continue even if the user denies it.
            maybeAskBatteryOptimization()
        }

    private val batteryOptimizationLauncher =
        registerForActivityResult(ActivityResultContracts.StartActivityForResult()) {
            // Result code is unreliable for this system dialog, so just re-check
            // the exemption state and continue regardless of the outcome.
            updateBatteryStatus()
            requestVpnConsent()
        }

    companion object {
        private const val DEFAULT_RESOLVERS = "77.88.8.8:53\n77.88.8.1:53"
        private const val PREF_BATTERY_PROMPTED = "battery_opt_prompted"
        private const val PREF_UPLINK_PERCENT = "uplink_percent"
        private const val PREF_DOWNLINK_PERCENT = "downlink_percent"
        private const val DEFAULT_UPLINK_PERCENT = 75
        private const val DEFAULT_DOWNLINK_PERCENT = 25
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_main)

        store = DnsListStore.get(this)
        store.seedFromLegacy(
            prefs.getString("resolvers", "").orEmpty(),
            DEFAULT_RESOLVERS.split('\n')
        )

        configInput = findViewById(R.id.configInput)
        activeListText = findViewById(R.id.activeListText)
        manageListsButton = findViewById(R.id.manageListsButton)
        uplinkPercentInput = findViewById(R.id.uplinkPercentInput)
        downlinkPercentInput = findViewById(R.id.downlinkPercentInput)
        splitHintText = findViewById(R.id.splitHintText)
        powerButton = findViewById(R.id.powerButton)
        powerIcon = findViewById(R.id.powerIcon)
        glow = findViewById(R.id.glow)
        statusText = findViewById(R.id.statusText)
        hintText = findViewById(R.id.hintText)
        batteryText = findViewById(R.id.batteryText)

        configInput.setText(prefs.getString("config_b64", ""))

        val up = prefs.getInt(PREF_UPLINK_PERCENT, DEFAULT_UPLINK_PERCENT)
        val down = prefs.getInt(PREF_DOWNLINK_PERCENT, DEFAULT_DOWNLINK_PERCENT)
        uplinkPercentInput.setText(up.toString())
        downlinkPercentInput.setText(down.toString())
        updateSplitHint(up, down)

        powerButton.setOnClickListener { onPowerTapped() }
        manageListsButton.setOnClickListener {
            startActivity(Intent(this, DnsListsActivity::class.java))
        }
        batteryText.setOnClickListener { onBatteryTapped() }
    }

    override fun onResume() {
        super.onResume()
        updateActiveList()
        updateBatteryStatus()
        val (up, down) = splitPercents()
        updateSplitHint(up, down)
        ui.post(poller)
    }

    override fun onPause() {
        super.onPause()
        ui.removeCallbacks(poller)
    }

    private fun updateActiveList() {
        val active = store.active()
        activeListText.text = if (active == null) {
            "No DNS list selected"
        } else {
            "${active.name}\n${active.servers.size} servers"
        }
    }

    private fun onPowerTapped() {
        if (isRunning()) {
            connecting = false
            val intent = Intent(this, MasterDnsVpnService::class.java)
            intent.action = MasterDnsVpnService.ACTION_DISCONNECT
            startService(intent)
        } else {
            saveInputs()
            if (configInput.text.toString().isBlank()) {
                Toast.makeText(this, "Paste your base64 config first", Toast.LENGTH_SHORT).show()
                return
            }
            if (store.active() == null) {
                Toast.makeText(this, "Create or select a DNS list first", Toast.LENGTH_SHORT).show()
                return
            }
            connecting = true
            refreshUi()
            beginConnect()
        }
    }

    private fun beginConnect() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU &&
            ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS) !=
            PackageManager.PERMISSION_GRANTED
        ) {
            // Ask first, then continue to the VPN consent in its result callback
            // so the two system dialogs never launch at the same time.
            notificationPermissionLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
            return
        }
        maybeAskBatteryOptimization()
    }

    private fun requestVpnConsent() {
        val intent = VpnService.prepare(this)
        if (intent != null) vpnPermissionLauncher.launch(intent) else startVpn()
    }

    private fun isIgnoringBatteryOptimizations(): Boolean {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.M) return true
        val pm = getSystemService(Context.POWER_SERVICE) as PowerManager
        return pm.isIgnoringBatteryOptimizations(packageName)
    }

    private fun maybeAskBatteryOptimization() {
        if (isIgnoringBatteryOptimizations() || prefs.getBoolean(PREF_BATTERY_PROMPTED, false)) {
            requestVpnConsent()
            return
        }
        // Ask once. If the user declines we still connect, but the BATTERY row
        // below stays visible so they can grant it later.
        prefs.edit().putBoolean(PREF_BATTERY_PROMPTED, true).apply()
        launchBatteryOptimizationRequest()
    }

    @SuppressLint("BatteryLife")
    private fun launchBatteryOptimizationRequest() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.M) {
            requestVpnConsent()
            return
        }
        try {
            val intent = Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS)
                .setData(Uri.parse("package:$packageName"))
            batteryOptimizationLauncher.launch(intent)
        } catch (e: Exception) {
            // Some OEMs do not expose this action; fall back to the settings list.
            try {
                startActivity(Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS))
            } catch (_: Exception) {
            }
            requestVpnConsent()
        }
    }

    private fun onBatteryTapped() {
        if (isIgnoringBatteryOptimizations()) {
            Toast.makeText(this, "Background access already allowed", Toast.LENGTH_SHORT).show()
            return
        }
        launchBatteryOptimizationRequest()
    }

    private fun updateBatteryStatus() {
        if (!::batteryText.isInitialized) return
        val accent = ContextCompat.getColor(this, R.color.accent)
        val warning = ContextCompat.getColor(this, R.color.warning)
        if (isIgnoringBatteryOptimizations()) {
            batteryText.text = "Background access allowed \u2013 tunnel stays alive in sleep"
            batteryText.setTextColor(accent)
        } else {
            batteryText.text = "Restricted \u2013 tap to stop the tunnel disconnecting in sleep"
            batteryText.setTextColor(warning)
        }
    }

    private fun saveInputs() {
        val (up, down) = splitPercents()
        prefs.edit()
            .putString("config_b64", configInput.text.toString().trim())
            .putInt(PREF_UPLINK_PERCENT, up)
            .putInt(PREF_DOWNLINK_PERCENT, down)
            .apply()
        uplinkPercentInput.setText(up.toString())
        downlinkPercentInput.setText(down.toString())
        updateSplitHint(up, down)
    }

    /**
     * Reads the uplink/downlink split, clamps both into 0..100 and normalizes
     * them to add up to 100. A (0, 0) entry falls back to the 75/25 default.
     */
    private fun splitPercents(): Pair<Int, Int> {
        var up = uplinkPercentInput.text.toString().trim().toIntOrNull() ?: DEFAULT_UPLINK_PERCENT
        var down = downlinkPercentInput.text.toString().trim().toIntOrNull() ?: DEFAULT_DOWNLINK_PERCENT
        up = up.coerceIn(0, 100)
        down = down.coerceIn(0, 100)
        when {
            up == 0 && down == 0 -> {
                up = DEFAULT_UPLINK_PERCENT
                down = DEFAULT_DOWNLINK_PERCENT
            }
            down == 0 -> up = 100
            up == 0 -> up = 100 - down
            up + down != 100 -> {
                down = down * 100 / (up + down)
                up = 100 - down
            }
        }
        return up to down
    }

    private fun updateSplitHint(up: Int, down: Int) {
        if (!::splitHintText.isInitialized) return
        splitHintText.text = "$up% upload / $down% download (disjoint pools)"
    }

    private fun startVpn() {
        val servers = store.serversText(store.active())
        val (up, down) = splitPercents()
        val intent = Intent(this, MasterDnsVpnService::class.java)
        intent.action = MasterDnsVpnService.ACTION_CONNECT
        intent.putExtra(MasterDnsVpnService.EXTRA_CONFIG_B64, configInput.text.toString().trim())
        intent.putExtra(MasterDnsVpnService.EXTRA_RESOLVERS, servers)
        intent.putExtra(MasterDnsVpnService.EXTRA_UPLINK_PERCENT, up)
        intent.putExtra(MasterDnsVpnService.EXTRA_DOWNLINK_PERCENT, down)
        ContextCompat.startForegroundService(this, intent)
        refreshUi()
    }

    private fun isRunning(): Boolean = try {
        Mobile.isRunning()
    } catch (e: Exception) {
        false
    }

    private fun refreshUi() {
        val startError = prefs.getString(MasterDnsVpnService.PREF_LAST_ERROR, null)
        if (startError != null) {
            prefs.edit().remove(MasterDnsVpnService.PREF_LAST_ERROR).apply()
            connecting = false
            Toast.makeText(this, "VPN failed: $startError", Toast.LENGTH_LONG).show()
        }

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
                statusText.text = "Connecting\u2026"
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
