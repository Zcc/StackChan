/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#include "hal_mic.h"

#include <audio_codec.h>
#include <board.h>
#include <esp_ae_rate_cvt.h>
#include <esp_audio_enc.h>
#include <esp_audio_types.h>
#include <esp_log.h>
#include <esp_opus_enc.h>
#include <esp_timer.h>
#include <freertos/FreeRTOS.h>
#include <freertos/task.h>

#include <atomic>
#include <mutex>

namespace stackchan::hal {

static constexpr const char* TAG = "HAL-Mic";
static constexpr uint32_t kMaxDurationMs = 300000;
static constexpr uint32_t kDefaultSampleRate = 16000;
static constexpr uint8_t kDefaultChannels = 1;
static constexpr uint16_t kDefaultFrameDurationMs = 60;

static esp_opus_enc_frame_duration_t opusFrameDuration(uint16_t durationMs)
{
    switch (durationMs) {
        case 5: return ESP_OPUS_ENC_FRAME_DURATION_5_MS;
        case 10: return ESP_OPUS_ENC_FRAME_DURATION_10_MS;
        case 20: return ESP_OPUS_ENC_FRAME_DURATION_20_MS;
        case 40: return ESP_OPUS_ENC_FRAME_DURATION_40_MS;
        case 60: return ESP_OPUS_ENC_FRAME_DURATION_60_MS;
        case 80: return ESP_OPUS_ENC_FRAME_DURATION_80_MS;
        case 100: return ESP_OPUS_ENC_FRAME_DURATION_100_MS;
        case 120: return ESP_OPUS_ENC_FRAME_DURATION_120_MS;
        default: return ESP_OPUS_ENC_FRAME_DURATION_ARG;
    }
}

struct MicSubsystem::Impl {
    mutable std::mutex mutex;
    std::atomic<bool> running{false};
    MicStreamConfig config{};
    void* opusEncoder = nullptr;
    esp_ae_rate_cvt_handle_t rateConverter = nullptr;
    int encoderFrameBytes = 0;
    int encoderOutbufBytes = 0;
    TaskHandle_t task = nullptr;
    uint32_t seq = 0;
    uint64_t frames = 0;
    uint64_t bytes = 0;
    uint64_t deadlineMs = 0;
    std::string stopReason = "user";
    FrameCallback frameCallback;
    StatusCallback statusCallback;

    static uint32_t fnv32(const std::string& value)
    {
        uint32_t hash = 2166136261u;
        for (char c : value) {
            hash ^= static_cast<uint8_t>(c);
            hash *= 16777619u;
        }
        return hash;
    }

    static void taskTrampoline(void* arg)
    {
        static_cast<Impl*>(arg)->run();
        vTaskDelete(nullptr);
    }

    void emitStatus(MicStatusEvent event)
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

    void emitFrame(const MicFrame& frame)
    {
        FrameCallback callback;
        {
            std::lock_guard<std::mutex> lock(mutex);
            callback = frameCallback;
        }
        if (callback) {
            callback(frame);
        }
    }

    void finish(const std::string& reason)
    {
        running.store(false);
        if (opusEncoder) {
            esp_opus_enc_close(opusEncoder);
            opusEncoder = nullptr;
        }
        if (rateConverter) {
            esp_ae_rate_cvt_close(rateConverter);
            rateConverter = nullptr;
        }

        MicStatusEvent event;
        event.kind = MicStatusEvent::Kind::Stopped;
        event.streamId = config.streamId;
        event.reason = reason;
        event.frames = frames;
        event.bytes = bytes;
        emitStatus(event);

        std::lock_guard<std::mutex> lock(mutex);
        task = nullptr;
    }

