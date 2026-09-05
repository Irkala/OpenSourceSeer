package resserver

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/seer-game/golang-version/internal/core/logger"
)

// Config 资源服务器配置
type Config struct {
	ResPort              int    `json:"res_port"`
	ResPort80            int    `json:"res_port_80"`
	ResDir               string `json:"res_dir"`
	ResProxyDir          string `json:"res_proxy_dir"`
	ResOfficialAddress   string `json:"res_official_address"`
	LocalServerMode      bool   `json:"local_server_mode"`
	UseOfficialResources bool   `json:"use_official_resources"`
	PureOfficialMode     bool   `json:"pure_official_mode"`
	LoginPort            int    `json:"login_port"`
	LoginServerAddress   string `json:"login_server_address"`
	OfficialLoginServer  string `json:"official_login_server"`
	OfficialLoginPort    int    `json:"official_login_port"`
}

// ResourceServer 资源服务器
type ResourceServer struct {
	config   Config
	server   *http.Server
	server80 *http.Server
}

// tryItemIconForPet 尝试将 groupFightResource/pet/XXXX.swf 映射到「道具图标」：
// - 精灵道具（300000-399999）：resource/item/petItem/icon/XXXX.swf
// - 元素/扭蛋牌等老道具：resource/fitment/icon/XXXX.swf
// 目前主要用于扭蛋/奖励界面中，客户端错误地按精灵处理的道具。
func (rs *ResourceServer) tryItemIconForPet(path string) (string, bool) {
	base := filepath.Base(path)
	if !strings.HasSuffix(base, ".swf") {
		return "", false
	}
	idStr := strings.TrimSuffix(base, ".swf")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		return "", false
	}
	// 只处理明显是「道具ID」的范围，避免误伤真正的组队战精灵（通常是三位数 ID）
	if id < 300000 && !(id >= 500001 && id <= 500014) && id != 400501 && id != 400505 {
		return "", false
	}

	var iconPath string
	if id >= 300000 && id <= 399999 {
		// 精灵道具：使用精灵道具图标路径
		iconPath = filepath.Join(rs.config.ResDir, "resource", "item", "petItem", "icon", idStr+".swf")
	} else {
		// 元素/扭蛋牌等老道具：仍然使用 fitment/icon
		iconPath = filepath.Join(rs.config.ResDir, "resource", "fitment", "icon", idStr+".swf")
	}

	if _, err := os.Stat(iconPath); err == nil {
		return iconPath, true
	}
	return "", false
}

// New 创建资源服务器实例
func New(config Config) *ResourceServer {
	// 确保资源目录存在
	if err := os.MkdirAll(config.ResDir, 0755); err != nil {
		logger.Error(fmt.Sprintf("创建资源目录失败: %v", err))
	}

	// 确保代理目录存在
	if err := os.MkdirAll(config.ResProxyDir, 0755); err != nil {
		logger.Error(fmt.Sprintf("创建代理目录失败: %v", err))
	}

	return &ResourceServer{
		config: config,
	}
}

// Start 启动资源服务器
func (rs *ResourceServer) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", rs.handleRequest)

	// 启动主服务器
	addr := fmt.Sprintf(":%d", rs.config.ResPort)
	rs.server = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	logger.Info(fmt.Sprintf("资源服务器启动在端口 %d", rs.config.ResPort))

	go func() {
		if err := rs.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error(fmt.Sprintf("资源服务器启动失败: %v", err))
		}
	}()

	// 尝试启动备用 HTTP 端口（原 80 需管理员权限，可改为 8088 等）
	if rs.config.ResPort80 > 0 {
		rs.server80 = &http.Server{
			Addr:    fmt.Sprintf(":%d", rs.config.ResPort80),
			Handler: mux,
		}

		go func() {
			if err := rs.server80.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Warning(fmt.Sprintf("备用资源端口 %d 启动失败: %v", rs.config.ResPort80, err))
			}
		}()
	}

	return nil
}

