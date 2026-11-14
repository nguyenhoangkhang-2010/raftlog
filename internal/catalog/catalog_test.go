package catalog

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgmigrate"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/pgtest"
)

var ctx = context.Background()

func newStore(t *testing.T) *Store {
	t.Helper()
	pool := pgtest.Pool(t)
	if _, err := pgmigrate.Apply(ctx, pool, Migrations(), pgmigrate.WithTable(MigrationsTable)); err != nil {
		t.Fatal(err)
	}
	return New(pool, nil)
}

func picture(t *testing.T, format string, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 120, 255})
		}
	}
	var b bytes.Buffer
	var err error
	if format == "png" {
		err = png.Encode(&b, img)
	} else {
		err = jpeg.Encode(&b, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestADocumentIsSavedAsVersionsAndTheNewestIsInUse(t *testing.T) {
	s := newStore(t)
	if _, ok, err := s.Latest(ctx, Menu); err != nil || ok {
		t.Fatalf("an empty store has a menu: %v %v", ok, err)
	}
	v1, err := s.Save(ctx, Menu, 0, []byte(`{"items":[]}`), "quan-ly", "first")
	if err != nil || v1.Number != 1 {
		t.Fatalf("first save: %+v %v", v1, err)
	}
	v2, err := s.Save(ctx, Menu, 1, []byte(`{"items":[{"id":"x"}]}`), "quan-ly", "added x")
	if err != nil || v2.Number != 2 {
		t.Fatalf("second save: %+v %v", v2, err)
	}
	cur, ok, err := s.Latest(ctx, Menu)
	if err != nil || !ok || cur.Number != 2 || !strings.Contains(string(cur.Doc), `"x"`) || cur.By != "quan-ly" || cur.Note != "added x" {
		t.Fatalf("latest: %+v %v %v", cur, ok, err)
	}
	old, err := s.Get(ctx, Menu, 1)
	if err != nil || strings.Contains(string(old.Doc), `"x"`) {
		t.Fatalf("version 1: %+v %v", old, err)
	}
	if _, err := s.Get(ctx, Menu, 9); !errors.Is(err, ErrNoVersion) {
		t.Fatalf("a version that does not exist: %v", err)
	}
	list, err := s.List(ctx, Menu, 10)
	if err != nil || len(list) != 2 || list[0].Number != 2 || len(list[0].Doc) != 0 {
		t.Fatalf("list: %+v %v", list, err)
	}
	// The menu and the promotions are numbered apart.
	p, err := s.Save(ctx, Promotions, 0, []byte(`[]`), "quan-ly", "")
	if err != nil || p.Number != 1 {
		t.Fatalf("promotions: %+v %v", p, err)
	}
}

func TestASaveThatStartedFromAnOldVersionIsRefused(t *testing.T) {
	s := newStore(t)
	if _, err := s.Save(ctx, Menu, 0, []byte(`{}`), "a", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, Menu, 1, []byte(`{"v":2}`), "a", ""); err != nil {
		t.Fatal(err)
	}
	for _, base := range []int{0, 1, 5} { // an old one, and one from the future
		if _, err := s.Save(ctx, Menu, base, []byte(`{"v":3}`), "b", ""); !errors.Is(err, ErrStale) {
			t.Errorf("a save from version %d gave %v", base, err)
		}
	}
	if cur, _, _ := s.Latest(ctx, Menu); cur.Number != 2 {
		t.Fatalf("a refused save changed the menu: version %d", cur.Number)
	}
}

func TestTwoSavesFromTheSameVersionLeaveExactlyOne(t *testing.T) {
	s := newStore(t)
	if _, err := s.Save(ctx, Menu, 0, []byte(`{}`), "a", ""); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 20; round++ {
		base := round + 1
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = s.Save(ctx, Menu, base, []byte(`{"r":1}`), "p", "")
			}()
		}
		wg.Wait()
		ok, stale := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrStale):
				stale++
			default:
				t.Fatalf("round %d: %v", round, err)
			}
		}
		if ok != 1 || stale != 1 {
			t.Fatalf("round %d: %d saved and %d were refused; want one of each", round, ok, stale)
		}
	}
}

func TestTheHistoryCannotBeChanged(t *testing.T) {
	s := newStore(t)
	if _, err := s.Save(ctx, Menu, 0, []byte(`{}`), "a", ""); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`UPDATE catalog_versions SET created_by = 'someone else'`, `DELETE FROM catalog_versions`, `TRUNCATE catalog_versions`} {
		if _, err := s.pool.Exec(ctx, q); err == nil {
			t.Errorf("%q was allowed", q)
		}
	}
}

