package loginserver

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/seer-game/golang-version/internal/core/logger"
	"github.com/seer-game/golang-version/internal/core/userdb"
)

// Config 登录服务器配置
type Config struct {
	LoginPort       int    `json:"login_port"`
	ServerID        int    `json:"server_id"`
	GameServerPort  int    `json:"gameserver_port"`
	LocalServerMode bool   `json:"local_server_mode"`
	UserDBPath      string `json:"user_db_path"`
}

// LoginServer 登录服务器
type LoginServer struct {
	config   Config
	userDB   *userdb.UserDB
	listener net.Listener
	clients  map[net.Conn]bool
	mu       sync.Mutex
}

// ServerInfo 服务器信息
type ServerInfo struct {
	ID        int    `json:"id"`
	UserCount int    `json:"userCount"`
	IP        string `json:"ip"`
	Port      int    `json:"port"`
	Friends   int    `json:"friends"`
}

// New 创建登录服务器实例
func New(config Config) *LoginServer {
	userDB := userdb.New(userdb.Config{
		LocalServerMode: config.LocalServerMode,
		DBPath:          config.UserDBPath,
	})

	return &LoginServer{
		config:  config,
		userDB:  userDB,
		clients: make(map[net.Conn]bool),
	}
}

// GetOnlineCount 获取在线用户数量
func (ls *LoginServer) GetOnlineCount() int {
	gameDataMap := ls.userDB.GetAllGameData()
	count := 0

	for _, data := range gameDataMap {
		if data.CurrentServer > 0 {
			count++
		}
	}

	return count
}

// GetGoodSrvList 获取可用服务器列表
func (ls *LoginServer) GetGoodSrvList() []ServerInfo {
	gamePort := ls.config.GameServerPort
	if gamePort == 0 {
		gamePort = 5000
	}

	serverID := ls.config.ServerID
	if serverID == 0 {
		serverID = 1
	}

	// 返回单个服务器，在线人数从数据库获取
	return []ServerInfo{
		{
			ID:        serverID,
			UserCount: ls.GetOnlineCount(),
			IP:        "127.0.0.1",
			Port:      gamePort,
			Friends:   1, // 固定为1，避免UI闪烁
		},
	}
}

// GetServerList 获取服务器列表
func (ls *LoginServer) GetServerList() []ServerInfo {
	return ls.GetGoodSrvList()
}

// GetMaxServerID 获取最大服务器ID
func (ls *LoginServer) GetMaxServerID() int {
	return 18
}

// Start 启动登录服务器
func (ls *LoginServer) Start() error {
	logger.Info(fmt.Sprintf("登录服务器启动在端口 %d", ls.config.LoginPort))
	logger.Info(fmt.Sprintf("服务器ID: %d", ls.config.ServerID))
	logger.Info(fmt.Sprintf("游戏服务器端口: %d", ls.config.GameServerPort))

	// 启动TCP服务器
	addr := fmt.Sprintf(":%d", ls.config.LoginPort)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("启动登录服务器失败: %v", err)
	}
	ls.listener = listener

	// 接受客户端连接
	go ls.acceptConnections()

	return nil
}

// ListenAddr 返回监听地址，用于测试或获取实际端口（如 LoginPort 为 0）
func (ls *LoginServer) ListenAddr() net.Addr {
	if ls.listener == nil {
		return nil
	}
	return ls.listener.Addr()
}

// acceptConnections 接受客户端连接
func (ls *LoginServer) acceptConnections() {
	for {
		conn, err := ls.listener.Accept()
		if err != nil {
			logger.Error(fmt.Sprintf("接受连接失败: %v", err))
			break
		}

		logger.Info(fmt.Sprintf("新登录连接: %s", conn.RemoteAddr()))

		ls.mu.Lock()
		ls.clients[conn] = true
		ls.mu.Unlock()

		go ls.handleClient(conn)
	}
}

// handleClient 处理客户端连接
func (ls *LoginServer) handleClient(conn net.Conn) {
	defer func() {
		ls.mu.Lock()
		delete(ls.clients, conn)
		ls.mu.Unlock()
		conn.Close()
	}()

	buffer := make([]byte, 4096)

	for {
		n, err := conn.Read(buffer)
		if err != nil {
			logger.Info(fmt.Sprintf("客户端断开连接: %s", conn.RemoteAddr()))
			break
		}

		if n > 0 {
			ls.handlePacket(conn, buffer[:n])
		}
	}
}

