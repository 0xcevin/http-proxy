package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Config 代理服务器配置
type Config struct {
	ListenAddr string `json:"listen_addr"` // 监听地址，如 :8080
	Auth       AuthConfig `json:"auth"`      // 认证配置
	Rules      RulesConfig `json:"rules"`    // 访问规则
	Logging    LogConfig `json:"logging"`    // 日志配置
}

// AuthConfig 认证配置
type AuthConfig struct {
	Enabled  bool              `json:"enabled"`   // 是否启用认证
	Users    map[string]string `json:"users"`     // 用户名:密码
	APITokens map[string]string `json:"api_tokens"` // API Token:用户名
}

// RulesConfig 访问规则配置
type RulesConfig struct {
	// 全局允许/拒绝的目标地址
	AllowedTargets []string `json:"allowed_targets"` // 允许访问的目标（域名或IP），空表示允许所有
	BlockedTargets []string `json:"blocked_targets"` // 拒绝访问的目标

	// 用户级别的规则
	UserRules map[string]UserRule `json:"user_rules"` // 用户名 -> 用户规则
}

// UserRule 单个用户的访问规则
type UserRule struct {
	AllowedTargets []string `json:"allowed_targets"` // 该用户允许访问的目标
	BlockedTargets []string `json:"blocked_targets"` // 该用户拒绝访问的目标
	RateLimit      RateLimitConfig `json:"rate_limit"` // 频次限制
}

// RateLimitConfig 频次限制配置
type RateLimitConfig struct {
	RequestsPerSecond float64 `json:"requests_per_second"` // 每秒请求数
	BurstSize         int     `json:"burst_size"`          // 突发容量
	DailyLimit        int     `json:"daily_limit"`         // 每日总请求限制，0表示不限制
}

// LogConfig 日志配置
type LogConfig struct {
	Enabled    bool   `json:"enabled"`     // 是否启用日志
	FilePath   string `json:"file_path"`   // 日志文件路径，空表示输出到stdout
	Format     string `json:"format"`      // 日志格式：json 或 text
}

// ProxyServer 代理服务器
type ProxyServer struct {
	config    *Config
	limiters  map[string]*rate.Limiter
	dailyCounts map[string]*DailyCounter
	mu        sync.RWMutex
	logger    *log.Logger
}

// DailyCounter 每日计数器
type DailyCounter struct {
	count int
	date  string
}

// AccessLog 访问日志结构
type AccessLog struct {
	Timestamp   string `json:"timestamp"`
	ClientIP    string `json:"client_ip"`
	User        string `json:"user"`
	Method      string `json:"method"`
	TargetURL   string `json:"target_url"`
	TargetHost  string `json:"target_host"`
	StatusCode  int    `json:"status_code"`
	BytesSent   int64  `json:"bytes_sent"`
	Duration    int64  `json:"duration_ms"`
	Allowed     bool   `json:"allowed"`
	BlockReason string `json:"block_reason,omitempty"`
}

func main() {
	// 解析命令行参数
	configPath := flag.String("config", "", "Path to config file")
	flag.Parse()

	// 加载配置
	var config *Config
	var err error

	if *configPath != "" {
		config, err = LoadConfigFromFile(*configPath)
		if err != nil {
			log.Printf("Failed to load config from %s: %v, using default config", *configPath, err)
			config = loadDefaultConfig()
		} else {
			log.Printf("Loaded config from %s", *configPath)
		}
	} else {
		config = loadDefaultConfig()
	}

	// 创建代理服务器
	server := NewProxyServer(config)

	// 启动服务器
	addr := config.ListenAddr
	if addr == "" {
		addr = ":8080"
	}

	log.Printf("Starting HTTP Proxy Server on %s", addr)
	log.Printf("Auth enabled: %v", config.Auth.Enabled)

	proxy := &http.Server{
		Addr:    addr,
		Handler: http.HandlerFunc(server.handleRequest),
	}

	log.Fatal(proxy.ListenAndServe())
}

