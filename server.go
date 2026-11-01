package main

// Mode --server: web UI satu halaman (di-embed) + API untuk:
//   - kelola akun (CRUD, tersimpan di akun.json seperti biasa)
//   - jalankan review (headless di VPS / headful di PC) untuk akun terpilih
//   - log live via SSE
//   - kode MFA live per akun
//   - upload gambar QR -> scan -> simpan secret
//
// Jalankan: ReviewASN.exe --server [--headless] [--port 8080]

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

//go:embed ui/*
var uiFS embed.FS

// ---------- log capture: semua fmt.Println dari mesin review juga masuk SSE ----------

var (
	logMu   sync.Mutex
	logSSES []chan string
	logBuf  []string // riwayat, untuk klien yang datang terlambat
)

func logLine(s string) {
	logMu.Lock()
	defer logMu.Unlock()
	logBuf = append(logBuf, s)
	if len(logBuf) > 500 { // batasi memori
		logBuf = logBuf[len(logBuf)-500:]
	}
	for _, ch := range logSSES {
		select {
		case ch <- s:
		default: // subscriber lambat: buang, jangan blokir
		}
	}
}

// Hook log: server.go mengisi ini supaya output juga masuk SSE.
var logHook func(msg string)

func p(a ...interface{}) {
	msg := fmt.Sprint(a...) + "\n"
	fmt.Print(msg)
	if logHook != nil {
		logHook(msg)
	}
}

func pf(format string, a ...interface{}) {
	msg := fmt.Sprintf(format, a...)
	fmt.Print(msg)
	if logHook != nil {
		logHook(msg)
	}
}

// ---------- status run ----------

var (
	runMu     sync.Mutex
	sedangJalan bool
	runInfo   string // NIP yang sedang jalan / info lain
)

func apiStatus(w http.ResponseWriter, r *http.Request) {
	runMu.Lock()
	defer runMu.Unlock()
	jawabJSON(w, map[string]interface{}{"jalan": sedangJalan, "info": runInfo})
}

// ---------- helpers ----------

func jawabJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func bacaBody(r *http.Request, v interface{}) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

// ---------- API: akun ----------

func apiAkun(w http.ResponseWriter, r *http.Request) {
	cfg := bacaConfig()
	switch r.Method {
	case http.MethodGet:
		// secret tidak diperas ke UI, cukup penanda ada/tidak
		type akunAman struct {
			NIP       string `json:"nip"`
			AdaSecret bool   `json:"adaSecret"`
		}
		list := []akunAman{}
		for _, a := range cfg.Akun {
			list = append(list, akunAman{NIP: a.NIP, AdaSecret: a.Secret != ""})
		}
		jawabJSON(w, map[string]interface{}{
			"akun": list, "tahun": cfg.Tahun, "jeda": cfg.JedaDashboard,
		})

	case http.MethodPost: // tambah / update
		var b struct {
			NIP      string `json:"nip"`
			Password string `json:"password"`
			Secret   string `json:"secret"`
		}
		if err := bacaBody(r, &b); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		b.NIP = strings.TrimSpace(b.NIP)
		if b.NIP == "" {
			http.Error(w, "NIP wajib", 400)
			return
		}
		secret := normalisasiSecret(b.Secret)
		ada := false
		for i := range cfg.Akun {
			if cfg.Akun[i].NIP == b.NIP {
				if b.Password != "" {
					cfg.Akun[i].Password = b.Password
				}
				if secret != "" {
					cfg.Akun[i].Secret = secret
				}
				ada = true
			}
		}
		if !ada {
			cfg.Akun = append(cfg.Akun, Akun{NIP: b.NIP, Password: b.Password, Secret: secret})
		}
		simpanConfig(cfg)
		pf("  Akun %s disimpan.\n", b.NIP)
		jawabJSON(w, map[string]interface{}{"ok": true})

	case http.MethodDelete:
		nip := strings.TrimSpace(r.URL.Query().Get("nip"))
		for i := range cfg.Akun {
			if cfg.Akun[i].NIP == nip {
				cfg.Akun = append(cfg.Akun[:i], cfg.Akun[i+1:]...)
				simpanConfig(cfg)
				pf("  Akun %s dihapus.\n", nip)
				jawabJSON(w, map[string]interface{}{"ok": true})
				return
			}
		}
		http.Error(w, "akun tidak ada", 404)
	}
}

// ---------- API: jalankan review ----------

func apiRun(w http.ResponseWriter, r *http.Request) {
	runMu.Lock()
	if sedangJalan {
		runMu.Unlock()
		http.Error(w, "review sedang berjalan", 409)
		return
	}
	var b struct {
		Nips []string `json:"nips"`
	}
	if err := bacaBody(r, &b); err != nil {
		runMu.Unlock()
		http.Error(w, err.Error(), 400)
		return
	}
	cfg := bacaConfig()
	pilih := []Akun{}
	for _, nip := range b.Nips {
		for _, a := range cfg.Akun {
			if a.NIP == strings.TrimSpace(nip) {
				pilih = append(pilih, a)
			}
		}
	}
	if len(pilih) == 0 {
		runMu.Unlock()
		http.Error(w, "tidak ada akun yang cocok", 400)
		return
	}
	sedangJalan = true
	runInfo = fmt.Sprintf("Menjalankan %d akun...", len(pilih))
	runMu.Unlock()

	go func() {
		defer func() {
			runMu.Lock()
			sedangJalan = false
			runInfo = "Selesai."
			runMu.Unlock()
		}()
		jalankanServer(pilih, os.Getenv("REVIEW_HEADLESS") == "1" || terdapatFlag("--headless"))
	}()

	jawabJSON(w, map[string]interface{}{"ok": true})
}