// handlePacket 处理登录数据包
func (ls *LoginServer) handlePacket(conn net.Conn, data []byte) {
	// 打印数据包的前20字节，以便分析格式
	logger.Info(fmt.Sprintf("数据包长度: %d", len(data)))
	logger.Info(fmt.Sprintf("数据包前20字节: %x", data[:min(len(data), 20)]))

	// 检查是否是Flash安全策略文件请求
	if len(data) >= 22 && string(data[:22]) == "<policy-file-request/>" {
		logger.Info("收到Flash安全策略文件请求")
		ls.sendPolicyFile(conn)
		return
	}

	// 检查是否是包含空字节终止符的Flash安全策略文件请求
	if len(data) >= 23 && string(data[:22]) == "<policy-file-request/>" && data[22] == '\x00' {
		logger.Info("收到包含空字节终止符的Flash安全策略文件请求")
		ls.sendPolicyFile(conn)
		return
	}

	// 检查是否是登录数据包
	if len(data) < 17 {
		logger.Warning("数据包长度不足")
		return
	}

	// 尝试不同的数据包格式解析
	ls.tryDifferentFormats(conn, data)
}

// sendPolicyFile 发送Flash安全策略文件
func (ls *LoginServer) sendPolicyFile(conn net.Conn) {
	policy := `<?xml version="1.0"?>
<!DOCTYPE cross-domain-policy SYSTEM "/xml/dtds/cross-domain-policy.dtd">
<cross-domain-policy>
	<allow-access-from domain="*" to-ports="*" />
</cross-domain-policy>`

	// 添加空字节作为终止符
	policy += "\x00"

	_, err := conn.Write([]byte(policy))
	if err != nil {
		logger.Error(fmt.Sprintf("发送安全策略文件失败: %v", err))
		return
	}

	logger.Info("发送Flash安全策略文件成功")

	// 保持连接打开，等待后续的登录请求
	logger.Info("等待后续的登录请求...")
}

// tryDifferentFormats 尝试不同的数据包格式
func (ls *LoginServer) tryDifferentFormats(conn net.Conn, data []byte) {
	// 打印完整的数据包内容，以便详细分析
	logger.Info(fmt.Sprintf("完整数据包: %x", data))

	// 格式1: 标准17字节头部（支持登录服命令 2–999 如 104/105/106，以及游戏服 1000+）
	if len(data) >= 17 {
		length := binary.BigEndian.Uint32(data[0:4])
		cmdID := int32(binary.BigEndian.Uint32(data[5:9]))
		userID := binary.BigEndian.Uint32(data[9:13])
		seqID := int32(binary.BigEndian.Uint32(data[13:17]))
		validCmd := (cmdID >= 1000 && cmdID <= 99999) || (cmdID >= 2 && cmdID <= 999)

		if length <= 100000 && validCmd {
			logger.Info(fmt.Sprintf("格式1解析成功: CMD=%d UID=%d SEQ=%d LEN=%d", cmdID, userID, seqID, length))
			ls.processCommand(conn, cmdID, int64(userID), seqID, data[17:])
			return
		}
	}

	// 格式2: 小端字节序
	if len(data) >= 17 {
		length := binary.LittleEndian.Uint32(data[0:4])
		cmdID := int32(binary.LittleEndian.Uint32(data[5:9]))
		userID := binary.LittleEndian.Uint32(data[9:13])
		seqID := int32(binary.LittleEndian.Uint32(data[13:17]))
		validCmd := (cmdID >= 1000 && cmdID <= 99999) || (cmdID >= 2 && cmdID <= 999)

		if length <= 100000 && validCmd {
			logger.Info(fmt.Sprintf("格式2解析成功: CMD=%d UID=%d SEQ=%d LEN=%d", cmdID, userID, seqID, length))
			ls.processCommand(conn, cmdID, int64(userID), seqID, data[17:])
			return
		}
	}

	// 格式3: 简化格式（可能是直接发送的命令）
	if len(data) >= 4 {
		cmdID := int32(binary.BigEndian.Uint32(data[0:4]))
		if cmdID >= 1000 && cmdID <= 99999 {
			logger.Info(fmt.Sprintf("格式3解析成功: CMD=%d", cmdID))
			ls.processCommand(conn, cmdID, 100000001, 1, data[4:])
			return
		}
	}

	// 格式4: 尝试解析为可能的登录请求格式
	if len(data) >= 8 {
		// 尝试解析可能的用户ID
		for i := 0; i <= len(data)-4; i++ {
			potentialUserID := binary.BigEndian.Uint32(data[i : i+4])
			if potentialUserID >= 100000000 && potentialUserID <= 999999999 {
				logger.Info(fmt.Sprintf("发现可能的用户ID: %d", potentialUserID))
				ls.defaultLoginWithUserID(conn, int64(potentialUserID))
				return
			}
		}
	}

	// 所有格式都解析失败，尝试默认登录
	logger.Warning("所有格式解析失败，尝试默认登录")
	ls.defaultLogin(conn)
}

