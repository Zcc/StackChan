/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#include "hal.h"
#include <stackchan/stackchan.h>
#include "board/hal_bridge.h"
#include <mooncake.h>
#include <mooncake_log.h>
#include <board.h>
#include <web_socket.h>
#include <esp_log.h>
#include <arpa/inet.h>
#include <jpg/image_to_jpeg.h>
#include <wifi_station.h>
#include <ArduinoJson.hpp>
#include <settings.h>
#include <mutex>
#include <queue>
#include <vector>
#include <esp_heap_caps.h>
#include <display.h>
#include <lvgl_image.h>
#include <wifi_manager.h>
#include "utils/jpeg_to_image/jpeg_decoder.h"
#include "utils/secret_logic/secret_logic.h"

static std::string _tag = "WS-Avatar";

static const std::string _setting_ns              = "stackchan";
static const std::string _setting_device_name_key = "device_name";
static const std::string _home_agent_setting_ns   = "home_agent";
static const std::string _home_agent_default_relay_url = "";
static const std::string _home_agent_default_device_id = "";
static const std::string _home_agent_default_token     = "";

class WebSocketAvatar {
public:
    enum class DataType : uint8_t {
        Opus              = 0x01,
        Jpeg              = 0x02,
        ControlAvatar     = 0x03,
        ControlMotion     = 0x04,
        StartCameraStream = 0x05,
        StopCameraStream  = 0x06,
        TextMessage       = 0x07,
        RequestCall       = 0x09,
        DeclineCall       = 0x0A,
        AcceptCall        = 0x0B,
        EndCall           = 0x0C,
        SetDeviceName     = 0x0D,
        GetDeviceName     = 0x0E,
        HeartbeatPing     = 0x10,
        HeartbeatPong     = 0x11,
        VideoModeOn       = 0x12,
        VideoModeOff      = 0x13,
        DanceSequence     = 0x14,
        StartAudioStream  = 0x18,
        StopAudioStream   = 0x19,
        AimedTakePhoto    = 0x1A,

        // HomeAgent 扩展能力 (0x20+)
        GetDeviceInfo     = 0x20,  // 双向: 请求/响应 JSON {mac,fw,wifi,battery,brightness,volume,...}
        GetBatteryStatus  = 0x21,  // 双向: 请求/响应 JSON {level,charging}
        SetBrightness     = 0x22,  // 入站: JSON {value:0-100}
        SetVolume         = 0x23,  // 入站: JSON {value:0-100}
        RebootDevice      = 0x24,  // 入站: 无 payload
        FactoryReset      = 0x25,  // 入站: 无 payload
        SetRgbLed         = 0x26,  // 入站: JSON [{i,r,g,b},...] 或 {leds:[...]}
        ShowRgbColor      = 0x27,  // 入站: JSON {r,g,b} 或 {color:"#RRGGBB"}
        ImuEvent          = 0x28,  // 出站: JSON {event:"shake|pickup"}
        HeadTouchEvent    = 0x29,  // 出站: JSON {gesture:"press|release|swipeForward|swipeBackward"}
        ScreenTouchEvent  = 0x2A,  // 出站 stub
        ButtonEvent       = 0x2B,  // 出站 stub

        IrSend            = 0x30,  // 入站 stub
        IrLearnStart      = 0x31,  // 入站 stub
        IrEvent           = 0x32,  // 出站 stub
        NfcRead           = 0x33,  // 入站 stub
        NfcWrite          = 0x34,  // 入站 stub
        NfcEvent          = 0x35,  // 出站 stub
        PlayAudio         = 0x36,  // 入站 stub
        MicStreamStart    = 0x37,  // 入站: JSON mic stream config
        MicStreamStop     = 0x38,  // 入站: JSON {stream_id}
        MicAudio          = 0x39,  // 出站: 16-byte header + Opus
        ScreenCapture     = 0x3A,  // 入站/响应 stub
        SdList            = 0x3B,  // 入站 stub
        MicStatus         = 0x3C,  // 出站: JSON {event:"started|stopped|stats", stream_id, ...}
        AudioStatus       = 0x3D,  // 出站: JSON {event:"started|stopped|stats", stream_id, ...}
        SdRead            = 0x42,  // 入站 stub
        SdWrite           = 0x43,  // 入站 stub
        ServoFeedback     = 0x44,  // 出站 stub
        ProximityLight    = 0x45,  // 出站 stub
        GetDriverHealth   = 0x40,  // 双向: 请求/响应 JSON {drivers:[...]}

        // 通用错误响应 - 桥可识别此 type 来知道某能力未实现
        CapabilityError   = 0x4F,
    };

    struct ReceivedMessage {
        bool binary;
        std::vector<uint8_t> data;
    };

