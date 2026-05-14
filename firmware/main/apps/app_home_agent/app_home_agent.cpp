/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#include "app_home_agent.h"
#include <apps/app_avatar/view/video_window.hpp>
#include <apps/common/common.h>
#include <assets/assets.h>
#include <hal/hal.h>
#include <fmt/format.h>
#include <mooncake.h>
#include <mooncake_log.h>
#include <smooth_lvgl.hpp>
#include <smooth_ui_toolkit.hpp>
#include <stackchan/stackchan.h>
#include <string>
#include <string_view>

using namespace mooncake;
using namespace stackchan;
using namespace smooth_ui_toolkit::lvgl_cpp;

AppHomeAgent::AppHomeAgent()
{
    setAppInfo().name = "HOME.AGENT";
    static auto icon  = assets::get_image("icon_sentinel.bin");
    setAppInfo().icon = (void*)&icon;
    static uint32_t theme_color = 0x00E5FF;
    setAppInfo().userData       = (void*)&theme_color;
}

void AppHomeAgent::onCreate()
{
    mclog::tagInfo(getAppInfo().name, "on create");
}

void AppHomeAgent::onOpen()
{
    mclog::tagInfo(getAppInfo().name, "on open");

    std::unique_ptr<view::LoadingPage> loading_page;
    {
        LvglLockGuard lock;
        loading_page = std::make_unique<view::LoadingPage>(0x070A18, 0x00E5FF);
        loading_page->setMessage("HOME.AGENT\nbooting...");
    }

    auto config = GetHAL().getHomeAgentConfig();
    bool is_configured = config.enabled && !config.relayUrl.empty();
    bool network_ready = false;

    if (is_configured) {
        constexpr uint32_t network_timeout_ms = 20000;
        network_ready = GetHAL().startWebSocketAvatarService(
            [&](std::string_view msg) {
                LvglLockGuard lock;
                loading_page->setMessage(msg);
            },
            network_timeout_ms);
    } else {
        LvglLockGuard lock;
        loading_page->setMessage("HOME.AGENT\nnot configured");
        GetHAL().delay(1200);
    }

    LvglLockGuard lock;
    loading_page.reset();

    attach_avatar();
    bind_ws_events();
    _video_window = std::make_unique<view::VideoWindow>(lv_screen_active());
    _relay_online = network_ready;
    _agent_seen = false;
    _camera_active = false;
    create_hud(is_configured, network_ready);

    if (!is_configured) {
        show_boot_line("Set relay in the app.");
    } else if (!network_ready) {
        show_boot_line("WiFi unavailable. Swipe up to exit.");
    } else {
        show_boot_line("Relay link armed.");
    }

    view::create_home_indicator([&]() { close(); }, 0x00E5FF, 0x11183A);
    view::create_status_bar(0x00E5FF, 0x11183A);
}

void AppHomeAgent::onRunning()
{
    std::lock_guard<std::mutex> lock(_mutex);
    LvglLockGuard lvgl_lock;

    auto& stackchan = GetStackChan();

    if (_speech_clear_tick > 0 && GetHAL().millis() >= _speech_clear_tick) {
        if (stackchan.hasAvatar()) {
            stackchan.avatar().clearSpeech();
        }
        _speech_clear_tick = 0;
    }

    if (_speech_clear_tick == 0 && GetHAL().millis() - _last_idle_tick > 9000) {
        _last_idle_tick = GetHAL().millis();
        if (stackchan.hasAvatar()) {
            stackchan.addModifier(std::make_unique<TimedSpeechModifier>("HomeAgent online.", 1800));
            stackchan.addModifier(std::make_unique<TimedEmotionModifier>(avatar::Emotion::Doubt, 1000));
        }
    }

    GetStackChan().update();
    if (GetHAL().millis() - _last_hud_tick > 1000) {
        _last_hud_tick = GetHAL().millis();
        update_hud();
    }

    view::update_home_indicator();
    view::update_status_bar();
}

