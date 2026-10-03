package main

import (
	"context"
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
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Huwa-Secret, X-Token, session-id, session_id, id, X-Session-ID")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Bypass auth untuk status/health check
		if r.URL.Path == "/api/status" || r.URL.Path == "/status" || r.URL.Path == "/health" {
			next(w, r)
			return
		}

		clientSecret := strings.TrimSpace(r.Header.Get("X-Huwa-Secret"))
		if clientSecret == "" {
			clientSecret = strings.TrimSpace(r.Header.Get("X-Token"))
		}
		if clientSecret == "" {
			clientSecret = strings.TrimSpace(r.URL.Query().Get("secret"))
		}

		serverSecret := strings.TrimSpace(h.secret)
		if serverSecret != "" && clientSecret != serverSecret {
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

// HandleSession menangani inisialisasi QR Code, pengecekan status, dan pemutusan sesi
func (h *APIHandler) HandleSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Ekstrak sessionID dari berbagai sumber: Headers, Query, Path, Body
	sessionID := r.Header.Get("session-id")
	if sessionID == "" {
		sessionID = r.Header.Get("session_id")
	}
	if sessionID == "" {
		sessionID = r.Header.Get("id")
	}
	if sessionID == "" {
		sessionID = r.Header.Get("X-Session-ID")
	}
	if sessionID == "" {
		sessionID = r.URL.Query().Get("session_id")
	}
	if sessionID == "" {
		sessionID = r.URL.Query().Get("sessionId")
	}

	// Cek path jika endpoint berbentuk /session/{id} atau /sessions/{id}
	if sessionID == "" {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) >= 2 {
			last := parts[len(parts)-1]
			if last != "session" && last != "sessions" && last != "" {
				sessionID = last
			}
		}
	}

	var reqBody struct {
		SessionID     string `json:"session_id"`
		SessionIdAlt  string `json:"sessionId"`
		ID            string `json:"id"`
		UserID        int    `json:"user_id"`
		WebhookURL    string `json:"webhook_url"`
		WebhookSecret string `json:"webhook_secret"`
	}

	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&reqBody)
			if sessionID == "" {
				if reqBody.SessionID != "" {
					sessionID = reqBody.SessionID
				} else if reqBody.SessionIdAlt != "" {
					sessionID = reqBody.SessionIdAlt
				} else if reqBody.ID != "" {
					sessionID = reqBody.ID
				}
			}
		}
	}

	if sessionID == "" {
		sessionID = "usr_default"
	}

	uid := reqBody.UserID
	if uid == 0 {
		uidStr := r.URL.Query().Get("uid")
		uid, _ = strconv.Atoi(uidStr)
	}

	ds, err := h.manager.GetOrCreateSession(sessionID, uid)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
		return
	}

	if reqBody.WebhookURL != "" {
		ds.WebhookURL = reqBody.WebhookURL
	} else if qWh := r.URL.Query().Get("webhook_url"); qWh != "" {
		ds.WebhookURL = qWh
	}
	if reqBody.WebhookSecret != "" {
		ds.WebhookSecret = reqBody.WebhookSecret
	} else if qSec := r.URL.Query().Get("webhook_secret"); qSec != "" {
		ds.WebhookSecret = qSec
	}

	// METHOD GET, POST, PUT: Semua mengembalikan status sesi & QR code jika pairing
	if r.Method == http.MethodGet || r.Method == http.MethodPost || r.Method == http.MethodPut {
		// Jika metode PUT atau query reset=1 (reset QR), putuskan koneksi client aktif dan generate QR baru
		isReset := (r.Method == http.MethodPut || r.URL.Query().Get("reset") == "1" || r.URL.Query().Get("force_new") == "1")
		if isReset && !ds.IsConnected {
			if ds.Client != nil {
				ds.Client.Disconnect()
			}
			ds.QRCode = ""
			ds.Status = "pairing"
		}

		if ds.IsConnected {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success":      true,
				"is_connected": true,
				"isConnected":  true,
				"status":       "connected",
				"phone":        ds.Phone,
				"name":         ds.Name,
				"push_name":    ds.PushName,
				"pairing_code": "",
				"id":           sessionID,
				"sessionId":    sessionID,
				"session_id":   sessionID,
			})
			return
		}

		// Jika QR Code / Pairing Code sudah ada dan sesi masih pairing serta bukan reset
		if !isReset && ds.Status == "pairing" && (ds.QRCode != "" || ds.PairingCode != "") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success":      true,
				"is_connected": false,
				"isConnected":  false,
				"status":       "pairing",
				"pairing_code": ds.PairingCode,
				"qrcode":       ds.QRCode,
				"qr":           ds.QRCode,
				"phone":        ds.Phone,
				"id":           sessionID,
				"sessionId":    sessionID,
				"session_id":   sessionID,
			})
			return
		}

		qrBase64, err := h.manager.GenerateQR(ds)
		if err != nil {
			// Jika error tapi sudah ada QRCode/PairingCode sebelumnya, kembalikan data sebelumnya
			if ds.QRCode != "" || ds.PairingCode != "" {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"success":      true,
					"is_connected": false,
					"isConnected":  false,
					"status":       "pairing",
					"pairing_code": ds.PairingCode,
					"qrcode":       ds.QRCode,
					"qr":           ds.QRCode,
					"phone":        ds.Phone,
					"id":           sessionID,
					"sessionId":    sessionID,
					"session_id":   sessionID,
				})
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success":      true,
			"is_connected": ds.IsConnected,
			"isConnected":  ds.IsConnected,
			"status":       ds.Status,
			"pairing_code": ds.PairingCode,
			"qrcode":       qrBase64,
			"qr":           qrBase64,
			"phone":        ds.Phone,
			"id":           sessionID,
			"sessionId":    sessionID,
			"session_id":   sessionID,
		})
		return
	}

	if r.Method == http.MethodDelete {
		h.manager.mu.Lock()
		if ds.Client != nil {
			_ = ds.Client.Logout(context.Background())
			ds.Client.Disconnect()
			if ds.Client.Store != nil {
				// [HARDCODED AI PROTECTION - JANGAN DIUBAH]: Whatsmeow versi modern wajib menerima argumen context.Context pada Store.Delete()
				_ = ds.Client.Store.Delete(context.Background())
			}
		}
		delete(h.manager.sessions, sessionID)
		_, _ = h.manager.db.Exec("DELETE FROM huwa_metadata WHERE session_id = ?", sessionID)
		h.manager.mu.Unlock()

		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "Sesi berhasil dihapus dan diputus secara permanen",
		})
		return
	}
}