    void init()
    {
        loadConfig();

        connect();

        GetHAL().onWsCallResponse.connect([this](bool accepted) {
            if (!isConnected()) {
                return;
            }

            if (accepted) {
                ESP_LOGI(_tag.c_str(), "Sending AcceptCall");
                sendPacket(DataType::AcceptCall, nullptr, 0);
            } else {
                ESP_LOGI(_tag.c_str(), "Sending DeclineCall");
                sendPacket(DataType::DeclineCall, nullptr, 0);
            }
        });

        GetHAL().onWsCallEnd.connect([this](WsSignalSource source) {
            if (!isConnected()) {
                return;
            }

            if (source != WsSignalSource::Local) {
                return;
            }

            ESP_LOGI(_tag.c_str(), "Sending EndCall");
            sendPacket(DataType::EndCall, nullptr, 0);
        });

        // HomeAgent: 将 IMU 动作事件透传到 relay/agent
        GetHAL().onImuMotionEvent.connect([this](ImuMotionEvent event) {
            if (!isConnected() || event == ImuMotionEvent::None) {
                return;
            }
            const char* name = (event == ImuMotionEvent::Shake)  ? "shake"
                               : (event == ImuMotionEvent::PickUp) ? "pickup"
                                                                   : "unknown";
            std::string json = std::string("{\"event\":\"") + name + "\",\"ts\":" +
                               std::to_string(GetHAL().millis()) + "}";
            sendPacket(DataType::ImuEvent, (const uint8_t*)json.c_str(), json.size());
        });

        // HomeAgent: 将头顶触摸手势透传到 relay/agent
        GetHAL().onHeadPetGesture.connect([this](HeadPetGesture gesture) {
            if (!isConnected() || gesture == HeadPetGesture::None) {
                return;
            }
            const char* name = (gesture == HeadPetGesture::Press)         ? "press"
                               : (gesture == HeadPetGesture::Release)       ? "release"
                               : (gesture == HeadPetGesture::SwipeForward)  ? "swipeForward"
                               : (gesture == HeadPetGesture::SwipeBackward) ? "swipeBackward"
                                                                            : "unknown";
            std::string json = std::string("{\"gesture\":\"") + name + "\",\"ts\":" +
                               std::to_string(GetHAL().millis()) + "}";
            sendPacket(DataType::HeadTouchEvent, (const uint8_t*)json.c_str(), json.size());
        });

        GetHAL().onScreenTouchEvent.connect([this](const ScreenTouchEvent_t& event) {
            if (!isConnected()) {
                return;
            }
            static bool last_pressed = false;
            const char* state = event.pressed && !last_pressed ? "down" : (!event.pressed && last_pressed ? "up" : "move");
            last_pressed = event.pressed;
            std::string json = std::string("{\"state\":\"") + state + "\",\"x\":" + std::to_string(event.x) +
                               ",\"y\":" + std::to_string(event.y) + ",\"pressed\":" +
                               (event.pressed ? "true" : "false") + ",\"ts\":" + std::to_string(event.tsMs) + "}";
            sendPacket(DataType::ScreenTouchEvent, (const uint8_t*)json.c_str(), json.size());
        });

        GetHAL().Mic().SetFrameCallback([this](const stackchan::hal::MicFrame& frame) {
            if (!isConnected()) {
                return;
            }
            std::vector<uint8_t> payload;
            payload.reserve(16 + frame.opusPayload.size());
            appendBe32(payload, frame.streamHash);
            appendBe32(payload, frame.seq);
            appendBe64(payload, frame.timestampMs);
            payload.insert(payload.end(), frame.opusPayload.begin(), frame.opusPayload.end());
            sendPacket(DataType::MicAudio, payload.data(), payload.size());
        });

        GetHAL().Mic().SetStatusCallback([this](const stackchan::hal::MicStatusEvent& event) {
            if (!isConnected()) {
                return;
            }
            ArduinoJson::JsonDocument doc;
            doc["event"] = event.kind == stackchan::hal::MicStatusEvent::Kind::Started    ? "started"
                           : event.kind == stackchan::hal::MicStatusEvent::Kind::Stopped ? "stopped"
                                                                                         : "stats";
            doc["stream_id"] = event.streamId;
            if (event.kind == stackchan::hal::MicStatusEvent::Kind::Started) {
                doc["sample_rate"]       = event.startedConfig.sampleRate;
                doc["channels"]          = event.startedConfig.channels;
                doc["frame_duration_ms"] = event.startedConfig.frameDurationMs;
                doc["duration_ms"]       = event.startedConfig.durationMs;
            } else {
                doc["frames"] = event.frames;
                doc["bytes"]  = event.bytes;
                if (event.kind == stackchan::hal::MicStatusEvent::Kind::Stopped) {
                    doc["reason"] = event.reason;
                }
            }
            std::string out;
            ArduinoJson::serializeJson(doc, out);
            sendPacket(DataType::MicStatus, (const uint8_t*)out.c_str(), out.size());
        });

        GetHAL().Speaker().SetStatusCallback([this](const stackchan::hal::SpeakerStatusEvent& event) {
            if (!isConnected()) {
                return;
            }
            ArduinoJson::JsonDocument doc;
            doc["event"] = event.kind == stackchan::hal::SpeakerStatusEvent::Kind::Started    ? "started"
                           : event.kind == stackchan::hal::SpeakerStatusEvent::Kind::Stopped ? "stopped"
                                                                                              : "stats";
            doc["stream_id"] = event.streamId;
            if (event.kind == stackchan::hal::SpeakerStatusEvent::Kind::Started) {
                doc["sample_rate"]       = event.startedConfig.sampleRate;
                doc["channels"]          = event.startedConfig.channels;
                doc["frame_duration_ms"] = event.startedConfig.frameDurationMs;
                doc["duration_ms"]       = event.startedConfig.durationMs;
            } else {
                doc["frames"] = event.frames;
                doc["bytes"]  = event.bytes;
                if (event.kind == stackchan::hal::SpeakerStatusEvent::Kind::Stopped) {
                    doc["reason"] = event.reason;
                }
            }
            std::string out;
            ArduinoJson::serializeJson(doc, out);
            sendPacket(DataType::AudioStatus, (const uint8_t*)out.c_str(), out.size());
        });
    }

