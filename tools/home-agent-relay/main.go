package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type peer struct {
	conn *websocket.Conn
	send chan outbound
}

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

	p := &peer{conn: conn, send: make(chan outbound, 64)}
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
		if dst := targetPeer(deviceID, role); dst != nil {
			select {
			case dst.send <- outbound{messageType: messageType, data: append([]byte(nil), data...)}:
			default:
				log.Printf("drop message for slow peer: %s", deviceID)
			}
		}
	}
}

func writeLoop(p *peer) {
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

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
		case <-ticker.C:
			_ = p.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := p.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