// HandlePairingCode menangani permintaan pembuatan 8-digit Kode Pairing WhatsApp tanpa scan QR
func (h *APIHandler) HandlePairingCode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "Metode tidak diizinkan"})
		return
	}

	sessionID := r.Header.Get("session-id")
	if sessionID == "" {
		sessionID = r.Header.Get("session_id")
	}
	if sessionID == "" {
		sessionID = r.Header.Get("X-Session-ID")
	}
	if sessionID == "" {
		sessionID = r.URL.Query().Get("session_id")
	}
	if sessionID == "" {
		sessionID = r.URL.Query().Get("sessionId")
	}

	var reqBody struct {
		SessionID     string `json:"session_id"`
		SessionIdAlt  string `json:"sessionId"`
		Phone         string `json:"phone"`
		PhoneNumber   string `json:"phone_number"`
		UserID        int    `json:"user_id"`
		WebhookURL    string `json:"webhook_url"`
		WebhookSecret string `json:"webhook_secret"`
	}

	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
		if sessionID == "" {
			if reqBody.SessionID != "" {
				sessionID = reqBody.SessionID
			} else if reqBody.SessionIdAlt != "" {
				sessionID = reqBody.SessionIdAlt
			}
		}
	}

	phone := reqBody.Phone
	if phone == "" {
		phone = reqBody.PhoneNumber
	}
	if phone == "" {
		phone = r.URL.Query().Get("phone")
	}

	if phone == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "Nomor WhatsApp wajib diisi untuk membuat kode pairing",
		})
		return
	}

	if sessionID == "" {
		sessionID = "usr_default"
	}

	uid := reqBody.UserID
	if uid == 0 {
		uidStr := r.URL.Query().Get("uid")
		uid, _ = strconv.Atoi(uidStr)
	}

	ds, err := h.manager.GetOrCreateSession(sessionID, uid)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
		return
	}

	if reqBody.WebhookURL != "" {
		ds.WebhookURL = reqBody.WebhookURL
	} else if qWh := r.URL.Query().Get("webhook_url"); qWh != "" {
		ds.WebhookURL = qWh
	}
	if reqBody.WebhookSecret != "" {
		ds.WebhookSecret = reqBody.WebhookSecret
	} else if qSec := r.URL.Query().Get("webhook_secret"); qSec != "" {
		ds.WebhookSecret = qSec
	}

	// Minta kode pairing dari engine Whatsmeow
	code, err := h.manager.PairPhone(ds, phone)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success":      true,
		"pairing_code": code,
		"code":         code,
		"phone":        ds.Phone,
		"status":       ds.Status,
		"is_connected": ds.IsConnected,
		"id":           sessionID,
		"session_id":   sessionID,
	})
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
		Target      string `json:"target"`
		Group       string `json:"group"`
		Type        string `json:"type"`
		Text        string `json:"text"`
		Message     string `json:"message"`
		MediaURL    string `json:"media_url"`
		TypingDelay int    `json:"typing_delay"`
		Async       bool   `json:"async"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "Payload JSON tidak valid"})
		return
	}

	if req.Phone == "" && req.Target != "" {
		req.Phone = req.Target
	}
	if req.Text == "" && req.Message != "" {
		req.Text = req.Message
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

	msgID, err := h.manager.SendTextMessage(req.SessionID, req.Phone, req.Group, req.Text, req.MediaURL)
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

// HandleWebhook mendaftarkan atau mengecek URL webhook untuk sesi tertentu
func (h *APIHandler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method == http.MethodGet {
		sessionID := r.URL.Query().Get("session_id")
		if sessionID == "" {
			sessionID = r.Header.Get("X-Session-ID")
		}
		if sessionID == "" {
			sessionID = "default"
		}
		h.manager.mu.RLock()
		whURL := ""
		whSec := ""
		if ds, exists := h.manager.sessions[sessionID]; exists {
			whURL = ds.WebhookURL
			whSec = ds.WebhookSecret
		}
		h.manager.mu.RUnlock()

		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"webhook": map[string]interface{}{
				"url":        whURL,
				"token":      whSec,
				"session_id": sessionID,
			},
		})
		return
	}

	var req struct {
		SessionID string `json:"session_id"`
		URL       string `json:"url"`
		Token     string `json:"token"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

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

// HandleGroups mengembalikan daftar grup WhatsApp yang diikuti sesi
func (h *APIHandler) HandleGroups(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		sessionID = r.Header.Get("X-Session-ID")
	}
	if sessionID == "" {
		sessionID = r.Header.Get("session-id")
	}
	if sessionID == "" {
		sessionID = r.Header.Get("session_id")
	}
	if sessionID == "" {
		sessionID = "default"
	}

	groups, err := h.manager.GetJoinedGroups(sessionID)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"docs":    groups,
		"data":    groups,
	})
}
