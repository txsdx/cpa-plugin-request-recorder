package recorder

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type ImageAsset struct {
	ID        string `json:"id"`
	SourceURL string `json:"source_url,omitempty"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	Hash      string `json:"hash,omitempty"`
	MIME      string `json:"mime,omitempty"`
	Size      int64  `json:"size,omitempty"`
}

func imageID(source string) string {
	sum := sha256.Sum256([]byte(source))
	return hex.EncodeToString(sum[:])
}
func validSHA256(id string) bool {
	if len(id) != 64 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && id == strings.ToLower(id)
}
func imageDirectory(cfg Config) string {
	if cfg.ImagesDir != "" {
		return cfg.ImagesDir
	}
	base := filepath.Base(cfg.DBPath)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	return filepath.Join(filepath.Dir(cfg.DBPath), base+"-images")
}

var cacheImageName = regexp.MustCompile(`^[a-f0-9]{64}\.img$`)
var cacheTempName = regexp.MustCompile(`^\.rr-tmp-[a-f0-9]{32}$`)

type imageArchive struct {
	sizes      map[string]int64
	store      *Store
	root       *os.Root
	cfg        Config
	fetcher    *imageFetcher
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	queue      chan string
	jobsMu     sync.Mutex
	jobs       map[string]bool // false queued; true dirty while currently processing
	processing map[string]bool
	retries    map[string]bool
	filesMu    sync.Mutex
	used       int64
	serving    chan struct{}
}

func newImageArchive(cfg Config, store *Store) (*imageArchive, error) {
	dir := imageDirectory(cfg)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &imageArchive{sizes: map[string]int64{}, store: store, root: root, cfg: cfg, fetcher: newImageFetcher(), ctx: ctx, cancel: cancel, queue: make(chan string, 256), jobs: map[string]bool{}, processing: map[string]bool{}, retries: map[string]bool{}, serving: make(chan struct{}, 2)}
	f, err := root.Open(".")
	if err != nil {
		root.Close()
		cancel()
		return nil, err
	}
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		root.Close()
		cancel()
		return nil, err
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			if cacheTempName.MatchString(entry.Name()) {
				_ = root.Remove(entry.Name())
				continue
			}
			if cacheImageName.MatchString(entry.Name()) {
				if info, err := entry.Info(); err == nil {
					a.used += info.Size()
					a.sizes[strings.TrimSuffix(entry.Name(), ".img")] = info.Size()
				}
			}
		}
	}
	return a, nil
}
func (a *imageArchive) start() {
	for i := 0; i < a.cfg.ImageWorkers; i++ {
		a.wg.Add(1)
		go a.worker()
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-a.ctx.Done():
				return
			case <-ticker.C:
				_ = a.collect(time.Now().Add(-5 * time.Minute))
				a.resume()
			}
		}
	}()
	a.resume()
}
func (a *imageArchive) resume() {
	ids, _ := a.store.PendingImageRecords(256)
	for _, id := range ids {
		if !a.pending(id) {
			a.enqueue(id)
		}
	}
}
func (a *imageArchive) close() error { a.cancel(); a.wg.Wait(); return a.root.Close() }
func (a *imageArchive) enqueue(id string) bool {
	a.jobsMu.Lock()
	defer a.jobsMu.Unlock()
	if a.ctx.Err() != nil {
		return false
	}
	if err := a.store.QueueImages(id); err != nil {
		return false
	}
	if _, exists := a.jobs[id]; exists {
		if a.processing[id] {
			a.jobs[id] = true
		}
		return true
	}
	select {
	case a.queue <- id:
		a.jobs[id] = false
		return true
	default:
		return false
	}
}
func (a *imageArchive) worker() {
	defer a.wg.Done()
	for {
		select {
		case <-a.ctx.Done():
			return
		case id := <-a.queue:
			a.jobsMu.Lock()
			a.processing[id] = true
			a.jobsMu.Unlock()
			complete := a.process(id)
			a.jobsMu.Lock()
			dirty := a.jobs[id]
			if complete && !dirty {
				_ = a.store.FinishImages(id)
			}
			delete(a.jobs, id)
			delete(a.processing, id)
			a.jobsMu.Unlock()
			if dirty {
				a.enqueue(id)
			} else if complete && len(a.queue) == 0 {
				a.resume()
			}
		}
	}
}

type imageSource struct{ id, url string }

func recordImageSources(r *Record, limit int) []imageSource {
	sources := []imageSource{}
	seen := map[string]bool{}
	msgs := Conversation(r.RequestBody, r.ResponseBody)
	if r.InterceptedBody != "" && r.InterceptedBody != r.RequestBody {
		msgs = append(msgs, Conversation(r.InterceptedBody, "")...)
	}
	// Archive images from every upstream attempt, even though the chat view
	// deliberately renders only the final attempt's output.
	for _, raw := range []string{r.UpstreamRequests, r.UpstreamResponses} {
		locs := sectionPattern.FindAllStringSubmatchIndex(raw, -1)
		for i, loc := range locs {
			end := len(raw)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			if _, body, ok := strings.Cut(raw[loc[1]:end], "Body:\n"); ok {
				name := raw[loc[2]:loc[3]]
				if strings.HasPrefix(name, "API REQUEST") {
					msgs = append(msgs, Conversation(strings.TrimSpace(body), "")...)
				} else if strings.HasPrefix(name, "API RESPONSE") {
					msgs = append(msgs, responseMessages(strings.TrimSpace(body))...)
				}
			}
		}
	}
	for _, m := range msgs {
		for _, img := range m.Images {
			if img.URL == "" {
				continue
			}
			id := imageID(img.URL)
			if seen[id] {
				continue
			}
			seen[id] = true
			sources = append(sources, imageSource{id, img.URL})
			if len(sources) >= limit {
				return sources
			}
		}
	}
	return sources
}
func (a *imageArchive) process(id string) bool {
	if a.ctx.Err() != nil {
		return false
	}
	a.jobsMu.Lock()
	force := a.retries[id]
	delete(a.retries, id)
	a.jobsMu.Unlock()
	r, err := a.store.Get(id)
	if err != nil {
		return false
	}
	sources := recordImageSources(r, a.cfg.MaxImagesPerRecord)
	existing := map[string]ImageAsset{}
	for _, im := range r.Images {
		existing[im.ID] = im
	}
	inventory := make([]ImageAsset, 0, len(sources))
	for _, src := range sources {
		asset, ok := existing[src.id]
		if !ok {
			asset = ImageAsset{ID: src.id, Status: "pending"}
			if !strings.HasPrefix(src.url, "data:") {
				asset.SourceURL = src.url
			}
		}
		if force && asset.Status != "ready" {
			asset.Status = "pending"
			asset.Error = ""
		}
		if asset.Status == "ready" && !a.exists(asset.Hash) {
			asset.Status = "pending"
			asset.Error = ""
		}
		inventory = append(inventory, asset)
	}
	if err := a.store.SetImages(id, inventory); err != nil {
		return false
	}
	for i, src := range sources {
		if a.ctx.Err() != nil {
			return false
		}
		if inventory[i].Status != "pending" {
			continue
		}
		if _, err := a.store.GetImages(id); err != nil {
			return false
		}
		asset := &inventory[i]
		if !strings.HasPrefix(src.url, "data:") && !a.cfg.ArchiveRemoteImages {
			asset.Status = "blocked"
			asset.Error = "服务端远程图片保存已关闭"
		} else {
			ctx, cancel := context.WithTimeout(a.ctx, time.Duration(a.cfg.ImageDownloadTimeoutSeconds)*time.Second)
			data, mime, err := a.fetcher.fetch(ctx, src.url, a.cfg.MaxImageBytes)
			cancel()
			if err == nil {
				var hash string
				hash, err = a.put(data)
				if err == nil {
					asset.Hash = hash
					asset.MIME = mime
					asset.Size = int64(len(data))
					asset.Status = "ready"
					asset.Error = ""
				}
			}
			if err != nil {
				if a.ctx.Err() != nil {
					return false
				}
				asset.Status = "failed"
				asset.Error = err.Error()
			}
		}
		if err := a.store.SetImages(id, inventory); err != nil {
			return false
		}
	}
	return true
}

func (a *imageArchive) exists(hash string) bool {
	if !validSHA256(hash) {
		return false
	}
	info, err := a.root.Lstat(hash + ".img")
	return err == nil && info.Mode().IsRegular()
}
func (a *imageArchive) put(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	name := hash + ".img"
	a.filesMu.Lock()
	defer a.filesMu.Unlock()
	if info, err := a.root.Lstat(name); err == nil {
		if !info.Mode().IsRegular() {
			return "", errors.New("图片存储目标不是普通文件")
		}
		oldFile, err := a.root.Open(name)
		var old []byte
		if err == nil {
			old, err = io.ReadAll(io.LimitReader(oldFile, a.cfg.MaxImageBytes+1))
			oldFile.Close()
		}
		if err == nil && imageBytesID(old) == hash {
			return hash, nil
		}
		if err := a.root.Remove(name); err != nil {
			return "", errors.New("无法替换损坏的图片")
		}
		a.used -= a.sizes[hash]
		delete(a.sizes, hash)
	}
	if _, err := a.root.Lstat(name); os.IsNotExist(err) {
		a.used -= a.sizes[hash]
		delete(a.sizes, hash)
	}
	if len(a.sizes) >= a.cfg.MaxImageFiles {
		return "", errors.New("图片文件数达到上限，请清理过期记录或增大配额")
	}
	if int64(len(data)) > a.cfg.MaxImageStorageBytes-a.used {
		return "", errors.New("图片存储空间已达上限，请清理过期记录或增大配额")
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	tmp := ".rr-tmp-" + hex.EncodeToString(random)
	f, err := a.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", errors.New("无法创建图片文件")
	}
	defer a.root.Remove(tmp)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return "", errors.New("无法写入图片文件")
	}
	if err = a.root.Rename(tmp, name); err != nil {
		return "", errors.New("无法提交图片文件")
	}
	a.used += int64(len(data))
	a.sizes[hash] = int64(len(data))
	return hash, nil
}
func (a *imageArchive) read(asset ImageAsset) ([]byte, error) {
	if !validSHA256(asset.Hash) || asset.Size < 1 || asset.Size > a.cfg.MaxImageBytes {
		return nil, errors.New("无效图片元数据")
	}
	select {
	case a.serving <- struct{}{}:
		defer func() { <-a.serving }()
	default:
		return nil, errImageBusy
	}
	a.filesMu.Lock()
	defer a.filesMu.Unlock()
	info, err := a.root.Lstat(asset.Hash + ".img")
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("图片文件不可用")
	}
	f, err := a.root.Open(asset.Hash + ".img")
	if err != nil {
		return nil, errors.New("图片文件不可用")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, a.cfg.MaxImageBytes+1))
	if err != nil || int64(len(data)) != asset.Size || imageBytesID(data) != asset.Hash {
		return nil, errors.New("图片文件完整性检查失败")
	}
	mime, err := imageMIME(data)
	if err != nil || mime != asset.MIME {
		return nil, errors.New("图片文件类型检查失败")
	}
	return data, nil
}

var errImageBusy = errors.New("图片读取并发已满")

func (a *imageArchive) collect(before time.Time) error {
	a.filesMu.Lock()
	defer a.filesMu.Unlock()
	refs, err := a.store.ImageReferences()
	if err != nil {
		return err
	}
	f, err := a.root.Open(".")
	if err != nil {
		return err
	}
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !cacheImageName.MatchString(entry.Name()) {
			continue
		}
		hash := strings.TrimSuffix(entry.Name(), ".img")
		if refs[hash] {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.ModTime().Before(before) {
			continue
		}
		if err := a.root.Remove(entry.Name()); err != nil {
			return err
		}
		a.used -= a.sizes[hash]
		delete(a.sizes, hash)
	}
	return nil
}
func (a *imageArchive) retry(id string) error {
	images, err := a.store.GetImages(id)
	if err != nil {
		return err
	}
	a.jobsMu.Lock()
	a.retries[id] = true
	a.jobsMu.Unlock()
	for i := range images {
		if images[i].Status != "ready" {
			images[i].Status = "pending"
			images[i].Error = ""
		}
	}
	if err := a.store.SetImages(id, images); err != nil {
		return err
	}
	if !a.enqueue(id) {
		return errors.New("图片保存队列已满，请稍后重试")
	}
	return nil
}
func (a *imageArchive) decorate(messages []Message, inventory []ImageAsset) {
	byID := map[string]ImageAsset{}
	for _, asset := range inventory {
		byID[asset.ID] = asset
	}
	for mi := range messages {
		for ii := range messages[mi].Images {
			img := &messages[mi].Images[ii]
			id := imageID(img.URL)
			img.ID = id
			img.URL = "" // never send a usable external image URL to the renderer
			if asset, ok := byID[id]; ok {
				img.Status = asset.Status
				img.Error = asset.Error
				img.MIME = asset.MIME
			} else if len(inventory) >= a.cfg.MaxImagesPerRecord {
				img.Status = "blocked"
				img.Error = "超过单条记录图片数量限制"
			} else {
				img.Status = "pending"
			}
		}
	}
}
func (a *imageArchive) status() map[string]any {
	a.jobsMu.Lock()
	jobs := len(a.jobs)
	a.jobsMu.Unlock()
	a.filesMu.Lock()
	used := a.used
	a.filesMu.Unlock()
	return map[string]any{"images_dir": imageDirectory(a.cfg), "image_storage_bytes": used, "max_image_files": a.cfg.MaxImageFiles, "max_image_storage_bytes": a.cfg.MaxImageStorageBytes, "pending_image_records": jobs, "max_image_bytes": a.cfg.MaxImageBytes}
}
func imageDownloadName(asset ImageAsset) string {
	ext := map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/gif": "gif", "image/webp": "webp", "image/bmp": "bmp", "image/avif": "avif"}[asset.MIME]
	return fmt.Sprintf("%s.%s", asset.ID, ext)
}

func (a *imageArchive) pending(id string) bool {
	a.jobsMu.Lock()
	defer a.jobsMu.Unlock()
	_, ok := a.jobs[id]
	return ok
}

func imageBytesID(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
