// ORSO Navegador — versión Go (un solo archivo, Tor incrustado)
//
// Compilar:
//   go mod init orso-navegador
//   go get github.com/webview/webview_go
//   go build -ldflags="-H windowsgui -s -w" -o ORSO-Navegador.exe main.go
//
// Estructura mínima SOLO en tiempo de compilación:
//   orso-navegador/
//   ├── main.go
//   └── tor/
//       ├── tor.exe      (obligatorio)
//       ├── geoip        (opcional)
//       └── geoip6       (opcional)
//
// Logs:  %LOCALAPPDATA%\orso-navegador\logs\orso.log  y  tor.log
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	webview "github.com/webview/webview_go"
)

// ----------------------------------------------------------------------------
// Constantes
// ----------------------------------------------------------------------------

const (
	torSocksAddr   = "127.0.0.1:19050"
	torControlAddr = "127.0.0.1:19051"
	smartProxyAddr = "127.0.0.1:19060"
	socksPort      = 19050
	controlPort    = 19051
)

// ----------------------------------------------------------------------------
// DNS propios
// ----------------------------------------------------------------------------

var publicDNSServers = []string{
	"1.1.1.1:53", "1.0.0.1:53",
	"8.8.8.8:53", "8.8.4.4:53",
	"9.9.9.9:53",
}

var customResolver = &net.Resolver{
	PreferGo:     true,
	StrictErrors: false,
	Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		var lastErr error
		for _, server := range publicDNSServers {
			for _, proto := range []string{"udp", "tcp"} {
				d := net.Dialer{Timeout: 5 * time.Second}
				conn, err := d.DialContext(ctx, proto, server)
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
		}
		return nil, fmt.Errorf("ningún DNS público accesible: %w", lastErr)
	},
}

func dialWithCustomDNS(target string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return nil, err
	}
	if net.ParseIP(host) != nil {
		return net.DialTimeout("tcp", target, 15*time.Second)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	ips, dnsErr := customResolver.LookupIPAddr(ctx, host)
	cancel()
	if dnsErr != nil || len(ips) == 0 {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel2()
		ips, dnsErr = net.DefaultResolver.LookupIPAddr(ctx2, host)
		if dnsErr != nil || len(ips) == 0 {
			return nil, fmt.Errorf("DNS lookup falló para %s: %w", host, dnsErr)
		}
	}
	var lastErr error
	for _, ip := range ips {
		addr := net.JoinHostPort(ip.IP.String(), port)
		conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("no se pudo conectar a %s: %w", host, lastErr)
}

// ----------------------------------------------------------------------------
// Logging
// ----------------------------------------------------------------------------

var (
	logDir     string
	logFile    *os.File
	torLogFile *os.File
)

func setupLogging() {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	logDir = filepath.Join(base, "orso-navegador", "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return
	}
	if f, err := os.OpenFile(filepath.Join(logDir, "orso.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		logFile = f
		log.SetOutput(f)
		log.SetFlags(log.LstdFlags | log.Lshortfile)
	}
	if tf, err := os.OpenFile(filepath.Join(logDir, "tor.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		torLogFile = tf
	}
}

// ----------------------------------------------------------------------------
// Tor incrustado
// ----------------------------------------------------------------------------

//go:embed all:tor
var torFS embed.FS

func ensureTorExtracted() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	torData, err := torFS.ReadFile("tor/tor.exe")
	if err != nil {
		return "", fmt.Errorf("tor.exe no encontrado en el binario: %w", err)
	}
	hash := sha256.Sum256(torData)
	version := hex.EncodeToString(hash[:8])
	dir := filepath.Join(base, "orso-navegador", "tor-"+version)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	torPath := filepath.Join(dir, "tor.exe")
	if info, err := os.Stat(torPath); err == nil && info.Size() == int64(len(torData)) {
		return torPath, nil
	}
	if err := os.WriteFile(torPath, torData, 0o700); err != nil {
		return "", fmt.Errorf("escribiendo tor.exe: %w", err)
	}
	for _, name := range []string{"geoip", "geoip6"} {
		data, err := torFS.ReadFile("tor/" + name)
		if err != nil {
			continue
		}
		_ = os.WriteFile(filepath.Join(dir, name), data, 0o600)
	}
	return torPath, nil
}

// ----------------------------------------------------------------------------
// TorManager
// ----------------------------------------------------------------------------

type TorManager struct {
	torPath string
	dataDir string
	cmd     *exec.Cmd
	mu      sync.Mutex
	bootPct atomic.Int32
	ready   atomic.Bool
	running atomic.Bool
	status  atomic.Value
}

func NewTorManager(torPath, dataDir string) *TorManager {
	t := &TorManager{torPath: torPath, dataDir: dataDir}
	t.status.Store("Detenido")
	return t
}

func (t *TorManager) Status() (int, bool, string) {
	return int(t.bootPct.Load()), t.ready.Load(), t.status.Load().(string)
}

func (t *TorManager) killOrphans() {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("taskkill", "/F", "/IM", "tor.exe")
	} else {
		cmd = exec.Command("pkill", "-f", "tor")
	}
	_ = cmd.Run()
}

func (t *TorManager) Start() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.running.Load() {
		return nil
	}
	t.killOrphans()
	if err := os.MkdirAll(t.dataDir, 0o700); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(t.dataDir, "lock"))

	torDir := filepath.Dir(t.torPath)
	args := []string{
		"--SocksPort", strconv.Itoa(socksPort),
		"--ControlPort", strconv.Itoa(controlPort),
		"--DataDirectory", t.dataDir,
		"--CookieAuthentication", "1",
		"--Log", "notice stdout",
		"--ClientDNSRejectInternalAddresses", "1",
		"--SafeLogging", "1",
	}
	if _, err := os.Stat(filepath.Join(torDir, "geoip")); err == nil {
		args = append(args, "--GeoIPFile", filepath.Join(torDir, "geoip"))
	}
	if _, err := os.Stat(filepath.Join(torDir, "geoip6")); err == nil {
		args = append(args, "--GeoIPv6File", filepath.Join(torDir, "geoip6"))
	}

	cmd := exec.Command(t.torPath, args...)
	if torLogFile != nil {
		cmd.Stdout = torLogFile
		cmd.Stderr = torLogFile
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	t.cmd = cmd
	t.running.Store(true)
	t.ready.Store(false)
	t.bootPct.Store(0)
	t.status.Store("Iniciando…")

	go func() { _ = cmd.Wait(); t.running.Store(false) }()
	go t.monitor()
	return nil
}

