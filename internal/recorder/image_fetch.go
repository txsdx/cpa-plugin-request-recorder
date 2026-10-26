package recorder

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Remote images are untrusted request data. Resolve every hop, reject all
// non-public answers, then dial only a vetted IP (also for proxy CONNECT).
var imageDeniedNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/96"), netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"), netip.MustParsePrefix("2001::/32"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("2001:2::/48"), netip.MustParsePrefix("2001:10::/28"), netip.MustParsePrefix("2001:20::/28"), netip.MustParsePrefix("3fff::/20"), netip.MustParsePrefix("fc00::/7"), netip.MustParsePrefix("fe80::/10"), netip.MustParsePrefix("ff00::/8"),
}

func publicImageIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range imageDeniedNetworks {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}
func imageRemoteURL(raw string) (*url.URL, error) {
	if len(raw) > 16384 {
		return nil, errors.New("图片 URL 过长")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("远程图片只允许无用户凭证的 HTTPS URL")
	}
	if u.Port() != "" && u.Port() != "443" {
		return nil, errors.New("图片下载仅允许 HTTPS 443 端口")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" || len(host) > 253 || strings.ContainsAny(host, "%\\\x00\r\n") {
		return nil, errors.New("无效的图片域名")
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if !publicImageIP(addr) {
			return nil, errors.New("图片 URL 指向非公网地址")
		}
	} else {
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return nil, errors.New("无效的图片域名")
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
					return nil, errors.New("图片域名必须使用 ASCII / punycode")
				}
			}
		}
	}
	return u, nil
}

type imageFetcher struct {
	lookup    func(context.Context, string) ([]netip.Addr, error)
	dial      func(context.Context, string, string) (net.Conn, error)
	proxy     func(*http.Request) (*url.URL, error)
	tlsConfig *tls.Config
}

