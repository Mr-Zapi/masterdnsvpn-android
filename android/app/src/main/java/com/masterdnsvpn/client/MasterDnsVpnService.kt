package com.masterdnsvpn.client

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.SharedPreferences
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.net.VpnService
import android.os.Build
import android.os.Handler
import android.os.Looper
import android.os.ParcelFileDescriptor
import android.os.PowerManager
import android.util.Log
import mobile.Mobile
import mobile.Protector

/**
 * VpnService that establishes a TUN interface, hands its file descriptor to the
 * Go core (mobile.aar), and routes all device traffic through the DNS tunnel.
 */
class MasterDnsVpnService : VpnService() {

    companion object {
        const val ACTION_CONNECT = "com.masterdnsvpn.client.CONNECT"
        const val ACTION_DISCONNECT = "com.masterdnsvpn.client.DISCONNECT"

        const val EXTRA_CONFIG_B64 = "config_b64"
        const val EXTRA_RESOLVERS = "resolvers"
        const val EXTRA_UPLINK_PERCENT = "uplink_percent"
        const val EXTRA_DOWNLINK_PERCENT = "downlink_percent"
        const val EXTRA_RESOLVER_POOL_SIZE = "resolver_pool_size"

        // Default directional split: 75% of resolvers carry upload/control
        // traffic, 25% are reserved for download pulling.
        const val DEFAULT_UPLINK_PERCENT = 75
        const val DEFAULT_DOWNLINK_PERCENT = 25
        // Default cap on active resolvers (0 = no cap).
        const val DEFAULT_RESOLVER_POOL_SIZE = 32

        private const val TAG = "MasterDnsVPN"
        private const val MTU = 1500
        private const val SOCKS_PORT = 18000
        private const val CHANNEL_ID = "masterdnsvpn"
        private const val NOTIF_ID = 1
        private const val PREFS_NAME = "masterdnsvpn"
        const val PREF_LAST_ERROR = "vpn_last_error"

        // Persisted so a sticky restart (or a redelivered intent that arrives
        // with no extras) can bring the tunnel back up without the Activity.
        private const val PREF_CONFIG_B64 = "vpn_config_b64"
        private const val PREF_RESOLVERS = "vpn_resolvers"
        private const val PREF_UPLINK_PERCENT = "vpn_uplink_percent"
        private const val PREF_DOWNLINK_PERCENT = "vpn_downlink_percent"
        private const val PREF_RESOLVER_POOL_SIZE = "vpn_resolver_pool_size"

        // DNS servers used to resolve names DIRECTLY (over a VPN-protected
        // socket), bypassing the DNS tunnel. Yandex DNS stays reachable in
        // Russia during blocking. Empty string = resolve through the tunnel.
        private const val DIRECT_DNS = "77.88.8.8,77.88.8.1"
    }

    // Raw tun fd, owned by the Go core once started. -1 when not established.
    @Volatile
    private var tunFd: Int = -1

    // Keeps the CPU awake during transfers; Android doze otherwise throttles the
    // tunnel when the screen is off. Note: Doze ignores wake locks, so this only
    // covers the window between screen-off and full Doze; the battery
    // optimization exemption is what actually keeps the tunnel alive.
    private var wakeLock: PowerManager.WakeLock? = null

    private var networkCallback: ConnectivityManager.NetworkCallback? = null

    // Set when the underlying (non-VPN) network disappears, so the matching
    // onAvailable is treated as "connectivity is back" rather than the initial
    // registration callback.
    @Volatile
    private var underlyingNetworkLost = false

    private val mainHandler = Handler(Looper.getMainLooper())

    override fun onCreate() {
        super.onCreate()
        registerNetworkCallback()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_DISCONNECT) {
            stopTunnel()
            return START_NOT_STICKY
        }

        // Always enter the foreground immediately: the system kills services
        // that fail to do so within its startForegroundService() deadline, even
        // when we are about to bail out for a missing config.
        startForeground(NOTIF_ID, buildNotification("Connecting..."))

        if (Mobile.isRunning()) {
            Log.w(TAG, "already running")
            return START_REDELIVER_INTENT
        }

        // Prefer fresh extras, but fall back to the persisted copy so a
        // redelivered intent (or a process-death restart with a null intent)
        // still has the configuration.
        val configB64 = intent?.getStringExtra(EXTRA_CONFIG_B64)
            ?.takeIf { it.isNotBlank() }
            ?: prefs().getString(PREF_CONFIG_B64, "").orEmpty()
        val resolvers = intent?.getStringExtra(EXTRA_RESOLVERS)
            ?.takeIf { it.isNotBlank() }
            ?: prefs().getString(PREF_RESOLVERS, "").orEmpty()