// defaultLoginWithUserID 使用指定用户ID的默认登录
func (ls *LoginServer) defaultLoginWithUserID(conn net.Conn, userID int64) {
	// 获取或创建用户数据
	gameData := ls.userDB.GetOrCreateGameData(userID)

	// 检查用户是否存在
	user := ls.userDB.FindByUserID(userID)
	if user == nil {
		// 用户不存在，自动注册
		email := fmt.Sprintf("user%d@example.com", userID)
		password := "123456"
		newUser, err := ls.userDB.CreateUser(email, password)
		if err != nil {
			logger.Error(fmt.Sprintf("创建用户失败: %v", err))
			// 即使创建用户失败，也要继续登录流程
			// 因为用户可能已经存在，只是邮箱注册失败
			logger.Info("继续使用现有用户数据进行登录")
		} else {
			logger.Info(fmt.Sprintf("自动注册新用户: UserID=%d, Email=%s", newUser.UserID, newUser.Email))
		}
	}

	// 构建登录响应
	response := ls.buildLoginResponse(userID, gameData)

	// 发送响应
	ls.sendResponse(conn, 1001, userID, 1, response)

	// 推送服务器列表
	ls.pushServerList(conn, userID)

	// 推送频道列表
	ls.pushChannelList(conn, userID)

	logger.Info(fmt.Sprintf("默认登录成功: UserID=%d", userID))
}

// pushChannelList 推送频道列表
func (ls *LoginServer) pushChannelList(conn net.Conn, userID int64) {
	// 构建频道列表响应
	channelList := make([]byte, 4+29*40) // 29个频道，每个40字节
	index := 0

	// 频道数量
	binary.BigEndian.PutUint32(channelList[index:], 29)
	index += 4

	// 填充频道数据
	for i := 1; i <= 29; i++ {
		// 频道ID
		binary.BigEndian.PutUint32(channelList[index:], uint32(i))
		index += 4

		// 频道名称
		channelName := fmt.Sprintf("频道%d", i)
		nameBytes := []byte(channelName)
		copy(channelList[index:index+32], nameBytes)
		index += 32

		// 在线人数
		binary.BigEndian.PutUint32(channelList[index:], 100)
		index += 4
	}

	// 发送频道列表响应
	ls.sendResponse(conn, 80001, userID, 0, channelList)
	logger.Info("推送频道列表成功")
}

// processCommand 处理命令
func (ls *LoginServer) processCommand(conn net.Conn, cmdID int32, userID int64, seqID int32, body []byte) {
	switch cmdID {
	case 104: // CMD 104 MAIN_LOGIN_IN - 邮箱登录（主要登录方式）
		ls.handleEmailLogin(conn, cmdID, userID, seqID, body)
	case 1001: // 登录游戏服请求（走 1863 时的默认登录）
		ls.handleLogin(conn, cmdID, userID, seqID, body)
	case 105: // CMD 105 COMMEND_ONLINE - 客户端请求推荐服务器列表（拿游戏服 ip:port 连「频道服务器」）
		ls.handleCommendOnline(conn, userID, seqID)
	case 106: // CMD 106 RANGE_ONLINE - 按范围获取服务器列表
		ls.handleRangeOnline(conn, userID, seqID, body)
	case 108: // CMD 108 CREATE_ROLE - 创建角色
		ls.handleCreateRole(conn, cmdID, userID, seqID, body)
	case 80008: // 心跳包
		ls.handleHeartbeat(conn, cmdID, userID, seqID)
	default:
		logger.Warning(fmt.Sprintf("未处理的登录命令: %d", cmdID))
		ls.sendEmptyResponse(conn, cmdID, userID, seqID)
	}
}

