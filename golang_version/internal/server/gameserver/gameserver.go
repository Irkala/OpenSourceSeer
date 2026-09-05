package gameserver

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/seer-game/golang-version/internal/core/logger"
	"github.com/seer-game/golang-version/internal/core/packet"
	"github.com/seer-game/golang-version/internal/core/userdb"
	"github.com/seer-game/golang-version/internal/game/mapogres"
)

const flashPolicyRequest = "<policy-file-request/>"

// ClientData 客户端数据
type ClientData struct {
	Socket         net.Conn
	Buffer         []byte
	UserID         int64
	Session        string
	SeqID          int32
	HeartbeatTimer *time.Timer
	LoggedIn       bool
}

// BattleState 战斗状态
// 与 Lua fight_handlers 对齐：支持 status(20) 与 battleLv(6) 用于技能附加效果（烧伤、属性升降等）
type BattleState struct {
	EnemyHP     uint32
	EnemyMaxHP  uint32
	PlayerHP    uint32
	PlayerMaxHP uint32
	EnemyID     int
	EnemyLevel  int
	// EnemyDisplayName 敌方显示名（如地图 461 二当家），2503 中用于 FighetUserInfo.nick，空则客户端用精灵名
	EnemyDisplayName string
	// EnemyShiny 敌方异色：0=非异色，1=异色（用于 2503 包体，地图野怪来自槽位 Shiny，BOSS 可为 0 或 1）
	EnemyShiny uint32
	// ActivePetIndex 当前出战精灵在 user.Pets 切片中的下标（0 开始），用于 2405 伤害计算/经验结算等
	// 默认 0；切换精灵(2407) 时更新，但不改变 user.Pets 本身的顺序，这样战斗结束后背包首发仍然与客户端一致。
	ActivePetIndex int
	// ActivePetCatchTime 当前出战精灵的 CatchTime，用于在结算/同步时精确定位这只精灵（即使期间背包顺序发生变化）。
	ActivePetCatchTime int
	// TotalPlayerPets 本场战斗开始时玩家背包中可用精灵总数（用于判断“是否还有后备精灵”）
	TotalPlayerPets int
	// DeadPlayerPets 当前已被击败的精灵数量（仅在 PlayerHP 从 >0 变为 0 时累加）
	DeadPlayerPets int
	// TotalEnemyPets PvP 时对方精灵总数；仅当 OpponentUserID!=0 时有效，用于多精灵 PvP 判定“对方是否全灭”
	TotalEnemyPets int
	// DeadEnemyPets PvP 时已被击败的对方精灵数量（敌方当前精灵被击败时累加）；DeadEnemyPets>=TotalEnemyPets 时己方获胜
	DeadEnemyPets int
	IsActive       bool
	// OpponentUserID PvP 时对方 UID，0 表示 PvE
	OpponentUserID int64
	// PvPHasSelected / PvPSelectedSkillID：仅 PvP 使用。
	// - 每当客户端发送 2405 时，先在各自 BattleState 上记录“本回合选择的技能”；
	// - 只有当双方都已选择（两个 BattleState 的 PvPHasSelected 均为 true）时，
	//   才由“第二个到达的 2405”触发一次完整的回合结算与 2505 广播。
	PvPHasSelected      bool
	PvPSelectedSkillID  uint32
	// PvPChangedPetThisRound：仅 PvP 使用。
	// - 当客户端发送 2407 主动切换精灵时，标记为 true；
	// - 当双方在同一回合都仅切换精灵且未出招时，由第二个 2407 触发一次“零伤害 2505”以结束该回合，并清零双方标记。
	PvPChangedPetThisRound bool
	// 技能效果：异常状态 status[0..19]，能力等级 battleLv[0..5]=攻击/防御/特攻/特防/速度/命中（-6~+6）
	PlayerStatus   [20]byte
	EnemyStatus    [20]byte
	PlayerBattleLv [6]int8
	EnemyBattleLv  [6]int8
	// NextPetBattleLvBoost：传承类技能等为“下一只出战精灵”预留的能力等级增益，
	// 在新精灵上场（2407 切换或被打死后自动换宠）时一次性应用，然后清零。
	PlayerNextPetBattleLvBoost [6]int8
	EnemyNextPetBattleLvBoost  [6]int8
	// NextPetCritBuffRounds：为“下一只出战精灵”预留的必定致命一击回合数，
	// 在新精灵上场时转移到 PlayerCritBuffRounds / EnemyCritBuffRounds，然后清零。
	PlayerNextPetCritBuffRounds byte
	EnemyNextPetCritBuffRounds  byte
	// NextGuaranteedCritHits：为“后续若干次攻击技能”预留的“必定致命一击次数”。
	// 该计数跨精灵继承：例如 A 出招自杀后，B 第一击暴击消耗 1 次；B 被击败后换 C，C 第一击再暴击消耗 1 次。
	PlayerNextGuaranteedCritHits byte
	EnemyNextGuaranteedCritHits  byte
	// 额外的“回合类固定伤害”效果：由技能配置触发，按回合开始结算
	PlayerFixedDotDamage uint32
	PlayerFixedDotRounds byte
	EnemyFixedDotDamage  uint32
	EnemyFixedDotRounds  byte
	// 暴击强化/加成：
	// - PlayerCritBuffRounds/EnemyCritBuffRounds：下 N 回合内攻击技能必定致命一击（SideEffect=58）
	// - PlayerCritBonusRounds/EnemyCritBonusRounds：下 N 回合内暴击率 +1 阶（SideEffect=32），等效于 +1/16
	PlayerCritBuffRounds  byte
	EnemyCritBuffRounds   byte
	PlayerCritBonusRounds byte
	EnemyCritBonusRounds  byte
	// 盖亚挑战条件判定：战斗发生时的地图 ID；回合数（每次 2405 使用技能+1）；最后一击是否致命一击
	BattleMapID     int
	RoundCount      int
	LastHitWasCrit  bool
	// 特殊野生精灵逻辑：用于尼尔/尼奥事件
	// HasSeenNeal 表示本场战斗中我方是否曾经让尼尔(77)登场过；
	// NiaoEscapeCountdown>0 表示尼奥(416)将在若干回合后自动逃跑。
	HasSeenNeal        bool
	NiaoEscapeCountdown int
	// BossRegion 2411 挑战 BOSS 时传入的 param2（地图多 BOSS 时区分，如谱尼 514 的 1~8）
	BossRegion uint32
	// PuniTotalLives 谱尼多命形态总命数：轮回封印=2，真身=6，其余 0
	PuniTotalLives int
	// PuniCurrentLife 当前第几命（从 1 开始）；仅在 PuniTotalLives>0 时使用
	PuniCurrentLife int
	// PuniTrueFormSuppressed 真身第1/5命的“虚无+永恒”是否被时空感应(20300)消除
	PuniTrueFormSuppressed bool
	// PuniElementLastType 谱尼元素封印(2) 上次命中的技能属性：0=无 12=光 13=暗影，需光暗交替
	PuniElementLastType byte
	// 哈莫雷特(216) 顺序破防：始终循环 0=需水系(2)，1=需火系(3)，2=需草系(1)，非当前属性伤害为 0
	HaMoLeiTePhase int
	// 尤纳斯(132)：0=未受贯穿水枪，所受攻击伤害为 0；1=已受贯穿水枪，正常受伤但保留 1 血，仅里奥斯幻影可击杀
	YouNaSiPhase int
	// IsWildMonster 本场战斗是否为野生精灵（由 2408 FIGHT_NPC_MONSTER 发起）；击败野生精灵时给出战精灵加学习力
	IsWildMonster bool
	// PlayerSkillIDs 当前出战精灵 4 个技能槽的技能 ID，与 PlayerSkillPP 对应；进入战斗/切换时从 user.Pets 填充
	PlayerSkillIDs [4]int
	// PlayerSkillPP 当前出战精灵 4 个技能槽的剩余 PP；战毕写回 user.Pets[ActivePetIndex].SkillPP
	PlayerSkillPP [4]int
	// EnemySkillIDs/EnemySkillPP 敌方 4 个技能槽与剩余 PP。
	// 目前仅用于特定 PvE BOSS（如哈默雷特）做“按最大 PP 进入战斗、用完不出招”的限制；PvP 不使用。
	EnemySkillIDs [4]int
	EnemySkillPP  [4]int
	// HitsTakenFromEnemy 本场战斗中玩家受到敌方攻击并实际扣血的次数（盖亚特训-邪气凌然判定用）
	HitsTakenFromEnemy int
	// LastPlayerSkillID 玩家本回合使用的最后一个技能 ID（盖亚特训-石破天惊判定用）
	LastPlayerSkillID int
	// PlayerUsedSkills 本场战斗中玩家曾使用过的技能集合（用于雷伊技能特训等“使用指定技能获领悟”逻辑）
	PlayerUsedSkills map[int]bool
	// LastDamageFromEnemySkill 敌方上一次通过技能对我方造成的实际伤害值（不含异常/固伤），用于“克制”等反弹类技能
	LastDamageFromEnemySkill uint32
	// LastDamageFromEnemySkillRound 该伤害发生时的 RoundCount（用于限定“当前回合”的判定）
	LastDamageFromEnemySkillRound int
	// PlayerStatDropImmuneRounds 玩家当前“免疫能力下降”剩余回合数（由部分技能设置，如百变随心）
	PlayerStatDropImmuneRounds byte
	// EnemyNextAttackMustMiss 敌方下一个攻击技能必定 MISS（除 MustHit=1 外），由部分“回避类”技能设置，命中后立即清零。
	EnemyNextAttackMustMiss bool
	// 多回合效果（回合结束递减）：effect 41 火系伤害减半 / 42 电系伤害×2 / 44 特攻伤害减半 / 46 抵挡N次攻击
	// 48 免疫异常 / 50 物攻伤害减半 / 54 对方伤害 1/m / 57 每用技能回血 1/m / 68 致死留1血
	// 78 物攻必 miss / 81 必中 / 89 吸血 1/m / 90 伤害×m
	PlayerFireResistRounds    byte
	PlayerElectricPowerRounds byte
	PlayerSpecDefRounds       byte
	PlayerShieldRounds        byte
	PlayerStatusImmuneRounds  byte
	PlayerPhysDefRounds       byte
	// effect 45：n 回合内自身防御力与对手相同（仅影响物理伤害公式中的防御值）
	PlayerDefenceEqualRounds byte
	// effect 51：n 回合内自身攻击力与对手相同（仅影响物理伤害公式中的攻击值）
	PlayerAttackEqualRounds byte
	EnemyDamageHalvedRounds   byte
	EnemyDamageHalvedDenom    byte
	PlayerHealPerSkillRounds  byte
	PlayerHealPerSkillDenom   byte
	PlayerEndureRounds        byte
	EnemyEndureRounds         byte
	PlayerPhysImmuneRounds    byte
	PlayerMustHitRounds       byte
	PlayerVampRounds          byte
	PlayerVampDenom           byte
	PlayerDmgMulRounds        byte
	PlayerDmgMul              byte
	// 性别相关与优先级强化：
	// - PlayerFirstPriorityRounds/EnemyFirstPriorityRounds：下 N 回合必定先手（effect 83 雄性分支）
	// - PlayerMaleDmgMulRounds/EnemyMaleDmgMulRounds：对雄性精灵造成的伤害倍率（effect 98）
	PlayerFirstPriorityRounds byte
	EnemyFirstPriorityRounds  byte
	// effect 116/117：先手附加吸血 / 先手附加害怕（仅当本回合“自己先出手”且命中时触发）
	PlayerFirstVampRounds  byte
	EnemyFirstVampRounds   byte
	PlayerFirstFearRounds  byte
	EnemyFirstFearRounds   byte
	PlayerFirstFearChance  byte // effect 117：先手害怕的触发几率（百分比），来自 SideEffectArg[1]
	EnemyFirstFearChance   byte
	PlayerMaleDmgMulRounds    byte
	PlayerMaleDmgMul          byte
	EnemyMaleDmgMulRounds     byte
	EnemyMaleDmgMul           byte
	// 敌方对应效果（PvP 同步用）：opponentBattle.Enemy* = battle.Player*
	EnemyFireResistRounds    byte
	EnemyElectricPowerRounds byte
	EnemySpecDefRounds       byte
	EnemyShieldRounds        byte
	EnemyStatusImmuneRounds  byte
	EnemyPhysDefRounds       byte
	EnemyDefenceEqualRounds  byte
	EnemyAttackEqualRounds   byte
	PlayerDamageHalvedRounds byte
	PlayerDamageHalvedDenom  byte
	EnemyHealPerSkillRounds  byte
	EnemyHealPerSkillDenom   byte
	EnemyPhysImmuneRounds    byte
	// effect 106：n 回合内，特殊攻击对自身必定 miss
	PlayerSpecImmuneRounds byte
	EnemySpecImmuneRounds  byte
	EnemyMustHitRounds       byte
	EnemyVampRounds          byte
	EnemyVampDenom           byte
	EnemyDmgMulRounds        byte
	EnemyDmgMul              byte
	// effect 49：抵挡 n 点伤害（吸收盾，累计扣减）
	PlayerDamageAbsorb uint32
	// effect 77：n 回合每用技能恢复 m 点体力（固定值，与 57 的按比例区分）
	PlayerHealPerSkillPointsRounds byte
	PlayerHealPerSkillPoints       uint32
	// effect 128：n 回合内“受到的伤害转化为自身体力”（伤害转治疗），不与 77 复用状态字段
	PlayerDamageConvertRounds byte
	// effect 21：m~n 回合每回合反弹对手 1/k 的伤害
	PlayerReflectRounds byte
	PlayerReflectDenom  byte
	// effect 84/92/108：n 回合内受到物理攻击时，有 m% 几率令对手进入异常状态
	PlayerOnHitParalysisRounds byte
	PlayerOnHitParalysisChance byte
	PlayerOnHitFreezeRounds    byte
	PlayerOnHitFreezeChance    byte
	PlayerOnHitBurnRounds      byte
	PlayerOnHitBurnChance      byte

	EnemyOnHitParalysisRounds byte
	EnemyOnHitParalysisChance byte
	EnemyOnHitFreezeRounds    byte
	EnemyOnHitFreezeChance    byte
	EnemyOnHitBurnRounds      byte
	EnemyOnHitBurnChance      byte
	// effect 109：n 回合内，给对手造成伤害时，有 m% 几率令对手冻伤
	PlayerOnDamageFreezeRounds byte
	PlayerOnDamageFreezeChance byte
	EnemyOnDamageFreezeRounds  byte
	EnemyOnDamageFreezeChance  byte
	// effect 110：n 回合内，每次躲避攻击都有 m% 几率使自身某项能力 +1 级
	PlayerOnDodgeBuffRounds byte
	PlayerOnDodgeBuffChance byte
	PlayerOnDodgeBuffStat   byte
	EnemyOnDodgeBuffRounds  byte
	EnemyOnDodgeBuffChance  byte
	EnemyOnDodgeBuffStat    byte
	// effect 67：本次直接击败对方出战精灵时，削减对方下一只出战精灵最大体力 1/n
	PlayerNextPetMaxHPDropDenom byte
	EnemyNextPetMaxHPDropDenom  byte
	// effect 55/56：多回合“属性交换/复制”带来的临时属性覆盖（用于 STAB 与克制判定）
	PlayerTypeOverride1      byte
	PlayerTypeOverride2      byte
	PlayerTypeOverrideRounds byte
	EnemyTypeOverride1       byte
	EnemyTypeOverride2       byte
	EnemyTypeOverrideRounds  byte
	// effect 91：n 回合内，对手的状态变化（异常与能力等级变化）会同时作用在自己身上
	PlayerSyncOpponentChangesRounds byte
	EnemySyncOpponentChangesRounds  byte
	// effect 73：若先手攻击，则在受攻击时把所受伤害 2 倍反击给对手（持续 n 回合）
	PlayerCounterDoubleRounds byte
	EnemyCounterDoubleRounds  byte
	// effect 86：n 回合内，属性/变化技能（Category=4）对自身必定 miss
	PlayerAttrMoveImmuneRounds byte
	EnemyAttrMoveImmuneRounds  byte
	// effect 65：n 回合内某属性技能威力×m
	PlayerTypePowerRounds byte
	PlayerTypePowerType   byte
	PlayerTypePowerMul    byte
	// effect 123：受伤时提升自身能力；保存持续回合数、目标能力下标及增减等级
	PlayerOnDamagedBuffRounds byte
	PlayerOnDamagedBuffStat   byte
	PlayerOnDamagedBuffDelta  int8
	EnemyOnDamagedBuffRounds  byte
	EnemyOnDamagedBuffStat    byte
	EnemyOnDamagedBuffDelta   int8
	// IsGrandMelee 本场是否为精灵大乱斗（随机分配精灵、胜方获得可分配经验，战毕不写回背包）
	IsGrandMelee bool
	// IsPetKing 本场是否为精灵王之战（太空站 2413 PET_KING_JOIN 等入口）
	// 用于在战斗结束或逃跑时统计胜场数（MonKingWin），不影响其他 PvP 模式。
	IsPetKing bool
	// 勇者之塔（地图 500）多精灵层：当前击败到第几只（0-based），BraveTowerBossIDs 为本层全部 Boss ID
	BraveTowerBossIndex int
	BraveTowerBossIDs   []int
}

