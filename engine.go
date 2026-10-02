package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	_ "modernc.org/sqlite"
)

// DeviceSession merepresentasikan status 1 nomor WhatsApp yang terdaftar
type DeviceSession struct {
	SessionID     string    `json:"session_id"`
	UserID        int       `json:"user_id"`
	Phone         string    `json:"phone"`
	Name          string    `json:"name"`
	PushName      string    `json:"push_name"`
	IsConnected   bool      `json:"is_connected"`
	Status        string    `json:"status"` // disconnected, pairing, connected
	QRCode        string    `json:"qrcode,omitempty"`
	WebhookURL    string    `json:"webhook_url"`
	WebhookSecret string    `json:"webhook_secret"`
	LastSeen      time.Time `json:"last_seen"`
	Client        *whatsmeow.Client
	QRChan        <-chan whatsmeow.QRChannelItem
	CancelFunc    context.CancelFunc
}

// EngineManager mengelola multi-tenant device sessions di SQLite
type EngineManager struct {
	mu            sync.RWMutex
	container     *sqlstore.Container
	sessions      map[string]*DeviceSession
	db            *sql.DB
	logger        waLog.Logger
	defaultWH     string
	secret        string
	httpClient    *http.Client
}

func NewEngineManager(dbPath, defaultWH, secret string) (*EngineManager, error) {
	logger := waLog.Stdout("Huwa", "INFO", true)

	// SQLite connection string dengan WAL mode dan busy timeout 5s
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)", dbPath)
	container, err := sqlstore.New("sqlite", dsn, logger)
	if err != nil {
		return nil, fmt.Errorf("gagal inisialisasi sqlstore whatsmeow: %w", err)
	}

	rawDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("gagal buka database sqlite: %w", err)
	}

	// Inisialisasi tabel metadata sesi jika belum ada
	initQuery := `
	CREATE TABLE IF NOT EXISTS huwa_metadata (
		session_id TEXT PRIMARY KEY,
		user_id INTEGER DEFAULT 0,
		webhook_url TEXT DEFAULT '',
		webhook_secret TEXT DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`
	if _, err := rawDB.Exec(initQuery); err != nil {
		return nil, fmt.Errorf("gagal inisialisasi tabel metadata: %w", err)
	}

	mgr := &EngineManager{
		container:  container,
		sessions:   make(map[string]*DeviceSession),
		db:         rawDB,
		logger:     logger,
		defaultWH:  defaultWH,
		secret:     secret,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}

	// Muat seluruh sesi yang tersimpan sebelumnya di SQLite
	if err := mgr.restoreAllSessions(); err != nil {
		log.Printf("[Huwa] Warning saat restore sessions: %v", err)
	}

	return mgr, nil
}

// restoreAllSessions menyambungkan kembali semua nomor WA yang pernah dipairing saat service start
func (m *EngineManager) restoreAllSessions() error {
	devices, err := m.container.GetAllDevices()
	if err != nil {
		return err
	}

	for _, dev := range devices {
		sessionID := dev.ID.String()
		if dev.PushName != "" {
			sessionID = dev.PushName
		}

		// Cari metadata custom jika ada
		var customSessionID, whURL, whSec string
		var uid int
		row := m.db.QueryRow("SELECT session_id, user_id, webhook_url, webhook_secret FROM huwa_metadata WHERE session_id = ? OR session_id = ?", sessionID, dev.ID.User)
		_ = row.Scan(&customSessionID, &uid, &whURL, &whSec)
		if customSessionID != "" {
			sessionID = customSessionID
		}

		client := whatsmeow.NewClient(dev, m.logger)
		ds := &DeviceSession{
			SessionID:     sessionID,
			UserID:        uid,
			Phone:         dev.ID.User,
			Name:          dev.PushName,
			Status:        "disconnected",
			WebhookURL:    whURL,
			WebhookSecret: whSec,
			LastSeen:      time.Now(),
			Client:        client,
		}

		m.setupEventHandler(ds)
		m.sessions[sessionID] = ds

		// Connect di background dengan goroutine
		go func(c *whatsmeow.Client, s *DeviceSession) {
			if err := c.Connect(); err != nil {
				log.Printf("[Huwa] Gagal auto-reconnect sesi %s: %v", s.SessionID, err)
			} else {
				s.IsConnected = true
				s.Status = "connected"
				log.Printf("[Huwa] Auto-reconnect berhasil: %s (+%s)", s.SessionID, s.Phone)
			}
		}(client, ds)
	}

	return nil
}