// Stop 停止资源服务器
func (rs *ResourceServer) Stop() error {
	if rs.server != nil {
		if err := rs.server.Close(); err != nil {
			return err
		}
	}

	if rs.server80 != nil {
		if err := rs.server80.Close(); err != nil {
			return err
		}
	}

	return nil
}

// handleRequest 处理HTTP请求
func (rs *ResourceServer) handleRequest(w http.ResponseWriter, r *http.Request) {
	// 设置CORS头
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")

	// 处理OPTIONS请求
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	path := r.URL.Path

	// 过滤不需要的请求
	if strings.HasSuffix(path, "favicon.ico") || strings.HasSuffix(path, "logo.png") {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 处理特殊路径
	switch {
	case path == "/config/ServerR.xml":
		rs.handleServerRXML(w, r)
		return
	case strings.HasSuffix(path, "/dll/NieoCore.swf"):
		rs.handleNieoCore(w, r)
		return
	case path == "/__log__":
		rs.handleJSLog(w, r)
		return
	case path == "/ip.txt" || path == "/ip":
		rs.handleIPText(w, r)
		return
	}

	// 处理SPA路由
	if rs.isSPARoute(path) {
		path = "/index.html"
	}

	// 针对部分客户端误把「道具ID」当作「精灵ID」去加载
	// /resource/groupFightResource/pet/XXXX.swf 的情况，这里尝试将其
	// 重定向到道具图标资源 /resource/fitment/icon/XXXX.swf，避免 404 与错误的“获得精灵”表现。
	if strings.HasPrefix(path, "/resource/groupFightResource/pet/") {
		if iconPath, ok := rs.tryItemIconForPet(path); ok {
			rs.serveFile(w, r, iconPath, path)
			logger.Info(fmt.Sprintf("[重定向] %s -> 使用道具图标 SWF %s", path, iconPath))
			return
		}
	}

	// 解析代理规则
	filePath, code := rs.resolvePathByProxyRules(path)

	if code == http.StatusNotFound {
		logger.Warning(fmt.Sprintf("[404][PROXY_RULES] %s -> INVISIBLE", path))
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("the file is defined as invisible by proxy_rules"))
		return
	}

	// 检查文件是否存在
	if _, err := os.Stat(filePath); err == nil {
		// 文件存在，直接提供
		rs.serveFile(w, r, filePath, path)
		return
	}

	// 对战/稀有精灵 SWF 缺失时用默认精灵回退，避免加载卡在 12%
	if fallbackPath := rs.fightPetFallback(path, filePath); fallbackPath != "" {
		rs.serveFile(w, r, fallbackPath, path)
		logger.Info(fmt.Sprintf("[回退] %s -> 使用默认精灵 SWF", path))
		return
	}

	// 文件不存在且启用了官方资源，从官服下载
	if rs.config.UseOfficialResources {
		rs.fetchFromOfficial(w, r, path)
		return
	}

	// 文件不存在且未启用官方资源，提供默认响应
	if path == "/" || path == "/index.html" {
		// 提供默认的HTML响应
		defaultHTML := `
<!DOCTYPE html>
<html lang="zh-CN">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>赛尔号怀旧服</title>
    <style>
        body {
            font-family: Arial, sans-serif;
            background-color: #f0f0f0;
            margin: 0;
            padding: 20px;
        }
        .container {
            max-width: 800px;
            margin: 0 auto;
            background-color: white;
            padding: 40px;
            border-radius: 8px;
            box-shadow: 0 2px 10px rgba(0,0,0,0.1);
        }
        h1 {
            color: #333;
            text-align: center;
        }
        p {
            color: #666;
            line-height: 1.6;
        }
        .server-info {
            background-color: #f8f8f8;
            padding: 20px;
            border-radius: 4px;
            margin: 20px 0;
        }
        .port {
            font-family: monospace;
            background-color: #e8e8e8;
            padding: 2px 6px;
            border-radius: 3px;
        }
    </style>
</head>
<body>
    <div class="container">
        <h1>赛尔号怀旧服</h1>
        <p>欢迎来到赛尔号怀旧服！这是一个基于Go语言开发的赛尔号私人服务器。</p>
        
        <div class="server-info">
            <h3>服务器信息</h3>
            <p>游戏服务器: <span class="port">127.0.0.1:5000</span></p>
            <p>资源服务器: <span class="port">127.0.0.1:32400</span></p>
            <p>登录IP服务器: <span class="port">127.0.0.1:32401</span></p>
            <p>登录服务器: <span class="port">127.0.0.1:1863</span></p>
        </div>
        
        <p><strong>提示：</strong>请确保您的客户端已正确配置，指向上述服务器地址。</p>
        
        <p>© 2026 赛尔号怀旧服</p>
    </div>
</body>
</html>
		`
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(defaultHTML))
		return
	}

	// 其他文件不存在，返回404
	logger.Warning(fmt.Sprintf("[404] %s (resolved=%s) official=%v", path, filePath, rs.config.UseOfficialResources))
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte(fmt.Sprintf("File not found: %s", path)))
}