func (t *TorManager) monitor() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for t.running.Load() {
		<-ticker.C
		pct, summary := t.queryBootstrap()
		if pct < 0 {
			continue
		}
		t.bootPct.Store(int32(pct))
		if pct >= 100 {
			t.ready.Store(true)
			t.status.Store("Listo — circuito establecido")
		} else if summary != "" {
			t.status.Store(fmt.Sprintf("%d%% — %s", pct, summary))
		} else {
			t.status.Store(fmt.Sprintf("%d%%", pct))
		}
	}
}

func (t *TorManager) authenticate(conn net.Conn) error {
	cookiePath := filepath.Join(t.dataDir, "control_auth_cookie")
	var cookie []byte
	for i := 0; i < 30; i++ {
		b, err := os.ReadFile(cookiePath)
		if err == nil && len(b) > 0 {
			cookie = b
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(cookie) == 0 {
		return fmt.Errorf("cookie no disponible")
	}
	fmt.Fprintf(conn, "AUTHENTICATE %s\r\n", hex.EncodeToString(cookie))
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		if strings.HasPrefix(line, "250") {
			return nil
		}
		if strings.HasPrefix(line, "5") {
			return fmt.Errorf("auth rechazada: %s", strings.TrimSpace(line))
		}
	}
}

func (t *TorManager) queryBootstrap() (int, string) {
	conn, err := net.DialTimeout("tcp", torControlAddr, 1500*time.Millisecond)
	if err != nil {
		return -1, ""
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if err := t.authenticate(conn); err != nil {
		return -1, ""
	}
	fmt.Fprintf(conn, "GETINFO status/bootstrap-phase\r\n")
	br := bufio.NewReader(conn)
	var sb strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		sb.WriteString(line)
		if strings.HasPrefix(line, "250 ") || strings.HasPrefix(line, "5") {
			break
		}
	}
	resp := sb.String()
	idx := strings.Index(resp, "PROGRESS=")
	if idx < 0 {
		return -1, ""
	}
	rest := resp[idx+len("PROGRESS="):]
	end := strings.IndexAny(rest, " \r\n")
	if end < 0 {
		end = len(rest)
	}
	pct, _ := strconv.Atoi(rest[:end])
	summary := ""
	if s := strings.Index(resp, `SUMMARY="`); s >= 0 {
		r := resp[s+len(`SUMMARY="`):]
		if e := strings.Index(r, `"`); e >= 0 {
			summary = r[:e]
		}
	}
	return pct, summary
}

func (t *TorManager) Shutdown() {
	t.mu.Lock()
	cmd := t.cmd
	t.mu.Unlock()
	if !t.running.Load() && cmd == nil {
		return
	}
	if conn, err := net.DialTimeout("tcp", torControlAddr, 2*time.Second); err == nil {
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		if err := t.authenticate(conn); err == nil {
			fmt.Fprintf(conn, "SIGNAL SHUTDOWN\r\n")
			_, _ = bufio.NewReader(conn).ReadString('\n')
		}
		conn.Close()
	}
	if cmd != nil && cmd.Process != nil {
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	t.killOrphans()
	t.running.Store(false)
	t.ready.Store(false)
	t.bootPct.Store(0)
	t.status.Store("Detenido")
	_ = os.Remove(filepath.Join(t.dataDir, "lock"))
}

// ----------------------------------------------------------------------------
// SOCKS5
// ----------------------------------------------------------------------------

func socks5Dial(socksAddr, target string) (net.Conn, error) {
	c, err := net.DialTimeout("tcp", socksAddr, 10*time.Second)
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		c.Close()
		return nil, err
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(c, buf); err != nil {
		c.Close()
		return nil, err
	}
	if buf[0] != 5 || buf[1] != 0 {
		c.Close()
		return nil, fmt.Errorf("socks5 handshake rechazado")
	}
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		c.Close()
		return nil, err
	}
	port, _ := strconv.Atoi(portStr)
	req := []byte{5, 1, 0}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append(req, 1)
			req = append(req, ip4...)
		} else {
			req = append(req, 4)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			c.Close()
			return nil, fmt.Errorf("host demasiado largo")
		}
		req = append(req, 3, byte(len(host)))
		req = append(req, []byte(host)...)
	}
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		c.Close()
		return nil, err
	}
	resp := make([]byte, 4)
	if _, err := io.ReadFull(c, resp); err != nil {
		c.Close()
		return nil, err
	}
	if resp[1] != 0 {
		c.Close()
		return nil, fmt.Errorf("socks5 error %d", resp[1])
	}
	switch resp[3] {
	case 1:
		_, _ = io.ReadFull(c, make([]byte, 6))
	case 4:
		_, _ = io.ReadFull(c, make([]byte, 18))
	case 3:
		l := make([]byte, 1)
		_, _ = io.ReadFull(c, l)
		_, _ = io.ReadFull(c, make([]byte, int(l[0])+2))
	}
	_ = c.SetDeadline(time.Time{})
	return c, nil
}