// defaultLogin 默认登录处理
func (ls *LoginServer) defaultLogin(conn net.Conn) {
	// 使用默认用户ID
	defaultUserID := int64(100000001)

	// 获取或创建用户数据
	gameData := ls.userDB.GetOrCreateGameData(defaultUserID)
	// 所有用户上线时地图ID固定为1（传送舱）
	gameData.MapID = 1

	// 检查用户是否存在
	user := ls.userDB.FindByUserID(defaultUserID)
	if user == nil {
		// 用户不存在，自动注册
		email := fmt.Sprintf("user%d@example.com", defaultUserID)
		password := "123456"
		newUser, err := ls.userDB.CreateUser(email, password)
		if err != nil {
			logger.Error(fmt.Sprintf("创建用户失败: %v", err))
			// 即使创建用户失败，也要继续登录流程
			// 因为用户可能已经存在，只是邮箱注册失败
			logger.Info("继续使用现有用户数据进行登录")
		} else {
			logger.Info(fmt.Sprintf("自动注册新用户: UserID=%d, Email=%s", newUser.UserID, newUser.Email))
		}
	}

	// 构建登录响应
	response := ls.buildLoginResponse(defaultUserID, gameData)

	// 发送响应
	ls.sendResponse(conn, 1001, defaultUserID, 1, response)

	// 推送服务器列表
	ls.pushServerList(conn, defaultUserID)

	// 推送频道列表
	ls.pushChannelList(conn, defaultUserID)

	logger.Info(fmt.Sprintf("默认登录成功: UserID=%d", defaultUserID))
}

// min 返回两个整数中的较小值
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// handleLogin 处理登录请求
func (ls *LoginServer) handleLogin(conn net.Conn, cmdID int32, userID int64, seqID int32, body []byte) {
	// 检查用户是否存在
	user := ls.userDB.FindByUserID(userID)
	if user == nil {
		// 用户不存在，自动注册
		email := fmt.Sprintf("user%d@example.com", userID)
		password := "123456" // 默认密码
		newUser, err := ls.userDB.CreateUser(email, password)
		if err != nil {
			logger.Error(fmt.Sprintf("创建用户失败: %v", err))
			// 即使创建用户失败，也要继续登录流程
			// 因为用户可能已经存在，只是邮箱注册失败
			logger.Info("继续使用现有用户数据进行登录")
		} else {
			logger.Info(fmt.Sprintf("自动注册新用户: UserID=%d, Email=%s", newUser.UserID, newUser.Email))
		}
	}

	// 获取或创建用户数据
	gameData := ls.userDB.GetOrCreateGameData(userID)
	// 所有用户上线时地图ID固定为1（传送舱）
	gameData.MapID = 1

	// 构建登录响应
	response := ls.buildLoginResponse(userID, gameData)

	// 发送响应
	ls.sendResponse(conn, cmdID, userID, seqID, response)

	// 推送服务器列表
	ls.pushServerList(conn, userID)

	// 推送频道列表
	ls.pushChannelList(conn, userID)

	logger.Info(fmt.Sprintf("登录成功: UserID=%d", userID))
}

