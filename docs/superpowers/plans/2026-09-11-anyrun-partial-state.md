# anyrun Milestone: Ketahanan Status Parsial

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** anyrun menyimpan riwayat sesi secara bertahap, sehingga run yang
dimatikan di tengah jalan (timeout, max turn, SIGKILL dari claude-agent) tetap
meninggalkan jejak langkah yang sudah dijalankan. Saat job di-retry, model tahu
di mana ia berhenti — bukan mengulang dari nol.

**Why:** Dilaporkan Kak Pande 11 Sep 2026: *"Ketika anyrun jalankan long task
multi turn yang melewati agent timeout atau max turn, dia exit/killed. Ketika
agent retry jobjobnya dia lupa sampai mana."*

## Diagnosis (sudah diverifikasi, bukan dugaan)

`internal/loop/loop.go` menampung seluruh pesan turn di `newMsgs`, lalu
menuliskannya **sekali di akhir**:

```go
newMsgs := []envelope.Msg{userMsg}
for turn := 0; ; turn++ { /* semua numpuk di sini */ }
// Persist AFTER the run succeeded
return d.Store.Append(d.SessionID, newMsgs...)
```

Setiap jalur yang tidak sampai akhir `Turn` — provider error, context
cancelled, SIGKILL — membuang **seluruh** turn. Termasuk tool call yang sudah
benar-benar dieksekusi dan punya efek samping nyata.

Reproduksi (uji yang sudah dijalankan, lalu dihapus):

```
baseline: 2 pesan
setelah kill: 2 pesan (err=context canceled, provider dipanggil 3 kali)
HASIL: SELURUH turn yang dibunuh HILANG — 2 percobaan tool tidak tercatat
```

Bukti pendukung di produksi: dari 13 berkas sesi di `~/.anyrun/sessions/`,
**semuanya** berakhir rapi dengan `assistant text`. Tidak ada satu pun yang
berakhir di tengah turn. Kalau status parsial pernah tersimpan, seharusnya ada
yang terpotong. Nol. Konsisten dengan sifat all-or-nothing.

Bandingkan dengan Claude Code: transkrip `~/.claude/projects/**/*.jsonl`
ditulis inkremental per pesan. Contoh nyata — `8e22508f` berakhir di tengah
turn (`tool_use:Bash` → `tool_result`) karena mati, dan jejaknya tetap ada.

**Siapa yang mematikan proses:** `claude-agent` `internal/claude/runner.go:481`
mengirim `SIGKILL` ke seluruh process group saat `runCtx` dibatalkan. SIGKILL
tidak bisa ditangkap. Karena itu perbaikan **tidak boleh** bergantung pada
signal handler — satu-satunya jalan adalah menulis lebih awal.

## Bug tetangga yang ditemukan saat investigasi

Ketiganya memperbesar kerusakan dari bug utama. Perlu dikerjakan di milestone
yang sama karena saling mengunci.

1. **`Load` menolak seluruh sesi karena satu baris rusak.** `session/store.go`
   mengembalikan error pada baris JSON apa pun yang gagal di-parse. Append yang
   di-SIGKILL di tengah `Write` meninggalkan baris terakhir yang terpotong.
   Sekarang risikonya kecil (satu append besar); begitu penulisan jadi
   inkremental, risikonya naik sebanding. **Prasyarat wajib Task 1.**

2. **`max_turns` meninggalkan `tool_result` menggantung.** Loop `break`, lalu
   tetap menulis — jadi berkas berakhir di `[assistant: tool_use] [user:
   tool_result]` tanpa kesimpulan assistant. Saat resume: `[tool_result] →
   [user: pesan baru]`. Bukan crash, tapi model menerima hasil tool tanpa
   konteks keputusan yang memakainya.

3. **Jendela hilangnya berkas sesi saat compaction.** `compact.Rewrite`
   melakukan `os.Rename(old, old+".pre-compact")` **lalu** `os.Rename(tmp, old)`.
   Di antara dua rename itu berkas sesi tidak ada. Peluangnya mikrodetik, tapi
   kalau kena: seluruh riwayat sesi lenyap (hanya `.pre-compact` yang tersisa,
   dan tidak ada yang membacanya). Perbaikannya: tukar urutan — rename `tmp`
   ke `old` lebih dulu (rename atomic di POSIX), baru buat cadangan.

