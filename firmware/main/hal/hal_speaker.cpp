/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#include "hal_speaker.h"

#include <audio_codec.h>
#include <board.h>
#include <esp_audio_dec.h>
#include <esp_audio_types.h>
#include <esp_log.h>
#include <esp_opus_dec.h>
#include <esp_timer.h>
#include <freertos/FreeRTOS.h>
#include <freertos/task.h>

#include <atomic>
#include <mutex>
#include <queue>

namespace stackchan::hal {

static constexpr const char* TAG = "HAL-Speaker";
static constexpr uint32_t kMaxDurationMs = 300000;
static constexpr uint32_t kDefaultSampleRate = 16000;
static constexpr uint8_t kDefaultChannels = 1;
static constexpr uint16_t kDefaultFrameDurationMs = 60;
static constexpr size_t kFrameQueueMax = 64;

static esp_opus_dec_frame_duration_t opusFrameDuration(uint16_t durationMs)
{
    switch (durationMs) {
        case 5: return ESP_OPUS_DEC_FRAME_DURATION_5_MS;
        case 10: return ESP_OPUS_DEC_FRAME_DURATION_10_MS;
        case 20: return ESP_OPUS_DEC_FRAME_DURATION_20_MS;
        case 40: return ESP_OPUS_DEC_FRAME_DURATION_40_MS;
        case 60: return ESP_OPUS_DEC_FRAME_DURATION_60_MS;
        case 80: return ESP_OPUS_DEC_FRAME_DURATION_80_MS;
        case 100: return ESP_OPUS_DEC_FRAME_DURATION_100_MS;
        case 120: return ESP_OPUS_DEC_FRAME_DURATION_120_MS;
        default: return ESP_OPUS_DEC_FRAME_DURATION_INVALID;
    }
}

struct SpeakerSubsystem::Impl {
    mutable std::mutex mutex;
    std::atomic<bool> running{false};
    SpeakerStreamConfig config{};
    void* opusDecoder = nullptr;
    TaskHandle_t task = nullptr;
    uint64_t frames = 0;
    uint64_t bytes = 0;
    uint64_t deadlineMs = 0;
    std::string stopReason = "user";
    StatusCallback statusCallback;

    // Frame queue: incoming Opus frames from bridge
    std::mutex queueMutex;
    std::queue<std::vector<uint8_t>> frameQueue;

    void emitStatus(SpeakerStatusEvent event)
    {
        StatusCallback callback;
        {
            std::lock_guard<std::mutex> lock(mutex);
            callback = statusCallback;
        }
        if (callback) {
            callback(event);
        }
    }

    void finish(const std::string& reason)
    {
        running.store(false);
        if (opusDecoder) {
            esp_opus_dec_close(opusDecoder);
            opusDecoder = nullptr;
        }

        // Drain remaining frames
        {
            std::lock_guard<std::mutex> lock(queueMutex);
            while (!frameQueue.empty()) {
                frameQueue.pop();
            }
        }

        SpeakerStatusEvent event;
        event.kind = SpeakerStatusEvent::Kind::Stopped;
        event.streamId = config.streamId;
        event.reason = reason;
        event.frames = frames;
        event.bytes = bytes;
        emitStatus(event);

        std::lock_guard<std::mutex> lock(mutex);
        task = nullptr;
    }

    static void taskTrampoline(void* arg)
    {
        static_cast<Impl*>(arg)->run();
        vTaskDelete(nullptr);
    }

