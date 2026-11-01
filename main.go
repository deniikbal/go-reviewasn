// Package main: ReviewASN versi Go + Rod.
//
// Porting dari review_asn.py (Python + Playwright). Logika alur sama:
//   login SMARTASN (NIP + password + MFA/TOTP) -> buka Kinerja (pop-up
//   window.open dengan banyak lapis fallback) -> ambil kartu review
//   "Belum Direview" -> klik semua "Selalu" + Simpan.
package main

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// =============================================
//   KONFIGURASI (akun.json) + WIZARD
// =============================================

var (
	BASE_DIR       string
	CONFIG_PATH    string
	TAHUN          = "2026"
	JEDA_DASHBOARD = 2.0
	USERNAME, PASSWORD, TOTP_SECRET string
)

const (
	BASE_URL   = "https://kinerja.jabarprov.go.id"
	URL_REVIEW = BASE_URL + "/review-perilaku"
	URL_LOGIN  = "https://smartasn.jabarprov.go.id/login"
)

type Akun struct {
	NIP      string `json:"nip"`
	Password string `json:"password"`
	Secret   string `json:"secret"`
}

type Config struct {
	Tahun          string `json:"tahun"`
	JedaDashboard  float64 `json:"jeda_dashboard"`
	Akun           []Akun `json:"akun"`
}

func init() {
	exe, err := os.Executable()
	if err != nil {
		exe = "."
	}
	BASE_DIR = filepath.Dir(exe)
	CONFIG_PATH = filepath.Join(BASE_DIR, "akun.json")
	muatEnv()
	// Buat konsol tetap tampil saat double-click exe di Windows (rod/leakless
	// sudah menangani ini; exe Go biasa tidak detach console).
}

// muatEnv: baca file .env di samping exe (format KEY=VALUE per baris,
// baris kosong & awalan # diabaikan) lalu set sebagai environment variable.
// Variabel yang SUDAH ada di environment tidak ditimpa (env sistem menang).
// Dipakai untuk: TELEGRAM_TOKEN, TG_IZIN, REVIEW_HEADLESS.
func muatEnv() {
	data, err := os.ReadFile(filepath.Join(BASE_DIR, ".env"))
	if err != nil {
		return // .env tidak ada, tidak masalah
	}
	for _, baris := range strings.Split(string(data), "\n") {
		baris = strings.TrimSpace(baris)
		if baris == "" || strings.HasPrefix(baris, "#") {
			continue
		}
		k, v, ok := strings.Cut(baris, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		// buang tanda kutip pembungkus opsional
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if _, sudah := os.LookupEnv(k); !sudah && k != "" {
			_ = os.Setenv(k, v)
		}
	}
}

func bacaConfig() Config {
	cfg := Config{Tahun: "2026", JedaDashboard: 2}
	data, err := os.ReadFile(CONFIG_PATH)
	if err == nil {
		if err := json.Unmarshal(data, &cfg); err == nil {
			if cfg.Tahun == "" {
				cfg.Tahun = "2026"
			}
			if cfg.JedaDashboard == 0 {
				cfg.JedaDashboard = 2
			}
			return cfg
		}
		pf("PERINGATAN: akun.json tidak terbaca (%v). Mulai dari kosong.\n", err)
	}
	return cfg
}

func simpanConfig(cfg Config) {
	data, _ := json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile(CONFIG_PATH, data, 0600)
}

var reader = bufio.NewReader(os.Stdin)

func tanya(teks string) string {
	for {
		fmt.Print(teks)
		line, err := reader.ReadString('\n')
		if err != nil {
			p("\nDibatalkan.")
			os.Exit(1)
		}
		nilai := strings.TrimSpace(line)
		if nilai != "" {
			return nilai
		}
		p("  Tidak boleh kosong.")
	}
}

// tanyaRahasia: Go stdlib tidak punya getpass lintas-platform sederhana.
// Di Windows pakai `powershell Read-Host -AsSecureString` agar tidak ter-echo;
// di Unix pakai `stty -echo`. Jika gagal, fallback ke input biasa.
func tanyaRahasia(teks string) string {
	fmt.Print(teks)
	var line string
	switch runtime.GOOS {
	case "windows":
		cmd := exec.Command("powershell", "-NoProfile", "-Command",
			"$s = Read-Host -AsSecureString; [Runtime.InteropServices.Marshal]::SecureStringToBSS($s) | ForEach-Object { [Runtime.InteropServices.Marshal]::PtrToStringBSTR($_) }")
		cmd.Stdin = os.Stdin
		out, err := cmd.Output()
		if err == nil {
			line = strings.TrimRight(string(out), "\r\n")
			p()
			break
		}
		// fallback: baca dari stdin normal
		l, _ := reader.ReadString('\n')
		line = strings.TrimSpace(l)
	default:
		_ = exec.Command("stty", "-echo").Run() // mungkin gagal (bukan tty), abaikan
		l, _ := reader.ReadString('\n')
		line = strings.TrimSpace(l)
		_ = exec.Command("stty", "echo").Run()
		p()
	}
	return strings.TrimSpace(line)
}

// =============================================
//   SECRET MFA: normalisasi + TOTP (tanpa lib)
// =============================================

// normalisasiSecret: terima secret mentah base32, otpauth://totp/...,
// atau otpauth-migration://offline?data=... (ekspor Google Authenticator).
func normalisasiSecret(teks string) string {
	t := strings.TrimSpace(teks)
	if t == "" {
		return ""
	}
	low := strings.ToLower(t)

	if strings.HasPrefix(low, "otpauth-migration://") {
		u, err := url.Parse(t)
		if err != nil {
			return ""
		}
		raw, err := base64.StdEncoding.DecodeString(u.Query().Get("data"))
		if err != nil {
			return ""
		}
		outer := parseProto(raw)
		innerList, ok := outer[1]
		if !ok || len(innerList) == 0 {
			return ""
		}
		inner := parseProto(innerList[0])
		keys, ok := inner[1]
		if !ok || len(keys) == 0 {
			return ""
		}
		return strings.TrimRight(base32.StdEncoding.EncodeToString(keys[0]), "=")
	}

	if strings.HasPrefix(low, "otpauth://") {
		u, err := url.Parse(t)
		if err != nil {
			return ""
		}
		t = u.Query().Get("secret")
	}

	t = strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(t))
	t = strings.TrimRight(t, "=")
	if _, err := base32.StdEncoding.DecodeString(t + strings.Repeat("=", (8-len(t)%8)%8)); err != nil {
		return ""
	}
	return t
}

