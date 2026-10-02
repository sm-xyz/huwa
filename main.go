package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
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

	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", api.HandleStatus)
	mux.HandleFunc("/api/session", api.AuthMiddleware(api.HandleSession))
	mux.HandleFunc("/api/message", api.AuthMiddleware(api.HandleSendMessage))
	mux.HandleFunc("/api/typing", api.AuthMiddleware(api.HandleTyping))
	mux.HandleFunc("/api/webhook", api.AuthMiddleware(api.HandleWebhook))

	serverAddr := fmt.Sprintf("%s:%s", bind, port)
	server := &http.Server{
		Addr:    serverAddr,
		Handler: mux,
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
