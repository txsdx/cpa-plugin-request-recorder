package recorder

import (
	"bytes"
	"container/list"
	"errors"
	"fmt"
	"image"
	"sync"

	_ "github.com/gen2brain/avif"
	"github.com/gen2brain/vpx/webp"
	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
)

const previewCacheLimit = 32 << 20
const previewCacheEntries = 64
const previewPixelLimit = 32_000_000

// Disposable compressed previews stay in bounded memory, never in the DB or
// original image directory. One encoder bounds simultaneous decode work.
type previewCache struct {
	mu     sync.Mutex
	items  map[string]*list.Element
	lru    *list.List
	bytes  int
	worker chan struct{}
}
type previewEntry struct {
	key  string
	data []byte
}

func newPreviewCache() *previewCache {
	return &previewCache{items: map[string]*list.Element{}, lru: list.New(), worker: make(chan struct{}, 1)}
}
func (c *previewCache) get(key string) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.items[key]; entry != nil {
		c.lru.MoveToFront(entry)
		return entry.Value.(previewEntry).data
	}
	return nil
}
func (c *previewCache) put(key string, data []byte) {
	if len(data) > previewCacheLimit {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items[key] != nil {
		return
	}
	for c.bytes+len(data) > previewCacheLimit || c.lru.Len() >= previewCacheEntries {
		old := c.lru.Back()
		entry := old.Value.(previewEntry)
		delete(c.items, entry.key)
		c.bytes -= len(entry.data)
		c.lru.Remove(old)
	}
	c.items[key] = c.lru.PushFront(previewEntry{key, data})
	c.bytes += len(data)
}

func (a *imageArchive) preview(asset ImageAsset) ([]byte, error) {
	if err := a.ctx.Err(); err != nil {
		return nil, err
	}
	if !validSHA256(asset.Hash) {
		return nil, errors.New("invalid image hash")
	}
	key := fmt.Sprintf("%s/%d/%d", asset.Hash, a.cfg.PreviewQuality, a.cfg.PreviewMaxDimension)
	if data := a.previews.get(key); data != nil {
		return data, nil
	}
	select {
	case a.previews.worker <- struct{}{}:
		defer func() { <-a.previews.worker }()
	default:
		return nil, errImageBusy
	}
	if data := a.previews.get(key); data != nil {
		return data, nil
	}
	data, err := a.read(asset)
	if err != nil {
		return nil, err
	}
	preview, err := encodePreview(data, a.cfg.PreviewQuality, a.cfg.PreviewMaxDimension)
	if err != nil {
		return nil, err
	}
	if err := a.ctx.Err(); err != nil {
		return nil, err
	}
	a.previews.put(key, preview)
	return preview, nil
}

func encodePreview(data []byte, quality, maxDimension int) ([]byte, error) {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("图片预览无法读取；仍可下载原图")
	}
	if config.Width < 1 || config.Height < 1 || config.Width > previewPixelLimit || config.Height > previewPixelLimit || int64(config.Width)*int64(config.Height) > previewPixelLimit {
		return nil, errors.New("图片超过预览解码尺寸限制（3200 万像素）；请下载原图")
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("图片预览无法解码；仍可下载原图")
	}
	width, height := img.Bounds().Dx(), img.Bounds().Dy()
	canvas := image.Rect(0, 0, config.Width, config.Height)
	if !img.Bounds().In(canvas) || width < 1 || height < 1 {
		return nil, errors.New("图片尺寸与头部不一致；请下载原图")
	}
	if img.Bounds() != canvas {
		// GIF's first frame can occupy only part of its logical canvas.
		composited := image.NewNRGBA(canvas)
		draw.Draw(composited, img.Bounds(), img, img.Bounds().Min, draw.Src)
		img = composited
		width, height = config.Width, config.Height
	}
	if width > maxDimension || height > maxDimension {
		if width >= height {
			height = max(1, height*maxDimension/width)
			width = maxDimension
		} else {
			width = max(1, width*maxDimension/height)
			height = maxDimension
		}
		resized := image.NewNRGBA(image.Rect(0, 0, width, height))
		draw.ApproxBiLinear.Scale(resized, resized.Bounds(), img, img.Bounds(), draw.Src, nil)
		img = resized
	}
	var out bytes.Buffer
	if err := webp.Encode(&out, img, webp.EncodeOptions{Quality: quality, Method: 3, Threads: 1}); err != nil {
		return nil, errors.New("WebP 预览压缩失败；仍可下载原图")
	}
	return out.Bytes(), nil
}
