#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="${HOMEAGENT_ENV_FILE:-$ROOT_DIR/tools/.env.local}"
ACTION="${1:-get}"

usage() {
  cat <<USAGE
Usage: tools/config_homeagent.sh [get|set|reset|wifi-list|wifi-remove <index>|wifi-default <index>|wifi-clear|wifi-set <ssid> <password>]

Configure StackChan HomeAgent and Wi-Fi profiles over BLE.

Before running 'set':
  cp tools/.env.example tools/.env.local
  edit tools/.env.local

For 'wifi-set' you may pass the SSID and password as positional args, or set
WIFI_SSID and WIFI_PASSWORD in $ENV_FILE.

Environment:
  HOMEAGENT_ENV_FILE  Override env file path. Default: tools/.env.local
USAGE
}

case "$ACTION" in
  get|set|reset|wifi-list|wifi-remove|wifi-default|wifi-clear|wifi-set) ;;
  -h|--help|help) usage; exit 0 ;;
  *) usage; exit 1 ;;
esac

if [[ -f "$ENV_FILE" ]]; then
  set -a
  # shellcheck disable=SC1090
  source "$ENV_FILE"
  set +a
elif [[ "$ACTION" == "set" ]]; then
  echo "Missing env file: $ENV_FILE" >&2
  echo "Create it with: cp tools/.env.example tools/.env.local" >&2
  exit 1
fi

if [[ "$ACTION" == "set" ]]; then
  : "${RELAY_URL:?RELAY_URL is required in $ENV_FILE}"
  : "${RELAY_TOKEN:?RELAY_TOKEN is required in $ENV_FILE}"
  : "${DEVICE_ID:?DEVICE_ID is required in $ENV_FILE}"
fi

if [[ "$ACTION" == "wifi-set" ]]; then
  WIFI_SSID="${2:-${WIFI_SSID:-}}"
  WIFI_PASSWORD="${3:-${WIFI_PASSWORD:-}}"
  if [[ -z "$WIFI_SSID" ]]; then
    echo "wifi-set requires an SSID (positional arg or WIFI_SSID env)" >&2
    exit 1
  fi
  export WIFI_SSID WIFI_PASSWORD
  exec swift "$ROOT_DIR/tools/ble_config_homeagent.swift" "$ACTION"
fi

exec swift "$ROOT_DIR/tools/ble_config_homeagent.swift" "$ACTION" "${2:-}"