// GetOrCreateSession mengambil sesi atau membuat sesi baru untuk dipairing
func (m *EngineManager) GetOrCreateSession(sessionID string, userID int) (*DeviceSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if ds, exists := m.sessions[sessionID]; exists {
		if ds.Client != nil && ds.Client.IsConnected() {
			ds.IsConnected = true
			ds.Status = "connected"
			if ds.Client.Store.ID != nil {
				ds.Phone = ds.Client.Store.ID.User
				ds.PushName = ds.Client.Store.PushName
			}
			return ds, nil
		}
		// Jika sesi ada tapi belum tersambung, coba sambungkan
		if ds.Client != nil && ds.Client.Store.ID != nil {
			go func() {
				_ = ds.Client.Connect()
			}()
			return ds, nil
		}
	}

	// Buat store device baru di SQLite
	deviceStore := m.container.NewDevice()
	client := whatsmeow.NewClient(deviceStore, m.logger)

	ds := &DeviceSession{
		SessionID: sessionID,
		UserID:    userID,
		Status:    "pairing",
		LastSeen:  time.Now(),
		Client:    client,
	}

	m.setupEventHandler(ds)
	m.sessions[sessionID] = ds

	// Simpan metadata ke SQLite
	_, _ = m.db.Exec(`
		INSERT INTO huwa_metadata (session_id, user_id, updated_at) 
		VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(session_id) DO UPDATE SET updated_at = CURRENT_TIMESTAMP
	`, sessionID, userID)

	return ds, nil
}

// GenerateQR menghasilkan QR code base64 untuk pairing WhatsApp
func (m *EngineManager) GenerateQR(ds *DeviceSession) (string, error) {
	if ds.Client == nil {
		return "", fmt.Errorf("client tidak tersedia")
	}

	if ds.Client.IsConnected() {
		ds.IsConnected = true
		ds.Status = "connected"
		return "", nil
	}

	qrChan, err := ds.Client.GetQRChannel(context.Background())
	if err != nil {
		if ds.Client.Store.ID != nil {
			// Sesi sudah punya kredensial, tinggal reconnect
			_ = ds.Client.Connect()
			ds.IsConnected = true
			ds.Status = "connected"
			return "", nil
		}
		return "", fmt.Errorf("gagal mendapatkan QR channel: %w", err)
	}

	if err := ds.Client.Connect(); err != nil {
		return "", fmt.Errorf("gagal menghubungkan socket client: %w", err)
	}

	// Tunggu event QR code pertama dari channel
	select {
	case evt, ok := <-qrChan:
		if !ok {
			return "", fmt.Errorf("qr channel ditutup")
		}
		if evt.Event == "code" {
			png, err := qrcode.Encode(evt.Code, qrcode.Medium, 256)
			if err != nil {
				return "", fmt.Errorf("gagal encode qr code: %w", err)
			}
			base64Img := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
			ds.QRCode = base64Img
			ds.Status = "pairing"
			return base64Img, nil
		} else if evt.Event == "success" {
			ds.IsConnected = true
			ds.Status = "connected"
			return "", nil
		}
	case <-time.After(10 * time.Second):
		return "", fmt.Errorf("timeout menunggu QR Code dari server WhatsApp")
	}

	return "", nil
}

// setupEventHandler memasang listener realtime WhatsApp (Pesan masuk, typing, status)
func (m *EngineManager) setupEventHandler(ds *DeviceSession) {
	ds.Client.AddEventHandler(func(rawEvt interface{}) {
		switch evt := rawEvt.(type) {
		case *events.Connected:
			ds.IsConnected = true
			ds.Status = "connected"
			if ds.Client.Store.ID != nil {
				ds.Phone = ds.Client.Store.ID.User
				ds.PushName = ds.Client.Store.PushName
			}
			log.Printf("[Huwa] Device Terhubung: %s (+%s)", ds.SessionID, ds.Phone)

		case *events.Disconnected:
			ds.IsConnected = false
			ds.Status = "disconnected"
			log.Printf("[Huwa] Device Terputus: %s", ds.SessionID)

		case *events.LoggedOut:
			ds.IsConnected = false
			ds.Status = "logged_out"
			log.Printf("[Huwa] Device Dikeluarkan oleh Pengguna (Logged Out): %s", ds.SessionID)

		case *events.Message:
			m.handleIncomingMessage(ds, evt)
		}
	})
}

