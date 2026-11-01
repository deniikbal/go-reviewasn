package main

// Mode --telegram: bot Telegram untuk mengelola ReviewASN dari jarak jauh.
//
// Jalankan via: ReviewASN.exe --telegram (token via env TELEGRAM_TOKEN atau --token)
// atau gabung dengan web server: ReviewASN.exe --all
// Sesi pendaftaran per user disimpan di memori (tidak di file).
//   1. /start  -> bot minta NIP
//   2. user ketik NIP            -> disimpan
//   3. bot minta password        -> disimpan
//   4. bot minta QR (kirim foto) -> discan -> secret disimpan
//   5. selesai -> tombol "▶ Jalankan Review" / "📋 Status" / "🔑 Kode MFA"
//
// Semua perintah bot dibatasi hanya untuk user Telegram yang terdaftar
// di variabel lingkungan TG_IZIN (daftar user ID dipisah koma), kalau
// diisi; kalau kosong, siapa pun yang mulai chat boleh (tidak disarankan).
//
// Jalankan: ReviewASN.exe --telegram --token 123:ABC
//   atau set env TELEGRAM_TOKEN.

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	tg "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// ---------- output log ----------

func p(a ...interface{}) {
	fmt.Println(a...)
}

func pf(format string, a ...interface{}) {
	fmt.Printf(format, a...)
}

// ---------- status run ----------

var (
	runMu       sync.Mutex
	sedangJalan bool
	runInfo     string
)

// jalankanReview: seperti jalankan() tapi headless selalu (untuk bot/VPS)
// dan tanpa wizard.
func jalankanReview(daftar []Akun) {
	TAHUN = bacaConfig().Tahun
	JEDA_DASHBOARD = bacaConfig().JedaDashboard

	u := launcher.New().
		Set("incognito").
		Set("disable-popup-blocking").
		Set("no-sandbox"). // penting di VPS (root/container)
		Set("lang", "id-ID").
		Headless(true).
		MustLaunch()

	browser := rod.New().ControlURL(u).MustConnect()
	defer browser.MustClose()
	_ = proto.NetworkClearBrowserCookies{}.Call(browser)

	for i, akun := range daftar {
		pf("\n========== AKUN %d/%d: %s ==========\n", i+1, len(daftar), akun.NIP)
		halaman := browser.MustPages()
		var page *rod.Page
		if len(halaman) > 0 {
			page = halaman[0]
			for _, t := range halaman[1:] {
				_ = t.Close()
			}
			_ = page.Navigate("about:blank")
		} else {
			page = browser.MustPage("about:blank")
		}
		_, _ = page.EvalOnNewDocument(jsTangkapOpen)

		if err := satuAkun(page, browser, akun); err != nil {
			pf("ERROR akun %s: %v\n", akun.NIP, err)
			screenshotDebug(page, fmt.Sprintf("debug_%s.png", akun.NIP))
		}
	}
}

// ---------- sesi pendaftaran per user ----------

type sesiTg struct {
	Step   string // "", "nip", "pass", "qr"
	Nip    string
	Pass   string
}

var (
	sesiMu sync.Mutex
	sesi   = map[int64]*sesiTg{}
)

func sesiOf(id int64) *sesiTg {
	sesiMu.Lock()
	defer sesiMu.Unlock()
	s := sesi[id]
	if s == nil {
		s = &sesiTg{}
		sesi[id] = s
	}
	return s
}

// ---------- status run global (share dengan server.go) ----------

var tgRunMu sync.Mutex

func tgSedangJalan() bool {
	runMu.Lock()
	defer runMu.Unlock()
	return sedangJalan
}

func tgSetRun(v bool, info string) {
	runMu.Lock()
	sedangJalan = v
	runInfo = info
	runMu.Unlock()
}

// ---------- izin ----------

func boleh(id int64) bool {
	izin := os.Getenv("TG_IZIN")
	if izin == "" {
		return true
	}
	for _, s := range strings.Split(izin, ",") {
		if strings.TrimSpace(s) == strconv.FormatInt(id, 10) {
			return true
		}
	}
	return false
}

// ---------- utama ----------

