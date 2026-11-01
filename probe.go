package main

// probe.go: mode diagnostik (--probe). Login seperti biasa lalu membuang
// struktur DOM dashboard SSO ke probe_*.json supaya kita tahu persis elemen
// mana yang membuka aplikasi Kinerja dan apa yang terjadi setelah diklik.
// File ini hanya alat bantu debug, tidak dipakai alur normal.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
)

const jsProbe = `
() => {
	const norm = s => (s || "").replace(/\s+/g, " ").trim();
	const out = { url: location.href, judul: document.title, kandidat: [], layanan: "", nav: [] };

	document.querySelectorAll("a,button,[role='button'],div,span,li").forEach(el => {
		const t = norm(el.innerText);
		if (!t || t.length > 200) return;
		if (!/kinerja/i.test(t)) return;
		const r = el.getBoundingClientRect();
		if (r.width === 0 || r.height === 0) return;
		out.kandidat.push({
			tag: el.tagName,
			cls: (el.className || "").toString().slice(0, 140),
			href: el.getAttribute("href") || "",
			onclick: (el.getAttribute("onclick") || "").slice(0, 80),
			text: t.slice(0, 120),
			x: Math.round(r.x), y: Math.round(r.y),
			w: Math.round(r.width), h: Math.round(r.height)
		});
	});

	document.querySelectorAll("header a, nav a").forEach(a => {
		out.nav.push({ href: a.getAttribute("href") || "", text: norm(a.innerText).slice(0, 60) });
	});

	const semua = [...document.querySelectorAll("div,section")];
	const sec = semua.find(el => {
		const t = norm(el.innerText);
		return t.startsWith("Layanan Unggulan") && el.querySelectorAll("a,button").length > 0 && t.length < 2000;
	});
	if (sec) out.layanan = sec.outerHTML.slice(0, 12000);
	return out;
}
`

func tulisFile(nama string, data []byte) {
	path := filepath.Join(BASE_DIR, nama)
	_ = os.WriteFile(path, data, 0644)
	pf("      -> %s (%d byte)\n", nama, len(data))
}

// urlAman: MustInfo() bisa panic kalau target sudah di-swap Chrome
// (navigasi lintas-origin saat SSO). Jangan sampai probe ikut mati.
func urlAman(p *rod.Page) (u string) {
	defer func() {
		if r := recover(); r != nil {
			u = "(target hilang)"
		}
	}()
	return p.MustInfo().URL
}

func tabSemua(browser *rod.Browser) []string {
	urls := []string{}
	for _, p := range browser.MustPages() {
		urls = append(urls, urlAman(p))
	}
	return urls
}

// pageHidup: ambil handle tab yang masih valid (yang terakhir dibuka).
func pageHidup(browser *rod.Browser) *rod.Page {
	ps := browser.MustPages()
	if len(ps) == 0 {
		return browser.MustPage("about:blank")
	}
	return ps[len(ps)-1]
}

func buangProbe(page *rod.Page, label string) {
	res, err := page.Evaluate(rod.Eval(jsProbe))
	if err != nil {
		pf("      probe %s gagal: %v\n", label, err)
		return
	}
	data, _ := json.MarshalIndent(res.Value, "", "  ")
	tulisFile("probe_"+label+".json", data)
}