// handleEmailLogin 处理 CMD 104 邮箱登录
func (ls *LoginServer) handleEmailLogin(conn net.Conn, cmdID int32, userID int64, seqID int32, body []byte) {
	if len(body) < 96 {
		logger.Warning(fmt.Sprintf("CMD 104 包体长度不足: %d", len(body)))
		ls.sendResponseWithError(conn, 104, 0, 1, []byte{}) // errorCode=1 表示失败
		return
	}

	// 解析邮箱（64字节）和密码MD5（32字节）
	email := strings.TrimRight(string(body[0:64]), "\x00")
	passwordMD5 := strings.TrimRight(string(body[64:96]), "\x00")

	logger.Info(fmt.Sprintf("[LOGIN-104] 邮箱登录请求: email=%s", email))

	// 查找用户
	user := ls.userDB.FindByEmail(email)
	loginUserID := int64(0)
	errorCode := int32(0)

	if user != nil {
		// 验证密码（客户端发送的是MD5后的密码）
		// 如果存储的密码是原始密码，计算MD5；如果已经是MD5，直接比较
		var storedPasswordMD5 string
		if len(user.Password) == 32 {
			// 可能是MD5 hex（32字符），直接使用
			storedPasswordMD5 = user.Password
		} else {
			// 原始密码，计算MD5
			hash := md5.Sum([]byte(user.Password))
			storedPasswordMD5 = hex.EncodeToString(hash[:])
		}
		if passwordMD5 == storedPasswordMD5 || passwordMD5 == user.Password {
			// 登录成功
			loginUserID = user.UserID
			logger.Info(fmt.Sprintf("[LOGIN-104] 登录成功: userId=%d, email=%s", loginUserID, email))
		} else {
			// 密码错误
			errorCode = 5003
			logger.Warning(fmt.Sprintf("[LOGIN-104] 密码错误: email=%s (存储=%s, 收到=%s)", email, storedPasswordMD5[:16]+"...", passwordMD5[:16]+"..."))
		}
	} else {
		// 用户不存在 - 自动注册
		logger.Info(fmt.Sprintf("[LOGIN-104] 用户不存在，自动注册: email=%s", email))
		newUser, err := ls.userDB.CreateUser(email, passwordMD5)
		if err == nil && newUser != nil {
			loginUserID = newUser.UserID
			user = newUser
			logger.Info(fmt.Sprintf("[LOGIN-104] 自动注册成功: userId=%d", loginUserID))
		} else {
			errorCode = 1
			logger.Error(fmt.Sprintf("[LOGIN-104] 自动注册失败: %v", err))
		}
	}

	// 生成session（16字节随机数据）
	session := make([]byte, 16)
	if _, err := rand.Read(session); err != nil {
		logger.Error(fmt.Sprintf("生成session失败: %v", err))
		session = make([]byte, 16) // 使用全0作为fallback
	}

	// 保存session到用户数据
	if user != nil && loginUserID > 0 {
		user.Session = string(session)
		sessionHex := hex.EncodeToString(session)
		user.SessionHex = sessionHex
		ls.userDB.SaveUser(user)
		logger.Info(fmt.Sprintf("[LOGIN-104] Session已保存: %s", sessionHex))
	}

	// roleCreate: 0=未创建角色(新用户), 1=已创建角色
	roleCreate := uint32(0)
	if user != nil && user.RoleCreated {
		roleCreate = 1
	}

	// 构建登录响应体：session(16字节) + roleCreate(4字节) = 20字节
	responseBody := make([]byte, 20)
	copy(responseBody[0:16], session)
	binary.BigEndian.PutUint32(responseBody[16:20], roleCreate)

	// 发送响应（登录服头：length, version=0x31, cmd, userId, result=errorCode）
	ls.sendResponseWithError(conn, 104, loginUserID, errorCode, responseBody)

	if errorCode == 0 {
		logger.Info(fmt.Sprintf("╔══════════════════════════════════════════════════════════════╗"))
		logger.Info(fmt.Sprintf("║ ✅ 登录成功！米米号: %d", loginUserID))
		if roleCreate == 1 {
			logger.Info(fmt.Sprintf("║ 👤 角色状态: 已创建"))
		} else {
			logger.Info(fmt.Sprintf("║ 👤 角色状态: 未创建"))
		}
		logger.Info(fmt.Sprintf("╚══════════════════════════════════════════════════════════════╝"))
	}
}

// sendResponseWithError 发送带错误码的响应（登录服协议：第5个字段是result/errorCode，不是seqID）
func (ls *LoginServer) sendResponseWithError(conn net.Conn, cmdID int32, userID int64, errorCode int32, body []byte) {
	header := make([]byte, 17)
	binary.BigEndian.PutUint32(header[0:4], uint32(17+len(body)))
	header[4] = 0x31
	binary.BigEndian.PutUint32(header[5:9], uint32(cmdID))
	binary.BigEndian.PutUint32(header[9:13], uint32(userID))
	binary.BigEndian.PutUint32(header[13:17], uint32(errorCode)) // result/errorCode

	response := append(header, body...)
	_, err := conn.Write(response)
	if err != nil {
		logger.Error(fmt.Sprintf("发送响应失败: %v", err))
	}
	logger.Info(fmt.Sprintf("发送登录响应: CMD=%d UID=%d ERROR=%d LEN=%d", cmdID, userID, errorCode, len(response)))
}