// ---- protobuf parsing minimal (varint + length-delimited), sama seperti di Python

func parseVarint(data []byte, pos int) (uint64, int) {
	var hasil uint64
	var shift uint
	for {
		b := data[pos]
		pos++
		hasil |= uint64(b&0x7F) << shift
		if b&0x80 == 0 {
			break
		}
		shift += 7
	}
	return hasil, pos
}

// parseProto: field number -> daftar nilai (varint sbg uint64, string/bytes sbg []byte)
func parseProto(data []byte) map[int][][]byte {
	fields := map[int][][]byte{}
	pos := 0
	for pos < len(data) {
		tag, p2 := parseVarint(data, pos)
		pos = p2
		field, wtype := int(tag>>3), int(tag&7)
		if wtype == 0 {
			val, p3 := parseVarint(data, pos)
			pos = p3
			buf := make([]byte, 8)
			binary.LittleEndian.PutUint64(buf, val)
			fields[field] = append(fields[field], buf)
		} else if wtype == 2 {
			ln, p3 := parseVarint(data, pos)
			pos = p3
			if pos+int(ln) > len(data) {
				break
			}
			fields[field] = append(fields[field], data[pos:pos+int(ln)])
			pos += int(ln)
		} else {
			break
		}
	}
	return fields
}

// kodeTOTP: RFC 6238, SHA-1, 6 digit, 30 detik. Setara pyotp.TOTP(secret).now().
func kodeTOTP(secret string) string {
	key, err := base32.StdEncoding.DecodeString(secret + strings.Repeat("=", (8-len(secret)%8)%8))
	if err != nil {
		return ""
	}
	counter := uint64(time.Now().Unix() / 30)
	msg := make([]byte, 8)
	binary.BigEndian.PutUint64(msg, counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg)
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0F
	code := (binary.BigEndian.Uint32(sum[off:off+4]) & 0x7FFFFFFF) % 1000000
	return fmt.Sprintf("%06d", code)
}

// sisaDetik: berapa detik lagi window TOTP 30s berakhir.
func sisaDetik() int {
	return 30 - int(time.Now().Unix()%30)
}

// =============================================
//   HELPER BROWSER (padanan helper Playwright)
// =============================================

// isi: bersihkan input lalu ketik per karakter (rod Input meniru pengetikan
// sungguhan, setara press_sequentially di Playwright) supaya framework
// reactive (Vue) di situs mendeteksi perubahan.
func isi(page *rod.Page, selector, nilai string, timeout time.Duration) error {
	el, err := page.Timeout(timeout).Element(selector)
	if err != nil {
		return err
	}
	// Klik dulu supaya fokus, lalu select-all. Pengetikan berikutnya otomatis
	// MENGANTIKAN teks ter-seleksi (perilaku standar input), jadi tidak perlu
	// backspace manual. JANGAN kirim karakter "\b" - Rod mengetikkannya sebagai
	// teks literal, bukan tombol backspace (bug yang pernah terjadi).
	_ = el.Click(proto.InputMouseButtonLeft, 1)
	_ = el.SelectAllText()
	return el.Input(nilai)
}

// aktif: klik elemen yang terlihat & enabled. Setara _aktif().
func aktif(page *rod.Page, selector string, timeout time.Duration) error {
	el, err := page.Timeout(timeout).Element(selector)
	if err != nil {
		return err
	}
	if visible, _ := el.Visible(); !visible {
		return fmt.Errorf("elemen tidak terlihat: %s", selector)
	}
	return el.Click(proto.InputMouseButtonLeft, 1)
}

// submit: klik tombol, fallback requestSubmit() via JS kalau gagal.
func submit(page *rod.Page, timeout time.Duration) error {
	err := aktif(page, "button[type='submit']", timeout)
	if err == nil {
		return nil
	}
	// Fallback: form requestSubmit() = jalankan validasi HTML5, sama seperti klik.
	_, e2 := page.Timeout(timeout).Evaluate(rod.Eval(`
		() => { const f = document.querySelector('form'); if (f) f.requestSubmit(); }
	`))
	if e2 != nil {
		return fmt.Errorf("submit gagal: %v lalu %v", err, e2)
	}
	return nil
}

