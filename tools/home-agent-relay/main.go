package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type peer struct {
	conn      *websocket.Conn
	send      chan outbound
	role      string
	deviceID  string
	connected time.Time
	lastSeen  time.Time
}

const heartbeatPing byte = 0x10

type outbound struct {
	messageType int
	data        []byte
}

type room struct {
	device *peer
	agent  *peer
}

var (
	rooms    = map[string]*room{}
	roomsMu  sync.Mutex
	upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
)

func main() {
	addr := flag.String("addr", ":8787", "listen address")
	flag.Parse()

	http.HandleFunc("/ws", handleWS)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	http.HandleFunc("/status", handleStatus)

	log.Printf("home-agent relay listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func handleWS(w http.ResponseWriter, r *http.Request) {
	role := r.URL.Query().Get("role")
	deviceID := r.URL.Query().Get("deviceId")
	if role != "device" && role != "agent" {
		http.Error(w, "role must be device or agent", http.StatusBadRequest)
		return
	}
	if deviceID == "" {
		http.Error(w, "deviceId is required", http.StatusBadRequest)
		return
	}
	if want := os.Getenv("STACKCHAN_RELAY_TOKEN"); want != "" && r.Header.Get("Authorization") != want {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade failed: %v", err)
		return
	}

	p := &peer{
		conn:      conn,
		send:      make(chan outbound, 64),
		role:      role,
		deviceID:  deviceID,
		connected: time.Now(),
		lastSeen:  time.Now(),
	}
	register(deviceID, role, p)
	log.Printf("%s connected: %s", role, deviceID)

	go writeLoop(p)
	readLoop(deviceID, role, p)
}

func register(deviceID, role string, p *peer) {
	roomsMu.Lock()
	defer roomsMu.Unlock()

	r := rooms[deviceID]
	if r == nil {
		r = &room{}
		rooms[deviceID] = r
	}
	var old *peer
	if role == "device" {
		old, r.device = r.device, p
	} else {
		old, r.agent = r.agent, p
	}
	if old != nil {
		_ = old.conn.Close()
	}
}

func unregister(deviceID, role string, p *peer) {
	roomsMu.Lock()
	defer roomsMu.Unlock()

	r := rooms[deviceID]
	if r == nil {
		return
	}
	if role == "device" && r.device == p {
		r.device = nil
	}
	if role == "agent" && r.agent == p {
		r.agent = nil
	}
	if r.device == nil && r.agent == nil {
		delete(rooms, deviceID)
	}
}

func targetPeer(deviceID, role string) *peer {
	roomsMu.Lock()
	defer roomsMu.Unlock()

	r := rooms[deviceID]
	if r == nil {
		return nil
	}
	if role == "device" {
		return r.agent
	}
	return r.device
}

func readLoop(deviceID, role string, p *peer) {
	defer func() {
		unregister(deviceID, role, p)
		_ = p.conn.Close()
		log.Printf("%s disconnected: %s", role, deviceID)
	}()

	p.conn.SetReadLimit(8 << 20)
	_ = p.conn.SetReadDeadline(time.Now().Add(70 * time.Second))
	p.conn.SetPongHandler(func(string) error {
		_ = p.conn.SetReadDeadline(time.Now().Add(70 * time.Second))
		return nil
	})

	for {
		messageType, data, err := p.conn.ReadMessage()
		if err != nil {
			return
		}
		p.lastSeen = time.Now()
		if dst := targetPeer(deviceID, role); dst != nil {
			select {
			case dst.send <- outbound{messageType: messageType, data: append([]byte(nil), data...)}:
			default:
				log.Printf("drop message for slow peer: %s", deviceID)
			}
		}
	}
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	if want := os.Getenv("STACKCHAN_RELAY_TOKEN"); want != "" && r.Header.Get("Authorization") != want {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	type peerState struct {
		Connected bool   `json:"connected"`
		Since     string `json:"since,omitempty"`
		LastSeen  string `json:"lastSeen,omitempty"`
	}
	type roomState struct {
		Device peerState `json:"device"`
		Agent  peerState `json:"agent"`
	}

	roomsMu.Lock()
	out := map[string]roomState{}
	for deviceID, room := range rooms {
		state := roomState{}
		if room.device != nil {
			state.Device = peerState{Connected: true, Since: room.device.connected.Format(time.RFC3339), LastSeen: room.device.lastSeen.Format(time.RFC3339)}
		}
		if room.agent != nil {
			state.Agent = peerState{Connected: true, Since: room.agent.connected.Format(time.RFC3339), LastSeen: room.agent.lastSeen.Format(time.RFC3339)}
		}
		out[deviceID] = state
	}
	roomsMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"rooms": out,
		"count": len(out),
	})
}

func writeLoop(p *peer) {
	wsTicker := time.NewTicker(25 * time.Second)
	protocolTicker := time.NewTicker(5 * time.Second)
	defer wsTicker.Stop()
	defer protocolTicker.Stop()

	for {
		select {
		case msg, ok := <-p.send:
			if !ok {
				return
			}
			_ = p.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := p.conn.WriteMessage(msg.messageType, msg.data); err != nil {
				return
			}
		case <-protocolTicker.C:
			_ = p.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := p.conn.WriteMessage(websocket.BinaryMessage, heartbeatPacket()); err != nil {
				return
			}
		case <-wsTicker.C:
			_ = p.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := p.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func heartbeatPacket() []byte {
	return []byte{heartbeatPing, 0, 0, 0, 0}
}