// handleServerRXML 处理ServerR.xml请求
func (rs *ResourceServer) handleServerRXML(w http.ResponseWriter, r *http.Request) {
	if rs.config.LocalServerMode {
		// 本地模式：使用本地配置文件
		configFile := filepath.Join(rs.config.ResProxyDir, "config", "ServerR.xml")

		// 检查文件是否存在
		if _, err := os.Stat(configFile); err == nil {
			// 文件存在，直接使用
			rs.serveFile(w, r, configFile, "/config/ServerR.xml")
			return
		}

		// 文件不存在，生成默认配置
		rs.generateDefaultServerRXML(w, r, configFile)
		return
	}

	// 官服代理模式：从官服获取并修改
	cachedFile := filepath.Join(rs.config.ResProxyDir, "config", "ServerOfficial.xml")

	// 确保config目录存在
	if err := os.MkdirAll(filepath.Dir(cachedFile), 0755); err != nil {
		logger.Error(fmt.Sprintf("创建配置目录失败: %v", err))
	}

	// 检查缓存文件是否存在
	if _, err := os.Stat(cachedFile); err == nil {
		// 缓存文件存在，直接使用
		rs.serveFile(w, r, cachedFile, "/config/ServerR.xml")
		return
	}

	// 从官服获取
	rs.fetchAndModifyServerRXML(w, r, cachedFile)
}

// handleNieoCore 处理NieoCore.swf请求
func (rs *ResourceServer) handleNieoCore(w http.ResponseWriter, r *http.Request) {
	var coreFile string
	if rs.config.LocalServerMode {
		// 本地模式：使用NieoCore.swf
		coreFile = filepath.Join(rs.config.ResDir, "dll", "NieoCore.swf")
	} else {
		// 官服模式：使用NieoCore2.swf
		coreFile = filepath.Join(rs.config.ResDir, "dll", "NieoCore2.swf")
	}

	rs.serveFile(w, r, coreFile, r.URL.Path)
}

// handleJSLog 处理JavaScript日志请求
func (rs *ResourceServer) handleJSLog(w http.ResponseWriter, r *http.Request) {
	logType := r.URL.Query().Get("type")
	logUrl := r.URL.Query().Get("url")

	// 打印JavaScript网络日志
	switch logType {
	case "Fetch":
		logger.Info(fmt.Sprintf("[JS-Fetch] %s", logUrl))
	case "XHR":
		logger.Info(fmt.Sprintf("[JS-XHR] %s", logUrl))
	case "WebSocket":
		logger.Info(fmt.Sprintf("[JS-WebSocket] 连接: %s", logUrl))
	case "WebSocket-Open":
		logger.Info(fmt.Sprintf("[JS-WebSocket] 已连接: %s", logUrl))
	}

	// 返回空响应
	w.WriteHeader(http.StatusNoContent)
}