    void connect()
    {
        auto token = _auth_token.empty() ? secret_logic::generate_auth_token() : _auth_token;

        // 销毁旧实例，确保状态复位
        _websocket.reset();

        auto& board  = Board::GetInstance();
        auto network = board.GetNetwork();

        // 创建 WebSocket 实例
        _websocket = network->CreateWebSocket(1);

        if (!_websocket) {
            ESP_LOGE(_tag.c_str(), "Failed to create websocket");
            return;
        }

        // 设置认证头
        _websocket->SetHeader("Authorization", token.c_str());
        if (_home_agent_enabled) {
            _websocket->SetHeader("X-StackChan-Role", "device");
            _websocket->SetHeader("X-StackChan-Device-Id", _device_id.c_str());
        }

        // 设置回调
        _websocket->OnConnected([this]() {
            ESP_LOGI(_tag.c_str(), "Connected to server!");
            GetHAL().onWsLog.emit(CommonLogLevel::Info, _home_agent_enabled ? "Relay connected" : "Server connected");
            _last_heartbeat_time = GetHAL().millis();
            setRelayLinkAlive(true);
            _websocket->Send("{\"type\":\"hello\", \"msg\":\"Hello from StackChan!\"}");
        });

        _websocket->OnDisconnected([this]() {
            ESP_LOGI(_tag.c_str(), "Disconnected!");
            GetHAL().onWsLog.emit(CommonLogLevel::Error, _home_agent_enabled ? "Relay disconnected" : "Server disconnected");
            setRelayLinkAlive(false);
        });

        _websocket->OnData([this](const char* data, size_t len, bool binary) {
            std::lock_guard<std::mutex> lock(_mutex);
            _msg_queue.push({binary, std::vector<uint8_t>(data, data + len)});
        });

        ESP_LOGI(_tag.c_str(), "Connecting to %s", _url.c_str());
        // GetHAL().onWsLog.emit(CommonLogLevel::Info, "Connecting to server...");
        if (!_websocket->Connect(_url.c_str())) {
            ESP_LOGE(_tag.c_str(), "Failed to connect");
            GetHAL().onWsLog.emit(CommonLogLevel::Error, "Connect to server Failed");
            setRelayLinkAlive(false);
        }
        _last_reconnect_attempt = GetHAL().millis();
    }

    void update()
    {
        if (!_websocket) {
            return;
        }

        if (!_websocket->IsConnected()) {
            if (GetHAL().millis() - _last_reconnect_attempt > 5000) {
                connect();
            }
        } else {
            processMessages();

            // Check heartbeat timeout
            if (GetHAL().millis() - _last_heartbeat_time > 10000) {
                ESP_LOGE(_tag.c_str(), "Heartbeat timeout!");
                GetHAL().onWsLog.emit(CommonLogLevel::Error, "Heartbeat Timeout");
                _last_heartbeat_time = GetHAL().millis();
                setRelayLinkAlive(false);
                return;
            }
        }

        if (_is_streaming) {
            if (GetHAL().millis() - _last_capture_time >= (_is_video_mode ? 700 : 350)) {
                captureAndSendFrame();
                _last_capture_time = GetHAL().millis();
            }
        }
    }

    void processMessages()
    {
        std::vector<ReceivedMessage> messages;
        {
            std::lock_guard<std::mutex> lock(_mutex);
            while (!_msg_queue.empty()) {
                messages.push_back(std::move(_msg_queue.front()));
                _msg_queue.pop();
            }
        }

        for (const auto& msg : messages) {
            handleMessage(msg);
        }
    }

