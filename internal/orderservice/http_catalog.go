package orderservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nguyenhoangkhang-2010/raftlog/internal/catalog"
	"github.com/nguyenhoangkhang-2010/raftlog/internal/kitchen/rbac"
)

// maxCatalogBody bounds a document of the menu or of the promotions: far more than a venue's menu.
const maxCatalogBody = 512 << 10

// WithCatalog turns on the editing of the menu and the promotions, their history, and the photos
// of the items, with the versions and the photos kept in the store.
func WithCatalog(store *catalog.Store) APIOption {
	return func(a *API) { a.cat = store }
}

func (a *API) catalogRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/images/{id}", a.photo)

	mux.HandleFunc("GET /api/admin/menu", a.can(rbac.ManageMenu, a.getDoc(catalog.Menu)))
	mux.HandleFunc("PUT /api/admin/menu", a.can(rbac.ManageMenu, a.putDoc(catalog.Menu)))
	mux.HandleFunc("GET /api/admin/menu/versions", a.can(rbac.ManageMenu, a.listVersions(catalog.Menu)))
	mux.HandleFunc("GET /api/admin/menu/versions/{n}", a.can(rbac.ManageMenu, a.getVersion(catalog.Menu)))
	mux.HandleFunc("POST /api/admin/menu/versions/{n}/restore", a.can(rbac.ManageMenu, a.restoreVersion(catalog.Menu)))
	mux.HandleFunc("PUT /api/admin/menu/items/{id}/image", a.can(rbac.ManageMenu, a.putPhoto))
	mux.HandleFunc("DELETE /api/admin/menu/items/{id}/image", a.can(rbac.ManageMenu, a.deletePhoto))

	mux.HandleFunc("GET /api/admin/promotions", a.can(rbac.ManagePromos, a.getDoc(catalog.Promotions)))
	mux.HandleFunc("PUT /api/admin/promotions", a.can(rbac.ManagePromos, a.putDoc(catalog.Promotions)))
	mux.HandleFunc("GET /api/admin/promotions/versions", a.can(rbac.ManagePromos, a.listVersions(catalog.Promotions)))
	mux.HandleFunc("GET /api/admin/promotions/versions/{n}", a.can(rbac.ManagePromos, a.getVersion(catalog.Promotions)))
	mux.HandleFunc("POST /api/admin/promotions/versions/{n}/restore", a.can(rbac.ManagePromos, a.restoreVersion(catalog.Promotions)))
}

// can is authID for the handlers that need a permission.
func (a *API) can(p rbac.Permission, f func(http.ResponseWriter, *http.Request, Identity)) http.HandlerFunc {
	return a.authID(func(w http.ResponseWriter, r *http.Request, id Identity) {
		if !id.Actor.Can(p) {
			a.fail(w, fmt.Errorf("%w: needs %s", ErrNotAllowed, p))
			return
		}
		f(w, r, id)
	})
}

// failCatalog answers an error of the catalog, or of the checks of a document.
func (a *API) failCatalog(w http.ResponseWriter, err error) {
	var inv *catalog.InvalidError
	var bad *documentError
	body := errorBody{Error: strings.TrimPrefix(err.Error(), "catalog: ")}
	var code int // every case below sets it
	switch {
	case errors.Is(err, catalog.ErrStale):
		body.Code, code = "stale", http.StatusConflict
		body.Error = "Someone else changed this since you opened it. Read it again, and make your change on that."
	case errors.As(err, &bad):
		body.Code, code = "invalid_document", http.StatusBadRequest
	case errors.As(err, &inv):
		body.Code, code = "invalid", http.StatusBadRequest
	case errors.Is(err, catalog.ErrNoVersion), errors.Is(err, catalog.ErrNoImage):
		body.Code, code = "not_found", http.StatusNotFound
	default:
		a.logger.Error("a catalog request failed", "err", err)
		w.Header().Set("Retry-After", "2")
		body.Code, code, body.Error = "unavailable", http.StatusServiceUnavailable, "the database did not answer"
	}
	writeJSON(w, code, body)
}