// ----------------------------------------------------------------------------
// SmartProxy
// ----------------------------------------------------------------------------

type SmartProxy struct {
	listener net.Listener
	anonMode atomic.Bool
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
	closed   atomic.Bool
}

func (p *SmartProxy) Start() error {
	ln, err := net.Listen("tcp", smartProxyAddr)
	if err != nil {
		return err
	}
	p.listener = ln
	p.conns = make(map[net.Conn]struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			if p.closed.Load() {
				conn.Close()
				return
			}
			p.mu.Lock()
			p.conns[conn] = struct{}{}
			p.mu.Unlock()
			p.wg.Add(1)
			go func(c net.Conn) {
				defer p.wg.Done()
				p.handle(c)
				p.mu.Lock()
				delete(p.conns, c)
				p.mu.Unlock()
			}(conn)
		}
	}()
	return nil
}

func (p *SmartProxy) Stop() {
	if p.closed.Swap(true) {
		return
	}
	if p.listener != nil {
		_ = p.listener.Close()
	}
	p.mu.Lock()
	for c := range p.conns {
		_ = c.Close()
	}
	p.mu.Unlock()
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}

func (p *SmartProxy) dial(target string) (net.Conn, error) {
	if p.anonMode.Load() {
		return socks5Dial(torSocksAddr, target)
	}
	return dialWithCustomDNS(target)
}

