package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	maxAccessRecords = 100 // 每个订阅组保留最近多少次拉取
	maxKnownIPs      = 200 // 每个订阅组记住多少个出现过的 IP，用来标记「新 IP」
)

// AccessRecord 是订阅链接被拉取一次的记录。
type AccessRecord struct {
	Time   time.Time `json:"time"`
	IP     string    `json:"ip"`
	UA     string    `json:"ua"`
	Nodes  int       `json:"nodes"`           // 返回了多少节点
	Status int       `json:"status"`          // 返回给客户端的 HTTP 状态码
	NewIP  bool      `json:"newIp,omitempty"` // 这个 IP 此前没拉取过本订阅组（第一个 IP 不算）
}

type profileAccess struct {
	Records []AccessRecord       `json:"records"` // 旧 → 新
	IPs     map[string]time.Time `json:"ips"`     // 出现过的 IP → 最近一次时间
}

// AccessLog 按订阅组保存最近的拉取记录，存在 DATA_DIR/access.json。
// 它不属于配置：不随配置导出，丢了也只是少了历史。
type AccessLog struct {
	mu      sync.Mutex
	path    string
	delay   time.Duration // 攒多久写一次盘；0 为每次都立即写（测试用）
	pending bool
	data    map[string]*profileAccess
}

func OpenAccessLog(dataDir string, delay time.Duration) *AccessLog {
	a := &AccessLog{path: filepath.Join(dataDir, "access.json"), delay: delay, data: map[string]*profileAccess{}}
	if b, err := os.ReadFile(a.path); err == nil {
		if json.Unmarshal(b, &a.data) != nil || a.data == nil { // 损坏就从空的开始
			a.data = map[string]*profileAccess{}
		}
	}
	for id, p := range a.data {
		if p == nil {
			delete(a.data, id)
		} else if p.IPs == nil {
			p.IPs = map[string]time.Time{}
		}
	}
	return a
}

// 调用方需持有 a.mu。
func (a *AccessLog) changed() {
	if a.delay <= 0 {
		a.write()
		return
	}
	if !a.pending {
		a.pending = true
		time.AfterFunc(a.delay, func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.pending = false
			a.write()
		})
	}
}

func (a *AccessLog) write() {
	b, err := json.Marshal(a.data)
	if err != nil {
		return
	}
	tmp := a.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, a.path)
	}
}

func (a *AccessLog) Add(profileID string, r AccessRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.data[profileID]
	if p == nil {
		p = &profileAccess{IPs: map[string]time.Time{}}
		a.data[profileID] = p
	}
	if _, seen := p.IPs[r.IP]; !seen {
		r.NewIP = len(p.IPs) > 0
		if len(p.IPs) >= maxKnownIPs { // 淘汰最久没出现的
			oldest := ""
			for ip, t := range p.IPs {
				if oldest == "" || t.Before(p.IPs[oldest]) {
					oldest = ip
				}
			}
			delete(p.IPs, oldest)
		}
	}
	p.IPs[r.IP] = r.Time
	p.Records = append(p.Records, r)
	if n := len(p.Records) - maxAccessRecords; n > 0 {
		p.Records = append([]AccessRecord(nil), p.Records[n:]...)
	}
	a.changed()
}

// Get 返回某个订阅组的拉取记录，最新的在前。
func (a *AccessLog) Get(profileID string) []AccessRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []AccessRecord{}
	if p := a.data[profileID]; p != nil {
		for i := len(p.Records) - 1; i >= 0; i-- {
			out = append(out, p.Records[i])
		}
	}
	return out
}

// Clear 清空某个订阅组的记录，包括记住的 IP。
func (a *AccessLog) Clear(profileID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.data[profileID]; ok {
		delete(a.data, profileID)
		a.changed()
	}
}

// Prune 去掉已不存在的订阅组的记录。
func (a *AccessLog) Prune(keep map[string]bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := len(a.data)
	for id := range a.data {
		if !keep[id] {
			delete(a.data, id)
		}
	}
	if len(a.data) != n {
		a.changed()
	}
}

func isLocalAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast()
}

// clientIP 返回来拉取订阅的客户端 IP，只用于访问记录的显示。
//
// 转发头是客户端可以随便填的，所以只在直接对端是本机或内网地址时才看它（说明前面有反向代理，
// 对端地址本身没有信息量）；直接从公网连进来的请求一律用连接的对端地址。看转发头时，
// X-Forwarded-For 从右往左跳过内网地址，取第一个公网地址：右边的是离我们近的代理加上的，
// 左边的可以由客户端伪造。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || !isLocalAddr(peer) {
		return host
	}
	parts := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	inner := "" // 全是内网地址时（内网客户端经本机反代访问），取最左边的那个
	for i := len(parts) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			break
		}
		if !isLocalAddr(a) {
			return a.Unmap().String()
		}
		inner = a.Unmap().String()
	}
	if inner != "" {
		return inner
	}
	if a, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("X-Real-IP"))); err == nil {
		return a.Unmap().String()
	}
	return peer.Unmap().String()
}

func (a *App) handleGetAccess(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"records": a.access.Get(r.URL.Query().Get("profile"))})
}

func (a *App) handleClearAccess(w http.ResponseWriter, r *http.Request) {
	a.access.Clear(r.URL.Query().Get("profile"))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