// NewProxyServer 创建新的代理服务器
func NewProxyServer(config *Config) *ProxyServer {
	return &ProxyServer{
		config:      config,
		limiters:    make(map[string]*rate.Limiter),
		dailyCounts: make(map[string]*DailyCounter),
		logger:      log.Default(),
	}
}

// 处理代理请求
func (s *ProxyServer) handleRequest(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	clientIP := getClientIP(r)
	
	accessLog := &AccessLog{
		Timestamp: time.Now().Format(time.RFC3339),
		ClientIP:  clientIP,
		Method:    r.Method,
	}

	// 1. 认证
	user, authenticated := s.authenticate(r)
	if s.config.Auth.Enabled && !authenticated {
		s.logAccess(accessLog, http.StatusProxyAuthRequired, 0, false, "authentication required")
		w.Header().Set("Proxy-Authenticate", "Basic realm=\"Proxy\"")
		http.Error(w, "Proxy authentication required", http.StatusProxyAuthRequired)
		return
	}
	accessLog.User = user

	// 2. 解析目标地址
	targetURL, targetHost, err := s.parseTarget(r)
	if err != nil {
		s.logAccess(accessLog, http.StatusBadRequest, 0, false, "invalid target")
		http.Error(w, "Invalid target URL: "+err.Error(), http.StatusBadRequest)
		return
	}
	accessLog.TargetURL = targetURL.String()
	accessLog.TargetHost = targetHost

	// 3. 访问控制检查
	allowed, reason := s.checkAccess(user, targetHost, targetURL)
	if !allowed {
		s.logAccess(accessLog, http.StatusForbidden, 0, false, reason)
		http.Error(w, "Access denied: "+reason, http.StatusForbidden)
		return
	}

	// 4. 频次限制检查
	if !s.checkRateLimit(user) {
		s.logAccess(accessLog, http.StatusTooManyRequests, 0, false, "rate limit exceeded")
		http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	// 5. 执行代理请求
	var statusCode int
	var bytesSent int64

	if r.Method == http.MethodConnect {
		// HTTPS 代理（CONNECT 隧道）
		statusCode, bytesSent = s.handleHTTPS(w, r, targetHost, accessLog)
	} else {
		// HTTP 代理
		statusCode, bytesSent = s.handleHTTP(w, r, targetURL, accessLog)
	}

	duration := time.Since(start).Milliseconds()
	s.logAccess(accessLog, statusCode, bytesSent, true, "")
	
	if s.config.Logging.Enabled {
		log.Printf("[%s] %s %s %s -> %d (%dms)", 
			accessLog.Timestamp, user, r.Method, targetHost, statusCode, duration)
	}
}

// 认证
func (s *ProxyServer) authenticate(r *http.Request) (string, bool) {
	if !s.config.Auth.Enabled {
		return "anonymous", true
	}

	// 检查 Basic Auth
	user, pass, ok := r.BasicAuth()
	if ok {
		if expectedPass, exists := s.config.Auth.Users[user]; exists && expectedPass == pass {
			return user, true
		}
	}

	// 检查 API Token (从 Header: X-API-Token)
	token := r.Header.Get("X-API-Token")
	if token != "" {
		if username, exists := s.config.Auth.APITokens[token]; exists {
			return username, true
		}
	}

	return "", false
}

// 解析目标地址
func (s *ProxyServer) parseTarget(r *http.Request) (*url.URL, string, error) {
	var targetURL *url.URL
	var err error

	if r.Method == http.MethodConnect {
		// CONNECT 请求，目标在 URL 路径中
		targetURL, err = url.Parse("https://" + r.Host)
		if err != nil {
			return nil, "", err
		}
	} else {
		// 普通 HTTP 代理请求
		targetURL, err = url.Parse(r.URL.String())
		if err != nil {
			return nil, "", err
		}
		
		// 如果没有 scheme，可能是绝对 URL 在 RequestURI 中
		if targetURL.Scheme == "" {
			targetURL, err = url.Parse(r.RequestURI)
			if err != nil {
				return nil, "", err
			}
		}
	}

	host := targetURL.Hostname()
	if host == "" {
		host = r.Host
	}
	
	// 去掉端口
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		host = host[:idx]
	}

	return targetURL, host, nil
}

