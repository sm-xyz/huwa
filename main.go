package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		if intVal, err := strconv.Atoi(val); err == nil {
			return intVal
		}
	}
	return defaultVal
}

func main() {
	log.Println("==========================================================")
	log.Println("   HUWA (High-Utility WhatsApp Agent) - Starting Daemon   ")
	log.Println("==========================================================")

	bind := getEnv("HUWA_BIND", "127.0.0.1")
	port := getEnv("HUWA_PORT", "8080")
	dbPath := getEnv("HUWA_DB_PATH", "/var/lib/huwa/huwa.db")
	secret := getEnv("HUWA_SECRET", "huwa_secret_auth_paling_aman_12345")
	defaultWH := getEnv("HUWA_DEFAULT_WEBHOOK", "http://127.0.0.1/wp-json/adv/v1/chatbot/webhook")

	minTyping := getEnvInt("HUWA_MIN_TYPING_DELAY", 1500)
	maxTyping := getEnvInt("HUWA_MAX_TYPING_DELAY", 4000)
	minInter := getEnvInt("HUWA_MIN_INTER_DELAY", 2000)
	maxInter := getEnvInt("HUWA_MAX_INTER_DELAY", 6000)

	// Pastikan folder database ada
	_ = os.MkdirAll("/var/lib/huwa", 0755)
	_ = os.MkdirAll("/var/log/huwa", 0755)

	// Inisialisasi Engine Multi-Tenant
	mgr, err := NewEngineManager(dbPath, defaultWH, secret)
	if err != nil {
		log.Fatalf("[Huwa Fatal] Gagal menginisialisasi Whatsmeow Engine: %v", err)
	}

	// Inisialisasi Anti-Ban Queue Worker
	queue := NewAntiBanQueue(mgr, minTyping, maxTyping, minInter, maxInter)

	// Inisialisasi HTTP REST Handlers
	api := NewAPIHandler(mgr, queue, secret)

	// [NATIVE UNIVERSAL ROUTER] Zero-redirect & Zero-404 Router untuk seluruh variasi Nginx / Reverse Proxy
	router := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// CORS Headers universal di setiap response
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Huwa-Secret, X-Token, session-id, session_id, id, X-Session-ID, secret")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		path := strings.ToLower(strings.TrimRight(r.URL.Path, "/"))
		if path == "" {
			path = "/"
		}

		// 1. Status / Health Check (Bypass Auth)
		if path == "/api/status" || path == "/status" || path == "/health" || strings.HasSuffix(path, "/status") || strings.HasSuffix(path, "/health") {
			api.HandleStatus(w, r)
			return
		}

		// 2. Session / Pairing / QR Code (GET, POST, PUT, DELETE)
		if path == "/api/session" || path == "/session" || path == "/api/sessions" || path == "/sessions" ||
			path == "/api/qr" || path == "/qr" || strings.Contains(path, "session") || strings.Contains(path, "qr") {
			api.AuthMiddleware(api.HandleSession)(w, r)
			return
		}

		// 3. Send Message
		if path == "/api/message" || path == "/message" || path == "/api/send-message" || path == "/send-message" ||
			path == "/api/send" || path == "/send" || strings.Contains(path, "message") || strings.Contains(path, "send") {
			api.AuthMiddleware(api.HandleSendMessage)(w, r)
			return
		}

		// 4. Typing Presence
		if path == "/api/typing" || path == "/typing" || strings.Contains(path, "typing") {
			api.AuthMiddleware(api.HandleTyping)(w, r)
			return
		}

		// 5. Webhook Registration
		if path == "/api/webhook" || path == "/webhook" || strings.Contains(path, "webhook") {
			api.AuthMiddleware(api.HandleWebhook)(w, r)
			return
		}

		// 6. WhatsApp Groups List
		if path == "/api/group" || path == "/group" || path == "/api/groups" || path == "/groups" || strings.Contains(path, "group") {
			api.AuthMiddleware(api.HandleGroups)(w, r)
			return
		}

		// 7. Fallback cerdas: jika ada session_id di query / header, arahkan ke HandleSession
		if r.URL.Query().Get("session_id") != "" || r.Header.Get("session-id") != "" || r.Header.Get("session_id") != "" || r.Header.Get("X-Session-ID") != "" {
			api.AuthMiddleware(api.HandleSession)(w, r)
			return
		}

		log.Printf("[Huwa 404] Path tidak dikenali: %s %s from %s", r.Method, r.URL.RequestURI(), r.RemoteAddr)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("Endpoint %s tidak ditemukan di Huwa Gateway Node", r.URL.Path),
		})
	})

	serverAddr := fmt.Sprintf("%s:%s", bind, port)
	server := &http.Server{
		Addr:    serverAddr,
		Handler: router,
	}

	// Graceful Shutdown Handler
	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("[Huwa] Server HTTP aktif di %s", serverAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[Huwa Fatal] HTTP Server Error: %v", err)
		}
	}()

	<-stopChan
	log.Println("[Huwa] Menerima sinyal shutdown, memutuskan koneksi WhatsApp secara tertib...")
	mgr.mu.RLock()
	for id, s := range mgr.sessions {
		if s.Client != nil && s.Client.IsConnected() {
			log.Printf("[Huwa] Disconnecting sesi %s...", id)
			s.Client.Disconnect()
		}
	}
	mgr.mu.RUnlock()
	log.Println("[Huwa] Daemon berhasil dihentikan dengan aman.")
}
