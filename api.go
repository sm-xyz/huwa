package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// APIHandler mengelola seluruh route HTTP REST
type APIHandler struct {
	manager *EngineManager
	queue   *AntiBanQueue
	secret  string
}

func NewAPIHandler(mgr *EngineManager, queue *AntiBanQueue, secret string) *APIHandler {
	return &APIHandler{
		manager: mgr,
		queue:   queue,
		secret:  secret,
	}
}

// AuthMiddleware memvalidasi token rahasia X-Huwa-Secret atau query secret
func (h *APIHandler) AuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clientSecret := r.Header.Get("X-Huwa-Secret")
		if clientSecret == "" {
			clientSecret = r.Header.Get("X-Token")
		}
		if clientSecret == "" {
			clientSecret = r.URL.Query().Get("secret")
		}

		if h.secret != "" && clientSecret != h.secret {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "Akses ditolak: Token X-Huwa-Secret tidak valid",
			})
			return
		}

		next(w, r)
	}
}

// HandleStatus mengembalikan health check dan jumlah nomor aktif
func (h *APIHandler) HandleStatus(w http.ResponseWriter, r *http.Request) {
	h.manager.mu.RLock()
	totalSessions := len(h.manager.sessions)
	connected := 0
	for _, s := range h.manager.sessions {
		if s.IsConnected {
			connected++
		}
	}
	h.manager.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success":            true,
		"engine":             "huwa-v1.0",
		"status":             "running",
		"total_sessions":     totalSessions,
		"connected_sessions": connected,
		"timestamp":          time.Now().Format(time.RFC3339),
	})
}

// HandleSession menangani pengecekan status atau inisialisasi QR Code
func (h *APIHandler) HandleSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		sessionID = r.Header.Get("X-Session-ID")
	}
	if sessionID == "" {
		sessionID = "default"
	}

	uidStr := r.URL.Query().Get("uid")
	uid, _ := strconv.Atoi(uidStr)

	ds, err := h.manager.GetOrCreateSession(sessionID, uid)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
		return
	}

	if r.Method == http.MethodGet {
		if ds.IsConnected {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success":      true,
				"is_connected": true,
				"status":       "connected",
				"phone":        ds.Phone,
				"name":         ds.Name,
				"push_name":    ds.PushName,
			})
			return
		}

		// Belum terhubung, ambil QR Code
		qrBase64, err := h.manager.GenerateQR(ds)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success":      true,
			"is_connected": ds.IsConnected,
			"status":       ds.Status,
			"qrcode":       qrBase64,
			"qr":           qrBase64,
			"phone":        ds.Phone,
		})
		return
	}

	if r.Method == http.MethodDelete {
		h.manager.mu.Lock()
		if ds.Client != nil {
			_ = ds.Client.Logout()
			ds.Client.Disconnect()
		}
		delete(h.manager.sessions, sessionID)
		h.manager.mu.Unlock()

		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "Sesi berhasil dihapus dan diputus",
		})
		return
	}
}

// HandleSendMessage menangani pengiriman pesan teks/gambar
func (h *APIHandler) HandleSendMessage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		SessionID   string `json:"session_id"`
		Phone       string `json:"phone"`
		Group       string `json:"group"`
		Type        string `json:"type"`
		Text        string `json:"text"`
		MediaURL    string `json:"media_url"`
		TypingDelay int    `json:"typing_delay"`
		Async       bool   `json:"async"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "Payload JSON tidak valid"})
		return
	}

	if req.SessionID == "" {
		req.SessionID = r.Header.Get("X-Session-ID")
		if req.SessionID == "" {
			req.SessionID = "default"
		}
	}

	if req.Phone == "" && req.Group == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "Nomor tujuan (phone) atau group JID wajib diisi"})
		return
	}

	// Jika mode async (antrean broadcast masal anti-banned)
	if req.Async {
		h.queue.Enqueue(&QueueItem{
			SessionID:   req.SessionID,
			Phone:       req.Phone,
			GroupJID:    req.Group,
			Text:        req.Text,
			MediaURL:    req.MediaURL,
			TypingDelay: req.TypingDelay,
		})

		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"status":  "queued",
			"message": "Pesan telah masuk dalam antrean aman",
		})
		return
	}

	// Kirim langsung (Synchronous untuk Chatbot AI / Notifikasi OTP)
	if req.TypingDelay > 0 {
		_ = h.manager.SendTypingPresence(req.SessionID, req.Phone, req.Group)
		time.Sleep(time.Duration(req.TypingDelay) * time.Millisecond)
	}

	msgID, err := h.manager.SendTextMessage(req.SessionID, req.Phone, req.Group, req.Text)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"message_id": msgID,
		"status":     "sent",
	})
}

// HandleTyping mengirim sinyal typing presence
func (h *APIHandler) HandleTyping(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req struct {
		SessionID string `json:"session_id"`
		Phone     string `json:"phone"`
		Group     string `json:"group"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	if req.SessionID == "" {
		req.SessionID = r.Header.Get("X-Session-ID")
		if req.SessionID == "" {
			req.SessionID = "default"
		}
	}

	err := h.manager.SendTypingPresence(req.SessionID, req.Phone, req.Group)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// HandleWebhook mendaftarkan URL webhook untuk sesi tertentu
func (h *APIHandler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req struct {
		SessionID string `json:"session_id"`
		URL       string `json:"url"`
		Token     string `json:"token"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	if req.SessionID == "" {
		req.SessionID = r.Header.Get("X-Session-ID")
		if req.SessionID == "" {
			req.SessionID = "default"
		}
	}

	h.manager.mu.Lock()
	if ds, exists := h.manager.sessions[req.SessionID]; exists {
		ds.WebhookURL = req.URL
		ds.WebhookSecret = req.Token
	}
	h.manager.mu.Unlock()

	_, _ = h.manager.db.Exec(`
		UPDATE huwa_metadata SET webhook_url = ?, webhook_secret = ?, updated_at = CURRENT_TIMESTAMP
		WHERE session_id = ?
	`, req.URL, req.Token, req.SessionID)

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Webhook URL berhasil dikonfigurasi",
	})
}