func (p *SmartProxy) handle(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	mode := "normal"
	if p.anonMode.Load() {
		mode = "anon"
	}
	if req.Method == http.MethodConnect {
		upstream, err := p.dial(req.Host)
		if err != nil {
			log.Printf("[proxy/%s] CONNECT %s → 502: %v", mode, req.Host, err)
			_, _ = c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
			return
		}
		defer upstream.Close()
		if _, err := c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			return
		}
		done := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(upstream, br); done <- struct{}{} }()
		go func() { _, _ = io.Copy(c, upstream); done <- struct{}{} }()
		<-done
		return
	}
	host := req.URL.Host
	if !strings.Contains(host, ":") {
		if req.URL.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	upstream, err := p.dial(host)
	if err != nil {
		_, _ = c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer upstream.Close()
	if err := req.Write(upstream); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, br); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, upstream); done <- struct{}{} }()
	<-done
}

// ----------------------------------------------------------------------------
// Ventanas WebView nativas
// ----------------------------------------------------------------------------

var (
	windowsMu sync.Mutex
	windows   = make(map[int]bool)
	nextWinID int
)

func applyProxyEnv() {
	os.Setenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS",
		"--proxy-server=http://"+smartProxyAddr)
}

func openWindow(rawURL string, anonMode *atomic.Bool) {
	windowsMu.Lock()
	id := nextWinID
	nextWinID++
	windows[id] = true
	windowsMu.Unlock()

	go func() {
		defer func() {
			windowsMu.Lock()
			delete(windows, id)
			windowsMu.Unlock()
			log.Printf("[ventana %d] cerrada", id)
		}()

		applyProxyEnv()

		anon := anonMode.Load()
		prefix := ""
		if anon {
			prefix = "🕶️ "
		}
		title := prefix + rawURL

		wv := webview.New(false)
		defer wv.Destroy()
		wv.SetTitle(title)
		wv.SetSize(1200, 800, webview.HintNone)

		_ = wv.Bind("goOpenWindow", func(u string) string {
			openWindow(u, anonMode)
			return "ok"
		})

		wv.Navigate(rawURL)
		log.Printf("[ventana %d] abierta (%s): %s",
			id, map[bool]string{true: "anon", false: "normal"}[anon], rawURL)
		wv.Run()
	}()
}

// ----------------------------------------------------------------------------
// UI del hub
// ----------------------------------------------------------------------------