// handleCreateRole 处理 CMD 108 创建角色
func (ls *LoginServer) handleCreateRole(conn net.Conn, cmdID int32, userID int64, seqID int32, body []byte) {
	if len(body) < 24 {
		logger.Warning(fmt.Sprintf("CMD 108 包体长度不足: %d", len(body)))
		ls.sendResponseWithError(conn, 108, userID, 1, []byte{}) // errorCode=1 表示失败
		return
	}

	// body 格式: userID(4字节，跳过) + nickname(16字节) + color(4字节) = 24字节
	nickname := strings.TrimRight(string(body[4:20]), "\x00")
	color := binary.BigEndian.Uint32(body[20:24])

	if nickname == "" {
		nickname = fmt.Sprintf("%d", userID)
	}

	logger.Info(fmt.Sprintf("[CREATE_ROLE] 创建角色请求: userId=%d, nickname=%s, color=%d", userID, nickname, color))

	// 查找用户
	user := ls.userDB.FindByUserID(userID)
	if user == nil {
		logger.Warning(fmt.Sprintf("[CREATE_ROLE] 用户不存在: userId=%d", userID))
		ls.sendResponseWithError(conn, 108, userID, 1, []byte{})
		return
	}

	// 标记角色已创建，保存昵称和颜色
	user.RoleCreated = true
	user.Nickname = nickname
	user.Color = int(color)
	ls.userDB.SaveUser(user)

	// 更新游戏数据中的昵称和颜色
	gameData := ls.userDB.GetOrCreateGameData(userID)
	gameData.Nick = nickname
	gameData.Color = int(color)
	ls.userDB.SaveGameData(userID, gameData)

	// 生成新的session（16字节随机数据）
	session := make([]byte, 16)
	if _, err := rand.Read(session); err != nil {
		logger.Error(fmt.Sprintf("生成session失败: %v", err))
		session = make([]byte, 16)
	}

	// 保存新session
	user.Session = string(session)
	sessionHex := hex.EncodeToString(session)
	user.SessionHex = sessionHex
	ls.userDB.SaveUser(user)

	// 返回新session (16字节)
	responseBody := make([]byte, 16)
	copy(responseBody, session)

	ls.sendResponseWithError(conn, 108, userID, 0, responseBody)

	logger.Info(fmt.Sprintf("╔══════════════════════════════════════════════════════════════╗"))
	logger.Info(fmt.Sprintf("║ ✅ 角色创建成功！米米号: %d", userID))
	logger.Info(fmt.Sprintf("║ 👤 昵称: %s, 颜色: %d", nickname, color))
	logger.Info(fmt.Sprintf("╚══════════════════════════════════════════════════════════════╝"))
}

// handleHeartbeat 处理心跳包
func (ls *LoginServer) handleHeartbeat(conn net.Conn, cmdID int32, userID int64, seqID int32) {
	ls.sendEmptyResponse(conn, cmdID, userID, seqID)
}

// handleCommendOnline 处理 CMD 105 推荐服务器列表（客户端用其中的 ip:port 连接「频道服务器」即游戏服）
func (ls *LoginServer) handleCommendOnline(conn net.Conn, userID int64, seqID int32) {
	servers := ls.GetGoodSrvList()
	body := ls.buildGoodSrvList105(servers, userID)
	ls.sendResponse(conn, 105, userID, 0, body)
	logger.Info(fmt.Sprintf("CMD 105 已返回 %d 个服务器（游戏服 127.0.0.1:%d）", len(servers), ls.config.GameServerPort))
}

// handleRangeOnline 处理 CMD 106 按范围获取服务器列表
func (ls *LoginServer) handleRangeOnline(conn net.Conn, userID int64, seqID int32, body []byte) {
	startID, endID := 1, 29
	if len(body) >= 8 {
		startID = int(binary.BigEndian.Uint32(body[0:4]))
		endID = int(binary.BigEndian.Uint32(body[4:8]))
	}
	servers := ls.GetGoodSrvList()
	if endID > len(servers) {
		endID = len(servers)
	}
	if startID < 1 {
		startID = 1
	}
	subset := make([]ServerInfo, 0, endID-startID+1)
	for i := startID; i <= endID && i-1 < len(servers); i++ {
		subset = append(subset, servers[i-1])
	}
	resBody := ls.buildSrvList106(subset)
	ls.sendResponse(conn, 106, userID, 0, resBody)
	logger.Info(fmt.Sprintf("CMD 106 已返回范围 %d-%d 共 %d 个服务器", startID, endID, len(subset)))
}

