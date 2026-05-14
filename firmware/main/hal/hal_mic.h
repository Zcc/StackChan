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

struct MicStreamConfig {
    uint32_t sampleRate      = 16000;
    uint8_t channels         = 1;
    uint16_t frameDurationMs = 60;
    uint32_t durationMs      = 30000;
    std::string streamId;
};

struct MicFrame {
    uint32_t streamHash   = 0;
    uint32_t seq          = 0;
    uint64_t timestampMs  = 0;
    std::vector<uint8_t> opusPayload;
};

struct MicStatusEvent {
    enum class Kind { Started, Stopped, Stats };

    Kind kind = Kind::Stats;
    std::string streamId;
    std::string reason;
    uint64_t frames = 0;
    uint64_t bytes  = 0;
    MicStreamConfig startedConfig{};
};

class MicSubsystem {
public:
    using FrameCallback  = std::function<void(const MicFrame&)>;
    using StatusCallback = std::function<void(const MicStatusEvent&)>;

    MicSubsystem();
    ~MicSubsystem();

    bool Start(const MicStreamConfig& config, std::string* err);
    void Stop(const std::string& reason);
    bool IsActive() const;

    void SetFrameCallback(FrameCallback callback);
    void SetStatusCallback(StatusCallback callback);

private:
    struct Impl;
    Impl* impl_ = nullptr;
};

}  // namespace stackchan::hal