func TestAnythingThatIsNotAValidSaveIsRefused(t *testing.T) {
	s := newStore(t)
	var inv *InvalidError
	for name, c := range map[string]struct {
		kind, doc, by, note string
	}{
		"a kind that does not exist": {"drinks", `{}`, "a", ""},
		"not JSON":                   {Menu, `{"items":`, "a", ""},
		"nobody saving":              {Menu, `{}`, "", ""},
		"a long note":                {Menu, `{}`, "a", strings.Repeat("n", 201)},
		"a note that is not UTF-8":   {Menu, `{}`, "a", "\xff\xfe"},
	} {
		if _, err := s.Save(ctx, c.kind, 0, []byte(c.doc), c.by, c.note); !errors.As(err, &inv) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, ok, _ := s.Latest(ctx, Menu); ok {
		t.Fatal("a refused save left a version")
	}
}

func TestAPhotoIsKeptAndFoundAndItsTypeIsReadFromItsBytes(t *testing.T) {
	s := newStore(t)
	jpg := picture(t, "jpeg", 200, 150)
	hash, err := s.PutImage(ctx, "ca-phe-sua", jpg, "quan-ly")
	if err != nil || len(hash) != 64 {
		t.Fatalf("put: %q %v", hash, err)
	}
	im, err := s.Image(ctx, "ca-phe-sua")
	if err != nil || im.ContentType != "image/jpeg" || !bytes.Equal(im.Data, jpg) || im.Hash != hash {
		t.Fatalf("image: %+v %v", im, err)
	}
	// A new photo replaces the old, and has another hash.
	png2 := picture(t, "png", 100, 100)
	hash2, err := s.PutImage(ctx, "ca-phe-sua", png2, "quan-ly")
	if err != nil || hash2 == hash {
		t.Fatalf("replace: %q %v", hash2, err)
	}
	if im, _ := s.Image(ctx, "ca-phe-sua"); im.ContentType != "image/png" {
		t.Fatalf("the type after replacing: %s", im.ContentType)
	}
	hashes, err := s.ImageHashes(ctx)
	if err != nil || len(hashes) != 1 || hashes["ca-phe-sua"] != hash2 {
		t.Fatalf("hashes: %v %v", hashes, err)
	}
	if err := s.DeleteImage(ctx, "ca-phe-sua"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Image(ctx, "ca-phe-sua"); !errors.Is(err, ErrNoImage) {
		t.Fatalf("after delete: %v", err)
	}
	if err := s.DeleteImage(ctx, "ca-phe-sua"); err != nil {
		t.Fatalf("deleting a photo that is not there: %v", err)
	}
}

func TestWhatIsNotAPhotoIsRefused(t *testing.T) {
	s := newStore(t)
	big := picture(t, "png", 200, 200)
	huge := append(append([]byte{}, big...), bytes.Repeat([]byte{0}, MaxPhotoBytes)...)
	for name, data := range map[string][]byte{
		"nothing":                      nil,
		"text":                         []byte("<html><script>alert(1)</script></html>"),
		"an SVG, which can run script": []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="200" height="200"><script>alert(1)</script></svg>`),
		"a GIF header":                 []byte("GIF89a\x01\x00\x01\x00"),
		"a truncated JPEG":             picture(t, "jpeg", 200, 200)[:100],
		"too small":                    picture(t, "png", 10, 10),
		"too large in bytes":           huge,
	} {
		var inv *InvalidError
		if _, err := s.PutImage(ctx, "x-item", data, "a"); !errors.As(err, &inv) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A bad item id is refused by the database, and said so.
	var inv *InvalidError
	if _, err := s.PutImage(ctx, "Not An Id!", big, "a"); !errors.As(err, &inv) {
		t.Errorf("a bad item id: %v", err)
	}
	if hashes, _ := s.ImageHashes(ctx); len(hashes) != 0 {
		t.Fatalf("refused photos were stored: %v", hashes)
	}
}

func TestACheckOfAPhotoDoesNotDependOnTheClock(t *testing.T) {
	// CheckImage reads bytes and nothing else; this keeps it so.
	start := time.Now()
	for i := 0; i < 50; i++ {
		if ct, err := CheckImage(picture(t, "png", 80, 80)); err != nil || ct != "image/png" {
			t.Fatal(ct, err)
		}
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("checking fifty small photos took more than five seconds")
	}
}
