package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/ebitengine/oto/v3"
	"github.com/gorilla/websocket"
	"github.com/mdp/qrterminal/v3"
)

//go:embed web/*.html web/index.html
var webFiles embed.FS

// State managed centrally and broadcasted to all WebSocket clients
type MetronomeState struct {
	BPM         int  `json:"bpm"`
	Numerator   int  `json:"numerator"`   // e.g., 4 in 4/4
	Denominator int  `json:"denominator"` // e.g., 4 in 4/4
	IsPlaying   bool `json:"isPlaying"`
	CurrentBeat int  `json:"currentBeat"` // For visual sync in Web UI
}

type Hub struct {
	clients   map[*websocket.Conn]bool
	broadcast chan []byte
	mu        sync.Mutex
}

var (
	state    = MetronomeState{BPM: 120, Numerator: 4, Denominator: 4, IsPlaying: false, CurrentBeat: 0}
	stateMu  sync.RWMutex
	hub      = Hub{clients: make(map[*websocket.Conn]bool), broadcast: make(chan []byte)}
	upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
)

// Helper function to find the host computer's local Wi-Fi IP address
func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "localhost"
	}
	for _, address := range addrs {
		if ipnet, ok := address.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return "localhost"
}

// Generate synthesized sine-wave clicks in memory (16-bit PCM, 44100Hz)
func generateClickPCM(freq float64, duration time.Duration) []byte {
	sampleRate := 44100
	numSamples := int(float64(sampleRate) * duration.Seconds())
	buf := new(bytes.Buffer)

	for i := 0; i < numSamples; i++ {
		t := float64(i) / float64(sampleRate)
		// Exponential decay envelope for crisp click
		envelope := math.Exp(-t * 60)
		sample := int16(32767 * math.Sin(2*math.Pi*freq*t) * envelope)

		buf.WriteByte(byte(sample))
		buf.WriteByte(byte(sample >> 8))
	}
	return buf.Bytes()
}

func main() {
	// Initialize Host Audio Output
	op := &oto.NewContextOptions{
		SampleRate:   44100,
		ChannelCount: 2,
		Format:       oto.FormatSignedInt16LE,
	}
	otoCtx, ready, err := oto.NewContext(op)
	if err != nil {
		log.Fatalf("Failed to initialize host audio: %v", err)
	}
	<-ready

	// Pre-generate audio samples for host playback
	accentSound := generateClickPCM(1200.0, 50*time.Millisecond) // High pitch (Beat 1)
	normalSound := generateClickPCM(800.0, 50*time.Millisecond)  // Lower pitch

	// Start Background Audio Worker Thread
	go hostAudioEngine(otoCtx, accentSound, normalSound)
	go hub.run()

	// HTTP & WebSocket Server Setup
	subFS, _ := fs.Sub(webFiles, "web")
	ip := getLocalIP()
	url := fmt.Sprintf("http://%s:8080", ip)

	fmt.Println("\n==================================================")
	fmt.Printf(" Remote Metronome Running!\n")
	fmt.Printf(" Local URL: %s\n", url)
	fmt.Println(" Scan the QR code below with your phone camera:")
	fmt.Println("==================================================")

	// Print QR code directly to the terminal screen
	qrterminal.GenerateWithConfig(url, qrterminal.Config{
		Level:      qrterminal.M,
		Writer:     os.Stdout,
		HalfBlocks: true,
	})

	http.Handle("/", http.FileServer(http.FS(subFS)))
	http.HandleFunc("/ws", handleWebSockets)

	log.Fatal(http.ListenAndServe(":8080", nil))
}

func hostAudioEngine(ctx *oto.Context, accent, normal []byte) {
	beat := 0
	for {
		stateMu.RLock()
		playing := state.IsPlaying
		bpm := state.BPM
		num := state.Numerator
		stateMu.RUnlock()

		if !playing {
			beat = 0
			time.Sleep(20 * time.Millisecond)
			continue
		}

		// Play sound on Host Speakers
		var sound []byte
		if beat == 0 {
			sound = accent
		} else {
			sound = normal
		}
		player := ctx.NewPlayer(bytes.NewReader(sound))
		player.Play()

		// Update beat counter and notify clients for UI highlight
		stateMu.Lock()
		state.CurrentBeat = beat + 1
		broadcastState()
		stateMu.Unlock()

		// Calculate interval dynamically to support instant BPM edits
		interval := time.Duration(float64(time.Minute) / float64(bpm))
		time.Sleep(interval)

		beat = (beat + 1) % num
	}
}

func handleWebSockets(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	hub.mu.Lock()
	hub.clients[conn] = true
	hub.mu.Unlock()

	// Send initial state on connect
	stateMu.RLock()
	data, _ := json.Marshal(state)
	stateMu.RUnlock()
	conn.WriteMessage(websocket.TextMessage, data)

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			hub.mu.Lock()
			delete(hub.clients, conn)
			hub.mu.Unlock()
			break
		}

		// Receive state updates from any web controller
		var newState MetronomeState
		if err := json.Unmarshal(msg, &newState); err == nil {
			stateMu.Lock()
			state.BPM = newState.BPM
			state.Numerator = newState.Numerator
			state.Denominator = newState.Denominator
			state.IsPlaying = newState.IsPlaying
			broadcastState()
			stateMu.Unlock()
		}
	}
}

func broadcastState() {
	data, _ := json.Marshal(state)
	hub.broadcast <- data
}

func (h *Hub) run() {
	for data := range h.broadcast {
		h.mu.Lock()
		for client := range h.clients {
			client.WriteMessage(websocket.TextMessage, data)
		}
		h.mu.Unlock()
	}
}