    bool readPcm(AudioCodec* codec, std::vector<int16_t>& pcm)
    {
        const int codecChannels = codec->input_channels();
        const int codecSampleRate = codec->input_sample_rate();
        const size_t targetSamples = config.sampleRate / 1000 * config.frameDurationMs;
        const size_t sourceSamples = targetSamples * codecSampleRate / config.sampleRate;
        std::vector<int16_t> source(sourceSamples * codecChannels);

        if (!codec->InputData(source)) {
            return false;
        }

        if (codecSampleRate != static_cast<int>(config.sampleRate)) {
            uint32_t outSampleCount = 0;
            if (esp_ae_rate_cvt_get_max_out_sample_num(rateConverter, sourceSamples, &outSampleCount) != ESP_AE_ERR_OK) {
                return false;
            }
            std::vector<int16_t> resampled(outSampleCount * codecChannels);
            uint32_t actualOutSamples = outSampleCount;
            if (esp_ae_rate_cvt_process(rateConverter, reinterpret_cast<esp_ae_sample_t>(source.data()), sourceSamples,
                                        reinterpret_cast<esp_ae_sample_t>(resampled.data()),
                                        &actualOutSamples) != ESP_AE_ERR_OK) {
                return false;
            }
            resampled.resize(actualOutSamples * codecChannels);
            source = std::move(resampled);
        }

        if (codecChannels == 1) {
            pcm.assign(source.begin(), source.begin() + std::min(source.size(), targetSamples));
        } else {
            pcm.resize(targetSamples);
            for (size_t i = 0, j = 0; i < targetSamples && j < source.size(); ++i, j += codecChannels) {
                pcm[i] = source[j];
            }
        }
        return pcm.size() == targetSamples;
    }