// documentError says that a document does not pass the checks that the service makes when it
// reads one from a file, and why: the message is the one of those checks, as it would be at start.
type documentError struct{ err error }

func (e *documentError) Error() string { return e.err.Error() }
func (e *documentError) Unwrap() error { return e.err }

// apply checks a document with the code that reads the files, and puts it in use. It returns an
// error before anything is changed if the document is not good.
type applier func(doc []byte) (commit func() error, err error)

func (a *API) applier(kind string) applier {
	if kind == catalog.Menu {
		return func(doc []byte) (func() error, error) {
			m, low, err := ParseMenu(doc)
			if err != nil {
				return nil, &documentError{err}
			}
			return func() error { return a.svc.SetMenu(m, low) }, nil
		}
	}
	return func(doc []byte) (func() error, error) {
		p, err := ParsePromotions(doc)
		if err != nil {
			return nil, &documentError{err}
		}
		return func() error { return a.svc.SetPromotions(p) }, nil
	}
}

// --- documents -----------------------------------------------------------------------------

type versionView struct {
	Version int       `json:"version"`
	By      string    `json:"by"`
	At      time.Time `json:"at"`
	Note    string    `json:"note"`
}

func versionOf(v catalog.Version) versionView {
	return versionView{Version: v.Number, By: v.By, At: v.At, Note: v.Note}
}

func (a *API) getDoc(kind string) func(http.ResponseWriter, *http.Request, Identity) {
	return func(w http.ResponseWriter, r *http.Request, _ Identity) {
		v, ok, err := a.cat.Latest(r.Context(), kind)
		if err != nil {
			a.failCatalog(w, err)
			return
		}
		if !ok {
			a.failCatalog(w, catalog.ErrNoVersion)
			return
		}
		out := map[string]any{"version": versionOf(v), "doc": json.RawMessage(v.Doc)}
		if kind == catalog.Menu {
			out["ingredients"] = a.svc.Ingredients()
			if h, err := a.cat.ImageHashes(r.Context()); err == nil {
				out["images"] = h
			}
		}
		writeJSON(w, http.StatusOK, out)
	}
}

type saveBody struct {
	Base int             `json:"base_version"`
	Note string          `json:"note"`
	Doc  json.RawMessage `json:"doc"`
}

func readSave(w http.ResponseWriter, r *http.Request) (saveBody, error) {
	var b saveBody
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCatalogBody))
	if err != nil {
		return b, &catalog.InvalidError{Reason: "the request is too large or could not be read"}
	}
	if err := decodeStrict(raw, &b); err != nil {
		return b, &catalog.InvalidError{Reason: "the request is not what was expected: " + err.Error()}
	}
	if len(b.Doc) == 0 {
		return b, &catalog.InvalidError{Reason: "the request has no document"}
	}
	return b, nil
}

// save checks a document, stores it as the next version, and puts it in use. If the store
// accepts it and putting it in use fails (it was checked, so that would be a bug), the error is
// reported and the version stays in the history: the service uses the one it had until the next start.
func (a *API) save(ctx context.Context, kind string, base int, doc []byte, by, note string) (catalog.Version, error) {
	commit, err := a.applier(kind)(doc)
	if err != nil {
		return catalog.Version{}, err
	}
	v, err := a.cat.Save(ctx, kind, base, doc, by, note)
	if err != nil {
		return catalog.Version{}, err
	}
	if err := commit(); err != nil {
		return catalog.Version{}, err
	}
	return v, nil
}

func (a *API) putDoc(kind string) func(http.ResponseWriter, *http.Request, Identity) {
	return func(w http.ResponseWriter, r *http.Request, id Identity) {
		b, err := readSave(w, r)
		if err != nil {
			a.failCatalog(w, err)
			return
		}
		v, err := a.save(r.Context(), kind, b.Base, b.Doc, id.Actor.ID, b.Note)
		if err != nil {
			a.failCatalog(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"version": versionOf(v)})
	}
}