// 检查访问权限
func (s *ProxyServer) checkAccess(user, targetHost string, targetURL *url.URL) (bool, string) {
	rules := s.config.Rules

	// 检查全局黑名单
	for _, blocked := range rules.BlockedTargets {
		if matchHost(targetHost, blocked) {
			return false, "target in global blocklist"
		}
	}

	// 检查全局白名单（如果配置了）
	if len(rules.AllowedTargets) > 0 {
		allowed := false
		for _, allowedTarget := range rules.AllowedTargets {
			if matchHost(targetHost, allowedTarget) {
				allowed = true
				break
			}
		}
		if !allowed {
			return false, "target not in global allowlist"
		}
	}

	// 检查用户级别规则
	if userRule, exists := rules.UserRules[user]; exists {
		// 检查用户黑名单
		for _, blocked := range userRule.BlockedTargets {
			if matchHost(targetHost, blocked) {
				return false, "target in user blocklist"
			}
		}

		// 检查用户白名单
		if len(userRule.AllowedTargets) > 0 {
			allowed := false
			for _, allowedTarget := range userRule.AllowedTargets {
				if matchHost(targetHost, allowedTarget) {
					allowed = true
					break
				}
			}
			if !allowed {
				return false, "target not in user allowlist"
			}
		}
	}

	return true, ""
}

// 检查频次限制
func (s *ProxyServer) checkRateLimit(user string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 获取用户规则
	userRule, exists := s.config.Rules.UserRules[user]
	if !exists {
		// 没有用户规则，使用默认限制（每秒10，突发20）
		userRule = UserRule{
			RateLimit: RateLimitConfig{
				RequestsPerSecond: 10,
				BurstSize:         20,
				DailyLimit:        10000,
			},
		}
	}

	// 检查每日限制
	if userRule.RateLimit.DailyLimit > 0 {
		today := time.Now().Format("2006-01-02")
		counter, exists := s.dailyCounts[user]
		if !exists || counter.date != today {
			s.dailyCounts[user] = &DailyCounter{count: 1, date: today}
		} else {
			if counter.count >= userRule.RateLimit.DailyLimit {
				return false
			}
			counter.count++
		}
	}

	// 检查速率限制
	limiter, exists := s.limiters[user]
	if !exists {
		limiter = rate.NewLimiter(rate.Limit(userRule.RateLimit.RequestsPerSecond), userRule.RateLimit.BurstSize)
		s.limiters[user] = limiter
	}

	return limiter.Allow()
}

// 处理 HTTP 请求
func (s *ProxyServer) handleHTTP(w http.ResponseWriter, r *http.Request, targetURL *url.URL, accessLog *AccessLog) (int, int64) {
	// 创建新的请求
	req, err := http.NewRequest(r.Method, targetURL.String(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return http.StatusInternalServerError, 0
	}

	// 复制请求头
	for key, values := range r.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	// 设置 X-Forwarded-For
	clientIP := getClientIP(r)
	if prior := req.Header.Get("X-Forwarded-For"); prior != "" {
		req.Header.Set("X-Forwarded-For", prior+", "+clientIP)
	} else {
		req.Header.Set("X-Forwarded-For", clientIP)
	}

	// 执行请求
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // 不自动跟随重定向
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "Proxy error: "+err.Error(), http.StatusBadGateway)
		return http.StatusBadGateway, 0
	}
	defer resp.Body.Close()

	// 复制响应头
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	w.WriteHeader(resp.StatusCode)

	// 复制响应体
	bytesCopied, _ := io.Copy(w, resp.Body)

	return resp.StatusCode, bytesCopied
}