func probe() {
	defer func() {
		if r := recover(); r != nil {
			pf("PROBE panic: %v\n", r)
		}
	}()

	cfg := bacaConfig()
	if len(cfg.Akun) == 0 {
		p("Tidak ada akun di akun.json")
		return
	}
	akun := cfg.Akun[0]
	USERNAME, PASSWORD, TOTP_SECRET = akun.NIP, akun.Password, akun.Secret
	TAHUN = cfg.Tahun

	u := launcher.New().
		Set("incognito").
		Set("disable-popup-blocking").
		Set("no-sandbox").
		Set("lang", "id-ID").
		Headless(true).
		MustLaunch()
	browser := rod.New().ControlURL(u).MustConnect()
	defer browser.MustClose()

	page := browser.MustPage("about:blank")
	_, _ = page.EvalOnNewDocument(jsTangkapOpen)

	p("=== PROBE: login ===")
	if err := login(page); err != nil {
		pf("Login gagal: %v\n", err)
	}
	pf("      Tab setelah login: %v\n", tabSemua(browser))

	page = pageHidup(browser)
	time.Sleep(4 * time.Second)
	buangProbe(page, "1_dashboard")

	p("=== PROBE: klik teks persis 'Kinerja' ===")
	klik := func(teks string) bool {
		res, err := page.Evaluate(rod.Eval(`(t) => {
			const norm = s => (s || "").replace(/\s+/g, " ").trim();
			const el = [...document.querySelectorAll("a,button,[role='button'],div")].find(e => {
				const s = norm(e.innerText);
				return s === t && e.getBoundingClientRect().width > 0;
			});
			if (!el) return false;
			el.click();
			return true;
		}`, teks))
		return err == nil && res.Value.Bool()
	}
	ok := klik("Kinerja")
	pf("      klik berhasil=%v\n", ok)
	time.Sleep(2500 * time.Millisecond)
	pf("      Tab: %v\n", tabSemua(browser))
	page = pageHidup(browser)
	buangProbe(page, "2_setelah_klik_teks")

	p("=== PROBE: klik kartu 'Kinerja Kinerja Pegawai' ===")
	ok = klik("Kinerja Kinerja Pegawai")
	pf("      klik berhasil=%v\n", ok)
	time.Sleep(3 * time.Second)
	pf("      Tab: %v\n", tabSemua(browser))
	page = pageHidup(browser)
	buangProbe(page, "3_setelah_klik_kartu")

	pf("      window.open tertangkap: %q\n", urlOpenTertangkap(page))
	screenshotDebug(page, "probe.png")

	// Buang isi dialog/modal yang muncul setelah klik kartu: di sinilah
	// tombol "Lanjutkan" / "Buka Aplikasi Sekarang" berada.
	detail := `
	() => {
		const norm = s => (s || "").replace(/\s+/g, " ").trim();
		const out = { url: location.href, tombol: [], input: [], dialog: [], teks: "" };
		document.querySelectorAll("button, a").forEach(el => {
			const r = el.getBoundingClientRect();
			if (r.width === 0 || r.height === 0) return;
			out.tombol.push({ tag: el.tagName, cls: (el.className||"").toString().slice(0,120),
				text: norm(el.innerText).slice(0,60), href: el.getAttribute("href")||"" });
		});
		document.querySelectorAll("input, select, [role='radio'], [role='option']").forEach(el => {
			out.input.push({ tag: el.tagName, type: el.getAttribute("type")||"", name: el.getAttribute("name")||"",
				value: el.value||"", text: norm(el.innerText).slice(0,40) });
		});
		document.querySelectorAll("[role='dialog'], [class*='fixed'][class*='inset-0']").forEach(el => {
			out.dialog.push(el.outerHTML.slice(0, 6000));
		});
		out.teks = norm(document.body.innerText).slice(0, 1500);
		return out;
	}
	`
	if res, err := page.Evaluate(rod.Eval(detail)); err == nil {
		data, _ := json.MarshalIndent(res.Value, "", "  ")
		tulisFile("probe_5_modal.json", data)
	}

	p("=== PROBE: pilih tahun + Lanjutkan ===")
	res, err := page.Evaluate(rod.Eval(`(tahun) => {
		const out = { radio: false, lanjut: false };
		const r = document.querySelector("input[name='tahun'][value='" + tahun + "']");
		if (r) { r.click(); out.radio = r.checked; }
		const btn = [...document.querySelectorAll("button")].find(b =>
			(b.innerText || "").includes("Lanjutkan") && b.getBoundingClientRect().width > 0);
		if (btn) { btn.click(); out.lanjut = true; }
		return out;
	}`, TAHUN))
	if err == nil {
		pf("      radio=%v lanjut=%v\n", res.Value.Get("radio").Bool(), res.Value.Get("lanjut").Bool())
	} else {
		pf("      gagal: %v\n", err)
	}

	for i := 0; i < 10; i++ {
		time.Sleep(1 * time.Second)
		urls := tabSemua(browser)
		pf("      t+%ds tab: %v\n", i+1, urls)
		if len(urls) > 1 {
			break
		}
	}
	pf("      window.open tertangkap: %q\n", urlOpenTertangkap(page))
	page = pageHidup(browser)
	screenshotDebug(page, "probe_lanjut.png")
	buangProbe(page, "6_setelah_lanjut")

	p("=== PROBE selesai ===")
	fmt.Print("Tekan Enter untuk menutup browser...")
	_, _ = reader.ReadString('\n')
}

// cekAlur: uji alur login -> buka Kinerja -> baca daftar review, TANPA
// menekan tombol Simpan. Dipakai untuk memastikan perbaikan jalan.
func cekAlur() {
	cfg := bacaConfig()
	if len(cfg.Akun) == 0 {
		p("Tidak ada akun di akun.json")
		return
	}
	akun := cfg.Akun[0]
	USERNAME, PASSWORD, TOTP_SECRET = akun.NIP, akun.Password, akun.Secret
	TAHUN = cfg.Tahun
	JEDA_DASHBOARD = cfg.JedaDashboard

	u := launcher.New().
		Set("incognito").
		Set("disable-popup-blocking").
		Set("no-sandbox").
		Set("lang", "id-ID").
		Headless(true).
		MustLaunch()
	browser := rod.New().ControlURL(u).MustConnect()
	defer browser.MustClose()

	page := browser.MustPage("about:blank")
	_, _ = page.EvalOnNewDocument(jsTangkapOpen)

	if err := login(page); err != nil {
		pf("GAGAL login: %v\n", err)
		return
	}
	page = halamanHidup(browser, page)

	tab, err := bukaKinerja(page, browser)
	if err != nil {
		pf("GAGAL buka Kinerja: %v\n", err)
		screenshotDebug(page, "cek_gagal.png")
		return
	}

	paths := ambilPaths(tab)
	pf("HASIL: %d kartu 'Belum Direview' terbaca.\n", len(paths))
	for i, s := range paths {
		if i >= 5 {
			pf("  ... dan %d lagi\n", len(paths)-5)
			break
		}
		pf("  - %s\n", s)
	}
	screenshotDebug(tab, "cek_kinerja.png")
}
