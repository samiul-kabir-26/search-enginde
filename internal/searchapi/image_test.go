package searchapi

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/zavisoft/search-engine/internal/auth"
	"github.com/zavisoft/search-engine/internal/platform"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{200, uint8(x), uint8(y), 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func (h *harness) imageSearch(t *testing.T, tok string, img []byte, fields map[string]string) (int, map[string]any, platform.Problem) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if img != nil {
		fw, _ := mw.CreateFormFile("image", "query.png")
		_, _ = fw.Write(img)
	}
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	_ = mw.Close()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, h.srv.URL+"/v1/search/image", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	var pr platform.Problem
	if resp.StatusCode == http.StatusOK {
		_ = json.Unmarshal(b, &out)
	} else {
		_ = json.Unmarshal(b, &pr)
	}
	return resp.StatusCode, out, pr
}

func TestImageSearch(t *testing.T) {
	h := newHarness(t)
	tok := h.token(t, auth.ScopeSearchRead)
	img := pngBytes(t, 64, 48)

	code, out, pr := h.imageSearch(t, tok, img, map[string]string{"page": `{"size":2}`, "q": "red bag"})
	if code != http.StatusOK {
		t.Fatalf("code %d %+v", code, pr)
	}
	if _, ok := out["predicted_categories"]; !ok || out["next_cursor"] == nil || len(out["results"].([]any)) != 2 {
		t.Fatalf("response = %v", out)
	}
	if out["facets"] == nil {
		t.Error("refinement text must produce facets")
	}
	runs := h.engine.runs
	code, page2, _ := h.imageSearch(t, tok, img, map[string]string{"page": `{"size":2,"cursor":"` + out["next_cursor"].(string) + `"}`, "q": "red bag"})
	if code != http.StatusOK || h.engine.runs != runs {
		t.Errorf("cursor paging must reuse the snapshot: code %d runs %d->%d", code, runs, h.engine.runs)
	}
	if page2["offset"].(float64) != 2 {
		t.Errorf("page 2 offset = %v", page2["offset"])
	}

	code, noText, _ := h.imageSearch(t, tok, img, map[string]string{"crop": `{"x":0,"y":0,"w":32,"h":32}`})
	if code != http.StatusOK || noText["facets"] != nil {
		t.Errorf("image-only search: code %d facets %v", code, noText["facets"])
	}
}

func TestImageSearchRejects(t *testing.T) {
	h := newHarness(t)
	tok := h.token(t, auth.ScopeSearchRead)
	img := pngBytes(t, 32, 32)
	cases := map[string]struct {
		img    []byte
		fields map[string]string
		code   int
		path   string
	}{
		"missing image": {nil, nil, http.StatusBadRequest, "/image"},
		"gif":           {[]byte("GIF89a\x01\x00\x01\x00\x00\x00\x00"), nil, http.StatusUnsupportedMediaType, "/image"},
		"truncated":     {img[:30], nil, http.StatusBadRequest, "/image"},
		"too large":     {make([]byte, MaxImageRequestBytes+10), nil, http.StatusRequestEntityTooLarge, "/image"},
		"crop outside":  {img, map[string]string{"crop": `{"x":30,"y":0,"w":10,"h":10}`}, http.StatusBadRequest, "/crop"},
		"bad crop json": {img, map[string]string{"crop": `{"x":"a"}`}, http.StatusBadRequest, "/crop"},
		"bad filter":    {img, map[string]string{"filters": `{"field":"title","op":"eq","value":"x"}`}, http.StatusBadRequest, "/filters/field"},
		"bad explain":   {img, map[string]string{"explain": "maybe"}, http.StatusBadRequest, "/explain"},
		"two modes":     {img, map[string]string{"page": `{"number":1,"offset":0}`}, http.StatusBadRequest, "/page"},
	}
	for name, tc := range cases {
		code, _, pr := h.imageSearch(t, tok, tc.img, tc.fields)
		found := false
		for _, e := range pr.Errors {
			found = found || e.Path == tc.path
		}
		if code != tc.code || !found {
			t.Errorf("%s: code %d errors %+v, want %d at %s", name, code, pr.Errors, tc.code, tc.path)
		}
	}
}

func TestSimilar(t *testing.T) {
	h := newHarness(t)
	tok := h.token(t, auth.ScopeSearchRead)
	get := func(path string) (int, SearchResponse) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, h.srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var sr SearchResponse
		_ = json.NewDecoder(resp.Body).Decode(&sr)
		return resp.StatusCode, sr
	}
	code, sr := get("/v1/products/p01/similar?size=3")
	if code != http.StatusOK || len(sr.Results) == 0 || len(sr.Results) > 3 || sr.NextCursor != nil {
		t.Fatalf("code %d results %+v", code, sr.Results)
	}
	for _, r := range sr.Results {
		if r.ProductID == "p01" {
			t.Error("the anchor must be excluded")
		}
		if r.ProductID == "p08" || r.ProductID == "p03" {
			t.Errorf("%s violates always_filters", r.ProductID)
		}
	}
	if sr.Results[0].ProductID != "p02" && sr.Results[0].ProductID != "p10" {
		t.Errorf("most similar to the red leather handbag = %s", sr.Results[0].ProductID)
	}
	if code, _ := get("/v1/products/nope/similar"); code != http.StatusNotFound {
		t.Errorf("unknown product: %d", code)
	}
	if code, _ := get("/v1/products/p01/similar?size=0"); code != http.StatusBadRequest {
		t.Errorf("bad size: %d", code)
	}
}