// buildGoodSrvList105 构建 CMD 105 体：maxOnlineID(4)+isVIP(4)+onlineCnt(4)+[ServerInfo 30字节]*n+friendData，与 Lua 完全一致
func (ls *LoginServer) buildGoodSrvList105(servers []ServerInfo, userID int64) []byte {
	maxID := ls.GetMaxServerID()
	if maxID < 1 {
		maxID = 18
	}
	n := len(servers)
	if n == 0 {
		servers = ls.GetGoodSrvList()
		n = len(servers)
	}
	friends := []userdb.Friend{}
	blacks := []userdb.BlacklistEntry{}
	if userID > 0 {
		friends = ls.userDB.GetFriends(userID)
		blacks = ls.userDB.GetBlacklist(userID)
	}
	friendSize := 4 + len(friends)*8 + 4 + len(blacks)*4
	total := 12 + n*30 + friendSize
	buf := make([]byte, total)
	off := 0

	binary.BigEndian.PutUint32(buf[off:], uint32(maxID))
	off += 4
	binary.BigEndian.PutUint32(buf[off:], 0) // isVIP
	off += 4
	binary.BigEndian.PutUint32(buf[off:], uint32(n))
	off += 4

	for _, s := range servers {
		binary.BigEndian.PutUint32(buf[off:], uint32(s.ID))
		off += 4
		binary.BigEndian.PutUint32(buf[off:], uint32(s.UserCount))
		off += 4
		ip := s.IP
		if ip == "" {
			ip = "127.0.0.1"
		}
		ipb := []byte(ip)
		if len(ipb) > 16 {
			ipb = ipb[:16]
		}
		copy(buf[off:off+16], ipb)
		off += 16
		binary.BigEndian.PutUint16(buf[off:], uint16(s.Port))
		off += 2
		binary.BigEndian.PutUint32(buf[off:], uint32(s.Friends))
		off += 4
	}

	binary.BigEndian.PutUint32(buf[off:], uint32(len(friends)))
	off += 4
	for _, f := range friends {
		binary.BigEndian.PutUint32(buf[off:], uint32(f.UserID))
		off += 4
		binary.BigEndian.PutUint32(buf[off:], uint32(f.TimePoke))
		off += 4
	}
	binary.BigEndian.PutUint32(buf[off:], uint32(len(blacks)))
	off += 4
	for _, b := range blacks {
		binary.BigEndian.PutUint32(buf[off:], uint32(b.UserID))
		off += 4
	}
	return buf
}

// buildSrvList106 构建 CMD 106 体：count(4)+[ServerInfo 30字节]*n，与 Lua makeSrvList 一致
func (ls *LoginServer) buildSrvList106(servers []ServerInfo) []byte {
	n := len(servers)
	buf := make([]byte, 4+n*30)
	off := 0
	binary.BigEndian.PutUint32(buf[off:], uint32(n))
	off += 4
	for _, s := range servers {
		binary.BigEndian.PutUint32(buf[off:], uint32(s.ID))
		off += 4
		binary.BigEndian.PutUint32(buf[off:], uint32(s.UserCount))
		off += 4
		ip := s.IP
		if ip == "" {
			ip = "127.0.0.1"
		}
		ipb := []byte(ip)
		if len(ipb) > 16 {
			ipb = ipb[:16]
		}
		copy(buf[off:off+16], ipb)
		off += 16
		binary.BigEndian.PutUint16(buf[off:], uint16(s.Port))
		off += 2
		binary.BigEndian.PutUint32(buf[off:], uint32(s.Friends))
		off += 4
	}
	return buf
}

// buildLoginResponse 构建登录响应
func (ls *LoginServer) buildLoginResponse(userID int64, gameData *userdb.GameData) []byte {
	// 创建固定大小的响应包
	buffer := make([]byte, 1640)
	index := 0

	// 写入昵称
	nickBytes := []byte(gameData.Nick)
	copy(buffer[index:index+32], nickBytes)
	index += 32

	// 写入颜色
	binary.BigEndian.PutUint32(buffer[index:], uint32(gameData.Color))
	index += 4

	// 写入赛尔豆
	binary.BigEndian.PutUint32(buffer[index:], uint32(gameData.Coins))
	index += 4

	// 写入能量
	binary.BigEndian.PutUint32(buffer[index:], uint32(gameData.Energy))
	index += 4

	// 写入当前地图ID
	binary.BigEndian.PutUint32(buffer[index:], uint32(gameData.MapID))
	index += 4

	// 写入坐标
	binary.BigEndian.PutUint32(buffer[index:], uint32(gameData.PosX))
	index += 4
	binary.BigEndian.PutUint32(buffer[index:], uint32(gameData.PosY))
	index += 4

	// 写入精灵数量
	binary.BigEndian.PutUint32(buffer[index:], uint32(len(gameData.Pets)))
	index += 4

	// 写入精灵数据
	for _, pet := range gameData.Pets {
		// 精灵ID
		binary.BigEndian.PutUint32(buffer[index:], uint32(pet.ID))
		index += 4

		// 精灵名称
		petNameBytes := []byte(pet.Name)
		copy(buffer[index:index+32], petNameBytes)
		index += 32

		// 等级
		binary.BigEndian.PutUint32(buffer[index:], uint32(pet.Level))
		index += 4

		// DV值
		binary.BigEndian.PutUint32(buffer[index:], uint32(pet.DV))
		index += 4

		// 性格
		binary.BigEndian.PutUint32(buffer[index:], uint32(pet.Nature))
		index += 4

		// 经验值
		binary.BigEndian.PutUint32(buffer[index:], uint32(pet.Exp))
		index += 4
	}

	// 写入服装数量
	binary.BigEndian.PutUint32(buffer[index:], uint32(len(gameData.Clothes)))
	index += 4

	// 写入服装数据
	for _, cloth := range gameData.Clothes {
		binary.BigEndian.PutUint32(buffer[index:], uint32(cloth))
		index += 4
	}

	// 写入任务数量
	binary.BigEndian.PutUint32(buffer[index:], uint32(len(gameData.Tasks)))
	index += 4

	// 写入任务数据
	for taskID, task := range gameData.Tasks {
		// 任务ID
		taskIDBytes := []byte(taskID)
		copy(buffer[index:index+16], taskIDBytes)
		index += 16

		// 任务状态
		statusBytes := []byte(task.Status)
		copy(buffer[index:index+4], statusBytes)
		index += 4
	}

	// 写入NoNo数据
	nonoBytes := []byte(gameData.Nono.Nick)
	copy(buffer[index:index+16], nonoBytes)
	index += 16

	// 填充剩余空间为0
	for i := index; i < 1640; i++ {
		buffer[i] = 0
	}

	return buffer
}

