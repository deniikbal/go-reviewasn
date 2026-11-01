package main

// Mode --qr: bantu ambil secret TOTP langsung dari QR code di layar.
//
// Alur:
//  1. Buka browser (jendela terlihat) ke SMARTASN.
//  2. User login manual dan sampai ke halaman yang menampilkan QR MFA.
//  3. Program men-screenshot halaman tiap 1 detik dan memindai QR.
//  4. Begitu QR otpauth:// ditemukan -> tampilkan secret, minta konfirmasi
//     (atau --ya untuk langsung simpan), lalu tulis ke akun.json.
//
// Pendekatan ini bekerja untuk QR apa pun (SMARTASN, Google Authenticator
// migration, dll) karena kita scan gambar di layar, bukan cari elemen DOM.

import (
	"image"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "golang.org/x/image/bmp"
	_ "image/jpeg"
	_ "image/png"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

// pindaiQR: cari QR code otpauth:// di gambar screenshot. Kembalikan isi
// QR-nya (string) atau "" kalau tidak ada.
func pindaiQR(img image.Image) string {
	if img == nil {
		return ""
	}
	// gozxing kadang panic di gambar aneh; amankan dengan recover.
	defer func() {
		if r := recover(); r != nil {
			_ = r
		}
	}()
	bmp, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		return ""
	}
	res, err := qrcode.NewQRCodeReader().Decode(bmp, nil)
	if err != nil {
		return ""
	}
	teks := res.GetText()
	if strings.HasPrefix(teks, "otpauth://") || strings.HasPrefix(teks, "otpauth-migration://") {
		return teks
	}
	return ""
}

// tangkapQR: jendela utama mode --qr.
//
// Dua sub-mode:
//   --qr           : buka browser, pindai QR dari layar (untuk halaman
//                    pendaftaran/reset MFA yang tidak minta MFA lagi).
//   --qr foto.png  : scan QR dari file gambar (screenshot/foto). Berguna
//                    untuk QR ekspor Google Authenticator yang muncul di HP:
//                    foto QR di layar HP -> kirim ke PC -> scan di sini.
func tangkapQR() {
	// Sub-mode 1: file gambar eksplisit di argumen
	if len(os.Args) > 2 {
		for _, f := range os.Args[2:] {
			if !strings.HasSuffix(strings.ToLower(f), ".png") &&
				!strings.HasSuffix(strings.ToLower(f), ".jpg") &&
				!strings.HasSuffix(strings.ToLower(f), ".jpeg") &&
				!strings.HasSuffix(strings.ToLower(f), ".bmp") &&
				!strings.HasSuffix(strings.ToLower(f), ".webp") {
				continue
			}
			scanFile(f)
		}
		return
	}

	// Sub-mode 2: pantau folder "qr" — semua gambar di situ discan,
	// hasilnya ditampilkan (secret + kode 6 digit) lalu diperbarui
	// berkala mengikuti jam berjalan TOTP (30 detik).
	folderQR := filepath.Join(BASE_DIR, "qr")
	if err := os.MkdirAll(folderQR, 0755); err != nil {
		pf("Gagal buat folder qr: %v\n", err)
		return
	}

	p("==============================================")
	p("  PEMINDAI QR MFA DARI FOLDER")
	p("==============================================")
	pf("  Folder dipantau : %s\n", folderQR)
	p("  Cara pakai     :")
	p("    1. Simpan screenshot/foto QR ke folder 'qr' di samping")
	p("       aplikasi (png/jpg/jpeg/bmp/webp).")
	p("    2. Program otomatis memindai & menampilkan kode 6 digit.")
	p("    3. Salin kode ke SMARTASN. Kode berganti tiap 30 detik.")
	p("    4. Tekan Ctrl+C untuk keluar.")
	p("==============================================")
	p()

	terproses := map[string]bool{}
	var secretAktif string

	for {
		// 1. Scan file baru di folder
		entri, err := os.ReadDir(folderQR)
		if err == nil {
			for _, e := range entri {
				if e.IsDir() || terproses[e.Name()] {
					continue
				}
				nama := strings.ToLower(e.Name())
				if !strings.HasSuffix(nama, ".png") && !strings.HasSuffix(nama, ".jpg") &&
					!strings.HasSuffix(nama, ".jpeg") && !strings.HasSuffix(nama, ".bmp") &&
					!strings.HasSuffix(nama, ".webp") {
					continue
				}
				terproses[e.Name()] = true
				q := scanFile(filepath.Join(folderQR, e.Name()))
				if secret := normalisasiSecret(q); secret != "" {
					secretAktif = secret
					cfg := bacaConfig()
					simpanQRKeConfig(&cfg, secret)
				}
			}
		}

		// 2. Tampilkan kode berjalan untuk secret aktif
		if secretAktif != "" {
			kode := kodeTOTP(secretAktif)
			sisa := 30 - (time.Now().Unix() % 30)
			pf("\r  Kode MFA: %s   (berlaku %2ds lagi) ", kode, sisa)
		}
		time.Sleep(1 * time.Second)
	}
}

// scanFile: scan satu file gambar. Kembalikan isi QR (string) atau "".
func scanFile(nama string) string {
	pf("\n  Scan file: %s\n", filepath.Base(nama))
	data, err := os.ReadFile(nama)
	if err != nil {
		pf("      Gagal baca file: %v\n", err)
		return ""
	}
	img := bytesToImg(data)
	if img == nil {
		p("      Format gambar tidak dikenali.")
		return ""
	}
	q := pindaiQR(img)
	if q == "" {
		p("      QR otpauth tidak ditemukan di gambar ini.")
		p("      (Pastikan QR terlihat utuh & cukup besar)")
		return ""
	}
	secret := normalisasiSecret(q)
	pf("  QR DITEMUKAN! Isi: %.60s...\n", q)
	if secret == "" {
		p("      Secret tidak bisa dibaca dari QR ini.")
		return ""
	}
	pf("  Secret: %s\n", secret)
	pf("  Kode MFA sekarang: %s\n", kodeTOTP(secret))
	return q
}

// simpanQRKeConfig: pasangkan secret dengan akun.
// Kalau sudah ada 1 akun di config, secret dipasangkan ke akun itu.
// Kalau ada 0 atau >1 akun, minta NIP.
func simpanQRKeConfig(cfg *Config, secret string) {
	if len(cfg.Akun) == 1 {
		cfg.Akun[0].Secret = secret
		simpanConfig(*cfg)
		pf("  Secret diperbarui untuk akun %s.\n", cfg.Akun[0].NIP)
		return
	}
	nip := tanya("  NIP untuk akun ini: ")
	for i := range cfg.Akun {
		if cfg.Akun[i].NIP == nip {
			cfg.Akun[i].Secret = secret
			simpanConfig(*cfg)
			pf("  Secret akun %s diperbarui.\n", nip)
			return
		}
	}
	cfg.Akun = append(cfg.Akun, Akun{NIP: nip, Secret: secret})
	simpanConfig(*cfg)
	pf("  Akun baru %s ditambahkan (password belum diisi, jalankan --akun untuk melengkapi).\n", nip)
}

// bytesToImg: decode PNG bytes ke image.Image.
func bytesToImg(b []byte) image.Image {
	img, _, err := image.Decode(strings.NewReader(string(b)))
	if err != nil {
		return nil
	}
	return img
}
