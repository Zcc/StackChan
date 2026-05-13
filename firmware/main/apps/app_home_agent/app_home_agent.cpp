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
#include <stackchan/stackchan.h>
#include <string>
#include <string_view>

using namespace mooncake;
using namespace stackchan;

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
    if (!config.enabled || config.relayUrl.empty()) {
        LvglLockGuard lock;
        loading_page->setMessage("HOME.AGENT\nnot configured");
        GetHAL().delay(1400);
    }

    GetHAL().startWebSocketAvatarService([&](std::string_view msg) {
        LvglLockGuard lock;
        loading_page->setMessage(msg);
    });

    LvglLockGuard lock;
    loading_page.reset();

    attach_avatar();
    bind_ws_events();
    _video_window = std::make_unique<view::VideoWindow>(lv_screen_active());

    show_boot_line(config.enabled ? "Relay link armed." : "Set relay in the app.");

    view::create_home_indicator([&]() { close(); }, 0x00E5FF, 0x11183A);
    view::create_status_bar(0x00E5FF, 0x11183A);
}

void AppHomeAgent::onRunning()
{
    std::lock_guard<std::mutex> lock(_mutex);
    LvglLockGuard lvgl_lock;

    if (GetHAL().millis() - _last_idle_tick > 9000) {
        _last_idle_tick = GetHAL().millis();
        auto& stackchan = GetStackChan();
        if (stackchan.hasAvatar()) {
            stackchan.addModifier(std::make_unique<TimedSpeechModifier>("HomeAgent online.", 1800));
            stackchan.addModifier(std::make_unique<TimedEmotionModifier>(avatar::Emotion::Doubt, 1000));
        }
    }

    GetStackChan().update();
    view::update_home_indicator();
    view::update_status_bar();
}

void AppHomeAgent::onClose()
{
    mclog::tagInfo(getAppInfo().name, "on close");

    {
        LvglLockGuard lock;
        GetStackChan().resetAvatar();
        _video_window.reset();
        GetHAL().stopWebSocketAvatarService();

        GetHAL().onWsAvatarData.clear();
        GetHAL().onWsMotionData.clear();
        GetHAL().onWsTextMessage.clear();
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
        show_agent_message(message.name, message.content);
    });

    GetHAL().onWsDanceData.connect([&](std::string_view data) {
        LvglLockGuard lock;
        auto sequence = animation::parse_sequence_from_json(data.data());
        if (!sequence.empty()) {
            GetStackChan().addModifier(std::make_unique<DanceModifier>(sequence));
        }
    });

    GetHAL().onWsLog.connect([&](CommonLogLevel level, std::string_view msg) {
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
    auto& stackchan = GetStackChan();
    stackchan.addModifier(std::make_unique<TimedSpeechModifier>(fmt::format("{}: {}", speaker, content), 6500));
    stackchan.addModifier(std::make_unique<SpeakingModifier>(2200));
    stackchan.addModifier(std::make_unique<TimedEmotionModifier>(avatar::Emotion::Happy, 1800));
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