// DarkPortalDoorInfo 暗黑武斗场门状态：记录玩家当前打开的是哪一门以及子门索引
type DarkPortalDoorInfo struct {
	DoorIndex uint32 // 门索引（0-10），对应第1～第11门
	SubIndex  uint32 // 子门/槽位索引（0 开始），由 curDoor % 6 计算，用于区分 3-1/3-2/…、11-1/11-2/11-3 等
}

// GameServer 游戏服务器
type GameServer struct {
	Port            int
	Clients         []*ClientData
	Sessions        map[string]*ClientData
	Users           map[int64]*userdb.GameData
	ServerList      []map[string]interface{}
	NextSeqID       int32
	UserDB          *userdb.UserDB
	mu              sync.RWMutex
	commandHandlers map[int32]CommandHandler
	BattleStates    map[int64]*BattleState // 用户ID -> 战斗状态
	BattleMu        sync.RWMutex           // 战斗状态锁
	// 地图玩家：mapID -> userID -> *ClientData，用于 2003 列表与广播
	MapUsers   map[int]map[int64]*ClientData
	MapUsersMu sync.RWMutex
	// 客户端断开时回调（由 handlers 注册，用于广播 2003 给同地图其他玩家）；mapID 为该用户断线前所在地图
	OnClientDisconnect func(*ClientData, int)

	// 暗黑武斗场：记录每个玩家当前打开的暗黑之门，用于 2425 FIGHT_DARKPORTAL 选择正确 BOSS
	DarkPortalDoors   map[int64]DarkPortalDoorInfo
	DarkPortalDoorsMu sync.RWMutex

	// 精灵王之战(PET_KING_JOIN) 匹配队列：按模式区分（5=单精灵，6=多精灵），每种模式同时只保留一名等待者。
	PetKingQueue   map[uint32]int64
	PetKingQueueMu sync.Mutex

	// 精灵大乱斗(CMD 2431)：匹配队列与对战时临时精灵列表（仅本场使用，不写回背包）
	GrandMeleeQueue   int64       // 等待匹配的 UID，0 表示无人等待
	GrandMeleeQueueMu sync.Mutex
	GrandMeleePets    map[int64][]userdb.Pet // uid -> 本场随机分配到的 3 只精灵（满血副本）

	// 野生精灵定时刷新（与 Lua 一致）：每玩家每地图槽位 + 进图/战毕时间
	OgreMu             sync.RWMutex
	OgreRefreshState   map[int64]*OgreRefreshState   // userId -> 进图/战毕/上次刷新时间
	OgreSlots          map[int64]map[int][]mapogres.Slot // userId -> mapID -> 当前槽位（长度 4 或 9）
	stopOgreTicker     chan struct{}
}