func newImageFetcher() *imageFetcher {
	dialer := &net.Dialer{Timeout: 8 * time.Second}
	return &imageFetcher{lookup: func(ctx context.Context, h string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", h)
	}, dial: dialer.DialContext, proxy: http.ProxyFromEnvironment, tlsConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
}
func (f *imageFetcher) resolve(ctx context.Context, u *url.URL) (netip.Addr, error) {
	if _, err := imageRemoteURL(u.String()); err != nil {
		return netip.Addr{}, err
	}
	host := strings.TrimSuffix(u.Hostname(), ".")
	var addresses []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addresses = []netip.Addr{ip}
	} else {
		var err error
		addresses, err = f.lookup(ctx, host)
		if err != nil {
			return netip.Addr{}, errors.New("图片域名解析失败")
		}
	}
	if len(addresses) == 0 {
		return netip.Addr{}, errors.New("图片域名没有地址")
	}
	for _, ip := range addresses {
		if !publicImageIP(ip) {
			return netip.Addr{}, errors.New("图片域名含非公网地址，已拒绝")
		}
	}
	return addresses[0].Unmap(), nil
}
func (f *imageFetcher) connection(ctx context.Context, u *url.URL, ip netip.Addr, proxy *url.URL) (net.Conn, error) {
	target := net.JoinHostPort(ip.String(), "443")
	var conn net.Conn
	var err error
	if proxy == nil {
		conn, err = f.dial(ctx, "tcp", target)
	} else {
		if proxy.Scheme != "http" && proxy.Scheme != "https" {
			return nil, errors.New("图片出口代理仅支持 HTTP(S) CONNECT")
		}
		port := proxy.Port()
		if port == "" {
			port = "80"
			if proxy.Scheme == "https" {
				port = "443"
			}
		}
		conn, err = f.dial(ctx, "tcp", net.JoinHostPort(proxy.Hostname(), port))
	}
	if err != nil {
		return nil, errors.New("图片网络连接失败")
	}
	success := false
	defer func() {
		if !success {
			conn.Close()
		}
	}()
	rawConn := conn
	stop := context.AfterFunc(ctx, func() { rawConn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if proxy != nil {
		if proxy.Scheme == "https" {
			cfg := f.tlsConfig.Clone()
			cfg.ServerName = proxy.Hostname()
			secure := tls.Client(conn, cfg)
			if err := secure.HandshakeContext(ctx); err != nil {
				return nil, errors.New("出口代理 TLS 验证失败")
			}
			conn = secure
		}
		req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target, Header: make(http.Header)}
		if proxy.User != nil {
			password, _ := proxy.User.Password()
			req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(proxy.User.Username()+":"+password)))
		}
		if err := req.Write(conn); err != nil {
			return nil, errors.New("图片出口代理请求失败")
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), req)
		if err != nil {
			return nil, errors.New("图片出口代理响应失败")
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			return nil, fmt.Errorf("图片出口代理拒绝连接（HTTP %d）", resp.StatusCode)
		}
	}
	cfg := f.tlsConfig.Clone()
	cfg.ServerName = strings.TrimSuffix(u.Hostname(), ".")
	secure := tls.Client(conn, cfg)
	if err := secure.HandshakeContext(ctx); err != nil {
		return nil, errors.New("图片源站 TLS 验证失败")
	}
	success = true
	return secure, nil
}
func (f *imageFetcher) fetch(ctx context.Context, source string, max int64) ([]byte, string, error) {
	if strings.HasPrefix(source, "data:") {
		return inlineImage(source, max)
	}
	u, err := imageRemoteURL(source)
	if err != nil {
		return nil, "", err
	}
	for redirects := 0; redirects <= 4; redirects++ {
		ip, err := f.resolve(ctx, u)
		if err != nil {
			return nil, "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, "", errors.New("无效图片请求")
		}
		req.Header.Set("User-Agent", "CPA-Request-Recorder-Image-Archive/1")
		req.Header.Set("Accept", "image/png,image/jpeg,image/gif,image/webp,image/avif,image/bmp")
		// A fresh request/transport has no cookie jar, caller headers or HTTP auth.
		proxy, err := f.proxy(req)
		if err != nil {
			return nil, "", errors.New("图片出口代理配置无效")
		}
		current := *u
		transport := &http.Transport{DisableKeepAlives: true, DisableCompression: true, MaxResponseHeaderBytes: 32768, DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return f.connection(ctx, &current, ip, proxy)
		}}
		client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Do(req)
		if err != nil {
			transport.CloseIdleConnections()
			return nil, "", errors.New("图片下载失败（网络、代理或 TLS 验证）")
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			next, err := resp.Location()
			resp.Body.Close()
			transport.CloseIdleConnections()
			if err != nil {
				return nil, "", errors.New("图片重定向地址无效")
			}
			if _, err = imageRemoteURL(next.String()); err != nil {
				return nil, "", err
			}
			u = next
			continue
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			transport.CloseIdleConnections()
			return nil, "", fmt.Errorf("图片源站返回 HTTP %d", resp.StatusCode)
		}
		if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
			resp.Body.Close()
			transport.CloseIdleConnections()
			return nil, "", errors.New("不接受压缩的图片传输编码")
		}
		if resp.ContentLength > max {
			resp.Body.Close()
			transport.CloseIdleConnections()
			return nil, "", errors.New("图片超过单张大小限制")
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
		resp.Body.Close()
		transport.CloseIdleConnections()
		if err != nil {
			return nil, "", errors.New("图片下载未完成")
		}
		if int64(len(data)) > max {
			return nil, "", errors.New("图片超过单张大小限制")
		}
		mime, err := imageMIME(data)
		return data, mime, err
	}
	return nil, "", errors.New("图片重定向次数超过限制")
}
func inlineImage(raw string, max int64) ([]byte, string, error) {
	meta, payload, ok := strings.Cut(raw, ",")
	if !ok || !strings.HasSuffix(strings.ToLower(meta), ";base64") {
		return nil, "", errors.New("仅支持 base64 内嵌图片")
	}
	if int64(len(payload)) > ((max+2)/3)*4+4 {
		return nil, "", errors.New("内嵌图片超过大小限制")
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, "", errors.New("内嵌图片 base64 无效")
	}
	if int64(len(data)) > max {
		return nil, "", errors.New("图片超过单张大小限制")
	}
	mime, err := imageMIME(data)
	return data, mime, err
}
func imageMIME(data []byte) (string, error) {
	mime := http.DetectContentType(data)
	switch mime {
	case "image/png", "image/jpeg", "image/gif":
		config, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 80_000_000 {
			return "", errors.New("图片格式或尺寸无效（最大 8000 万像素）")
		}
		return mime, nil
	case "image/webp", "image/bmp":
		return mime, nil
	}
	if len(data) >= 16 && string(data[4:8]) == "ftyp" && (string(data[8:12]) == "avif" || string(data[8:12]) == "avis") {
		return "image/avif", nil
	}
	return "", errors.New("只保存 PNG、JPEG、GIF、WebP、BMP 或 AVIF 图片；拒绝 HTML / SVG 等内容")
}