// 处理 HTTPS 请求 (CONNECT 隧道)
func (s *ProxyServer) handleHTTPS(w http.ResponseWriter, r *http.Request, targetHost string, accessLog *AccessLog) (int, int64) {
	// 连接到目标服务器
	targetConn, err := net.Dial("tcp", r.Host)
	if err != nil {
		http.Error(w, "Cannot connect to target: "+err.Error(), http.StatusServiceUnavailable)
		return http.StatusServiceUnavailable, 0
	}
	defer targetConn.Close()

	// 通知客户端连接已建立
	w.WriteHeader(http.StatusOK)

	// 获取底层连接
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return http.StatusInternalServerError, 0
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return http.StatusServiceUnavailable, 0
	}
	defer clientConn.Close()

	// 双向转发数据
	var bytesSent int64
	done := make(chan struct{}, 2)

	go func() {
		n, _ := io.Copy(targetConn, clientConn)
		bytesSent = n
		done <- struct{}{}
	}()

	go func() {
		io.Copy(clientConn, targetConn)
		done <- struct{}{}
	}()

	<-done

	return http.StatusOK, bytesSent
}

// 日志记录
func (s *ProxyServer) logAccess(log *AccessLog, statusCode int, bytesSent int64, allowed bool, reason string) {
	log.StatusCode = statusCode
	log.BytesSent = bytesSent
	log.Allowed = allowed
	log.BlockReason = reason

	if s.config.Logging.Format == "json" {
		data, _ := json.Marshal(log)
		fmt.Println(string(data))
	} else {
		fmt.Printf("[%s] %s %s %s -> %d allowed=%v reason=%s\n",
			log.Timestamp, log.User, log.Method, log.TargetHost, 
			statusCode, allowed, reason)
	}
}

// 获取客户端 IP
func getClientIP(r *http.Request) string {
	// 检查 X-Forwarded-For
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		ips := strings.Split(xff, ",")
		return strings.TrimSpace(ips[0])
	}

	// 检查 X-Real-IP
	xri := r.Header.Get("X-Real-IP")
	if xri != "" {
		return xri
	}

	// 使用 RemoteAddr
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

// 匹配主机名（支持通配符）
func matchHost(host, pattern string) bool {
	host = strings.ToLower(host)
	pattern = strings.ToLower(pattern)

	// 精确匹配
	if host == pattern {
		return true
	}

	// 通配符匹配，如 *.example.com
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:] // .example.com
		return strings.HasSuffix(host, suffix)
	}

	// 子串匹配
	return strings.Contains(host, pattern)
}

// 加载默认配置
func loadDefaultConfig() *Config {
	// 默认配置
	config := &Config{
		ListenAddr: ":8080",
		Auth: AuthConfig{
			Enabled: false,
			Users:   make(map[string]string),
			APITokens: make(map[string]string),
		},
		Rules: RulesConfig{
			AllowedTargets: []string{},
			BlockedTargets: []string{
				"localhost",
				"127.0.0.1",
				"::1",
				"10.0.0.0/8",
				"172.16.0.0/12",
				"192.168.0.0/16",
			},
			UserRules: make(map[string]UserRule),
		},
		Logging: LogConfig{
			Enabled:  true,
			Format:   "text",
			FilePath: "",
		},
	}

	// 这里可以从配置文件加载，简化起见使用硬编码示例
	// 启用认证
	config.Auth.Enabled = true
	config.Auth.Users["admin"] = "admin123"
	config.Auth.Users["user1"] = "password1"
	config.Auth.APITokens["token-abc-123"] = "user1"

	// 配置用户规则
	config.Rules.UserRules["user1"] = UserRule{
		AllowedTargets: []string{
			"*.google.com",
			"github.com",
			"api.github.com",
		},
		BlockedTargets: []string{
			"internal.company.com",
		},
		RateLimit: RateLimitConfig{
			RequestsPerSecond: 5,
			BurstSize:         10,
			DailyLimit:        1000,
		},
	}

	config.Rules.UserRules["admin"] = UserRule{
		AllowedTargets: []string{}, // 空表示允许所有
		BlockedTargets: []string{},
		RateLimit: RateLimitConfig{
			RequestsPerSecond: 100,
			BurstSize:         200,
			DailyLimit:        0, // 0表示无限制
		},
	}

	return config
}