func terdapatFlag(f string) bool {
	for _, a := range os.Args[1:] {
		if a == f {
			return true
		}
	}
	return false
}

// jalankanServer: seperti jalankan() tapi headless opsional & hasil per akun
// dilaporkan ke log (yang masuk SSE).
func jalankanServer(daftar []Akun, headless bool) {
	TAHUN = bacaConfig().Tahun
	JEDA_DASHBOARD = bacaConfig().JedaDashboard

	u := launcher.New().
		Set("incognito").
		Set("disable-popup-blocking").
		Set("no-sandbox"). // penting di VPS (root/container)
		Set("lang", "id-ID").
		Headless(headless).
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

// ---------- API: MFA live ----------

func apiMFA(w http.ResponseWriter, r *http.Request) {
	cfg := bacaConfig()
	nip := r.URL.Query().Get("nip")
	out := []map[string]string{}
	for _, a := range cfg.Akun {
		if nip != "" && a.NIP != nip {
			continue
		}
		if a.Secret == "" {
			continue
		}
		sisa := int(30 - (time.Now().Unix() % 30))
		out = append(out, map[string]string{
			"nip":  a.NIP,
			"kode": kodeTOTP(a.Secret),
			"sisa": strconv.Itoa(sisa),
		})
	}
	jawabJSON(w, map[string]interface{}{"mfa": out})
}

// ---------- API: upload QR ----------

func apiQR(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		http.Error(w, "file terlalu besar", 400)
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "file tidak ada", 400)
		return
	}
	defer f.Close()

	// simpan sementara lalu scan
	tmp := filepath.Join(BASE_DIR, fmt.Sprintf("_qr_%d.png", time.Now().UnixNano()))
	data, _ := io.ReadAll(f)
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer os.Remove(tmp)

	img := bytesToImg(data)
	q := pindaiQR(img)
	if q == "" {
		jawabJSON(w, map[string]interface{}{"ok": false, "pesan": "QR otpauth tidak ditemukan di gambar."})
		return
	}
	secret := normalisasiSecret(q)
	if secret == "" {
		jawabJSON(w, map[string]interface{}{"ok": false, "pesan": "Secret tidak bisa dibaca dari QR."})
		return
	}

	nip := strings.TrimSpace(r.FormValue("nip"))
	cfg := bacaConfig()
	if nip == "" && len(cfg.Akun) == 1 {
		nip = cfg.Akun[0].NIP
	}
	if nip == "" {
		jawabJSON(w, map[string]interface{}{"ok": true, "secret": secret, "kode": kodeTOTP(secret), "pesan": "secret dibaca; kirim nip untuk menyimpan"})
		return
	}
	ditemukan := false
	for i := range cfg.Akun {
		if cfg.Akun[i].NIP == nip {
			cfg.Akun[i].Secret = secret
			ditemukan = true
		}
	}
	if !ditemukan {
		cfg.Akun = append(cfg.Akun, Akun{NIP: nip, Secret: secret})
	}
	simpanConfig(cfg)
	jawabJSON(w, map[string]interface{}{"ok": true, "secret": secret, "kode": kodeTOTP(secret), "nip": nip})
}

// ---------- SSE: log live ----------

func apiLog(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE tidak didukung", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	ch := make(chan string, 256)
	logMu.Lock()
	logSSES = append(logSSES, ch)
	// kirim riwayat dulu supaya UI yang baru dibuka tetap melihat log lama
	riwayat := append([]string(nil), logBuf...)
	logMu.Unlock()
	for _, msg := range riwayat {
		fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(msg, "\n", "\u2028"))
	}
	fl.Flush()
	defer func() {
		logMu.Lock()
		for i, c := range logSSES {
			if c == ch {
				logSSES = append(logSSES[:i], logSSES[i+1:]...)
				break
			}
		}
		logMu.Unlock()
	}()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(msg, "\n", "\u2028"))
			fl.Flush()
		}
	}
}

// ---------- server utama ----------

func serverMain() {
	port := "8080"
	for i, a := range os.Args[1:] {
		if a == "--port" && i+1 < len(os.Args)-1 {
			port = os.Args[i+2]
		}
	}

	logHook = logLine // output p()/pf() juga masuk SSE

	http.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		b, err := uiFS.ReadFile("ui/index.html")
		if err != nil {
			http.Error(w, "UI tidak ditemukan", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
	}))
	http.HandleFunc("/api/status", apiStatus)
	http.HandleFunc("/api/akun", apiAkun)
	http.HandleFunc("/api/run", apiRun)
	http.HandleFunc("/api/mfa", apiMFA)
	http.HandleFunc("/api/qr", apiQR)
	http.HandleFunc("/api/log", apiLog)

	pf("UI web berjalan di http://localhost:%s\n", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		pf("Server gagal: %v\n", err)
	}
}