void AppHomeAgent::onClose()
{
    mclog::tagInfo(getAppInfo().name, "on close");

    {
        LvglLockGuard lock;
        GetStackChan().resetAvatar();
        _hud_wifi.reset();
        _hud_camera.reset();
        _hud_agent.reset();
        _hud_relay.reset();
        _hud_title.reset();
        _hud_panel.reset();
        _video_window.reset();
        GetHAL().stopWebSocketAvatarService();

        GetHAL().onWsAvatarData.clear();
        GetHAL().onWsMotionData.clear();
        GetHAL().onWsTextMessage.clear();
        GetHAL().onWsVideoModeChange.clear();
        GetHAL().onWsDanceData.clear();
        GetHAL().onWsLog.clear();

        view::destroy_home_indicator();
        view::destroy_status_bar();
    }

    GetHAL().requestWarmReboot(1);
}

void AppHomeAgent::attach_avatar()
{
    auto avatar = std::make_unique<avatar::DefaultAvatar>();
    avatar->primaryColor   = lv_color_hex(0x00E5FF);
    avatar->secondaryColor = lv_color_hex(0x070A18);
    avatar->init(lv_screen_active());
    GetStackChan().attachAvatar(std::move(avatar));
}

void AppHomeAgent::bind_ws_events()
{
    GetHAL().onWsAvatarData.connect([&](std::string_view data) {
        LvglLockGuard lock;
        GetStackChan().updateAvatarFromJson(data.data());
    });

    GetHAL().onWsMotionData.connect([&](std::string_view data) {
        LvglLockGuard lock;
        check_auto_angle_sync_mode();
        GetStackChan().updateMotionFromJson(data.data());
    });

    GetHAL().onWsTextMessage.connect([&](const WsTextMessage_t& message) {
        LvglLockGuard lock;
        _agent_seen = true;
        update_hud();
        show_agent_message(message.name, message.content);
    });

    GetHAL().onWsVideoModeChange.connect([&](bool enabled) {
        LvglLockGuard lock;
        _camera_active = enabled;
        update_hud();
    });

    GetHAL().onWsDanceData.connect([&](std::string_view data) {
        LvglLockGuard lock;
        auto sequence = animation::parse_sequence_from_json(data.data());
        if (!sequence.empty()) {
            GetStackChan().addModifier(std::make_unique<DanceModifier>(sequence));
        }
    });

    GetHAL().onWsLog.connect([&](CommonLogLevel level, std::string_view msg) {
        std::string log_msg(msg);
        if (log_msg.find("Server connected") != std::string::npos ||
            log_msg.find("Relay connected") != std::string::npos) {
            _relay_online = true;
        } else if (log_msg.find("Server disconnected") != std::string::npos ||
                   log_msg.find("Heartbeat Timeout") != std::string::npos ||
                   log_msg.find("Connect to server Failed") != std::string::npos) {
            _relay_online = false;
        }
        update_hud();

        auto type         = static_cast<view::ToastType>(level);
        uint32_t duration = type == view::ToastType::Error ? 12000 : 1800;
        view::pop_a_toast(msg, type, duration);
    });
}

void AppHomeAgent::show_boot_line(const std::string& line)
{
    auto& stackchan = GetStackChan();
    stackchan.addModifier(std::make_unique<TimedSpeechModifier>(line, 3200));
    stackchan.addModifier(std::make_unique<TimedEmotionModifier>(avatar::Emotion::Happy, 1600));
}

void AppHomeAgent::show_agent_message(const std::string& name, const std::string& content)
{
    auto speaker = name.empty() ? "Agent" : name;
    auto speech = fmt::format("{}: {}", speaker, content);
    mclog::tagInfo(getAppInfo().name, "show agent message: {}", speech);

    auto& stackchan = GetStackChan();
    if (stackchan.hasAvatar()) {
        stackchan.avatar().setSpeech(speech);
        stackchan.avatar().setEmotion(avatar::Emotion::Happy);
        _speech_clear_tick = GetHAL().millis() + 8000;
    }
    stackchan.addModifier(std::make_unique<SpeakingModifier>(2200));
}