    void handleMessage(const ReceivedMessage& msg)
    {
        if (msg.binary) {
            if (msg.data.size() < 1) return;
            DataType type = static_cast<DataType>(msg.data[0]);
            ESP_LOGI(_tag.c_str(), "Received binary type: %d, len: %d", (int)type, (int)msg.data.size());

            switch (type) {
                // Opus handled in OnData Fast Path
                // case DataType::Opus: {
                //     if (msg.data.size() > 5) {
                //         auto packet = std::make_unique<AudioStreamPacket>();
                //         packet->payload.assign(msg.data.begin() + 5, msg.data.end());
                //         _audio_service.PushPacketToDecodeQueue(std::move(packet));
                //     }
                //     break;
                // }
                case DataType::StartCameraStream: {
                    ESP_LOGI(_tag.c_str(), "Start Camera Stream");
                    setStreamingEnabled(true);
                    _websocket->Send("camera stream started");
                    break;
                }
                case DataType::StopCameraStream: {
                    ESP_LOGI(_tag.c_str(), "Stop Camera Stream");
                    setStreamingEnabled(false);
                    _websocket->Send("camera stream stopped");
                    break;
                }
                case DataType::ControlAvatar: {
                    // Protocol: [Type(1)] [Length(4)] [Payload]
                    if (msg.data.size() >= 5) {
                        std::string payload(msg.data.begin() + 5, msg.data.end());
                        // ESP_LOGI(_tag.c_str(), "Control Avatar Payload: %s", payload.c_str());
                        GetHAL().onWsAvatarData.emit(payload);
                    }
                    break;
                }
                case DataType::ControlMotion: {
                    // Protocol: [Type(1)] [Length(4)] [Payload]
                    if (msg.data.size() >= 5) {
                        std::string payload(msg.data.begin() + 5, msg.data.end());
                        // ESP_LOGI(_tag.c_str(), "Control Motion Payload: %s", payload.c_str());
                        GetHAL().onWsMotionData.emit(payload);
                    }
                    break;
                }
                case DataType::RequestCall: {
                    // Protocol: [Type(1)] [Length(4)] [Payload]
                    if (msg.data.size() >= 5) {
                        std::string payload(msg.data.begin() + 5, msg.data.end());
                        ESP_LOGI(_tag.c_str(), "RequestCall Payload: %s", payload.c_str());
                        GetHAL().onWsCallRequest.emit(payload);
                    }
                    break;
                }
                case DataType::EndCall: {
                    ESP_LOGI(_tag.c_str(), "EndCall");
                    GetHAL().onWsCallEnd.emit(WsSignalSource::Remote);
                    break;
                }
                case DataType::SetDeviceName: {
                    // Protocol: [Type(1)] [Length(4)] [Payload]
                    if (msg.data.size() >= 5) {
                        std::string payload(msg.data.begin() + 5, msg.data.end());
                        ESP_LOGI(_tag.c_str(), "SetDeviceName Payload: %s", payload.c_str());

                        Settings settings(_setting_ns, true);
                        settings.SetString(_setting_device_name_key, payload);
                    }
                    break;
                }
                case DataType::GetDeviceName: {
                    ESP_LOGI(_tag.c_str(), "GetDeviceName");

                    Settings settings(_setting_ns, false);
                    auto device_name = settings.GetString(_setting_device_name_key, "StackChan");

                    sendPacket(DataType::GetDeviceName, (const uint8_t*)device_name.c_str(), device_name.size());
                    break;
                }
                case DataType::HeartbeatPing: {
                    ESP_LOGI(_tag.c_str(), "HeartbeatPing");
                    _last_heartbeat_time = GetHAL().millis();
                    setRelayLinkAlive(true);
                    sendPacket(DataType::HeartbeatPong, nullptr, 0);
                    break;
                }
                case DataType::TextMessage: {
                    // Protocol: [Type(1)] [Length(4)] [Payload]
                    if (msg.data.size() >= 5) {
                        std::string payload(msg.data.begin() + 5, msg.data.end());
                        ESP_LOGI(_tag.c_str(), "TextMessage Payload: %s", payload.c_str());

                        ArduinoJson::JsonDocument doc;
                        auto error = ArduinoJson::deserializeJson(doc, payload);
                        if (error) {
                            ESP_LOGE(_tag.c_str(), "DeserializeJson failed: %s", error.c_str());
                            return;
                        }

                        WsTextMessage_t text_msg;

                        if (doc["name"].is<std::string>()) {
                            text_msg.name = doc["name"].as<std::string>();
                        }
                        if (doc["content"].is<std::string>()) {
                            text_msg.content = doc["content"].as<std::string>();
                        }

                        GetHAL().onWsTextMessage.emit(text_msg);
                    }
                    break;
                }
                case DataType::VideoModeOn: {
                    ESP_LOGI(_tag.c_str(), "VideoModeOn");
                    GetHAL().onWsVideoModeChange.emit(true);
                    _is_video_mode = true;
                    break;
                }
                case DataType::VideoModeOff: {
                    ESP_LOGI(_tag.c_str(), "VideoModeOff");
                    GetHAL().onWsVideoModeChange.emit(false);
                    _is_video_mode = false;
                    break;
                }
                case DataType::Jpeg: {
                    // Protocol: [Type(1)] [Length(4)] [Payload]
                    if (msg.data.size() >= 5) {
                        ESP_LOGI(_tag.c_str(), "Jpeg Frame Received, size: %d", (int)(msg.data.size() - 5));

                        static int64_t _time_count = 0;
                        static int64_t _interval   = 0;
                        _time_count                = esp_timer_get_time();

                        size_t jpeg_len    = msg.data.size() - 5;
                        uint8_t* jpeg_data = (uint8_t*)heap_caps_malloc(jpeg_len, MALLOC_CAP_8BIT);
                        if (jpeg_data) {
                            memcpy(jpeg_data, msg.data.data() + 5, jpeg_len);

                            auto image = jpeg_dec::decode_to_lvgl(jpeg_data, jpeg_len);
                            if (image) {
                                // ESP_LOGI(_tag.c_str(), "Done");

                                _interval = esp_timer_get_time() - _time_count;
                                mclog::info("jpeg decode time: {} ms", _interval / 1000);

                                GetHAL().onWsVideoFrame.emit(image);
                            } else {
                                ESP_LOGE(_tag.c_str(), "Failed to decode JPEG");
                            }
                            heap_caps_free(jpeg_data);
                        } else {
                            ESP_LOGE(_tag.c_str(), "Failed to allocate memory for JPEG");
                        }
                    }
                    break;
                }
                case DataType::DanceSequence: {
                    // Protocol: [Type(1)] [Length(4)] [Payload]
                    if (msg.data.size() >= 5) {
                        std::string payload(msg.data.begin() + 5, msg.data.end());
                        // ESP_LOGI(_tag.c_str(), "Dance Payload:\n%s", payload.c_str());
                        ESP_LOGI(_tag.c_str(), "DanceSequence size: %d", (int)payload.size());
                        GetHAL().onWsDanceData.emit(payload);
                    }
                    break;
                }
                case DataType::AimedTakePhoto: {
                    ESP_LOGI(_tag.c_str(), "AimedTakePhoto");
                    captureAndSendFrame(DataType::AimedTakePhoto);
                    break;
                }
                case DataType::GetDeviceInfo: {
                    handleGetDeviceInfo();
                    break;
                }
                case DataType::GetBatteryStatus: {
                    handleGetBatteryStatus();
                    break;
                }
                case DataType::SetBrightness: {
                    if (msg.data.size() >= 5) {
                        std::string payload(msg.data.begin() + 5, msg.data.end());
                        handleSetBrightness(payload);
                    }
                    break;
                }
                case DataType::SetVolume: {
                    if (msg.data.size() >= 5) {
                        std::string payload(msg.data.begin() + 5, msg.data.end());
                        handleSetVolume(payload);
                    }
                    break;
                }
                case DataType::RebootDevice: {
                    ESP_LOGW(_tag.c_str(), "RebootDevice requested by HomeAgent");
                    GetHAL().reboot();
                    break;
                }
                case DataType::FactoryReset: {
                    ESP_LOGW(_tag.c_str(), "FactoryReset requested by HomeAgent");
                    GetHAL().factoryReset();
                    break;
                }
                case DataType::SetRgbLed: {
                    if (msg.data.size() >= 5) {
                        std::string payload(msg.data.begin() + 5, msg.data.end());
                        handleSetRgbLed(payload);
                    }
                    break;
                }
                case DataType::ShowRgbColor: {
                    if (msg.data.size() >= 5) {
                        std::string payload(msg.data.begin() + 5, msg.data.end());
                        handleShowRgbColor(payload);
                    }
                    break;
                }
                case DataType::GetDriverHealth: {
                    handleGetDriverHealth();
                    break;
                }
                case DataType::MicStreamStart: {
                    if (msg.data.size() < 5) {
                        sendCapabilityError("mic", "invalid_args", "missing mic start payload", (int)type);
                        break;
                    }
                    std::string payload(msg.data.begin() + 5, msg.data.end());
                    handleMicStreamStart(payload, type);
                    break;
                }
                case DataType::MicStreamStop: {
                    if (msg.data.size() >= 5) {
                        std::string payload(msg.data.begin() + 5, msg.data.end());
                        handleMicStreamStop(payload, type);
                    } else {
                        GetHAL().Mic().Stop("user");
                    }
                    break;
                }
                case DataType::PlayAudio: {
                    if (msg.data.size() < 5) {
                        break;
                    }
                    const uint8_t* payload = msg.data.data() + 5;
                    size_t payloadLen = msg.data.size() - 5;
                    if (GetHAL().Speaker().IsActive()) {
                        // Feed Opus frame to running speaker stream
                        GetHAL().Speaker().FeedFrame(payload, payloadLen);
                    } else {
                        // First frame: parse JSON config and start speaker
                        std::string jsonStr(reinterpret_cast<const char*>(payload), payloadLen);
                        handlePlayAudioStart(jsonStr, type);
                    }
                    break;
                }
                case DataType::StartAudioStream: {
                    // Used as PlayAudioStop
                    break;
                }
                case DataType::StopAudioStream: {
                    GetHAL().Speaker().Stop("user");
                    break;
                }
                case DataType::IrSend:
                case DataType::IrLearnStart:
                case DataType::NfcRead:
                case DataType::NfcWrite:
                case DataType::ScreenCapture:
                case DataType::SdList:
                case DataType::SdRead:
                case DataType::SdWrite: {
                    ESP_LOGW(_tag.c_str(), "Capability 0x%02X not implemented in firmware yet", (int)type);
                    sendCapabilityError(type, "not_implemented", "capability exists but hardware driver is not implemented");
                    break;
                }
                default:
                    break;
            }
        } else {
            ESP_LOGI(_tag.c_str(), "Received text: %.*s", (int)msg.data.size(), (char*)msg.data.data());
        }
    }