// screenshotDebug simpan debug.png di BASE_DIR.
func screenshotDebug(page *rod.Page, nama string) {
	if page == nil {
		return
	}
	// page bisa saja sudah "mati" (target di-swap Chrome saat navigasi
	// lintas-origin). Jangan sampai proses utama ikut panic.
	defer func() { _ = recover() }()
	opt := &proto.PageCaptureScreenshot{}
	img, err := page.Screenshot(false, opt)
	if err == nil {
		path := filepath.Join(BASE_DIR, nama)
		if werr := os.WriteFile(path, img, 0644); werr == nil {
			pf("      Screenshot disimpan: %s\n", nama)
		}
	}
}

// urlHalaman: MustInfo() melempar panic kalau target halaman sudah tidak ada
// (Chrome mengganti target saat navigasi lintas-origin, mis. SSO -> dashboard).
// Semua pemakaian URL halaman di aplikasi ini harus lewat sini.
func urlHalaman(p *rod.Page) (u string) {
	if p == nil {
		return ""
	}
	defer func() {
		if r := recover(); r != nil {
			u = ""
		}
	}()
	return p.MustInfo().URL
}

// halamanHidup: kembalikan handle tab yang masih valid. Dipakai setelah login
// karena handle lama bisa mati walau browsernya sehat.
func halamanHidup(browser *rod.Browser, page *rod.Page) *rod.Page {
	if urlHalaman(page) != "" {
		return page
	}
	ps := browser.MustPages()
	for i := len(ps) - 1; i >= 0; i-- {
		if urlHalaman(ps[i]) != "" && urlHalaman(ps[i]) != "about:blank" {
			return ps[i]
		}
	}
	if len(ps) > 0 {
		return ps[len(ps)-1]
	}
	return browser.MustPage("about:blank")
}

// =============================================
//   LOGIN SMARTASN
// =============================================

func login(page *rod.Page) error {
	pf("[1/5] Buka %s\n", URL_LOGIN)
	if err := page.Navigate(URL_LOGIN); err != nil {
		return err
	}
	stabil(page, 500*time.Millisecond)

	p("      Isi form login...")
	if err := isi(page, "input[name='nip'], input[type='text']", USERNAME, 30*time.Second); err != nil {
		return fmt.Errorf("input NIP: %w", err)
	}
	if err := isi(page, "input[type='password']", PASSWORD, 30*time.Second); err != nil {
		return fmt.Errorf("input password: %w", err)
	}
	if err := submit(page, 10*time.Second); err != nil {
		return err
	}

	// Bisa jadi langsung sukses (tanpa MFA) atau masuk halaman MFA.
	p("[2/5] Masukkan kode MFA...")
	for i := 0; i < 15; i++ {
		if strings.Contains(urlHalaman(page), "kinerja") {
			p("      Tidak butuh MFA, langsung masuk.")
			return nil
		}
		otpInput, err := page.Timeout(2 * time.Second).Element("input[inputmode='numeric'], input[autocomplete='one-time-code']")
		if err == nil && otpInput != nil {
			break
		}
		stabil(page, 500*time.Millisecond)
	}

	otpInput, err := page.Timeout(15 * time.Second).Element("input[inputmode='numeric'], input[autocomplete='one-time-code'], input[maxlength='6']")
	if err != nil {
		return fmt.Errorf("halaman MFA tidak muncul: %w", err)
	}

	// Jeda cerdas: tunggu window TOTP baru supaya OTP tidak basi saat submit
	// (dipakai 2 detik, butuh sisa >= 8 detik).
	if sisa := sisaDetik(); sisa < 8 {
		pf("      Tunggu %ds supaya kode MFA tidak basi (window 30 detik)...\n", sisa+1)
		time.Sleep(time.Duration(sisa+1) * time.Second)
	}
	otp := kodeTOTP(TOTP_SECRET)
	pf("      Kode MFA: %s\n", otp)

	_ = otpInput.Click(proto.InputMouseButtonLeft, 1)
	_ = otpInput.SelectAllText() // field MFA kosong, tapi select-all tetap aman
	_ = otpInput.Input(otp)      // ketik per karakter, event Vue kepicik
	if err := submit(page, 10*time.Second); err != nil {
		return err
	}

	stabil(page, 2*time.Second)
	return nil
}

// =============================================
//   BUKA KINERJA (pop-up window.open + fallback)
// =============================================

// Tangkap URL apa pun yang dikirim ke window.open, sebagai jaring pengaman
// kalau pop-up diblokir: URL-nya bisa kita buka sendiri.
const jsTangkapOpen = `
() => {
	if (window.__asnTangkapOpen) return;
	window.__asnTangkapOpen = true;
	const asli = window.open;
	window.open = function(u) {
		if (u) { try { window.__asnUrlOpen = String(u); } catch(e) {} }
		return asli.apply(this, arguments);
	};
}
`

func diKinerja(u string) bool {
	return strings.Contains(u, "kinerja.jabarprov.go.id") &&
		!strings.Contains(u, "smartasn")
}

