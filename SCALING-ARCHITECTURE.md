# ARSITEKTUR SKALABILITAS HUWA: DARI PULUHAN HINGGA RATUSAN RIBU NOMOR WHATSAPP

Dokumen ini menjelaskan strategi rekayasa infrastruktur, manajemen konkurensi, dan mitigasi anti-banned agar **`huwa`** mampu bertransformasi dari instalasi single-node (1 – 100 nomor) hingga kluster terdistribusi masal (**10.000 – 100.000+ nomor WhatsApp**) secara stabil, aman, dan efisien.

---

## 1. MATEMATIKA RESOURCE: JEJAK MEMORI & KONEKSI TCP

Protokol Multi-Device WhatsApp (Whatsmeow) berjalan via **persistent encrypted TCP socket (TLS Noise Protocol)** langsung ke server Meta tanpa browser Chromium.

| Jumlah Device | Kebutuhan RAM | Kebutuhan CPU | IOPS Disk (SQLite/DB) | Rekomendasi Infrastruktur |
|---|---|---|---|---|
| **1 – 100 Nomor** | **1.8 – 2.5 GB** | 2 – 4 vCPU | Standar SSD aaPanel | **Single VPS (VPS yang sama dengan WordPress)** |
| **500 – 1.000 Nomor** | **12 – 20 GB** | 8 – 16 vCPU | NVMe Enterprise SSD | **1 Dedicated Server / VPS High-Memory** |
| **5.000 – 10.000 Nomor** | **100 – 180 GB** | 64 – 128 Core | Distributed DB / Redis | **Kluster 5 – 8 Node Worker (24 GB RAM/node)** |
| **50.000 – 100.000 Nomor** | **1.0 – 1.8 TB** | Kluster Terdistribusi | Sharded DB + NATS Bus | **Kluster Horisontal 30 – 50 Node Worker** |

---

## 2. EMPAT TIER TAHAPAN SKALABILITAS HUWA

```
                  ┌─────────────────────────────────────────────────┐
                  │           WordPress Core (solusi-wp)            │
                  │   REST API Caller / Webhook Receiver Controller │
                  └────────────────────────┬────────────────────────┘
                                           │
                                           ▼
                  ┌─────────────────────────────────────────────────┐
                  │            HUWA API ROUTER & GATEWAY            │
                  │       (Consistent Hashing by SessionID)         │
                  └──────┬─────────────────┬─────────────────┬──────┘
                         │                 │                 │
                         ▼                 ▼                 ▼
                 ┌──────────────┐  ┌──────────────┐  ┌──────────────┐
                 │  Worker #1   │  │  Worker #2   │  │  Worker #N   │
                 │ 2.000 Nomor  │  │ 2.000 Nomor  │  │ 2.000 Nomor  │
                 │  (Whatsmeow) │  │  (Whatsmeow) │  │  (Whatsmeow) │
                 └──────┬───────┘  └──────┬───────┘  └──────┬───────┘
                        │                 │                 │
                        ▼                 ▼                 ▼
                 [Proxy Pool #1]   [Proxy Pool #2]   [Proxy Pool #N]
                 (Subnet IP A)     (Subnet IP B)     (Subnet IP C)
                        │                 │                 │
                        └─────────────────┼─────────────────┘
                                          ▼
                         [ WhatsApp Cloud Infrastructure ]
```

---

### TIER 1: TAHAP AWAL (1 – 100 NOMOR) — *Current Stage*
- **Penempatan**: Satu VPS aaPanel bersama `solusi-wp`.
- **Database**: SQLite 3 WAL Mode (`/var/lib/huwa/huwa.db`).
- **Komunikasi**: Internal loopback `127.0.0.1:8080`.
- **Karakteristik**: Nol latensi, biaya server minimum, maintenance sangat simpel tanpa server tambahan.

---

### TIER 2: TAHAP MENENGAH (100 – 1.000 NOMOR) — *Dedicated Gateway Node*
Saat jumlah CS dan affiliator bertambah menjadi ratusan nomor:
1. **Pemisahan VPS**:
   - **VPS 1**: WordPress `solusi-wp` + Nginx + MySQL (Fokus pada web application).
   - **VPS 2**: Khusus `huwa` Gateway (Dedicated 8 Core / 16 GB RAM).
2. **Sharding File SQLite**:
   - Membagi SQLite per-tenant directory: `/var/lib/huwa/shards/{tenant_id}.db` agar penulisan file database tidak pernah mengalami *lock contention*.
3. **Koneksi Private Network (VPC)**:
   - Hubungkan VPS 1 dan VPS 2 menggunakan Private IP internal cloud provider (latensi $< 1\text{ ms}$).

---

### TIER 3: TAHAP ENTERPRISE (1.000 – 10.000 NOMOR) — *Stateful Worker Kluster*
WhatsApp membutuhkan koneksi TCP yang *stateful* (nomor A harus selalu terhubung di server yang sama selama online).
1. **Huwa Router (Reverse Proxy Layer)**:
   - Menggunakan algoritma **Consistent Hashing**:
     $$\text{Target Node} = \text{MurmurHash3}(\text{SessionID}) \pmod{\text{Jumlah Node}}$$
   - Request dari `solusi-wp` cukup menembak ke Router Utama. Router secara otomatis meneruskan paket ke Node Worker tempat nomor tersebut aktif.