void AppHomeAgent::create_hud(bool configured, bool networkReady)
{
    _hud_panel = std::make_unique<Container>(lv_screen_active());
    _hud_panel->setSize(124, 104);
    _hud_panel->align(LV_ALIGN_TOP_RIGHT, -7, 24);
    _hud_panel->setBgColor(lv_color_hex(0x070A18));
    _hud_panel->setBgOpa(LV_OPA_80);
    _hud_panel->setBorderWidth(1);
    _hud_panel->setBorderColor(lv_color_hex(0x00E5FF));
    _hud_panel->setRadius(5);
    _hud_panel->setPadding(5, 5, 4, 4);
    _hud_panel->removeFlag(LV_OBJ_FLAG_SCROLLABLE);
    _hud_panel->addFlag(LV_OBJ_FLAG_FLOATING);

    _hud_title = std::make_unique<Label>(_hud_panel->get());
    _hud_title->setText("HOME");
    _hud_title->setTextFont(&lv_font_montserrat_16);
    _hud_title->setTextColor(lv_color_hex(0x00E5FF));
    _hud_title->align(LV_ALIGN_TOP_LEFT, 2, 0);

    _hud_relay = std::make_unique<Label>(_hud_panel->get());
    _hud_agent = std::make_unique<Label>(_hud_panel->get());
    _hud_camera = std::make_unique<Label>(_hud_panel->get());
    _hud_wifi = std::make_unique<Label>(_hud_panel->get());

    _hud_relay->setTextFont(&lv_font_montserrat_16);
    _hud_agent->setTextFont(&lv_font_montserrat_16);
    _hud_camera->setTextFont(&lv_font_montserrat_16);
    _hud_wifi->setTextFont(&lv_font_montserrat_16);

    _hud_relay->align(LV_ALIGN_TOP_LEFT, 2, 20);
    _hud_agent->align(LV_ALIGN_TOP_LEFT, 2, 40);
    _hud_camera->align(LV_ALIGN_TOP_LEFT, 2, 60);
    _hud_wifi->align(LV_ALIGN_TOP_LEFT, 2, 80);

    _relay_online = configured && networkReady;
    update_hud();
}

void AppHomeAgent::update_hud()
{
    if (!_hud_panel) {
        return;
    }

    set_hud_chip(_hud_relay.get(), _relay_online ? "RLY  ON" : "RLY  --", _relay_online);
    set_hud_chip(_hud_agent.get(), _agent_seen ? "AGT  SEEN" : "AGT  WAIT", _agent_seen);
    set_hud_chip(_hud_camera.get(), _camera_active ? "CAM  LIVE" : "CAM  IDLE", _camera_active);

    auto wifi = GetHAL().getWifiStatus();
    bool wifi_online = wifi != WifiStatus::None;
    set_hud_chip(_hud_wifi.get(), wifi_status_text(), wifi_online);
}

void AppHomeAgent::set_hud_chip(Label* label, const std::string& text, bool online)
{
    if (!label) {
        return;
    }
    label->setText(text);
    label->setTextColor(lv_color_hex(online ? 0x7CFFB2 : 0xFF5577));
}

std::string AppHomeAgent::wifi_status_text() const
{
    switch (GetHAL().getWifiStatus()) {
        case WifiStatus::High:
            return "WIFI HIGH";
        case WifiStatus::Medium:
            return "WIFI MID";
        case WifiStatus::Low:
            return "WIFI LOW";
        case WifiStatus::None:
        default:
            return "WIFI --";
    }
}

void AppHomeAgent::check_auto_angle_sync_mode()
{
    auto& motion = GetStackChan().motion();

    if (GetHAL().millis() - _last_motion_cmd_tick > 2000) {
        motion.setAutoAngleSyncEnabled(true);
    } else {
        motion.setAutoAngleSyncEnabled(false);
    }

    _last_motion_cmd_tick = GetHAL().millis();
}
