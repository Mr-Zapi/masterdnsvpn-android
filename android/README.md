# MasterDnsVPN — Android client

A native Android app that carries **all device traffic** through the
MasterDnsVPN DNS tunnel using Android's `VpnService`.

## How it works

```
 Android apps ──► TUN (VpnService) ──► tun2socks ──► 127.0.0.1:18000 (SOCKS5)
                                                          │
                                              MasterDnsVPN Go client
                                                          │
                                                    DNS queries
                                                          ▼
                                            Recursive resolvers ──► your VPS
```

The Go client and the `tun2socks` engine are compiled into a single native
library, `mobile.aar` (see `../mobile/mobile.go`). The Kotlin app just:

1. establishes the `VpnService` TUN interface,
2. passes its file descriptor to `Mobile.start(...)`,
3. the Go side runs the DNS tunnel + forwards every packet to it.

---

## 1. Prerequisites

You need a **working MasterDnsVPN server** first. Set it up on your VPS per the
main project README, which requires:

- A domain whose **NS record is delegated to your VPS IP** (e.g.
  `t.example.com  NS  ns.example.com`, and `ns.example.com  A  <VPS_IP>`).
  Without NS delegation a DNS tunnel cannot work.
- The server installed and running (systemd) via `server_linux_install.sh`.
- The **encryption method + key** and the **tunnel domain(s)** noted down —
  the client must match them exactly.

## 1b. Install the toolchain (no Android Studio needed)

On Arch Linux, terminal-only build. You need JDK, Go, Android SDK + NDK.

```bash
# JDK + Go
sudo pacman -S --needed jdk-openjdk go unzip

# Android command-line tools
mkdir -p ~/Android/Sdk/cmdline-tools
cd ~/Android/Sdk/cmdline-tools
curl -sSLO https://dl.google.com/android/repository/commandlinetools-linux-11076708_latest.zip
unzip -q commandlinetools-linux-*.zip
mv cmdline-tools latest        # tools expect .../cmdline-tools/latest/bin

export ANDROID_HOME=$HOME/Android/Sdk
export PATH="$ANDROID_HOME/cmdline-tools/latest/bin:$PATH"

# SDK packages + NDK (accept licenses when prompted)
sdkmanager --licenses
sdkmanager "platform-tools" "platforms;android-35" "build-tools;35.0.0" "ndk;27.2.12479018"

export ANDROID_NDK_HOME=$ANDROID_HOME/ndk/27.2.12479018
```

Point Gradle at the SDK:

```bash
cp android/local.properties.example android/local.properties
# edit sdk.dir=  →  /home/<you>/Android/Sdk
```

## 2. Build the native library (`mobile.aar`)

With `ANDROID_HOME` and `ANDROID_NDK_HOME` exported (see above):

```bash
# from the repo root:
./scripts/build-android-aar.sh
```

This installs gomobile and produces `android/app/libs/mobile.aar`.

## 3. Build the APK (Gradle wrapper — no Android Studio required)

```bash
cd android
./gradlew assembleDebug
# APK: android/app/build/outputs/apk/debug/app-debug.apk
```

Install on a connected phone:

```bash
adb install app/build/outputs/apk/debug/app-debug.apk
```

(The first `./gradlew` run downloads Gradle 8.11.1 automatically.)

## 4. Configure & connect

The app has two text fields:

### Config (base64 JSON)

Build a JSON config with the **same keys as the TOML client config**, then
base64-encode it. Minimal example:

```json
{
  "DOMAINS": ["t.example.com"],
  "DATA_ENCRYPTION_METHOD": 1,
  "ENCRYPTION_KEY": "the-shared-key-that-matches-your-server"
}
```

`DATA_ENCRYPTION_METHOD`: `0`=None `1`=XOR `2`=ChaCha20 `3`=AES-128-GCM
`4`=AES-192-GCM `5`=AES-256-GCM (must match the server).

Encode it:

```bash
base64 -w0 config.json
```

Paste the resulting string into the **Config** field. (The app forces
`PROTOCOL_TYPE=SOCKS5`, `LISTEN_IP=127.0.0.1`, `LISTEN_PORT=18000` itself — you
don't need those keys. Any other advanced tunables from `client_config.toml`
can be added to the JSON if you want to override defaults.)

### Resolvers

The recursive DNS servers the tunnel sends its queries **through** (not your
server — public recursors that will reach your delegated NS). One per line.
The app pre-fills **Yandex DNS**, which stays reachable from Russian networks
even during blocking:

```
77.88.8.8:53
77.88.8.1:53
```

You can add more (Google/Cloudflare/Quad9) below them — MasterDnsVPN balances
across all working resolvers and auto-disables broken ones:

```
77.88.8.8:53
77.88.8.1:53
8.8.8.8:53
1.1.1.1:53
```

Then tap **Connect** and accept the system VPN consent dialog. A key icon in the
status bar means the tunnel is up.

---

## Notes & limitations

- **DNS/UDP:** tun2socks forwards UDP (including DNS) through the SOCKS5 proxy
  via UDP ASSOCIATE. If your server/resolver path only carries TCP well, and DNS
  resolution misbehaves, the cleanest fix is to keep `addDnsServer(...)` (already
  set to `1.1.1.1`) so DNS is answered over the tunnelled TCP path, or enable the
  client's local DNS feature in a future iteration.
- IPv6 is not routed (IPv4 default route only). Add `addRoute("::",0)` +
  `addAddress` for an IPv6 ULA if you need it.
- This is a first working client. Battery optimisation whitelisting and
  auto-reconnect are not implemented yet.
```
