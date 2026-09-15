#!/usr/bin/env bash
# =============================================================================
# scan_resolvers.sh — curate working DNS-tunnel resolvers for MasterDnsVPN.
#
# ВАЖНО: запускай ИЗНУТРИ РФ (там, где реально будет клиент), иначе цифры
# скорости не отражают боевой путь. RTT до резолвера решает почти всё.
#
# Двухэтапно:
#   Этап 1 (быстро, параллельно): "честный пронос" — уникальная метка
#           <rand>.$DOMAIN через каждый резолвер должна дойти до нашего
#           авторитативного сервера (rcode NOERROR). Мёртвые/REFUSED/кэширующие
#           отсеиваются за секунды.
#   Этап 2 (throughput): каждый выживший резолвер поднимается СОЛО, и меряется
#           реальная устойчивая скорость через локальный SOCKS5.
# На выходе: resolvers_working.txt (отсортирован по скорости) — его и скармливай
#           клиенту (-resolvers) или вставляй в поле Resolvers приложения.
#
# Usage:
#   ./scan_resolvers.sh -d t.example.com -b ./mdvpn-client -c ./client.toml \
#                       -i candidates.txt [-s 12] [-m 12]
# =============================================================================
set -u
DOMAIN=""; BIN="./mdvpn-client"; CFG="./client_config.tuned.toml"
CANDS="candidates.txt"; MEASURE_S=12; STARTUP_S=25; STREAMS=2; PORT=18000
MINSPEED="0.05"   # Мбит/с: порог включения в итоговый список

while getopts "d:b:c:i:m:p:t:" opt; do case $opt in
  d) DOMAIN=$OPTARG;; b) BIN=$OPTARG;; c) CFG=$OPTARG;; i) CANDS=$OPTARG;;
  m) MEASURE_S=$OPTARG;; p) PORT=$OPTARG;; t) MINSPEED=$OPTARG;; esac; done

[ -z "$DOMAIN" ] && { echo "need -d <tunnel domain>"; exit 1; }
[ -x "$BIN" ] || { echo "client binary not found/executable: $BIN"; exit 1; }
URL="http://speedtest.tele2.net/100MB.zip"
WORK="$(pwd)/resolvers_working.txt"; TSV="$(pwd)/scan_results.tsv"
kill_old(){ local p; p=$(ss -ltnpH 2>/dev/null | grep ":$PORT " | grep -oP 'pid=\K[0-9]+' | head -1); [ -n "$p" ] && kill "$p" 2>/dev/null; sleep 1; }

echo "### Stage 1: faithful-carry probe (domain=$DOMAIN) ..."
probe="$(mktemp)"
grep -vE '^\s*#|^\s*$' "$CANDS" | awk '{print $NF}' | sort -u | while read -r ip; do
  ( r="s$RANDOM$RANDOM"
    st=$(dig +time=2 +tries=1 "$r.$DOMAIN" @"$ip" 2>/dev/null | grep -oP 'status: \K[A-Z]+' | head -1)
    echo "$ip ${st:-TIMEOUT}" >> "$probe" ) &
done; wait
faithful=$(grep " NOERROR$" "$probe" | awk '{print $1}' | sort -u)
echo "  faithful (NOERROR): $(echo "$faithful" | grep -c .) / $(grep -c . "$probe")"

echo "### Stage 2: solo throughput ..."
printf "ip\taccepted\tdown_mtu\tMbit_s\n" > "$TSV"
for ip in $faithful; do
  kill_old
  log="/tmp/mdscan_$ip.log"; rl="/tmp/mdscan_$ip.res"; rf="/tmp/mdscan_$ip.txt"
  printf '%s:53\n' "$ip" > "$rf"; rm -f "$log" "$rl"
  (
    for i in $(seq 1 "$STARTUP_S"); do grep -q "listening on 127.0.0.1:$PORT" "$log" 2>/dev/null && break; sleep 1; done
    sleep 1; td=$(mktemp -d)
    for s in $(seq 1 $STREAMS); do ( curl -s --socks5-hostname 127.0.0.1:$PORT --max-time $MEASURE_S -o /dev/null -w "%{speed_download}" "$URL" > "$td/$s" 2>/dev/null ) & done
    wait; t=0; for f in "$td"/*; do t=$(awk -v a="$t" -v b="$(cat "$f" 2>/dev/null||echo 0)" 'BEGIN{print a+b}'); done
    awk -v t="$t" 'BEGIN{printf "%.3f",t*8/1e6}' > "$rl"; rm -rf "$td"
  ) &
  timeout $((STARTUP_S+MEASURE_S+6)) "$BIN" -config "$CFG" -resolvers "$rf" -log "$log" >/dev/null 2>&1
  acc=$(grep -c "Accepted (" "$log" 2>/dev/null); [ "$acc" -ge 1 ] && acc=yes || acc=no
  down=$(grep -E "Selected Synced" "$log" | tail -1 | grep -oP 'Download MTU: \K[0-9]+')
  spd=$(cat "$rl" 2>/dev/null); spd=${spd:-0.000}
  printf "%s\t%s\t%s\t%s\n" "$ip" "$acc" "${down:-0}" "$spd" >> "$TSV"
  echo "  $ip: accepted=$acc down_mtu=${down:-0} -> $spd Mbit/s"
done

echo "### Ranked:"; { head -1 "$TSV"; tail -n +2 "$TSV" | sort -t$'\t' -k4 -gr; } | column -t -s$'\t'
awk -v m="$MINSPEED" -F'\t' 'NR>1 && $4+0>=m {print $1":53"}' "$TSV" | \
  awk -v tsv="$TSV" -F'\t' 'NR==FNR{next}1' /dev/null - > "$WORK"
# rank the working list by speed
{ tail -n +2 "$TSV" | sort -t$'\t' -k4 -gr | awk -v m="$MINSPEED" -F'\t' '$4+0>=m{print $1":53"}'; } > "$WORK"
echo "### Wrote $(grep -c . "$WORK") working resolvers -> $WORK"
rm -f "$probe"
