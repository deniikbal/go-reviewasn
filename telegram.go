package main

// Mode --telegram: bot Telegram untuk mengelola ReviewASN dari jarak jauh.
//
// Alur user (per user Telegram, tersimpan di telegram.json):
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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	tg "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

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

	u := tg.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	for up := range updates {
		if up.Message != nil {
			if !boleh(up.Message.Chat.ID) {
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
	}

	// Alur pendaftaran
	switch s.Step {
	case "nip":
		nip := strings.TrimSpace(m.Text)
		if len(nip) < 8 {
			balas(bot, m.Chat.ID, "NIP sepertinya salah, coba lagi:")
			return
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
			jalankanServer(cfg.Akun, true) // selalu headless
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

func httpGet(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

var _ = filepath.Join // jaga import
