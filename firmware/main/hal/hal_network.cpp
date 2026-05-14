/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#include "hal.h"
#include <stackchan/stackchan.h>
#include <mooncake.h>
#include <mooncake_log.h>
#include <wifi_manager.h>
#include <ssid_manager.h>
#include <settings.h>
#include <board.h>
#include <mutex>
#include <queue>
#include <vector>
#include <ctime>
#include <sys/time.h>
#include <esp_sntp.h>
#include <atomic>

static std::string _tag           = "Network";
static bool _is_network_connected = false;
static const std::string _wifi_profile_setting_ns = "home_agent_wifi";
static const std::string _last_success_ssid_key = "last_success_ssid";

static void time_sync_notification_cb(struct timeval* tv)
{
    mclog::tagInfo(_tag, "SNTP time synchronized");
    GetHAL().syncSystemTimeToRtc();
}

void Hal::startSntp()
{
    mclog::tagInfo(_tag, "SNTP init");

    if (esp_sntp_enabled()) {
    } else {
        esp_sntp_setoperatingmode(SNTP_OPMODE_POLL);

        esp_sntp_setservername(0, "pool.ntp.org");
        esp_sntp_setservername(1, "time.google.com");
        esp_sntp_setservername(2, "cn.pool.ntp.org");

        sntp_set_time_sync_notification_cb(time_sync_notification_cb);

        esp_sntp_init();
    }
}

bool Hal::startNetwork(std::function<void(std::string_view)> onLog, uint32_t timeoutMs)
{
    if (_is_network_connected) {
        mclog::tagInfo(_tag, "network already connected");
        return true;
    }

    std::atomic<bool> network_connected = false;

    auto& board = Board::GetInstance();
    mclog::tagInfo(_tag, "start and wait for network connected...");

    board.SetNetworkEventCallback([&network_connected, &onLog](NetworkEvent event, const std::string& data) {
        switch (event) {
            case NetworkEvent::Scanning:
                if (onLog) {
                    onLog("WiFi scanning...");
                }
                break;
            case NetworkEvent::Connecting: {
                if (data.empty()) {
                    if (onLog) {
                        onLog("WiFi connecting...");
                    }
                } else {
                    if (onLog) {
                        onLog(fmt::format("Connecting to {} ...", data));
                    }
                }
                break;
            }
            case NetworkEvent::Connected: {
                network_connected = true;
                if (!data.empty()) {
                    Settings settings(_wifi_profile_setting_ns, true);
                    settings.SetString(_last_success_ssid_key, data);
                }
                break;
            }
            case NetworkEvent::Disconnected:
                break;
            case NetworkEvent::WifiConfigModeEnter: {
                auto& wifi_manager = WifiManager::GetInstance();
                auto msg = fmt::format("Enter WiFi config mode. Hotspot: {}, Config URL: {}", wifi_manager.GetApSsid(),
                                       wifi_manager.GetApWebUrl());
                if (onLog) {
                    onLog(msg);
                }
                break;
            }
            case NetworkEvent::WifiConfigModeExit:
                // WiFi config mode exit is handled by WifiBoard internally
                break;
            // Cellular modem specific events
            case NetworkEvent::ModemDetecting:
                break;
            case NetworkEvent::ModemErrorNoSim:
                break;
            case NetworkEvent::ModemErrorRegDenied:
                break;
            case NetworkEvent::ModemErrorInitFailed:
                break;
            case NetworkEvent::ModemErrorTimeout:
                break;
        }
    });
    board.StartNetwork();

    auto start_tick = GetHAL().millis();
    while (!network_connected) {
        if (timeoutMs > 0 && GetHAL().millis() - start_tick >= timeoutMs) {
            mclog::tagWarn(_tag, "network start timeout after {} ms", timeoutMs);
            if (onLog) {
                onLog("WiFi unavailable. Swipe up to exit.");
            }
            board.SetNetworkEventCallback(nullptr);
            return false;
        }
        GetHAL().delay(500);
    }
    mclog::tagInfo(_tag, "network connected");
    board.SetNetworkEventCallback(nullptr);

    startSntp();

    _is_network_connected = true;
    return true;
}

WifiStatus Hal::getWifiStatus()
{
    auto& wifi = WifiManager::GetInstance();

    if (wifi.IsConfigMode()) {
        return WifiStatus::None;
    }
    if (!wifi.IsConnected()) {
        return WifiStatus::None;
    }

    int rssi = wifi.GetRssi();
    if (rssi >= -65) {
        return WifiStatus::High;
    } else if (rssi >= -75) {
        return WifiStatus::Medium;
    }
    return WifiStatus::Low;
}

std::vector<WifiProfile_t> Hal::getWifiProfiles()
{
    auto last_success = getLastSuccessfulWifiSsid();
    std::vector<WifiProfile_t> profiles;
    for (const auto& item : SsidManager::GetInstance().GetSsidList()) {
        profiles.push_back({item.ssid, !last_success.empty() && item.ssid == last_success});
    }
    return profiles;
}

void Hal::removeWifiProfile(int index)
{
    auto profiles = getWifiProfiles();
    if (index >= 0 && index < static_cast<int>(profiles.size()) && profiles[index].lastSuccessful) {
        Settings settings(_wifi_profile_setting_ns, true);
        settings.SetString(_last_success_ssid_key, "");
    }
    SsidManager::GetInstance().RemoveSsid(index);
}

void Hal::setDefaultWifiProfile(int index)
{
    SsidManager::GetInstance().SetDefaultSsid(index);
}

void Hal::clearWifiProfiles()
{
    SsidManager::GetInstance().Clear();
    Settings settings(_wifi_profile_setting_ns, true);
    settings.SetString(_last_success_ssid_key, "");
}

std::string Hal::getLastSuccessfulWifiSsid()
{
    Settings settings(_wifi_profile_setting_ns, false);
    return settings.GetString(_last_success_ssid_key, "");
}

std::string Hal::getCurrentWifiSsid()
{
    return WifiManager::GetInstance().GetSsid();
}

std::string Hal::getWifiIpAddress()
{
    return WifiManager::GetInstance().GetIpAddress();
}

bool Hal::isWifiConfigMode()
{
    return WifiManager::GetInstance().IsConfigMode();
}

void Hal::enterWifiConfigMode()
{
    auto& wifi = WifiManager::GetInstance();
    if (!wifi.IsInitialized()) {
        WifiManagerConfig config;
        config.ssid_prefix = "StackChan";
        config.language = "zh-CN";
        wifi.Initialize(config);
    }
    wifi.StartConfigAp();
}
