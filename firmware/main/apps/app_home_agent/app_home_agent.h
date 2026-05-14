/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#pragma once
#include <mooncake.h>
#include <memory>
#include <mutex>
#include <string>

namespace view {
class VideoWindow;
}
namespace smooth_ui_toolkit::lvgl_cpp {
class Container;
class Label;
}

class AppHomeAgent : public mooncake::AppAbility {
public:
    AppHomeAgent();

    void onCreate() override;
    void onOpen() override;
    void onRunning() override;
    void onClose() override;

private:
    std::mutex _mutex;

    uint32_t _last_motion_cmd_tick = 0;
    uint32_t _speech_clear_tick    = 0;
    uint32_t _last_hud_tick        = 0;
    bool _relay_online             = false;
    bool _agent_seen               = false;
    bool _camera_active            = false;
    std::unique_ptr<view::VideoWindow> _video_window;
    std::unique_ptr<smooth_ui_toolkit::lvgl_cpp::Container> _hud_topbar;
    std::unique_ptr<smooth_ui_toolkit::lvgl_cpp::Container> _hud_dot;
    std::unique_ptr<smooth_ui_toolkit::lvgl_cpp::Label> _hud_title;
    std::unique_ptr<smooth_ui_toolkit::lvgl_cpp::Label> _hud_wifi;
    std::unique_ptr<smooth_ui_toolkit::lvgl_cpp::Container> _hud_relay_chip;
    std::unique_ptr<smooth_ui_toolkit::lvgl_cpp::Container> _hud_agent_chip;
    std::unique_ptr<smooth_ui_toolkit::lvgl_cpp::Container> _hud_camera_chip;
    std::unique_ptr<smooth_ui_toolkit::lvgl_cpp::Label> _hud_relay;
    std::unique_ptr<smooth_ui_toolkit::lvgl_cpp::Label> _hud_agent;
    std::unique_ptr<smooth_ui_toolkit::lvgl_cpp::Label> _hud_camera;
    std::unique_ptr<smooth_ui_toolkit::lvgl_cpp::Container> _agent_card;
    std::unique_ptr<smooth_ui_toolkit::lvgl_cpp::Label> _agent_card_label;
    std::unique_ptr<smooth_ui_toolkit::lvgl_cpp::Label> _agent_card_text;

    void attach_avatar();
    void bind_ws_events();
    void show_boot_line(const std::string& line);
    void show_agent_message(const std::string& name, const std::string& content);
    void create_hud(bool configured, bool networkReady);
    void update_hud();
    void set_hud_chip(smooth_ui_toolkit::lvgl_cpp::Container* chip, smooth_ui_toolkit::lvgl_cpp::Label* label, const std::string& text, bool online);
    void set_agent_card(const std::string& label, const std::string& text, bool accent = false);
    std::string wifi_status_text() const;
    void check_auto_angle_sync_mode();
};
