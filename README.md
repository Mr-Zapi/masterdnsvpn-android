# MasterDnsVPN — Android Client

A native Android VPN client for the [MasterDnsVPN](https://github.com/masterking32/MasterDnsVPN)
DNS tunnel. It carries **all device traffic** through the tunnel using Android's
`VpnService`, with a small dark, minimal UI.

<p align="center">
  <img src="android/app/src/main/res/mipmap-xxxhdpi/ic_launcher.png" width="96" alt="icon">
</p>

## How it works

```
 Android apps ──► TUN (VpnService) ──► tun2socks ──► SOCKS5 (Go core)
                        │                                 │
                    DNS :53 ──► resolved directly ──► Yandex DNS (protected socket)
                                                          │
                                              TCP through the DNS tunnel ──► your VPS
```

The Go client + tun2socks are compiled into a single native library
(`mobile.aar`) via gomobile. The Kotlin app establishes the TUN interface and
hands its fd to the Go core. DNS is resolved directly via public resolvers
(Yandex by default) over a VPN-protected socket, which is both fast and robust
across devices.

## Build

You need JDK 17+, Go 1.25+, and the Android SDK + NDK.

```bash
# 1) native library
export ANDROID_HOME=$HOME/Android/Sdk
export ANDROID_NDK_HOME=$ANDROID_HOME/ndk/<version>
./scripts/build-android-aar.sh          # -> android/app/libs/mobile.aar

# 2) debug APK
cd android
./gradlew assembleDebug                  # -> app/build/outputs/apk/debug/app-debug.apk
```

Full setup (installing the SDK/NDK from scratch, no Android Studio needed) and
the in-app configuration format are documented in
[`android/README.md`](android/README.md).

## Release build (for sharing)

```bash
keytool -genkey -v -keystore ~/masterdns.jks -keyalg RSA -keysize 2048 \
  -validity 10000 -alias masterdns
cp android/keystore.properties.example android/keystore.properties   # fill in
cd android && ./gradlew assembleRelease  # -> app/build/outputs/apk/release/app-release.apk
```

Keep your `.jks` and `keystore.properties` out of git (already in `.gitignore`).

## Server

This is the **client** only. You also need a MasterDnsVPN **server** on a VPS
and a domain whose NS record is delegated to it — see the upstream project and
[`android/README.md`](android/README.md).

## Credits & license

Built on top of [masterking32/MasterDnsVPN](https://github.com/masterking32/MasterDnsVPN)
(the Go DNS-tunnel core) and [xjasonlyu/tun2socks](https://github.com/xjasonlyu/tun2socks).
The original project's README is preserved as [`UPSTREAM.md`](UPSTREAM.md).
See [`LICENSE`](LICENSE).