// handleIPText 处理ip.txt请求
func (rs *ResourceServer) handleIPText(w http.ResponseWriter, r *http.Request) {
	var resp string

	if rs.config.LocalServerMode {
		// 本地模式：返回 TCP 登录服务器地址，客户端用此建连后发 104/105，再根据 105 里的游戏服连「频道服务器」
		resp = fmt.Sprintf("127.0.0.1:1863")
		logger.Info(fmt.Sprintf("[Local Mode] ✓ 返回登录服务器地址(TCP): %s", resp))
	} else if rs.config.PureOfficialMode && rs.config.UseOfficialResources {
		// 完全官服模式：返回官服的ip.txt
		resp = rs.fetchOfficialIPText()
	} else {
		// 官服代理模式：返回本地代理地址
		resp = rs.config.LoginServerAddress
		logger.Info(fmt.Sprintf("[官服代理模式] ✓ 返回本地代理地址: %s", resp))

		// 异步获取官服ip.txt作为参考
		if rs.config.UseOfficialResources {
			go rs.fetchOfficialIPTextForReference()
		}
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(resp))
}

// fightPetFallback 对战精灵 SWF 缺失时返回默认精灵路径，避免卡 12%
// 匹配 /resource/fightResource/pet/swf/XXXX.swf 或 /resource/groupFightResource/pet/XXXX.swf
func (rs *ResourceServer) fightPetFallback(path, _ string) string {
	// 按优先级尝试常见精灵 ID（玩家出战常用 009 等，资源通常存在）
	fallbackIDs := []string{"009", "013", "007", "035", "005"}
	if !strings.HasSuffix(path, ".swf") {
		return ""
	}
	var baseDir string
	if strings.HasPrefix(path, "/resource/fightResource/pet/swf/") {
		baseDir = filepath.Join(rs.config.ResDir, "resource", "fightResource", "pet", "swf")
	} else if strings.HasPrefix(path, "/resource/groupFightResource/pet/") {
		baseDir = filepath.Join(rs.config.ResDir, "resource", "groupFightResource", "pet")
	} else {
		return ""
	}
	for _, id := range fallbackIDs {
		p := filepath.Join(baseDir, id+".swf")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// resolvePathByProxyRules 解析代理规则
func (rs *ResourceServer) resolvePathByProxyRules(path string) (string, int) {
	// 代理规则
	proxyRules := map[string]interface{}{
		"/":                                 "/index.html",
		"/index.html":                       "PROXY",
		"/config/ServerR.xml":               "DYNAMIC_SERVER_CONFIG",
		"/config/ServerOfficial.xml":        "PROXY",
		"/config/ServerLocal.xml":           "PROXY",
		"/config/doorConfig.xml":            "PROXY",
		"/crossdomain.xml":                  "PROXY",
		"/js/swfobject.js":                  "PROXY",
		"/js/server-config.js":              "PROXY",
		"/js/client-emulator.js":            "PROXY",
		"/resource/login/Advertisement.swf": "INVISIBLE",
	}

	if rule, exists := proxyRules[path]; exists {
		switch v := rule.(type) {
		case string:
			if v == "PROXY" {
				return filepath.Join(rs.config.ResProxyDir, path), http.StatusOK
			} else if v == "INVISIBLE" {
				return "", http.StatusNotFound
			} else if v == "DYNAMIC_SERVER_CONFIG" {
				return "", http.StatusOK
			} else {
				// 递归解析
				return rs.resolvePathByProxyRules(v)
			}
		}
	}

	// 默认规则
	return filepath.Join(rs.config.ResDir, path), http.StatusOK
}

// isSPARoute 判断是否为SPA路由
func (rs *ResourceServer) isSPARoute(path string) bool {
	spaRoutes := map[string]bool{
		"/game": true,
	}
	return spaRoutes[path]
}

// serveFile 提供文件
func (rs *ResourceServer) serveFile(w http.ResponseWriter, r *http.Request, filePath, originalPath string) {
	// 检查文件是否存在
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(fmt.Sprintf("File not found: %s", originalPath)))
		return
	}

	// 检查是否为文件
	if !fileInfo.Mode().IsRegular() {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(fmt.Sprintf("Requested url is not a file: %s", originalPath)))
		return
	}

	// 0 字节占位文件：视为缺失。若开启官服资源，则直接拉取并缓存覆盖，避免地图/NPC 缺失导致流程无法进行。
	if fileInfo.Size() == 0 && rs.config.UseOfficialResources && rs.config.ResOfficialAddress != "" {
		logger.Warning(fmt.Sprintf("[0B] %s 是0字节占位文件，尝试从官服重新拉取", originalPath))
		rs.fetchFromOfficial(w, r, originalPath)
		return
	}

	// 某些资源在民间包里被 0 字节占位（典型：/resource/newNpc/multi/0.swf），会导致客户端缺少 NPC/剧情素材。
	// 若检测到 0 字节，尝试回退到同目录下的非空 SWF，至少避免“看不到 NPC / 触发点”。
	// 注：这不是最终解决方案，最终应补齐正确的 0.swf 资源；但该回退能让流程继续。
	if fileInfo.Size() == 0 && strings.HasSuffix(strings.ToLower(originalPath), "/resource/newnpc/multi/0.swf") {
		dir := filepath.Dir(filePath)
		candidates := []string{
			filepath.Join(dir, "10.swf"),
			filepath.Join(dir, "50.swf"),
			filepath.Join(dir, "90000.swf"),
		}
		for _, c := range candidates {
			if fi, e := os.Stat(c); e == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
				logger.Warning(fmt.Sprintf("[回退][0B] %s 是0字节，占位文件；回退到 %s (%d bytes)", originalPath, c, fi.Size()))
				filePath = c
				fileInfo = fi
				break
			}
		}
	}

	// 获取文件类型
	contentType := rs.getContentType(filePath)

	// 设置响应头
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", fileInfo.Size()))

	// 记录日志
	logger.Info(fmt.Sprintf("[GET] %s %d bytes", originalPath, fileInfo.Size()))

	// 如果是SWF文件，在控制台显示
	if strings.HasSuffix(strings.ToLower(originalPath), ".swf") {
		logger.Info(fmt.Sprintf("[SWF加载] %s (%d bytes)", originalPath, fileInfo.Size()))
	}

	// 提供文件
	file, err := os.Open(filePath)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf("Error opening file: %v", err)))
		return
	}
	defer file.Close()

	io.Copy(w, file)
}

