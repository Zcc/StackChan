package main

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"gopkg.in/hraban/opus.v2"
)

// Edge-TTS constants
const (
	edgeTTSEndpoint = "wss://speech.platform.bing.com/consumer/speech/synthesize/readaloud/edge/v1"
	edgeTTSToken    = "6A5AA1D4EAFF4E9FB37E23D68491D6F4"
	// Request raw 24kHz 16-bit mono PCM — no MP3 decode needed
	edgeTTSOutputFormat = "raw-24khz-16bit-mono-pcm"
	edgeTTSDefaultVoice = "zh-CN-XiaoxiaoNeural"

	// Opus encoding params matching firmware SpeakerSubsystem expectations
	ttsOpusSampleRate      = 16000
	ttsOpusChannels        = 1
	ttsOpusFrameDurationMs = 60
	ttsOpusFrameSize       = ttsOpusSampleRate * ttsOpusFrameDurationMs / 1000 // 960 samples
)

type ttsRequest struct {
	Text       string `json:"text"`
	Voice      string `json:"voice,omitempty"`
	DurationMs int    `json:"duration_ms,omitempty"`
}

// handleTTSSpeak handles POST /tts/speak — converts text to speech and streams
// audio to the firmware.
func (b *bridge) handleTTSSpeak(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if b.audio == nil {
		b.audio = newAudioState()
	}

	var req ttsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		http.Error(w, `{"code":"invalid_args","message":"text is required"}`, http.StatusBadRequest)
		return
	}
	if req.Voice == "" {
		req.Voice = edgeTTSDefaultVoice
	}
	dur := req.DurationMs
	if dur <= 0 {
		dur = audioDefaultDurationMs
	}
	if dur > audioMaxDurationMs {
		dur = audioMaxDurationMs
	}

	streamID := newStreamID()
	if !b.audio.beginStart(streamID, dur) {
		http.Error(w, `{"code":"audio.busy"}`, http.StatusConflict)
		return
	}

	// Send config frame to firmware to start speaker
	cfg := map[string]any{
		"sample_rate":       ttsOpusSampleRate,
		"channels":          ttsOpusChannels,
		"frame_duration_ms": ttsOpusFrameDurationMs,
		"duration_ms":       dur,
		"stream_id":         streamID,
	}
	payload, _ := json.Marshal(cfg)
	if err := b.sendAudioPacket(playAudio, payload); err != nil {
		b.audio.markStopped(streamID, "error", 0, 0)
		http.Error(w, "device send failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Launch streaming goroutine
	go b.streamTTS(streamID, req.Text, req.Voice)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"stream_id": streamID,
		"status":    "streaming",
		"voice":     req.Voice,
	})
}

// streamTTS connects to Edge-TTS, receives raw PCM, encodes to Opus, and
// pushes frames to the firmware via sendPacket.
func (b *bridge) streamTTS(streamID, text, voice string) {
	defer func() {
		// Send stop to firmware
		stopPayload, _ := json.Marshal(map[string]string{"stream_id": streamID})
		_ = b.sendAudioPacket(stopAudioStream, stopPayload)
	}()

	// Create Opus encoder
	enc, err := opus.NewEncoder(ttsOpusSampleRate, ttsOpusChannels, opus.AppVoIP)
	if err != nil {
		log.Printf("tts: opus encoder init failed: %v", err)
		return
	}
	_ = enc.SetBitrate(24000)

	// Connect to Edge-TTS
	pcmCh, errCh := edgeTTSSynthesize(text, voice)

	// PCM accumulator for framing
	var pcmBuf []int16
	opusBuf := make([]byte, 4000)

	for {
		select {
		case chunk, ok := <-pcmCh:
			if !ok {
				// Channel closed — flush remaining
				b.flushOpusFrames(enc, pcmBuf, opusBuf)
				return
			}
			// Resample 24kHz → 16kHz
			resampled := resample24to16(chunk)
			pcmBuf = append(pcmBuf, resampled...)

			// Encode complete frames
			for len(pcmBuf) >= ttsOpusFrameSize {
				frame := pcmBuf[:ttsOpusFrameSize]
				pcmBuf = pcmBuf[ttsOpusFrameSize:]

				n, encErr := enc.Encode(frame, opusBuf)
				if encErr != nil {
					log.Printf("tts: opus encode error: %v", encErr)
					continue
				}
				if sendErr := b.sendAudioPacket(playAudio, opusBuf[:n]); sendErr != nil {
					log.Printf("tts: send frame error: %v", sendErr)
					return
				}
			}

		case ttsErr := <-errCh:
			if ttsErr != nil {
				log.Printf("tts: edge-tts error: %v", ttsErr)
			}
			// Flush remaining PCM
			b.flushOpusFrames(enc, pcmBuf, opusBuf)
			return
		}
	}
}