const uiHTML = `<!DOCTYPE html>
<html lang="es">
<head>
<meta charset="UTF-8">
<title>ORSO Navegador</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
html,body{width:100%;height:100%;overflow:hidden;font-family:'Segoe UI',system-ui,sans-serif;background:#1e1e1e;color:#e0e0e0}
#app{display:flex;flex-direction:column;height:100vh}
#toolbar{display:flex;align-items:center;gap:6px;padding:8px;background:#2d2d30}
#toolbar button{background:#3c3c40;border:none;color:#e0e0e0;min-width:34px;height:34px;border-radius:6px;cursor:pointer;font-size:14px;padding:0 10px}
#toolbar button:hover{background:#4a4a50}
#toolbar button.active{background:#6b3fa0;box-shadow:0 0 10px rgba(162,89,255,.8)}
#url{flex:1;height:34px;padding:0 14px;background:#1e1e1e;border:1px solid #3c3c40;border-radius:17px;color:#e0e0e0;font-size:13px;outline:none}
#url:focus{border-color:#6b3fa0}
#body{flex:1;display:flex;overflow:hidden}
#sidebar{width:260px;background:#252526;border-right:1px solid #333;padding:12px;overflow-y:auto}
#sidebar h3{font-size:12px;text-transform:uppercase;color:#888;margin-bottom:8px;letter-spacing:.5px}
.quick{display:block;width:100%;text-align:left;padding:10px 12px;margin-bottom:6px;background:#2d2d30;border:none;color:#e0e0e0;border-radius:6px;cursor:pointer;font-size:13px;transition:background .15s}
.quick:hover{background:#3c3c40}
#main{flex:1;display:flex;flex-direction:column;align-items:center;justify-content:center;padding:32px;text-align:center}
#main h1{font-size:32px;font-weight:300;letter-spacing:-1px;color:#fff;margin-bottom:8px}
#main .tag{color:#888;font-size:13px;margin-bottom:32px}
#bigurl{width:100%;max-width:640px;height:48px;padding:0 20px;background:#1e1e1e;border:1px solid #444;border-radius:24px;color:#e0e0e0;font-size:15px;outline:none;margin-bottom:16px}
#bigurl:focus{border-color:#6b3fa0;box-shadow:0 0 12px rgba(162,89,255,.3)}
#go{height:48px;padding:0 32px;background:#6b3fa0;border:none;color:#fff;border-radius:24px;cursor:pointer;font-size:14px;font-weight:500}
#go:hover{background:#7d4fbd}
#statusbar{height:30px;background:#252526;font-size:12px;display:flex;align-items:center;padding:0 12px;color:#aaa;gap:16px;border-top:1px solid #333}
.mode-badge{padding:3px 10px;border-radius:12px;font-weight:600;font-size:11px;letter-spacing:.5px}
.mode-normal{background:#2d5c2d;color:#9fe89f}
.mode-anon{background:#6b3fa0;color:#fff;box-shadow:0 0 8px rgba(162,89,255,.6)}
#settings{position:absolute;top:60px;right:12px;background:#2d2d30;border:1px solid #444;border-radius:8px;padding:14px;width:360px;box-shadow:0 8px 24px rgba(0,0,0,.6);z-index:100;display:none}
#settings.open{display:block}
#settings h3{font-size:13px;margin-bottom:6px;color:#fff}
#settings p{font-size:12px;margin:4px 0;color:#bbb;word-break:break-all}
#settings hr{border:none;border-top:1px solid #444;margin:10px 0}
#settings button{padding:6px 10px;background:#3c3c40;border:none;color:#e0e0e0;border-radius:4px;cursor:pointer;margin:2px 4px 2px 0;font-size:12px}
#settings button:hover{background:#4a4a50}
</style>
</head>
<body>
<div id="app">
  <div id="toolbar">
    <input id="url" type="text" placeholder="Buscar en Google o escribe una URL y pulsa Enter" autocomplete="off" spellcheck="false"/>
    <button id="anon" title="Modo anónimo (Ctrl+Shift+N)">🕶️</button>
    <button id="ipcheck" title="Verificar IP">IP</button>
    <button id="settingsBtn" title="Ajustes">⚙️</button>
  </div>
  <div id="body">
    <div id="sidebar">
      <h3>Accesos rápidos</h3>
      <button class="quick" data-url="https://check.torproject.org/">🔒 Comprobar Tor</button>
      <button class="quick" data-url="https://www.google.com/">🔎 Google</button>
      <button class="quick" data-url="https://duckduckgo.com/">🦆 DuckDuckGo</button>
      <button class="quick" data-url="https://www.wikipedia.org/">📚 Wikipedia</button>
      <button class="quick" data-url="https://github.com/">🐙 GitHub</button>
      <button class="quick" data-url="https://news.ycombinator.com/">📰 Hacker News</button>
      <button class="quick" data-url="https://www.youtube.com/">▶️ YouTube</button>
    </div>
    <div id="main">
      <h1>ORSO Navegador</h1>
      <div class="tag">Tor se activa automáticamente. Cada búsqueda se abre en una ventana nativa.</div>
      <input id="bigurl" type="text" placeholder="Buscar o URL…" autocomplete="off" spellcheck="false"/>
      <button id="go">Abrir</button>
    </div>
  </div>
  <div id="statusbar">
    <span id="modeBadge" class="mode-badge mode-anon">🕶️ MODO ANÓNIMO</span>
    <span id="torStatus">Tor: iniciando…</span>
    <span style="margin-left:auto" id="dnsStatus">DNS: —</span>
  </div>
</div>
<div id="settings">
  <h3>Estado de Tor</h3>
  <p id="torDetail">Iniciando…</p>
  <button id="torStart">Iniciar</button>
  <button id="torRestart">Reiniciar</button>
  <button id="torStop">Detener</button>
  <hr>
  <h3>Modo anónimo</h3>
  <p>Cuando está activo, todo el tráfico sale por Tor.</p>
  <p id="anonDetail">🕶️ ACTIVADO por defecto</p>
  <hr>
  <h3>DNS</h3>
  <p id="dnsList">—</p>
</div>

<script>
const el = id => document.getElementById(id);

function normalizeUrl(input){
  let url = (input || '').trim();
  if (!url) return '';
  const isUrl =
       /^[a-z][a-z0-9+.\-]*:\/\//i.test(url)
    || /^[\w.-]+\.[a-z]{2,}(:\d+)?(\/.*)?$/i.test(url)
    || /^localhost(:\d+)?(\/.*)?$/i.test(url)
    || /^\d{1,3}(\.\d{1,3}){3}(:\d+)?(\/.*)?$/.test(url);
  if (!isUrl){
    return 'https://www.google.com/search?q=' + encodeURIComponent(url);
  }
  if (!/^[a-z][a-z0-9+.\-]*:\/\//i.test(url)) url = 'https://' + url;
  return url;
}

async function openURL(input){
  const url = normalizeUrl(input);
  if (!url) return;
  await goOpenWindow(url);
  el('url').value = '';
  el('bigurl').value = '';
}

el('url').addEventListener('keydown', e => { if (e.key==='Enter') openURL(el('url').value); });
el('bigurl').addEventListener('keydown', e => { if (e.key==='Enter') openURL(el('bigurl').value); });
el('go').onclick = () => openURL(el('bigurl').value);

document.querySelectorAll('.quick').forEach(b => {
  b.onclick = () => openURL(b.dataset.url);
});

el('anon').onclick = async () => {
  const s = JSON.parse(await goTorStatus());
  await goSetAnon(!s.anon);
  refreshStatus();
};

el('ipcheck').onclick = async () => {
  const s = JSON.parse(await goTorStatus());
  const mode = s.anon ? 'ANÓNIMO (Tor)' : 'NORMAL (IP real)';
  const r = await goCheckIP();
  let pretty = r;
  try { pretty = JSON.stringify(JSON.parse(r), null, 2); } catch(e){}
  alert('Modo: ' + mode + '\n\nRespuesta de check.torproject.org:\n\n' + pretty);
};

el('settingsBtn').onclick = () => el('settings').classList.toggle('open');
el('torStart').onclick = async () => { await goStartTor(); refreshStatus(); };
el('torRestart').onclick = async () => { await goStopTor(); await goStartTor(); refreshStatus(); };
el('torStop').onclick = async () => { await goStopTor(); refreshStatus(); };

document.addEventListener('keydown', e => {
  if (e.ctrlKey && e.key==='l'){ e.preventDefault(); el('url').focus(); el('url').select(); }
  else if (e.ctrlKey && e.shiftKey && (e.key==='N' || e.key==='n')){ e.preventDefault(); el('anon').click(); }
  else if (e.key==='F11'){ e.preventDefault(); if (!document.fullscreenElement) document.documentElement.requestFullscreen(); else document.exitFullscreen(); }
});

async function refreshStatus(){
  try {
    const s = JSON.parse(await goTorStatus());
    const label = s.ready ? 'listo' : (s.percent>0 ? s.percent+'%' : s.message);
    el('torStatus').textContent = 'Tor: ' + label;
    el('torDetail').textContent = s.message + ' (' + s.percent + '%)';
    el('anonDetail').textContent = s.anon ? '🕶️ ACTIVADO' : 'Desactivado';
    el('anon').classList.toggle('active', s.anon);

    const badge = el('modeBadge');
    if (s.anon) {
      badge.textContent = '🕶️ MODO ANÓNIMO';
      badge.className = 'mode-badge mode-anon';
    } else {
      badge.textContent = 'MODO NORMAL';
      badge.className = 'mode-badge mode-normal';
    }

    el('dnsList').textContent = (s.dns || []).join('  ·  ');
    el('dnsStatus').textContent = 'DNS: ' + ((s.dns || []).length) + ' servidores';
  } catch(e){}
}

setInterval(refreshStatus, 2000);
refreshStatus();
</script>
</body>
</html>`