// getContentType 获取文件类型
func (rs *ResourceServer) getContentType(filePath string) string {
	ext := strings.ToLower(filepath.Ext(filePath))
	if ext == "" {
		return "application/octet-stream"
	}

	// 移除点号
	ext = ext[1:]

	// MIME类型映射
	mimes := map[string]string{
		"html": "text/html",
		"css":  "text/css",
		"js":   "application/javascript",
		"xml":  "application/xml",
		"swf":  "application/x-shockwave-flash",
		"png":  "image/png",
		"jpg":  "image/jpeg",
		"jpeg": "image/jpeg",
		"gif":  "image/gif",
		"txt":  "text/plain",
	}

	if contentType, exists := mimes[ext]; exists {
		return contentType
	}

	return "application/octet-stream"
}

// fetchFromOfficial 从官服获取文件
func (rs *ResourceServer) fetchFromOfficial(w http.ResponseWriter, r *http.Request, path string) {
	officialURL := rs.config.ResOfficialAddress + path
	logger.Info(fmt.Sprintf("[官服下载] 开始下载: %s", officialURL))

	// 发送请求到官服
	resp, err := http.Get(officialURL)
	if err != nil {
		logger.Error(fmt.Sprintf("[官服下载] 请求错误: %v", err))
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf("Error fetching from official: %v", err)))
		return
	}
	defer resp.Body.Close()

	// 检查响应状态
	if resp.StatusCode != http.StatusOK {
		logger.Error(fmt.Sprintf("[官服下载] 响应错误: status=%d", resp.StatusCode))
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(fmt.Sprintf("File not found on official server: %s", path)))
		return
	}

	// 读取响应体
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Error(fmt.Sprintf("[官服下载] 读取错误: %v", err))
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf("Error reading response: %v", err)))
		return
	}

	logger.Info(fmt.Sprintf("[官服下载] 响应: status=%d, size=%d bytes", resp.StatusCode, len(body)))

	// 保存文件
	savePath := filepath.Join(rs.config.ResDir, path)
	if err := rs.saveFile(savePath, body); err != nil {
		logger.Error(fmt.Sprintf("[官服下载] 保存错误: %v", err))
	}

	// 提供文件
	w.Header().Set("Content-Type", rs.getContentType(path))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.Write(body)
}