// OgreRefreshState 单玩家的精灵刷新时间状态
type OgreRefreshState struct {
	EnterMapTime     time.Time  // 进入当前地图时间
	LastFightEndTime  *time.Time // 战斗结束时间，用于战毕 3 秒内不刷新
	LastRefreshTime   time.Time  // 上次推送 2004 的刷新时间
}

// CommandHandler 命令处理器接口
type CommandHandler func(ctx *HandlerContext)

// HandlerContext 处理器上下文
type HandlerContext struct {
	UserID     int64
	CmdID      int32
	SeqID      int32
	Body       []byte
	ClientData *ClientData
	GameServer *GameServer
}

// New 创建游戏服务器实例
func New(config userdb.Config) *GameServer {
	gs := &GameServer{
		Port:              5000,
		Clients:           make([]*ClientData, 0),
		Sessions:          make(map[string]*ClientData),
		Users:             make(map[int64]*userdb.GameData),
		ServerList:        make([]map[string]interface{}, 0),
		NextSeqID:         1,
		UserDB:            userdb.New(config),
		commandHandlers:   make(map[int32]CommandHandler),
		BattleStates:      make(map[int64]*BattleState),
		MapUsers:          make(map[int]map[int64]*ClientData),
		DarkPortalDoors:  make(map[int64]DarkPortalDoorInfo),
		OgreRefreshState: make(map[int64]*OgreRefreshState),
		OgreSlots:        make(map[int64]map[int][]mapogres.Slot),
		stopOgreTicker:   make(chan struct{}),
		PetKingQueue:     make(map[uint32]int64),
		GrandMeleePets:   make(map[int64][]userdb.Pet),
	}

	gs.initServerList()
	return gs
}

