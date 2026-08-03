package com.masterdnsvpn.client

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Intent
import android.net.VpnService
import android.os.Build
import android.os.Handler
import android.os.Looper
import android.os.ParcelFileDescriptor
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

        private const val TAG = "MasterDnsVPN"
        private const val MTU = 1500
        private const val SOCKS_PORT = 18000
        private const val CHANNEL_ID = "masterdnsvpn"
        private const val NOTIF_ID = 1

        // DNS servers used to resolve names DIRECTLY (over a VPN-protected
        // socket), bypassing the DNS tunnel. Yandex DNS stays reachable in
        // Russia during blocking. Empty string = resolve through the tunnel.
        private const val DIRECT_DNS = "77.88.8.8,77.88.8.1"
    }

    // Raw tun fd, owned by the Go core once started. -1 when not established.
    @Volatile
    private var tunFd: Int = -1

    private val mainHandler = Handler(Looper.getMainLooper())

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_DISCONNECT -> {
                stopTunnel()
                return START_NOT_STICKY
            }
            else -> {
                val configB64 = intent?.getStringExtra(EXTRA_CONFIG_B64).orEmpty()
                val resolvers = intent?.getStringExtra(EXTRA_RESOLVERS).orEmpty()
                startTunnel(configB64, resolvers)
                return START_STICKY
            }
        }
    }

    private fun startTunnel(configB64: String, resolvers: String) {
        stopping = false
        if (Mobile.isRunning()) {
            Log.w(TAG, "already running")
            return
        }

        startForeground(NOTIF_ID, buildNotification("Connecting..."))

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
                    filesDir.absolutePath,
                    // Protector: make direct-DNS sockets bypass the VPN so they
                    // don't loop back into tun2socks.
                    Protector { fd -> protect(fd.toInt()) }
                )
                updateNotification("Connected")
                Log.i(TAG, "tunnel started")
            } catch (e: Exception) {
                Log.e(TAG, "Mobile.start failed", e)
                stopTunnel()
            }
        }.start()
    }

    @Volatile
    private var stopping = false

    private fun stopTunnel() {
        if (stopping) return
        stopping = true

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
        stopTunnel()
        super.onDestroy()
    }

    private fun buildNotification(text: String): Notification {
        val nm = getSystemService(NotificationManager::class.java)
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

        return Notification.Builder(this, CHANNEL_ID)
            .setContentTitle("MasterDnsVPN")
            .setContentText(text)
            .setSmallIcon(android.R.drawable.ic_dialog_info)
            .setContentIntent(pi)
            .setOngoing(true)
            .build()
    }

    private fun updateNotification(text: String) {
        val nm = getSystemService(NotificationManager::class.java)
        nm.notify(NOTIF_ID, buildNotification(text))
    }
}
