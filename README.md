# HUWA (High-Utility WhatsApp Agent)
### High-Performance Native Multi-Tenant WhatsApp Gateway for Solusi-WP

HUWA adalah WhatsApp Gateway berbasis **Golang (Whatsmeow)** murni tanpa CGO, dirancang khusus untuk menangani 100–150 device WhatsApp aktif per VPS Node dengan konsumsi RAM super ringan (~20 MB per nomor), simulasi perilaku manusia anti-banned, dan integrasi mulus dengan WordPress Plugin `solusi-wp`.

---

## ⚡ Fitur Utama
- **Native Multi-Tenant**: Mengelola hingga 150 nomor WhatsApp aktif secara simultan dalam 1 SQLite database berkinerja tinggi (WAL Mode + Foreign Keys).
- **Zero Heavy Dependencies**: Murni binary Go statis tanpa Chromium, tanpa Node.js, tanpa Docker, dan tanpa CGO (`CGO_ENABLED=0`).
- **Anti-Banned Human Simulation**:
  - Simulasi mengetik acak (*dynamic typing presence* 1.5s – 4.0s).
  - Jeda pengiriman acak (*random jitter delay* 2s – 6s) untuk blast / broadcast pesan.
  - Circuit Breaker proteksi rate-limit.
- **REST API + Realtime Webhook**: Kompatibel 100% dengan modul AI Chatbot Bynara dan WhatsApp Rotator.

---

## 🚀 Panduan Instalasi Cepat (1-Click)

Dapat dijalankan langsung dari akun non-root. Skrip akan otomatis mengeskalasi `sudo` dan mengonfigurasi segalanya:

```bash
# 1. Clone repository
git clone https://github.com/sm-xyz/huwa.git /tmp/huwa-src

# 2. Masuk dan jalankan installer
cd /tmp/huwa-src
bash install.sh
```

Installer otomatis melakukan:
1. Pengecekan & instalasi compiler Go resmi jika belum ada.
2. Pembuatan symlink global `/usr/bin/go` dan `/usr/bin/gofmt`.
3. Kompilasi binary mandiri `/opt/huwa/huwa`.
4. Pembuatan secret token otorisasi acak di `/opt/huwa/huwa.env`.
5. Pemasangan & aktivasi systemd service daemon `huwa.service`.
6. Verifikasi kesehatan API endpoint.

---

## 🛡️ Pengaturan Firewall (UFW)
Untuk mengamankan VPS Huwa agar port `8080` hanya bisa diakses oleh IP VPS Anda:

```bash
sudo ufw default deny incoming
sudo ufw allow 22/tcp
sudo ufw allow from <IP_VPS> to any port 8080 proto tcp
sudo ufw enable
```

---

## 🛠️ Perintah Pengelolaan
- **Cek Status**: `curl http://127.0.0.1:8080/api/status`
- **Lihat Service**: `sudo systemctl status huwa`
- **Lihat Log**: `sudo tail -f /var/log/huwa/huwa.log`
- **Restart Daemon**: `sudo systemctl restart huwa`
- **Ambil Secret Token**: `sudo grep "HUWA_SECRET=" /opt/huwa/huwa.env`