// ----------------------------------------------------------------------------
// main
// ----------------------------------------------------------------------------

func main() {
	setupLogging()
	log.Println("=== ORSO Navegador iniciado ===")

	var shutdownOnce sync.Once
	shutdown := func(reason string) {
		shutdownOnce.Do(func() {
			log.Printf("Cerrando ORSO Navegador (%s)…", reason)
		})
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		s := <-sigCh
		shutdown("señal " + s.String())
		os.Exit(0)
	}()

	dataFlag := flag.String("data", "", "Directorio de datos de Tor")
	dnsFlag := flag.String("dns", "", "DNS separados por coma")
	noAnonFlag := flag.Bool("no-anon", false, "Arrancar en modo NORMAL (sin Tor)")
	flag.Parse()

	if *dnsFlag != "" {
		parts := strings.Split(*dnsFlag, ",")
		var list []string
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if !strings.Contains(p, ":") {
				p += ":53"
			}
			list = append(list, p)
		}
		if len(list) > 0 {
			publicDNSServers = list
		}
	}
	log.Printf("DNS públicos: %v", publicDNSServers)

	torPath, err := ensureTorExtracted()
	if err != nil {
		log.Fatalf("No se pudo preparar Tor: %v", err)
	}

	dataDir := *dataFlag
	if dataDir == "" {
		if cache, err := os.UserCacheDir(); err == nil {
			dataDir = filepath.Join(cache, "orso-navegador", "tor-data")
		} else {
			dataDir = filepath.Join(os.TempDir(), "orso-tor")
		}
	}

	log.Printf("ORSO Navegador — tor=%s data=%s", torPath, dataDir)

	tm := NewTorManager(torPath, dataDir)
	proxy := &SmartProxy{}
	if err := proxy.Start(); err != nil {
		log.Fatalf("No se pudo iniciar el proxy local en %s: %v", smartProxyAddr, err)
	}

	// ─── ACTIVACIÓN AUTOMÁTICA ─────────────────────────────────────────────
	// 1) Modo anónimo ACTIVADO por defecto (salvo -no-anon)
	startAnon := !*noAnonFlag
	proxy.anonMode.Store(startAnon)
	if startAnon {
		log.Println("✅ Modo anónimo ACTIVADO automáticamente al inicio")
	} else {
		log.Println("Modo normal (flag -no-anon)")
	}

	// 2) Tor arranca solo en segundo plano
	go func() {
		if err := tm.Start(); err != nil {
			log.Printf("Aviso: no se pudo arrancar Tor: %v", err)
		} else {
			log.Println("✅ Tor arrancado automáticamente en segundo plano")
		}
	}()
	// ───────────────────────────────────────────────────────────────────────

	applyProxyEnv()

	w := webview.New(false)
	w.SetTitle("ORSO Navegador")
	w.SetSize(1080, 700, webview.HintNone)

	_ = w.Bind("goTorStatus", func() string {
		pct, ready, msg := tm.Status()
		b, _ := json.Marshal(map[string]interface{}{
			"percent": pct,
			"ready":   ready,
			"message": msg,
			"anon":    proxy.anonMode.Load(),
			"dns":     publicDNSServers,
		})
		return string(b)
	})
	_ = w.Bind("goStartTor", func() string {
		if err := tm.Start(); err != nil {
			return "error: " + err.Error()
		}
		return "ok"
	})
	_ = w.Bind("goStopTor", func() string {
		tm.Shutdown()
		return "ok"
	})
	_ = w.Bind("goSetAnon", func(anon bool) string {
		proxy.anonMode.Store(anon)
		if anon && !tm.running.Load() {
			_ = tm.Start()
		}
		log.Printf("Modo anónimo: %v", anon)
		return "ok"
	})
	_ = w.Bind("goOpenWindow", func(u string) string {
		openWindow(u, &proxy.anonMode)
		return "ok"
	})
	_ = w.Bind("goCheckIP", func() string {
		proxyURL, _ := url.Parse("http://" + smartProxyAddr)
		client := &http.Client{
			Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
			Timeout:   30 * time.Second,
		}
		resp, err := client.Get("https://check.torproject.org/api/ip")
		if err != nil {
			return `{"error":"` + err.Error() + `"}`
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	})

	w.SetHtml(uiHTML)
	w.Run()
	shutdown("ventana principal cerrada")

	w.Destroy()
	proxy.Stop()
	tm.Shutdown()

	runtime.GC()
	debug.FreeOSMemory()

	if torLogFile != nil {
		_ = torLogFile.Close()
	}
	if logFile != nil {
		log.Println("=== ORSO Navegador cerrado ===")
		_ = logFile.Close()
	}
}