package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
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
	sqliteDriver "modernc.org/sqlite"
)

func init() {
	// [HARDCODED AI PROTECTION - JANGAN DIUBAH]: Konfigurasi identitas OS/Platform WhatsApp agar di HP tertulis "AffiliaThor"
	store.SetOSInfo("AffiliaThor", [3]uint32{1, 0, 0})

	found := false
	for _, d := range sql.Drivers() {
		if d == "sqlite3" {
			found = true
			break
		}
	}
	if !found {
		sql.Register("sqlite3", &sqliteDriver.Driver{})
	}
}

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
	PairingCode   string    `json:"pairing_code,omitempty"`
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

	// SQLite connection string dengan Foreign Keys ON, WAL mode dan busy timeout 5s
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_foreign_keys=on&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)", dbPath)
	container, err := sqlstore.New(context.Background(), "sqlite3", dsn, logger)
	if err != nil {
		return nil, fmt.Errorf("gagal inisialisasi sqlstore whatsmeow: %w", err)
	}

	rawDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("gagal buka database sqlite: %w", err)
	}

	// Inisialisasi tabel metadata sesi jika belum ada
	initQuery := `
	CREATE TABLE IF NOT EXISTS huwa_metadata (
		session_id TEXT PRIMARY KEY,
		device_jid TEXT DEFAULT '',
		phone TEXT DEFAULT '',
		user_id INTEGER DEFAULT 0,
		webhook_url TEXT DEFAULT '',
		webhook_secret TEXT DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`
	if _, err := rawDB.Exec(initQuery); err != nil {
		return nil, fmt.Errorf("gagal inisialisasi tabel metadata: %w", err)
	}
	_, _ = rawDB.Exec(`ALTER TABLE huwa_metadata ADD COLUMN device_jid TEXT DEFAULT ''`)
	_, _ = rawDB.Exec(`ALTER TABLE huwa_metadata ADD COLUMN phone TEXT DEFAULT ''`)

	mgr := &EngineManager{
		container:  container,
		sessions:   make(map[string]*DeviceSession),
		db:         rawDB,
		logger:     logger,
		defaultWH:  defaultWH,
		secret:     secret,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}

	// Muat seluruh sesi yang tersimpan sebelumnya di SQLite
	if err := mgr.restoreAllSessions(); err != nil {
		log.Printf("[Huwa] Warning saat restore sessions: %v", err)
	}

	// [ALWAYS ACTIVE PRESENCE & AUTO-RECONNECT WATCHDOG]
	// Menjaga koneksi tetap online 24/7 dan status di WhatsApp HP selalu "Aktif" (seperti Fonnte)
	go mgr.startWatchdogLoop(30 * time.Second)

	return mgr, nil
}