// saveFile 保存文件
func (rs *ResourceServer) saveFile(filePath string, data []byte) error {
	// 确保目录存在
	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		return err
	}

	// 写入文件
	return os.WriteFile(filePath, data, 0644)
}

// fetchOfficialIPText 获取官服的ip.txt
func (rs *ResourceServer) fetchOfficialIPText() string {
	officialURL := rs.config.ResOfficialAddress + "/ip.txt"

	resp, err := http.Get(officialURL)
	if err != nil || resp.StatusCode != http.StatusOK {
		return rs.config.LoginServerAddress
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return rs.config.LoginServerAddress
	}

	// 官服可能返回多个服务器地址，用 | 分隔，取第一个
	respStr := strings.TrimSpace(string(body))
	if parts := strings.Split(respStr, "|"); len(parts) > 0 {
		respStr = parts[0]
	}

	// 保存官服的ip.txt到本地
	ipFilePath := filepath.Join(rs.config.ResDir, "ip.txt.official")
	if err := rs.saveFile(ipFilePath, []byte(respStr)); err != nil {
		logger.Error(fmt.Sprintf("保存官服ip.txt失败: %v", err))
	}

	return respStr
}

// fetchOfficialIPTextForReference 异步获取官服ip.txt作为参考
func (rs *ResourceServer) fetchOfficialIPTextForReference() {
	officialURL := "https://seerlogin.61.com/ip.txt"

	resp, err := http.Get(officialURL)
	if err != nil || resp.StatusCode != http.StatusOK {
		logger.Error(fmt.Sprintf("获取官服ip.txt失败: %v", err))
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Error(fmt.Sprintf("读取官服ip.txt失败: %v", err))
		return
	}

	respStr := strings.TrimSpace(string(body))
	logger.Info(fmt.Sprintf("[官服代理模式] 官服 ip.txt: %s", respStr))

	// 保存官服的ip.txt到本地（作为参考）
	ipFilePath := filepath.Join(rs.config.ResDir, "ip.txt.official")
	if err := rs.saveFile(ipFilePath, []byte(respStr)); err != nil {
		logger.Error(fmt.Sprintf("保存官服ip.txt失败: %v", err))
	}
}

// generateDefaultServerRXML 生成默认的ServerR.xml配置
func (rs *ResourceServer) generateDefaultServerRXML(w http.ResponseWriter, r *http.Request, configFile string) {
	// 生成默认的ServerR.xml配置
	defaultConfig := `<?xml version="1.0" encoding="utf-8"?>
<Servers>
  <ipConfig>
    <Email ip="127.0.0.1" port="32401" />
    <DirSer ip="127.0.0.1" port="32401" />
    <Visitor ip="127.0.0.1" port="32401" />
    <SubServer ip="127.0.0.1" port="32401" />
    <RegistSer ip="127.0.0.1" port="32401" />
  </ipConfig>
  <version>1.0.0.0</version>
  <clientUrl>http://127.0.0.1:%d/Client.swf</clientUrl>
  <loginUrl>http://127.0.0.1:%d/login/Login.swf</loginUrl>
  <newsUrl>http://127.0.0.1:%d/news.html</newsUrl>
  <patchUrl>http://127.0.0.1:%d/patch</patchUrl>
  <helpUrl>http://127.0.0.1:%d/help</helpUrl>
  <bugUrl>http://127.0.0.1:%d/bug</bugUrl>
  <vipUrl>http://127.0.0.1:%d/vip</vipUrl>
  <payUrl>http://127.0.0.1:%d/pay</payUrl>
  <eventUrl>http://127.0.0.1:%d/event</eventUrl>
  <teamUrl>http://127.0.0.1:%d/team</teamUrl>
  <clubUrl>http://127.0.0.1:%d/club</clubUrl>
  <minigameUrl>http://127.0.0.1:%d/minigame</minigameUrl>
  <gamepassUrl>http://127.0.0.1:%d/gamepass</gamepassUrl>
  <activityUrl>http://127.0.0.1:%d/activity</activityUrl>
  <serverList>
    <server id="1" name="服务器1" ip="127.0.0.1" port="5000" status="1" />
  </serverList>
</Servers>`

	// 替换配置参数
	resPort := rs.config.ResPort
	modifiedData := fmt.Sprintf(defaultConfig, resPort, resPort, resPort, resPort, resPort, resPort, resPort, resPort, resPort, resPort, resPort, resPort, resPort)

	// 保存到本地
	if err := rs.saveFile(configFile, []byte(modifiedData)); err != nil {
		logger.Error(fmt.Sprintf("保存默认ServerR.xml失败: %v", err))
	} else {
		logger.Info(fmt.Sprintf("[CONFIG] ✓ 生成默认配置到 %s", configFile))
	}

	// 提供生成的文件
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(modifiedData)))
	w.Write([]byte(modifiedData))
}

