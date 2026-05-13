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
    uint32_t _last_idle_tick       = 0;
    std::unique_ptr<view::VideoWindow> _video_window;

    void attach_avatar();
    void bind_ws_events();
    void show_boot_line(const std::string& line);
    void show_agent_message(const std::string& name, const std::string& content);
    void check_auto_angle_sync_mode();
};