// handleIncomingMessage memproses chat masuk dan meneruskannya ke Webhook lokal solusi-wp
func (m *EngineManager) handleIncomingMessage(ds *DeviceSession, msg *events.Message) {
	// Jangan teruskan pesan jika dikirim oleh bot sendiri
	if msg.Info.IsFromMe {
		return
	}

	sender := msg.Info.Sender.User
	chatJID := msg.Info.Chat.String()
	isGroup := msg.Info.IsGroup

	// Ekstraksi isi teks pesan
	var textMessage string
	if msg.Message.GetConversation() != "" {
		textMessage = msg.Message.GetConversation()
	} else if msg.Message.GetExtendedTextMessage() != nil {
		textMessage = msg.Message.GetExtendedTextMessage().GetText()
	} else if msg.Message.GetImageMessage() != nil {
		textMessage = msg.Message.GetImageMessage().GetCaption()
	} else if msg.Message.GetDocumentMessage() != nil {
		textMessage = msg.Message.GetDocumentMessage().GetCaption()
	}

	if textMessage == "" {
		return
	}

	// Bangun payload kompatibel Zawa v2 / Solusi-WP
	payload := map[string]interface{}{
		"type": "message",
		"data": map[string]interface{}{
			"Info": map[string]interface{}{
				"Sender":    sender,
				"Chat":      chatJID,
				"IsFromMe":  false,
				"IsGroup":   isGroup,
				"PushName":  msg.Info.PushName,
				"Timestamp": msg.Info.Timestamp.Format(time.RFC3339),
			},
			"Message": map[string]interface{}{
				"conversation": textMessage,
			},
			"fromMe":  false,
			"isGroup": isGroup,
			"sender":  sender,
			"message": textMessage,
		},
	}

	targetWH := ds.WebhookURL
	if targetWH == "" {
		targetWH = m.defaultWH
	}

	if targetWH == "" {
		return
	}

	// Kirim asynchronous ke Webhook internal WordPress (Latensi < 1ms)
	go func() {
		jsonBytes, _ := json.Marshal(payload)
		req, err := http.NewRequest("POST", targetWH, bytes.NewBuffer(jsonBytes))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if ds.WebhookSecret != "" {
			req.Header.Set("X-Token", ds.WebhookSecret)
		}
		resp, err := m.httpClient.Do(req)
		if err == nil && resp != nil {
			_ = resp.Body.Close()
		}
	}()
}

// SendTextMessage mengirim pesan teks ke kontak atau grup
func (m *EngineManager) SendTextMessage(sessionID, phone, groupJID, text string) (string, error) {
	m.mu.RLock()
	ds, exists := m.sessions[sessionID]
	m.mu.RUnlock()

	if !exists || ds.Client == nil || !ds.Client.IsConnected() {
		return "", fmt.Errorf("sesi %s belum terhubung", sessionID)
	}

	var recipient types.JID
	if groupJID != "" {
		recipient, _ = types.ParseJID(groupJID)
	} else {
		// Bersihkan format nomor HP
		cleanPhone := strings.TrimPrefix(phone, "+")
		cleanPhone = strings.TrimPrefix(cleanPhone, "0")
		if !strings.HasPrefix(cleanPhone, "62") {
			cleanPhone = "62" + cleanPhone
		}
		recipient = types.NewJID(cleanPhone, types.DefaultUserServer)
	}

	msg := &waProto.Message{
		Conversation: &text,
	}

	res, err := ds.Client.SendMessage(context.Background(), recipient, msg)
	if err != nil {
		return "", fmt.Errorf("gagal kirim pesan: %w", err)
	}

	return res.ID, nil
}

// SendTypingPresence mengirim sinyal "sedang mengetik..."
func (m *EngineManager) SendTypingPresence(sessionID, phone, groupJID string) error {
	m.mu.RLock()
	ds, exists := m.sessions[sessionID]
	m.mu.RUnlock()

	if !exists || ds.Client == nil || !ds.Client.IsConnected() {
		return fmt.Errorf("sesi tidak aktif")
	}

	var recipient types.JID
	if groupJID != "" {
		recipient, _ = types.ParseJID(groupJID)
	} else {
		cleanPhone := strings.TrimPrefix(phone, "+")
		recipient = types.NewJID(cleanPhone, types.DefaultUserServer)
	}

	return ds.Client.SendChatPresence(recipient, types.ChatPresenceComposing, types.ChatPresenceMediaText)
}
