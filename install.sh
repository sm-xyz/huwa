#!/bin/bash
# ==============================================================================
# HUWA (High-Utility WhatsApp Agent) - One-Click Installer for aaPanel / Linux
# ==============================================================================
set -e

# Otomatis eskalasi ke sudo jika dijalankan oleh non-root user (seperti user ibun)
if [ "$EUID" -ne 0 ]; then
  echo "[INFO] Skrip membutuhkan hak akses root. Melanjutkan dengan sudo..."
  exec sudo -E bash "$0" "$@"
fi

# Pastikan /usr/local/go/bin dan /usr/bin ada dalam PATH
export PATH="/usr/local/go/bin:/usr/bin:$PATH"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

echo -e "${BLUE}================================================================${NC}"
echo -e "${GREEN}   HUWA (High-Utility WhatsApp Agent) - Automated Installer     ${NC}"
echo -e "${BLUE}================================================================${NC}"

INSTALL_DIR="/opt/huwa"
DATA_DIR="/var/lib/huwa"
LOG_DIR="/var/log/huwa"
BIN_TARGET="$INSTALL_DIR/huwa"
SERVICE_FILE="/etc/systemd/system/huwa.service"

echo -e "${YELLOW}[1/6] Mempersiapkan direktori sistem...${NC}"
mkdir -p "$INSTALL_DIR"
mkdir -p "$DATA_DIR"
mkdir -p "$LOG_DIR"
chmod 755 "$INSTALL_DIR"
chmod 755 "$DATA_DIR"
chmod 755 "$LOG_DIR"

echo -e "${YELLOW}[2/6] Memeriksa compiler Golang...${NC}"
NEED_GO_INSTALL=false
if ! command -v go &> /dev/null; then
    NEED_GO_INSTALL=true
else
    # Cek versi minimal
    GO_VER=$(go version | awk '{print $3}' | sed 's/go//')
    GO_MAJOR=$(echo "$GO_VER" | cut -d. -f1)
    GO_MINOR=$(echo "$GO_VER" | cut -d. -f2)
    if [ "$GO_MAJOR" -lt 1 ] || [ "$GO_MINOR" -lt 22 ]; then
        echo -e "${BLUE}[INFO] Versi Go terpasang ($GO_VER) terlalu lama. Memperbarui ke Go terbaru...${NC}"
        NEED_GO_INSTALL=true
    fi
fi

if [ "$NEED_GO_INSTALL" = true ]; then
    echo -e "${BLUE}[INFO] Mengunduh dan menginstall Golang 1.22 resmi...${NC}"
    ARCH=$(uname -m)
    GO_ARCH="amd64"
    if [ "$ARCH" = "aarch64" ] || [ "$ARCH" = "arm64" ]; then
        GO_ARCH="arm64"
    fi
    GO_TAR="go1.22.8.linux-$GO_ARCH.tar.gz"
    wget -q --show-progress "https://go.dev/dl/$GO_TAR" -O "/tmp/$GO_TAR"
    rm -rf /usr/local/go && tar -C /usr/local -xzf "/tmp/$GO_TAR"
    rm -f "/tmp/$GO_TAR"
    export PATH="/usr/local/go/bin:$PATH"
    if ! grep -q "/usr/local/go/bin" /etc/profile; then
        echo 'export PATH=$PATH:/usr/local/go/bin' >> /etc/profile
    fi
fi

# Buat symlink universal di /usr/bin agar user mana pun bisa langsung akses perintah go
ln -sf /usr/local/go/bin/go /usr/bin/go 2>/dev/null || true
ln -sf /usr/local/go/bin/gofmt /usr/bin/gofmt 2>/dev/null || true

echo -e "${GREEN}[OK] Golang aktif: $(go version)${NC}"

echo -e "${YELLOW}[3/6] Mengompilasi binary huwa (Pure Go, Statically Linked)...${NC}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# Pastikan go.mod bersih
echo -e "module huwa\n\ngo 1.22" > "$SCRIPT_DIR/go.mod"

# Download dependensi resmi
echo "[INFO] Mengunduh modul whatsmeow dan modernc sqlite..."
go get -u go.mau.fi/whatsmeow@latest
go get -u github.com/skip2/go-qrcode@latest
go get -u modernc.org/sqlite@latest
go mod tidy