// restoreAllSessions menyambungkan kembali semua nomor WA yang pernah dipairing saat service start
func (m *EngineManager) restoreAllSessions() error {
	devices, err := m.container.GetAllDevices(context.Background())
	if err != nil {
		return err
	}

	for _, dev := range devices {
		sessionID := dev.ID.String()
		if dev.PushName != "" {
			sessionID = dev.PushName
		}

		// Cari metadata custom berdasarkan JID lengkap, nomor telepon, atau session_id
		var customSessionID, whURL, whSec string
		var uid int
		row := m.db.QueryRow("SELECT session_id, user_id, webhook_url, webhook_secret FROM huwa_metadata WHERE device_jid = ? OR phone = ? OR session_id = ? OR session_id = ? ORDER BY updated_at DESC LIMIT 1", dev.ID.String(), dev.ID.User, sessionID, dev.ID.User)
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
			} else if c.Store.ID != nil && c.IsLoggedIn() {
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
		if ds.Client != nil && ds.Client.IsConnected() && ds.Client.IsLoggedIn() && ds.Client.Store.ID != nil {
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
		// Sesi dalam proses pairing yang masih aktif: gunakan sesi yang ada agar tidak membuat device ganda
		if ds.Client != nil && ds.Status == "pairing" {
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

	if ds.Client.IsConnected() && ds.Client.IsLoggedIn() && ds.Client.Store.ID != nil {
		ds.IsConnected = true
		ds.Status = "connected"
		return "", nil
	}

	if ds.QRCode != "" && ds.Status == "pairing" {
		return ds.QRCode, nil
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

	// Tunggu event QR code pertama dari channel dan jalankan background listener untuk rotasi QR
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

			// Terus baca channel QR di background untuk memperbarui rotasi QR dan menangkap event success
			go func(ch <-chan whatsmeow.QRChannelItem, s *DeviceSession) {
				for item := range ch {
					if item.Event == "code" {
						if p, e := qrcode.Encode(item.Code, qrcode.Medium, 256); e == nil {
							s.QRCode = "data:image/png;base64," + base64.StdEncoding.EncodeToString(p)
							s.Status = "pairing"
						}
					} else if item.Event == "success" {
						s.IsConnected = true
						s.Status = "connected"
						s.QRCode = ""
						return
					}
				}
			}(qrChan, ds)

			return base64Img, nil
		} else if evt.Event == "success" {
			ds.IsConnected = true
			ds.Status = "connected"
			ds.QRCode = ""
			return "", nil
		}
	case <-time.After(15 * time.Second):
		return "", fmt.Errorf("timeout menunggu QR Code dari server WhatsApp")
	}

	return "", nil
}

// PairPhone menghasilkan 8-digit Kode Pairing WhatsApp tanpa perlu scan QR
func (m *EngineManager) PairPhone(ds *DeviceSession, rawPhone string) (string, error) {
	if ds == nil {
		return "", fmt.Errorf("sesi tidak valid")
	}

	// Normalisasi nomor telepon: hanya digit, buang +, spasi, strip
	cleanPhone := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, rawPhone)

	// Ubah awalan 08xxx menjadi 628xxx (format internasional standar WhatsApp)
	if strings.HasPrefix(cleanPhone, "0") {
		cleanPhone = "62" + cleanPhone[1:]
	}

	if len(cleanPhone) < 9 {
		return "", fmt.Errorf("nomor telepon WhatsApp tidak valid (minimal 9 digit)")
	}

	m.mu.Lock()
	// Jika client lama sudah terhubung ke nomor lain atau ingin pair ulang, reset store bersih
	if ds.Client != nil {
		if ds.Client.IsConnected() {
			ds.Client.Disconnect()
		}
		if ds.Client.Store != nil {
			// [HARDCODED AI PROTECTION - JANGAN DIUBAH]: Whatsmeow versi modern wajib menerima argumen context.Context pada Store.Delete()
			_ = ds.Client.Store.Delete(context.Background())
		}
	}
	newStore := m.container.NewDevice()
	newClient := whatsmeow.NewClient(newStore, m.logger)
	ds.Client = newClient
	ds.IsConnected = false
	ds.Status = "pairing"
	ds.Phone = cleanPhone
	ds.QRCode = ""
	ds.PairingCode = ""
	m.setupEventHandler(ds)
	m.mu.Unlock()

	// Hubungkan socket ke WhatsApp
	if err := ds.Client.Connect(); err != nil {
		return "", fmt.Errorf("gagal menghubungkan socket ke server WhatsApp: %w", err)
	}

	// Tunggu sebentar (1 detik) agar socket handshake stabil
	time.Sleep(1 * time.Second)

	// Minta Kode Pairing 8 digit ke server WhatsApp
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	// [HARDCODED AI PROTECTION - JANGAN DIUBAH]: Gunakan identitas branding AffiliaThor untuk handshake pairing WhatsApp
	code, err := ds.Client.PairPhone(ctx, cleanPhone, true, whatsmeow.PairClientChrome, "AffiliaThor")
	if err != nil {
		return "", fmt.Errorf("gagal mendapatkan kode pairing dari WhatsApp: %w", err)
	}

	ds.PairingCode = code
	ds.Status = "pairing"

	log.Printf("[Huwa] Kode Pairing berhasil dibuat untuk sesi %s (+%s): %s", ds.SessionID, cleanPhone, code)
	return code, nil
}

// setupEventHandler memasang listener realtime WhatsApp (Pesan masuk, typing, status)
func (m *EngineManager) setupEventHandler(ds *DeviceSession) {
	ds.Client.AddEventHandler(func(rawEvt interface{}) {
		switch evt := rawEvt.(type) {
		case *events.Connected:
			if ds.Client.Store.ID != nil && ds.Client.IsLoggedIn() {
				ds.IsConnected = true
				ds.Status = "connected"
				ds.QRCode = ""
				ds.PairingCode = ""
				ds.Phone = ds.Client.Store.ID.User
				ds.PushName = ds.Client.Store.PushName
				_, _ = m.db.Exec(`
					UPDATE huwa_metadata SET device_jid = ?, phone = ?, updated_at = CURRENT_TIMESTAMP 
					WHERE session_id = ?
				`, ds.Client.Store.ID.String(), ds.Phone, ds.SessionID)
				// [HARDCODED AI PROTECTION - JANGAN DIUBAH]: Modern Whatsmeow WAJIB menyertakan context.Context sebagai argumen pertama SendPresence
				_ = ds.Client.SendPresence(context.Background(), types.PresenceAvailable)
				log.Printf("[Huwa] Device Terhubung & Terotentikasi: %s (+%s)", ds.SessionID, ds.Phone)
			} else {
				// Socket WhatsApp terhubung ke server Meta untuk negosiasi pairing, belum login ke nomor WA
				ds.IsConnected = false
				ds.Status = "pairing"
				log.Printf("[Huwa] Socket WhatsApp terhubung ke Meta (status pairing aktif): %s", ds.SessionID)
			}

		case *events.PairSuccess:
			// [HARDCODED AI PROTECTION - JANGAN DIUBAH]: Pengguna berhasil memasukkan kode pairing 8-digit di WhatsApp HP
			ds.IsConnected = true
			ds.Status = "connected"
			ds.QRCode = ""
			ds.PairingCode = ""
			if ds.Client.Store.ID != nil {
				ds.Phone = ds.Client.Store.ID.User
				ds.PushName = ds.Client.Store.PushName
				_, _ = m.db.Exec(`
					UPDATE huwa_metadata SET device_jid = ?, phone = ?, updated_at = CURRENT_TIMESTAMP 
					WHERE session_id = ?
				`, ds.Client.Store.ID.String(), ds.Phone, ds.SessionID)
			}
			// [HARDCODED AI PROTECTION - JANGAN DIUBAH]: Modern Whatsmeow WAJIB menyertakan context.Context sebagai argumen pertama SendPresence
			_ = ds.Client.SendPresence(context.Background(), types.PresenceAvailable)
			log.Printf("[Huwa] Pairing Berhasil Diverifikasi! Device Terhubung: %s (+%s)", ds.SessionID, ds.Phone)

		case *events.Disconnected:
			ds.IsConnected = false
			ds.Status = "disconnected"
			log.Printf("[Huwa] Device Terputus: %s", ds.SessionID)
			// Auto-reconnect background segera jika bukan di-logout oleh pengguna
			if ds.Client != nil && ds.Client.Store != nil && ds.Client.Store.ID != nil {
				go func(c *whatsmeow.Client, s *DeviceSession) {
					time.Sleep(3 * time.Second)
					if !c.IsConnected() && s.Status != "logged_out" {
						log.Printf("[Huwa] Mencoba menyambungkan kembali sesi %s...", s.SessionID)
						_ = c.Connect()
					}
				}(ds.Client, ds)
			}

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

	// Bangun payload kompatibel HUWA / Solusi-WP
	payload := map[string]interface{}{
		"type":       "message",
		"session_id": ds.SessionID,
		"user_id":    ds.UserID,
		"data": map[string]interface{}{
			"session_id": ds.SessionID,
			"user_id":    ds.UserID,
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
		req.Header.Set("X-Session-ID", ds.SessionID)
		if ds.UserID > 0 {
			req.Header.Set("X-User-ID", fmt.Sprintf("%d", ds.UserID))
		}
		if ds.WebhookSecret != "" {
			req.Header.Set("X-Token", ds.WebhookSecret)
		}
		resp, err := m.httpClient.Do(req)
		if err == nil && resp != nil {
			_ = resp.Body.Close()
		}
	}()
}

// uploadMedia mengunggah file media gambar ke server WhatsApp Whatsmeow
func (m *EngineManager) uploadMedia(client *whatsmeow.Client, mediaURL string) (*whatsmeow.UploadResponse, error) {
	resp, err := m.httpClient.Get(mediaURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil || len(data) == 0 {
		return nil, fmt.Errorf("media kosong")
	}

	res, err := client.Upload(context.Background(), data, whatsmeow.MediaImage)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// GetJoinedGroups mengambil daftar grup WhatsApp yang diikuti sesi
func (m *EngineManager) GetJoinedGroups(sessionID string) ([]map[string]interface{}, error) {
	m.mu.RLock()
	ds, exists := m.sessions[sessionID]
	m.mu.RUnlock()

	if !exists || ds.Client == nil || !ds.Client.IsConnected() {
		return nil, fmt.Errorf("sesi %s belum terhubung", sessionID)
	}

	groups, err := ds.Client.GetJoinedGroups(context.Background())
	if err != nil {
		return nil, err
	}

	res := make([]map[string]interface{}, 0, len(groups))
	for _, g := range groups {
		res = append(res, map[string]interface{}{
			"id":          g.JID.String(),
			"name":        g.Name,
			"subject":     g.Name,
			"topic":       g.Topic,
			"description": g.Topic,
		})
	}
	return res, nil
}

// SendTextMessage mengirim pesan teks atau gambar ke kontak atau grup
func (m *EngineManager) SendTextMessage(sessionID, phone, groupJID, text, mediaURL string) (string, error) {
	m.mu.RLock()
	ds, exists := m.sessions[sessionID]
	m.mu.RUnlock()

	if !exists || ds.Client == nil || !ds.Client.IsConnected() {
		return "", fmt.Errorf("sesi %s belum terhubung", sessionID)
	}

	var recipient types.JID
	if groupJID != "" {
		recipient, _ = types.ParseJID(groupJID)
	} else if strings.Contains(phone, "@") {
		recipient, _ = types.ParseJID(phone)
	} else {
		// Bersihkan format nomor HP
		cleanPhone := strings.TrimPrefix(phone, "+")
		cleanPhone = strings.TrimPrefix(cleanPhone, "0")
		if !strings.HasPrefix(cleanPhone, "62") {
			cleanPhone = "62" + cleanPhone
		}
		recipient = types.NewJID(cleanPhone, types.DefaultUserServer)
	}

	var msg *waProto.Message
	if mediaURL != "" {
		if uploaded, err := m.uploadMedia(ds.Client, mediaURL); err == nil && uploaded != nil {
			mime := "image/jpeg"
			msg = &waProto.Message{
				ImageMessage: &waProto.ImageMessage{
					URL:           &uploaded.URL,
					DirectPath:    &uploaded.DirectPath,
					MediaKey:      uploaded.MediaKey,
					Mimetype:      &mime,
					FileEncSHA256: uploaded.FileEncSHA256,
					FileSHA256:    uploaded.FileSHA256,
					FileLength:    &uploaded.FileLength,
					Caption:       &text,
				},
			}
		}
	}
	if msg == nil {
		msg = &waProto.Message{
			Conversation: &text,
		}
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

	return ds.Client.SendChatPresence(context.Background(), recipient, types.ChatPresenceComposing, types.ChatPresenceMediaText)
}

// startWatchdogLoop menjaga koneksi tetap hidup 24/7 dan mengirim presence online seperti Fonnte
func (m *EngineManager) startWatchdogLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		m.mu.RLock()
		sessionsToInspect := make([]*DeviceSession, 0, len(m.sessions))
		for _, s := range m.sessions {
			sessionsToInspect = append(sessionsToInspect, s)
		}
		m.mu.RUnlock()

		for _, ds := range sessionsToInspect {
			if ds.Client == nil || ds.Client.Store == nil || ds.Client.Store.ID == nil {
				continue
			}
			if ds.Status == "logged_out" {
				continue
			}

			// Jika socket terputus, sambungkan kembali otomatis
			if !ds.Client.IsConnected() {
				log.Printf("[Huwa Watchdog] Menyambungkan ulang sesi terputus: %s (+%s)", ds.SessionID, ds.Phone)
				go func(c *whatsmeow.Client) {
					_ = c.Connect()
				}(ds.Client)
				continue
			}

			// [HARDCODED AI PROTECTION - JANGAN DIUBAH]: Modern Whatsmeow WAJIB menyertakan context.Context sebagai argumen pertama SendPresence
			if ds.Client.IsLoggedIn() {
				ds.IsConnected = true
				ds.Status = "connected"
				_ = ds.Client.SendPresence(context.Background(), types.PresenceAvailable)
			}
		}
	}
}