    bool isConnected()
    {
        return _websocket && _websocket->IsConnected();
    }

    void captureAndSendFrame(DataType response_type = DataType::Jpeg)
    {
        if (!isConnected()) {
            return;
        }

        static int64_t _time_count = 0;
        static int64_t _interval   = 0;

        auto camera = hal_bridge::board_get_camera();
        if (!camera) {
            return;
        }

        _time_count = esp_timer_get_time();
        if (camera->StreamCaptures()) {
            _interval = esp_timer_get_time() - _time_count;
            mclog::info("camera capture time: {} ms", _interval / 1000);

            const uint8_t* frameData = camera->GetFrameData();
            size_t frameSize         = camera->GetFrameSize();
            int width                = camera->GetFrameWidth();
            int height               = camera->GetFrameHeight();
            int format               = camera->GetFrameFormat();

            uint8_t* jpeg_data = nullptr;
            size_t jpeg_len    = 0;

            // 压缩为 JPEG
            _time_count = esp_timer_get_time();
            if (image_to_jpeg((uint8_t*)frameData, frameSize, width, height, (v4l2_pix_fmt_t)format, 20, &jpeg_data,
                              &jpeg_len)) {
                _interval = esp_timer_get_time() - _time_count;
                // mclog::info("jpeg encode time: {} ms, size: {}", _interval / 1000, jpeg_len);
                mclog::info("jpeg encode time: {} ms", _interval / 1000);

                if (jpeg_data) {
                    sendPacket(response_type, jpeg_data, jpeg_len);
                    free(jpeg_data);
                }
            }
        }
    }

    void setStreamingEnabled(bool enabled)
    {
        _is_streaming = enabled;
    }

    void setRelayLinkAlive(bool alive)
    {
        if (_relay_link_alive == alive && _relay_link_initialized) {
            return;
        }
        _relay_link_alive       = alive;
        _relay_link_initialized = true;
        GetHAL().onWsRelayLink.emit(alive);
    }

private:
    std::unique_ptr<WebSocket> _websocket;
    std::string _url;
    uint32_t _last_reconnect_attempt = 0;
    uint32_t _last_capture_time      = 0;
    uint32_t _last_heartbeat_time    = 0;
    bool _is_streaming               = false;
    bool _is_video_mode              = false;
    bool _relay_link_alive           = false;
    bool _relay_link_initialized     = false;
    std::mutex _mutex;
    std::queue<ReceivedMessage> _msg_queue;

    std::mutex _send_mutex;
    bool _home_agent_enabled = false;
    std::string _device_id;
    std::string _auth_token;

    static bool startsWith(const std::string& value, const char* prefix)
    {
        return value.rfind(prefix, 0) == 0;
    }

    static char joinerForUrl(const std::string& url)
    {
        return url.find('?') == std::string::npos ? '?' : '&';
    }

    void loadConfig()
    {
        auto config = GetHAL().getHomeAgentConfig();
        _home_agent_enabled = config.enabled && !config.relayUrl.empty();
        _device_id = config.deviceId.empty() ? GetHAL().getFactoryMacString("") : config.deviceId;
        _auth_token = config.token;

        if (_home_agent_enabled) {
            _url = config.relayUrl;
            if (!startsWith(_url, "ws://") && !startsWith(_url, "wss://")) {
                _url = std::string("wss://") + _url;
            }
            if (_url.find("role=") == std::string::npos) {
                _url += joinerForUrl(_url);
                _url += "role=device";
            }
            if (_url.find("deviceId=") == std::string::npos) {
                _url += joinerForUrl(_url);
                _url += "deviceId=" + _device_id;
            }
        } else {
            _url = fmt::format("{}/stackChan/ws?deviceType=StackChan", secret_logic::get_server_url());
        }
    }