func telegramMain() {
	token := os.Getenv("TELEGRAM_TOKEN")
	for i, a := range os.Args[1:] {
		if a == "--token" && i+1 < len(os.Args)-1 {
			token = os.Args[i+2]
		}
	}
	if token == "" {
		p("Token bot belum ada. Pakai: --token <token> atau env TELEGRAM_TOKEN")
		p("Buat bot via @BotFather di Telegram.")
		return
	}

	bot, err := tg.NewBotAPI(token)
	if err != nil {
		pf("Gagal konek Telegram: %v\n", err)
		return
	}
	bot.Debug = false
	pf("Bot Telegram aktif sebagai @%s\n", bot.Self.UserName)

	// Telegram hanya mengizinkan SATU proses memanggil getUpdates. Kalau masih
	// ada instance lain (mis. jendela ReviewASN lama belum tertutup), Telegram
	// membalas 409 Conflict dan tgbotapi akan spam "Conflict: terminated by
	// other getUpdates request" tiap ~40 detik. Di sini kita kelola sendiri:
	// tunggu sampai token bebas, lalu beri pesan yang jelas.
	if !tungguTelegramBebas(bot) {
		p("PERINGATAN: token bot masih dipakai proses lain.")
		p("            Bot tetap dicoba, tapi tutup dulu ReviewASN lain agar jalan.")
	}

	// GetUpdates manual (bukan GetUpdatesChan) supaya conflict bisa dibedakan
	// dari gangguan jaringan dan log tidak dibanjiri pesan library.
	offset := 0
	for {
		cfg := tg.NewUpdate(offset)
		cfg.Timeout = 60
		cfg.Limit = 100
		ups, err := bot.GetUpdates(cfg)
		if err != nil {
			if galatKonflik(err) {
				p("Telegram: instance lain memakai token bot ini, tunggu 10 detik...")
				time.Sleep(10 * time.Second)
				continue
			}
			pf("Telegram: %v (coba lagi 3 detik)\n", err)
			time.Sleep(3 * time.Second)
			continue
		}
		for _, up := range ups {
			if up.UpdateID >= offset {
				offset = up.UpdateID + 1
			}
			if up.Message != nil {
				if !boleh(up.Message.Chat.ID) {
					continue
				}
				// Foto = QR TOTP (langkah terakhir pendaftaran akun).
				if len(up.Message.Photo) > 0 {
					prosesFoto(bot, up.Message)
					continue
				}
				prosesPesan(bot, up.Message)
			} else if up.CallbackQuery != nil {
				if !boleh(up.CallbackQuery.Message.Chat.ID) {
					continue
				}
				prosesTombol(bot, up.CallbackQuery)
			}
		}
	}
}

// galatKonflik: deteksi balasan 409 Conflict dari Telegram.
func galatKonflik(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "conflict") || strings.Contains(s, "409") ||
		strings.Contains(s, "terminated by other getupdates")
}

// tungguTelegramBebas: panggil getUpdates sekali dengan offset -1 (sekalian
// membuang pesan lama yang menumpuk) sampai token tidak dipakai proses lain.
func tungguTelegramBebas(bot *tg.BotAPI) bool {
	batas := time.Now().Add(30 * time.Second)
	for {
		cfg := tg.NewUpdate(-1)
		cfg.Timeout = 0
		cfg.Limit = 1
		_, err := bot.GetUpdates(cfg)
		if err == nil {
			return true
		}
		if !galatKonflik(err) {
			pf("Telegram: %v\n", err)
			return false
		}
		if time.Now().After(batas) {
			return false
		}
		p("Telegram: token masih dipakai instance lain, menunggu...")
		time.Sleep(3 * time.Second)
	}
}

// ---------- pesan teks ----------