## Design decisions (settled)

- **Satuan flush = satu peristiwa, bukan satu turn.** Tepat setelah respons
  assistant diterima, dan tepat setelah seluruh tool result turn itu kembali.
- **`assistant` + `tool_result` ditulis sebagai pasangan.** Format OpenAI
  (`internal/provider/openai/openai.go`) mewajibkan setiap `tool_call` diikuti
  pesan `role=tool` yang cocok. Menulis `tool_use` tanpa hasil = API 400 pada
  resume. Karena itu hasil tool ditulis setelah tool selesai dieksekusi.
- **`assistant` ditulis SEBELUM tool dieksekusi** — inilah yang membuat model
  tahu "aku sedang/akan menjalankan X". Konsekuensinya berkas bisa berakhir
  dengan `tool_use` tanpa hasil bila terbunuh saat tool berjalan; itu ditangani
  oleh repair saat load (Task 3).
- **`userMsg` tidak pernah ditulis sendirian.** Ia menempel pada flush pertama
  (bersamaan dengan respons assistant pertama). Ini menghilangkan duplikasi
  pesan user saat job di-retry: kalau run mati sebelum assistant menjawab,
  tidak ada yang tertulis, dan retry mengirim ulang pesan itu dengan bersih.
- **Repair saat load, bukan saat simpan.** Kalau `Load` menemukan berkas
  berakhir dengan `tool_use` tanpa pasangan `tool_result`, sisipkan satu
  `tool_result` sintetis bertanda "run sebelumnya terputus". Riwayat jadi selalu
  bisa di-replay, dan model tahu tool itu tidak selesai.
- **Baris terakhir yang rusak ditoleransi, baris lain tidak.** Hanya baris
  terakhir yang bisa terpotong oleh kill (append selalu ke ekor). Kerusakan di
  tengah file tetap dianggap korupsi nyata.
- **Signal handler: DI LUAR SCOPE.** `runner.go` memakai `SIGKILL` yang tidak
  bisa ditangkap, jadi `signal.Notify` di anyrun tidak menolong jalur timeout.
  Flush inkremental sudah cukup; menambah handler hanya menambah kode mati.
- **Meta (`context_tokens`, `compact_count`) tetap ditulis di akhir.** Kalau run
  mati, meta basi — self-healing di turn berikutnya karena nilainya
  ditimpa, bukan diakumulasi. Tidak perlu diperbaiki.
- Tidak ada perubahan pada `claude-agent`. Sisi itu sudah benar; yang salah
  adalah asumsi anyrun bahwa ia akan selalu sampai akhir.

---

### Task 1: toleransi baris terakhir yang terpotong

Prasyarat untuk semua task lain — tanpa ini, penulisan inkremental justru
memperbesar peluang sesi tidak bisa dibuka sama sekali.

**Files:** `internal/session/store.go` + `internal/session/store_test.go`

Ubah `Load`: baris terakhir yang gagal di-parse **dan** berada di akhir berkas
→ lewati, tulis peringatan ke `os.Stderr`, lanjut. Baris rusak di posisi lain →
tetap error.

Petunjuk implementasi: `bufio.Scanner` tidak memberi tahu apakah baris yang
sedang diproses adalah yang terakhir. Kenyamanan termudah — baca seluruh baris
dulu ke slice, baru proses dengan indeks terakhir diketahui. Alternatif: catat
posisi baris terakhir sambil iterasi.

Test:
- berkas dengan baris terakhir terpotong (`{"role":"user","content":`) → Load
  sukses, mengembalikan pesan-pesan sebelumnya, panjang berkurang satu
- berkas dengan baris rusak di tengah → tetap error
- berkas utuh → tidak berubah perilakunya (regresi)

- [x] red → green → commit `fix(session): tolerate torn final line on load` — `8948114`

---

### Task 2: flush inkremental di loop

**Files:** `internal/loop/loop.go` + `internal/loop/loop_test.go`

Tambahkan closure flush di dalam `Turn`, dan panggil di empat titik.

```go
pending := []envelope.Msg{userMsg}
flush := func() error {
    if len(pending) == 0 {
        return nil
    }
    if err := d.Store.Append(d.SessionID, pending...); err != nil {
        return err
    }
    pending = pending[:0]
    return nil
}
```