    // ---------- HomeAgent 扩展能力实现 ----------

    static std::string ssidOfWifiStatus(WifiStatus s)
    {
        switch (s) {
            case WifiStatus::Low: return "low";
            case WifiStatus::Medium: return "medium";
            case WifiStatus::High: return "high";
            default: return "none";
        }
    }

    void handleGetDeviceInfo()
    {
        auto& hal = GetHAL();
        Settings settings(_setting_ns, false);
        auto deviceName = settings.GetString(_setting_device_name_key, "StackChan");

        ArduinoJson::JsonDocument doc;
        doc["mac"]        = hal.getFactoryMacString("");
        doc["deviceName"] = deviceName;
        doc["firmware"]   = FIRMWARE_VERSION;
        doc["battery"]    = hal.getBatteryLevel();
        doc["charging"]   = hal.isBatteryCharging();
        doc["brightness"] = hal.getBackLightBrightness();
        doc["volume"]     = hal.getSpeakerVolume();
        doc["wifiSsid"]   = hal.getCurrentWifiSsid();
        doc["wifiIp"]     = hal.getWifiIpAddress();
        doc["wifiSignal"] = ssidOfWifiStatus(hal.getWifiStatus());
        doc["freeHeap"]   = (uint32_t)heap_caps_get_free_size(MALLOC_CAP_DEFAULT);
        doc["psramFree"]  = (uint32_t)heap_caps_get_free_size(MALLOC_CAP_SPIRAM);

        std::string out;
        ArduinoJson::serializeJson(doc, out);
        sendPacket(DataType::GetDeviceInfo, (const uint8_t*)out.c_str(), out.size());
    }

    void handleGetBatteryStatus()
    {
        auto& hal = GetHAL();
        ArduinoJson::JsonDocument doc;
        doc["level"]    = hal.getBatteryLevel();
        doc["charging"] = hal.isBatteryCharging();
        doc["ts"]       = hal.millis();
        std::string out;
        ArduinoJson::serializeJson(doc, out);
        sendPacket(DataType::GetBatteryStatus, (const uint8_t*)out.c_str(), out.size());
    }

    static const char* capabilityNameForType(DataType type)
    {
        switch (type) {
            case DataType::IrSend: return "ir.send";
            case DataType::IrLearnStart: return "ir.learn.start";
            case DataType::NfcRead: return "nfc.read";
            case DataType::NfcWrite: return "nfc.write";
            case DataType::PlayAudio: return "audio.play";
            case DataType::StopAudioStream: return "audio.stop";
            case DataType::MicStreamStart: return "mic.start";
            case DataType::MicStreamStop: return "mic.stop";
            case DataType::ScreenCapture: return "screen.capture";
            case DataType::SdList: return "sd.list";
            case DataType::SdRead: return "sd.read";
            case DataType::SdWrite: return "sd.write";
            default: return "unknown";
        }
    }

    static void appendBe32(std::vector<uint8_t>& out, uint32_t value)
    {
        out.push_back((value >> 24) & 0xFF);
        out.push_back((value >> 16) & 0xFF);
        out.push_back((value >> 8) & 0xFF);
        out.push_back(value & 0xFF);
    }

    static void appendBe64(std::vector<uint8_t>& out, uint64_t value)
    {
        for (int shift = 56; shift >= 0; shift -= 8) {
            out.push_back((value >> shift) & 0xFF);
        }
    }

    void sendCapabilityError(const char* capability, const char* code, const char* message, int typeValue)
    {
        ArduinoJson::JsonDocument doc;
        if (typeValue >= 0) {
            doc["type"] = typeValue;
        }
        doc["capability"] = capability;
        doc["code"]       = code;
        doc["message"]    = message;
        doc["ts"]         = GetHAL().millis();
        doc["details"]    = ArduinoJson::JsonObject();
        std::string out;
        ArduinoJson::serializeJson(doc, out);
        sendPacket(DataType::CapabilityError, (const uint8_t*)out.c_str(), out.size());
    }

    void sendCapabilityError(DataType type, const char* code, const char* message)
    {
        sendCapabilityError(capabilityNameForType(type), code, message, (int)type);
    }

    void handleGetDriverHealth()
    {
        auto now = GetHAL().millis();
        ArduinoJson::JsonDocument doc;
        auto drivers      = doc["drivers"].to<ArduinoJson::JsonArray>();
        auto appendDriver = [&](const char* name, bool ready, const char* lastError) {
            auto d        = drivers.add<ArduinoJson::JsonObject>();
            d["name"]     = name;
            d["ready"]    = ready;
            d["lastError"] = lastError;
            d["lastTickMs"] = now;
        };

        appendDriver("ir", false, "driver_unavailable");
        appendDriver("nfc", false, "driver_unavailable");
        appendDriver("ambientLight", false, "driver_unavailable");

        std::string out;
        ArduinoJson::serializeJson(doc, out);
        sendPacket(DataType::GetDriverHealth, (const uint8_t*)out.c_str(), out.size());
    }