// initServerList 初始化服务器列表
func (gs *GameServer) initServerList() {
	// 模拟官服的29个服务器
	for i := 1; i <= 29; i++ {
		gs.ServerList = append(gs.ServerList, map[string]interface{}{
			"id":      i,
			"userCnt": 30, // 模拟用户数
			"ip":      "127.0.0.1",
			"port":    5000 + i,
			"friends": 0,
		})
	}
	logger.Info(fmt.Sprintf("初始化 %d 个服务器", len(gs.ServerList)))
}

// Start 启动服务器
func (gs *GameServer) Start() error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", gs.Port))
	if err != nil {
		return err
	}

	logger.Info(fmt.Sprintf("游戏服务器启动在端口 %d", gs.Port))

	go gs.runOgreRefreshLoop()

	go func() {
		defer ln.Close()

		for {
			conn, err := ln.Accept()
			if err != nil {
				logger.Error(fmt.Sprintf("接受连接失败: %v", err))
				continue
			}

			addr := conn.RemoteAddr()
			logger.Info(fmt.Sprintf("新连接: %s", addr))

			clientData := &ClientData{
				Socket:   conn,
				Buffer:   make([]byte, 0),
				UserID:   0,
				Session:  "",
				SeqID:    0,
				LoggedIn: false,
			}

			gs.mu.Lock()
			gs.Clients = append(gs.Clients, clientData)
			gs.mu.Unlock()

			go gs.handleClient(clientData)
		}
	}()

	return nil
}

