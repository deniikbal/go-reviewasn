# ReviewASN

Aplikasi otomasi review kinerja ASN (SMARTASN / Kinerja Jabarprov) — porting dari versi Python (Playwright) ke **Go + Rod**.

Login otomatis (NIP + password + MFA/TOTP), membuka halaman Kinerja, mengambil kartu "Belum Direview", lalu mengisi review perilaku ("Selalu" + Simpan) untuk semua item.

## Fitur

| Mode | Perintah | Fungsi |
|---|---|---|
| Interaktif | `ReviewASN.exe` | Jalankan review lewat menu terminal |
| Telegram | `ReviewASN --telegram` | Bot Telegram: daftar akun, jalankan review, lihat status & kode MFA |
| QR MFA | `ReviewASN --qr` | Scan QR TOTP dari screenshot di folder `qr/`, tampilkan kode 6 digit live |
| Cek mandiri | `ReviewASN --cek` | Cek Chrome, config, dan kesiapan aplikasi |
| Cek alur | `ReviewASN --cek-alur` | Uji login → buka Kinerja → baca daftar review **tanpa** menyimpan review |
| Diagnosa | `ReviewASN --probe` | Buang struktur DOM dashboard SSO ke `probe_*.json` (debug) |
| Kelola akun | `ReviewASN --akun` | Tambah/hapus akun via terminal |

Flag tambahan:

- `--token <token>` — token bot Telegram (alternatif env `TELEGRAM_TOKEN`)

## Konfigurasi

Salin `.env.example` menjadi `.env` lalu isi:

```env
TELEGRAM_TOKEN=123456789:AAHfLx....   # token bot dari @BotFather
TG_IZIN=346120845                     # user ID Telegram yang boleh pakai bot
```

Data akun (NIP, password, secret TOTP) tersimpan di `akun.json` (dibuat otomatis via wizard saat pertama kali jalan, atau kelola lewat bot).

> ⚠️ **Keamanan:** `.env` dan `akun.json` berisi kredensial. Keduanya sudah masuk `.gitignore` — jangan pernah di-commit atau dibagikan.

## Deploy di VPS (Linux)

### 1. Persiapan server

```bash
# update & install dependensi dasar
sudo apt update && sudo apt install -y wget ca-certificates

# Google Chrome (dibutuhkan Rod untuk otomasi browser)
wget -q -O - https://dl.google.com/linux/linux_signing_key.pub | sudo gpg --dearmor -o /usr/share/keyrings/google-chrome.gpg
echo "deb [arch=amd64 signed-by=/usr/share/keyrings/google-chrome.gpg] http://dl.google.com/linux/chrome/deb/ stable main" | sudo tee /etc/apt/sources.list.d/google-chrome.list
sudo apt update && sudo apt install -y google-chrome-stable

# verifikasi
google-chrome --version
```

### 2. Install Go di VPS

Jangan pakai `apt install golang-go` (versinya terlalu tua — proyek ini butuh Go 1.26). Install manual:

```bash
wget https://go.dev/dl/go1.26.0.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.26.0.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc

# verifikasi
go version   # harus go1.26.0 atau lebih baru
```

### 3. Upload source code ke VPS

Dari PC Windows (Git Bash), upload folder proyek:

```bash
scp main.go qr.go probe.go telegram.go single.go lock_unix.go lock_windows.go go.mod go.sum root@IP_VPS:/opt/reviewasn/
scp .env.example root@IP_VPS:/opt/reviewasn/
```

### 4. Build di VPS

```bash
cd /opt/reviewasn
go build -o reviewasn .
```

Struktur folder di VPS:

```
/opt/reviewasn/
├── reviewasn        # hasil build
├── main.go qr.go probe.go telegram.go single.go lock_unix.go lock_windows.go
├── go.mod go.sum
├── .env             # konfigurasi (isi dulu!)
└── akun.json        # dibuat otomatis / salin dari PC
```

### 5. Jalankan

```bash
cd /opt/reviewasn
chmod +x reviewasn

# cek kesiapan
./reviewasn --cek

# jalankan bot Telegram
./reviewasn --telegram

### 6. Jalankan sebagai service (systemd)

Supaya tetap hidup setelah VPS di-restart / sesi SSH ditutup:

```bash
sudo tee /etc/systemd/system/reviewasn.service > /dev/null <<'EOF'
[Unit]
Description=ReviewASN - otomasi review SMARTASN
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/reviewasn
ExecStart=/opt/reviewasn/reviewasn --telegram
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now reviewasn

# cek status & log
sudo systemctl status reviewasn
journalctl -u reviewasn -f
```

Bot Telegram tidak butuh port terbuka — firewall tidak perlu diubah.
## Menggunakan via Telegram

1. Buat bot via [@BotFather](https://t.me/BotFather) → salin token → isi di `.env`
2. Cari user ID Telegram Anda (misal via [@userinfobot](https://t.me/userinfobot)) → isi di `TG_IZIN`
3. Alur bot: `/start` → kalau akun sudah tersimpan langsung tombol **▶ Jalankan Review** (sekali daftar tersimpan; daftar ulang cukup kirim NIP lagi)
4. Perintah bot: `/akun` daftar akun tersimpan, `/hapus <NIP>` hapus akun, `/status`, `/batal`

## Troubleshooting VPS

| Masalah | Solusi |
|---|---|
| Chrome tidak jalan | Pastikan `google-chrome --version` berhasil; Rod butuh Chrome/Chromium terpasang |
| Chrome crash saat headless | Sudah otomatis pakai `--no-sandbox` (wajib untuk root/container) |
| Bot tidak bisa konek | Cek internet/porxy VPS; bot butuh akses ke `api.telegram.org` (port 443) |
| Gagal konek Telegram | Periksa `TELEGRAM_TOKEN` di `.env`; jalankan `./reviewasn --telegram` untuk lihat error |
| Screenshot debug muncul | `debug*.png` di folder aplikasi dibuat saat scraping gagal — gunakan untuk diagnosa |

## Struktur proyek

```
main.go        # alur review, TOTP, wizard akun, entry point
telegram.go    # bot Telegram (mode --telegram)
qr.go          # pemindai QR TOTP (mode --qr)
probe.go       # diagnostik DOM dashboard (mode --probe)
single.go      # single-instance (lock file, matikan instance lama)
lock_windows.go / lock_unix.go  # implementasi file lock per platform
```