Titik panggil:

1. **Setelah respons assistant diterima**, sebelum tool dieksekusi:
   `pending = append(pending, aMsg); if err := flush(); err != nil { return err }`
   Ini sekaligus menulis `userMsg` (flush pertama).
2. **Setelah seluruh tool result turn itu kembali**:
   `pending = append(pending, rMsg); flush()`.
3. **Setelah loop selesai** (teks akhir atau `max_turns`): `flush()` menutup
   sisa — untuk teks akhir, `pending` berisi pesan assistant terakhir.
4. **Pada jalur error**: sebelum `return err` dari kegagalan provider, panggil
   `flush()` (abaikan error-nya, error asli lebih penting) supaya langkah yang
   sudah selesai tidak ikut hilang.

Hapus `d.Store.Append(d.SessionID, newMsgs...)` di akhir — sudah tergantikan.
Variabel `newMsgs` bisa dihapus seluruhnya.

Test (pakai `fakeProvider` + `script` yang sudah ada di `loop_test.go`):
- **run yang dibunuh menyimpan langkah yang selesai** — provider mengembalikan
  tool call, lalu `context.Canceled` pada panggilan berikutnya. Setelah `Turn`
  kembali dengan error, `Load` harus mengembalikan `[user, assistant, tool_result]`
  (3 pesan), bukan kosong. **Ini test regresi untuk bug yang dilaporkan.**
- **run yang mati pada panggilan pertama tidak menulis apa pun** — `pending`
  masih berisi `userMsg` saja dan belum pernah di-flush → `Load` kosong. Ini
  yang menjaga retry tidak menduplikasi pesan user.
- `max_turns` tetap tersimpan lengkap (regresi `TestMaxTurnsStopsLoop`).

- [x] red → green → commit `fix(loop): persist session state per step, not per turn` — `5f0089d`

---

### Task 3: repair `tool_use` menggantung saat load

Tanpa ini, Task 2 membuka jalur baru menuju kegagalan keras: terbunuh di antara
"assistant menulis `tool_use`" dan "hasil tool tersimpan" meninggalkan riwayat
yang ditolak provider OpenAI-compatible (400: setiap `tool_call` wajib punya
pesan `tool` pasangannya).

**Files:** `internal/loop/loop.go` (atau paket baru `internal/session/repair.go`
kalau lebih rapi) + test

Setelah `history := d.Store.Load(...)`, periksa pesan terakhir:

- Kalau ia `assistant` dengan blok `tool_use` yang belum punya `tool_result`
  sesudahnya → tambahkan satu pesan `user` berisi blok `tool_result` untuk
  **setiap** `tool_use_id` yang menggantung, isinya penanda jujur:
  `"run sebelumnya terputus sebelum tool ini selesai — hasilnya tidak diketahui"`.
- Kalau pesan terakhir sudah berpasangan, atau bukan `tool_use` → tidak ada
  yang dilakukan.

Repair ini **tidak** ditulis ulang ke berkas — cukup dipakai di memori untuk
run kali ini, dan akan tersimpan wajar saat flush berikutnya.

Test:
- history berakhir `assistant(tool_use)` → hasil `Load`+repair berakhir
  `user(tool_result)` dengan `tool_use_id` yang cocok
- `tool_use` dengan dua panggilan → dua `tool_result` sintetis
- history berakhir `assistant(text)` → tidak berubah
- history berakhir `user(tool_result)` (kasus `max_turns`) → tidak berubah

- [x] red → green → commit `fix(loop): repair dangling tool_use on load` — masuk `5f0089d` (dikerjakan satu paket dengan Task 2; keduanya saling mengunci)

---

### Task 4: tutup jendela hilangnya berkas saat compaction

**Files:** `internal/compact/compact.go` + `internal/compact/compact_test.go`

`Rewrite` sekarang: tulis `tmp` → `Rename(old, old+".pre-compact")` →
`Rename(tmp, old)`. Di antara dua rename itu berkas sesi tidak ada. Kalau proses
mati persis di situ, seluruh riwayat sesi lenyap.

Urutan yang benar:

1. tulis `tmp`
2. `os.Rename(tmp, old)` — atomic di POSIX, berkas lama langsung tergantikan
3. **baru** salin `old` → `old+".pre-compact"` (cadangan satu generasi)