// handleClient 处理客户端连接
func (gs *GameServer) handleClient(clientData *ClientData) {
	defer func() {
		gs.removeClient(clientData)
		clientData.Socket.Close()
	}()

	buffer := make([]byte, 4096)

	for {
		n, err := clientData.Socket.Read(buffer)
		if err != nil {
			logger.Info("客户端断开连接")
			return
		}

		clientData.Buffer = append(clientData.Buffer, buffer[:n]...)
		gs.processBuffer(clientData)
	}
}

// processBuffer 处理缓冲区数据
func (gs *GameServer) processBuffer(clientData *ClientData) {
	// Flash 客户端可能先发策略请求（不带 17 字节头），必须优先处理，否则会把 '<pol' 当 length 导致卡死
	for gs.tryConsumeFlashPolicyRequest(clientData) {
		// 连续处理多次（极少见，但防御）
	}

	for len(clientData.Buffer) >= 17 {
		// 读取长度
		length := packet.ReadUInt32BE(clientData.Buffer, 0)

		if len(clientData.Buffer) < int(length) {
			break // 等待更多数据
		}

		// 提取完整数据包
		packetData := clientData.Buffer[:length]
		clientData.Buffer = clientData.Buffer[length:]

		// 解析数据包
		gs.processPacket(clientData, packetData)
	}
}

// tryConsumeFlashPolicyRequest 检测并响应 Flash policy-file-request。
// 返回 true 表示消费了一次请求（buffer 发生变化），调用方可继续循环处理。
func (gs *GameServer) tryConsumeFlashPolicyRequest(clientData *ClientData) bool {
	if len(clientData.Buffer) < len(flashPolicyRequest) {
		return false
	}

	// 可能带 \x00 终止符
	if len(clientData.Buffer) >= len(flashPolicyRequest) && string(clientData.Buffer[:len(flashPolicyRequest)]) == flashPolicyRequest {
		consume := len(flashPolicyRequest)
		if len(clientData.Buffer) > consume && clientData.Buffer[consume] == 0x00 {
			consume++
		}
		clientData.Buffer = clientData.Buffer[consume:]
		gs.sendFlashPolicyFile(clientData)
		return true
	}
	return false
}

func (gs *GameServer) sendFlashPolicyFile(clientData *ClientData) {
	policy := `<?xml version="1.0"?>
<!DOCTYPE cross-domain-policy SYSTEM "/xml/dtds/cross-domain-policy.dtd">
<cross-domain-policy>
	<allow-access-from domain="*" to-ports="*" />
</cross-domain-policy>` + "\x00"

	if clientData.Socket == nil {
		return
	}
	if _, err := clientData.Socket.Write([]byte(policy)); err != nil {
		logger.Error(fmt.Sprintf("发送Flash安全策略文件失败: %v", err))
		return
	}
	logger.Info("已发送Flash安全策略文件（gameserver）")
}

// processPacket 处理数据包
func (gs *GameServer) processPacket(clientData *ClientData, packetData []byte) {
	if len(packetData) < 17 {
		return
	}

	// 读取包头
	length := packet.ReadUInt32BE(packetData, 0)
	// version := packetData[4] // 暂未使用
	cmdId := int32(packet.ReadUInt32BE(packetData, 5))
	userId := packet.ReadUInt32BE(packetData, 9)
	seqId := int32(packet.ReadUInt32BE(packetData, 13))

	body := make([]byte, 0)
	if len(packetData) > 17 {
		body = packetData[17:]
	}

	clientData.UserID = int64(userId)
	clientData.SeqID = seqId

	// 打印收到包的摘要信息（不再输出十六进制包体，以免刷屏）
	logger.Info(fmt.Sprintf("收到 CMD=%d UID=%d SEQ=%d LEN=%d BodyLen=%d", cmdId, userId, seqId, length, len(body)))

	// 处理命令
	gs.handleCommand(clientData, cmdId, int64(userId), seqId, body)
}