    void run()
    {
        auto* codec = Board::GetInstance().GetAudioCodec();
        if (!codec) {
            finish("error");
            return;
        }

        codec->Start();
        if (!codec->output_enabled()) {
            codec->EnableOutput(true);
        }

        // PCM buffer for decoded frames
        const size_t pcmSamples = config.sampleRate / 1000 * config.frameDurationMs;
        std::vector<int16_t> pcmBuf(pcmSamples * config.channels);

        while (running.load()) {
            if (deadlineMs != 0 && static_cast<uint64_t>(esp_timer_get_time() / 1000) >= deadlineMs) {
                finish("timeout");
                return;
            }

            // Pop a frame from queue
            std::vector<uint8_t> opusFrame;
            {
                std::lock_guard<std::mutex> lock(queueMutex);
                if (!frameQueue.empty()) {
                    opusFrame = std::move(frameQueue.front());
                    frameQueue.pop();
                }
            }

            if (opusFrame.empty()) {
                vTaskDelay(pdMS_TO_TICKS(5));
                continue;
            }

            // Decode Opus to PCM
            esp_audio_dec_in_raw_t in = {};
            in.buffer = opusFrame.data();
            in.len = static_cast<uint32_t>(opusFrame.size());
            esp_audio_dec_out_frame_t out = {};
            out.buffer = reinterpret_cast<uint8_t*>(pcmBuf.data());
            out.len = static_cast<uint32_t>(pcmBuf.size() * sizeof(int16_t));
            esp_audio_dec_info_t info = {};
            if (esp_opus_dec_decode(opusDecoder, &in, &out, &info) != ESP_AUDIO_ERR_OK) {
                ESP_LOGW(TAG, "Opus decode failed, skipping frame");
                continue;
            }

            // Write PCM to speaker via public OutputData
            int samplesDecoded = out.decoded_size / sizeof(int16_t);
            if (samplesDecoded > 0) {
                std::vector<int16_t> outputPcm(
                    reinterpret_cast<int16_t*>(out.buffer),
                    reinterpret_cast<int16_t*>(out.buffer) + samplesDecoded);
                codec->OutputData(outputPcm);
            }

            frames++;
            bytes += opusFrame.size();
        }

        std::string reason;
        {
            std::lock_guard<std::mutex> lock(mutex);
            reason = stopReason.empty() ? "user" : stopReason;
        }
        finish(reason);
    }
};

SpeakerSubsystem::SpeakerSubsystem() : impl_(new Impl()) {}

SpeakerSubsystem::~SpeakerSubsystem()
{
    Stop("shutdown");
    delete impl_;
}

bool SpeakerSubsystem::Start(const SpeakerStreamConfig& requestedConfig, std::string* err)
{
    std::lock_guard<std::mutex> lock(impl_->mutex);
    if (impl_->running.load()) {
        if (err) *err = "busy";
        return false;
    }

    SpeakerStreamConfig config = requestedConfig;
    if (config.sampleRate == 0) config.sampleRate = kDefaultSampleRate;
    if (config.channels == 0) config.channels = kDefaultChannels;
    if (config.frameDurationMs == 0) config.frameDurationMs = kDefaultFrameDurationMs;
    if (config.durationMs == 0) config.durationMs = 300000;
    if (config.durationMs > kMaxDurationMs) config.durationMs = kMaxDurationMs;
    if (config.streamId.empty()) config.streamId = "default";

    auto frameDur = opusFrameDuration(config.frameDurationMs);
    if (frameDur == ESP_OPUS_DEC_FRAME_DURATION_INVALID) {
        if (err) *err = "invalid_args";
        return false;
    }

    // Open Opus decoder
    esp_opus_dec_cfg_t decConfig = {
        .sample_rate = config.sampleRate,
        .channel = config.channels,
        .frame_duration = frameDur,
        .self_delimited = false,
    };
    auto openResult = esp_opus_dec_open(&decConfig, sizeof(decConfig), &impl_->opusDecoder);
    if (openResult != ESP_AUDIO_ERR_OK || !impl_->opusDecoder) {
        if (err) *err = "unavailable";
        return false;
    }

    impl_->config = config;
    impl_->frames = 0;
    impl_->bytes = 0;
    impl_->deadlineMs = static_cast<uint64_t>(esp_timer_get_time() / 1000) + config.durationMs;
    impl_->stopReason = "user";
    impl_->running.store(true);

    // Emit started event
    SpeakerStatusEvent event;
    event.kind = SpeakerStatusEvent::Kind::Started;
    event.streamId = config.streamId;
    event.startedConfig = config;
    auto statusCallback = impl_->statusCallback;
    if (statusCallback) {
        statusCallback(event);
    }

    if (xTaskCreatePinnedToCore(&Impl::taskTrampoline, "hal_spk", 32768, impl_, 5, &impl_->task, 1) != pdPASS) {
        impl_->running.store(false);
        esp_opus_dec_close(impl_->opusDecoder);
        impl_->opusDecoder = nullptr;
        SpeakerStatusEvent stopped;
        stopped.kind = SpeakerStatusEvent::Kind::Stopped;
        stopped.streamId = config.streamId;
        stopped.reason = "error";
        if (statusCallback) {
            statusCallback(stopped);
        }
        if (err) *err = "unavailable";
        return false;
    }

    return true;
}

void SpeakerSubsystem::FeedFrame(const uint8_t* opusData, size_t len)
{
    if (!impl_ || !impl_->running.load()) {
        return;
    }
    std::lock_guard<std::mutex> lock(impl_->queueMutex);
    if (impl_->frameQueue.size() >= kFrameQueueMax) {
        impl_->frameQueue.pop();
    }
    impl_->frameQueue.emplace(opusData, opusData + len);
}

void SpeakerSubsystem::Stop(const std::string& reason)
{
    if (impl_) {
        std::lock_guard<std::mutex> lock(impl_->mutex);
        impl_->stopReason = reason.empty() ? "user" : reason;
        impl_->running.store(false);
    }
}

bool SpeakerSubsystem::IsActive() const
{
    return impl_ && impl_->running.load();
}

void SpeakerSubsystem::SetStatusCallback(StatusCallback callback)
{
    std::lock_guard<std::mutex> lock(impl_->mutex);
    impl_->statusCallback = std::move(callback);
}

}  // namespace stackchan::hal
