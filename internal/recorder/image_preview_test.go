package recorder

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gen2brain/vpx/webp"
	"github.com/txsdx/cpa-plugin-request-recorder/internal/wire"
	"golang.org/x/image/bmp"
)

func TestWebPPreviewIsLossyBoundedAndOriginalDownloadIsUnchanged(t *testing.T) {
	e := testEngine(t)
	img := image.NewNRGBA(image.Rect(0, 0, 1200, 800))
	rng := rand.New(rand.NewSource(42))
	for y := range 800 {
		for x := range 1200 {
			img.SetNRGBA(x, y, color.NRGBA{uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(rng.Intn(256)), 255})
		}
	}
	var original bytes.Buffer
	check(t, png.Encode(&original, img))
	source := "data:image/png;base64," + base64.StdEncoding.EncodeToString(original.Bytes())
	r := recordWithImage("preview", source)
	check(t, e.store.Save(r))
	e.images.enqueue(r.ID)
	assets := awaitAssetsWithin(t, e.store, r.ID, 45*time.Second)
	asset := assets[0]
	e.images.cfg.PreviewMaxDimension = 600
	query := url.Values{"record_id": {r.ID}, "id": {asset.ID}, "variant": {"preview"}}
	preview := e.Management(wire.ManagementRequest{Method: "GET", Path: APIBase + "/image", Query: query}, nil)
	if preview.StatusCode != 200 || preview.Headers.Get("Content-Type") != "image/webp" {
		t.Fatalf("preview response: %d %s", preview.StatusCode, preview.Body)
	}
	config, err := webp.DecodeConfig(bytes.NewReader(preview.Body))
	check(t, err)
	if config.Width != 600 || config.Height != 400 || !bytes.Contains(preview.Body, []byte("VP8 ")) {
		t.Fatal("preview did not use resized lossy WebP", config)
	}
	if len(preview.Body) >= original.Len()/2 {
		t.Fatalf("preview did not save bandwidth: original=%d preview=%d", original.Len(), len(preview.Body))
	}
	chatView := e.Management(wire.ManagementRequest{Method: "GET", Path: APIBase + "/record", Query: url.Values{"id": {r.ID}, "view": {"chat"}}}, nil)
	if chatView.StatusCode != 200 || bytes.Contains(chatView.Body, []byte("base64,")) {
		t.Fatal("chat view transfers hidden original image data")
	}
	query.Set("variant", "original")
	download := e.Management(wire.ManagementRequest{Method: "GET", Path: APIBase + "/image", Query: query}, nil)
	if download.StatusCode != 200 || download.Headers.Get("Content-Type") != "image/png" || !bytes.Equal(download.Body, original.Bytes()) || !strings.Contains(download.Headers.Get("Content-Disposition"), "attachment;") {
		t.Fatal("original download changed")
	}
	query.Set("variant", "preview")
	cached := e.Management(wire.ManagementRequest{Method: "GET", Path: APIBase + "/image", Query: query}, nil)
	if !bytes.Equal(cached.Body, preview.Body) || e.images.previews.bytes != len(preview.Body) {
		t.Fatal("preview cache missing")
	}
	query.Set("record_id", "different-record")
	denied := e.Management(wire.ManagementRequest{Method: "GET", Path: APIBase + "/image", Query: query}, nil)
	if denied.StatusCode != 404 {
		t.Fatal("cache bypassed record ownership")
	}
	t.Logf("1200x800 PNG %d bytes -> 600x400 WebP %d bytes (%.1f%%)", original.Len(), len(preview.Body), 100*float64(len(preview.Body))/float64(original.Len()))
}

func TestPreviewSmallJPEGAndPNGAndCacheLimits(t *testing.T) {
	for _, format := range []string{"png", "jpeg", "gif", "bmp", "webp", "avif"} {
		var data bytes.Buffer
		img := image.NewNRGBA(image.Rect(0, 0, 3, 2))
		img.SetNRGBA(0, 0, color.NRGBA{255, 0, 0, 255})
		switch format {
		case "png":
			check(t, png.Encode(&data, img))
		case "jpeg":
			check(t, jpeg.Encode(&data, img, nil))
		case "gif":
			check(t, gif.Encode(&data, img, nil))
		case "bmp":
			check(t, bmp.Encode(&data, img))
		case "webp":
			check(t, webp.Encode(&data, img))
		case "avif":
			raw, err := base64.StdEncoding.DecodeString("AAAAIGZ0eXBhdmlmAAAAAGF2aWZtaWYxbWlhZk1BMUEAAAGGbWV0YQAAAAAAAAAhaGRscgAAAAAAAAAAcGljdAAAAAAAAAAAAAAAAAAAAAAOcGl0bQAAAAAAAQAAACxpbG9jAAAAAEQAAAIAAQAAAAEAAAHTAAAAMQACAAAAAQAAAa4AAAAlAAAAQmlpbmYAAAAAAAIAAAAaaW5mZQIAAAAAAQAAYXYwMUNvbG9yAAAAABppbmZlAgAAAAACAABhdjAxQWxwaGEAAAAAGmlyZWYAAAAAAAAADmF1eGwAAgABAAEAAADDaXBycAAAAJ1pcGNvAAAAFGlzcGUAAAAAAAAAAwAAAAIAAAAQcGl4aQAAAAADCAgIAAAADGF2MUOBIAAAAAAAE2NvbHJuY2x4AAIAAgACgAAAAA5waXhpAAAAAAEIAAAADGF2MUOBABwAAAAAOGF1eEMAAAAAdXJuOm1wZWc6bXBlZ0I6Y2ljcDpzeXN0ZW1zOmF1eGlsaWFyeTphbHBoYQAAAAAeaXBtYQAAAAAAAAACAAEEAQKDBAACBAEFhgcAAABebWRhdBIACgUYBCsKgDIaGAAooQBHLDpo7a1SJr9lAR21QTr1AAAAABASAAoFOAQrRIAyJhlCYwTDzzzzQSAAkEER6WnCO4cjv2UirPdnIAAANCN+/m62E191")
			check(t, err)
			data.Write(raw)
		}
		preview, err := encodePreview(data.Bytes(), 75, 1600)
		check(t, err)
		config, err := webp.DecodeConfig(bytes.NewReader(preview))
		check(t, err)
		if config.Width != 3 || config.Height != 2 {
			t.Fatal("small image was enlarged", config)
		}
	}
	c := newPreviewCache()
	for i := 0; i < previewCacheEntries+10; i++ {
		c.put(string(rune(i)), make([]byte, 1<<20))
	}
	if c.bytes > previewCacheLimit || c.lru.Len() > previewCacheEntries || c.get(string(rune(0))) != nil {
		t.Fatal("preview cache is unbounded")
	}
	if _, err := encodePreview([]byte("not an image"), 75, 1600); err == nil {
		t.Fatal("accepted malformed preview")
	}
	// A header-only BMP with enormous dimensions must be rejected before decode.
	bmp := make([]byte, 54)
	copy(bmp, "BM")
	binary.LittleEndian.PutUint32(bmp[10:], 54)
	binary.LittleEndian.PutUint32(bmp[14:], 40)
	binary.LittleEndian.PutUint32(bmp[18:], 100000)
	binary.LittleEndian.PutUint32(bmp[22:], 100000)
	binary.LittleEndian.PutUint16(bmp[26:], 1)
	binary.LittleEndian.PutUint16(bmp[28:], 24)
	if _, err := encodePreview(bmp, 75, 1600); err == nil {
		t.Fatal("accepted oversized preview")
	}
}
