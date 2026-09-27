package market

import (
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// A-186: what an upload stores, for whom, and what it never keeps (PostgreSQL).

// stallScans makes f hold every stream until release (also run at cleanup); entered gets one value per stream
// held.
func stallScans(t *testing.T, f *fakeClamd) (entered <-chan struct{}, release func()) {
	in, gate := make(chan struct{}, 32), make(chan struct{})
	f.SetVerdict(func([]byte) string {
		in <- struct{}{}
		<-gate
		return "stream: OK"
	})
	release = sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release)
	return in, release
}

// uploadAsync posts in the background; the result arrives on the channel.
func (e *testEnv) uploadAsync(path, session string, parts ...part) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- e.upload(path, session, parts...) }()
	return done
}

func waitEntered(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the upload never reached the scanner")
	}
}

// The file name and the part's Content-Type are never stored, logged or shown, whatever the outcome.
func TestFilenameNeverPersistedLoggedOrRendered(t *testing.T) {
	e, f := uploadEnv(t)
	vendorID, vendor := e.user("filename_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	logs := captureLog(t)
	const name, ctype = "FIXTURE-FILENAME-q7x2 home.jpg", "image/x-fixture-type-q7x2"
	withName := func(parts []part) []part {
		p := slices.Clone(parts)
		p[len(p)-1].filename, p[len(p)-1].ctype = name, ctype
		return p
	}
	var pages []string
	for _, w := range []*httptest.ResponseRecorder{
		e.upload("/listings/photo", vendor, withName(e.photoParts(vendor, listing, metadataJPEG(t, 300, 200)))...),
		e.upload("/account/avatar", vendor, withName(e.avatarParts(vendor, ancillaryPNG(t, 100, 100)))...),
		e.upload("/account/avatar", vendor, withName(e.avatarParts(vendor, markerPNG(t)))...),                // refused by the scanner
		e.upload("/account/avatar", vendor, withName(e.avatarParts(vendor, pngHeader(4097, 1, 8, 2, 0)))...), // refused by the caps
		e.upload("/account/avatar", vendor, withName(e.avatarParts(vendor, []byte("GIF89a")))...),            // not an image
	} {
		pages = append(pages, w.Body.String())
	}
	f.SetVerdict(func([]byte) string { return fakeClamdClose })
	pages = append(pages, e.upload("/account/avatar", vendor, withName(e.avatarParts(vendor, smallJPEG(t)))...).Body.String())
	pages = append(pages, e.do("GET", "/listing-edit?id="+listing, vendor, nil).Body.String(), e.do("GET", "/account", vendor, nil).Body.String())
	if e.imageCount() != 2 {
		t.Fatalf("stored %d", e.imageCount())
	}
	for _, needle := range []string{"FIXTURE-FILENAME", "q7x2", "x-fixture-type"} {
		for i, p := range pages {
			if strings.Contains(p, needle) {
				t.Fatalf("response %d shows %q", i, needle)
			}
		}
		if strings.Contains(logs.String(), needle) {
			t.Fatalf("the log carries %q:\n%s", needle, logs.String())
		}
		assertNowhereInDB(t, e, needle)
	}
}

// assertNowhereInDB fails when any text or bytea column of any table in the test schema holds needle.
func assertNowhereInDB(t *testing.T, e *testEnv, needle string) {
	t.Helper()
	rows, err := e.DB.Query(`SELECT table_name,column_name,data_type FROM information_schema.columns WHERE table_schema=current_schema() AND data_type IN ('text','character varying','bytea','jsonb','json')`)
	if err != nil {
		t.Fatal(err)
	}
	var cols [][3]string
	for rows.Next() {
		var c [3]string
		rows.Scan(&c[0], &c[1], &c[2])
		cols = append(cols, c)
	}
	rows.Close()
	if len(cols) < 20 {
		t.Fatalf("only %d columns found", len(cols))
	}
	for _, c := range cols {
		cond := `"` + c[1] + `"::text LIKE '%'||$1||'%'`
		if c[2] == "bytea" {
			cond = `position(convert_to($1,'UTF8') in "` + c[1] + `")>0`
		}
		var n int
		if err := e.DB.QueryRow(`SELECT count(*) FROM "`+c[0]+`" WHERE `+cond, needle).Scan(&n); err != nil {
			t.Fatalf("%s.%s: %v", c[0], c[1], err)
		}
		if n != 0 {
			t.Fatalf("%s.%s holds %q", c[0], c[1], needle)
		}
	}
}

// C4: an upload the scanner refuses, or that fails the caps, leaves no row and none of its bytes anywhere in the
// database; only the infected one leaves its fixed-copy audit row.
func TestRefusedUploadLeavesNoRow(t *testing.T) {
	e, f := uploadEnv(t)
	vendorID, vendor := e.user("no_row_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	const planted = "FIXTURE-REFUSED-UPLOAD-BYTES"
	infected := withPNGChunks(markerPNG(t), pngChunk("tEXt", []byte("Comment\x00"+planted)))
	capped := withPNGChunks(pngHeader(4097, 1, 8, 2, 0), pngChunk("tEXt", []byte("Comment\x00"+planted)))
	checkPage(t, e.upload("/listings/photo", vendor, e.photoParts(vendor, listing, infected)...), 422, "the malware scanner flagged this file")
	checkPage(t, e.upload("/listings/photo", vendor, e.photoParts(vendor, listing, capped)...), 422, "4097×1 pixels")
	f.SetVerdict(func([]byte) string { return fakeClamdClose })
	checkPage(t, e.upload("/account/avatar", vendor, e.avatarParts(vendor, withPNGChunks(fixturePNG(t, fixturePicture(64, 48)), pngChunk("tEXt", []byte("Comment\x00"+planted))))...), 503, "did not respond")
	if e.imageCount() != 0 {
		t.Fatalf("%d image rows after refusals", e.imageCount())
	}
	assertNowhereInDB(t, e, planted)
	assertNowhereInDB(t, e, fakeClamdMarker)
	var audits int
	e.DB.QueryRow("SELECT count(*) FROM audit_events WHERE user_id=$1", vendorID).Scan(&audits)
	if audits != 1 {
		t.Fatalf("%d audit rows, want the one infected refusal", audits)
	}
}

// The images table has exactly the ruled columns: nothing for a file name, source, original size, hash, scan
// or hidden flag.
func TestImageSchemaColumnsAllowList(t *testing.T) {
	e := newTestApp(t)
	rows, err := e.DB.Query(`SELECT column_name FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='images' ORDER BY column_name`)
	if err != nil {
		t.Fatal(err)
	}
	var cols []string
	for rows.Next() {
		var c string
		rows.Scan(&c)
		cols = append(cols, c)
	}
	rows.Close()
	want := []string{"created", "description", "full", "height", "id", "kind", "owner_user_id", "product_id", "thumb", "width"}
	if !slices.Equal(cols, want) {
		t.Fatalf("images columns %q, want %q", cols, want)
	}
	// The table's own CHECKs refuse what the application never writes.
	uid, _ := e.user("schema_vendor", "vendor")
	pid := e.product(uid, "physical")
	for name, q := range map[string]string{
		"photo without listing":  `INSERT INTO images(id,owner_user_id,kind,"full",thumb,width,height) VALUES('0123456789abcdef0123456789abcdef',$1,'photo','\x00','\x00',1,1)`,
		"avatar with listing":    `INSERT INTO images(id,owner_user_id,product_id,kind,"full",thumb,width,height) VALUES('0123456789abcdef0123456789abcdef',$1,'` + pid + `','avatar','\x00','\x00',1,1)`,
		"full over 2 MiB":        `INSERT INTO images(id,owner_user_id,kind,"full",thumb,width,height) VALUES('0123456789abcdef0123456789abcdef',$1,'avatar',decode(repeat('00',2097153),'hex'),'\x00',1,1)`,
		"thumb over 256 KiB":     `INSERT INTO images(id,owner_user_id,kind,"full",thumb,width,height) VALUES('0123456789abcdef0123456789abcdef',$1,'avatar','\x00',decode(repeat('00',262145),'hex'),1,1)`,
		"guessable id":           `INSERT INTO images(id,owner_user_id,kind,"full",thumb,width,height) VALUES('1',$1,'avatar','\x00','\x00',1,1)`,
		"long description":       `INSERT INTO images(id,owner_user_id,kind,"full",thumb,width,height,description) VALUES('0123456789abcdef0123456789abcdef',$1,'avatar','\x00','\x00',1,1,repeat('a',251))`,
		"width over 1200 pixels": `INSERT INTO images(id,owner_user_id,kind,"full",thumb,width,height) VALUES('0123456789abcdef0123456789abcdef',$1,'avatar','\x00','\x00',1201,1)`,
	} {
		if _, err := e.DB.Exec(q, uid); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	e.DB.Exec(`INSERT INTO images(id,owner_user_id,kind,"full",thumb,width,height) VALUES('0123456789abcdef0123456789abcdef',$1,'avatar','\x00','\x00',1,1)`, uid)
	if _, err := e.DB.Exec(`INSERT INTO images(id,owner_user_id,kind,"full",thumb,width,height) VALUES('fedcba9876543210fedcba9876543210',$1,'avatar','\x00','\x00',1,1)`, uid); err == nil {
		t.Error("a second avatar for one owner accepted")
	}
	// Owner decision storage: staff are hidden by default and the public staff list is off.
	var hidden bool
	var list string
	if err := e.DB.QueryRow(`SELECT staff_hidden,(SELECT value FROM settings WHERE key='staff_list_public') FROM users WHERE id=$1`, uid).Scan(&hidden, &list); err != nil || !hidden || list != "false" {
		t.Fatalf("staff_hidden=%v staff_list_public=%q %v", hidden, list, err)
	}
}

// The same file from two vendors is two rows with unrelated 128-bit ids; nothing links them.
func TestSameImageTwoVendorsDistinctIDs(t *testing.T) {
	e, _ := uploadEnv(t)
	photo := smallJPEG(t)
	for _, h := range []string{"same_a", "same_b"} {
		id, s := e.user(h, "vendor")
		if w := e.upload("/listings/photo", s, e.photoParts(s, e.product(id, "physical"), photo)...); w.Code != 303 {
			t.Fatalf("%s: %d", h, w.Code)
		}
	}
	rows, _ := e.DB.Query("SELECT id FROM images")
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	if len(ids) != 2 || ids[0] == ids[1] || !hex32.MatchString(ids[0]) || !hex32.MatchString(ids[1]) {
		t.Fatalf("ids %q", ids)
	}
}

// Another vendor's listing, or no listing, is "not found", and nothing is stored.
func TestPhotoOtherVendorListing404(t *testing.T) {
	e, _ := uploadEnv(t)
	ownerID, _ := e.user("owner_vendor", "vendor")
	listing := e.product(ownerID, "physical")
	_, other := e.user("other_vendor", "vendor")
	checkPage(t, e.upload("/listings/photo", other, e.photoParts(other, listing, smallJPEG(t))...), 404, "listing not found")
	checkPage(t, e.upload("/listings/photo", other, e.photoParts(other, randomToken(), smallJPEG(t))...), 404, "listing not found")
	checkPage(t, e.upload("/listings/photo", other, e.photoParts(other, "", smallJPEG(t))...), 404, "listing not found")
	if e.imageCount() != 0 {
		t.Fatal("stored")
	}
}

// Moderators take photos down (A-190) but never add them; they may set a staff avatar (owner decision).
func TestPhotoModeratorRefused(t *testing.T) {
	e, f := uploadEnv(t)
	vendorID, _ := e.user("mod_case_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	_, moderator := e.user("photo_moderator", "moderator")
	checkPage(t, e.upload("/listings/photo", moderator, e.photoParts(moderator, listing, smallJPEG(t))...), 403, "only vendors and administrators")
	if scansOf(f) != 0 || e.imageCount() != 0 {
		t.Fatal("a moderator's listing photo was scanned or stored")
	}
	if w := e.upload("/account/avatar", moderator, e.avatarParts(moderator, smallJPEG(t))...); w.Code != 303 {
		t.Fatalf("moderator avatar: %d", w.Code)
	}
}

// The avatar is always the actor's own: an id in the form is ignored.
func TestAvatarIgnoresFormUserID(t *testing.T) {
	e, _ := uploadEnv(t)
	actorID, actor := e.user("avatar_actor", "vendor")
	victimID, _ := e.user("avatar_victim", "vendor")
	parts := []part{e.csrfPart(actor), {name: "id", body: []byte(victimID)}, {name: "description", body: []byte("FIXTURE-avatar-description")}, {name: "photo", body: smallJPEG(t)}}
	if w := e.upload("/account/avatar", actor, parts...); w.Code != 303 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var owner, desc string
	if err := e.DB.QueryRow("SELECT owner_user_id,description FROM images").Scan(&owner, &desc); err != nil || owner != actorID || desc != "" {
		t.Fatalf("owner %q (actor %q) description %q %v", owner, actorID, desc, err)
	}
}

// C5: a role taken away while the upload was being scanned is noticed at commit.
func TestPhotoRecheckAfterScanDemotion(t *testing.T) {
	e, f := uploadEnv(t)
	vendorID, vendor := e.user("demoted_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	entered, release := stallScans(t, f)
	done := e.uploadAsync("/listings/photo", vendor, e.photoParts(vendor, listing, smallJPEG(t))...)
	waitEntered(t, entered)
	if _, err := e.DB.Exec("UPDATE users SET role='buyer' WHERE id=$1", vendorID); err != nil {
		t.Fatal(err)
	}
	release()
	checkPage(t, <-done, 403, "role changed")
	if e.imageCount() != 0 {
		t.Fatal("stored after demotion")
	}
}

// C5: a session revoked while the upload was being scanned commits nothing.
func TestRevokedSessionUploadDoesNotCommit(t *testing.T) {
	e, f := uploadEnv(t)
	vendorID, vendor := e.user("revoked_vendor", "vendor")
	entered, release := stallScans(t, f)
	done := e.uploadAsync("/account/avatar", vendor, e.avatarParts(vendor, smallJPEG(t))...)
	waitEntered(t, entered)
	if _, err := e.DB.Exec("DELETE FROM sessions WHERE user_id=$1", vendorID); err != nil {
		t.Fatal(err)
	}
	release()
	w := <-done
	checkPage(t, w, 401, "your session has ended")
	if !strings.Contains(w.Body.String(), `action="/login"`) || e.imageCount() != 0 {
		t.Fatal("not the sign-in page, or stored")
	}
}

// An archived or taken-down listing takes no photo.
func TestPhotoTakenDownListingRefused(t *testing.T) {
	e, _ := uploadEnv(t)
	vendorID, vendor := e.user("takedown_vendor", "vendor")
	archived, takenDown := e.product(vendorID, "physical"), e.product(vendorID, "physical")
	e.DB.Exec("UPDATE products SET archived=true WHERE id=$1", archived)
	e.DB.Exec("UPDATE products SET archived=true,admin_archive_reason='FIXTURE takedown reason' WHERE id=$1", takenDown)
	checkPage(t, e.upload("/listings/photo", vendor, e.photoParts(vendor, archived, smallJPEG(t))...), 409, "this listing is archived")
	w := e.upload("/listings/photo", vendor, e.photoParts(vendor, takenDown, smallJPEG(t))...)
	checkPage(t, w, 409, "an administrator took this listing down")
	if strings.Contains(w.Body.String(), "FIXTURE takedown reason") || e.imageCount() != 0 {
		t.Fatal("stored, or the reason shown on the refusal")
	}
	// The refusal links back to the listing.
	if !strings.Contains(w.Body.String(), `href="/listing-edit?id=`+takenDown+`#photos"`) {
		t.Fatalf("no link back:\n%s", w.Body.String())
	}
}

// Replacing an avatar deletes the old row in the same transaction.
func TestReplaceDeletesOldRows(t *testing.T) {
	e, _ := uploadEnv(t)
	uid, s := e.user("replace_vendor", "vendor")
	var ids []string
	for i := range 3 {
		if w := e.upload("/account/avatar", s, e.avatarParts(s, fixtureJPEG(t, fixturePicture(100+i, 100), 80))...); w.Code != 303 {
			t.Fatalf("upload %d: %d", i, w.Code)
		}
		var id string
		if err := e.DB.QueryRow("SELECT id FROM images WHERE owner_user_id=$1", uid).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if e.imageCount() != 1 || ids[0] == ids[1] || ids[1] == ids[2] {
		t.Fatalf("rows %d ids %q", e.imageCount(), ids)
	}
	var audits []string
	rows, _ := e.DB.Query("SELECT action FROM audit_events WHERE user_id=$1 ORDER BY id", uid)
	for rows.Next() {
		var a string
		rows.Scan(&a)
		audits = append(audits, a)
	}
	rows.Close()
	if !slices.Equal(audits, []string{"Added avatar", "Replaced avatar", "Replaced avatar"}) {
		t.Fatalf("audit %q", audits)
	}
}

// An administrator's photo on a vendor's listing belongs to the vendor, is audited as the administrator's and
// notifies the vendor.
func TestUploadAdminPhotoOnVendorListing(t *testing.T) {
	e, _ := uploadEnv(t)
	vendorID, _ := e.user("admin_case_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	adminID, admin := e.user("photo_admin", "admin")
	if w := e.upload("/listings/photo", admin, e.photoParts(admin, listing, smallJPEG(t))...); w.Code != 303 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var owner, audit, note string
	e.DB.QueryRow("SELECT owner_user_id FROM images").Scan(&owner)
	e.DB.QueryRow("SELECT action FROM audit_events WHERE user_id=$1 ORDER BY id DESC LIMIT 1", adminID).Scan(&audit)
	e.DB.QueryRow("SELECT body FROM notifications WHERE user_id=$1", vendorID).Scan(&note)
	if owner != vendorID || audit != "Added a photo to listing "+listing[:8]+" as administrator" || note != "An administrator added a photo to your listing "+listing[:8]+"." {
		t.Fatalf("owner %q audit %q note %q", owner, audit, note)
	}
}

// No database connection or transaction is held while clamd scans.
func TestNoTransactionHeldDuringScan(t *testing.T) {
	e, f := uploadEnv(t)
	vendorID, vendor := e.user("no_tx_vendor", "vendor")
	listing := e.product(vendorID, "physical")
	entered, release := stallScans(t, f)
	done := e.uploadAsync("/listings/photo", vendor, e.photoParts(vendor, listing, smallJPEG(t))...)
	waitEntered(t, entered)
	if inUse := e.A.db.Stats().InUse; inUse != 0 {
		t.Fatalf("%d application pool connections in use during the scan", inUse)
	}
	var open int
	if err := e.DB.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND state LIKE 'idle in transaction%' AND query ILIKE '%products%'`).Scan(&open); err != nil || open != 0 {
		t.Fatalf("transactions open during the scan: %d %v", open, err)
	}
	release()
	if w := <-done; w.Code != 303 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

// The admin scanner panel shows the images' count and bytes against IMAGE_STORAGE_LIMIT_MB.
func TestAdminPanelImageStorage(t *testing.T) {
	e, _ := uploadEnv(t)
	_, admin := e.user("storage_admin", "admin")
	panel := scannerPanel(e.do("GET", "/admin", admin, nil).Body.String())
	if !strings.Contains(panel, "Stored photos and avatars: 0, using 0.0 MB of the 2048 MB limit (<code>IMAGE_STORAGE_LIMIT_MB</code>)") {
		t.Fatalf("empty:\n%s", panel)
	}
	if w := e.upload("/account/avatar", admin, e.avatarParts(admin, smallJPEG(t))...); w.Code != 303 {
		t.Fatal(w.Code)
	}
	e.A.storageLimit = 100 << 20
	panel = scannerPanel(e.do("GET", "/admin", admin, nil).Body.String())
	if !regexp.MustCompile(`Stored photos and avatars: 1, using 0\.\d MB of the 100 MB limit`).MatchString(panel) {
		t.Fatalf("one avatar:\n%s", panel)
	}
}

func TestImageStorageLimitSetting(t *testing.T) {
	for v, want := range map[string]int64{"": 2048 << 20, "1": 1 << 20, " 500 ": 500 << 20, "1048576": 1 << 40} {
		if got, err := imageStorageLimitFromEnv(func(string) string { return v }); err != nil || got != want {
			t.Errorf("%q: %d %v", v, got, err)
		}
	}
	for _, v := range []string{"0", "-1", "2.5", "1048577", "2GB"} {
		if _, err := imageStorageLimitFromEnv(func(string) string { return v }); err == nil || !strings.Contains(err.Error(), "IMAGE_STORAGE_LIMIT_MB") {
			t.Errorf("%q accepted: %v", v, err)
		}
	}
}

// worstCasePNG is an upload at every cap at once: a 16-bit RGBA PNG estimated just under the 64 MiB decode
// limit (2800×2800×8 bytes), padded with a private chunk to the 4 MB byte cap.
var worstCasePNG = sync.OnceValue(func() []byte {
	const side = 2800
	var z bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&z, zlib.BestSpeed)
	row := make([]byte, 1+side*8)
	for y := range side {
		for x := range side { // opaque, one colour a row: compresses to little, decodes to 62.7 MB
			binary.BigEndian.PutUint16(row[1+x*8:], uint16(y*23))
			binary.BigEndian.PutUint16(row[1+x*8+2:], uint16(0xFFFF-y*23))
			binary.BigEndian.PutUint16(row[1+x*8+6:], 0xFFFF)
		}
		zw.Write(row)
	}
	zw.Close()
	ihdr := binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, side), side)
	ihdr = append(ihdr, 16, 6, 0, 0, 0)
	out := append([]byte(nil), pngMagic...)
	out = append(out, pngChunk("IHDR", ihdr)...)
	pad := make([]byte, uploadCap-len(out)-len(z.Bytes())-3*12-1024)
	rand.Read(pad)
	out = append(out, pngChunk("prVt", pad)...)
	out = append(out, pngChunk("IDAT", z.Bytes())...)
	return append(out, pngChunk("IEND", nil)...)
})

// D10: concurrent worst-case uploads stay inside the memory budget documented beside maxConns: at most two
// bodies are read (uploadWork) and one decode runs (imageDecodeWork) at a time.
func TestUploadConcurrentMaxSizeStaysInBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("decodes two 7.8-megapixel 16-bit PNGs")
	}
	e, _ := uploadEnv(t)
	photo := worstCasePNG()
	if len(photo) > uploadCap || len(photo) < uploadCap-4096 {
		t.Fatalf("fixture %d bytes", len(photo))
	}
	info, err := inspectImage(photo)
	if err != nil || info.decodeBytes < maxDecodeBytes*9/10 {
		t.Fatalf("fixture not worst case: %+v %v", info, err)
	}
	var sessions, listings []string
	for i := range 6 {
		id, s := e.user("budget_vendor_"+string(rune('a'+i)), "vendor")
		sessions, listings = append(sessions, s), append(listings, e.product(id, "physical"))
	}
	// The requests (4 MB each) are built before the baseline: only what the server allocates is measured.
	var reqs []*http.Request
	for i, s := range sessions {
		r, _ := uploadReq("/listings/photo", s, e.photoParts(s, listings[i], photo)...)
		reqs = append(reqs, r)
	}
	defer debug.SetGCPercent(debug.SetGCPercent(25))
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	stop, peak := make(chan struct{}), make(chan uint64)
	go func() {
		var m runtime.MemStats
		var top uint64
		for {
			select {
			case <-stop:
				peak <- top
				return
			case <-time.After(5 * time.Millisecond):
				runtime.ReadMemStats(&m)
				top = max(top, m.HeapInuse)
			}
		}
	}()
	var wg sync.WaitGroup
	codes := make([]int, len(reqs))
	for i, r := range reqs {
		wg.Go(func() {
			w := httptest.NewRecorder()
			e.A.ServeHTTP(w, r)
			codes[i] = w.Code
		})
	}
	wg.Wait()
	close(stop)
	grew := int64(<-peak) - int64(base.HeapInuse)
	stored := 0
	for _, c := range codes {
		switch c {
		case 303:
			stored++
		case 503: // uploadWork full
		default:
			t.Fatalf("codes %v", codes)
		}
	}
	// Two bodies (plus the fake clamd's copy of each stream), the decode, the resampled and encoded outputs.
	// One decode is live at a time, but the collector may not yet have freed the previous upload's decode when
	// the next begins, so the heap may briefly hold two. Without the slots all six would decode at once.
	budget := int64(2*uploadBodyLimit + 2*2*uploadCap + 2*maxDecodeBytes + 32<<20)
	t.Logf("stored %d of %d; heap grew %d MiB at peak (budget %d MiB)", stored, len(codes), grew>>20, budget>>20)
	if stored < 2 || e.imageCount() != stored {
		t.Fatalf("stored %d, rows %d", stored, e.imageCount())
	}
	if grew > budget {
		t.Fatalf("heap grew %d MiB, budget %d MiB", grew>>20, budget>>20)
	}
}