// handleCommand 处理命令
func (gs *GameServer) handleCommand(clientData *ClientData, cmdId int32, userId int64, seqId int32, body []byte) {
	// 检查是否有注册的处理器
	handler, exists := gs.commandHandlers[cmdId]
	if exists {
		ctx := &HandlerContext{
			UserID:     userId,
			CmdID:      cmdId,
			SeqID:      seqId,
			Body:       body,
			ClientData: clientData,
			GameServer: gs,
		}
		handler(ctx)
		return
	}

	// 处理默认命令
	switch cmdId {
	case 80008:
		gs.handleHeartbeat(clientData, cmdId, userId, seqId, body)
	default:
		logger.Warning(fmt.Sprintf("未实现的命令: %d (UID=%d SEQ=%d BodyLen=%d)", cmdId, userId, seqId, len(body)))
		// 对于未实现的命令，尽量回“可解析的最小包体”，避免前端因 data==null 导致 #1009。
		// 典型场景：GET_TASK_BUF(2203) 若回空包，客户端 TasksManager 会把 data as TaskBufInfo 得到 null 并崩溃。
		if cmdId == 2203 {
			var taskID uint32
			if len(body) >= 4 {
				taskID = binary.BigEndian.Uint32(body[0:4])
			}
			min := make([]byte, 4+4+20) // taskId + flag + buf(20)
			binary.BigEndian.PutUint32(min[0:4], taskID)
			binary.BigEndian.PutUint32(min[4:8], 0)
			gs.SendResponse(clientData, cmdId, userId, seqId, min)
			return
		}
		// 其他未实现命令：返回空包但 result=0
		gs.SendResponse(clientData, cmdId, userId, seqId, []byte{})
	}
}

// handleHeartbeat 处理心跳包
func (gs *GameServer) handleHeartbeat(clientData *ClientData, cmdId int32, userId int64, seqId int32, body []byte) {
	// 客户端回复的心跳包，不需要再回复
	logger.Debug("收到心跳包响应")
}

// SendResponse 发送响应
func (gs *GameServer) SendResponse(clientData *ClientData, cmdId int32, userId int64, seqId int32, body []byte) {
	// AS3 客户端把第5字段当 result/errorCode，正常必须为 0
	response := packet.BuildResponse(cmdId, uint32(userId), 0, body)

	_, err := clientData.Socket.Write(response)
	if err != nil {
		logger.Error(fmt.Sprintf("发送响应失败: %v", err))
		return
	}

	// 打印发送包的摘要信息（只保留长度等，不输出详细包体）
	logger.Info(fmt.Sprintf("发送 CMD=%d UID=%d SEQ=%d LEN=%d BodyLen=%d", cmdId, userId, seqId, len(response), len(body)))
}

// removeClient 移除客户端
func (gs *GameServer) removeClient(clientData *ClientData) {
	// 清理心跳定时器
	if clientData.HeartbeatTimer != nil {
		clientData.HeartbeatTimer.Stop()
	}

	// 设置用户离线状态并持久化到磁盘，避免下线后物品/套装丢失
	if clientData.UserID > 0 && gs.UserDB != nil {
		gs.UserDB.SetUserOffline(clientData.UserID)
		gs.UserDB.SaveToFile()
		logger.Info(fmt.Sprintf("用户 %d 已离线", clientData.UserID))
	}

	// 从地图玩家中移除，并触发广播 2003 给同地图其他玩家（在解锁后由 OnClientDisconnect 执行）
	mapID := 0
	if clientData.UserID > 0 {
		u := gs.GetOrCreateUser(clientData.UserID)
		if u != nil {
			mapID = u.MapID
		}
		// 兜底清理：断线时清除战斗状态，避免遗留 IsActive=true 导致该账号后续永远无法刷新野怪/进入正常流程
		gs.BattleMu.Lock()
		delete(gs.BattleStates, clientData.UserID)
		gs.BattleMu.Unlock()
		gs.RemoveUserFromMap(mapID, clientData.UserID)
		// 清除暗黑武斗场门状态（用户断开连接时）
		gs.DarkPortalDoorsMu.Lock()
		delete(gs.DarkPortalDoors, clientData.UserID)
		gs.DarkPortalDoorsMu.Unlock()
		// 将该用户从精灵王之战匹配队列中移除
		gs.PetKingQueueMu.Lock()
		for mode, uid := range gs.PetKingQueue {
			if uid == clientData.UserID {
				delete(gs.PetKingQueue, mode)
			}
		}
		gs.PetKingQueueMu.Unlock()
	}

	// 从客户端列表移除
	gs.mu.Lock()
	for i, c := range gs.Clients {
		if c == clientData {
			gs.Clients = append(gs.Clients[:i], gs.Clients[i+1:]...)
			break
		}
	}
	// 从会话映射移除
	if clientData.Session != "" {
		delete(gs.Sessions, clientData.Session)
	}
	gs.mu.Unlock()

	if gs.OnClientDisconnect != nil && mapID != 0 {
		gs.OnClientDisconnect(clientData, mapID)
	}
}

// AddUserToMap 将用户加入地图（若曾在其他地图则需先 RemoveUserFromMap 旧地图）
func (gs *GameServer) AddUserToMap(mapID int, userID int64, client *ClientData) {
	if mapID <= 0 {
		return
	}
	gs.MapUsersMu.Lock()
	defer gs.MapUsersMu.Unlock()
	if gs.MapUsers[mapID] == nil {
		gs.MapUsers[mapID] = make(map[int64]*ClientData)
	}
	gs.MapUsers[mapID][userID] = client
}

// RemoveUserFromMap 将用户从地图移除
func (gs *GameServer) RemoveUserFromMap(mapID int, userID int64) {
	if mapID <= 0 {
		return
	}
	gs.MapUsersMu.Lock()
	defer gs.MapUsersMu.Unlock()
	if m, ok := gs.MapUsers[mapID]; ok {
		delete(m, userID)
		if len(m) == 0 {
			delete(gs.MapUsers, mapID)
		}
	}
}

// KickOtherSessionsOfUser 踢掉该用户在其他连接上的会话，保证同一账户只能一个在线
// excludeClient 为当前正在登录的连接，不会被踢；只关闭其他同 UID 的连接
func (gs *GameServer) KickOtherSessionsOfUser(excludeClient *ClientData, userID int64) {
	if userID <= 0 {
		return
	}
	gs.mu.Lock()
	var toClose net.Conn
	for _, c := range gs.Clients {
		if c != excludeClient && c.UserID == userID {
			toClose = c.Socket
			break
		}
	}
	gs.mu.Unlock()
	if toClose != nil {
		toClose.Close()
		logger.Info(fmt.Sprintf("同一账户重复登录: 已踢掉 UID=%d 的旧连接", userID))
	}
}