Konsekuensi yang perlu diterima: cadangan dibuat dari isi yang **baru**, bukan
yang lama. Untuk pemulihan darurat ini tetap berguna, dan menghilangkan jendela
berkas-hilang sepenuhnya. Kalau cadangan generasi lama tetap diinginkan, tulis
salinan `old` ke `.pre-compact` **sebelum** langkah 2 — salinan, bukan rename,
supaya berkas asli tidak pernah berpindah tempat.

Test:
- `Rewrite` mengganti isi, cadangan ada, berkas utama selalu ada di setiap
  tahap (verifikasi lewat kegagalan yang disuntikkan, atau cukup pastikan tidak
  ada `Rename` yang memindahkan berkas hidup)
- `Rewrite` gagal di tengah → berkas lama tetap utuh

- [x] red → green → commit `fix(compact): stop moving the live session file during Rewrite` — `7e0e750`

---

### Task 6: penanda lanjut untuk run yang terputus

Ditemukan saat uji langsung Task 5, bukan dari membaca kode.

**Masalah:** setelah fix Task 2, riwayat memang memuat langkah yang sudah selesai —
tapi model belum tentu memakainya. Jalur auto-retry worker mengirim ulang pesan
yang **sama persis**, dan tanpa penanda model membacanya sebagai perintah baru:

```
[tool] UNIT-1   ← diulang, padahal sudah selesai di run sebelumnya
```

Kalau yang diulang punya efek samping (`git push`, deploy, kirim email), mengulang
itu merusak — bukan sekadar boros.

**Files:** `internal/loop/loop.go` + `internal/loop/loop_test.go`

Kalau riwayat berakhir di tengah run (`tool_result` terakhir, atau baru saja
diperbaiki dari `tool_use` menggantung), sisipkan satu pesan `user` berisi catatan
sebelum pesan masuk: langkah yang sudah selesai ada di atas, permintaan di bawah
mungkin pengiriman ulang, jangan ulangi yang sudah beres.

Catatan ini **tidak ditulis ke berkas** — sama seperti `tool_result` sintetis, ia
hanya pandangan untuk run ini. Riwayat tetap berisi rekaman asli.

Bukti sebelum/sesudah (uji langsung, sesi yang sama):

| | Kalimat pertama | Tool dipanggil ulang |
|---|---|---|
| tanpa catatan | *"saya ulang dari UNIT-1"* | UNIT-1..6 (semua) |
| dengan catatan | *"UNIT-1,2,3 sudah beres. UNIT-4 terputus — saya ulang dari situ"* | UNIT-4,5,6 |

- [x] red → green → commit `fix(loop): tell the model it is resuming, not starting over`

---

### Task 5: verifikasi menyeluruh + dokumentasi

- [x] `go build ./...` && `go test ./...` — semua lulus (9 paket)
- [x] Uji manual (11 Sep, sesi nyata): SIGKILL di tengah run 8 langkah → 7 pesan
      bertahan (UNIT-1..3 lengkap). Resume dengan pesan baru → model melaporkan
      tahap selesai, tahap terputus (belum terverifikasi), dan tahap belum jalan.
      Binary: `/home/devops/.local/bin/anyrun` (dipasang atomic via rename).
- [ ] Perbarui `README.md` — bagian sesi: status ditulis per langkah,
      `tool_result` sintetis, catatan lanjut, dan berkas boleh berakhir di tengah turn.
- [ ] Commit `docs: document partial-state persistence semantics`

---

## Catatan untuk pelaksana

- **Jangan menyentuh `claude-agent`.** Bug ada di anyrun.
- Perubahan bersifat penambahan; tidak ada format berkas yang berubah, tidak ada
  migrasi. Sesi lama tetap terbaca apa adanya.
- Beban tulis naik dari ~1 append menjadi ~2 per langkah tool. Untuk sesi yang
  panjang ini lebih banyak syscall, tapi tiap append kecil dan berurutan. Kalau
  nanti terbukti mengganggu, jalur optimasinya adalah buffering per langkah —
  bukan kembali ke per-turn.
- Test regresi dari Task 2 adalah **bukti bug-nya**, bukan sekadar penjaga.
  Jangan dihapus setelah hijau.