func prosesPesan(bot *tg.BotAPI, m *tg.Message) {
	s := sesiOf(m.Chat.ID)

	// Perintah
	switch m.Command() {
	case "start":
		if tgSedangJalan() {
			balas(bot, m.Chat.ID, "⏳ Review sedang berjalan. Tunggu sampai selesai.")
			return
		}
		s.Step = ""
		// Cek: kalau NIP ini sudah pernah didaftarkan, tidak perlu input ulang.
		nip := strings.TrimSpace(m.CommandArguments())
		cfg := bacaConfig()
		if nip != "" {
			// /start 198711... -> langsung cek NIP spesifik
			for _, a := range cfg.Akun {
				if a.NIP == nip && a.Secret != "" {
					balasTombol(bot, m.Chat.ID,
						fmt.Sprintf("✅ Akun %s sudah terdaftar.\nKode MFA sekarang: %s\n\nTinggal jalankan review — tidak perlu input ulang.",
							nip, kodeTOTP(a.Secret)))
					return
				}
			}
			// NIP belum ada -> mulai pendaftaran dari password
			s.Nip = nip
			s.Step = "pass"
			balas(bot, m.Chat.ID, "NIP baru terdeteksi. Kirim password SMARTASN untuk "+nip+":")
			return
		}
		if len(cfg.Akun) > 0 {
			// Sudah ada akun tersimpan: langsung pakai, tanpa input ulang.
			daftar := ""
			for _, a := range cfg.Akun {
				daftar += "• " + a.NIP + "\n"
			}
			balasTombol(bot, m.Chat.ID,
				"👋 Selamat datang kembali! Akun tersimpan:\n"+daftar+
					"\nTinggal klik ▶ Jalankan Review — tidak perlu input NIP/password/QR lagi.\n"+
					"(Untuk daftar ulang akun, kirim NIP-nya langsung)")
			return
		}
		s.Step = "nip"
		balas(bot, m.Chat.ID,
			"Halo! 👋 Saya bot ReviewASN.\n\n"+
				"Kirim NIP kamu untuk mulai pendaftaran.\n"+
				"(Langkah: NIP → password → foto QR TOTP)")
		return
	case "status":
		if tgSedangJalan() {
			balas(bot, m.Chat.ID, "⏳ Sedang berjalan: "+runInfoStatus())
		} else {
			balas(bot, m.Chat.ID, "💤 Idle. Gunakan tombol Jalankan.")
		}
		return
	case "batal":
		s.Step = ""
		balas(bot, m.Chat.ID, "Pendaftaran dibatalkan.")
		return
	case "hapus":
		nip := strings.TrimSpace(m.CommandArguments())
		cfg := bacaConfig()
		baruDihapus := false
		akunBaru := []Akun{}
		for _, a := range cfg.Akun {
			if nip != "" && a.NIP == nip || (nip == "" && len(cfg.Akun) == 1) {
				baruDihapus = true
				continue
			}
			akunBaru = append(akunBaru, a)
		}
		if baruDihapus {
			cfg.Akun = akunBaru
			simpanConfig(cfg)
			balas(bot, m.Chat.ID, "🗑 Akun "+nip+" dihapus. Kirim NIP baru untuk daftar ulang.")
		} else {
			balas(bot, m.Chat.ID, "NIP tidak ditemukan. Pakai: /hapus <NIP>")
		}
		return
	case "akun":
		cfg := bacaConfig()
		if len(cfg.Akun) == 0 {
			balas(bot, m.Chat.ID, "Belum ada akun tersimpan. Ketik /start.")
			return
		}
		daftar := "📋 Akun tersimpan:\n"
		for _, a := range cfg.Akun {
			daftar += "• " + a.NIP
			if a.Secret != "" {
				daftar += " ✅ (siap)"
			} else {
				daftar += " ⚠️ (secret kosong)"
			}
			daftar += "\n"
		}
		balas(bot, m.Chat.ID, daftar+"\nDaftar ulang: kirim NIP-nya langsung.\nHapus: /hapus <NIP>")
		return
	}

	// Alur pendaftaran
	switch s.Step {
	case "nip":
		nip := strings.TrimSpace(m.Text)
		if len(nip) < 8 {
			balas(bot, m.Chat.ID, "NIP sepertinya salah, coba lagi:")
			return
		}
		// NIP sudah pernah didaftarkan? Langsung pakai, tanpa input ulang.
		cfg := bacaConfig()
		for _, a := range cfg.Akun {
			if a.NIP == nip && a.Secret != "" {
				s.Step = ""
				balasTombol(bot, m.Chat.ID,
					fmt.Sprintf("✅ Akun %s sudah terdaftar.\nKode MFA sekarang: %s\n\nTinggal klik ▶ Jalankan Review.",
						nip, kodeTOTP(a.Secret)))
				return
			}
		}
		s.Nip = nip
		s.Step = "pass"
		balas(bot, m.Chat.ID, "NIP ✓\nSekarang kirim password SMARTASN:")
	case "pass":
		s.Pass = m.Text
		s.Step = "qr"
		balas(bot, m.Chat.ID,
			"Password ✓\nTerakhir: kirim FOTO QR TOTP-nya.\n"+
				"(Dari authenticator: Transfer akun → Ekspor → screenshot)")
	case "":
		// Bukan perintah & tidak sedang mendaftar: kalau teksnya NIP yang sudah
		// terdaftar, langsung pakai; kalau terlihat seperti NIP baru, mulai daftar.
		teks := strings.TrimSpace(m.Text)
		cfg := bacaConfig()
		for _, a := range cfg.Akun {
			if a.NIP == teks && a.Secret != "" {
				balasTombol(bot, m.Chat.ID,
					fmt.Sprintf("✅ Akun %s sudah terdaftar.\nKode MFA sekarang: %s\n\nTinggal klik ▶ Jalankan Review.",
						teks, kodeTOTP(a.Secret)))
				return
			}
		}
		if len(teks) >= 8 && hanyaAngka(teks) {
			s.Step = "nip" // jalankan alur pendaftaran: akan dicek, kalau NIP
			// sudah terdaftar langsung dipakai tanpa password/QR.
			prosesPesan(bot, m)
			return
		}
		balas(bot, m.Chat.ID, "Ketik /start untuk mulai.")
	}
}

