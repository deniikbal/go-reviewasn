// single.go: pastikan hanya ada SATU instance ReviewASN yang berjalan.
//
// Mekanisme: file lock di samping exe (reviewasn.lock).
//   - Saat aplikasi start, coba kunci file. Kalau berhasil -> lanjut.
//   - Kalau gagal (ada instance lain memegang lock), tulis "quit" ke file
//     kontrol -> instance lama melihat sinyal dan keluar -> tunggu lock
//     dilepas -> kunci dan lanjut.
//
// File lock otomatis terlepas oleh OS jika instance lama crash/terkill,
// jadi tidak ada risiko lock "basi" menggantung.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	LOCK_PATH    string
	CONTROL_PATH string
	lockFile     *os.File // pegangan lock (tetap terbuka selama app jalan)
)

// pastikanSatuInstance dipanggil di awal main().
// Mengembalikan setelah instance lain sudah berhenti dan lock berhasil dipegang.
//
// Penting: hanya proses yang MEMEGANG lock yang boleh memantau file kontrol.
// Dulu proses mana pun (termasuk build lain seperti probe_test.exe) ikut
// membaca file kontrol dan keluar sendiri karena sisa file "quit" lama.
func pastikanSatuInstance() {
	LOCK_PATH = filepath.Join(BASE_DIR, "reviewasn.lock")
	CONTROL_PATH = filepath.Join(BASE_DIR, "reviewasn.control")

	// File kontrol basi dari proses sebelumnya jangan sampai mematikan
	// proses ini. Yang berhak membuatnya hanya proses yang gagal ambil lock.
	os.Remove(CONTROL_PATH)

	deadline := time.Now().Add(20 * time.Second)
	sudahSinyal := false
	for {
		f, err := os.OpenFile(LOCK_PATH, os.O_CREATE|os.O_RDWR, 0644)
		if err == nil {
			if kunciFile(f) == nil {
				lockFile = f
				os.Remove(CONTROL_PATH)
				return
			}
			f.Close()
		}
		// err != nil (mis. "Access is denied") juga berarti ada instance lain
		// yang sedang memegang file lock.

		if !sudahSinyal {
			sinyalKeluar()
			sudahSinyal = true
		}
		if time.Now().After(deadline) {
			// Jangan lanjut: dua instance = bot Telegram saling rebutan
			// (error "Conflict: terminated by other getUpdates request").
			fmt.Println("ERROR: instance ReviewASN lain masih berjalan.")
			fmt.Println("       Tutup jendela ReviewASN yang lama, lalu jalankan lagi.")
			os.Exit(1)
		}
		fmt.Println("Instance lain terdeteksi, memintanya keluar...")
		time.Sleep(500 * time.Millisecond)
	}
}

// sinyalKeluar: minta instance lama berhenti lewat file kontrol.
func sinyalKeluar() {
	f, err := os.OpenFile(CONTROL_PATH, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return
	}
	f.WriteString("quit")
	f.Close()
}

// pantauKontrol: goroutine background yang menutup aplikasi bila ada
// instance baru menulis "quit" ke file kontrol.
func pantauKontrol() {
	// Tanpa lock, file kontrol bukan untuk kita (bisa sisa proses lain).
	if lockFile == nil {
		return
	}
	for {
		time.Sleep(500 * time.Millisecond)
		st, err := os.Stat(CONTROL_PATH)
		if err != nil {
			continue
		}
		// Abaikan file basi supaya tidak mematikan proses yang tidak diminta.
		if time.Since(st.ModTime()) > 30*time.Second {
			os.Remove(CONTROL_PATH)
			continue
		}
		data, err := os.ReadFile(CONTROL_PATH)
		if err == nil && strings.TrimSpace(string(data)) == "quit" {
			os.Remove(CONTROL_PATH)
			fmt.Println("\nInstance baru diminta jalan — aplikasi ini keluar otomatis.")
			// os.Exit melepas file lock secara otomatis (proses mati).
			os.Exit(0)
		}
	}
}