func (a *API) listVersions(kind string) func(http.ResponseWriter, *http.Request, Identity) {
	return func(w http.ResponseWriter, r *http.Request, _ Identity) {
		list, err := a.cat.List(r.Context(), kind, 100)
		if err != nil {
			a.failCatalog(w, err)
			return
		}
		out := make([]versionView, 0, len(list))
		for _, v := range list {
			out = append(out, versionOf(v))
		}
		writeJSON(w, http.StatusOK, map[string]any{"versions": out})
	}
}

func versionNumber(r *http.Request) (int, error) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 {
		return 0, &catalog.InvalidError{Reason: "a version is a number from 1"}
	}
	return n, nil
}

func (a *API) getVersion(kind string) func(http.ResponseWriter, *http.Request, Identity) {
	return func(w http.ResponseWriter, r *http.Request, _ Identity) {
		n, err := versionNumber(r)
		if err != nil {
			a.failCatalog(w, err)
			return
		}
		v, err := a.cat.Get(r.Context(), kind, n)
		if err != nil {
			a.failCatalog(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"version": versionOf(v), "doc": json.RawMessage(v.Doc)})
	}
}

// restoreVersion saves an old version again as a new one, after the others: the history is never
// rewritten. The old document is checked again, because the rules may have changed since.
func (a *API) restoreVersion(kind string) func(http.ResponseWriter, *http.Request, Identity) {
	return func(w http.ResponseWriter, r *http.Request, id Identity) {
		n, err := versionNumber(r)
		if err != nil {
			a.failCatalog(w, err)
			return
		}
		var b struct {
			Base int `json:"base_version"`
		}
		raw, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err := decodeStrict(raw, &b); err != nil {
			a.failCatalog(w, &catalog.InvalidError{Reason: "the request is not what was expected: " + err.Error()})
			return
		}
		old, err := a.cat.Get(r.Context(), kind, n)
		if err != nil {
			a.failCatalog(w, err)
			return
		}
		v, err := a.save(r.Context(), kind, b.Base, old.Doc, id.Actor.ID, fmt.Sprintf("restored version %d", n))
		if err != nil {
			a.failCatalog(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"version": versionOf(v), "doc": json.RawMessage(old.Doc)})
	}
}

// --- photos --------------------------------------------------------------------------------

// photo serves the photo of an item. Anyone may see it, like the menu. The address carries the
// hash of the photo, so the browser may keep it for good; and a photo never runs anything.
func (a *API) photo(w http.ResponseWriter, r *http.Request) {
	im, err := a.cat.Image(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, catalog.ErrNoImage) {
			http.NotFound(w, r)
			return
		}
		a.failCatalog(w, err)
		return
	}
	etag := `"` + im.Hash + `"`
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Content-Type", im.ContentType)
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(im.Data)
}

func (a *API) putPhoto(w http.ResponseWriter, r *http.Request, id Identity) {
	item := r.PathValue("id")
	if !a.svc.HasItem(item) {
		a.failCatalog(w, catalog.ErrNoImage) // no such item: not found
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, catalog.MaxPhotoBytes+1))
	if err != nil {
		a.failCatalog(w, &catalog.InvalidError{Reason: fmt.Sprintf("a photo is at most %d KB", catalog.MaxPhotoBytes/1024)})
		return
	}
	if _, err := a.cat.PutImage(r.Context(), item, data, id.Actor.ID); err != nil {
		a.failCatalog(w, err)
		return
	}
	a.refreshPhotos(r.Context(), w)
}

func (a *API) deletePhoto(w http.ResponseWriter, r *http.Request, _ Identity) {
	if err := a.cat.DeleteImage(r.Context(), r.PathValue("id")); err != nil {
		a.failCatalog(w, err)
		return
	}
	a.refreshPhotos(r.Context(), w)
}

// refreshPhotos tells the service which items have a photo now, and answers with them.
func (a *API) refreshPhotos(ctx context.Context, w http.ResponseWriter) {
	hashes, err := a.cat.ImageHashes(ctx)
	if err != nil {
		a.failCatalog(w, err)
		return
	}
	a.svc.SetImages(hashes)
	writeJSON(w, http.StatusOK, map[string]any{"images": hashes})
}