// GetClientByUserID 根据用户ID获取其连接（用于向指定用户推送如对战邀请等）
func (gs *GameServer) GetClientByUserID(userID int64) *ClientData {
	gs.mu.RLock()
	defer gs.mu.RUnlock()
	for _, c := range gs.Clients {
		if c.UserID == userID {
			return c
		}
	}
	return nil
}

// GetClientsOnMap 返回当前在该地图上的所有客户端（调用方只读，勿长时间持锁）
func (gs *GameServer) GetClientsOnMap(mapID int) []*ClientData {
	gs.MapUsersMu.RLock()
	m := gs.MapUsers[mapID]
	if m == nil || len(m) == 0 {
		gs.MapUsersMu.RUnlock()
		return nil
	}
	list := make([]*ClientData, 0, len(m))
	for _, c := range m {
		list = append(list, c)
	}
	gs.MapUsersMu.RUnlock()
	return list
}

// BroadcastToMap 向同地图除 excludeUserID 外的所有客户端发送一条协议
func (gs *GameServer) BroadcastToMap(mapID int, excludeUserID int64, cmdID int32, body []byte) {
	clients := gs.GetClientsOnMap(mapID)
	for _, c := range clients {
		if c.UserID == excludeUserID {
			continue
		}
		gs.SendResponse(c, cmdID, c.UserID, 0, body)
	}
}

// StartHeartbeat 启动心跳定时器
func (gs *GameServer) StartHeartbeat(clientData *ClientData, userId int64) {
	// 清理现有定时器
	if clientData.HeartbeatTimer != nil {
		clientData.HeartbeatTimer.Stop()
	}

	// 每6秒发送一次心跳包
	clientData.HeartbeatTimer = time.AfterFunc(6*time.Second, func() {
		gs.sendHeartbeat(clientData, userId)
		gs.StartHeartbeat(clientData, userId)
	})
}

// sendHeartbeat 发送心跳包
func (gs *GameServer) sendHeartbeat(clientData *ClientData, userId int64) {
	if clientData.Socket != nil && clientData.LoggedIn {
		response := packet.BuildResponse(80008, uint32(userId), 0, []byte{})
		_, err := clientData.Socket.Write(response)
		if err != nil {
			logger.Error(fmt.Sprintf("发送心跳包失败: %v", err))
		}
	}
}

// GetOrCreateUser 获取或创建用户
func (gs *GameServer) GetOrCreateUser(userId int64) *userdb.GameData {
	gs.mu.RLock()
	user, exists := gs.Users[userId]
	gs.mu.RUnlock()

	if exists {
		return user
	}

	// 从数据库获取
	gameData := gs.UserDB.GetOrCreateGameData(userId)

	gs.mu.Lock()
	gs.Users[userId] = gameData
	gs.mu.Unlock()

	return gameData
}

// RemoveUserCache 从内存缓存中移除用户（GM 删号后调用）
func (gs *GameServer) RemoveUserCache(userID int64) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	delete(gs.Users, userID)
}

// UpdateUserCache 更新内存中的用户数据（GM 修改后调用，保证对战 2503 等用到最新数据）
func (gs *GameServer) UpdateUserCache(userID int64, gameData *userdb.GameData) {
	if gameData == nil {
		return
	}
	gs.mu.Lock()
	gs.Users[userID] = gameData
	gs.mu.Unlock()
}

// 野生精灵定时刷新常量
const (
	// 进图后多久允许首次定时刷新；与 Lua 版保持一致为 5 秒，
	// 这样进入地图时先收到一包空 2004，5 秒后定时器再推送真实精灵列表。
	OgreEnterMapDelay = 5 * time.Second
	// 战斗结束后多久开始刷新（战毕 ≥3 秒可刷新）
	OgreFightEndDelay = 3 * time.Second
	// 两次刷新间隔
	OgreRefreshInterval = 10 * time.Second
	// 定时器检查间隔
	OgreTickInterval = 1 * time.Second
)

// SetOgreEnterMapTime 记录玩家进入当前地图时间，供定时刷新判断
func (gs *GameServer) SetOgreEnterMapTime(userID int64) {
	gs.OgreMu.Lock()
	defer gs.OgreMu.Unlock()
	if gs.OgreRefreshState[userID] == nil {
		gs.OgreRefreshState[userID] = &OgreRefreshState{}
	}
	gs.OgreRefreshState[userID].EnterMapTime = time.Now()
}

// SetOgreLastRefreshTime 记录本次推送 2004 的时间，下次定时刷新将在此后 10 秒
func (gs *GameServer) SetOgreLastRefreshTime(userID int64) {
	gs.OgreMu.Lock()
	defer gs.OgreMu.Unlock()
	if gs.OgreRefreshState[userID] == nil {
		gs.OgreRefreshState[userID] = &OgreRefreshState{}
	}
	gs.OgreRefreshState[userID].LastRefreshTime = time.Now()
}

// SetOgreFightEndTime 记录战斗结束时间，战毕 3 秒内不刷新
func (gs *GameServer) SetOgreFightEndTime(userID int64) {
	gs.OgreMu.Lock()
	defer gs.OgreMu.Unlock()
	now := time.Now()
	if gs.OgreRefreshState[userID] == nil {
		gs.OgreRefreshState[userID] = &OgreRefreshState{}
	}
	gs.OgreRefreshState[userID].LastFightEndTime = &now
}