// pushServerList 推送服务器列表
func (ls *LoginServer) pushServerList(conn net.Conn, userID int64) {
	serverList := ls.GetServerList()

	// 构建服务器列表响应
	response := make([]byte, 4+len(serverList)*44)
	index := 0

	// 服务器数量
	binary.BigEndian.PutUint32(response[index:], uint32(len(serverList)))
	index += 4

	// 填充服务器数据
	for _, server := range serverList {
		// 服务器ID
		binary.BigEndian.PutUint32(response[index:], uint32(server.ID))
		index += 4

		// 服务器名称
		serverName := fmt.Sprintf("服务器%d", server.ID)
		nameBytes := []byte(serverName)
		copy(response[index:index+32], nameBytes)
		index += 32

		// 在线人数
		binary.BigEndian.PutUint32(response[index:], uint32(server.UserCount))
		index += 4

		// 服务器状态
		binary.BigEndian.PutUint32(response[index:], 1) // 1表示正常
		index += 4
	}

	// 发送服务器列表响应
	ls.sendResponse(conn, 80002, userID, 0, response)
}

// sendResponse 发送响应（登录服头与 Lua 一致：length(4)+version(1)=0x31+cmd(4)+userId(4)+result(4)）
func (ls *LoginServer) sendResponse(conn net.Conn, cmdID int32, userID int64, seqID int32, body []byte) {
	header := make([]byte, 17)
	binary.BigEndian.PutUint32(header[0:4], uint32(17+len(body)))
	header[4] = 0x31 // 与 Lua 登录服一致
	binary.BigEndian.PutUint32(header[5:9], uint32(cmdID))
	binary.BigEndian.PutUint32(header[9:13], uint32(userID))
	binary.BigEndian.PutUint32(header[13:17], uint32(seqID)) // result/seq，105/106 时传 0

	response := append(header, body...)
	_, err := conn.Write(response)
	if err != nil {
		logger.Error(fmt.Sprintf("发送响应失败: %v", err))
	}
	logger.Info(fmt.Sprintf("发送登录响应: CMD=%d UID=%d SEQ=%d LEN=%d", cmdID, userID, seqID, len(response)))
}

// sendEmptyResponse 发送空响应
func (ls *LoginServer) sendEmptyResponse(conn net.Conn, cmdID int32, userID int64, seqID int32) {
	ls.sendResponse(conn, cmdID, userID, seqID, []byte{})
}

// Stop 停止登录服务器
func (ls *LoginServer) Stop() error {
	// 保存用户数据
	ls.userDB.SaveToFile()

	// 关闭listener
	if ls.listener != nil {
		ls.listener.Close()
	}

	// 关闭所有客户端连接
	ls.mu.Lock()
	for conn := range ls.clients {
		conn.Close()
	}
	ls.clients = make(map[net.Conn]bool)
	ls.mu.Unlock()

	return nil
}

// GetUserDB 获取用户数据库
func (ls *LoginServer) GetUserDB() *userdb.UserDB {
	return ls.userDB
}