# Kompilasi ke binary lokal lalu salin dengan aman
echo "[INFO] Mengompilasi binary huwa..."
CGO_ENABLED=0 go build -ldflags="-s -w" -o "$SCRIPT_DIR/huwa_bin" .

# Hentikan daemon sebentar jika sedang berjalan agar update binary tidak error 'text file busy'
systemctl stop huwa 2>/dev/null || true
install -m 755 "$SCRIPT_DIR/huwa_bin" "$BIN_TARGET"
rm -f "$SCRIPT_DIR/huwa_bin"

echo -e "${GREEN}[OK] Binary huwa berhasil dikompilasi ($(du -h "$BIN_TARGET" | cut -f1)).${NC}"

echo -e "${YELLOW}[4/6] Menyiapkan file konfigurasi environment...${NC}"
if [ ! -f "$INSTALL_DIR/huwa.env" ]; then
    RANDOM_SECRET=$(tr -dc A-Za-z0-9 </dev/urandom | head -c 32 ; echo)
    cat <<EOF > "$INSTALL_DIR/huwa.env"
HUWA_BIND=0.0.0.0
HUWA_PORT=8080
HUWA_SECRET=$RANDOM_SECRET
HUWA_DB_PATH=$DATA_DIR/huwa.db
HUWA_DEFAULT_WEBHOOK=http://127.0.0.1/wp-json/adv/v1/chatbot/webhook
HUWA_MAX_DEVICES=150
HUWA_LOG_LEVEL=info
HUWA_MIN_TYPING_DELAY=1500
HUWA_MAX_TYPING_DELAY=4000
HUWA_MIN_INTER_DELAY=2000
HUWA_MAX_INTER_DELAY=6000
EOF
    chmod 600 "$INSTALL_DIR/huwa.env"
    echo -e "${GREEN}[OK] Konfigurasi baru dibuat dengan secret otomatis.${NC}"
else
    echo -e "${BLUE}[INFO] Konfigurasi yang sudah ada dipertahankan di $INSTALL_DIR/huwa.env.${NC}"
fi

echo -e "${YELLOW}[5/6] Memasang Systemd Service Daemon...${NC}"
cat <<EOF > "$SERVICE_FILE"
[Unit]
Description=Huwa Native WhatsApp Gateway Daemon for Solusi-WP
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=$INSTALL_DIR
EnvironmentFile=$INSTALL_DIR/huwa.env
ExecStart=$BIN_TARGET
Restart=always
RestartSec=3s
LimitNOFILE=65535
StandardOutput=append:$LOG_DIR/huwa.log
StandardError=append:$LOG_DIR/huwa.err

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable huwa
systemctl restart huwa

echo -e "${YELLOW}[6/6] Memverifikasi status daemon huwa...${NC}"
sleep 3

if curl -s "http://127.0.0.1:8080/api/status" | grep -q "running"; then
    echo -e "${GREEN}================================================================${NC}"
    echo -e "${GREEN}   INSTALASI HUWA BERHASIL & DAEMON AKTIF NORMAL!              ${NC}"
    echo -e "${GREEN}================================================================${NC}"
    echo -e "Port Bind       : ${YELLOW}0.0.0.0:8080${NC}"
    SECRET_VAL=$(grep "HUWA_SECRET=" "$INSTALL_DIR/huwa.env" | cut -d'=' -f2)
    echo -e "Huwa Secret Key : ${YELLOW}$SECRET_VAL${NC}"
    echo -e "Database Path   : ${YELLOW}$DATA_DIR/huwa.db${NC}"
    echo -e "Log File        : ${YELLOW}$LOG_DIR/huwa.log${NC}"
    echo ""
    echo -e "${BLUE}Perintah Pengelolaan:${NC}"
    echo -e "• Cek status  : ${YELLOW}curl http://127.0.0.1:8080/api/status${NC}"
    echo -e "• Lihat log   : ${YELLOW}tail -f $LOG_DIR/huwa.log${NC}"
    echo -e "• Restart     : ${YELLOW}sudo systemctl restart huwa${NC}"
else
    echo -e "${RED}[WARNING] Service terpasang tetapi respon HTTP di port 8080 belum terdeteksi.${NC}"
    echo "Silakan periksa log via: sudo journalctl -u huwa -n 30 --no-pager"
fi