    void run()
    {
        auto* codec = Board::GetInstance().GetAudioCodec();
        if (!codec) {
            finish("error");
            return;
        }

        codec->Start();
        if (!codec->input_enabled()) {
            codec->EnableInput(true);
        }

        std::vector<int16_t> pcm(encoderFrameBytes / sizeof(int16_t));
        std::vector<uint8_t> opusOut(encoderOutbufBytes);

        while (running.load()) {
            if (deadlineMs != 0 && static_cast<uint64_t>(esp_timer_get_time() / 1000) >= deadlineMs) {
                finish("timeout");
                return;
            }

            if (!readPcm(codec, pcm)) {
                ESP_LOGW(TAG, "PCM read failed");
                finish("error");
                return;
            }

            esp_audio_enc_in_frame_t in = {
                .buffer = reinterpret_cast<uint8_t*>(pcm.data()),
                .len = static_cast<uint32_t>(pcm.size() * sizeof(int16_t)),
            };
            esp_audio_enc_out_frame_t out = {
                .buffer = opusOut.data(),
                .len = static_cast<uint32_t>(opusOut.size()),
                .encoded_bytes = 0,
                .pts = 0,
            };
            if (esp_opus_enc_process(opusEncoder, &in, &out) != ESP_AUDIO_ERR_OK) {
                ESP_LOGW(TAG, "Opus encode failed");
                continue;
            }

            MicFrame frame;
            frame.streamHash = fnv32(config.streamId);
            frame.seq = seq++;
            frame.timestampMs = static_cast<uint64_t>(esp_timer_get_time() / 1000);
            frame.opusPayload.assign(opusOut.data(), opusOut.data() + out.encoded_bytes);
            frames++;
            bytes += out.encoded_bytes;
            emitFrame(frame);
        }

        std::string reason;
        {
            std::lock_guard<std::mutex> lock(mutex);
            reason = stopReason.empty() ? "user" : stopReason;
        }
        finish(reason);
    }
};

MicSubsystem::MicSubsystem() : impl_(new Impl()) {}

MicSubsystem::~MicSubsystem()
{
    Stop("shutdown");
    delete impl_;
}

bool MicSubsystem::Start(const MicStreamConfig& requestedConfig, std::string* err)
{
    std::lock_guard<std::mutex> lock(impl_->mutex);
    if (impl_->running.load()) {
        if (err) *err = "busy";
        return false;
    }

    MicStreamConfig config = requestedConfig;
    if (config.sampleRate == 0) config.sampleRate = kDefaultSampleRate;
    if (config.channels == 0) config.channels = kDefaultChannels;
    if (config.frameDurationMs == 0) config.frameDurationMs = kDefaultFrameDurationMs;
    if (config.durationMs == 0) config.durationMs = 30000;
    if (config.durationMs > kMaxDurationMs) config.durationMs = kMaxDurationMs;
    if (config.streamId.empty()) config.streamId = "default";

    if (config.sampleRate != kDefaultSampleRate || config.channels != kDefaultChannels ||
        opusFrameDuration(config.frameDurationMs) == ESP_OPUS_ENC_FRAME_DURATION_ARG) {
        if (err) *err = "invalid_args";
        return false;
    }

    auto* codec = Board::GetInstance().GetAudioCodec();
    if (!codec) {
        if (err) *err = "unavailable";
        return false;
    }

    if (codec->input_sample_rate() != static_cast<int>(config.sampleRate)) {
        esp_ae_rate_cvt_cfg_t rateConfig = {
            .src_rate = static_cast<uint32_t>(codec->input_sample_rate()),
            .dest_rate = config.sampleRate,
            .channel = static_cast<uint8_t>(codec->input_channels()),
            .bits_per_sample = ESP_AUDIO_BIT16,
            .complexity = 2,
            .perf_type = ESP_AE_RATE_CVT_PERF_TYPE_SPEED,
        };
        if (esp_ae_rate_cvt_open(&rateConfig, &impl_->rateConverter) != ESP_AE_ERR_OK || !impl_->rateConverter) {
            if (err) *err = "unavailable";
            return false;
        }
    }

    esp_opus_enc_config_t opusConfig = {
        .sample_rate = static_cast<int>(config.sampleRate),
        .channel = ESP_AUDIO_MONO,
        .bits_per_sample = ESP_AUDIO_BIT16,
        .bitrate = ESP_OPUS_BITRATE_AUTO,
        .frame_duration = opusFrameDuration(config.frameDurationMs),
        .application_mode = ESP_OPUS_ENC_APPLICATION_AUDIO,
        .complexity = 0,
        .enable_fec = false,
        .enable_dtx = true,
        .enable_vbr = true,
    };
    auto openResult = esp_opus_enc_open(&opusConfig, sizeof(opusConfig), &impl_->opusEncoder);
    if (openResult != ESP_AUDIO_ERR_OK || !impl_->opusEncoder) {
        if (impl_->rateConverter) {
            esp_ae_rate_cvt_close(impl_->rateConverter);
            impl_->rateConverter = nullptr;
        }
        if (err) *err = "unavailable";
        return false;
    }

    if (esp_opus_enc_get_frame_size(impl_->opusEncoder, &impl_->encoderFrameBytes, &impl_->encoderOutbufBytes) !=
        ESP_AUDIO_ERR_OK) {
        esp_opus_enc_close(impl_->opusEncoder);
        impl_->opusEncoder = nullptr;
        if (impl_->rateConverter) {
            esp_ae_rate_cvt_close(impl_->rateConverter);
            impl_->rateConverter = nullptr;
        }
        if (err) *err = "unavailable";
        return false;
    }

    impl_->config = config;
    impl_->seq = 0;
    impl_->frames = 0;
    impl_->bytes = 0;
    impl_->deadlineMs = static_cast<uint64_t>(esp_timer_get_time() / 1000) + config.durationMs;
    impl_->stopReason = "user";
    impl_->running.store(true);

    MicStatusEvent event;
    event.kind = MicStatusEvent::Kind::Started;
    event.streamId = config.streamId;
    event.startedConfig = config;
    auto statusCallback = impl_->statusCallback;
    if (statusCallback) {
        statusCallback(event);
    }

    if (xTaskCreatePinnedToCore(&Impl::taskTrampoline, "hal_mic", 8192, impl_, 5, &impl_->task, 1) != pdPASS) {
        impl_->running.store(false);
        esp_opus_enc_close(impl_->opusEncoder);
        impl_->opusEncoder = nullptr;
        if (impl_->rateConverter) {
            esp_ae_rate_cvt_close(impl_->rateConverter);
            impl_->rateConverter = nullptr;
        }
        MicStatusEvent stopped;
        stopped.kind = MicStatusEvent::Kind::Stopped;
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

void MicSubsystem::Stop(const std::string& reason)
{
    if (impl_) {
        std::lock_guard<std::mutex> lock(impl_->mutex);
        impl_->stopReason = reason.empty() ? "user" : reason;
        impl_->running.store(false);
    }
}

bool MicSubsystem::IsActive() const
{
    return impl_ && impl_->running.load();
}

void MicSubsystem::SetFrameCallback(FrameCallback callback)
{
    std::lock_guard<std::mutex> lock(impl_->mutex);
    impl_->frameCallback = std::move(callback);
}

void MicSubsystem::SetStatusCallback(StatusCallback callback)
{
    std::lock_guard<std::mutex> lock(impl_->mutex);
    impl_->statusCallback = std::move(callback);
}

}  // namespace stackchan::hal