2. **Database Pusat (PostgreSQL / CockroachDB)**:
   - Whatsmeow memiliki adapter native untuk PostgreSQL (`sqlstore.New("postgres", ...)`).
   - Seluruh kunci enkripsi dan session store tersimpan di database PostgreSQL terpusat dengan sistem replikasi master-slave.
3. **Central Event Bus (NATS JetStream / Redis Cluster)**:
   - Antrean broadcast dari ratusan user dimasukkan ke antrean terpusat, lalu didistribusikan ke masing-masing node worker secara paralel.

---

### TIER 4: SKALA MASAL (10.000 – 100.000+ NOMOR) — *Global Telco Grade*
1. **Dynamic Egress IP Subnet Sharding**: Setiap kluster worker memiliki pool ratusan IP publik yang dialokasikan per blok nomor.
2. **Kubernetes StatefulSets**: Setiap pod mengelola 500 – 1.000 nomor dengan alokasi volume persistent SSD masing-masing.
3. **Automated Health Monitoring & Circuit Breaker**: Jika ada node worker yang mengalami kegagalan hardware, orchestrator secara otomatis memindahkan sesi ke node cadangan dalam waktu $< 10$ detik.

---

## 3. FORMULA ANTI-BANNED UNTUK SKALA PULUHAN RIBU NOMOR

Mengelola ribuan nomor WhatsApp memiliki tantangan terbesar pada **sistem radar keamanan Meta**. Jika ribuan nomor dijalankan secara ceroboh dari satu data center, seluruh subnet IP dapat di-flag bersamaan.

Huwa mengimplementasikan 5 pilar anti-banned masal:

### 1. Egress IP & Proxy Rotation (Multi-Subnet)
- **Problem**: 5.000 nomor WhatsApp aktif dari satu IP server VPS yang sama akan langsung memicu deteksi anomali Meta.
- **Solusi Huwa**: Integrasi SOCKS5 / HTTP Proxy Pool per tenant.
  - Whatsmeow mendukung konfigurasi `client.SetProxyAddress("socks5://user:pass@proxy_ip:port")`.
  - Setiap grup nomor (misal 20–50 nomor) menggunakan IP keluar (egress IP) atau residential proxy terpisah.

### 2. Behavioral Typing Jitter (Simulasi Biologis)
- Manusia tidak pernah mengetik dengan kecepatan statis.
- Rumus jeda mengetik Huwa menggabungkan *Gaussian distribution*:
  $$\text{Delay} = \text{BaseDelay} + (\text{Length} \times \text{CharFactor}) + \mathcal{N}(\mu, \sigma^2)$$
- Sebelum pesan dikirim, sinyal `composing` wajib dikirimkan ke WhatsApp server, meniru tindakan user membuka chat dan mengetik di keyboard.

### 3. Account Warm-up Curve (Pemanasan Nomor Baru)
Untuk nomor WhatsApp yang baru didaftarkan ke sistem, antrean worker otomatis menerapkan kurva pemanasan:
- **Hari 1 – 3**: Maksimal 30 – 50 pesan per hari (jeda 15 – 30 detik).
- **Hari 4 – 7**: Maksimal 100 – 200 pesan per hari (jeda 10 – 20 detik).
- **Hari 8+**: Kuota normal broadcast sesuai paket pengguna di `solusi-wp`.

### 4. Content Checksum Obfuscation (Zero-Width & Spintax)
- Jika 1.000 nomor mengirim teks promo yang 100% identik, algoritma spam Meta mendeteksi *pattern matching*.
- Huwa mewajibkan integrasi Spintax `{Halo|Hai|Selamat Pagi}` dan secara otomatis menyisipkan karakter tersembunyi (*Unicode Zero-Width Joiner* `\u200D` / `\u200B`) pada posisi acak. Checksum SHA-256 setiap pesan yang keluar menjadi **unik 100%**.

### 5. Smart Circuit Breaker & Health Score
- Huwa memantau *bounce rate* dan status respons Meta:
  - Jika sebuah nomor menerima status respon 429 (*Too Many Requests*) atau 408 (*Timeout*), antrean nomor tersebut langsung di-pause (*cooling down*) selama 10 – 30 menit secara otomatis.
  - Melindungi nomor agar tidak sampai menerima sanksi banned permanen.

---

## 4. KESIMPULAN ARSITEKTURAL

Dengan fondasi **Golang Whatsmeow + SQLite/PostgreSQL Store + Consistent Hashing Router**, arsitektur `huwa` menjamin:
1. **Hari ini**: Berjalan sangat efisien dan ringan di VPS yang sama dengan `solusi-wp` (RAM ~20 MB/nomor).
2. **Masa Depan**: Kapan pun Anda memutuskan untuk scaling bisnis ke ribuan atau ratusan ribu pengguna/nomor WhatsApp, sistem tidak perlu dibongkar ulang dari nol, melainkan cukup menambahkan worker node secara horizontal sesuai panduan arsitektur ini.
