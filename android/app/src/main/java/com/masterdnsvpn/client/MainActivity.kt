package com.masterdnsvpn.client

import android.content.Context
import android.content.Intent
import android.net.VpnService
import android.os.Bundle
import android.os.Handler
import android.os.Looper
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
    private lateinit var resolversInput: EditText
    private lateinit var powerButton: FrameLayout
    private lateinit var powerIcon: ImageView
    private lateinit var glow: android.view.View
    private lateinit var statusText: TextView
    private lateinit var hintText: TextView

    private val prefs by lazy { getSharedPreferences("masterdnsvpn", Context.MODE_PRIVATE) }
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

    companion object {
        private const val DEFAULT_RESOLVERS = "77.88.8.8:53\n77.88.8.1:53"
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_main)

        configInput = findViewById(R.id.configInput)
        resolversInput = findViewById(R.id.resolversInput)
        powerButton = findViewById(R.id.powerButton)
        powerIcon = findViewById(R.id.powerIcon)
        glow = findViewById(R.id.glow)
        statusText = findViewById(R.id.statusText)
        hintText = findViewById(R.id.hintText)

        configInput.setText(prefs.getString("config_b64", ""))
        resolversInput.setText(prefs.getString("resolvers", DEFAULT_RESOLVERS))

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
            connecting = true
            refreshUi()
            val intent = VpnService.prepare(this)
            if (intent != null) vpnPermissionLauncher.launch(intent) else startVpn()
        }
    }

    private fun saveInputs() {
        prefs.edit()
            .putString("config_b64", configInput.text.toString().trim())
            .putString("resolvers", resolversInput.text.toString())
            .apply()
    }

    private fun startVpn() {
        val intent = Intent(this, MasterDnsVpnService::class.java)
        intent.action = MasterDnsVpnService.ACTION_CONNECT
        intent.putExtra(MasterDnsVpnService.EXTRA_CONFIG_B64, configInput.text.toString().trim())
        intent.putExtra(MasterDnsVpnService.EXTRA_RESOLVERS, resolversInput.text.toString())
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
