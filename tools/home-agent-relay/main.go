package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type peer struct {
	id        string
	conn      *websocket.Conn
	send      chan outbound
	role      string
	deviceID  string
	remote    string
	userAgent string
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
	agents map[string]*peer
}

type peerState struct {
	Connected bool   `json:"connected"`
	ID        string `json:"id,omitempty"`
	Since     string `json:"since,omitempty"`
	LastSeen  string `json:"lastSeen,omitempty"`
	Remote    string `json:"remote,omitempty"`
	UserAgent string `json:"userAgent,omitempty"`
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
		id:        peerID(role, r),
		conn:      conn,
		send:      make(chan outbound, 64),
		role:      role,
		deviceID:  deviceID,
		remote:    r.RemoteAddr,
		userAgent: r.UserAgent(),
		connected: time.Now(),
		lastSeen:  time.Now(),
	}
	register(deviceID, role, p)
	log.Printf("%s connected: device=%s peer=%s remote=%s", role, deviceID, p.id, p.remote)

	go writeLoop(p)
	readLoop(deviceID, role, p)
}

func peerID(role string, r *http.Request) string {
	if id := r.URL.Query().Get("clientId"); id != "" {
		return id
	}
	return fmt.Sprintf("%s-%d", role, time.Now().UnixNano())
}

func register(deviceID, role string, p *peer) {
	roomsMu.Lock()
	defer roomsMu.Unlock()

	r := rooms[deviceID]
	if r == nil {
		r = &room{agents: map[string]*peer{}}
		rooms[deviceID] = r
	}
	if r.agents == nil {
		r.agents = map[string]*peer{}
	}

	if role == "device" {
		old := r.device
		r.device = p
		if old != nil {
			log.Printf("replace device: device=%s oldPeer=%s newPeer=%s", deviceID, old.id, p.id)
			_ = old.conn.Close()
		}
		return
	}

	if old := r.agents[p.id]; old != nil {
		log.Printf("replace agent: device=%s peer=%s", deviceID, p.id)
		_ = old.conn.Close()
	}
	r.agents[p.id] = p
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
	if role == "agent" && r.agents[p.id] == p {
		delete(r.agents, p.id)
	}
	if r.device == nil && len(r.agents) == 0 {
		delete(rooms, deviceID)
	}
}

func targetPeers(deviceID, role string) []*peer {
	roomsMu.Lock()
	defer roomsMu.Unlock()

	r := rooms[deviceID]
	if r == nil {
		return nil
	}
	if role == "agent" {
		if r.device == nil {
			return nil
		}
		return []*peer{r.device}
	}

	peers := make([]*peer, 0, len(r.agents))
	for _, agent := range r.agents {
		peers = append(peers, agent)
	}
	return peers
}

func readLoop(deviceID, role string, p *peer) {
	defer func() {
		unregister(deviceID, role, p)
		_ = p.conn.Close()
		log.Printf("%s disconnected: device=%s peer=%s", role, deviceID, p.id)
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
			log.Printf("read ended: device=%s peer=%s role=%s err=%v", deviceID, p.id, role, err)
			return
		}
		p.lastSeen = time.Now()
		for _, dst := range targetPeers(deviceID, role) {
			select {
			case dst.send <- outbound{messageType: messageType, data: append([]byte(nil), data...)}:
			default:
				log.Printf("drop message for slow peer: device=%s peer=%s", deviceID, dst.id)
			}
		}
	}
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	if want := os.Getenv("STACKCHAN_RELAY_TOKEN"); want != "" && r.Header.Get("Authorization") != want {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	type roomState struct {
		Device peerState   `json:"device"`
		Agent  peerState   `json:"agent"`
		Agents []peerState `json:"agents,omitempty"`
	}

	roomsMu.Lock()
	out := map[string]roomState{}
	for deviceID, room := range rooms {
		state := roomState{}
		if room.device != nil {
			state.Device = stateForPeer(room.device)
		}
		for _, agent := range room.agents {
			agentState := stateForPeer(agent)
			state.Agents = append(state.Agents, agentState)
			if !state.Agent.Connected {
				state.Agent = agentState
			}
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

func stateForPeer(p *peer) peerState {
	return peerState{
		Connected: true,
		ID:        p.id,
		Since:     p.connected.Format(time.RFC3339),
		LastSeen:  p.lastSeen.Format(time.RFC3339),
		Remote:    p.remote,
		UserAgent: p.userAgent,
	}
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
				log.Printf("write ended: device=%s peer=%s role=%s err=%v", p.deviceID, p.id, p.role, err)
				return
			}
		case <-protocolTicker.C:
			_ = p.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := p.conn.WriteMessage(websocket.BinaryMessage, heartbeatPacket()); err != nil {
				log.Printf("protocol heartbeat write ended: device=%s peer=%s role=%s err=%v", p.deviceID, p.id, p.role, err)
				return
			}
		case <-wsTicker.C:
			_ = p.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := p.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				log.Printf("ws ping write ended: device=%s peer=%s role=%s err=%v", p.deviceID, p.id, p.role, err)
				return
			}
		}
	}
}

func heartbeatPacket() []byte {
	return []byte{heartbeatPing, 0, 0, 0, 0}
}