        val uplinkDefault = prefs().getInt(PREF_UPLINK_PERCENT, DEFAULT_UPLINK_PERCENT)
        val downlinkDefault = prefs().getInt(PREF_DOWNLINK_PERCENT, DEFAULT_DOWNLINK_PERCENT)
        val poolDefault = prefs().getInt(PREF_RESOLVER_POOL_SIZE, DEFAULT_RESOLVER_POOL_SIZE)
        val uplinkPercent = intent?.getIntExtra(EXTRA_UPLINK_PERCENT, uplinkDefault) ?: uplinkDefault
        val downlinkPercent = intent?.getIntExtra(EXTRA_DOWNLINK_PERCENT, downlinkDefault) ?: downlinkDefault
        val resolverPoolSize = intent?.getIntExtra(EXTRA_RESOLVER_POOL_SIZE, poolDefault) ?: poolDefault

        if (configB64.isBlank() || resolvers.isBlank()) {
            Log.e(TAG, "missing config or resolvers; cannot start")
            setLastError("missing configuration")
            stopTunnel()
            return START_NOT_STICKY
        }

        startTunnel(configB64, resolvers, uplinkPercent, downlinkPercent, resolverPoolSize)
        // Redeliver the CONNECT intent if the process is killed, so the tunnel
        // comes back up automatically instead of dying silently.
        return START_REDELIVER_INTENT
    }

    private fun startTunnel(configB64: String, resolvers: String, uplinkPercent: Int, downlinkPercent: Int, resolverPoolSize: Int) {
        stopping = false
        clearLastError()
        if (Mobile.isRunning()) {
            Log.w(TAG, "already running")
            return
        }

        // Persist for sticky/redelivered restarts.
        prefs().edit()
            .putString(PREF_CONFIG_B64, configB64)
            .putString(PREF_RESOLVERS, resolvers)
            .putInt(PREF_UPLINK_PERCENT, uplinkPercent)
            .putInt(PREF_DOWNLINK_PERCENT, downlinkPercent)
            .putInt(PREF_RESOLVER_POOL_SIZE, resolverPoolSize)
            .apply()

        acquireWakeLock()

        val builder = Builder()
            .setSession("MasterDnsVPN")
            .setMtu(MTU)
            .addAddress("10.0.0.2", 32)
            .addRoute("0.0.0.0", 0)          // capture all IPv4 traffic
            // Advertise a private, in-tunnel DNS IP. A public resolver here (e.g.
            // 1.1.1.1) makes Android's opportunistic Private DNS validate DoT
            // against it and then tunnel slow/broken TLS on :853. A private IP
            // fails DoT validation, so netd falls back to cleartext UDP :53,
            // which our SOCKS5 UDP handler resolves directly via Yandex.
            .addDnsServer("10.0.0.1")
            .setBlocking(false)

        // Do not tunnel our own app (avoids a routing loop for the DNS queries).
        try {
            builder.addDisallowedApplication(packageName)
        } catch (e: Exception) {
            Log.w(TAG, "addDisallowedApplication failed", e)
        }

        val tun = try {
            builder.establish()
        } catch (e: Exception) {
            Log.e(TAG, "establish failed", e)
            null
        }

        if (tun == null) {
            Log.e(TAG, "TUN interface is null")
            setLastError("could not establish the VPN interface")
            stopTunnel()
            return
        }

        // Transfer fd ownership to the Go core. If Java's ParcelFileDescriptor
        // and Go both own the same fd, Android's fdsan aborts the process
        // (SIGABRT) when the fd is closed twice on Disconnect. detachFd() makes
        // Go the sole owner; the tun2socks engine closes it on Mobile.stop().
        val fd = tun.detachFd()
        tunFd = fd

        Thread {
            try {
                Mobile.start(
                    fd.toLong(),
                    MTU.toLong(),
                    SOCKS_PORT.toLong(),
                    configB64,
                    resolvers,
                    DIRECT_DNS,
                    uplinkPercent.toLong(),
                    downlinkPercent.toLong(),
                    resolverPoolSize.toLong(),
                    filesDir.absolutePath,
                    // Protector: make direct-DNS sockets bypass the VPN so they
                    // don't loop back into tun2socks.
                    Protector { fd -> protect(fd.toInt()) }
                )
                updateNotification("Connected")
                Log.i(TAG, "tunnel started")
            } catch (e: Exception) {
                Log.e(TAG, "Mobile.start failed", e)
                setLastError(e.message ?: "failed to start the tunnel")
                stopTunnel()
            }
        }.start()
    }

    @Volatile
    private var stopping = false

    private fun stopTunnel() {
        if (stopping) return
        stopping = true
        releaseWakeLock()

        // Mobile.stop() drives tun2socks engine.Stop(), which blocks on the
        // netstack shutdown. Doing that on the main thread (onStartCommand runs
        // there) freezes the UI thread, so run teardown on a background thread.
        val fd = tunFd
        val wasRunning = try {
            Mobile.isRunning()
        } catch (e: Throwable) {
            false
        }
        tunFd = -1

        Thread {
            try {
                Mobile.stop()
            } catch (e: Throwable) {
                Log.w(TAG, "Mobile.stop failed", e)
            }
            // The Go engine owns the tun fd (via detachFd) and closes it in
            // Mobile.stop(). Only close it ourselves if the engine never took
            // ownership (e.g. Mobile.start failed before it started).
            if (!wasRunning && fd >= 0) {
                try {
                    ParcelFileDescriptor.adoptFd(fd).close()
                } catch (e: Throwable) {
                    Log.w(TAG, "close orphan tun fd failed", e)
                }
            }
            // Foreground/service lifecycle calls must run on the main thread.
            mainHandler.post {
                if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.N) {
                    stopForeground(STOP_FOREGROUND_REMOVE)
                } else {
                    @Suppress("DEPRECATION")
                    stopForeground(true)
                }
                stopSelf()
            }
        }.start()
    }

    override fun onDestroy() {
        unregisterNetworkCallback()
        stopTunnel()
        super.onDestroy()
    }

    // The system tears the TUN interface down on its own in some cases (another
    // VPN taking over, user revoking consent, ...). Without this the service and
    // the UI can stay stuck in a "Connected" state that no longer routes.
    override fun onRevoke() {
        Log.w(TAG, "VPN revoked by system; tearing down")
        setLastError("VPN was revoked by the system")
        stopTunnel()
        super.onRevoke()
    }

    private fun registerNetworkCallback() {
        if (networkCallback != null) return
        val cm = getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager
        val cb = object : ConnectivityManager.NetworkCallback() {
            override fun onLost(network: Network) {
                // Only reacts to the real underlying (non-VPN) network. The
                // app's own default network is the VPN, so a default-network
                // callback would never see the radio/Wi-Fi dropping.
                underlyingNetworkLost = true
            }

            override fun onAvailable(network: Network) {
                // Ignore the callback that fires immediately on registration;
                // only rebuild once the underlying network actually came back.
                if (!underlyingNetworkLost) return
                underlyingNetworkLost = false
                if (Mobile.isRunning()) {
                    Log.i(TAG, "underlying network available; requesting session refresh")
                    Thread {
                        try {
                            Mobile.notifyNetworkChanged()
                        } catch (e: Throwable) {
                            Log.w(TAG, "notifyNetworkChanged failed", e)
                        }
                    }.start()
                }
            }
        }
        val request = NetworkRequest.Builder()
            .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
            .addCapability(NetworkCapabilities.NET_CAPABILITY_NOT_VPN)
            .build()
        try {
            cm.registerNetworkCallback(request, cb)
            networkCallback = cb
        } catch (e: Throwable) {
            Log.w(TAG, "registerNetworkCallback failed", e)
        }
    }

    private fun unregisterNetworkCallback() {
        val cb = networkCallback ?: return
        networkCallback = null
        try {
            val cm = getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager
            cm.unregisterNetworkCallback(cb)
        } catch (e: Throwable) {
            Log.w(TAG, "unregisterNetworkCallback failed", e)
        }
    }

    private fun prefs(): SharedPreferences =
        getSharedPreferences(PREFS_NAME, MODE_PRIVATE)

    private fun buildNotification(text: String): Notification {
        val nm = notificationManager()
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            val channel = NotificationChannel(
                CHANNEL_ID, "MasterDnsVPN", NotificationManager.IMPORTANCE_LOW
            )
            nm.createNotificationChannel(channel)
        }

        val pi = PendingIntent.getActivity(
            this, 0,
            Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE
        )

        @Suppress("DEPRECATION")
        val builder = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            Notification.Builder(this, CHANNEL_ID)
        } else {
            Notification.Builder(this)
        }
        return builder
            .setContentTitle("MasterDnsVPN")
            .setContentText(text)
            .setSmallIcon(android.R.drawable.ic_dialog_info)
            .setContentIntent(pi)
            .setOngoing(true)
            .build()
    }

    private fun updateNotification(text: String) {
        notificationManager().notify(NOTIF_ID, buildNotification(text))
    }

    private fun notificationManager(): NotificationManager =
        getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager

    private fun acquireWakeLock() {
        if (wakeLock?.isHeld == true) return
        try {
            val pm = getSystemService(Context.POWER_SERVICE) as PowerManager
            wakeLock = pm.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "$TAG::tunnel").apply {
                setReferenceCounted(false)
                acquire()
            }
        } catch (e: Throwable) {
            Log.w(TAG, "acquireWakeLock failed", e)
        }
    }

    private fun releaseWakeLock() {
        val lock = wakeLock ?: return
        wakeLock = null
        try {
            if (lock.isHeld) lock.release()
        } catch (e: Throwable) {
            Log.w(TAG, "releaseWakeLock failed", e)
        }
    }

    private fun setLastError(message: String) {
        getSharedPreferences(PREFS_NAME, MODE_PRIVATE).edit()
            .putString(PREF_LAST_ERROR, message)
            .apply()
    }

    private fun clearLastError() {
        getSharedPreferences(PREFS_NAME, MODE_PRIVATE).edit()
            .remove(PREF_LAST_ERROR)
            .apply()
    }
}