    void handleMicStreamStart(const std::string& payload, DataType requestType)
    {
        ArduinoJson::JsonDocument doc;
        auto error = ArduinoJson::deserializeJson(doc, payload);
        if (error) {
            sendCapabilityError("mic", "invalid_args", "bad mic start json", (int)requestType);
            return;
        }

        stackchan::hal::MicStreamConfig config;
        if (doc["sample_rate"].is<uint32_t>()) {
            config.sampleRate = doc["sample_rate"].as<uint32_t>();
        }
        if (doc["channels"].is<uint8_t>()) {
            config.channels = doc["channels"].as<uint8_t>();
        }
        if (doc["frame_duration_ms"].is<uint16_t>()) {
            config.frameDurationMs = doc["frame_duration_ms"].as<uint16_t>();
        }
        if (doc["duration_ms"].is<uint32_t>()) {
            config.durationMs = doc["duration_ms"].as<uint32_t>();
        }
        if (doc["stream_id"].is<std::string>()) {
            config.streamId = doc["stream_id"].as<std::string>();
        }

        std::string err;
        if (!GetHAL().Mic().Start(config, &err)) {
            const char* code = err.empty() ? "unavailable" : err.c_str();
            sendCapabilityError("mic", code, "mic stream start failed", (int)requestType);
        }
    }

    void handleMicStreamStop(const std::string& payload, DataType requestType)
    {
        if (!payload.empty()) {
            ArduinoJson::JsonDocument doc;
            auto error = ArduinoJson::deserializeJson(doc, payload);
            if (error) {
                sendCapabilityError("mic", "invalid_args", "bad mic stop json", (int)requestType);
                return;
            }
        }
        GetHAL().Mic().Stop("user");
    }

    void handlePlayAudioStart(const std::string& payload, DataType requestType)
    {
        ArduinoJson::JsonDocument doc;
        auto error = ArduinoJson::deserializeJson(doc, payload);
        if (error) {
            sendCapabilityError("audio.play", "invalid_args", "bad audio play json", (int)requestType);
            return;
        }

        stackchan::hal::SpeakerStreamConfig config;
        if (doc["sample_rate"].is<uint32_t>()) {
            config.sampleRate = doc["sample_rate"].as<uint32_t>();
        }
        if (doc["channels"].is<uint8_t>()) {
            config.channels = doc["channels"].as<uint8_t>();
        }
        if (doc["frame_duration_ms"].is<uint16_t>()) {
            config.frameDurationMs = doc["frame_duration_ms"].as<uint16_t>();
        }
        if (doc["duration_ms"].is<uint32_t>()) {
            config.durationMs = doc["duration_ms"].as<uint32_t>();
        }
        if (doc["stream_id"].is<std::string>()) {
            config.streamId = doc["stream_id"].as<std::string>();
        }

        std::string err;
        if (!GetHAL().Speaker().Start(config, &err)) {
            const char* code = err.empty() ? "unavailable" : err.c_str();
            sendCapabilityError("audio.play", code, "audio play start failed", (int)requestType);
        }
    }

    void handleSetBrightness(const std::string& payload)
    {
        ArduinoJson::JsonDocument doc;
        if (ArduinoJson::deserializeJson(doc, payload)) return;
        int value = doc["value"].is<int>() ? doc["value"].as<int>() : -1;
        if (value < 0 || value > 100) {
            ESP_LOGW(_tag.c_str(), "SetBrightness invalid value");
            return;
        }
        bool permanent = doc["permanent"].is<bool>() ? doc["permanent"].as<bool>() : false;
        GetHAL().setBackLightBrightness((uint8_t)value, permanent);
    }

    void handleSetVolume(const std::string& payload)
    {
        ArduinoJson::JsonDocument doc;
        if (ArduinoJson::deserializeJson(doc, payload)) return;
        int value = doc["value"].is<int>() ? doc["value"].as<int>() : -1;
        if (value < 0 || value > 100) {
            ESP_LOGW(_tag.c_str(), "SetVolume invalid value");
            return;
        }
        bool permanent = doc["permanent"].is<bool>() ? doc["permanent"].as<bool>() : false;
        GetHAL().setSpeakerVolume((uint8_t)value, permanent);
    }

    static uint8_t clamp8(int v)
    {
        if (v < 0) return 0;
        if (v > 255) return 255;
        return (uint8_t)v;
    }

    void handleSetRgbLed(const std::string& payload)
    {
        ArduinoJson::JsonDocument doc;
        if (ArduinoJson::deserializeJson(doc, payload)) {
            ESP_LOGW(_tag.c_str(), "SetRgbLed parse failed");
            return;
        }

        // 支持 {"leds":[...]} 或裸数组 [...]
        ArduinoJson::JsonArrayConst arr;
        if (doc["leds"].is<ArduinoJson::JsonArrayConst>()) {
            arr = doc["leds"].as<ArduinoJson::JsonArrayConst>();
        } else if (doc.is<ArduinoJson::JsonArrayConst>()) {
            arr = doc.as<ArduinoJson::JsonArrayConst>();
        } else {
            ESP_LOGW(_tag.c_str(), "SetRgbLed expects leds array");
            return;
        }

        auto& hal = GetHAL();
        for (auto v : arr) {
            int i = v["i"].is<int>() ? v["i"].as<int>() : -1;
            if (i < 0 || i > 11) continue;
            uint8_t r = clamp8(v["r"].is<int>() ? v["r"].as<int>() : 0);
            uint8_t g = clamp8(v["g"].is<int>() ? v["g"].as<int>() : 0);
            uint8_t b = clamp8(v["b"].is<int>() ? v["b"].as<int>() : 0);
            hal.setRgbColor((uint8_t)i, r, g, b);
        }
        hal.refreshRgb();
    }