// GetPlayerOgreSlots 获取该玩家在该地图的当前精灵槽位（只读），无则返回 nil
func (gs *GameServer) GetPlayerOgreSlots(userID int64, mapID int) []mapogres.Slot {
	gs.OgreMu.RLock()
	defer gs.OgreMu.RUnlock()
	if gs.OgreSlots[userID] == nil {
		return nil
	}
	return gs.OgreSlots[userID][mapID]
}

// SetPlayerOgreSlots 设置该玩家在该地图的精灵槽位
func (gs *GameServer) SetPlayerOgreSlots(userID int64, mapID int, slots []mapogres.Slot) {
	gs.OgreMu.Lock()
	defer gs.OgreMu.Unlock()
	if gs.OgreSlots[userID] == nil {
		gs.OgreSlots[userID] = make(map[int][]mapogres.Slot)
	}
	gs.OgreSlots[userID][mapID] = slots
}

// ClearPlayerOgreSlots 清空该玩家在该地图的槽位（战斗结束后调用，下次定时刷新会重新生成 3 只）
func (gs *GameServer) ClearPlayerOgreSlots(userID int64, mapID int) {
	gs.OgreMu.Lock()
	defer gs.OgreMu.Unlock()
	if gs.OgreSlots[userID] != nil {
		delete(gs.OgreSlots[userID], mapID)
	}
}

// runOgreRefreshLoop 每秒检查一次，对满足条件的玩家每 10 秒整批刷新该地图野生精灵并推送 2004
func (gs *GameServer) runOgreRefreshLoop() {
	ticker := time.NewTicker(OgreTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-gs.stopOgreTicker:
			return
		case <-ticker.C:
			gs.TickOgreRefresh()
		}
	}
}

// TickOgreRefresh 遍历在线玩家，满足条件则刷新该玩家当前地图精灵并推送 2004
func (gs *GameServer) TickOgreRefresh() {
	gs.mu.RLock()
	clients := make([]*ClientData, 0, len(gs.Clients))
	for _, c := range gs.Clients {
		if c.UserID > 0 && c.LoggedIn {
			clients = append(clients, c)
		}
	}
	gs.mu.RUnlock()

	now := time.Now()
	for _, client := range clients {
		userID := client.UserID
		user := gs.GetOrCreateUser(userID)
		mapID := user.MapID
		if mapID <= 0 {
			continue
		}

		gs.OgreMu.Lock()
		state := gs.OgreRefreshState[userID]
		if state == nil {
			state = &OgreRefreshState{EnterMapTime: now}
			gs.OgreRefreshState[userID] = state
		}
		// 战斗中不刷新
		gs.BattleMu.RLock()
		inFight := gs.BattleStates[userID] != nil && gs.BattleStates[userID].IsActive
		gs.BattleMu.RUnlock()
		if inFight {
			gs.OgreMu.Unlock()
			continue
		}
		// 战毕 3 秒内不刷新
		if state.LastFightEndTime != nil {
			if now.Sub(*state.LastFightEndTime) < OgreFightEndDelay {
				gs.OgreMu.Unlock()
				continue
			}
			state.LastFightEndTime = nil
		}
		// 进图 5 秒内不刷新
		if now.Sub(state.EnterMapTime) < OgreEnterMapDelay {
			gs.OgreMu.Unlock()
			continue
		}
		// 距上次刷新不足 10 秒不刷新
		if !state.LastRefreshTime.IsZero() && now.Sub(state.LastRefreshTime) < OgreRefreshInterval {
			gs.OgreMu.Unlock()
			continue
		}
		state.LastRefreshTime = now

		if gs.OgreSlots[userID] == nil {
			gs.OgreSlots[userID] = make(map[int][]mapogres.Slot)
		}
		gs.OgreMu.Unlock()

		// 每次到点刷新：整批重刷一轮精灵。
		// 为了让前端先看到“这一轮全部消失，再出现下一轮”，这里先推送一包空 2004（9 格全 0），
		// 随后再生成新槽位并推送真实 2004。
		emptyBody := gs.BuildMapOgreListFromSlots(nil)
		gs.SendResponse(client, 2004, userID, 0, emptyBody)

		newSlots := mapogres.GenerateNewSlotsNoCache(mapID)
		if len(newSlots) > 0 {
			gs.SetPlayerOgreSlots(userID, mapID, newSlots)
			body := gs.BuildMapOgreListFromSlots(newSlots)
			gs.SendResponse(client, 2004, userID, 0, body)
			logger.Info(fmt.Sprintf("[精灵刷新] UID=%d MapID=%d 整批刷新 %d 只", userID, mapID, len(newSlots)))
		}
	}
}

// BuildMapOgreListFromSlots 根据槽位列表构建 2004 包体（9 格，前 4 格用 slots，不足填 0）；slots 为 nil 时全 0
// 格式：9 槽位 * [petId(4)+shiny(4)] = 72 字节，客户端用 shiny 在地图上显示异色
func (gs *GameServer) BuildMapOgreListFromSlots(slots []mapogres.Slot) []byte {
	body := make([]byte, 72)
	for i := 0; i < 9; i++ {
		var petID, shiny uint32
		if i < len(slots) && slots[i].PetID > 0 {
			petID = uint32(slots[i].PetID)
			if slots[i].Shiny {
				shiny = 1
			}
		}
		binary.BigEndian.PutUint32(body[i*8:], petID)
		binary.BigEndian.PutUint32(body[i*8+4:], shiny)
	}
	return body
}

// RegisterCommandHandler 注册命令处理器
func (gs *GameServer) RegisterCommandHandler(cmdId int32, handler CommandHandler) {
	gs.commandHandlers[cmdId] = handler
}
