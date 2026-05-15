/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#pragma once

#include <cstdint>
#include <functional>
#include <string>
#include <vector>

namespace stackchan::hal {

struct SpeakerStreamConfig {
    uint32_t sampleRate      = 16000;
    uint8_t channels         = 1;
    uint16_t frameDurationMs = 60;
    uint32_t durationMs      = 300000;
    std::string streamId;
};

struct SpeakerStatusEvent {
    enum class Kind { Started, Stopped, Stats };

    Kind kind = Kind::Stats;
    std::string streamId;
    std::string reason;
    uint64_t frames = 0;
    uint64_t bytes  = 0;
    SpeakerStreamConfig startedConfig{};
};

class SpeakerSubsystem {
public:
    using StatusCallback = std::function<void(const SpeakerStatusEvent&)>;

    SpeakerSubsystem();
    ~SpeakerSubsystem();

    bool Start(const SpeakerStreamConfig& config, std::string* err);
    void FeedFrame(const uint8_t* opusData, size_t len);
    void Stop(const std::string& reason);
    bool IsActive() const;

    void SetStatusCallback(StatusCallback callback);

private:
    struct Impl;
    Impl* impl_ = nullptr;
};

}  // namespace stackchan::hal