    void handleShowRgbColor(const std::string& payload)
    {
        ArduinoJson::JsonDocument doc;
        if (ArduinoJson::deserializeJson(doc, payload)) {
            ESP_LOGW(_tag.c_str(), "ShowRgbColor parse failed");
            return;
        }
        uint8_t r = 0, g = 0, b = 0;
        if (doc["r"].is<int>()) {
            r = clamp8(doc["r"].as<int>());
            g = clamp8(doc["g"].as<int>());
            b = clamp8(doc["b"].as<int>());
        } else if (doc["color"].is<std::string>()) {
            std::string color = doc["color"].as<std::string>();
            if (color.size() == 7 && color[0] == '#') color = color.substr(1);
            if (color.size() == 6) {
                auto hex = [](char c) -> int {
                    if (c >= '0' && c <= '9') return c - '0';
                    if (c >= 'a' && c <= 'f') return c - 'a' + 10;
                    if (c >= 'A' && c <= 'F') return c - 'A' + 10;
                    return -1;
                };
                int rh1 = hex(color[0]), rh2 = hex(color[1]);
                int gh1 = hex(color[2]), gh2 = hex(color[3]);
                int bh1 = hex(color[4]), bh2 = hex(color[5]);
                if (rh1 >= 0 && rh2 >= 0 && gh1 >= 0 && gh2 >= 0 && bh1 >= 0 && bh2 >= 0) {
                    r = (rh1 << 4) | rh2;
                    g = (gh1 << 4) | gh2;
                    b = (bh1 << 4) | bh2;
                }
            }
        }
        GetHAL().showRgbColor(r, g, b);
    }

    void sendPacket(DataType type, const uint8_t* data, size_t len)
    {
        std::lock_guard<std::mutex> lock(_send_mutex);
        if (!_websocket || !_websocket->IsConnected()) {
            return;
        }

        // mclog::info("sending packet type: {}, len: {}", (int)type, (int)len);

        // static int64_t _time_count = 0;
        // static int64_t _interval   = 0;
        // _time_count                = esp_timer_get_time();

        std::vector<uint8_t> packet;
        packet.reserve(1 + 4 + len);

        // [1 byte type]
        packet.push_back(static_cast<uint8_t>(type));

        // [4 bytes length] (Big Endian)
        uint32_t net_len       = htonl((uint32_t)len);
        const uint8_t* len_ptr = (const uint8_t*)&net_len;
        packet.push_back(len_ptr[0]);
        packet.push_back(len_ptr[1]);
        packet.push_back(len_ptr[2]);
        packet.push_back(len_ptr[3]);

        // [payload]
        if (len > 0) {
            packet.insert(packet.end(), data, data + len);
        }

        // _interval = esp_timer_get_time() - _time_count;
        // mclog::info("pack time: {} ms, size: {}", _interval / 1000, packet.size());

        // _time_count = esp_timer_get_time();
        _websocket->Send(packet.data(), packet.size(), true);
        // _interval = esp_timer_get_time() - _time_count;
        // mclog::info("send time: {} ms, size: {}", _interval / 1000, packet.size());
    }
};

class WebsocketAvatarWorker : public mooncake::BasicAbility {
public:
    WebsocketAvatarWorker()
    {
        _service = std::make_unique<WebSocketAvatar>();
        _service->init();
    }

    void onCreate() override
    {
    }

    void onRunning() override
    {
        if (!_service) {
            return;
        }
        if (GetHAL().millis() - _last_tick < 20) {
            return;
        }
        _last_tick = GetHAL().millis();
        _service->update();
    }

    void onDestroy() override
    {
        _service.reset();
    }

    void stop()
    {
        _service.reset();
    }

private:
    std::unique_ptr<WebSocketAvatar> _service;
    uint32_t _last_tick = 0;
};

static WebsocketAvatarWorker* _ws_avatar_worker = nullptr;

bool Hal::startWebSocketAvatarService(std::function<void(std::string_view)> onStartLog, uint32_t networkTimeoutMs)
{
    mclog::tagInfo(_tag, "start websocket avatar service");

    if (!startNetwork(onStartLog, networkTimeoutMs)) {
        return false;
    }

    onStartLog("Connecting to\nserver...");

    if (_ws_avatar_worker) {
        _ws_avatar_worker->stop();
        _ws_avatar_worker = nullptr;
    }

    auto worker = std::make_unique<WebsocketAvatarWorker>();
    _ws_avatar_worker = worker.get();
    mooncake::GetMooncake().extensionManager()->createAbility(std::move(worker));
    return true;
}

void Hal::stopWebSocketAvatarService()
{
    if (_ws_avatar_worker) {
        _ws_avatar_worker->stop();
        _ws_avatar_worker = nullptr;
    }
}

HomeAgentConfig_t Hal::getHomeAgentConfig()
{
    Settings settings(_home_agent_setting_ns, false);
    HomeAgentConfig_t config;
    config.enabled = settings.GetBool("enabled", true);
    config.relayUrl = settings.GetString("relay_url", _home_agent_default_relay_url);
    config.deviceId = settings.GetString("device_id", _home_agent_default_device_id);
    config.token = settings.GetString("token", _home_agent_default_token);

    if (config.relayUrl.empty()) {
        config.enabled = true;
        config.relayUrl = _home_agent_default_relay_url;
    }
    if (config.deviceId.empty()) {
        config.deviceId = _home_agent_default_device_id;
    }
    if (config.token.empty()) {
        config.token = _home_agent_default_token;
    }

    return config;
}

void Hal::setHomeAgentConfig(const HomeAgentConfig_t& config)
{
    Settings settings(_home_agent_setting_ns, true);
    settings.SetBool("enabled", config.enabled);
    settings.SetString("relay_url", config.relayUrl);
    settings.SetString("device_id", config.deviceId.empty() ? getFactoryMacString("") : config.deviceId);
    settings.SetString("token", config.token);
}

void Hal::resetHomeAgentConfig()
{
    Settings settings(_home_agent_setting_ns, true);
    settings.SetBool("enabled", false);
    settings.SetString("relay_url", "");
    settings.SetString("device_id", getFactoryMacString(""));
    settings.SetString("token", "");
}