// fetchAndModifyServerRXML 从官服获取并修改ServerR.xml
func (rs *ResourceServer) fetchAndModifyServerRXML(w http.ResponseWriter, r *http.Request, cachedFile string) {
	officialURL := rs.config.ResOfficialAddress + "/config/ServerR.xml"

	resp, err := http.Get(officialURL)
	if err != nil || resp.StatusCode != http.StatusOK {
		logger.Error(fmt.Sprintf("获取官服ServerR.xml失败: %v", err))
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf("Failed to fetch ServerR.xml: %v", err)))
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Error(fmt.Sprintf("读取官服ServerR.xml失败: %v", err))
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf("Failed to read ServerR.xml: %v", err)))
		return
	}

	// 修改ipConfig，让所有Socket连接走本地代理
	modifiedData := string(body)
	proxyPort := fmt.Sprintf("%d", rs.config.LoginPort)

	// 替换IP地址
	modifiedData = strings.ReplaceAll(modifiedData, "<Email ip=\"", "<Email ip=\"127.0.0.1\"")
	modifiedData = strings.ReplaceAll(modifiedData, "<DirSer ip=\"", "<DirSer ip=\"127.0.0.1\"")
	modifiedData = strings.ReplaceAll(modifiedData, "<Visitor ip=\"", "<Visitor ip=\"127.0.0.1\"")
	modifiedData = strings.ReplaceAll(modifiedData, "<SubServer ip=\"", "<SubServer ip=\"127.0.0.1\"")
	modifiedData = strings.ReplaceAll(modifiedData, "<RegistSer ip=\"", "<RegistSer ip=\"127.0.0.1\"")

	// 替换端口
	modifiedData = strings.ReplaceAll(modifiedData, "<Email port=\"", fmt.Sprintf("<Email port=\"%s\"", proxyPort))
	modifiedData = strings.ReplaceAll(modifiedData, "<DirSer port=\"", fmt.Sprintf("<DirSer port=\"%s\"", proxyPort))
	modifiedData = strings.ReplaceAll(modifiedData, "<Visitor port=\"", fmt.Sprintf("<Visitor port=\"%s\"", proxyPort))
	modifiedData = strings.ReplaceAll(modifiedData, "<SubServer port=\"", fmt.Sprintf("<SubServer port=\"%s\"", proxyPort))
	modifiedData = strings.ReplaceAll(modifiedData, "<RegistSer port=\"", fmt.Sprintf("<RegistSer port=\"%s\"", proxyPort))

	logger.Info(fmt.Sprintf("[CONFIG] 已修改 ipConfig -> 127.0.0.1:%s", proxyPort))

	// 保存到本地缓存
	if err := rs.saveFile(cachedFile, []byte(modifiedData)); err != nil {
		logger.Error(fmt.Sprintf("缓存ServerR.xml失败: %v", err))
	} else {
		logger.Info(fmt.Sprintf("[CONFIG] ✓ 已缓存到 %s", cachedFile))
	}

	// 提供修改后的文件
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(modifiedData)))
	w.Write([]byte(modifiedData))
}