// flushOpusFrames encodes and sends any remaining PCM samples (zero-padded to
// frame size).
func (b *bridge) flushOpusFrames(enc *opus.Encoder, pcmBuf []int16, opusBuf []byte) {
	if len(pcmBuf) == 0 {
		return
	}
	// Zero-pad to frame size
	padded := make([]int16, ttsOpusFrameSize)
	copy(padded, pcmBuf)
	n, err := enc.Encode(padded, opusBuf)
	if err != nil {
		return
	}
	_ = b.sendAudioPacket(playAudio, opusBuf[:n])
}

// ---------- Edge-TTS WebSocket client ----------

func generateUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%08x%04x%04x%04x%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func edgeTTSTimestamp() string {
	return time.Now().UTC().Format("Mon Jan 02 2006 15:04:05 GMT-0700 (Coordinated Universal Time)")
}

// edgeTTSSynthesize connects to the Edge-TTS WebSocket and returns a channel of
// PCM int16 chunks (24kHz mono) and an error channel.
func edgeTTSSynthesize(text, voice string) (<-chan []int16, <-chan error) {
	pcmCh := make(chan []int16, 32)
	errCh := make(chan error, 1)

	go func() {
		defer close(pcmCh)
		defer close(errCh)

		url := fmt.Sprintf("%s?TrustedClientToken=%s&Sec-MS-GEC=&Sec-MS-GEC-Version=",
			edgeTTSEndpoint, edgeTTSToken)

		dialer := websocket.Dialer{
			HandshakeTimeout: 10 * time.Second,
		}
		headers := http.Header{
			"Pragma":          {"no-cache"},
			"Cache-Control":   {"no-cache"},
			"Origin":          {"chrome-extension://jdiccldimpdaibmpdkjnbmckianbfold"},
			"Accept-Language": {"en-US,en;q=0.9"},
			"User-Agent":      {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36 Edg/130.0.0.0"},
		}

		conn, _, err := dialer.Dial(url, headers)
		if err != nil {
			errCh <- fmt.Errorf("edge-tts connect: %w", err)
			return
		}
		defer conn.Close()

		requestID := generateUUID()

		// Step 1: Send speech.config
		configMsg := fmt.Sprintf(
			"X-Timestamp:%s\r\nContent-Type:application/json; charset=utf-8\r\nPath:speech.config\r\n\r\n"+
				`{"context":{"synthesis":{"audio":{"metadataoptions":{"sentenceBoundaryEnabled":"false","wordBoundaryEnabled":"false"},"outputFormat":"%s"}}}}`,
			edgeTTSTimestamp(), edgeTTSOutputFormat)
		if err := conn.WriteMessage(websocket.TextMessage, []byte(configMsg)); err != nil {
			errCh <- fmt.Errorf("edge-tts send config: %w", err)
			return
		}

		// Step 2: Send SSML
		ssml := fmt.Sprintf(
			"X-RequestId:%s\r\nContent-Type:application/ssml+xml\r\nX-Timestamp:%s\r\nPath:ssml\r\n\r\n"+
				"<speak version='1.0' xmlns='http://www.w3.org/2001/10/synthesis' xml:lang='en-US'>"+
				"<voice name='%s'><prosody rate='+0%%' pitch='+0Hz'>%s</prosody></voice></speak>",
			requestID, edgeTTSTimestamp(), voice, escapeXML(text))
		if err := conn.WriteMessage(websocket.TextMessage, []byte(ssml)); err != nil {
			errCh <- fmt.Errorf("edge-tts send ssml: %w", err)
			return
		}

		// Step 3: Read response frames
		for {
			msgType, data, readErr := conn.ReadMessage()
			if readErr != nil {
				errCh <- fmt.Errorf("edge-tts read: %w", readErr)
				return
			}

			if msgType == websocket.TextMessage {
				// Check for turn.end
				if strings.Contains(string(data), "Path:turn.end") {
					return // done successfully
				}
				continue
			}

			if msgType == websocket.BinaryMessage {
				// Binary frames have a 2-byte header length prefix
				if len(data) < 2 {
					continue
				}
				headerLen := int(binary.BigEndian.Uint16(data[:2]))
				audioData := data[2+headerLen:]
				if len(audioData) == 0 {
					continue
				}

				// Convert bytes to int16 samples (little-endian)
				samples := make([]int16, len(audioData)/2)
				for i := range samples {
					samples[i] = int16(binary.LittleEndian.Uint16(audioData[i*2:]))
				}
				pcmCh <- samples
			}
		}
	}()

	return pcmCh, errCh
}

// escapeXML escapes text for safe embedding in SSML.
func escapeXML(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"'", "&apos;",
		`"`, "&quot;",
	)
	return r.Replace(s)
}