// ---------- foto = QR ----------

func prosesFoto(bot *tg.BotAPI, m *tg.Message) {
	s := sesiOf(m.Chat.ID)
	if s.Step != "qr" {
		balas(bot, m.Chat.ID, "Ketik /start dulu untuk mulai pendaftaran.")
		return
	}
	id := m.Photo[len(m.Photo)-1].FileID
	fc, err := bot.GetFile(tg.FileConfig{FileID: id})
	if err != nil {
		balas(bot, m.Chat.ID, "Gagal ambil foto: "+err.Error())
		return
	}
	url := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", bot.Token, fc.FilePath)
	data, err := httpGet(url)
	if err != nil {
		balas(bot, m.Chat.ID, "Gagal unduh foto: "+err.Error())
		return
	}
	q := pindaiQR(bytesToImg(data))
	secret := ""
	if q != "" {
		secret = normalisasiSecret(q)
	}
	if secret == "" {
		balas(bot, m.Chat.ID,
			"❌ QR tidak terbaca. Pastikan QR utuh & cukup besar.\nKirim ulang fotonya, atau /batal.")
		return
	}

	// Simpan akun
	cfg := bacaConfig()
	ada := false
	for i := range cfg.Akun {
		if cfg.Akun[i].NIP == s.Nip {
			cfg.Akun[i].Password = s.Pass
			cfg.Akun[i].Secret = secret
			ada = true
		}
	}
	if !ada {
		cfg.Akun = append(cfg.Akun, Akun{NIP: s.Nip, Password: s.Pass, Secret: secret})
	}
	simpanConfig(cfg)

	s.Step = ""
	kode := kodeTOTP(secret)
	balasTombol(bot, m.Chat.ID,
		fmt.Sprintf("✅ Akun %s tersimpan!\nSecret: %s\nKode MFA sekarang: %s\n\nSiap dijalankan.",
			s.Nip, secret, kode))
}

// ---------- tombol ----------

func prosesTombol(bot *tg.BotAPI, cq *tg.CallbackQuery) {
	switch cq.Data {
	case "run":
		if tgSedangJalan() {
			balas(bot, cq.Message.Chat.ID, "⏳ Sedang berjalan.")
			return
		}
		cfg := bacaConfig()
		if len(cfg.Akun) == 0 {
			balas(bot, cq.Message.Chat.ID, "Belum ada akun. Ketik /start.")
			return
		}
		tgSetRun(true, "Telegram")
		balas(bot, cq.Message.Chat.ID, fmt.Sprintf("▶ Menjalankan review untuk %d akun...", len(cfg.Akun)))
		go func() {
			defer tgSetRun(false, "Selesai.")
			jalankanReview(cfg.Akun) // selalu headless
		}()
	case "mfa":
		cfg := bacaConfig()
		msg := "🔑 Kode MFA:\n"
		for _, a := range cfg.Akun {
			if a.Secret == "" {
				continue
			}
			sisa := 30 - (time.Now().Unix() % 30)
			msg += fmt.Sprintf("\n%s: %s (%ds lagi)", a.NIP, kodeTOTP(a.Secret), sisa)
		}
		balas(bot, cq.Message.Chat.ID, msg)
	case "status":
		if tgSedangJalan() {
			balas(bot, cq.Message.Chat.ID, "⏳ Sedang berjalan: "+runInfoStatus())
		} else {
			balas(bot, cq.Message.Chat.ID, "💤 Idle.")
		}
	}
}

func runInfoStatus() string {
	runMu.Lock()
	defer runMu.Unlock()
	if runInfo == "" {
		return "berjalan"
	}
	return runInfo
}

// ---------- helper ----------

func balas(bot *tg.BotAPI, chat int64, teks string) {
	msg := tg.NewMessage(chat, teks)
	_, _ = bot.Send(msg)
}

func balasTombol(bot *tg.BotAPI, chat int64, teks string) {
	msg := tg.NewMessage(chat, teks)
	msg.ReplyMarkup = tg.NewInlineKeyboardMarkup(
		tg.NewInlineKeyboardRow(
			tg.NewInlineKeyboardButtonData("▶ Jalankan Review", "run"),
		),
		tg.NewInlineKeyboardRow(
			tg.NewInlineKeyboardButtonData("🔑 Kode MFA", "mfa"),
			tg.NewInlineKeyboardButtonData("📋 Status", "status"),
		),
	)
	_, _ = bot.Send(msg)
}

// hanyaAngka: true kalau string terdiri dari digit saja.
func hanyaAngka(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func httpGet(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