func cariTabKinerja(browser *rod.Browser, timeout time.Duration) *rod.Page {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, p := range browser.MustPages() {
			if diKinerja(urlHalaman(p)) {
				return p
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return nil
}

func klikPertamaTerlihat(page *rod.Page, selector string, timeout time.Duration) bool {
	el, err := page.Timeout(timeout).Element(selector)
	if err != nil {
		return false
	}
	if vis, _ := el.Visible(); !vis {
		return false
	}
	return el.Click(proto.InputMouseButtonLeft, 1) == nil
}

func klikBukaAplikasi(page *rod.Page, timeout time.Duration) bool {
	teks := []string{"Buka Aplikasi Sekarang", "Buka Aplikasi"}
	for _, t := range teks {
		els, err := page.Timeout(timeout).Elements("button, a")
		if err != nil {
			continue
		}
		for _, el := range els {
			txt, _ := el.Text()
			if strings.Contains(strings.TrimSpace(txt), t) {
				if vis, _ := el.Visible(); vis {
					// Coba klik mouse biasa dulu.
					if el.Click(proto.InputMouseButtonLeft, 1) == nil {
						return true
					}
					// Fallback: klik via JS (window.open bisa terblokir,
					// tapi URL tetap tertangkap oleh jsTangkapOpen).
					if res, err := el.Evaluate(rod.Eval(`() => this.click()`)); err == nil {
						_ = res
						return true
					}
				}
			}
		}
	}
	return false
}

func urlOpenTertangkap(page *rod.Page) string {
	val, err := page.Evaluate(rod.Eval(`() => window.__asnUrlOpen || ""`))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprintf("%v", val.Value))
}

func layakDibuka(u string, page *rod.Page) bool {
	return u != "" && (strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")) &&
		strings.Contains(u, "kinerja") && !diKinerja(urlHalaman(page))
}

func bukaURLDiTab(browser *rod.Browser, rawurl string) {
	_, err := url.Parse(rawurl)
	if err != nil {
		return
	}
	tab, err := browser.Page(proto.TargetCreateTarget{URL: rawurl})
	if err != nil {
		return
	}
	stabil(tab, 2*time.Second)
}

// jsKlikKartuKinerja: klik KARTU layanan "Kinerja" di dashboard SMARTASN.
//
// Dashboard punya dua elemen berteks "Kinerja":
//  1. <button>Kinerja</button> di navbar -> hanya membuka dropdown, tidak
//     pernah memunculkan modal tahun dan tidak pernah membuka aplikasi.
//  2. kartu di "Layanan Unggulan" dengan teks gabungan
//     "Kinerja Kinerja Pegawai" -> inilah pintu masuk aplikasi Kinerja.
//
// Versi lama mencari teks PERSIS "Kinerja" sehingga selalu kena tombol navbar.
// Sekarang: cari elemen terkecil (paling dalam) yang memuat "Kinerja Pegawai",
// abaikan header/nav, lalu klik.
const jsKlikKartuKinerja = `
(target) => {
	const norm = s => (s || "").replace(/\s+/g, " ").trim();
	const kandidat = [...document.querySelectorAll("button,a,[role='button'],div")].filter(e => {
		const t = norm(e.innerText);
		if (!t) return false;
		const r = e.getBoundingClientRect();
		if (r.width === 0 || r.height === 0) return false;
		if (e.closest("header,nav")) return false;
		if (target === "Kinerja Pegawai") return t.includes("Kinerja Pegawai");
		return t.startsWith("Kinerja") && t.length <= 40;
	});
	if (!kandidat.length) return null;
	kandidat.sort((a, b) => {
		const ra = a.getBoundingClientRect(), rb = b.getBoundingClientRect();
		return ra.width * ra.height - rb.width * rb.height;
	});
	const el = kandidat[0];
	el.scrollIntoView({ block: "center" });
	el.click();
	return norm(el.innerText).slice(0, 60);
}
`

func klikCardKinerja(page *rod.Page) bool {
	// Dashboard Vue butuh beberapa detik untuk render; kalau diklik terlalu
	// cepat, elemennya belum ada dan klik gagal tanpa error.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, target := range []string{"Kinerja Pegawai", "Kinerja"} {
			res, err := page.Evaluate(rod.Eval(jsKlikKartuKinerja, target))
			if err == nil && res.Value.Val() != nil {
				pf("      Kartu diklik: %q\n", res.Value.Str())
				return true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// pilihTahun: modal "Pilih Tahun Kinerja" -> pilih tahun -> klik
// "Lanjutkan ke Kinerja". Tombol ini yang membuka tab autologin Kinerja.
//
// Dua jebakan yang diperbaiki:
//   - Radio-nya berlabel "Tahun 2026" (bukan "2026"), jadi pencarian teks
//     persis "2026" tidak pernah kena dan tahun tidak pernah terpilih.
//   - Modal harus ditunggu dulu; versi lama langsung memindai halaman.
func pilihTahun(page *rod.Page) bool {
	if _, err := page.Timeout(12*time.Second).Element("input[name='tahun']"); err != nil {
		return false
	}
	stabil(page, 500*time.Millisecond)

	res, err := page.Evaluate(rod.Eval(`(tahun) => {
		const r = document.querySelector("input[name='tahun'][value='" + tahun + "']");
		if (!r) return false;
		if (!r.checked) r.click();
		return r.checked;
	}`, TAHUN))
	if err != nil || !res.Value.Bool() {
		pf("      Tahun %s tidak ada di modal, pakai pilihan default.\n", TAHUN)
	}
	time.Sleep(300 * time.Millisecond)

	res, err = page.Evaluate(rod.Eval(`() => {
		const btn = [...document.querySelectorAll("button")].find(b => {
			const t = (b.innerText || "").trim();
			const r = b.getBoundingClientRect();
			return r.width > 0 && r.height > 0 && t.includes("Lanjutkan");
		});
		if (!btn) return false;
		btn.click();
		return true;
	}`))
	if err != nil {
		return false
	}
	return res.Value.Bool()
}

func bukaKinerja(page *rod.Page, browser *rod.Browser) (*rod.Page, error) {
	tMulai := time.Now()
	p("[3/5] Masuk dashboard SSO, buka Kinerja...")

	// Handle halaman bisa mati setelah login (Chrome mengganti target saat
	// navigasi lintas-origin SSO), jadi ambil ulang tab yang masih hidup.
	page = halamanHidup(browser, page)

	// ALUR BARU: setelah login mendarat di
	// https://smartasn.jabarprov.go.id/framer/kinerja?tahun=2026 yang
	// berisi tombol "Buka Aplikasi Sekarang". Tombol itu membuka TAB BARU
	// ke https://kinerja.jabarprov.go.id/kinerjajabar/dashboard.
	if strings.Contains(urlHalaman(page), "framer/kinerja") {
		p("      Halaman framer terdeteksi, klik 'Buka Aplikasi Sekarang'...")
		if klikBukaAplikasi(page, 20*time.Second) {
			tab := cariTabKinerja(browser, 30*time.Second)
			if tab != nil {
				stabil(tab, 3*time.Second)
				pf("[4/5] Masuk ke aplikasi Kinerja (%.1fs): %s\n", time.Since(tMulai).Seconds(), urlHalaman(tab))
				return tab, nil
			}
			// Klik berhasil tapi tab baru tidak terdeteksi: coba fallback di bawah.
		}
		// Fallback: URL tertangkap dari window.open -> buka sendiri
		if u := urlOpenTertangkap(page); layakDibuka(u, page) {
			pf("      Buka URL hasil window.open: %.80s\n", u)
			bukaURLDiTab(browser, u)
			if tab := cariTabKinerja(browser, 25*time.Second); tab != nil {
				stabil(tab, 3*time.Second)
				pf("[4/5] Masuk ke aplikasi Kinerja (%.1fs): %s\n", time.Since(tMulai).Seconds(), urlHalaman(tab))
				return tab, nil
			}
		}
		// Fallback terakhir: buka langsung URL dashboard Kinerja
		p("      Tab tidak muncul, buka langsung URL Kinerja...")
		bukaURLDiTab(browser, BASE_URL+"/kinerjajabar/dashboard")
		if tab := cariTabKinerja(browser, 25*time.Second); tab != nil {
			stabil(tab, 3*time.Second)
			pf("[4/5] Masuk ke aplikasi Kinerja (%.1fs): %s\n", time.Since(tMulai).Seconds(), urlHalaman(tab))
			return tab, nil
		}
		screenshotDebug(page, "debug.png")
		return nil, fmt.Errorf("halaman framer ada tapi tab Kinerja tidak muncul")
	}

	// ALUR LAMA (fallback): dashboard SSO dengan card Kinerja.
	if !klikCardKinerja(page) {
		// Fallback: langsung menuju URL kinerja (alur lama)
		if err := page.Navigate(BASE_URL); err != nil {
			return nil, err
		}
		stabil(page, 2*time.Second)
	}

	time.Sleep(time.Duration(JEDA_DASHBOARD) * time.Second)
	pilihTahun(page)

	// Tunggu tab Kinerja muncul (window.open dari tombol "Lanjutkan").
	tab := cariTabKinerja(browser, 20*time.Second)

	if tab == nil {
		tab = cariTabKinerja(browser, 2*time.Second)
	}

	// Lapis 1: klik "Buka Aplikasi Sekarang" (gesture baru, pop-up dapat izin)
	if tab == nil {
		p("      Klik 'Buka Aplikasi Sekarang'...")
		if klikBukaAplikasi(page, 8*time.Second) {
			tab = cariTabKinerja(browser, 15*time.Second)
		}
	}

	// Lapis 2: URL tertangkap dari window.open -> buka sendiri
	if tab == nil {
		u := urlOpenTertangkap(page)
		if layakDibuka(u, page) {
			pf("      Pop-up tetap gagal; buka URL hasil window.open: %.80s\n", u)
			bukaURLDiTab(browser, u)
			tab = cariTabKinerja(browser, 25*time.Second)
		}
	}

	if tab != nil {
		stabil(tab, 2*time.Second)
		pf("[4/5] Masuk ke aplikasi Kinerja (%.1fs): %s\n", time.Since(tMulai).Seconds(), urlHalaman(tab))
		return tab, nil
	}

	screenshotDebug(page, "debug.png")
	urls := []string{}
	for _, p := range browser.MustPages() {
		urls = append(urls, urlHalaman(p))
	}
	return nil, fmt.Errorf("tidak sampai ke aplikasi Kinerja. Tab yang ada: %v", urls)
}

// =============================================
//   AMBIL & ISI REVIEW
// =============================================

// jsKartu: ambil link review yang benar-benar berstatus "Belum Direview".
// Sama persis dengan _JS_KARTU di Python: naik dari tiap link ke kartu
// terdekat yang menyebut statusnya supaya link yang sudah direview tidak ikut.
const jsKartu = `
() => {
	const hasil = { total: 0, belum: [] };
	document.querySelectorAll("a[href*='review-perilaku/']").forEach(a => {
		hasil.total++;
		let el = a, teks = "";
		while (el && el !== document.body) {
			const t = (el.innerText || "").trim();
			if (t.includes("Belum Direview") || t.includes("Sudah Direview")) {
				teks = t;
				break;
			}
			el = el.parentElement;
		}
		if (teks.includes("Belum Direview") && !teks.includes("Sudah Direview")) {
			hasil.belum.push(a.getAttribute("href"));
		}
	});
	return hasil;
}
`


// stabil: menunggu DOM stabil MAKSIMAL batasTotal. SPA Kinerja terus
// polling sehingga WaitStable polos bisa menggantung selamanya; di sini
// kita pakai page.Timeout agar melewati batas = lanjut, bukan error.
func stabil(p *rod.Page, batasTotal time.Duration) {
	_ = p.Timeout(batasTotal).WaitStable(batasTotal / 2)
}

// tutupDialogBerita: saat baru mendarat di dashboard Kinerja sering muncul
// dialog modal "Berita" (headlessui) yang menghalangi interaksi & navigasi.
// Tutup dengan klik ikon X di judul dialog; fallback tekan tombol Escape.
func tutupDialogBerita(page *rod.Page) {
	els, err := page.Elements("h1[id^='headlessui-dialog-title'] svg")
	if err == nil {
		for _, el := range els {
			if vis, _ := el.Visible(); vis {
				if el.Click(proto.InputMouseButtonLeft, 1) == nil {
					p("      Dialog 'Berita' ditutup.")
					stabil(page, time.Second)
					return
				}
			}
		}
	}
	// fallback: kalau dialog masih terbuka, tekan Escape
	if _, err := page.Timeout(1500 * time.Millisecond).Element("h1[id^='headlessui-dialog-title']"); err == nil {
		// fallback: tekan tombol Escape (input.Escape)
		_ = page.Keyboard.Type(input.Escape)
		stabil(page, 500*time.Millisecond)
	}
}

func ambilPaths(page *rod.Page) []string {
	tMulai := time.Now()
	p("[5/5] Buka halaman review perilaku...")
	// Sebelum pindah ke review-perilaku, tutup dulu dialog "Berita" yang
	// biasanya muncul saat baru masuk dashboard (tab yang sama).
	tutupDialogBerita(page)

	// SPA: Navigate() me-reload penuh dan router melempar balik ke
	// /dashboard. Pindah route secara internal: klik link sidebar/menu
	// "Review Perilaku" (a[href='/kinerjajabar/review-perilaku']).
	klikMenuReview := func() bool {
		res, err := page.Evaluate(rod.Eval(`
			() => {
				const links = document.querySelectorAll("a[href*='review-perilaku']");
				for (const a of links) {
					const t = (a.innerText || "").trim();
					// menu sidebar, bukan link kartu review (yang mengandung '/')
					if (!a.getAttribute("href").endsWith("/review-perilaku")) continue;
					a.click();
					return true;
				}
				return false;
			}
		`))
		return err == nil && res.Value.Bool()
	}

	for percobaan := 0; percobaan < 3; percobaan++ {
		if strings.Contains(urlHalaman(page), "review-perilaku") {
			break
		}
		// 1) Coba klik menu internal (tanpa reload)
		if klikMenuReview() {
			p("      Klik menu 'Review Perilaku'...")
		} else {
			// 2) Fallback: pushState SPA-style via history API
			_, _ = page.Evaluate(rod.Eval(`
				() => { history.pushState({}, "", "/kinerjajabar/review-perilaku");
						dispatchEvent(new PopStateEvent("popstate")); }
			`))
			p("      Pindah route via history API...")
		}
		stabil(page, 5 * time.Second)
		if strings.Contains(urlHalaman(page), "review-perilaku") {
			break
		}
		pf("      Masih di %s, ulangi...\n", urlHalaman(page))
		time.Sleep(2 * time.Second)
	}

	// jangan tunggu networkidle: penanda siap yang sebenarnya adalah link kartu
	if _, err := page.Timeout(20 * time.Second).Element("a[href*='review-perilaku/']"); err != nil {
		screenshotDebug(page, "debug.png")
		p("      Card tidak muncul. Screenshot: debug.png")
		return nil
	}

	time.Sleep(600 * time.Millisecond)

	var hasil struct {
		Total int      `json:"total"`
		Belum []string `json:"belum"`
	}
	res, err := page.Evaluate(rod.Eval(jsKartu))
	if err == nil {
		_ = json.Unmarshal([]byte(res.Value.JSON("", "")), &hasil)
	}
	if hasil.Total > 0 && len(hasil.Belum) == 0 {
		time.Sleep(1500 * time.Millisecond) // status mungkin belum ter-render semua
		if res, err := page.Evaluate(rod.Eval(jsKartu)); err == nil {
			_ = json.Unmarshal([]byte(res.Value.JSON("", "")), &hasil)
		}
	}
	paths := []string{}
	for _, p := range hasil.Belum {
		if p != "" {
			paths = append(paths, p)
		}
	}

	if hasil.Total == 0 {
		// struktur tak terduga: pakai cara lama sebagai cadangan
		links, _ := page.Elements("div:has-text('Belum Direview') a[href*='review-perilaku/']")
		for _, link := range links {
			href, _ := link.Attribute("href")
			if href != nil && *href != "" {
				paths = append(paths, *href)
			}
		}
	} else {
		pf("      %d kartu ditemukan, %d belum direview (%.1fs)\n",
			hasil.Total, len(paths), time.Since(tMulai).Seconds())
	}

	// dedup jaga urutan
	lihat := map[string]bool{}
	uniq := paths[:0]
	for _, p := range paths {
		if !lihat[p] {
			lihat[p] = true
			uniq = append(uniq, p)
		}
	}
	return uniq
}

func isiReview(page *rod.Page, path string) bool {
	u := path
	if strings.HasPrefix(path, "/") {
		u = BASE_URL + path
	}
	_ = page.Navigate(u)

	if _, err := page.Timeout(15 * time.Second).Element(".hidden.md\\:flex"); err != nil {
		p("  SKIP: Form tidak muncul")
		return false
	}

	// Klik semua tombol "Selalu" sekaligus via JS tanpa scroll satu-satu
	res, err := page.Evaluate(rod.Eval(`
		() => {
			const btns = document.querySelectorAll('.hidden.md\\\\:flex button:last-child');
			btns.forEach(b => b.click());
			return btns.length;
		}
	`))
	if err != nil {
		p("  SKIP: Tombol tidak ditemukan")
		return false
	}
	jumlah := res.Value.Int()
	if jumlah == 0 {
		p("  SKIP: Tombol tidak ditemukan")
		return false
	}
	pf("  %d aspek diklik sekaligus\n", jumlah)

	// tombol Simpan: cari berdasarkan teks, scroll dulu baru klik
	var simpan *rod.Element
	if els, err := page.Elements("button"); err == nil {
		for _, el := range els {
			txt, _ := el.Text()
			if strings.TrimSpace(txt) == "Simpan" {
				simpan = el
				break
			}
		}
	}
	if simpan == nil {
		p("  SKIP: Tombol Simpan tidak ditemukan")
		return false
	}
	_ = simpan.ScrollIntoView()
	if err := simpan.Click(proto.InputMouseButtonLeft, 1); err != nil {
		p("  SKIP: gagal klik Simpan")
		return false
	}
	stabil(page, 10*time.Second)
	p("  Tersimpan!\n")
	return true
}

func satuAkun(page *rod.Page, browser *rod.Browser, akun Akun) error {
	USERNAME, PASSWORD, TOTP_SECRET = akun.NIP, akun.Password, akun.Secret

	var err error
	for percobaan := 0; percobaan < 3; percobaan++ {
		err = login(page)
		if err == nil {
			break
		}
		if percobaan == 2 {
			return err
		}
		pf("      Login gagal, ulangi (%v)\n", err)
		_ = proto.NetworkClearBrowserCookies{}.Call(page) // buang session basi, mulai login bersih
		time.Sleep(3 * time.Second)
	}

	// Handle tab bisa mati walau login sukses (Chrome mengganti target saat
	// redirect lintas-origin), jadi ambil ulang tab yang masih hidup.
	page = halamanHidup(browser, page)

	page, err = bukaKinerja(page, browser) // bisa pindah tab, jadi page bisa berganti
	if err != nil {
		return err
	}
	paths := ambilPaths(page)

	if len(paths) == 0 {
		p("Tidak ada rekan yang perlu direview.")
		return nil
	}

	pf("\nTotal rekan belum direview: %d\n\n", len(paths))
	berhasil := 0
	for i, path := range paths {
		pf("[%d/%d] %s\n", i+1, len(paths), path)
		if isiReview(page, path) {
			berhasil++
		}
		time.Sleep(1 * time.Second)
	}
	pf("Selesai! %d/%d review berhasil disubmit.\n", berhasil, len(paths))
	return nil
}

// =============================================
//   WIZARD + MENU UTAMA
// =============================================

func wizardAkun(cfg *Config) {
	p("\n--- Tambah akun ---")
	nip := tanya("NIP                : ")
	for _, a := range cfg.Akun {
		if a.NIP == nip {
			p("  NIP ini sudah terdaftar.")
			return
		}
	}
	sandi := tanyaRahasia("Password SMARTASN  : ")

	var secret string
	for {
		mentah := tanyaRahasia("Secret MFA         : ")
		secret = normalisasiSecret(mentah)
		if secret != "" {
			break
		}
		p("  Secret tidak dikenali. Contoh: JBSWY3DPEHPK3PXP,")
		p("  otpauth://totp/...?secret=XXX, atau hasil ekspor")
		p("  otpauth-migration://offline?data=...")
	}

	pf("\n  Kode MFA sekarang  : %s\n", kodeTOTP(secret))
	fmt.Print("  Cocok dengan authenticator? (Y/n) ")
	jawab, _ := reader.ReadString('\n')
	if strings.EqualFold(strings.TrimSpace(jawab), "n") {
		p("  Akun TIDAK disimpan. Ulangi dan cek secret-nya.")
		return
	}
	cfg.Akun = append(cfg.Akun, Akun{NIP: nip, Password: sandi, Secret: secret})
	simpanConfig(*cfg)
	pf("  Akun %s tersimpan.\n", nip)
}

func pastikanAdaAkun(cfg Config) Config {
	for len(cfg.Akun) == 0 {
		p("Belum ada akun tersimpan. Kita buat dulu.")
		wizardAkun(&cfg)
		if len(cfg.Akun) == 0 {
			p("Masih belum ada akun. Coba lagi.")
		}
	}
	return cfg
}

func pilihAkun(cfg Config) []Akun {
	pf("\nAkun tersimpan: %d\n", len(cfg.Akun))
	for i, a := range cfg.Akun {
		pf("  %d. %s\n", i+1, a.NIP)
	}
	fmt.Print("  0. Semua berurutan\nPilihan (nomor): ")
	line, _ := reader.ReadString('\n')
	pil := strings.TrimSpace(line)
	nomor, err := strconv.Atoi(pil)
	if err != nil || nomor < 0 || nomor > len(cfg.Akun) {
		p("  Pilihan tidak ada.")
		return nil
	}
	if nomor == 0 {
		return cfg.Akun
	}
	return []Akun{cfg.Akun[nomor-1]}
}

func main() {
	pastikanSatuInstance()
	go pantauKontrol()

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--telegram":
			telegramMain()
			return
		case "--qr":
			tangkapQR()
			return
		case "--cek":
			cekMandiri()
			return
		case "--akun":
			kelolaAkun()
			return
		case "--probe":
			probe()
			return
		case "--cek-alur":
			cekAlur()
			return
		}
	}

	cfg := pastikanAdaAkun(bacaConfig())
	TAHUN = cfg.Tahun
	JEDA_DASHBOARD = cfg.JedaDashboard
	daftar := pilihAkun(cfg)
	if len(daftar) == 0 {
		return
	}

	jalankan(daftar)
}

// =============================================
//   PEMERIKSAAN MANDIRI (--cek)
// =============================================

func cekMandiri() {
	p("==============================================")
	p("  PEMERIKSAAN APLIKASI")
	p("==============================================")
	pf("  Folder aplikasi : %s\n", BASE_DIR)
	if _, err := os.Stat(CONFIG_PATH); err == nil {
		p("  File config     : ada")
	} else {
		p("  File config     : belum ada (akan dibuat saat pertama jalan)")
	}

	// cek Chrome/Chromium yang bisa dipakai Rod
	cekChrome()

	cfg := bacaConfig()
	pf("  Akun tersimpan  : %d\n", len(cfg.Akun))
	p("==============================================")
}

func cekChrome() {
	fmt.Print("  Chrome/Chromium : ")
	// launcher.LookPath cek Chrome biasa, Edge, Chromium, dsb. di lokasi standar
	if path, exists := launcher.LookPath(); exists {
		versi, err := func() (string, error) {
			out, err := exec.Command(path, "--version").Output()
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(string(out)), nil
		}()
		if err == nil {
			pf("OK (%s)\n", versi)
		} else {
			pf("ADA (tidak bisa jalankan: %v)\n", err)
		}
		return
	}
	p("TIDAK ADA <-- Rod akan mencoba download otomatis saat pertama jalan")
}

// =============================================
//   KELOLA AKUN (--akun)
// =============================================

func kelolaAkun() {
	cfg := bacaConfig()
	for {
		pf("\nAkun tersimpan: %d\n", len(cfg.Akun))
		for i, a := range cfg.Akun {
			pf("  %d. %s\n", i+1, a.NIP)
		}
		p("  t. Tambah akun    h. Hapus akun    q. Keluar")
		fmt.Print("Pilihan: ")
		line, _ := reader.ReadString('\n')
		c := strings.ToLower(strings.TrimSpace(line))
		switch c {
		case "q":
			return
		case "t":
			wizardAkun(&cfg)
		case "h":
			fmt.Print("Nomor akun yang dihapus: ")
			l, _ := reader.ReadString('\n')
			nomor, err := strconv.Atoi(strings.TrimSpace(l))
			if err != nil || nomor < 1 || nomor > len(cfg.Akun) {
				p("  Nomor tidak ada.")
				continue
			}
			hapus := cfg.Akun[nomor-1]
			cfg.Akun = append(cfg.Akun[:nomor-1], cfg.Akun[nomor:]...)
			simpanConfig(cfg)
			pf("  %s dihapus.\n", hapus.NIP)
		default:
			p("  Pilihan tidak dikenal.")
		}
	}
}

// =============================================
//   JALANKAN REVIEW (inti main loop)
// =============================================

func jalankan(daftar []Akun) {
	// launcher.New() memakai Chrome yang sudah ada di PC; kalau tidak ada,
	// Rod otomatis download Chromium (~150MB sekali saja, disimpan per-user).
	// disable-popup-blocking: tombol "Lanjutkan" memakai window.open setelah
	// await request token; Chrome blokir pop-up karena gesture sudah kedaluwarsa
	// -> app tak pernah terbuka. Lihat komentar di review_asn.py.
	u := launcher.New().
		Set("incognito").
		Set("disable-popup-blocking").
		Set("lang", "id-ID").
		Headless(false).
		MustLaunch()

	browser := rod.New().ControlURL(u).MustConnect()
	defer browser.MustClose()
	_ = proto.NetworkClearBrowserCookies{}.Call(browser) // session lama jangan kebawa

	for i, akun := range daftar {
		pf("\n==============================================\n")
		pf("AKUN %d/%d: %s | tahun %s\n", i+1, len(daftar), akun.NIP, TAHUN)
		pf("==============================================\n")

		// Kinerja dibuka di tab baru. Sisa tab akun sebelumnya ditutup supaya
		// cariTabKinerja() tidak menemukan tab Kinerja milik akun lama.
		halaman := browser.MustPages()
		var page *rod.Page
		if len(halaman) > 0 {
			page = halaman[0]
			for _, tabLama := range halaman[1:] {
				_ = tabLama.Close()
			}
			_ = page.Navigate("about:blank")
		} else {
			page = browser.MustPage("about:blank")
		}

		// Catat URL apa pun yang dikirim ke window.open (jaring pengaman)
		_, _ = page.EvalOnNewDocument(jsTangkapOpen)

		if err := satuAkun(page, browser, akun); err != nil {
			pf("ERROR akun %d (%s): %v\n", i+1, akun.NIP, err)
			var tabErr *rod.Page
			if ps := browser.MustPages(); len(ps) > 0 {
				tabErr = ps[len(ps)-1]
			}
			screenshotDebug(tabErr, fmt.Sprintf("debug%d.png", i+1))
			if tabErr != nil {
				pf("URL saat error: %s\n", urlHalaman(tabErr))
			}
		}
	}
}
