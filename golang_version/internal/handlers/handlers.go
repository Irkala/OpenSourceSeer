package handlers

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/seer-game/golang-version/internal/core/logger"
	"github.com/seer-game/golang-version/internal/core/packet"
	"github.com/seer-game/golang-version/internal/core/userdb"
	gamebattle "github.com/seer-game/golang-version/internal/game/battle"
	gameevolve "github.com/seer-game/golang-version/internal/game/evolve"
	gameogres "github.com/seer-game/golang-version/internal/game/mapogres"
	gamepets "github.com/seer-game/golang-version/internal/game/pets"
	gameskills "github.com/seer-game/golang-version/internal/game/skills"
	"github.com/seer-game/golang-version/internal/game/sptboss"
	"github.com/seer-game/golang-version/internal/server/gameserver"
)

// ==================== 米币商品配置（MoneyProductXMLInfo.xml） ====================

type moneyProductEntry struct {
	ItemIDs []int
	Gold    int // 赠送金豆数（对齐 xml @gold）
}

type moneyProductXMLRoot struct {
	Items []struct {
		ProductID uint32 `xml:"productID,attr"`
		ItemID    string `xml:"itemID,attr"` // 可能为 "1001|1002|..."
		Gold      int    `xml:"gold,attr"`
	} `xml:"item"`
}

var (
	moneyProductOnce sync.Once
	moneyProductMap  map[uint32]moneyProductEntry
)

func loadMoneyProductXMLOnce() {
	moneyProductOnce.Do(func() {
		moneyProductMap = make(map[uint32]moneyProductEntry)

		readAndParse := func(p string) bool {
			data, err := os.ReadFile(p)
			if err != nil {
				return false
			}
			var root moneyProductXMLRoot
			if err := xml.Unmarshal(data, &root); err != nil {
				logger.Warning(fmt.Sprintf("[1102] 解析 MoneyProductXMLInfo.xml 失败: %v", err))
				return false
			}
			for _, it := range root.Items {
				if it.ProductID == 0 || strings.TrimSpace(it.ItemID) == "" {
					continue
				}
				parts := strings.Split(it.ItemID, "|")
				ids := make([]int, 0, len(parts))
				for _, s := range parts {
					s = strings.TrimSpace(s)
					if s == "" {
						continue
					}
					v, _ := strconv.Atoi(s)
					if v > 0 {
						ids = append(ids, v)
					}
				}
				if len(ids) == 0 {
					continue
				}
				moneyProductMap[it.ProductID] = moneyProductEntry{ItemIDs: ids, Gold: it.Gold}
			}
			logger.Info(fmt.Sprintf("[1102] 已加载米币商品配置: count=%d path=%s", len(moneyProductMap), p))
			return len(moneyProductMap) > 0
		}

		// 候选路径（运行目录可能为 golang_version 或 output/bin）
		var candidates []string
		if exePath, err := os.Executable(); err == nil && exePath != "" {
			exeDir := filepath.Dir(exePath)
			candidates = append(candidates,
				filepath.Join(exeDir, "..", "xml", "MoneyProductXMLInfo.xml"),
				filepath.Join(exeDir, "..", "..", "golang_version", "xml", "MoneyProductXMLInfo.xml"),
				filepath.Join(exeDir, "golang_version", "xml", "MoneyProductXMLInfo.xml"),
			)
		}
		candidates = append(candidates,
			filepath.Join("golang_version", "xml", "MoneyProductXMLInfo.xml"),
			filepath.Join("xml", "MoneyProductXMLInfo.xml"),
			filepath.Join("..", "golang_version", "xml", "MoneyProductXMLInfo.xml"),
		)
		for _, c := range candidates {
			if readAndParse(c) {
				return
			}
		}
		logger.Warning("[1102] 未能加载 MoneyProductXMLInfo.xml，米币购买将不发放物品")
	})
}

func getMoneyProduct(productID uint32) (moneyProductEntry, bool) {
	loadMoneyProductXMLOnce()
	e, ok := moneyProductMap[productID]
	return e, ok
}

// 本地定义特性 ID 范围（需与 userdb 中保持一致）
const (
	traitMinIdxLocal = 1006
	traitMaxIdxLocal = 1045
)

// BOSS 防护罩血量缓存：userID -> "mapID_region" -> 当前血量（0 表示满血/未初始化）
var (
	bossHpCache   = make(map[int64]map[string]int)
	bossHpCacheMu sync.RWMutex
)

// 巅峰之战匹配模式：1=单精灵，2=多精灵/巅峰对决
const (
	topLevelModeSingle = 1
	topLevelModeMulti  = 2
)

// 巅峰之战匹配队列：使用简单的“一个等待位”实现双人配对
var (
	waitingTopLevelSingle int64
	waitingTopLevelMulti  int64
	topLevelMu            sync.Mutex
)

// bossSkillOverrides 为特定 BOSS 设置固定技能列表（优先级高于按等级可学技能）
// 顺序即 UI 显示顺序，可按需要扩展
var bossSkillOverrides = map[int][]int{
	// 闪光波克尔（克洛斯星 Boss 及其变体 ID）：红韵、全力一击、魅惑、挥翼飘舞
	// 注：按照需求，将原来的“同生共死”(10036) 替换为 “全力一击”(10033)
	40:  {20210, 10033, 20209, 10486},
	63:  {20210, 10033, 20209, 10486},
	86:  {20210, 10033, 20209, 10486},
	166: {20210, 10033, 20209, 10486},
}

// bossHPOverrides 地图 BOSS 固定血量（仅用于 PVE 敌人，不影响玩家自己拥有的同名精灵）
// key 为精灵 ID，value 为战斗开始时的 MaxHP/HP。
var bossHPOverrides = map[int]int{
	47:  100,     // 蘑菇怪
	34:  200,     // 钢牙鲨
	42:  338,     // 里奥斯
	50:  1000,    // 阿克希亚
	69:  500,     // 提亚斯
	70:  2000,    // 雷伊
	5005: 1200,   // 雷伊的幻影（技能特训最终战 / 倒影雷伊）
	88:  1400,    // 纳多雷
	113: 1500,    // 雷纳多
	132: 2800,    // 尤纳斯
	187: 3000000, // 魔狮迪露
	216: 10000,   // 哈莫雷特
	264: 2500,    // 奈尼芬多
	421: 3000,    // 厄尔塞拉
	166: 2000,    // 闪光波克尔（克洛斯星密林）
	261: 2000,    // 盖亚
	274: 13000,   // 塔克林
	1337: 10000,  // 机械塔克林
	391: 10000,   // 塔西亚
	798: 2000,    // 卡修斯（怀特星）
	715: 10000,   // 德拉萨（怀特矿场）
	// SPT 五大神兽：血量按 ServerMapsXMLInfo.xml 中 SPTBoss 配置
	490: 2500,    // 劳克蒙德（沃尔夫洞穴 423 SPT，三种规则领域 Hp=2500）
	538: 1500,    // 克拉尼特（戴斯星 435 SPT 困难 Hp=1500）
	587: 1200,    // 墨杜萨（墨杜萨星 SPT Hp=1200）
	617: 1200,    // 肯佩德（拉铂尔星 441 SPT Hp=1200）
	672:  sptboss.YaLunSiBossHP, // 亚伦斯（冰超能系）
	5012: sptboss.YaLunSiBossHP, // 亚伦斯 SPT 形态（拖梯星第二场景 430）
	589:  8000,   // 克瑞斯（赫鲁卡城二当家定制 BOSS）
	// 暗黑武斗场各门 BOSS：仅设定 HP，不改其他特性
	169: 450,  // 试炼之门：卡特斯 Lv45 HP450
	171: 600,  // 第1门：魔牙鲨 Lv50 HP600
	174: 800,  // 第2门：贝鲁基德 Lv60 HP800
	177: 1000, // 第3门：巴弗洛 Lv70 HP1000
	195: 1000, // 第4门：西萨拉斯 Lv80 HP1000
	222: 900,  // 第5门：卡库 Lv90 HP900
	356: 1600, // 第6门：斯加尔卡 Lv100 HP1600
	438: 1800, // 第7门：魔花使者 Lv100 HP1800
	656: 2000, // 第8门：帕多尼 Lv100 HP2000
	779: 1500, // 第9门：迪普利德 Lv100 HP1500
	1182: 2000, // 第10门：鳞甲魔鱼 Lv100 HP2000
	1403: 2000, // 第11门：萨洛奇斯 11-1 HP2000

	// 暗黑武斗场子门 BOSS：按照你的表只配等级血量，不附加任何特性
	183: 1100, // 奇拉塔顿  第3门 3-2  HP1100
	192: 1200, // 克林卡修  第4门 4-2  HP1200
	224: 1300, // 赫德卡    第5门 5-2  HP1300
	227: 1400, // 伊兰罗尼  第5门 5-3  HP1400
	297: 1500, // 艾尔伊洛  第6门 6-2  HP1500
	359: 2000, // 布林克克  第6门 6-3  HP2000
	441: 2000, // 莫尔加斯  第7门 7-2  HP2000
	435: 2000, // 萨诺拉斯  第7门 7-3  HP2000
	659: 2100, // 加洛德    第8门 8-2  HP2100
	661: 2200, // 萨多拉尼  第8门 8-3  HP2200
	784: 1200, // 拜洛亚斯  第9门 9-2  HP1200
	782: 2500,  // 阿克诺亚  第9门 9-3  HP2500
	1185: 2000, // 艾斯德克  第10门 10-1 HP2000
	1397: 2000, // 查迪斯    第11门 11-2 HP2000
	1400: 3000, // 奈尼迪亚  第11门 11-3 HP3000（每回合额外恢复500）
}

// 石破天惊技能 ID（盖亚特训第三阶段判定用）
const skillIDShiPoTianJing = 10715

// applyBossHPOverride 若为地图 BOSS，则返回指定的固定血量，否则返回原始值
// battle 可为 nil；谱尼封印/真身(514+region1~8) 时用 PuniSealHP(region)，否则用 bossHPOverrides
// 玄武巴斯特(501)：仅在地图401 param2=1（真身）时使用50000血，param2=0（守护兽）用种族值
func applyBossHPOverride(petID int, original int, battle *gameserver.BattleState) int {
	if battle != nil && sptboss.IsPuniSealBoss(battle.BattleMapID, battle.BossRegion) && petID == sptboss.PetIDPuni {
		if hp := sptboss.PuniSealHP(battle.BossRegion); hp > 0 {
			return hp
		}
	}
	if battle != nil && battle.BattleMapID == sptboss.MapIDXuanWuSpace {
		if sptboss.IsXuanWuGuardianBoss(battle.BattleMapID, battle.BossRegion) {
			return sptboss.XuanWuGuardianHP // 六守护兽均为 5000 血
		}
		if petID == 501 && battle.BossRegion == 6 {
			return 50000 // 玄武真身
		}
	}
	if battle != nil && battle.BattleMapID == sptboss.MapIDQingLongSpace {
		if sptboss.IsQingLongGuardianBoss(battle.BattleMapID, battle.BossRegion) {
			return sptboss.QingLongGuardianHP // 四守护兽均为 5000 血
		}
		if petID == sptboss.PetIDDuoLaGe && battle.BossRegion == 4 {
			return sptboss.QingLongBossHP // 朵拉格真身 50000 血
		}
	}
	if battle != nil && battle.BattleMapID == sptboss.MapIDYiNengWang && petID == 1000 {
		if sptboss.IsYiNengSealBoss(battle.BattleMapID, battle.BossRegion) {
			if hp := sptboss.YiNengSealHP(battle.BossRegion); hp > 0 {
				return hp
			}
		}
		if sptboss.IsYiNengUltimateBoss(battle.BattleMapID, battle.BossRegion) {
			return sptboss.YiNengUltimateLifeMaxHP(1) // 终极试炼第一命 2000 血
		}
	}
	if hp, ok := bossHPOverrides[petID]; ok && hp > 0 {
		return hp
	}
	return original
}

// completeTaskIfNeeded 若指定任务未标记完成，则将其状态置为 "3" 并记录完成时间
// 这里不负责下发 2202 协议，仅用于在服务端内部“补齐”某些历史活动/空间的前置任务状态
func completeTaskIfNeeded(user *userdb.GameData, taskID int) {
	if user == nil {
		return
	}
	if user.Tasks == nil {
		user.Tasks = make(map[string]userdb.Task)
	}
	key := strconv.Itoa(taskID)
	t := user.Tasks[key]
	if t.Status == "3" {
		// 已完成则不重复写入
		return
	}
	t.Status = "3"
	t.CompleteTime = time.Now().Unix()
	user.Tasks[key] = t
}

// autoCompleteXuanWuTasksIfNeeded 针对玄武空间所需的老任务线，在玩家首次击败对应 SPT BOSS 时自动补齐任务完成状态
// 对应前端 XuanWuController._spt = [301,302,303,305,307,308]
func autoCompleteXuanWuTasksIfNeeded(user *userdb.GameData, bossPetID int) {
	switch bossPetID {
	case 47: // 蘑菇怪
		completeTaskIfNeeded(user, 301)
	case 34: // 钢牙鲨
		completeTaskIfNeeded(user, 302)
	case 42: // 里奥斯
		completeTaskIfNeeded(user, 303)
	case 69: // 提亚斯
		completeTaskIfNeeded(user, 305)
	case 88: // 纳多雷
		completeTaskIfNeeded(user, 307)
	case 113: // 雷纳多
		completeTaskIfNeeded(user, 308)
	}
}

// autoCompleteQingLongTasksIfNeeded 针对青龙空间所需的老任务线，在玩家首次击败对应 SPT BOSS 时自动补齐任务完成状态
// 对应前端 QingLongController._spt = [312,314,315,316]，分别对应 奈尼芬多、塔西亚、塔克林、厄尔塞拉
func autoCompleteQingLongTasksIfNeeded(user *userdb.GameData, bossPetID int) {
	switch bossPetID {
	case 264: // 奈尼芬多
		completeTaskIfNeeded(user, 312)
	case 391: // 塔西亚
		completeTaskIfNeeded(user, 314)
	case 274: // 塔克林
		completeTaskIfNeeded(user, 315)
	case 421: // 厄尔塞拉
		completeTaskIfNeeded(user, 316)
	}
}

// hasDefeatedQingLongSPTBosses 是否已击败进入青龙空间所需的四个 SPT BOSS（奈尼芬多、塔西亚、塔克林、厄尔塞拉）
// 依据 DefeatedSPTBossIds 列表判断，避免依赖前端任务状态。
func hasDefeatedQingLongSPTBosses(user *userdb.GameData) bool {
	if user == nil {
		return false
	}
	if len(sptboss.QingLongGuardianPetIDs) == 0 {
		// 理论上不会发生，兜底为放行
		return true
	}
	defeated := make(map[int]bool, len(user.DefeatedSPTBossIds))
	for _, id := range user.DefeatedSPTBossIds {
		defeated[id] = true
	}
	for _, need := range sptboss.QingLongGuardianPetIDs {
		if !defeated[need] {
			return false
		}
	}
	return true
}

// hasClearedAllPuniSeals 是否已击败谱尼七大封印（region 1~7）
// 使用 DefeatedSPTBossIds 中的特殊标记 30000+region 判断，每个封印/真身首次击败时会写入一条记录。
func hasClearedAllPuniSeals(user *userdb.GameData) bool {
	if user == nil {
		return false
	}
	if len(user.DefeatedSPTBossIds) == 0 {
		return false
	}
	var mask uint8
	for _, id := range user.DefeatedSPTBossIds {
		if id >= 30001 && id <= 30007 {
			region := id - 30000
			if region >= 1 && region <= 7 {
				mask |= 1 << uint(region-1)
			}
		}
	}
	// 0b0111_1111 表示 1~7 全部击败
	return mask == 0x7F
}

// resetDailyTasksIfNewDay 在玩家登录时检查是否跨天，
// 若已跨天，则清空「赛尔精灵训练营」相关的每日任务进度，
// 让 401/402/403 等每日任务在第二天可以重新接取。
func resetDailyTasksIfNewDay(user *userdb.GameData) {
	if user == nil {
		return
	}

	now := time.Now().Unix()
	// 兼容旧存档：若此前未记录过 LastOnline，只更新一次时间戳，不做重置
	if user.LastOnline == 0 {
		user.LastOnline = now
		return
	}

	lastDay := user.LastOnline / 86400
	curDay := now / 86400
	if curDay != lastDay {
		if user.Tasks != nil && len(user.Tasks) > 0 {
			// 目前训练营涉及的每日任务 ID（训练营面板中的 8 个）：
			// 401 每日任务之毛毛
			// 402 每日任务之小火猴武学梦想
			// 403 小医生布布
			// 404 每日任务之伊优环保任务
			// 405 每日任务之比比鼠的发电能源
			// 406 每日任务之爱捉迷藏的幽浮
			// 407 利牙鱼的口腔护理
			// 462 每日任务之领取扭蛋牌
			dailyIDs := []int{401, 402, 403, 404, 405, 406, 407, 462}
			for _, id := range dailyIDs {
				delete(user.Tasks, strconv.Itoa(id))
			}
		}
	}

	// 无论是否跨天，更新最近在线时间，避免同一天内重复重置
	user.LastOnline = now
}

// getEnemySkillsForPet 返回敌方精灵的技能列表，若有覆盖则优先覆盖
func getEnemySkillsForPet(petID, level int) []int {
	if skills, ok := bossSkillOverrides[petID]; ok {
		return skills
	}
	petMgr := gamepets.GetInstance()
	return petMgr.GetSkillsForLevel(petID, level)
}

// ensurePetSkillsFilledUpToFour 在精灵升级后，若当前携带技能数量不足 4 个，则按照「已学会技能的前几个」自动补满到 4 个。
// 规则：
// - 保留现有技能顺序与选择，不做替换；
// - 按精灵 LearnableMoves 中 Level 从小到大的顺序，遍历所有 <= 当前等级的技能；
// - 跳过已经携带的技能，将剩余技能依次追加到技能槽中，直到 4 个或没有可补为止；
// - 最终写回 p.Skills（长度固定为 4，允许尾部 0）。
func ensurePetSkillsFilledUpToFour(p *userdb.Pet, petMgr *gamepets.Pets) {
	if p == nil || petMgr == nil {
		return
	}
	if p.Level <= 0 || p.ID <= 0 {
		return
	}

	// 收集当前已携带技能（去重，最多 4 个）
	current := make([]int, 0, 4)
	seen := make(map[int]bool)
	for _, sid := range p.Skills {
		if sid <= 0 {
			continue
		}
		if seen[sid] {
			continue
		}
		seen[sid] = true
		current = append(current, sid)
		if len(current) >= 4 {
			break
		}
	}
	if len(current) >= 4 {
		return
	}

	def := petMgr.Get(p.ID)
	if def == nil {
		return
	}

	// 按学习等级从低到高，把已学会但尚未携带的技能补到 4 个
	for _, mv := range def.LearnableMoves {
		if mv.Level > p.Level || mv.ID <= 0 {
			continue
		}
		if seen[mv.ID] {
			continue
		}
		seen[mv.ID] = true
		current = append(current, mv.ID)
		if len(current) >= 4 {
			break
		}
	}

	// 写回到 p.Skills，固定 4 槽，尾部不足填 0
	newSkills := make([]int, 4)
	for i := 0; i < len(current) && i < 4; i++ {
		newSkills[i] = current[i]
	}
	p.Skills = newSkills
}

// pickEnemySkill 挑选敌方本回合要用的技能。
// 所有敌方（含 SPT、野生）：从当前可用技能中完全随机选择，攻击技与属性技（Category=4）等概率参与，
// 避免 SPT 只会放攻击技能从不放属性技能的问题。
func pickEnemySkill(skillMgr *gameskills.Skills, petID, level int) (*gameskills.Skill, uint32) {
	skillsForPet := getEnemySkillsForPet(petID, level)

	var allSkills []*gameskills.Skill
	var allIDs []uint32
	for _, sid := range skillsForPet {
		if sid <= 0 {
			continue
		}
		if sk := skillMgr.Get(sid); sk != nil {
			allSkills = append(allSkills, sk)
			allIDs = append(allIDs, uint32(sid))
		}
	}
	if len(allSkills) == 0 {
		return nil, 0
	}
	idx := rand.Intn(len(allSkills))
	return allSkills[idx], allIDs[idx]
}

// fillBattleEnemySkills 为敌方初始化技能槽与 PP（按 MaxPP 满 PP）。
// 目前仅供需要“敌方 PP 耗尽不出招”的 PvE BOSS 使用（如哈默雷特）。
func fillBattleEnemySkills(battle *gameserver.BattleState) {
	if battle == nil {
		return
	}
	petMgr := gamepets.GetInstance()
	skillMgr := gameskills.GetInstance()
	rawSkills := petMgr.GetSkillsForLevel(battle.EnemyID, battle.EnemyLevel)
	for i := 0; i < 4; i++ {
		sid := 0
		if i < len(rawSkills) {
			sid = rawSkills[i]
		}
		battle.EnemySkillIDs[i] = sid
		maxPP := 20
		if sid > 0 {
			if sk := skillMgr.Get(sid); sk != nil && sk.MaxPP > 0 {
				maxPP = sk.MaxPP
			}
		}
		battle.EnemySkillPP[i] = maxPP
	}
}

// pickEnemySkillWithPP 为特定 PvE BOSS 提供“按 PP 选招并消耗”的敌方选招逻辑。
// 当可用技能 PP 全为 0 时返回 (nil,0) 表示本回合不出招。
func pickEnemySkillWithPP(battle *gameserver.BattleState, skillMgr *gameskills.Skills) (*gameskills.Skill, uint32) {
	if battle == nil || skillMgr == nil {
		return nil, 0
	}
	// 敌方技能尚未初始化则先初始化
	if battle.EnemySkillIDs[0] == 0 && battle.EnemySkillIDs[1] == 0 && battle.EnemySkillIDs[2] == 0 && battle.EnemySkillIDs[3] == 0 {
		fillBattleEnemySkills(battle)
	}

	var candidates []*gameskills.Skill
	var candidateIDs []uint32
	var candidateIdx []int
	for i := 0; i < 4; i++ {
		sid := battle.EnemySkillIDs[i]
		if sid <= 0 || battle.EnemySkillPP[i] <= 0 {
			continue
		}
		if sk := skillMgr.Get(sid); sk != nil {
			candidates = append(candidates, sk)
			candidateIDs = append(candidateIDs, uint32(sid))
			candidateIdx = append(candidateIdx, i)
		}
	}
	if len(candidates) == 0 {
		return nil, 0
	}
	idx := rand.Intn(len(candidates))
	return candidates[idx], candidateIDs[idx]
}

// RegisterHandlers 注册所有命令处理器
func RegisterHandlers(gs *gameserver.GameServer) {
	// 注册核心命令处理器
	registerCoreHandlers(gs)

	// 注册游戏逻辑命令处理器
	registerGameHandlers(gs)

	// 注册精灵相关命令处理器
	registerPetHandlers(gs)

	// 注册战斗相关命令处理器
	registerBattleHandlers(gs)

	// 注册未实现命令的空响应（对齐 Lua 服 CMD 列表，避免客户端触发“未实现的命令”）
	registerStubHandlers(gs)

	// 客户端断线时向同地图其他玩家广播更新后的 2003 列表
	gs.OnClientDisconnect = func(_ *gameserver.ClientData, mapID int) {
		body := buildMapPlayerListForMap(gs, mapID)
		gs.BroadcastToMap(mapID, 0, 2003, body)
		logger.Info(fmt.Sprintf("[2003] 用户离开地图后广播: MapID=%d", mapID))
	}
}

// registerCoreHandlers 注册核心命令处理器
func registerCoreHandlers(gs *gameserver.GameServer) {
	// 兼容旧异能王面板使用的 FUCK_SHINEHOO_TIMES：部分独立 SWF 中该常量值为 0
	gs.RegisterCommandHandler(0, handleFuckShinehooTimes)
	gs.RegisterCommandHandler(1001, handleLogin)
	gs.RegisterCommandHandler(1002, handleSystemTime)
	// 登录后初始化阶段常见请求（对齐 Lua 服，避免客户端 Bean/模块卡死）
	gs.RegisterCommandHandler(2150, handleGetRelationList)  // 好友/黑名单
	gs.RegisterCommandHandler(2151, handleFriendAdd)        // 添加好友
	gs.RegisterCommandHandler(2152, handleFriendAnswer)     // 好友请求回复
	gs.RegisterCommandHandler(2153, handleFriendRemove)     // 删除好友
	gs.RegisterCommandHandler(2154, handleBlackAdd)         // 添加黑名单
	gs.RegisterCommandHandler(2155, handleBlackRemove)      // 移除黑名单
	gs.RegisterCommandHandler(2157, handleSeeOnline)        // 查看在线状态
	gs.RegisterCommandHandler(2158, handleRequestOut)       // 发送请求
	gs.RegisterCommandHandler(2159, handleRequestAnswer)    // 请求回复
	gs.RegisterCommandHandler(2751, handleMailGetList)      // 邮件列表
	gs.RegisterCommandHandler(2757, handleMailGetUnread)    // 未读邮件
	gs.RegisterCommandHandler(50004, handleXinCheck)        // 客户端上报
	gs.RegisterCommandHandler(50006, handleXinFusion)       // 精灵融合转生仓（XIN_FUSION）
	gs.RegisterCommandHandler(50008, handleXinGetQuadTime)  // 四倍经验时间
	gs.RegisterCommandHandler(70001, handleGetExchangeInfo) // 荣誉/交换
	gs.RegisterCommandHandler(1106, handleGoldOnlineCheckRemain)
	gs.RegisterCommandHandler(1104, handleGoldBuyProduct)   // 金豆购买商品
	gs.RegisterCommandHandler(1101, handleMoneyCheckPsw)    // 米币支付密码检查（返回1则客户端继续购买流程）
	gs.RegisterCommandHandler(1103, handleMoneyCheckRemain) // 米币余额检查
	gs.RegisterCommandHandler(80008, handleHeartbeat)
	gs.RegisterCommandHandler(2701, handleTalkCount)       // 对话计数（物品领取）
	gs.RegisterCommandHandler(2702, handleTalkCate)        // 对话分类（发放领取物品）
	gs.RegisterCommandHandler(2707, handleGetDailyValue)   // GET_DAILY_VALUE / FUCK_SHINEHOO_TIMES 每日计数查询
	gs.RegisterCommandHandler(2708, handleGetYiNengTaskStatus) // 地图677 六重试炼任务(758~763)完成状态同步，供客户端显示挑战真身按钮
	gs.RegisterCommandHandler(10006, handleFitmentUsering) // 正在使用的家具（基地）
	// 系统/支付 完整协议
	gs.RegisterCommandHandler(1005, handleGetImageAddress)
	gs.RegisterCommandHandler(1102, handleMoneyBuyProduct)
	gs.RegisterCommandHandler(1105, handleGoldCheckRemain)
	// 协议层/校验 完整协议
	gs.RegisterCommandHandler(1022, handleCheckFightCode)
	gs.RegisterCommandHandler(8002, handleSystemMessage)
	gs.RegisterCommandHandler(9049, handleOpenBagGet)
	gs.RegisterCommandHandler(11003, handleGetPetInfoAlt)
	gs.RegisterCommandHandler(11007, handleGetPetByCatchTime)
	gs.RegisterCommandHandler(11022, handleGetSecondBag)
	gs.RegisterCommandHandler(41983, handleReconnect)
	gs.RegisterCommandHandler(46046, handleGetMultiForever)
	gs.RegisterCommandHandler(40001, handleGetSuperValue)
	gs.RegisterCommandHandler(40002, handleGetSuperValueByIds)
	gs.RegisterCommandHandler(42023, handleBatchGetBitset)
	gs.RegisterCommandHandler(46057, handleGetMultiForeverByDb)
	gs.RegisterCommandHandler(41080, handleGetForeverValue)
	gs.RegisterCommandHandler(47334, handleFriendListAlt)
	gs.RegisterCommandHandler(47335, handleBlacklistAlt)
	gs.RegisterCommandHandler(10301, handleSystemTimeAlt)
	gs.RegisterCommandHandler(4475, handleItemListAlt)
	gs.RegisterCommandHandler(8001, handleInform)
	gs.RegisterCommandHandler(8004, handleGetBossMonster)
	// BufferRecord：缓冲标记读写（客户端 BufferRecordManager / bufferrecord.xml）
	gs.RegisterCommandHandler(1013, handleBufferRecord)
	// 成就/称号 协议（3403 ACHIEVETITLELIST 已实现，3404 SETTITLE 单独实现）
	gs.RegisterCommandHandler(3403, handleAchieveTitleList)
	gs.RegisterCommandHandler(3404, handleSetTitle)

	// 注册NONO系统命令处理器
	registerNonoHandlers(gs)
}

// registerGameHandlers 注册游戏逻辑命令处理器
func registerGameHandlers(gs *gameserver.GameServer) {
	// 注册地图相关命令处理器
	gs.RegisterCommandHandler(2001, handleEnterMap)
	gs.RegisterCommandHandler(2002, handleLeaveMap)
	gs.RegisterCommandHandler(2003, handleListMapPlayer)
	gs.RegisterCommandHandler(1004, handleMapHot) // 地图热点（宇宙地图热点数据）
	gs.RegisterCommandHandler(2004, handleMapOgreList)
	gs.RegisterCommandHandler(2401, handleInviteToFight)     // 邀请玩家对战（转发 2501 给被邀请方）
	gs.RegisterCommandHandler(2402, handleInviteFightCancel) // 取消 PvP / 精灵王之战匹配
	gs.RegisterCommandHandler(2403, handleHandleFightInvite) // 接受/拒绝对战邀请（转发 2502 给邀请方）
	// 巅峰之战 12：单精灵/多精灵 PVP 匹配入口（地图 433 顶部面板）
	gs.RegisterCommandHandler(2458, handleTopLevelJoin)   // PET_TOPLEVEL_JOIN
	gs.RegisterCommandHandler(2567, handleTopFightBeyond) // TOPFIGHT_BEYOND
	gs.RegisterCommandHandler(2408, handleFightNpcMonster)   // 地图野怪战斗
	gs.RegisterCommandHandler(2412, handleAttackBoss)        // 攻击 SPT BOSS（破除防护罩）
	gs.RegisterCommandHandler(2051, handleGetSimUserInfo)    // 获取简单用户信息
	gs.RegisterCommandHandler(2052, handleGetMoreUserInfo)   // 获取详细用户信息
	gs.RegisterCommandHandler(2101, handlePeopleWalk)        // 人物移动
	gs.RegisterCommandHandler(2102, handleChat)              // 聊天
	gs.RegisterCommandHandler(2104, handleAimat)             // 射击/瞄准（AIMAT）
	gs.RegisterCommandHandler(2107, handleTransformUser)     // 射击命中后变身（TRANSFORM_USER），广播 2108 给同图
	// 地图/玩家 完整协议
	gs.RegisterCommandHandler(2061, handleChangeNickName)
	gs.RegisterCommandHandler(2063, handleChangeColor)
	gs.RegisterCommandHandler(2103, handleDanceAction)
	gs.RegisterCommandHandler(2111, handlePeopleTransform)
	gs.RegisterCommandHandler(2112, handleOnOrOffFlying)

	// 精灵相关（部分，只实现新手流程必需）
	gs.RegisterCommandHandler(2301, handleGetPetInfo)
	// 2302 = MODIFY_PET_NAME（修改精灵名字），由 stub 处理；2308 = PET_DEFAULT（设为首发）
	gs.RegisterCommandHandler(2303, handleGetPetList) // 获取精灵列表（切换精灵时需要）
	gs.RegisterCommandHandler(2307, handlePetStudySkill) // 学习/替换技能（升级/学习器确认后）

	// 任务 / 新手奖励相关（2201/2202/2203）、每日任务（2231/2232/2233）、魂珠列表/进度/赋形（2354/2352/2353/2356/2357）
	gs.RegisterCommandHandler(2201, handleAcceptTask)
	gs.RegisterCommandHandler(2202, handleCompleteTask)
	gs.RegisterCommandHandler(2203, handleGetTaskBuf)        // 获取任务进度（GET_TASK_BUF），地图装置等依赖此接口
	gs.RegisterCommandHandler(2231, handleAcceptDailyTask)   // 接受每日任务
	gs.RegisterCommandHandler(2232, handleDeleteDailyTask)   // 放弃每日任务
	gs.RegisterCommandHandler(2233, handleCompleteDailyTask) // 完成每日任务（响应格式同 2202 NoviceFinishInfo）
	gs.RegisterCommandHandler(2354, handleGetSoulBeadList)
	gs.RegisterCommandHandler(2352, handleGetSoulBeadBuf)
	gs.RegisterCommandHandler(2353, handleSetSoulBeadBuf)
	gs.RegisterCommandHandler(2356, handleGetSoulBeadStatus)
	gs.RegisterCommandHandler(2357, handleTransformSoulBead)
	gs.RegisterCommandHandler(2204, handleAddTaskBuf)
	gs.RegisterCommandHandler(2234, handleGetDailyTaskBuf)
	gs.RegisterCommandHandler(2235, handleAddDailyTaskBuf) // ADD_DAILY_TASK_BUF，与 2204 类似，持久化每日任务 buf
	gs.RegisterCommandHandler(2065, handleExchangeNewYear)
	gs.RegisterCommandHandler(2251, handleExchangeOre)
	gs.RegisterCommandHandler(2902, handleExchangePetComplete)

	// 背包/物品系统
	registerItemHandlers(gs)

	// 新手战斗触发：2411 -> 回推 2503；2421 盖亚专用（FIGHT_SPECIAL_PET）同逻辑
	gs.RegisterCommandHandler(2411, handleChallengeBoss)
	gs.RegisterCommandHandler(2421, handleChallengeBoss)

	// 精灵王之战（太空站）：2413 PET_KING_JOIN 参与匹配（单/多精灵）
	gs.RegisterCommandHandler(2413, handlePetKingJoin)
	// 精灵大乱斗：2431 参与匹配（至少3只满状态精灵，随机分配后 3v3 PvP，胜方 10000 可分配经验）
	gs.RegisterCommandHandler(2431, handleGrandMeleeJoin)

	// 战斗初始化：2404 READY_TO_FIGHT
	gs.RegisterCommandHandler(2404, handleReadyToFight)

	// 空间站 102 擂台 Arena 协议（2417-2423），避免地图卡死
	gs.RegisterCommandHandler(2417, handleArenaSetOwenr)   // ARENA_SET_OWENR 成为擂主
	gs.RegisterCommandHandler(2418, handleArenaFightOwenr) // ARENA_FIGHT_OWENR 挑战擂主
	gs.RegisterCommandHandler(2419, handleArenaGetInfo)    // ARENA_GET_INFO 获取擂台信息（进图必调，需正确格式）
	gs.RegisterCommandHandler(2420, handleArenaUpfight)    // ARENA_UPFIGHT 放弃擂台
	gs.RegisterCommandHandler(2422, handleArenaOwenrAcce)  // ARENA_OWENR_ACCE 接受擂主（战斗胜利后）
	gs.RegisterCommandHandler(2423, handleArenaOwenrOut)   // ARENA_OWENR_OUT 擂主离开（服务端推送，客户端也可能请求）

	// 暗黑武斗场：2424 开门返回该门 BOSS 精灵 ID；2425 战斗初始化；2426 离开暗黑武斗场
	gs.RegisterCommandHandler(2424, handleOpenDarkPortal)
	gs.RegisterCommandHandler(2425, handleFightDarkPortal)
	gs.RegisterCommandHandler(2426, handleLeaveDarkPortal)

	// 青龙空间(403)：2461 进入校验（点击传送阵时调用），返回 1 可进入、0 不可进入；2462 查询守护兽击败进度
	gs.RegisterCommandHandler(2461, handleQingLongCheckEntry)
	gs.RegisterCommandHandler(2462, handleGetQingLongGuardianMask)

	// 盖亚魂印特训：查询当前效果列表（M_2149）
	gs.RegisterCommandHandler(2149, handleGaiyaEffectQuery)

	// 基地/家具购买
	gs.RegisterCommandHandler(10004, handleBuyFitment) // BUY_FITMENT 购买基地家具/房型
	gs.RegisterCommandHandler(10007, handleFitmentAll) // FITMENT_ALL 基地家具仓库列表
	gs.RegisterCommandHandler(10008, handleSetFitment) // SET_FITMENT 保存基地布置/设置房型

	// 雷伊特训：查询6项训练次数
	gs.RegisterCommandHandler(2393, handleLeiyiTrainGetStatus) // LEIYI_TRAIN_GET_STATUS
}

// registerPetHandlers 注册精灵相关命令处理器
func registerPetHandlers(gs *gameserver.GameServer) {
	gs.RegisterCommandHandler(2304, handlePetRelease)                   // 精灵仓库互转
	gs.RegisterCommandHandler(2305, handlePetShow)                      // 展示精灵（跟随面板）
	gs.RegisterCommandHandler(2306, handlePetCureAll)                   // 精灵恢复（全部），Nono 精灵治疗
	gs.RegisterCommandHandler(2308, handleSetDefaultPet)                // 设为首发精灵（客户端 PET_DEFAULT）
	gs.RegisterCommandHandler(2314, handlePetEvolveInBabin)             // 精灵进化（进化仓/普通进化共用）
	gs.RegisterCommandHandler(2310, handlePetOneCure)                   // 精灵恢复（单只），背包点击精灵恢复
	gs.RegisterCommandHandler(2319, handlePetGetExp)                    // 获取经验池经验
	gs.RegisterCommandHandler(2318, handlePetSetExp)                    // 从经验池分配经验给精灵
	gs.RegisterCommandHandler(2315, handlePetHatch)                     // 分子转化仪：放入精元开始孵化
	gs.RegisterCommandHandler(2316, handlePetHatchGet)                  // 分子转化仪：查询/领取孵化精灵
	gs.RegisterCommandHandler(2323, handlePetRoomShow)                  // 房间精灵展示/收回（保存展示列表）
	gs.RegisterCommandHandler(2324, handlePetRoomList)                  // 房间精灵列表（当前展示中的精灵）
	gs.RegisterCommandHandler(2325, handlePetRoomInfo)                  // 精灵房间信息（简略面板）
	gs.RegisterCommandHandler(2320, handlePetEvolveInBabin)             // 精灵进化舱进化
	gs.RegisterCommandHandler(3007, handleExperienceSharedComplete)     // 发明室经验接收器：领取并平均分配给背包精灵
	gs.RegisterCommandHandler(3009, handleMyExperiencePondComplete)     // 发明室经验接收器：查询教官积累经验值
	gs.RegisterCommandHandler(3011, handleGetMyExperienceComplete)      // 发明室经验接收器：教官查看未领取经验
	gs.RegisterCommandHandler(2312, handlePetSkillSwitch)               // 精灵技能切换（技能唤醒仪替换技能）
	gs.RegisterCommandHandler(2336, handleGetPetSkill)                  // 获取精灵技能（技能唤醒仪）
	gs.RegisterCommandHandler(2326, handleUsePetItemOutOfFight)         // 战斗外使用精灵道具（学习力清零/等）
	gs.RegisterCommandHandler(9278, handleUsePetItemFullAbilityOfStudy) // 学习力注入（单项拉满）
	// 融合 / 元神珠相关
	gs.RegisterCommandHandler(2351, handlePetFusion) // PET_FUSION 融合：生成元神珠记录
	gs.RegisterCommandHandler(2358, handleSoulBeadToPet)
}

// registerBattleHandlers 注册战斗相关命令处理器
func registerBattleHandlers(gs *gameserver.GameServer) {
	gs.RegisterCommandHandler(2405, handleUseSkill)     // 使用技能
	gs.RegisterCommandHandler(2406, handleUsePetItem)   // 使用道具
	gs.RegisterCommandHandler(2407, handleChangePet)    // 切换精灵
	gs.RegisterCommandHandler(2409, handleCatchMonster) // 捕捉精灵
	gs.RegisterCommandHandler(2410, handleEscapeFight)  // 逃跑
}

// registerStubHandlers 注册尚未实现完整协议的 CMD：按 Lua 响应格式区分 4 字节 0 / 8 字节 0 / 空包
func registerStubHandlers(gs *gameserver.GameServer) {
	// 扭蛋机：3201 精灵扭蛋机、9757 梦幻扭蛋机 已单独实现
	gs.RegisterCommandHandler(3201, handleGacha)
	gs.RegisterCommandHandler(9757, handleGacha)
	// Lua 返回 writeUInt32BE(0) 的 CMD，响应 4 字节 0
	stub4Zero := []int32{
		5001, 5002, 2442, 2444, 2445, 2446,
		// 用户信息相关 (2051/2052 已实现)：2053 REQUEST_COUNT, 2054 GP_GHAZI_MAX_LEVEL, 2055 USER_PARTY_GET_USER_IMAGE_NAME
		2053, 2054, 2055,
		2302, 2309, // 2311/2313 精灵收集奖励相关需 8 字节；2307(学习技能)有专门处理器，不再使用 stub4Zero；2314 已由 handlePetEvolveInBabin 实现
		2321, 2322, 2327, 2328, 2329, 2330, 2331, 2332, // 2320 由 handlePetEvolveInBabin 实现；2323/2324 已实现为 handlePetRoomShow/handlePetRoomList
		2343,
		// 成就/称号 (ACHIEVELIST/ACHIEVEINFO/CONFERACHIEVEMENT/ACHIEVE_AND_TITLE)，3403/3404 已单独实现，3405 在 stubEmpty
		3401, 3402, 3406, 3407,
		// 2411/2421 已在 registerGameHandlers 中注册为 handleChallengeBoss（SPT/盖亚挑战），此处不再 stub
		// 2414/2415/2416 勇者之塔选择/开始/离开已在下方单独注册；2417-2423 擂台协议已单独实现 handleArena*，此处不再 stub；2424 暗黑武斗场开门已实现 handleOpenDarkPortal
		2910, 2911, 2912, 2913, 2914, 2917, 2918, 2928, 2929, 2962, 2963,
		3001, 3002, 3003, 3004, 3005, 3006, 3008, 3010,
		4001, 4002, 4003, 4004, 4005, 4006, 4007, 4008, 4009, 4010, 4011, 4012, 4013, 4014,
		4017, 4018, 4019, 4020, 4023, 4024, 4025, 4101, 4102, 2481,
		10001, 10002, 10003, /*10004 BUY_FITMENT 已实现*/ 10005, /*10007/10008 基地家具已实现*/ 10009,
		// 2151-2159 好友协议已实现完整处理，不再使用 stub4Zero
	}
	for _, cmd := range stub4Zero {
		gs.RegisterCommandHandler(cmd, handleStub4Zero)
	}
	// 勇者之塔（地图 500）：2414 选择关卡，2415 开始战斗返回下一层 Boss，2416 离开（客户端不解析包体）
	gs.RegisterCommandHandler(2414, handleChoiceFightLevel)
	gs.RegisterCommandHandler(2415, handleStartFightLevel)
	gs.RegisterCommandHandler(2416, handleStub4Zero)
	// 试炼之塔（地图 600 新手试炼之塔）：2428 选择关卡，2429 开始战斗返回下一层 Boss
	gs.RegisterCommandHandler(2428, handleFreshChoiceFightLevel)
	gs.RegisterCommandHandler(2429, handleFreshStartFightLevel)
	// Lua 返回 ret(4)+count(4) 等 8 字节的 CMD；2311 精灵收集计划领取已实现 handlePetCollect；2313 IS_COLLECT 需 8 字节
	gs.RegisterCommandHandler(5052, handleStub8Zero)
	gs.RegisterCommandHandler(2311, handlePetCollect)
	gs.RegisterCommandHandler(2313, handleStub8Zero)
	// Lua 返回空或客户端不依赖包体的 CMD（含 gamepktprotocol.lua emptyCmds 中未单独实现的 CMD，9003/2354 已实现故不在此列）
	stubEmpty := []int32{
		5003, // LEAVE_GAME
		2441, // LOAD_PERCENT 战斗资源加载进度（客户端上报，服务端仅需空响应）
		2430, // FRESH_LEAVE_FIGHT_LEVEL 试炼之塔离开，客户端不解析包体
		1011, 1016, 2289, 2192, 2196, 2361, 3405, 4359, 4364, 4501, 5005,
		9112, 9677, 41006, 41249, 41253, 4148, 4178, 4181, 43706, 45512, 45524,
		45773, 45793, 45798, 45824, 47309, 45071,
		40006, 40007,
	}
	// 注：2313 已在 stub4Zero（精灵 IS_COLLECT），9003/2354 已有完整实现，故不放入 stubEmpty
	for _, cmd := range stubEmpty {
		gs.RegisterCommandHandler(cmd, handleStubEmpty)
	}
}

// handleSystemTime CMD 1002 系统时间，对齐 Lua system_handlers.handleSystemTime
func handleSystemTime(ctx *gameserver.HandlerContext) {
	buf := make([]byte, 8)
	t := uint32(time.Now().Unix())
	binary.BigEndian.PutUint32(buf[0:4], t)
	binary.BigEndian.PutUint32(buf[4:8], 0)
	ctx.GameServer.SendResponse(ctx.ClientData, 1002, ctx.UserID, ctx.SeqID, buf)
}

// handleStubEmpty 未实现命令的空响应（返回 result=0 空包，避免客户端报“未实现的命令”）
func handleStubEmpty(ctx *gameserver.HandlerContext) {
	ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, []byte{})
}

// handleStub4Zero 返回 4 字节 0 的协议体（对齐 Lua writeUInt32BE(0)）
func handleStub4Zero(ctx *gameserver.HandlerContext) {
	body := make([]byte, 4)
	ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
}

// handleStub8Zero 返回 8 字节 0（对齐 Lua ret(4)+count(4) 等）
func handleStub8Zero(ctx *gameserver.HandlerContext) {
	body := make([]byte, 8)
	ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
}

// 试炼之塔（地图 600）共 30 层，每层对应一个 Boss 精灵 ID（与客户端 FightLevelModel 展示一致，使用常见低阶精灵）
var freshTowerBossIDs = [30]uint32{
	10, 11, 12, 13, 14, 15, 16, 17, 18, 19, // 1-10 层 皮皮、毛毛、火炎贝等
	20, 21, 22, 23, 24, 25, 26, 27, 28, 29, // 11-20 层
	30, 31, 32, 33, 34, 35, 36, 37, 38, 39, // 21-30 层
}

const freshTowerMaxLevel = 30

// 勇者之塔（地图 500）共 80 层，按 4399 官方攻略每层 Boss 配置（多精灵层为依次对战）
// 1-50 单精灵；51-80 多精灵组合
var braveTowerBossIDs = [80][]uint32{
	{15}, {18}, {40}, {32}, {26}, {52}, {37}, {12}, {45}, {24},
	{18}, {32}, {40}, {15}, {12}, {61}, {58}, {32}, {26}, {37}, {24},
	{45}, {55}, {29}, {21}, {61}, {3}, {6}, {9}, {58},
	{40}, {26}, {45}, {52}, {15}, {32}, {37}, {18}, {64}, {67}, {73}, {107}, {24}, {101}, {99}, {110}, {104}, {90}, {85}, {76},
	{29, 18}, {32, 76}, {40, 37}, {45, 21}, {26, 24}, {118, 107, 104}, {127, 79, 121}, {93, 92, 94}, {90, 85, 110}, {138, 140, 142},
	{270, 271, 272, 280}, {243, 344, 360}, {290, 159, 248}, {229, 236}, {242, 231}, {12, 79, 166}, {202, 213, 224, 363}, {253, 256, 266, 350}, {239, 277, 353, 280}, {366, 368, 367, 369},
	{1244, 1252, 1268}, {1302, 1284, 1242}, {1270, 1276, 1236}, {1134, 1189, 1219}, {1113, 1206, 1142}, {1089, 1097, 1197, 1162}, {1014, 1023, 1071, 1066}, {1129, 1102, 1160, 1073}, {1175, 1177, 1140, 1199}, {1068, 1043, 1225},
}

const braveTowerMaxLevel = 80

// handleFreshChoiceFightLevel CMD 2428 试炼之塔选择关卡
// 请求: level(4) 选择的层数，0 表示“继续挑战”（从已到达层数下一层开始）
// 响应: curFightLevel(4) + bossCount(4) + bossCount×bossId(4)，与 FreshChoiceLevelRequestInfo 解析一致
func handleFreshChoiceFightLevel(ctx *gameserver.HandlerContext) {
	level := uint32(1)
	if len(ctx.Body) >= 4 {
		level = binary.BigEndian.Uint32(ctx.Body[0:4])
	}
	u := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	if level == 0 {
		level = uint32(u.MaxFreshStage + 1)
		if level < 1 {
			level = 1
		}
	}
	if level > freshTowerMaxLevel {
		level = freshTowerMaxLevel
	}
	idx := int(level) - 1
	if idx < 0 {
		idx = 0
	}
	bossID := freshTowerBossIDs[idx]
	// 同步当前选择的层数到服务端，供 2429 返回下一层 Boss 及战斗胜利后进度持久化
	u.CurFreshStage = int(level)
	body := make([]byte, 12)
	binary.BigEndian.PutUint32(body[0:4], level)
	binary.BigEndian.PutUint32(body[4:8], 1)
	binary.BigEndian.PutUint32(body[8:12], bossID)
	ctx.GameServer.SendResponse(ctx.ClientData, 2428, ctx.UserID, ctx.SeqID, body)
}

// handleFreshStartFightLevel CMD 2429 试炼之塔开始战斗
// 请求: 无
// 响应: 1) 2429 包体 curLevel(4)+bossCount(4)+bossId(4) 为下一层 Boss；2) 再推送 2503 进入对战（与 2411 一致，客户端收 2503 后打开对战并发 2404）
// 当前层由 CurFreshStage 决定，本场敌人为当前层 Boss，打赢后下一层 Boss 已在 2429 包体中返回
func handleFreshStartFightLevel(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	curLevel := user.CurFreshStage
	if curLevel < 1 {
		curLevel = 1
	}
	if curLevel > freshTowerMaxLevel {
		curLevel = freshTowerMaxLevel
	}
	curIdx := curLevel - 1
	if curIdx < 0 {
		curIdx = 0
	}
	currentBossID := freshTowerBossIDs[curIdx]

	// 1) 返回 2429：下一层层数 + 下一层 Boss（客户端用于打赢后展示下一层）
	nextLevel := curLevel + 1
	if nextLevel > freshTowerMaxLevel {
		nextLevel = freshTowerMaxLevel
	}
	nextIdx := nextLevel - 1
	if nextIdx < 0 {
		nextIdx = 0
	}
	nextBossID := freshTowerBossIDs[nextIdx]
	body2429 := make([]byte, 12)
	binary.BigEndian.PutUint32(body2429[0:4], uint32(nextLevel))
	binary.BigEndian.PutUint32(body2429[4:8], 1)
	binary.BigEndian.PutUint32(body2429[8:12], nextBossID)
	ctx.GameServer.SendResponse(ctx.ClientData, 2429, ctx.UserID, ctx.SeqID, body2429)

	// 2) 写入 BattleState（地图 600、当前层 Boss），供 2503/2404/2504 使用，再推送 2503 进入对战
	enemyLevel := 5 + (curLevel-1)/2
	if enemyLevel > 50 {
		enemyLevel = 50
	}
	ctx.GameServer.BattleMu.Lock()
	battle, _ := ctx.GameServer.BattleStates[ctx.UserID]
	if battle == nil {
		battle = &gameserver.BattleState{}
	}
	battle.EnemyID = int(currentBossID)
	battle.EnemyLevel = enemyLevel
	battle.EnemyShiny = 0
	battle.EnemyDisplayName = ""
	battle.IsActive = true
	battle.BattleMapID = 600
	battle.BossRegion = 0
	battle.RoundCount = 0
	battle.LastHitWasCrit = false
	activeIdx := getFirstAvailablePetIndex(user)
	if activeIdx < 0 {
		ctx.GameServer.BattleMu.Unlock()
		return
	}
	battle.ActivePetIndex = activeIdx
	if activeIdx >= 0 && activeIdx < len(user.Pets) {
		battle.ActivePetCatchTime = user.Pets[activeIdx].CatchTime
	}
	ctx.GameServer.BattleStates[ctx.UserID] = battle
	ctx.GameServer.BattleMu.Unlock()

	body2503 := buildNoteReadyToFightInfo(ctx, uint32(currentBossID))
	ctx.GameServer.SendResponse(ctx.ClientData, 2503, ctx.UserID, ctx.SeqID, body2503)
}

// handleChoiceFightLevel CMD 2414 勇者之塔选择关卡
// 请求: level(4) 选择的层数，0 表示“继续挑战”（从已到达层数下一层开始）
// 响应: curFightLevel(4) + bossCount(4) + bossCount×bossId(4)，与 ChoiceLevelRequestInfo 解析一致
func handleChoiceFightLevel(ctx *gameserver.HandlerContext) {
	level := uint32(1)
	if len(ctx.Body) >= 4 {
		level = binary.BigEndian.Uint32(ctx.Body[0:4])
	}
	u := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	if level == 0 {
		if u.MaxStage <= 0 {
			level = 1
		} else {
			level = uint32(u.MaxStage + 1)
		}
	}
	if level > braveTowerMaxLevel {
		level = braveTowerMaxLevel
	}
	idx := int(level) - 1
	if idx < 0 {
		idx = 0
	}
	bossList := braveTowerBossIDs[idx]
	if len(bossList) == 0 {
		bossList = []uint32{15}
	}
	// 当前勇者之塔层数记录在 CurStage
	u.CurStage = int(level)
	body := make([]byte, 8+len(bossList)*4)
	binary.BigEndian.PutUint32(body[0:4], level)
	binary.BigEndian.PutUint32(body[4:8], uint32(len(bossList)))
	for i, id := range bossList {
		binary.BigEndian.PutUint32(body[8+i*4:12+i*4], id)
	}
	ctx.GameServer.SendResponse(ctx.ClientData, 2414, ctx.UserID, ctx.SeqID, body)
}

// handleStartFightLevel CMD 2415 勇者之塔开始战斗
// 请求: 无
// 响应: 1) 2415 包体 nextLevel(4)+bossCount(4)+nextBossIds 为“下一层”全部 Boss；
//      2) 写入 BattleState（地图 500、当前层首只 Boss、BraveTowerBossIDs 本层全部），再推送 2503 进入对战
func handleStartFightLevel(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	curLevel := user.CurStage
	if curLevel < 1 {
		curLevel = 1
	}
	if curLevel > braveTowerMaxLevel {
		curLevel = braveTowerMaxLevel
	}
	curIdx := curLevel - 1
	if curIdx < 0 {
		curIdx = 0
	}
	curBossList := braveTowerBossIDs[curIdx]
	if len(curBossList) == 0 {
		curBossList = []uint32{15}
	}
	currentBossID := curBossList[0]

	nextLevel := curLevel + 1
	if nextLevel > braveTowerMaxLevel {
		nextLevel = braveTowerMaxLevel
	}
	nextIdx := nextLevel - 1
	if nextIdx < 0 {
		nextIdx = 0
	}
	nextBossList := braveTowerBossIDs[nextIdx]
	if len(nextBossList) == 0 {
		nextBossList = []uint32{15}
	}
	body2415 := make([]byte, 8+len(nextBossList)*4)
	binary.BigEndian.PutUint32(body2415[0:4], uint32(nextLevel))
	binary.BigEndian.PutUint32(body2415[4:8], uint32(len(nextBossList)))
	for i, id := range nextBossList {
		binary.BigEndian.PutUint32(body2415[8+i*4:12+i*4], id)
	}
	ctx.GameServer.SendResponse(ctx.ClientData, 2415, ctx.UserID, ctx.SeqID, body2415)

	// 第1层10级，以后每层+2级
	enemyLevel := 10 + (curLevel-1)*2
	if enemyLevel > 100 {
		enemyLevel = 100
	}
	braveBossIDs := make([]int, len(curBossList))
	for i, id := range curBossList {
		braveBossIDs[i] = int(id)
	}
	ctx.GameServer.BattleMu.Lock()
	battle, _ := ctx.GameServer.BattleStates[ctx.UserID]
	if battle == nil {
		battle = &gameserver.BattleState{}
	}
	battle.EnemyID = int(currentBossID)
	battle.EnemyLevel = enemyLevel
	battle.EnemyShiny = 0
	battle.EnemyDisplayName = ""
	battle.IsActive = true
	battle.BattleMapID = 500
	battle.BossRegion = 0
	battle.RoundCount = 0
	battle.LastHitWasCrit = false
	battle.BraveTowerBossIndex = 0
	battle.BraveTowerBossIDs = braveBossIDs
	activeIdx := getFirstAvailablePetIndex(user)
	if activeIdx < 0 {
		ctx.GameServer.BattleMu.Unlock()
		return
	}
	battle.ActivePetIndex = activeIdx
	if activeIdx >= 0 && activeIdx < len(user.Pets) {
		battle.ActivePetCatchTime = user.Pets[activeIdx].CatchTime
	}
	ctx.GameServer.BattleStates[ctx.UserID] = battle
	ctx.GameServer.BattleMu.Unlock()

	body2503 := buildNoteReadyToFightInfo(ctx, uint32(currentBossID))
	ctx.GameServer.SendResponse(ctx.ClientData, 2503, ctx.UserID, ctx.SeqID, body2503)
}

// 精灵收集计划奖励：第1期=三主宠选1(1/4/7)，第2期=派派(71)，第3期=尹赫(275)
var petCollectRewardMap = map[uint32]int{
	1: 4,   // 第1期默认伊优；客户端可传 param3：1=布布种子,2=小火猴,3=伊优
	2: 71,  // 派派
	3: 275, // 尹赫
}

// handlePetCollect CMD 2311 精灵收集计划领取奖励
// 请求: branch(4) + planId(4) [ + choice(4) 第1期三主宠选1：1=布布种子 2=小火猴 3=伊优 ]
// 响应: petID(4) + catchTime(4)
func handlePetCollect(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	branch := uint32(1)
	if len(ctx.Body) >= 4 {
		branch = binary.BigEndian.Uint32(ctx.Body[0:4])
	}
	rewardPetID, ok := petCollectRewardMap[branch]
	if !ok {
		rewardPetID = 4
	}
	// 精灵收集计划 1~3 期奖励每期只能领取一次：后端按期记录是否已领取
	if branch >= 1 && branch <= 3 {
		alreadyClaimed := false
		for _, b := range user.PetCollectRewardClaimed {
			if b == int(branch) {
				alreadyClaimed = true
				break
			}
		}
		if alreadyClaimed {
			// 已领取时仍返回一个合法响应，这里用 0 表示无新精灵（客户端可据此不再弹奖励）
			body := make([]byte, 8)
			binary.BigEndian.PutUint32(body[0:4], 0)
			binary.BigEndian.PutUint32(body[4:8], 0)
			ctx.GameServer.SendResponse(ctx.ClientData, 2311, ctx.UserID, ctx.SeqID, body)
			logger.Info(fmt.Sprintf("[2311] 精灵收集计划重复领取: user=%d branch=%d 已经领取过", ctx.UserID, branch))
			return
		}
	}
	if branch == 1 && len(ctx.Body) >= 12 {
		choice := binary.BigEndian.Uint32(ctx.Body[8:12])
		if id, ok := novicePetMap[choice]; ok {
			rewardPetID = id
		}
	}
	newCatchTime := int(time.Now().Unix())
	rand.Seed(time.Now().UnixNano() + int64(newCatchTime))
	newPet := userdb.Pet{
		ID:        rewardPetID,
		CatchTime: newCatchTime,
		Level:     1,
		DV:        rand.Intn(32),
		Nature:    rand.Intn(25),
		Exp:       0,
		Name:      "",
		Shiny:     0,
	}
	if len(user.Pets) >= 6 {
		if user.StoragePets == nil {
			user.StoragePets = []userdb.Pet{}
		}
		user.StoragePets = append(user.StoragePets, newPet)
	} else {
		user.Pets = append(user.Pets, newPet)
	}
	if ctx.GameServer.UserDB != nil {
		// 记录该期奖励已领取
		if branch >= 1 && branch <= 3 {
			user.PetCollectRewardClaimed = append(user.PetCollectRewardClaimed, int(branch))
		}
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
		ctx.GameServer.UserDB.RecordCatch(ctx.UserID, rewardPetID)
	}
	body := make([]byte, 8)
	binary.BigEndian.PutUint32(body[0:4], uint32(rewardPetID))
	binary.BigEndian.PutUint32(body[4:8], uint32(newCatchTime))
	ctx.GameServer.SendResponse(ctx.ClientData, 2311, ctx.UserID, ctx.SeqID, body)
	logger.Info(fmt.Sprintf("[2311] 精灵收集计划领取: branch=%d PetID=%d CatchTime=%d", branch, rewardPetID, newCatchTime))
}

// buildArenaInfoBody 构建 ArenaInfo 协议体（对齐 ArenaInfo.as：flag+hostID+hostNick(16)+hostWins+challengerID=32 字节）
func buildArenaInfoBody(flag, hostID, hostWins, challengerID uint32, hostNick string) []byte {
	body := make([]byte, 32)
	binary.BigEndian.PutUint32(body[0:4], flag)
	binary.BigEndian.PutUint32(body[4:8], hostID)
	putFixedString(body, 8, hostNick, 16)
	binary.BigEndian.PutUint32(body[24:28], hostWins)
	binary.BigEndian.PutUint32(body[28:32], challengerID)
	return body
}

// buildGaiyaEffectInfoBody 构建盖亚魂印信息协议体，对齐 GaiyaEffectInfo:
// defEffectID(4) + count(4) + [effectID(4)] * count
func buildGaiyaEffectInfoBody(user *userdb.GameData) []byte {
	if user == nil {
		return make([]byte, 8)
	}
	uniq := make(map[int]bool)
	for _, id := range user.GaiyaEffects {
		if id == 1 || id == 2 || id == 3 {
			uniq[id] = true
		}
	}
	effects := make([]int, 0, len(uniq))
	for id := range uniq {
		effects = append(effects, id)
	}
	sort.Ints(effects)

	defID := user.GaiyaDefaultEffect
	// 若未设置默认魂印：
	// - 已解锁列表非空，则使用第一个已解锁效果；
	// - 否则默认 1（嗜血之力），以兼容旧面板对 defEffectID 的假设。
	if defID == 0 {
		if len(effects) > 0 {
			defID = effects[0]
		} else {
			defID = 1
		}
	}

	buf := make([]byte, 8+4*len(effects))
	binary.BigEndian.PutUint32(buf[0:4], uint32(defID))
	binary.BigEndian.PutUint32(buf[4:8], uint32(len(effects)))
	off := 8
	for _, id := range effects {
		binary.BigEndian.PutUint32(buf[off:off+4], uint32(id))
		off += 4
	}
	return buf
}

// handleArenaGetInfo CMD 2419 ARENA_GET_INFO 获取擂台信息（地图 102 进图时必调，客户端期望 ArenaInfo 格式）
func handleArenaGetInfo(ctx *gameserver.HandlerContext) {
	// 暂不持久化擂台状态，始终返回空擂台（flag=0）
	body := buildArenaInfoBody(0, 0, 0, 0, "")
	ctx.GameServer.SendResponse(ctx.ClientData, 2419, ctx.UserID, ctx.SeqID, body)
}

// handleArenaSetOwenr CMD 2417 ARENA_SET_OWENR 成为擂主（空擂台时点击）
func handleArenaSetOwenr(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	nick := ""
	if user != nil && user.Nick != "" {
		nick = user.Nick
	} else {
		nick = fmt.Sprintf("Seer%d", ctx.UserID)
	}
	// 返回当前用户为擂主（暂不持久化，下次 ARENA_GET_INFO 仍为空）
	body := buildArenaInfoBody(1, uint32(ctx.UserID), 0, 0, nick)
	ctx.GameServer.SendResponse(ctx.ClientData, 2417, ctx.UserID, ctx.SeqID, body)
}

// handleArenaFightOwenr CMD 2418 ARENA_FIGHT_OWENR 挑战擂主
func handleArenaFightOwenr(ctx *gameserver.HandlerContext) {
	// 无 PvP 匹配，返回 flag=2 表示「战斗进行中」避免客户端卡死
	body := buildArenaInfoBody(2, 0, 0, 0, "")
	ctx.GameServer.SendResponse(ctx.ClientData, 2418, ctx.UserID, ctx.SeqID, body)
}

// handleArenaUpfight CMD 2420 ARENA_UPFIGHT 放弃擂台
func handleArenaUpfight(ctx *gameserver.HandlerContext) {
	ctx.GameServer.SendResponse(ctx.ClientData, 2420, ctx.UserID, ctx.SeqID, []byte{0, 0, 0, 0})
}

// handleArenaOwenrAcce CMD 2422 ARENA_OWENR_ACCE 接受擂主（战斗胜利后）
func handleArenaOwenrAcce(ctx *gameserver.HandlerContext) {
	ctx.GameServer.SendResponse(ctx.ClientData, 2422, ctx.UserID, ctx.SeqID, []byte{0, 0, 0, 0})
}

// handleArenaOwenrOut CMD 2423 ARENA_OWENR_OUT 擂主离开（客户端可能请求或服务端推送）
func handleArenaOwenrOut(ctx *gameserver.HandlerContext) {
	ctx.GameServer.SendResponse(ctx.ClientData, 2423, ctx.UserID, ctx.SeqID, []byte{0, 0, 0, 0})
}

// handleOpenDarkPortal CMD 2424 OPEN_DARKPORTAL 暗黑武斗场开门
// 请求：param1(4) 为客户端的「门槽位 ID」（DarkPortalModel.curDoor），并非 0-10 的门序号
// 响应：该门 BOSS 精灵 ID(4) 大端，客户端用于显示并进入 503-513 门内地图战斗
func handleOpenDarkPortal(ctx *gameserver.HandlerContext) {
	// 客户端传的是 curDoor，从 0 开始按 6 个一组映射到第 N 门：
	// curDoor 0-5   -> 第1门（doorIndex=0，对应地图503）
	// curDoor 6-11  -> 第2门（doorIndex=1，对应地图504）
	// curDoor 12-17 -> 第3门（doorIndex=2，对应地图505）
	// 以此类推，公式：doorIndex = curDoor / 6
	var curDoor uint32
	if len(ctx.Body) >= 4 {
		curDoor = binary.BigEndian.Uint32(ctx.Body[0:4])
	}
	doorIndex := curDoor / 6
	subIndex := curDoor % 6

	// 先根据 (mapID, param2=subIndex) 从 mapBossConfig 精确解析 BOSS，
	// 这样子门可以对应不同 BOSS，客户端显示的守门精灵也会正确。
	mapID := 503 + int(doorIndex)
	bossID := uint32(171)
	if e, ok := sptboss.GetByMapAndParam(mapID, subIndex); ok {
		bossID = uint32(e.BossPetID)
		logger.Info(fmt.Sprintf("[2424] 暗黑武斗场开门: curDoor=%d doorIndex=%d subIndex=%d mapID=%d -> bossID=%d",
			curDoor, doorIndex, subIndex, mapID, bossID))
	} else if petID, ok := sptboss.GetDarkPortalBoss(doorIndex); ok {
		// 兜底：按大门主 BOSS 映射（老行为）
		bossID = uint32(petID)
		logger.Info(fmt.Sprintf("[2424] 暗黑武斗场开门(兜底): curDoor=%d doorIndex=%d subIndex=%d -> bossID=%d",
			curDoor, doorIndex, subIndex, bossID))
	} else {
		// 再兜底：强制回退到第一门魔牙鲨
		bossID = 171
		logger.Warning(fmt.Sprintf("[2424] 暗黑武斗场开门: curDoor=%d doorIndex=%d subIndex=%d 超出范围，回退到第一门魔牙鲨",
			curDoor, doorIndex, subIndex))
	}

	// 记录玩家当前打开的暗黑之门，供 2425 FIGHT_DARKPORTAL 使用
	ctx.GameServer.DarkPortalDoorsMu.Lock()
	ctx.GameServer.DarkPortalDoors[ctx.UserID] = gameserver.DarkPortalDoorInfo{
		DoorIndex: doorIndex,
		SubIndex:  subIndex,
	}
	ctx.GameServer.DarkPortalDoorsMu.Unlock()

	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body, uint32(bossID))
	ctx.GameServer.SendResponse(ctx.ClientData, 2424, ctx.UserID, ctx.SeqID, body)
}

// handleFightDarkPortal CMD 2425 FIGHT_DARKPORTAL 暗黑武斗场战斗初始化
// 请求：无包体；根据最近一次打开的暗黑之门索引与子门索引选择对应 BOSS 与等级，写入 BattleStates 后回推 2503
func handleFightDarkPortal(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	bossID := uint32(13)
	enemyLevel := 5
	battleMapID := user.MapID

	// 优先使用 OPEN_DARKPORTAL 记录的门索引决定 BOSS（客户端使用 changeLocalMap，本地切图不会更新服务端 MapID）
	ctx.GameServer.DarkPortalDoorsMu.RLock()
	doorInfo, hasDoor := ctx.GameServer.DarkPortalDoors[ctx.UserID]
	ctx.GameServer.DarkPortalDoorsMu.RUnlock()

	if hasDoor {
		// 门索引 0-10 对应地图 503-513（暗黑第一门～第十一门），param2 一律为 0
		mapID := 503 + int(doorInfo.DoorIndex)
		// 子门索引：通过 2424 记录的 SubIndex 作为 param2 传入 mapBossConfig，
		// 例如第11门 mapID=513, param2=0/1/2 分别对应 11-1/11-2/11-3。
		param2 := doorInfo.SubIndex
		if e, ok := sptboss.GetByMapAndParam(mapID, param2); ok {
			bossID = uint32(e.BossPetID)
			enemyLevel = e.Level
			battleMapID = mapID
			logger.Info(fmt.Sprintf("[2425] 暗黑武斗场战斗: doorIndex=%d mapID=%d -> bossID=%d level=%d", doorInfo.DoorIndex, mapID, bossID, enemyLevel))
		} else {
			// 查表失败：直接使用门索引从 darkPortalDoorBoss 数组获取 BOSS ID（兜底方案，忽略子门）
			if bossPetID, ok := sptboss.GetDarkPortalBoss(doorInfo.DoorIndex); ok {
				bossID = uint32(bossPetID)
				enemyLevel = 80 // 暗黑武斗场 BOSS 统一 80 级
				battleMapID = mapID
				logger.Info(fmt.Sprintf("[2425] 暗黑武斗场战斗(兜底GetDarkPortalBoss): doorIndex=%d -> bossID=%d level=%d", doorInfo.DoorIndex, bossID, enemyLevel))
			} else {
				logger.Warning(fmt.Sprintf("[2425] 暗黑武斗场门索引无效: doorIndex=%d，回退到第一门魔牙鲨", doorInfo.DoorIndex))
				bossID = 171
				enemyLevel = 80
				battleMapID = 503
			}
		}
	} else {
		// 未通过 2424 打开之门直接请求 2425：尝试从当前地图 ID 推断门索引（仅当在暗黑武斗场门内地图时）
		mapID := user.MapID
		if mapID >= 503 && mapID <= 513 {
			// 用户在暗黑武斗场门内地图（503-513），反推门索引
			doorIndex := uint32(mapID - 503)
			if e, ok := sptboss.GetByMapAndParam(mapID, 0); ok {
				bossID = uint32(e.BossPetID)
				enemyLevel = e.Level
				battleMapID = mapID
				logger.Info(fmt.Sprintf("[2425] 暗黑武斗场战斗(从MapID反推): mapID=%d doorIndex=%d -> bossID=%d level=%d", mapID, doorIndex, bossID, enemyLevel))
			} else if bossPetID, ok := sptboss.GetDarkPortalBoss(doorIndex); ok {
				bossID = uint32(bossPetID)
				enemyLevel = 80
				battleMapID = mapID
				logger.Info(fmt.Sprintf("[2425] 暗黑武斗场战斗(从MapID反推兜底): mapID=%d doorIndex=%d -> bossID=%d", mapID, doorIndex, bossID))
			} else {
				logger.Warning(fmt.Sprintf("[2425] 暗黑武斗场从MapID反推失败: mapID=%d，回退到第一门", mapID))
				bossID = 171
				enemyLevel = 80
				battleMapID = 503
			}
		} else {
			// 不在暗黑武斗场门内地图，回退到第一门
			logger.Warning(fmt.Sprintf("[2425] 暗黑武斗场用户不在门内地图: mapID=%d，回退到第一门魔牙鲨", mapID))
			bossID = 171
			enemyLevel = 80
			battleMapID = 503
		}
	}

	// 写入 BattleStates，供 2503/2504 使用正确的敌方等级
	ctx.GameServer.BattleMu.Lock()
	battle, ok := ctx.GameServer.BattleStates[ctx.UserID]
	if !ok || battle == nil {
		battle = &gameserver.BattleState{}
	}
	battle.EnemyID = int(bossID)
	battle.EnemyLevel = enemyLevel
	battle.EnemyShiny = 0
	battle.IsActive = true
	battle.BattleMapID = battleMapID
	battle.BossRegion = 0
	battle.RoundCount = 0
	battle.LastHitWasCrit = false

	// 选择第一个有血的精灵作为首发；若无可用精灵则直接结束战斗
	activeIdx := getFirstAvailablePetIndex(user)
	if activeIdx < 0 {
		ctx.GameServer.BattleMu.Unlock()
		logger.Info(fmt.Sprintf("[2425] 无可用精灵（无精灵或全部0血），拒绝进入暗黑武斗场战斗: UID=%d", ctx.UserID))
		overBody := buildFightOverInfo(4, 0) // REASON_ERROR
		ctx.GameServer.SendResponse(ctx.ClientData, 2506, ctx.UserID, ctx.SeqID, overBody)
		return
	}
	battle.ActivePetIndex = activeIdx
	if activeIdx >= 0 && activeIdx < len(user.Pets) {
		battle.ActivePetCatchTime = user.Pets[activeIdx].CatchTime
	}
	ctx.GameServer.BattleStates[ctx.UserID] = battle
	ctx.GameServer.BattleMu.Unlock()

	body := buildNoteReadyToFightInfo(ctx, bossID)
	logger.Info(fmt.Sprintf("[2425] 即将发送 2503 暗黑武斗场准备对战 bossID=%d", bossID))
	ctx.GameServer.SendResponse(ctx.ClientData, 2503, ctx.UserID, ctx.SeqID, body)
}

// handleLeaveDarkPortal CMD 2426 LEAVE_DARKPORTAL 离开暗黑武斗场
// 客户端收到后返回地图110；此处仅返回 4 字节 0，语义与 Lua emptyResponse(4) 对齐
func handleLeaveDarkPortal(ctx *gameserver.HandlerContext) {
	// 离开暗黑武斗场时清除门状态，避免下次误用旧门索引
	ctx.GameServer.DarkPortalDoorsMu.Lock()
	delete(ctx.GameServer.DarkPortalDoors, ctx.UserID)
	ctx.GameServer.DarkPortalDoorsMu.Unlock()

	body := make([]byte, 4)
	ctx.GameServer.SendResponse(ctx.ClientData, 2426, ctx.UserID, ctx.SeqID, body)
}

// handleQingLongCheckEntry CMD 2461 青龙空间进入校验（点击青龙空间传送阵时调用）
// 请求：无包体；响应：4 字节大端 uint32，1=可进入、0=不可进入
// 根据 DefeatedSPTBossIds 判断是否已击败 奈尼芬多(264)、塔西亚(391)、塔克林(274)、厄尔塞拉(421) 四个 SPT BOSS
// 客户端 QingLongController.check 收到 0 时弹出对话「你只有战胜了奈尼芬多、塔西亚、塔克林和厄尔塞拉，我才会让你进去」，收到 1 时允许进入
func handleQingLongCheckEntry(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	var val uint32
	if hasDefeatedQingLongSPTBosses(user) {
		val = 1
	}
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body[0:4], val)
	ctx.GameServer.SendResponse(ctx.ClientData, 2461, ctx.UserID, ctx.SeqID, body)
}

// handleGetQingLongGuardianMask CMD 2462 查询青龙空间四守护兽击败进度
// 请求：无包体；响应：mask(4) 大端，与 DefeatedQingLongGuardianMask 一致，0xF 表示四只全击败可挑战朵拉格
// 客户端 MapProcess_403 进图后请求此 CMD，根据 mask 同步 qingLongStatus，避免本地为 1 但服务端 mask=0 导致界面错乱、点击无反应
func handleGetQingLongGuardianMask(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	mask := user.DefeatedQingLongGuardianMask
	logger.Info(fmt.Sprintf("青龙空间2462: UID=%d mask=%d", ctx.UserID, mask))
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body[0:4], mask)
	ctx.GameServer.SendResponse(ctx.ClientData, 2462, ctx.UserID, ctx.SeqID, body)
}

// putFixedString 将字符串写入固定长度（不足补0）
func putFixedString(buf []byte, off int, s string, n int) {
	b := []byte(s)
	if len(b) > n {
		b = b[:n]
	}
	copy(buf[off:], b)
	for i := len(b); i < n; i++ {
		buf[off+i] = 0
	}
}

// ==================== 系统/支付 完整协议 ====================

// handleGetImageAddress CMD 1005 GET_IMAGE_ADDRESS
// 响应: host(16) + port(2) + session(16)，对齐 Lua system_handlers.handleGetImageAddress
func handleGetImageAddress(ctx *gameserver.HandlerContext) {
	body := make([]byte, 16+2+16)
	putFixedString(body, 0, "127.0.0.1", 16)
	binary.BigEndian.PutUint16(body[16:18], 80)
	putFixedString(body, 18, "", 16)
	ctx.GameServer.SendResponse(ctx.ClientData, 1005, ctx.UserID, ctx.SeqID, body)
}

// handleMoneyBuyProduct CMD 1102 MONEY_BUY_PRODUCT
// 前端 ProductAction.sendPassword 发送：productID(4) + count(2) + md5(password)(32)（后续可能还有字段）
// 响应: unknown(4) + payMoney(4) + remain(4)，前端 MoneyBuyProductInfo 解析。
//
// 本地服实现：不扣米币，直接按 MoneyProductXMLInfo.xml 发放对应物品/金豆。
func handleMoneyBuyProduct(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	payMoney := uint32(0)
	remain := uint32(user.Gold * 100)
	if len(ctx.Body) >= 6 {
		productID := binary.BigEndian.Uint32(ctx.Body[0:4])
		count := int(binary.BigEndian.Uint16(ctx.Body[4:6]))
		if count <= 0 {
			count = 1
		}
		if entry, ok := getMoneyProduct(productID); ok {
			if user.Items == nil {
				user.Items = make(map[string]userdb.Item)
			}
			// 发放金豆
			if entry.Gold > 0 {
				user.Gold += entry.Gold * count
				if user.Gold < 0 {
					user.Gold = 0
				}
			}
			// 发放物品（itemID==5 表示“金豆”占位，实际只按 @gold 加金豆即可）
			for _, itemID := range entry.ItemIDs {
				if itemID <= 0 || itemID == 5 {
					continue
				}
				itemKey := strconv.Itoa(itemID)
				it := user.Items[itemKey]
				it.Count += count
				if it.ExpireTime == 0 {
					it.ExpireTime = 0x057E40
				}
				user.Items[itemKey] = it
				addClothIfNeeded(user, itemID)
			}
			if ctx.GameServer.UserDB != nil {
				ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
			}
			remain = uint32(user.Gold * 100)
			logger.Info(fmt.Sprintf("[1102] 米币购买成功: UID=%d productID=%d count=%d giveItemIDs=%v giveGold=%d",
				ctx.UserID, productID, count, entry.ItemIDs, entry.Gold*count))
		} else {
			logger.Warning(fmt.Sprintf("[1102] 未知米币商品 productID=%d", productID))
		}
	}
	body := make([]byte, 12)
	binary.BigEndian.PutUint32(body[0:4], 0)
	binary.BigEndian.PutUint32(body[4:8], payMoney)
	binary.BigEndian.PutUint32(body[8:12], remain)
	ctx.GameServer.SendResponse(ctx.ClientData, 1102, ctx.UserID, ctx.SeqID, body)
}

// handleGoldCheckRemain CMD 1105 GOLD_CHECK_REMAIN
// 响应: (gold*100)(4)，对齐 Lua
func handleGoldCheckRemain(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body, uint32(user.Gold*100))
	ctx.GameServer.SendResponse(ctx.ClientData, 1105, ctx.UserID, ctx.SeqID, body)
}

// ==================== 协议层/校验 完整协议 ====================

// handleCheckFightCode CMD 1022 验证战斗码，响应空包
func handleCheckFightCode(ctx *gameserver.HandlerContext) {
	ctx.GameServer.SendResponse(ctx.ClientData, 1022, ctx.UserID, ctx.SeqID, []byte{})
}

// handleSystemMessage CMD 8002 系统消息（客户端发，服回空）
func handleSystemMessage(ctx *gameserver.HandlerContext) {
	ctx.GameServer.SendResponse(ctx.ClientData, 8002, ctx.UserID, ctx.SeqID, []byte{})
}

// handleOpenBagGet CMD 9049 响应: count(4)=0 + extra(4)=0
func handleOpenBagGet(ctx *gameserver.HandlerContext) {
	body := make([]byte, 8)
	ctx.GameServer.SendResponse(ctx.ClientData, 9049, ctx.UserID, ctx.SeqID, body)
}

// handleGetPetInfoAlt CMD 11003 另一套编号的 GET_PET_INFO，响应: petCount(4)=0
func handleGetPetInfoAlt(ctx *gameserver.HandlerContext) {
	body := make([]byte, 4)
	ctx.GameServer.SendResponse(ctx.ClientData, 11003, ctx.UserID, ctx.SeqID, body)
}

// handleGetPetByCatchTime CMD 11007 响应空包
func handleGetPetByCatchTime(ctx *gameserver.HandlerContext) {
	ctx.GameServer.SendResponse(ctx.ClientData, 11007, ctx.UserID, ctx.SeqID, []byte{})
}

// handleGetSecondBag CMD 11022 响应: count(4)=0
func handleGetSecondBag(ctx *gameserver.HandlerContext) {
	body := make([]byte, 4)
	ctx.GameServer.SendResponse(ctx.ClientData, 11022, ctx.UserID, ctx.SeqID, body)
}

// handleReconnect CMD 41983 RECONNECT 响应空包
func handleReconnect(ctx *gameserver.HandlerContext) {
	ctx.GameServer.SendResponse(ctx.ClientData, 41983, ctx.UserID, ctx.SeqID, []byte{})
}

// handleGetMultiForever CMD 46046 响应: count(4)=5 + 5*uint32(0)=20 共24字节
func handleGetMultiForever(ctx *gameserver.HandlerContext) {
	body := make([]byte, 24)
	binary.BigEndian.PutUint32(body[0:4], 5)
	ctx.GameServer.SendResponse(ctx.ClientData, 46046, ctx.UserID, ctx.SeqID, body)
}

// handleGetSuperValue CMD 40001 响应: count(4)=0
func handleGetSuperValue(ctx *gameserver.HandlerContext) {
	body := make([]byte, 4)
	ctx.GameServer.SendResponse(ctx.ClientData, 40001, ctx.UserID, ctx.SeqID, body)
}

// handleGetSuperValueByIds CMD 40002 响应: count(4)=0
func handleGetSuperValueByIds(ctx *gameserver.HandlerContext) {
	body := make([]byte, 4)
	ctx.GameServer.SendResponse(ctx.ClientData, 40002, ctx.UserID, ctx.SeqID, body)
}

// handleBatchGetBitset CMD 42023 响应: count(4)=0
func handleBatchGetBitset(ctx *gameserver.HandlerContext) {
	body := make([]byte, 4)
	ctx.GameServer.SendResponse(ctx.ClientData, 42023, ctx.UserID, ctx.SeqID, body)
}

// handleGetMultiForeverByDb CMD 46057 响应: count(4)=0
func handleGetMultiForeverByDb(ctx *gameserver.HandlerContext) {
	body := make([]byte, 4)
	ctx.GameServer.SendResponse(ctx.ClientData, 46057, ctx.UserID, ctx.SeqID, body)
}

// handleGetForeverValue CMD 41080 响应: 4 字节 0
func handleGetForeverValue(ctx *gameserver.HandlerContext) {
	body := make([]byte, 4)
	ctx.GameServer.SendResponse(ctx.ClientData, 41080, ctx.UserID, ctx.SeqID, body)
}

// handleFriendListAlt CMD 47334 好友列表另一套编号，响应: count(4)=0
func handleFriendListAlt(ctx *gameserver.HandlerContext) {
	var friends []userdb.Friend
	if ctx.GameServer.UserDB != nil {
		friends = ctx.GameServer.UserDB.GetFriends(ctx.UserID)
	} else {
		user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
		friends = user.Friends
	}
	body := make([]byte, 0, 4+len(friends)*8)
	tmp := make([]byte, 4)
	binary.BigEndian.PutUint32(tmp, uint32(len(friends)))
	body = append(body, tmp...)
	for _, f := range friends {
		binary.BigEndian.PutUint32(tmp, uint32(f.UserID))
		body = append(body, tmp...)
		binary.BigEndian.PutUint32(tmp, uint32(f.TimePoke))
		body = append(body, tmp...)
	}
	ctx.GameServer.SendResponse(ctx.ClientData, 47334, ctx.UserID, ctx.SeqID, body)
}

// handleBlacklistAlt CMD 47335 黑名单另一套编号，响应: blackCount(4) + [userId(4)]*n
func handleBlacklistAlt(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	body := make([]byte, 0, 4+len(user.Blacklist)*4)
	tmp := make([]byte, 4)
	binary.BigEndian.PutUint32(tmp, uint32(len(user.Blacklist)))
	body = append(body, tmp...)
	for _, b := range user.Blacklist {
		binary.BigEndian.PutUint32(tmp, uint32(b.UserID))
		body = append(body, tmp...)
	}
	ctx.GameServer.SendResponse(ctx.ClientData, 47335, ctx.UserID, ctx.SeqID, body)
}

// handleSystemTimeAlt CMD 10301 系统时间另一套编号，响应同 1002: time(4)+4
func handleSystemTimeAlt(ctx *gameserver.HandlerContext) {
	body := make([]byte, 8)
	binary.BigEndian.PutUint32(body[0:4], uint32(time.Now().Unix()))
	ctx.GameServer.SendResponse(ctx.ClientData, 10301, ctx.UserID, ctx.SeqID, body)
}

// handleItemListAlt CMD 4475 物品列表另一套编号，响应: itemCount(4)=0 + updateTime(4)=0
func handleItemListAlt(ctx *gameserver.HandlerContext) {
	body := make([]byte, 8)
	ctx.GameServer.SendResponse(ctx.ClientData, 4475, ctx.UserID, ctx.SeqID, body)
}

// ==================== 地图/玩家 完整协议 ====================

// handleChangeNickName CMD 2061 修改昵称
// 请求: newNick(16)。响应: userId(4)+newNick(16)，对齐 Lua map_handlers.handleChangeNickName
func handleChangeNickName(ctx *gameserver.HandlerContext) {
	newNick := ""
	if len(ctx.Body) >= 16 {
		newNick = string(bytes.TrimRight(ctx.Body[0:16], "\x00"))
	}
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	user.Nick = newNick
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}
	body := make([]byte, 4+16)
	binary.BigEndian.PutUint32(body[0:4], uint32(ctx.UserID))
	putFixedString(body, 4, newNick, 16)
	ctx.GameServer.SendResponse(ctx.ClientData, 2061, ctx.UserID, ctx.SeqID, body)

	// 同步给同地图玩家，刷新昵称/外观
	if user.MapID > 0 {
		listBody := buildMapPlayerListForMap(ctx.GameServer, user.MapID)
		ctx.GameServer.BroadcastToMap(user.MapID, 0, 2003, listBody)
	}
}

// handleChangeColor CMD 2063 修改颜色
// 请求: newColor(4)。响应: userId(4)+newColor(4)+cost(4)+remain(4)，对齐 Lua
func handleChangeColor(ctx *gameserver.HandlerContext) {
	newColor := uint32(0)
	if len(ctx.Body) >= 4 {
		newColor = binary.BigEndian.Uint32(ctx.Body[0:4])
	}
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	user.Color = int(newColor)
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}
	body := make([]byte, 16)
	binary.BigEndian.PutUint32(body[0:4], uint32(ctx.UserID))
	binary.BigEndian.PutUint32(body[4:8], newColor)
	binary.BigEndian.PutUint32(body[8:12], 0)
	binary.BigEndian.PutUint32(body[12:16], uint32(user.Coins))
	ctx.GameServer.SendResponse(ctx.ClientData, 2063, ctx.UserID, ctx.SeqID, body)

	// 同步给同地图玩家，刷新外观
	if user.MapID > 0 {
		listBody := buildMapPlayerListForMap(ctx.GameServer, user.MapID)
		ctx.GameServer.BroadcastToMap(user.MapID, 0, 2003, listBody)
	}
}

// handleDanceAction CMD 2103 跳舞动作
// 请求: aid(4)+atype(4)。响应: userId(4)+aid(4)+atype(4)，对齐 Lua
func handleDanceAction(ctx *gameserver.HandlerContext) {
	aid, atype := uint32(0), uint32(0)
	if len(ctx.Body) >= 8 {
		aid = binary.BigEndian.Uint32(ctx.Body[0:4])
		atype = binary.BigEndian.Uint32(ctx.Body[4:8])
	}
	body := make([]byte, 12)
	binary.BigEndian.PutUint32(body[0:4], uint32(ctx.UserID))
	binary.BigEndian.PutUint32(body[4:8], aid)
	binary.BigEndian.PutUint32(body[8:12], atype)
	ctx.GameServer.SendResponse(ctx.ClientData, 2103, ctx.UserID, ctx.SeqID, body)
}

// handlePeopleTransform CMD 2111 变身
// 请求: transId(4)。响应: userId(4)+transId(4)，对齐 Lua；并广播给同地图其他玩家
func handlePeopleTransform(ctx *gameserver.HandlerContext) {
	transId := uint32(0)
	if len(ctx.Body) >= 4 {
		transId = binary.BigEndian.Uint32(ctx.Body[0:4])
	}
	body := make([]byte, 8)
	binary.BigEndian.PutUint32(body[0:4], uint32(ctx.UserID))
	binary.BigEndian.PutUint32(body[4:8], transId)
	ctx.GameServer.SendResponse(ctx.ClientData, 2111, ctx.UserID, ctx.SeqID, body)
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	if user.MapID > 0 {
		ctx.GameServer.BroadcastToMap(user.MapID, ctx.UserID, 2111, body)
	}
}

// handleOnOrOffFlying CMD 2112 飞行开关
// 请求: flyMode(4)。响应: userId(4)+flyMode(4)，对齐 Lua；并广播给同地图其他玩家
func handleOnOrOffFlying(ctx *gameserver.HandlerContext) {
	flyMode := uint32(0)
	if len(ctx.Body) >= 4 {
		flyMode = binary.BigEndian.Uint32(ctx.Body[0:4])
	}
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}
	body := make([]byte, 8)
	binary.BigEndian.PutUint32(body[0:4], uint32(ctx.UserID))
	binary.BigEndian.PutUint32(body[4:8], flyMode)
	ctx.GameServer.SendResponse(ctx.ClientData, 2112, ctx.UserID, ctx.SeqID, body)
	if user.MapID > 0 {
		ctx.GameServer.BroadcastToMap(user.MapID, ctx.UserID, 2112, body)
	}
}

// ==================== 交换 完整协议 ====================

// handleExchangeNewYear CMD 2065 新年交换，响应: 4 字节 0
func handleExchangeNewYear(ctx *gameserver.HandlerContext) {
	body := make([]byte, 4)
	ctx.GameServer.SendResponse(ctx.ClientData, 2065, ctx.UserID, ctx.SeqID, body)
}

// handleExchangeOre CMD 2251 矿石交换，响应: ret(4)=0 + count(4)=0
func handleExchangeOre(ctx *gameserver.HandlerContext) {
	body := make([]byte, 8)
	ctx.GameServer.SendResponse(ctx.ClientData, 2251, ctx.UserID, ctx.SeqID, body)
}

// handleExchangePetComplete CMD 2902 精灵交换完成，响应: 4 字节 0
func handleExchangePetComplete(ctx *gameserver.HandlerContext) {
	body := make([]byte, 4)
	ctx.GameServer.SendResponse(ctx.ClientData, 2902, ctx.UserID, ctx.SeqID, body)
}

// ==================== 任务 完整协议 ====================

// handleAddTaskBuf CMD 2204 添加/更新任务缓存（NPC 对话进度等）
// 请求（前端）: taskId(4) + buf(20 字节)，SetTaskBuf/TasksManager 为 writeUnsignedInt(taskId)+writeBytes(ByteArray(20))
// 兼容旧格式: taskId(4) + index(1) + value(4)。响应: 4 字节 0
func handleAddTaskBuf(ctx *gameserver.HandlerContext) {
	if len(ctx.Body) >= 4 {
		taskID := binary.BigEndian.Uint32(ctx.Body[0:4])
		user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
		if user.Tasks == nil {
			user.Tasks = make(map[string]userdb.Task)
		}
		key := strconv.FormatUint(uint64(taskID), 10)
		t := user.Tasks[key]
		if t.Buf == nil {
			t.Buf = make(map[int]int)
		}
		if len(ctx.Body) >= 24 {
			for i := 0; i < 20; i++ {
				t.Buf[i] = int(ctx.Body[4+i])
			}
		} else if len(ctx.Body) >= 5 {
			index := int(ctx.Body[4])
			value := 0
			if len(ctx.Body) >= 9 {
				value = int(binary.BigEndian.Uint32(ctx.Body[5:9]))
			}
			t.Buf[index] = value
		}
		user.Tasks[key] = t
		if ctx.GameServer.UserDB != nil {
			ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
		}
	}
	body := make([]byte, 4)
	ctx.GameServer.SendResponse(ctx.ClientData, 2204, ctx.UserID, ctx.SeqID, body)
}

// handleGetDailyTaskBuf CMD 2234 获取每日任务缓存（GET_DAILY_TASK_BUF）
// 为了兼容前端 TaskBufInfo 以及 TasksManager.getProStatus /
// setProStatus / getProStatusList 的实现，这里的返回结构需要与
// 2203(handleGetTaskBuf) 完全一致：
//   taskId(4) + flag(4) + buf(20 字节)
// 其中 buf 中的每一位都被当作 boolean 使用：前端会执行
//   buf.position = i; readBoolean()
// 来读取第 i 个进度点。
func handleGetDailyTaskBuf(ctx *gameserver.HandlerContext) {
	var taskID uint32
	if len(ctx.Body) >= 4 {
		taskID = binary.BigEndian.Uint32(ctx.Body[0:4])
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	flag := uint32(0)
	bufBytes := make([]byte, 20)

	// 复用 user.Tasks[taskID].Buf 与 2203 相同的存储结构，
	// 这样无论是普通任务还是每日任务，进度位读写逻辑都一致。
	if user.Tasks != nil {
		key := strconv.FormatUint(uint64(taskID), 10)
		if t, ok := user.Tasks[key]; ok && t.Buf != nil {
			for i := 0; i < 20; i++ {
				if v, ok := t.Buf[i]; ok {
					bufBytes[i] = byte(v & 0xff)
				}
			}
			flag = 1
		}
	}

	body := make([]byte, 4+4+20)
	binary.BigEndian.PutUint32(body[0:4], taskID)
	binary.BigEndian.PutUint32(body[4:8], flag)
	copy(body[8:28], bufBytes)
	ctx.GameServer.SendResponse(ctx.ClientData, 2234, ctx.UserID, ctx.SeqID, body)
}

// handleAddDailyTaskBuf CMD 2235 添加/更新每日任务缓存（与 2204 逻辑一致）
// 前端 TasksManager.setProStatus / sendCompletePro 在处理每日任务时会通过
// ADD_DAILY_TASK_BUF 写入进度位；后续 getProStatus(404, i) 会依赖这些位。
// 请求格式与 2204 相同：
//   - 完整写入: taskId(4) + buf(20 字节)
//   - 单点写入: taskId(4) + index(1) + value(4)
// 响应: 4 字节 0
func handleAddDailyTaskBuf(ctx *gameserver.HandlerContext) {
	if len(ctx.Body) >= 4 {
		taskID := binary.BigEndian.Uint32(ctx.Body[0:4])
		user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
		if user.Tasks == nil {
			user.Tasks = make(map[string]userdb.Task)
		}
		key := strconv.FormatUint(uint64(taskID), 10)
		t := user.Tasks[key]
		if t.Buf == nil {
			t.Buf = make(map[int]int)
		}
		if len(ctx.Body) >= 24 {
			for i := 0; i < 20; i++ {
				t.Buf[i] = int(ctx.Body[4+i])
			}
		} else if len(ctx.Body) >= 5 {
			index := int(ctx.Body[4])
			value := 0
			if len(ctx.Body) >= 9 {
				value = int(binary.BigEndian.Uint32(ctx.Body[5:9]))
			}
			t.Buf[index] = value
		}
		user.Tasks[key] = t
		if ctx.GameServer.UserDB != nil {
			ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
		}
	}
	body := make([]byte, 4)
	ctx.GameServer.SendResponse(ctx.ClientData, 2235, ctx.UserID, ctx.SeqID, body)
}

// handleGetDailyValue CMD 2707 获取/查询每日计数（GET_DAILY_VALUE / FUCK_SHINEHOO_TIMES）
// 旧服根据不同的 limitID 返回对应次数或按位标记，这里统一返回 0，表示“当日尚未使用”，
// 以保证依赖该接口的活动/面板（如异能王六重试炼终极挑战、部分日常奖励等）能够正常放行。
func handleGetDailyValue(ctx *gameserver.HandlerContext) {
	// 请求体通常为 4 字节 limitID，这里暂不区分具体含义，仅返回单个 0
	body := make([]byte, 4)
	ctx.GameServer.SendResponse(ctx.ClientData, 2707, ctx.UserID, ctx.SeqID, body)
}

// handleFuckShinehooTimes CMD 0 兼容旧异能王六重试炼终极挑战次数查询
// 部分独立 SWF 中 CommandID.FUCK_SHINEHOO_TIMES 常量值为 0。
// 为兼容盖亚特训面板的实现，这里返回:
// ret(4字节=0) + GaiyaEffectInfo(defEffectID/count/effects...)。
func handleFuckShinehooTimes(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	info := buildGaiyaEffectInfoBody(user)
	body := make([]byte, 4+len(info))
	// ret=0
	binary.BigEndian.PutUint32(body[0:4], 0)
	copy(body[4:], info)
	ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
}

// handleGetYiNengTaskStatus CMD 2708 地图677 六重试炼任务(758~763)完成状态
// 响应 6 字节：第 i 字节为 1 表示任务 758+i 已完成，0 表示未完成。客户端据此同步 TasksManager 并显示挑战真身按钮/水晶点亮。
func handleGetYiNengTaskStatus(ctx *gameserver.HandlerContext) {
	gameData := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	body := make([]byte, 6)
	for i, tid := range []int{758, 759, 760, 761, 762, 763} {
		if gameData.Tasks != nil {
			if t, ok := gameData.Tasks[strconv.Itoa(tid)]; ok && t.Status == "3" {
				body[i] = 1
			}
		}
	}
	ctx.GameServer.SendResponse(ctx.ClientData, 2708, ctx.UserID, ctx.SeqID, body)
}

// handleInform CMD 8001 通知，响应: type(4)+userID(4)+nick(16)+accept(4)+serverID(4)+mapType(4)+mapID(4)+mapName(64)=104 字节
func handleInform(ctx *gameserver.HandlerContext) {
	body := make([]byte, 104)
	binary.BigEndian.PutUint32(body[0:4], 0)
	binary.BigEndian.PutUint32(body[4:8], uint32(ctx.UserID))
	putFixedString(body, 8, "", 16)
	binary.BigEndian.PutUint32(body[24:28], 0)
	binary.BigEndian.PutUint32(body[28:32], 1)
	binary.BigEndian.PutUint32(body[32:36], 0)
	binary.BigEndian.PutUint32(body[36:40], 301)
	putFixedString(body, 40, "", 64)
	ctx.GameServer.SendResponse(ctx.ClientData, 8001, ctx.UserID, ctx.SeqID, body)
}

// handleGetBossMonster CMD 8004 获取 BOSS 怪物，响应: bonusID(4)+petID(4)+captureTm(4)+itemCount(4)=16 字节
func handleGetBossMonster(ctx *gameserver.HandlerContext) {
	body := make([]byte, 16)
	ctx.GameServer.SendResponse(ctx.ClientData, 8004, ctx.UserID, ctx.SeqID, body)
}

// handleAchieveTitleList CMD 3403 ACHIEVETITLELIST 获取已获得称号列表
// 响应与 AchieveTitleInfo 一致: count(4) + count×titleId(4)，客户端据此在称号栏显示可选项
func handleAchieveTitleList(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	ids := user.TitleIDs
	if ids == nil {
		ids = []int{}
	}
	body := make([]byte, 4+4*len(ids))
	binary.BigEndian.PutUint32(body[0:4], uint32(len(ids)))
	for i, id := range ids {
		binary.BigEndian.PutUint32(body[4+4*i:4+4*(i+1)], uint32(id))
	}
	ctx.GameServer.SendResponse(ctx.ClientData, 3403, ctx.UserID, ctx.SeqID, body)
}

// handleSetTitle CMD 3404 SETTITLE 设置当前称号
// 请求: titleId(4)。响应: 8 字节 = userID(4) + titleID(4)，与 SetTitleCmdListener.onSetTitle 的 readUnsignedInt×2 对齐
// 仅当 titleID 为 0 或属于用户已获得的 TitleIDs 时才允许设置，否则保持原 CurTitle
func handleSetTitle(ctx *gameserver.HandlerContext) {
	titleID := 0
	if len(ctx.Body) >= 4 {
		titleID = int(binary.BigEndian.Uint32(ctx.Body[0:4]))
	}
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	// 仅允许设置为 0（无称号）或已获得的称号
	if titleID != 0 {
		has := false
		for _, id := range user.TitleIDs {
			if id == titleID {
				has = true
				break
			}
		}
		if !has {
			titleID = user.CurTitle
		}
	}
	user.CurTitle = titleID
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}
	body := make([]byte, 8)
	binary.BigEndian.PutUint32(body[0:4], uint32(ctx.UserID))
	binary.BigEndian.PutUint32(body[4:8], uint32(titleID))
	ctx.GameServer.SendResponse(ctx.ClientData, 3404, ctx.UserID, ctx.SeqID, body)
}

// CMD 70005 GET_ACHIEVETITLE 客户端用于“获得新称号”弹窗
const cmdGetAchieveTitle int32 = 70005

// GrantTitle 给玩家授予称号：加入 TitleIDs、落库，若在线则推送 70005 以便客户端弹窗“获得称号”
// 可由任务完成、成就、活动等逻辑调用
func GrantTitle(gs *gameserver.GameServer, userID int64, titleID int) {
	if gs == nil || titleID <= 0 {
		return
	}
	user := gs.GetOrCreateUser(userID)
	for _, id := range user.TitleIDs {
		if id == titleID {
			return
		}
	}
	user.TitleIDs = append(user.TitleIDs, titleID)
	if gs.UserDB != nil {
		gs.UserDB.SaveGameData(userID, user)
	}
	client := gs.GetClientByUserID(userID)
	if client != nil {
		body := make([]byte, 4)
		binary.BigEndian.PutUint32(body, uint32(titleID))
		gs.SendResponse(client, cmdGetAchieveTitle, userID, 0, body)
	}
}

// handleMailGetList CMD 2751 获取邮件列表（空列表）
// 对齐 Lua: mail_handlers.handleMailGetList
func handleMailGetList(ctx *gameserver.HandlerContext) {
	body := make([]byte, 8)
	// total=0, count=0
	ctx.GameServer.SendResponse(ctx.ClientData, 2751, ctx.UserID, ctx.SeqID, body)
}

// handleMailGetUnread CMD 2757 获取未读邮件数（0）
// 对齐 Lua: mail_handlers.handleMailGetUnread
func handleMailGetUnread(ctx *gameserver.HandlerContext) {
	body := make([]byte, 4)
	// unread=0
	ctx.GameServer.SendResponse(ctx.ClientData, 2757, ctx.UserID, ctx.SeqID, body)
}

// handleGetRelationList CMD 2150 获取好友/黑名单列表
// 对齐 Lua: friend_handlers.handleGetRelationList
func handleGetRelationList(ctx *gameserver.HandlerContext) {
	// 从数据库加载最新的好友和黑名单列表（确保数据最新）
	var friends []userdb.Friend
	var black []userdb.BlacklistEntry
	if ctx.GameServer.UserDB != nil {
		friends = ctx.GameServer.UserDB.GetFriends(ctx.UserID)
		black = ctx.GameServer.UserDB.GetBlacklist(ctx.UserID)
		// 更新内存中的用户数据
		user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
		user.Friends = friends
		user.Blacklist = black
	} else {
		user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
		friends = user.Friends
		black = user.Blacklist
	}

	// friendCount(4) + blackCount(4) + [FriendInfo]*n + [BlackInfo]*m
	body := make([]byte, 0, 8+len(friends)*8+len(black)*4)
	tmp := make([]byte, 4)
	binary.BigEndian.PutUint32(tmp, uint32(len(friends)))
	body = append(body, tmp...)
	binary.BigEndian.PutUint32(tmp, uint32(len(black)))
	body = append(body, tmp...)

	for _, f := range friends {
		binary.BigEndian.PutUint32(tmp, uint32(f.UserID))
		body = append(body, tmp...)
		binary.BigEndian.PutUint32(tmp, uint32(f.TimePoke))
		body = append(body, tmp...)
	}
	for _, b := range black {
		binary.BigEndian.PutUint32(tmp, uint32(b.UserID))
		body = append(body, tmp...)
	}

	ctx.GameServer.SendResponse(ctx.ClientData, 2150, ctx.UserID, ctx.SeqID, body)
}

// handleFriendAdd CMD 2151 发送好友请求（需要对方同意）
// 请求: friendID(4)
// 响应: friendID(4) - 返回请求的好友ID
// 同时向对方推送 8001 (INFORM) 通知，type=FRIEND_ADD
func handleFriendAdd(ctx *gameserver.HandlerContext) {
	var friendID int64
	if len(ctx.Body) >= 4 {
		friendID = int64(binary.BigEndian.Uint32(ctx.Body[0:4]))
	}

	if friendID <= 0 {
		// 无效的好友ID
		body := make([]byte, 4)
		ctx.GameServer.SendResponse(ctx.ClientData, 2151, ctx.UserID, ctx.SeqID, body)
		return
	}

	// 不能添加自己为好友
	if friendID == ctx.UserID {
		body := make([]byte, 4)
		ctx.GameServer.SendResponse(ctx.ClientData, 2151, ctx.UserID, ctx.SeqID, body)
		logger.Info(fmt.Sprintf("[2151] 不能添加自己为好友: UID=%d", ctx.UserID))
		return
	}

	// 检查是否已经是好友
	if ctx.GameServer.UserDB != nil {
		if ctx.GameServer.UserDB.IsFriend(ctx.UserID, friendID) {
			body := make([]byte, 4)
			binary.BigEndian.PutUint32(body, uint32(friendID))
			ctx.GameServer.SendResponse(ctx.ClientData, 2151, ctx.UserID, ctx.SeqID, body)
			logger.Info(fmt.Sprintf("[2151] 已经是好友: UID=%d FriendID=%d", ctx.UserID, friendID))
			return
		}
	}

	// 向对方发送好友请求通知（8001 INFORM）
	// InformInfo: type(4) + userID(4) + nick(16) + accept(4) + serverID(4) + mapType(4) + mapID(4) + mapName(64) = 104字节
	requestUser := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	requestNick := requestUser.Nick
	if requestNick == "" {
		requestNick = fmt.Sprintf("Seer%d", ctx.UserID)
	}

	// 构建 InformInfo 包体
	informBody := make([]byte, 104)
	binary.BigEndian.PutUint32(informBody[0:4], 2151) // type = FRIEND_ADD
	binary.BigEndian.PutUint32(informBody[4:8], uint32(ctx.UserID))
	putFixedString := func(s string, n int, offset int) {
		b := []byte(s)
		if len(b) > n {
			b = b[:n]
		}
		copy(informBody[offset:offset+len(b)], b)
		for i := len(b); i < n; i++ {
			informBody[offset+i] = 0
		}
	}
	putFixedString(requestNick, 16, 8)
	binary.BigEndian.PutUint32(informBody[24:28], 0) // accept = 0（请求）
	binary.BigEndian.PutUint32(informBody[28:32], 1) // serverID = 1
	binary.BigEndian.PutUint32(informBody[32:36], 0) // mapType = 0
	mapID := requestUser.MapID
	if mapID <= 0 {
		mapID = 1
	}
	binary.BigEndian.PutUint32(informBody[36:40], uint32(mapID))
	putFixedString("", 64, 40) // mapName

	// 向对方推送通知
	if targetClient := ctx.GameServer.GetClientByUserID(friendID); targetClient != nil && targetClient.LoggedIn {
		ctx.GameServer.SendResponse(targetClient, 8001, friendID, 0, informBody)
		logger.Info(fmt.Sprintf("[2151] 发送好友请求通知: UID=%d -> FriendID=%d", ctx.UserID, friendID))
	} else {
		logger.Info(fmt.Sprintf("[2151] 对方不在线，无法发送好友请求: UID=%d -> FriendID=%d", ctx.UserID, friendID))
	}

	// 响应：返回 friendID（与 Lua 版本一致）
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body, uint32(friendID))
	ctx.GameServer.SendResponse(ctx.ClientData, 2151, ctx.UserID, ctx.SeqID, body)
	logger.Info(fmt.Sprintf("[2151] 发送好友请求: UID=%d FriendID=%d", ctx.UserID, friendID))
}

// handleFriendAnswer CMD 2152 好友请求回复
// 请求: targetID(4) + accept(4) - accept: 1=接受, 0=拒绝
// 响应: accept(4)
// 如果接受，双方都添加为好友，并向请求方发送 8001 (INFORM) 通知
func handleFriendAnswer(ctx *gameserver.HandlerContext) {
	var targetID int64
	var accept uint32
	if len(ctx.Body) >= 8 {
		targetID = int64(binary.BigEndian.Uint32(ctx.Body[0:4]))
		accept = binary.BigEndian.Uint32(ctx.Body[4:8])
	}

	if accept == 1 && targetID > 0 && ctx.GameServer.UserDB != nil {
		// 接受好友请求：双方都添加为好友
		// 1. 我添加对方为好友
		success1, msg1 := ctx.GameServer.UserDB.AddFriend(ctx.UserID, targetID)
		if success1 {
			logger.Info(fmt.Sprintf("[2152] 接受好友请求: UID=%d 添加 FriendID=%d", ctx.UserID, targetID))
			// 更新内存中的用户数据
			user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
			user.Friends = ctx.GameServer.UserDB.GetFriends(ctx.UserID)
		} else {
			logger.Info(fmt.Sprintf("[2152] 接受好友请求失败: UID=%d FriendID=%d Reason=%s", ctx.UserID, targetID, msg1))
		}

		// 2. 对方也添加我为好友（双向好友关系）
		success2, msg2 := ctx.GameServer.UserDB.AddFriend(targetID, ctx.UserID)
		if success2 {
			logger.Info(fmt.Sprintf("[2152] 双向添加好友: FriendID=%d 添加 UID=%d", targetID, ctx.UserID))
			// 更新对方内存中的用户数据
			if targetUser := ctx.GameServer.GetOrCreateUser(targetID); targetUser != nil {
				targetUser.Friends = ctx.GameServer.UserDB.GetFriends(targetID)
			}
		} else {
			logger.Info(fmt.Sprintf("[2152] 双向添加好友失败: FriendID=%d UID=%d Reason=%s", targetID, ctx.UserID, msg2))
		}

		// 3. 向请求方发送接受通知（8001 INFORM）
		// InformInfo: type(4) + userID(4) + nick(16) + accept(4) + serverID(4) + mapType(4) + mapID(4) + mapName(64) = 104字节
		myUser := ctx.GameServer.GetOrCreateUser(ctx.UserID)
		myNick := myUser.Nick
		if myNick == "" {
			myNick = fmt.Sprintf("Seer%d", ctx.UserID)
		}

		informBody := make([]byte, 104)
		binary.BigEndian.PutUint32(informBody[0:4], 2152) // type = FRIEND_ANSWER
		binary.BigEndian.PutUint32(informBody[4:8], uint32(ctx.UserID))
		putFixedString := func(s string, n int, offset int) {
			b := []byte(s)
			if len(b) > n {
				b = b[:n]
			}
			copy(informBody[offset:offset+len(b)], b)
			for i := len(b); i < n; i++ {
				informBody[offset+i] = 0
			}
		}
		putFixedString(myNick, 16, 8)
		binary.BigEndian.PutUint32(informBody[24:28], 1) // accept = 1（已接受）
		binary.BigEndian.PutUint32(informBody[28:32], 1) // serverID = 1
		binary.BigEndian.PutUint32(informBody[32:36], 0) // mapType = 0
		mapID := myUser.MapID
		if mapID <= 0 {
			mapID = 1
		}
		binary.BigEndian.PutUint32(informBody[36:40], uint32(mapID))
		putFixedString("", 64, 40) // mapName

		// 向请求方推送通知
		if requestClient := ctx.GameServer.GetClientByUserID(targetID); requestClient != nil && requestClient.LoggedIn {
			ctx.GameServer.SendResponse(requestClient, 8001, targetID, 0, informBody)
			logger.Info(fmt.Sprintf("[2152] 发送接受通知: UID=%d -> RequestID=%d", ctx.UserID, targetID))
		}
	} else if accept == 0 && targetID > 0 {
		logger.Info(fmt.Sprintf("[2152] 拒绝好友请求: UID=%d FriendID=%d", ctx.UserID, targetID))
		// 可选：向请求方发送拒绝通知
	}

	// 响应：返回 accept
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body, accept)
	ctx.GameServer.SendResponse(ctx.ClientData, 2152, ctx.UserID, ctx.SeqID, body)
}

// handleFriendRemove CMD 2153 删除好友
// 请求: friendID(4)
// 响应: 空
func handleFriendRemove(ctx *gameserver.HandlerContext) {
	var friendID int64
	if len(ctx.Body) >= 4 {
		friendID = int64(binary.BigEndian.Uint32(ctx.Body[0:4]))
	}

	if friendID > 0 && ctx.GameServer.UserDB != nil {
		success := ctx.GameServer.UserDB.RemoveFriend(ctx.UserID, friendID)
		if success {
			logger.Info(fmt.Sprintf("[2153] 删除好友成功: UID=%d FriendID=%d", ctx.UserID, friendID))
			// 更新内存中的用户数据
			user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
			user.Friends = ctx.GameServer.UserDB.GetFriends(ctx.UserID)
		} else {
			logger.Info(fmt.Sprintf("[2153] 删除好友失败（可能不存在）: UID=%d FriendID=%d", ctx.UserID, friendID))
		}
	}

	// 响应：空（与 Lua 版本一致）
	ctx.GameServer.SendResponse(ctx.ClientData, 2153, ctx.UserID, ctx.SeqID, []byte{})
}

// handleBlackAdd CMD 2154 添加黑名单
// 请求: targetID(4)
// 响应: 空
func handleBlackAdd(ctx *gameserver.HandlerContext) {
	var targetID int64
	if len(ctx.Body) >= 4 {
		targetID = int64(binary.BigEndian.Uint32(ctx.Body[0:4]))
	}

	if targetID > 0 && ctx.GameServer.UserDB != nil {
		success, msg := ctx.GameServer.UserDB.AddBlacklist(ctx.UserID, targetID)
		if success {
			logger.Info(fmt.Sprintf("[2154] 添加黑名单成功: UID=%d TargetID=%d", ctx.UserID, targetID))
			// 更新内存中的用户数据
			user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
			user.Blacklist = ctx.GameServer.UserDB.GetBlacklist(ctx.UserID)
		} else {
			logger.Info(fmt.Sprintf("[2154] 添加黑名单失败: UID=%d TargetID=%d Reason=%s", ctx.UserID, targetID, msg))
		}
	}

	// 响应：空（与 Lua 版本一致）
	ctx.GameServer.SendResponse(ctx.ClientData, 2154, ctx.UserID, ctx.SeqID, []byte{})
}

// handleBlackRemove CMD 2155 移除黑名单
// 请求: targetID(4)
// 响应: 空
func handleBlackRemove(ctx *gameserver.HandlerContext) {
	var targetID int64
	if len(ctx.Body) >= 4 {
		targetID = int64(binary.BigEndian.Uint32(ctx.Body[0:4]))
	}

	if targetID > 0 && ctx.GameServer.UserDB != nil {
		success := ctx.GameServer.UserDB.RemoveBlacklist(ctx.UserID, targetID)
		if success {
			logger.Info(fmt.Sprintf("[2155] 移除黑名单成功: UID=%d TargetID=%d", ctx.UserID, targetID))
			// 更新内存中的用户数据
			user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
			user.Blacklist = ctx.GameServer.UserDB.GetBlacklist(ctx.UserID)
		} else {
			logger.Info(fmt.Sprintf("[2155] 移除黑名单失败（可能不存在）: UID=%d TargetID=%d", ctx.UserID, targetID))
		}
	}

	// 响应：空（与 Lua 版本一致）
	ctx.GameServer.SendResponse(ctx.ClientData, 2155, ctx.UserID, ctx.SeqID, []byte{})
}

// handleSeeOnline CMD 2157 查看在线状态
// 请求: count(4) + userIDs[count] (每个4字节)
// 响应: onlineCount(4) + [OnLineInfo]...
// OnLineInfo: userID(4) + serverID(4) + mapType(4) + mapID(4) = 16 bytes
func handleSeeOnline(ctx *gameserver.HandlerContext) {
	var requestCount uint32
	if len(ctx.Body) >= 4 {
		requestCount = binary.BigEndian.Uint32(ctx.Body[0:4])
	}

	// 读取所有请求的用户ID
	userIDs := make([]int64, 0, requestCount)
	for i := uint32(0); i < requestCount; i++ {
		offset := 4 + int(i)*4
		if len(ctx.Body) >= offset+4 {
			userID := int64(binary.BigEndian.Uint32(ctx.Body[offset : offset+4]))
			userIDs = append(userIDs, userID)
		}
	}

	// 构建在线用户列表
	onlineUsers := make([]struct {
		userID   int64
		serverID uint32
		mapType  uint32
		mapID    uint32
	}, 0)

	for _, targetID := range userIDs {
		// 检查用户是否在线
		if client := ctx.GameServer.GetClientByUserID(targetID); client != nil && client.LoggedIn {
			user := ctx.GameServer.GetOrCreateUser(targetID)
			mapID := user.MapID
			if mapID == 0 {
				mapID = 1
			}
			mapType := uint32(0)  // 默认地图类型
			serverID := uint32(1) // 当前服务器ID

			onlineUsers = append(onlineUsers, struct {
				userID   int64
				serverID uint32
				mapType  uint32
				mapID    uint32
			}{
				userID:   targetID,
				serverID: serverID,
				mapType:  mapType,
				mapID:    uint32(mapID),
			})
		}
	}

	// 构建响应: count(4) + [OnLineInfo]...
	body := make([]byte, 0, 4+len(onlineUsers)*16)
	tmp := make([]byte, 4)
	binary.BigEndian.PutUint32(tmp, uint32(len(onlineUsers)))
	body = append(body, tmp...)

	for _, info := range onlineUsers {
		binary.BigEndian.PutUint32(tmp, uint32(info.userID))
		body = append(body, tmp...)
		binary.BigEndian.PutUint32(tmp, info.serverID)
		body = append(body, tmp...)
		binary.BigEndian.PutUint32(tmp, info.mapType)
		body = append(body, tmp...)
		binary.BigEndian.PutUint32(tmp, info.mapID)
		body = append(body, tmp...)
	}

	ctx.GameServer.SendResponse(ctx.ClientData, 2157, ctx.UserID, ctx.SeqID, body)
	logger.Info(fmt.Sprintf("[2157] 查看在线状态: UID=%d Requested=%d Online=%d", ctx.UserID, requestCount, len(onlineUsers)))
}

// handleRequestOut CMD 2158 发送请求
// 响应: 空
func handleRequestOut(ctx *gameserver.HandlerContext) {
	// 响应：空（与 Lua 版本一致）
	ctx.GameServer.SendResponse(ctx.ClientData, 2158, ctx.UserID, ctx.SeqID, []byte{})
}

// handleRequestAnswer CMD 2159 请求回复
// 响应: 空
func handleRequestAnswer(ctx *gameserver.HandlerContext) {
	// 响应：空（与 Lua 版本一致）
	ctx.GameServer.SendResponse(ctx.ClientData, 2159, ctx.UserID, ctx.SeqID, []byte{})
}

// handleGetExchangeInfo CMD 70001 获取交换/荣誉信息（当前返回 0）
// 对齐 Lua: misc_handlers.handleGetExchangeInfo
func handleGetExchangeInfo(ctx *gameserver.HandlerContext) {
	body := make([]byte, 4)
	// honorValue=0
	ctx.GameServer.SendResponse(ctx.ClientData, 70001, ctx.UserID, ctx.SeqID, body)
}

// handleXinCheck CMD 50004 客户端信息上报（空回包）
func handleXinCheck(ctx *gameserver.HandlerContext) {
	ctx.GameServer.SendResponse(ctx.ClientData, 50004, ctx.UserID, ctx.SeqID, []byte{})
}

// handleXinGetQuadTime CMD 50008 获取四倍经验时间（0）
func handleXinGetQuadTime(ctx *gameserver.HandlerContext) {
	body := make([]byte, 4)
	ctx.GameServer.SendResponse(ctx.ClientData, 50008, ctx.UserID, ctx.SeqID, body)
}

// handleGoldOnlineCheckRemain CMD 1106 在线金豆余额（客户端先读 gold*100 再读 coins，共 8 字节）
func handleGoldOnlineCheckRemain(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	body := make([]byte, 8)
	binary.BigEndian.PutUint32(body[0:4], uint32(user.Gold*100)) // 客户端 /100 显示为金豆数
	binary.BigEndian.PutUint32(body[4:8], uint32(user.Coins))   // 赛尔豆，与 2001 等一致
	ctx.GameServer.SendResponse(ctx.ClientData, 1106, ctx.UserID, ctx.SeqID, body)
}

// handleMoneyCheckPsw CMD 1101 米币支付密码检查
// 客户端：若返回 1 则继续发送 MONEY_CHECK_REMAIN(1103)；否则提示设置支付密码
func handleMoneyCheckPsw(ctx *gameserver.HandlerContext) {
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body, 1) // 1 = 已设置/跳过，继续购买流程
	ctx.GameServer.SendResponse(ctx.ClientData, 1101, ctx.UserID, ctx.SeqID, body)
}

// handleMoneyCheckRemain CMD 1103 米币余额检查（客户端 /100 显示为米币数）
func handleMoneyCheckRemain(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	// 本地服无米币体系，用金豆*100 返回，使购买流程可走通
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body, uint32(user.Gold*100))
	ctx.GameServer.SendResponse(ctx.ClientData, 1103, ctx.UserID, ctx.SeqID, body)
}

// goldProductEntry 金豆商城商品（与 xml/225.xml GoldProduct 一致）
type goldProductEntry struct {
	Price  int
	ItemID int
}

// goldProductMap 金豆商品 productID -> 价格与物品ID（来自 225.xml）
var goldProductMap = map[uint32]goldProductEntry{
	240000: {50, 300006}, 240001: {5, 300024}, 240002: {10, 300025}, 240003: {5, 300026},
	240004: {5, 300027}, 240005: {30, 100266}, 240006: {30, 100267}, 240007: {30, 100268},
	240008: {10, 400054}, 240009: {10, 300028}, 240010: {5, 300029}, 240011: {2, 300030},
	240012: {2, 300031}, 240013: {3, 300032}, 240014: {3, 300033}, 240015: {4, 300034},
	240016: {10, 300035}, 240017: {10, 300036}, 240018: {15, 300037}, 240019: {15, 300038},
	240020: {15, 300039}, 240021: {15, 300040}, 240022: {15, 300041}, 240023: {15, 300042},
	240024: {50, 300043}, 240025: {5, 300044}, 240026: {80, 300009}, 240027: {4, 300045},
	240028: {4, 300046}, 240029: {6, 300047}, 240030: {6, 300048}, 240031: {8, 300049},
	240032: {16, 300050},
	// 兼容前端 PetShopXMLInfo（productID 为 5/6 位）：天赋改造药剂(300668) 价格10金豆
	240039: {10, 300668},
	240037: {50, 300053}, // 特性附体芯片（特性开启晶片）
	240038: {25, 300054}, // 特性重组剂
}

// handleGoldBuyProduct CMD 1104 金豆购买商品
// 请求: productID(4) + count(2 字节 short 大端)
// 响应: result(4) + payGold*100(4) + gold*100(4)，客户端 GoldBuyProductInfo 解析 payGold/gold 为 /100
func handleGoldBuyProduct(ctx *gameserver.HandlerContext) {
	if len(ctx.Body) < 6 {
		ctx.GameServer.SendResponse(ctx.ClientData, 1104, ctx.UserID, ctx.SeqID, []byte{})
		return
	}
	productID := binary.BigEndian.Uint32(ctx.Body[0:4])
	count := int(uint32(ctx.Body[4])<<8 | uint32(ctx.Body[5]))
	if count <= 0 {
		count = 1
	}
	entry, ok := goldProductMap[productID]
	if !ok {
		logger.Warning(fmt.Sprintf("[1104] 未知金豆商品 productID=%d", productID))
		ctx.GameServer.SendResponse(ctx.ClientData, 1104, ctx.UserID, ctx.SeqID, []byte{})
		return
	}
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	totalCost := entry.Price * count
	if user.Gold < totalCost {
		logger.Warning(fmt.Sprintf("[1104] 金豆不足: 需要%d 当前%d", totalCost, user.Gold))
		ctx.GameServer.SendResponse(ctx.ClientData, 1104, ctx.UserID, ctx.SeqID, []byte{})
		return
	}
	user.Gold -= totalCost
	itemKey := strconv.Itoa(entry.ItemID)
	if it, has := user.Items[itemKey]; has {
		it.Count += count
		user.Items[itemKey] = it
	} else {
		user.Items[itemKey] = userdb.Item{Count: count, ExpireTime: 0x057E40}
	}
	// 服装/套装（100000-199999）需同时加入 Clothes，否则“我的服装”中不显示
	addClothIfNeeded(user, entry.ItemID)
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}
	// 响应: result(4) + payGold*100(4) + gold*100(4)
	body := make([]byte, 12)
	binary.BigEndian.PutUint32(body[0:4], 0)
	binary.BigEndian.PutUint32(body[4:8], uint32(totalCost*100))
	binary.BigEndian.PutUint32(body[8:12], uint32(user.Gold*100))
	ctx.GameServer.SendResponse(ctx.ClientData, 1104, ctx.UserID, ctx.SeqID, body)
	logger.Info(fmt.Sprintf("[1104] 金豆购买: productID=%d itemID=%d count=%d 消耗金豆=%d 剩余=%d", productID, entry.ItemID, count, totalCost, user.Gold))
}

// buildNonoInfo90 构建 9003 的 90 字节 body（与 handleNonoInfo 一致），供 9003 与 9019 跟随后补发 9003 使用
func buildNonoInfo90(userID int64, user *userdb.GameData) []byte {
	n := user.Nono
	buf := make([]byte, 0, 90)
	writeU32 := func(v uint32) {
		tmp := make([]byte, 4)
		binary.BigEndian.PutUint32(tmp, v)
		buf = append(buf, tmp...)
	}
	writeU16 := func(v uint16) {
		tmp := make([]byte, 2)
		binary.BigEndian.PutUint16(tmp, v)
		buf = append(buf, tmp...)
	}
	writeFixedString := func(s string, size int) {
		b := []byte(s)
		if len(b) > size {
			b = b[:size]
		}
		buf = append(buf, b...)
		if len(b) < size {
			buf = append(buf, make([]byte, size-len(b))...)
		}
	}
	writeU32(uint32(userID))
	writeU32(uint32(n.Flag))
	writeU32(uint32(n.State))
	writeFixedString(n.Nick, 16)
	if n.SuperNono > 0 {
		writeU32(1)
	} else {
		writeU32(0)
	}
	writeU32(uint32(n.Color))
	writeU32(uint32(n.Power * 1000))
	writeU32(uint32(n.Mate * 1000))
	writeU32(uint32(n.IQ))
	writeU16(uint16(n.AI))
	if n.Birth > 0 {
		writeU32(uint32(n.Birth))
	} else {
		writeU32(uint32(time.Now().Unix()))
	}
	writeU32(uint32(n.ChargeTime))
	funcBits := make([]byte, 20)
	if n.SuperNono > 0 {
		for i := range funcBits {
			funcBits[i] = 0xFF
		}
	} else if len(n.Func) > 0 {
		copy(funcBits, n.Func)
		if len(n.Func) < 20 {
			copy(funcBits[len(n.Func):], make([]byte, 20-len(n.Func)))
		}
	}
	buf = append(buf, funcBits...)
	writeU32(uint32(n.SuperEnergy))
	writeU32(uint32(n.SuperLevel))
	writeU32(uint32(n.SuperStage))
	return buf
}

// handleNonoInfo CMD 9003 获取 NONO 信息
// 对齐 Lua: nono_handlers.handleNonoInfo（body 固定 90 字节）
func handleNonoInfo(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	// 根据超能等级自动更新形态（超能等级模型），保证客户端显示的超能 NoNo 形态与等级一致
	if user.Nono.SuperLevel > 0 {
		updateSuperNonoTypeByLevel(user)
		if ctx.GameServer.UserDB != nil {
			ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
		}
	}
	buf := buildNonoInfo90(ctx.UserID, user)
	ctx.GameServer.SendResponse(ctx.ClientData, 9003, ctx.UserID, ctx.SeqID, buf)
}

// calculateSuperNonoTypeByLevel 根据超能等级计算对应的形态（超能等级模型）
// 超能等级最高为12级，超能形态分别是1、4、7、9、12，共五个形态
// 等级1-3：形态1，等级4-6：形态2，等级7-8：形态3，等级9-11：形态4，等级12：形态5
func calculateSuperNonoTypeByLevel(level int) int {
	if level < 1 {
		return 0
	}
	if level >= 12 {
		return 5 // 形态5：等级12
	}
	if level >= 9 {
		return 4 // 形态4：等级9-11
	}
	if level >= 7 {
		return 3 // 形态3：等级7-8
	}
	if level >= 4 {
		return 2 // 形态2：等级4-6
	}
	return 1 // 形态1：等级1-3
}

// updateSuperNonoTypeByLevel 根据超能等级更新形态
func updateSuperNonoTypeByLevel(user *userdb.GameData) {
	calculatedType := calculateSuperNonoTypeByLevel(user.Nono.SuperLevel)
	user.Nono.SuperNono = calculatedType
	// 绑定前端显示的 VipLevel 与超能等级：保持 VipLevel = SuperLevel
	user.Nono.VipLevel = user.Nono.SuperLevel
}

// ==================== 任务 / 新手奖励 ====================

// 新手任务 ID 对齐 Lua NOVICE_TASK；其他任务 ID 见前端 MapProcess / TaskClass
const (
	taskGetCloth     = 85 // 领取服装
	taskSelectPet    = 86 // 选择精灵
	taskWinBattle    = 87 // 战斗胜利
	taskUseItem      = 88 // 使用道具
	taskBombDisposal = 9  // 赫尔卡星拆弹小游戏（MapProcess_30.as accept(9)），奖励电能锯子
)

// 新手三选一精灵映射，对齐 Lua NOVICE_PET_MAP / MapProcess_102.as
// 1 -> 布布种子 (1)
// 2 -> 小火猴   (7)
// 3 -> 伊优     (4)
var novicePetMap = map[uint32]int{
	1: 1,
	2: 7,
	3: 4,
}

// handleAcceptTask CMD 2201 接受任务
// 对齐 Lua: task_handlers.handleAcceptTask
func handleAcceptTask(ctx *gameserver.HandlerContext) {
	var taskID uint32
	if len(ctx.Body) >= 4 {
		taskID = binary.BigEndian.Uint32(ctx.Body[0:4])
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	if user.Tasks == nil {
		user.Tasks = make(map[string]userdb.Task)
	}
	key := strconv.FormatUint(uint64(taskID), 10)
	t := user.Tasks[key]
	t.Status = "1" // 1 = 已接受
	user.Tasks[key] = t

	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}

	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, taskID)
	ctx.GameServer.SendResponse(ctx.ClientData, 2201, ctx.UserID, ctx.SeqID, buf)
}

// handleGetTaskBuf CMD 2203 获取任务进度（GET_TASK_BUF），地图装置/NPC 对话依赖此接口
// 请求: taskId(4)。响应: taskId(4) + flag(4) + buf(20 字节)，与前端 TaskBufInfo(taskId, flag, buf) 及 buf.position=i; readBoolean() 一致
func handleGetTaskBuf(ctx *gameserver.HandlerContext) {
	var taskID uint32
	if len(ctx.Body) >= 4 {
		taskID = binary.BigEndian.Uint32(ctx.Body[0:4])
	}
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	flag := uint32(0)
	bufBytes := make([]byte, 20)
	if user.Tasks != nil {
		key := strconv.FormatUint(uint64(taskID), 10)
		if t, ok := user.Tasks[key]; ok && t.Buf != nil {
			for i := 0; i < 20; i++ {
				if v, ok := t.Buf[i]; ok {
					bufBytes[i] = byte(v & 0xff)
				}
			}
			flag = 1
		}
	}
	body := make([]byte, 4+4+20)
	binary.BigEndian.PutUint32(body[0:4], taskID)
	binary.BigEndian.PutUint32(body[4:8], flag)
	copy(body[8:28], bufBytes)
	ctx.GameServer.SendResponse(ctx.ClientData, 2203, ctx.UserID, ctx.SeqID, body)
	logger.Info(fmt.Sprintf("[2203] GET_TASK_BUF: UID=%d taskID=%d flag=%d buf0=%d buf1=%d buf2=%d buf3=%d",
		ctx.UserID, taskID, flag, bufBytes[0], bufBytes[1], bufBytes[2], bufBytes[3]))
}

// handleAcceptDailyTask CMD 2231 接受每日任务
// 请求: taskID(4)，响应: taskID(4)，与 2201 逻辑一致
func handleAcceptDailyTask(ctx *gameserver.HandlerContext) {
	var taskID uint32
	if len(ctx.Body) >= 4 {
		taskID = binary.BigEndian.Uint32(ctx.Body[0:4])
	}
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	if user.Tasks == nil {
		user.Tasks = make(map[string]userdb.Task)
	}
	key := strconv.FormatUint(uint64(taskID), 10)
	t := user.Tasks[key]
	t.Status = "1" // 已接受
	user.Tasks[key] = t
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, taskID)
	ctx.GameServer.SendResponse(ctx.ClientData, 2231, ctx.UserID, ctx.SeqID, buf)
}

// handleDeleteDailyTask CMD 2232 放弃每日任务
// 请求: taskID(4)，响应: taskID(4)
func handleDeleteDailyTask(ctx *gameserver.HandlerContext) {
	var taskID uint32
	if len(ctx.Body) >= 4 {
		taskID = binary.BigEndian.Uint32(ctx.Body[0:4])
	}
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	if user.Tasks != nil {
		key := strconv.FormatUint(uint64(taskID), 10)
		delete(user.Tasks, key)
		if ctx.GameServer.UserDB != nil {
			ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
		}
	}
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, taskID)
	ctx.GameServer.SendResponse(ctx.ClientData, 2232, ctx.UserID, ctx.SeqID, buf)
}

// handleCompleteDailyTask CMD 2233 完成每日任务
// 请求: taskID(4) + param(4)，响应: NoviceFinishInfo（与 2202 相同），客户端据此 setTaskStatus(COMPLETE)
func handleCompleteDailyTask(ctx *gameserver.HandlerContext) {
	var taskID, param uint32
	if len(ctx.Body) >= 4 {
		taskID = binary.BigEndian.Uint32(ctx.Body[0:4])
	}
	if len(ctx.Body) >= 8 {
		param = binary.BigEndian.Uint32(ctx.Body[4:8])
	}
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	if user.Tasks == nil {
		user.Tasks = make(map[string]userdb.Task)
	}
	key := strconv.FormatUint(uint64(taskID), 10)
	t := user.Tasks[key]
	t.Status = "3" // 已完成
	user.Tasks[key] = t
	body := buildTaskCompleteResponse(taskID, param, user)
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}
	ctx.GameServer.SendResponse(ctx.ClientData, 2233, ctx.UserID, ctx.SeqID, body)
}

// handleCompleteTask CMD 2202 完成任务（包含新手奖励）
// 对齐 Lua: task_handlers.handleCompleteTask / buildTaskCompleteResponse
func handleCompleteTask(ctx *gameserver.HandlerContext) {
	var taskID, param uint32
	if len(ctx.Body) >= 4 {
		taskID = binary.BigEndian.Uint32(ctx.Body[0:4])
	}
	if len(ctx.Body) >= 8 {
		param = binary.BigEndian.Uint32(ctx.Body[4:8])
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	if user.Tasks == nil {
		user.Tasks = make(map[string]userdb.Task)
	}
	key := strconv.FormatUint(uint64(taskID), 10)
	t := user.Tasks[key]
	// 雷伊技能特训/雷伊特训(122)：前端 complete(122, proIndex) 用 param=0..3 表示完成某一步，
	// MapProcess_40 依赖 getProStatusList(122) 的 buf[0]/buf[1]/buf[2] 来显示阿克希亚剧情入口。
	// 因此这里需要把 param 写入 Buf；且在 param<3 时不要把整个任务置为 completed，否则后续流程会被跳过。
	if taskID == 122 && param < 20 {
		if t.Buf == nil {
			t.Buf = make(map[int]int)
		}
		t.Buf[int(param)] = 1
		if param >= 3 {
			t.Status = "3" // 最后一步完成才置完成
		} else {
			t.Status = "1" // 保持已接受
		}
	} else {
		t.Status = "3" // 默认：视为任务完成
	}
	user.Tasks[key] = t

	// 构建任务完成响应（包含新手三选一精灵和物品奖励等）
	body := buildTaskCompleteResponse(taskID, param, user)

	// 保存数据（任务完成可能添加了精灵或物品）
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}

	ctx.GameServer.SendResponse(ctx.ClientData, 2202, ctx.UserID, ctx.SeqID, body)
}

// buildTaskCompleteResponse 构建任务完成响应
// NoviceFinishInfo: taskID(4) + petID(4) + captureTm(4) + itemCount(4) + [itemID(4) + itemCnt(4)]...
// 这里只精确实现新手三选一精灵，其他任务返回空奖励但保持结构正确。
func buildTaskCompleteResponse(taskID, param uint32, user *userdb.GameData) []byte {
	writeU32To := func(buf []byte, off int, v uint32) {
		binary.BigEndian.PutUint32(buf[off:off+4], v)
	}

	var petID uint32
	var captureTm uint32

	// 处理新手选宠任务
	if taskID == taskSelectPet {
		if mapped, ok := novicePetMap[param]; ok {
			petID = uint32(mapped)
		} else {
			petID = 1
		}
		captureTm = 0x69686700 + petID

		// 添加到 pets 列表（如果还没有该精灵）
		found := false
		for _, p := range user.Pets {
			if p.ID == int(petID) && p.CatchTime == int(captureTm) {
				found = true
				break
			}
		}
		if !found {
			// 随机性格（0-24）
			rand.Seed(time.Now().UnixNano())
			randomNature := rand.Intn(25)
			// 随机个体值（15-31，新手精灵稍微好一点）
			randomDV := 15 + rand.Intn(17) // 15-31

			newPet := userdb.Pet{
				ID:        int(petID),
				CatchTime: int(captureTm),
				Level:     5,
				DV:        randomDV,
				Nature:    randomNature,
				Exp:       0,
				Name:      "",
				Shiny:     0, // 默认新手初始精灵为普通色
			}
			// 新手选宠不自动分配特性；后续可通过“特性开启芯片”等道具获得特性
			user.Pets = append(user.Pets, newPet)
			logger.Info(fmt.Sprintf("[2202] 新手选宠: PetID=%d DV=%d Nature=%d", petID, randomDV, randomNature))
		}
	}

	// 根据任务ID添加物品奖励
	itemCount := uint32(0)
	itemRewards := []struct {
		id    uint32
		count uint32
	}{}

	// 任务85（领取服装）给新手套装
	if taskID == 85 {
		noviceClothes := []struct {
			id    uint32
			count uint32
		}{
			{100027, 1}, // 新手服装1
			{100028, 1}, // 新手服装2
			{500001, 1}, // 新手家具
			{300650, 3}, // 新手道具1
			{300025, 3}, // 新手道具2
			{300035, 3}, // 新手道具3
			{500502, 1}, // 新手家具2
			{500503, 1}, // 新手家具3
		}

		for _, item := range noviceClothes {
			itemKey := strconv.FormatUint(uint64(item.id), 10)
			if existing, ok := user.Items[itemKey]; ok {
				existing.Count += int(item.count)
				user.Items[itemKey] = existing
			} else {
				user.Items[itemKey] = userdb.Item{Count: int(item.count), ExpireTime: 0x057E40}
			}
			itemRewards = append(itemRewards, item)
			itemCount++
			logger.Info(fmt.Sprintf("[2202] 任务完成奖励: 物品 %d x%d", item.id, item.count))
		}
	}

	// 任务87（战斗胜利）给一些物品奖励
	if taskID == taskWinBattle {
		// 添加治疗药水
		itemKey := "300001"
		if existing, ok := user.Items[itemKey]; ok {
			existing.Count += 5
			user.Items[itemKey] = existing
		} else {
			user.Items[itemKey] = userdb.Item{Count: 5, ExpireTime: 0x057E40}
		}
		itemRewards = append(itemRewards, struct {
			id    uint32
			count uint32
		}{300001, 5})
		itemCount++

		itemKey2 := "300011"
		if existing, ok := user.Items[itemKey2]; ok {
			existing.Count += 3
			user.Items[itemKey2] = existing
		} else {
			user.Items[itemKey2] = userdb.Item{Count: 3, ExpireTime: 0x057E40}
		}
		itemRewards = append(itemRewards, struct {
			id    uint32
			count uint32
		}{300011, 3})
		itemCount++
		logger.Info(fmt.Sprintf("[2202] 任务完成奖励: 物品 300001 x5, 300011 x3"))
	}

	// 任务88（使用道具）给金币奖励
	if taskID == taskUseItem {
		// 添加金币（特殊奖励类型1）
		itemRewards = append(itemRewards, struct {
			id    uint32
			count uint32
		}{1, 50000}) // 类型1=金币
		itemCount++

		// 添加经验（特殊奖励类型3）
		itemRewards = append(itemRewards, struct {
			id    uint32
			count uint32
		}{3, 250000}) // 类型3=经验
		itemCount++

		// 添加其他奖励（特殊奖励类型5）
		itemRewards = append(itemRewards, struct {
			id    uint32
			count uint32
		}{5, 20})
		itemCount++

		// 更新用户金币
		user.Coins += 50000
		logger.Info(fmt.Sprintf("[2202] 任务完成奖励: 金币 +50000, 经验 +250000, 其他 +20"))
	}

	// 任务 9：赫尔卡星拆弹小游戏完成，奖励电能锯子（100059）
	if taskID == taskBombDisposal {
		itemID := uint32(100059) // 电能锯子
		itemKey := strconv.FormatUint(uint64(itemID), 10)
		if existing, ok := user.Items[itemKey]; ok {
			existing.Count++
			user.Items[itemKey] = existing
		} else {
			user.Items[itemKey] = userdb.Item{Count: 1, ExpireTime: 0x057E40}
		}
		itemRewards = append(itemRewards, struct {
			id    uint32
			count uint32
		}{itemID, 1})
		itemCount++
		logger.Info(fmt.Sprintf("[2202] 任务完成奖励: 拆弹小游戏 电能锯子(100059) x1"))
	}

	// 无配置奖励时（如每日任务）给默认 2000 点积累经验，客户端 TaskClass 显示“你获得了 2000 点积累经验！”
	if itemCount == 0 && petID == 0 {
		defaultExp := uint32(2000)
		if user.ExpPool < 0 {
			user.ExpPool = 0
		}
		user.ExpPool += int(defaultExp)
		itemRewards = append(itemRewards, struct {
			id    uint32
			count uint32
		}{3, defaultExp}) // 类型 3 = 积累经验
		itemCount++
		logger.Info(fmt.Sprintf("[2202/2233] 任务完成默认奖励: 积累经验 +%d", defaultExp))
	}

	// 构建响应体
	bodySize := 16 + int(itemCount)*8 // taskID(4) + petID(4) + captureTm(4) + itemCount(4) + [itemID(4) + itemCnt(4)]...
	body := make([]byte, bodySize)
	writeU32To(body, 0, taskID)
	writeU32To(body, 4, petID)
	writeU32To(body, 8, captureTm)
	writeU32To(body, 12, itemCount)

	off := 16
	for _, item := range itemRewards {
		writeU32To(body, off, item.id)
		off += 4
		writeU32To(body, off, item.count)
		off += 4
	}

	return body
}

// handleGetSoulBeadList CMD 2354 获取魂珠列表
// 响应格式对齐客户端 HatchTaskMapManager/HatchTaskManager：
//   count(4) + [obtainTime(4) + itemID(4)] * count
// 其中 obtainTime 作为魂珠唯一标识，后续 2352/2353 通过它读取/写入 20 步进度。
func handleGetSoulBeadList(ctx *gameserver.HandlerContext) {
	db := ctx.GameServer.UserDB
	if db == nil {
		body := make([]byte, 4)
		ctx.GameServer.SendResponse(ctx.ClientData, 2354, ctx.UserID, ctx.SeqID, body)
		return
	}

	data := db.GetOrCreateGameData(ctx.UserID)
	count := len(data.SoulBeads)

	body := make([]byte, 4+count*8)
	binary.BigEndian.PutUint32(body[0:4], uint32(count))

	for i, bead := range data.SoulBeads {
		offset := 4 + i*8
		binary.BigEndian.PutUint32(body[offset:offset+4], bead.ObtainTime)
		binary.BigEndian.PutUint32(body[offset+4:offset+8], uint32(bead.ItemID))
	}

	ctx.GameServer.SendResponse(ctx.ClientData, 2354, ctx.UserID, ctx.SeqID, body)
}

// handleGetSoulBeadBuf CMD 2352 获取某颗魂珠的吸能进度
// 请求: obtainTime(4)
// 响应: obtainTime(4) + 20 * readBoolean()（每个步骤一个字节，非 0 即 true）
func handleGetSoulBeadBuf(ctx *gameserver.HandlerContext) {
	if len(ctx.Body) < 4 {
		// 包体异常，返回空进度（obtainTime=0，全 false），避免客户端报错
		body := make([]byte, 4+20)
		ctx.GameServer.SendResponse(ctx.ClientData, 2352, ctx.UserID, ctx.SeqID, body)
		return
	}

	obtainTime := binary.BigEndian.Uint32(ctx.Body[0:4])

	db := ctx.GameServer.UserDB
	if db == nil {
		body := make([]byte, 4+20)
		binary.BigEndian.PutUint32(body[0:4], obtainTime)
		ctx.GameServer.SendResponse(ctx.ClientData, 2352, ctx.UserID, ctx.SeqID, body)
		return
	}

	data := db.GetOrCreateGameData(ctx.UserID)

	// 查找对应魂珠
	var status []bool
	for _, bead := range data.SoulBeads {
		if bead.ObtainTime == obtainTime {
			status = bead.Status
			break
		}
	}

	body := make([]byte, 4+20)
	binary.BigEndian.PutUint32(body[0:4], obtainTime)

	// 将最多 20 个布尔值编码成 ByteArray.readBoolean() 兼容格式（一个 bool = 一个字节）
	for i := 0; i < 20; i++ {
		if status != nil && i < len(status) && status[i] {
			body[4+i] = 1
		} else {
			body[4+i] = 0
		}
	}

	ctx.GameServer.SendResponse(ctx.ClientData, 2352, ctx.UserID, ctx.SeqID, body)
}

// handleSetSoulBeadBuf CMD 2353 写入某颗魂珠的吸能进度
// 请求: obtainTime(4) + buf(<=20 字节，客户端通常固定 20 个 writeBoolean)
// 响应: 4 字节 0（客户端只关心成功回包）
func handleSetSoulBeadBuf(ctx *gameserver.HandlerContext) {
	if len(ctx.Body) < 4 {
		body := make([]byte, 4)
		ctx.GameServer.SendResponse(ctx.ClientData, 2353, ctx.UserID, ctx.SeqID, body)
		return
	}

	obtainTime := binary.BigEndian.Uint32(ctx.Body[0:4])
	raw := ctx.Body[4:]

	// 解析最多 20 个步骤的布尔值
	status := make([]bool, 20)
	for i := 0; i < 20 && i < len(raw); i++ {
		status[i] = raw[i] != 0
	}

	db := ctx.GameServer.UserDB
	if db != nil {
		data := db.GetOrCreateGameData(ctx.UserID)

		// 写回到对应魂珠；若不存在则忽略（未来融合/任务创建魂珠时会补齐）
		found := false
		for i, bead := range data.SoulBeads {
			if bead.ObtainTime == obtainTime {
				data.SoulBeads[i].Status = status
				found = true
				break
			}
		}
		if found {
			db.SaveGameData(ctx.UserID, data)
		}
	}

	// 返回 4 字节 0，对齐原 Lua 行为
	body := make([]byte, 4)
	ctx.GameServer.SendResponse(ctx.ClientData, 2353, ctx.UserID, ctx.SeqID, body)
}

// handleGetSoulBeadStatus CMD 2356 GET_SOULBEAD_STATUS：查询某颗元神珠赋形剩余时间
// 对齐 SoulTransformPanel.getTransformData()：客户端连续读 3 个 readUnsignedInt()
//   - obTainTm = 第 1 个 4 字节；_soulBeadItemID = 第 2 个 4 字节；breedTm = 第 3 个 4 字节（剩余秒数）
//   - 界面用 _breedTimeTxt.text = Math.ceil(breedTm/3600) 显示「剩余 X 小时」
// 响应：obtainTime(4) + soulBeadItemID(4) + remainSec(4)，共 12 字节；无赋形中则回 12 字节 0。
func handleGetSoulBeadStatus(ctx *gameserver.HandlerContext) {
	body := make([]byte, 12)

	db := ctx.GameServer.UserDB
	if db == nil {
		ctx.GameServer.SendResponse(ctx.ClientData, 2356, ctx.UserID, ctx.SeqID, body)
		return
	}

	data := db.GetOrCreateGameData(ctx.UserID)

	var bead *userdb.SoulBead
	if len(ctx.Body) >= 4 {
		reqObtain := binary.BigEndian.Uint32(ctx.Body[0:4])
		if reqObtain != 0 {
			for i := range data.SoulBeads {
				if data.SoulBeads[i].ObtainTime == reqObtain {
					bead = &data.SoulBeads[i]
					break
				}
			}
		}
	}
	if bead == nil {
		for i := range data.SoulBeads {
			if data.SoulBeads[i].TransformStartTime > 0 {
				bead = &data.SoulBeads[i]
				break
			}
		}
	}
	if bead == nil || bead.TransformStartTime == 0 {
		ctx.GameServer.SendResponse(ctx.ClientData, 2356, ctx.UserID, ctx.SeqID, body)
		return
	}

	now := time.Now().Unix()
	elapsed := now - bead.TransformStartTime
	remainingSec := soulBeadTransformSec - elapsed
	if remainingSec < 0 {
		remainingSec = 0
	}

	binary.BigEndian.PutUint32(body[0:4], bead.ObtainTime)
	binary.BigEndian.PutUint32(body[4:8], uint32(bead.ItemID))
	binary.BigEndian.PutUint32(body[8:12], uint32(remainingSec))
	ctx.GameServer.SendResponse(ctx.ClientData, 2356, ctx.UserID, ctx.SeqID, body)
}

// handleTransformSoulBead CMD 2357 TRANSFORM_SOULBEAD：开始元神赋形（计时）
// 约定格式：
//   - 请求：obtainTime(4)
//   - 响应：4 字节 0（保持与 Lua stub 一致）
// 逻辑：
//   - 只有在有至少一格吸能进度时才允许开始赋形
//   - 记录 TransformStartTime，供 2356 查询与 2358 校验
func handleTransformSoulBead(ctx *gameserver.HandlerContext) {
	body := make([]byte, 4)

	if ctx.GameServer.UserDB == nil {
		ctx.GameServer.SendResponse(ctx.ClientData, 2357, ctx.UserID, ctx.SeqID, body)
		return
	}

	db := ctx.GameServer.UserDB
	data := db.GetOrCreateGameData(ctx.UserID)

	// 优先按请求中的 obtainTime 精确匹配（如果客户端传了非 0）
	var targetIndex = -1
	if len(ctx.Body) >= 4 {
		obtainTime := binary.BigEndian.Uint32(ctx.Body[0:4])
		if obtainTime != 0 {
			for i := range data.SoulBeads {
				if data.SoulBeads[i].ObtainTime == obtainTime {
					targetIndex = i
					break
				}
			}
		}
	}
	// 如果客户端传 0（当前怀旧前端的情况），则选择第一颗“已有进度但尚未赋形”的元神珠
	if targetIndex == -1 {
		for i := range data.SoulBeads {
			if data.SoulBeads[i].TransformStartTime != 0 {
				continue
			}
			hasProgress := false
			for _, s := range data.SoulBeads[i].Status {
				if s {
					hasProgress = true
					break
				}
			}
			if hasProgress {
				targetIndex = i
				break
			}
		}
	}
	if targetIndex != -1 {
		data.SoulBeads[targetIndex].TransformStartTime = time.Now().Unix()
		db.SaveGameData(ctx.UserID, data)
	}
	// 成功/失败均回 4 字节（0=成功），客户端通过登录包中的 obtainTm/soulBeadItemID 得知正在赋形的珠子
	ctx.GameServer.SendResponse(ctx.ClientData, 2357, ctx.UserID, ctx.SeqID, body)
}

// ==================== 融合 / PET_FUSION ====================

// 元神珠赋形等待时长（秒）。默认 3600（1 小时）便于界面「X 小时」能显示；可用 GM 面板调整。
var soulBeadTransformSec int64 = 3600

// fusionFormula 预置的融合公式：主精灵 ID + 副精灵 ID -> (元神珠道具 ID, 融合精灵 ID)
// 仅覆盖怀旧服常见的 8+1 组，其他组合一律视为“米色珠”随机兔子。
type fusionFormula struct {
	MainID     int
	SubID      int
	BeadItemID int
	ResultID   int
}

var fusionFormulas = []fusionFormula{
	// 布布花(3) + 小豆芽(27) = 丽莎布布(303) -> 绿色元神珠(1000001)
	{MainID: 3, SubID: 27, BeadItemID: 1000001, ResultID: 303},
	// 巴鲁斯(6) + 小鳍鱼(198) = 鲁斯王(306) -> 蓝色元神珠(1000002)
	{MainID: 6, SubID: 198, BeadItemID: 1000002, ResultID: 306},
	// 烈焰猩猩(9) + 吉尔(35) = 魔焰猩猩(309) -> 红色元神珠(1000003)
	{MainID: 9, SubID: 35, BeadItemID: 1000003, ResultID: 309},
	// 灵翼蜂(155) + 幽浮(25) = 哈尔翼蜂(321) -> 橙色元神珠(1000006)
	{MainID: 155, SubID: 25, BeadItemID: 1000006, ResultID: 321},
	// 卡斯达克(180) + 莫比(53) = 卡鲁克斯(318) -> 咖啡色元神珠(1000004)
	{MainID: 180, SubID: 53, BeadItemID: 1000004, ResultID: 318},
	// 多鲁姆(210) + 火炎贝(38) = 多鲁鲁斯(334) -> 红色元神珠(1000003)
	{MainID: 210, SubID: 38, BeadItemID: 1000003, ResultID: 334},
	// 克洛亚(118) + 林奇(190) = 古雷亚(324) -> 天青色元神珠(1000005)
	{MainID: 118, SubID: 190, BeadItemID: 1000005, ResultID: 324},
	// 艾斯菲亚(79) + 闪光皮皮(164) = 闪光艾菲亚(312) -> 紫色元神珠(1000008)
	{MainID: 79, SubID: 164, BeadItemID: 1000008, ResultID: 312},
	// 艾斯菲亚(79) + 闪光皮皮(164) = 卡鲁耶克(315) -> 粉色元神珠(1000009)
	{MainID: 79, SubID: 164, BeadItemID: 1000009, ResultID: 315},
}

// wildBeigeBeadID 米色元神珠
const wildBeigeBeadID = 1000007

// wildBeigePets 米色珠三兔：卡门兔(325)、辛克(328)、伊达(330)
var wildBeigePets = []int{325, 328, 330}

// matchFusionFormula 根据主/副精灵 ID 查找是否命中预设融合公式
func matchFusionFormula(mainID, subID int) (beadItemID, resultPetID int, ok bool) {
	for _, f := range fusionFormulas {
		if (f.MainID == mainID && f.SubID == subID) || (f.MainID == subID && f.SubID == mainID) {
			return f.BeadItemID, f.ResultID, true
		}
	}
	return 0, 0, false
}

// calcFusionDV 按文档公式计算融合精灵个体值：
// DV_fusion = floor((2*DV_main + DV_sub)/3) + 2，并裁剪到 [1,31]
func calcFusionDV(dvMain, dvSub int) int {
	v := (2*dvMain + dvSub) / 3
	v += 2
	if v < 1 {
		v = 1
	}
	if v > 31 {
		v = 31
	}
	return v
}

// chooseFusionNature 性格继承规则：“主/副性格相同且非 0 则继承，否则随机”
// 由于目前没有性格枚举映射，这里简单按 1..25 随机。
func chooseFusionNature(natureMain, natureSub int) int {
	if natureMain != 0 && natureMain == natureSub {
		return natureMain
	}
	// 随机一个 1..25 的性格 ID
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	return 1 + r.Intn(25)
}

// handlePetFusion CMD 2351 PET_FUSION：执行融合，生成元神珠记录并返回 PetFusionInfo
// 目前请求体采用简化格式：mainCatchTime(4) + subCatchTime(4)，多余字节忽略。
// 响应：obtainTime(4) + soulID(4) + starterCpTm(4) + costItemFlag(4)
func handlePetFusion(ctx *gameserver.HandlerContext) {
	body := make([]byte, 16)

	// 兜底：包体长度不足时直接返回全 0，避免客户端报错
	if len(ctx.Body) < 8 || ctx.GameServer.UserDB == nil {
		ctx.GameServer.SendResponse(ctx.ClientData, 2351, ctx.UserID, ctx.SeqID, body)
		return
	}

	mainCatchTime := int(binary.BigEndian.Uint32(ctx.Body[0:4]))
	subCatchTime := int(binary.BigEndian.Uint32(ctx.Body[4:8]))

	db := ctx.GameServer.UserDB
	mainPet := db.GetPetByCatchTime(ctx.UserID, mainCatchTime)
	subPet := db.GetPetByCatchTime(ctx.UserID, subCatchTime)

	// 兼容老前端：部分客户端可能发送的是精灵 ID 而不是 catchTime
	// 若按 catchTime 未找到，则退化为按 ID 匹配一只精灵，并将 catchTime 修正为真实值，方便后续删除背包数据。
	if mainPet == nil || subPet == nil {
		data := db.GetOrCreateGameData(ctx.UserID)
		if mainPet == nil {
			for i := range data.Pets {
				if data.Pets[i].ID == mainCatchTime {
					mainPet = &data.Pets[i]
					mainCatchTime = data.Pets[i].CatchTime
					break
				}
			}
		}
		if subPet == nil {
			for i := range data.Pets {
				if data.Pets[i].ID == subCatchTime {
					subPet = &data.Pets[i]
					subCatchTime = data.Pets[i].CatchTime
					break
				}
			}
		}
	}
	if mainPet == nil || subPet == nil {
		ctx.GameServer.SendResponse(ctx.ClientData, 2351, ctx.UserID, ctx.SeqID, body)
		return
	}

	// 解析 4 个材料道具 ID（例如黄金矿），体现在第 3~6 个 uint32 中
	matIDs := []int{}
	if len(ctx.Body) >= 24 {
		for i := 0; i < 4; i++ {
			start := 8 + 4*i
			if len(ctx.Body) >= start+4 {
				id := int(binary.BigEndian.Uint32(ctx.Body[start : start+4]))
				if id != 0 {
					matIDs = append(matIDs, id)
				}
			}
		}
	}

	// 无论成功或失败，都会尝试扣除 4 个材料（若对应道具数量不足则跳过，不报错）
	for _, itemID := range matIDs {
		if db.GetItemCount(ctx.UserID, itemID) > 0 {
			db.RemoveItem(ctx.UserID, itemID, 1)
		}
	}

	// 成败判定：目前设为 50% 成功率
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	success := r.Intn(100) < 50

	// 失败：降低副精灵等级 5 级（最低 1 级），不生成元神珠、不删除主/副精灵，返回 obtainTime=0
	if !success {
		data := db.GetOrCreateGameData(ctx.UserID)
		for i := range data.Pets {
			if data.Pets[i].CatchTime == subCatchTime {
				data.Pets[i].Level -= 5
				if data.Pets[i].Level < 1 {
					data.Pets[i].Level = 1
				}
				db.SaveGameData(ctx.UserID, data)
				break
			}
		}

		binary.BigEndian.PutUint32(body[0:4], 0)
		binary.BigEndian.PutUint32(body[4:8], 0)
		binary.BigEndian.PutUint32(body[8:12], 0)
		binary.BigEndian.PutUint32(body[12:16], 0)
		ctx.GameServer.SendResponse(ctx.ClientData, 2351, ctx.UserID, ctx.SeqID, body)
		return
	}

	// 根据配方决定元神珠颜色 / 产物；否则视为“米色珠随机兔子”
	beadItemID, resultPetID, ok := matchFusionFormula(mainPet.ID, subPet.ID)
	if !ok {
		beadItemID = wildBeigeBeadID
		// 任意两只野生精灵：随机三兔之一
		r := rand.New(rand.NewSource(time.Now().UnixNano()))
		resultPetID = wildBeigePets[r.Intn(len(wildBeigePets))]
	}
	// 赋形发放小形态（进化链第一个），与分子转化仪一致
	if chain := gamepets.GetInstance().GetEvolutionChain(resultPetID); len(chain) > 0 {
		resultPetID = chain[0]
	}

	// 个体值 & 性格
	dv := calcFusionDV(mainPet.DV, subPet.DV)
	nature := chooseFusionNature(mainPet.Nature, subPet.Nature)

	// 异色规则：
	// - 若主精灵与副精灵均为异色，则结果必定为异色；
	// - 若仅一只是异色，则结果有 50% 概率为异色；
	// - 若两只均为普通，则结果绝不会是异色。
	shiny := 0
	if mainPet.Shiny != 0 && subPet.Shiny != 0 {
		shiny = 1
	} else if (mainPet.Shiny != 0 && subPet.Shiny == 0) || (mainPet.Shiny == 0 && subPet.Shiny != 0) {
		r := rand.New(rand.NewSource(time.Now().UnixNano()))
		if r.Intn(2) == 0 {
			shiny = 1
		}
	}

	// 预生成特性：沿用 AssignFusionTraitIfNeeded 的规则，后续可按材料公式细化
	tmpPet := &userdb.Pet{
		ID:        resultPetID,
		CatchTime: int(time.Now().Unix()),
		DV:        dv,
		Nature:    nature,
		Level:     1,
		Shiny:     shiny,
	}
	userdb.AssignFusionTraitIfNeeded(tmpPet)

	// 生成元神珠记录
	data := db.GetOrCreateGameData(ctx.UserID)
	obtainTime := uint32(time.Now().Unix())
	newBead := userdb.SoulBead{
		ObtainTime:  obtainTime,
		ItemID:      beadItemID,
		Status:      nil,
		ResultPetID: resultPetID,
		DV:          dv,
		Nature:      nature,
		Trait:       tmpPet.Trait,
		Shiny:       shiny,
	}
	data.SoulBeads = append(data.SoulBeads, newBead)
	db.SaveGameData(ctx.UserID, data)

	// 融合成功后消耗主精灵与副精灵（从背包移除），避免重登后两只精灵仍存在
	db.RemovePet(ctx.UserID, mainCatchTime)
	db.RemovePet(ctx.UserID, subCatchTime)

	// 给玩家背包发放 1 个对应颜色的元神珠道具，方便前端显示
	db.AddItem(ctx.UserID, beadItemID, 1)

	// 构造 PetFusionInfo：obtainTime + soulID(物品 ID) + starterCpTm(先用主精灵 catchTime) + costItemFlag(暂 0)
	binary.BigEndian.PutUint32(body[0:4], obtainTime)
	binary.BigEndian.PutUint32(body[4:8], uint32(beadItemID))
	binary.BigEndian.PutUint32(body[8:12], uint32(mainCatchTime))
	binary.BigEndian.PutUint32(body[12:16], 0)

	ctx.GameServer.SendResponse(ctx.ClientData, 2351, ctx.UserID, ctx.SeqID, body)
}

// handleXinFusion CMD 50006 XIN_FUSION：精灵融合转生仓（使用 4 个材料，有几率失败）
// 协议对齐 SpriteFusionPanel2.onSpriteFusion：
//   - 请求：mainCatchTime(4) + subCatchTime(4) + mat1ItemId(4) + mat2ItemId(4) + mat3ItemId(4) + mat4ItemId(4) + reserved1(4) + reserved2(4)
//   - 响应体格式沿用 PetFusionInfo：obtainTime(4) + soulID(4) + starterCpTm(4) + costItemFlag(4)
//     * obtainTime=0 表示融合失败；>0 表示成功（用于元神赋形）
//     * costItemFlag 用于前端提示某些特殊道具被消耗，这里暂不区分，保持 0。
// 逻辑：
//   - 读取主/副精灵（支持按 catchTime 或按精灵 ID 兼容老客户端）
//   - 计算一次随机结果：这里采用 50% 成功率
//   - 无论成败，尽量扣除 4 个材料道具（若背包数量不足则跳过，不报错）
//   - 成功：沿用 handlePetFusion 的逻辑生成元神珠记录、移除主/副精灵、发放元神珠道具，返回 obtainTime>0
//   - 失败：不生成元神珠，不删除主精灵，仅将副精灵等级降低 5 级（最低 1 级），返回 obtainTime=0
func handleXinFusion(ctx *gameserver.HandlerContext) {
	body := make([]byte, 16)

	if ctx.GameServer.UserDB == nil || len(ctx.Body) < 8 {
		ctx.GameServer.SendResponse(ctx.ClientData, 50006, ctx.UserID, ctx.SeqID, body)
		return
	}

	mainCatchTime := int(binary.BigEndian.Uint32(ctx.Body[0:4]))
	subCatchTime := int(binary.BigEndian.Uint32(ctx.Body[4:8]))

	db := ctx.GameServer.UserDB
	mainPet := db.GetPetByCatchTime(ctx.UserID, mainCatchTime)
	subPet := db.GetPetByCatchTime(ctx.UserID, subCatchTime)

	// 兼容：若按 catchTime 未找到，则尝试按精灵 ID 匹配
	if mainPet == nil || subPet == nil {
		data := db.GetOrCreateGameData(ctx.UserID)
		if mainPet == nil {
			for i := range data.Pets {
				if data.Pets[i].ID == mainCatchTime {
					mainPet = &data.Pets[i]
					mainCatchTime = data.Pets[i].CatchTime
					break
				}
			}
		}
		if subPet == nil {
			for i := range data.Pets {
				if data.Pets[i].ID == subCatchTime {
					subPet = &data.Pets[i]
					subCatchTime = data.Pets[i].CatchTime
					break
				}
			}
		}
	}
	if mainPet == nil || subPet == nil {
		ctx.GameServer.SendResponse(ctx.ClientData, 50006, ctx.UserID, ctx.SeqID, body)
		return
	}

	// 解析 4 个材料道具 ID
	matIDs := []int{}
	if len(ctx.Body) >= 24 {
		for i := 0; i < 4; i++ {
			start := 8 + 4*i
			if len(ctx.Body) >= start+4 {
				id := int(binary.BigEndian.Uint32(ctx.Body[start : start+4]))
				if id != 0 {
					matIDs = append(matIDs, id)
				}
			}
		}
	}

	// 成败判定：50% 成功
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	success := r.Intn(100) < 50

	// 无论成败，尽量扣除 4 个材料道具（若背包中不足则跳过）
	for _, itemID := range matIDs {
		if db.GetItemCount(ctx.UserID, itemID) > 0 {
			db.RemoveItem(ctx.UserID, itemID, 1)
		}
	}

	// 失败：降低副精灵等级 5 级（最低 1 级），不生成元神珠
	if !success {
		data := db.GetOrCreateGameData(ctx.UserID)
		for i := range data.Pets {
			if data.Pets[i].CatchTime == subCatchTime {
				data.Pets[i].Level -= 5
				if data.Pets[i].Level < 1 {
					data.Pets[i].Level = 1
				}
				db.SaveGameData(ctx.UserID, data)
				break
			}
		}

		// obtainTime=0 表示失败
		binary.BigEndian.PutUint32(body[0:4], 0)
		binary.BigEndian.PutUint32(body[4:8], 0)
		binary.BigEndian.PutUint32(body[8:12], 0)
		binary.BigEndian.PutUint32(body[12:16], 0)
		ctx.GameServer.SendResponse(ctx.ClientData, 50006, ctx.UserID, ctx.SeqID, body)
		return
	}

	// 以下为成功逻辑：基本沿用 handlePetFusion

	// 根据配方决定元神珠颜色 / 产物；否则视为“米色珠随机兔子”
	beadItemID, resultPetID, ok := matchFusionFormula(mainPet.ID, subPet.ID)
	if !ok {
		beadItemID = wildBeigeBeadID
		r2 := rand.New(rand.NewSource(time.Now().UnixNano()))
		resultPetID = wildBeigePets[r2.Intn(len(wildBeigePets))]
	}
	// 赋形发放小形态（进化链第一个）
	if chain := gamepets.GetInstance().GetEvolutionChain(resultPetID); len(chain) > 0 {
		resultPetID = chain[0]
	}

	// 个体值 & 性格
	dv := calcFusionDV(mainPet.DV, subPet.DV)
	nature := chooseFusionNature(mainPet.Nature, subPet.Nature)

	// 异色规则沿用 handlePetFusion
	shiny := 0
	if mainPet.Shiny != 0 && subPet.Shiny != 0 {
		shiny = 1
	} else if (mainPet.Shiny != 0 && subPet.Shiny == 0) || (mainPet.Shiny == 0 && subPet.Shiny != 0) {
		if r.Intn(2) == 0 {
			shiny = 1
		}
	}

	tmpPet := &userdb.Pet{
		ID:        resultPetID,
		CatchTime: int(time.Now().Unix()),
		DV:        dv,
		Nature:    nature,
		Level:     1,
		Shiny:     shiny,
	}
	userdb.AssignFusionTraitIfNeeded(tmpPet)

	data := db.GetOrCreateGameData(ctx.UserID)
	obtainTime := uint32(time.Now().Unix())
	newBead := userdb.SoulBead{
		ObtainTime:  obtainTime,
		ItemID:      beadItemID,
		Status:      nil,
		ResultPetID: resultPetID,
		DV:          dv,
		Nature:      nature,
		Trait:       tmpPet.Trait,
		Shiny:       shiny,
	}
	data.SoulBeads = append(data.SoulBeads, newBead)
	db.SaveGameData(ctx.UserID, data)

	// 成功后移除主/副精灵
	db.RemovePet(ctx.UserID, mainCatchTime)
	db.RemovePet(ctx.UserID, subCatchTime)

	// 给玩家发放 1 个对应颜色的元神珠道具
	db.AddItem(ctx.UserID, beadItemID, 1)

	// 构造成功返回的 PetFusionInfo
	binary.BigEndian.PutUint32(body[0:4], obtainTime)
	binary.BigEndian.PutUint32(body[4:8], uint32(beadItemID))
	binary.BigEndian.PutUint32(body[8:12], uint32(mainCatchTime))
	binary.BigEndian.PutUint32(body[12:16], 0) // costItemFlag 暂不区分

	ctx.GameServer.SendResponse(ctx.ClientData, 50006, ctx.UserID, ctx.SeqID, body)
}

// handleSoulBeadToPet CMD 2358 SOULBEAD_TO_PET：将已完成的元神珠孵化为融合精灵
// 简化请求：obtainTime(4)；响应：4 字节 0
// 流程：
//   - 根据 obtainTime 找到 SoulBead 记录
//   - 检查是否至少有一部分吸能进度（后续可严格要求 20 步全 true）
//   - 从背包扣除 1 个对应元神珠物品
//   - 按记录的 ResultPetID/DV/Nature/Trait 生成新精灵并加入背包
//   - 删除该 SoulBead 记录
func handleSoulBeadToPet(ctx *gameserver.HandlerContext) {
	body := make([]byte, 4)

	if ctx.GameServer.UserDB == nil {
		ctx.GameServer.SendResponse(ctx.ClientData, 2358, ctx.UserID, ctx.SeqID, body)
		return
	}

	db := ctx.GameServer.UserDB
	data := db.GetOrCreateGameData(ctx.UserID)

	index := -1
	var bead userdb.SoulBead
	// 优先按请求中的 obtainTime 精确匹配（如果客户端传了非 0）
	if len(ctx.Body) >= 4 {
		obtainTime := binary.BigEndian.Uint32(ctx.Body[0:4])
		if obtainTime != 0 {
			for i, b := range data.SoulBeads {
				if b.ObtainTime == obtainTime {
					index = i
					bead = b
					break
				}
			}
		}
	}
	// 若客户端传 0，则使用当前正在赋形且已完成等待时间的那一颗元神珠
	if index == -1 {
		now := time.Now().Unix()
		for i, b := range data.SoulBeads {
			if b.TransformStartTime > 0 && now-b.TransformStartTime >= soulBeadTransformSec {
				index = i
				bead = b
				break
			}
		}
	}
	if index == -1 || bead.ResultPetID == 0 {
		ctx.GameServer.SendResponse(ctx.ClientData, 2358, ctx.UserID, ctx.SeqID, body)
		return
	}

	// 检查吸能进度：至少要求有一步为 true（避免完全未开始就孵化）
	hasProgress := false
	for _, s := range bead.Status {
		if s {
			hasProgress = true
			break
		}
	}
	if !hasProgress {
		ctx.GameServer.SendResponse(ctx.ClientData, 2358, ctx.UserID, ctx.SeqID, body)
		return
	}

	// 检查赋形计时是否完成：若尚未调用 2357 或未到时间，则不允许孵化
	if bead.TransformStartTime == 0 {
		ctx.GameServer.SendResponse(ctx.ClientData, 2358, ctx.UserID, ctx.SeqID, body)
		return
	}
	now := time.Now().Unix()
	if now-bead.TransformStartTime < soulBeadTransformSec {
		ctx.GameServer.SendResponse(ctx.ClientData, 2358, ctx.UserID, ctx.SeqID, body)
		return
	}

	// 扣除 1 个元神珠道具（若不存在则继续，避免老存档出现问题）
	if db.GetItemCount(ctx.UserID, bead.ItemID) > 0 {
		db.RemoveItem(ctx.UserID, bead.ItemID, 1)
	}

	// 生成新精灵：使用预生成的 DV/Nature/Trait
	catchTime := int(time.Now().Unix())
	p := db.AddPet(ctx.UserID, bead.ResultPetID, catchTime, 1, bead.DV, bead.Nature)
	if p != nil {
		p.Trait = bead.Trait
		p.Shiny = bead.Shiny
		user := db.GetOrCreateGameData(ctx.UserID)
		for i := range user.Pets {
			if user.Pets[i].CatchTime == p.CatchTime {
				user.Pets[i].Trait = bead.Trait
				user.Pets[i].Shiny = bead.Shiny
				break
			}
		}
		db.SaveGameData(ctx.UserID, user)
	}

	// 删除该元神珠记录
	if index >= 0 && index < len(data.SoulBeads) {
		data.SoulBeads = append(data.SoulBeads[:index], data.SoulBeads[index+1:]...)
		db.SaveGameData(ctx.UserID, data)
	}

	// 先推送 CMD 8004，触发客户端「获得精灵」弹窗（与 SPT/分子转化仪一致）
	body8004 := buildBossMonster8004Body(0, uint32(bead.ResultPetID), uint32(catchTime), 0, 0)
	ctx.GameServer.SendResponse(ctx.ClientData, 8004, ctx.UserID, 0, body8004)
	ctx.GameServer.SendResponse(ctx.ClientData, 2358, ctx.UserID, ctx.SeqID, body)
}

// ==================== 物品 / 背包 ====================

// handleItemList CMD 2605 物品列表（战斗中/非战斗均可请求）
// 对齐 Lua: item_handlers.handleItemList
// 请求: itemType1(4)+itemType2(4)+itemType3(4)；不足 12 字节或 (a,0,0) 时按“全部”返回，避免战斗中道具面板为空
// 响应: itemCount(4) + [itemId(4)+count(4)+expireTime(4)+unknown(4)]...
func handleItemList(ctx *gameserver.HandlerContext) {
	var a, b, c uint32
	if len(ctx.Body) >= 12 {
		a = binary.BigEndian.Uint32(ctx.Body[0:4])
		b = binary.BigEndian.Uint32(ctx.Body[4:8])
		c = binary.BigEndian.Uint32(ctx.Body[8:12])
	}
	// 部分客户端在战斗中只发类型或 (type,0,0)，导致 b<c 或 b==0 时 inRange 几乎为假，返回空列表
	if a != 0 && b == 0 && c == 0 {
		a, b, c = 0, 0, 0
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	type itemRow struct {
		id         uint32
		count      uint32
		expireTime uint32
	}
	rows := make([]itemRow, 0, len(user.Items))

	// 收藏物品ID范围（与前端 SingleItemInfo ItemType.COLLECTON 一致，排除装备 100001-200000 和 1300001-1400000）
	isCollectionItem := func(id uint32) bool {
		return (id > 10000 && id < 100000) ||
			(id >= 400001 && id <= 500000) ||
			(id >= 1200001 && id <= 1300000) ||
			(id > 1500000 && id < 1600000)
	}

	inRange := func(id uint32) bool {
		if a == 0 && b == 0 && c == 0 {
			// 收藏 tab 请求(0,0,0)：只返回收藏类物品，装备与收藏分开显示
			return isCollectionItem(id)
		}
		return (id >= a && id <= b) || id == c
	}

	logger.Info(fmt.Sprintf("[2605] 物品列表请求: 范围 %d-%d, %d (用户总物品数: %d)", a, b, c, len(user.Items)))

	for k, it := range user.Items {
		id64, err := strconv.ParseUint(k, 10, 32)
		if err != nil {
			logger.Warning(fmt.Sprintf("[2605] 无效的物品ID: %s", k))
			continue
		}
		id := uint32(id64)
		if !inRange(id) {
			continue
		}
		exp := uint32(it.ExpireTime)
		if exp == 0 {
			exp = 0x057E40
		}
		rows = append(rows, itemRow{
			id:         id,
			count:      uint32(it.Count),
			expireTime: exp,
		})
		logger.Info(fmt.Sprintf("[2605] 添加物品: ID=%d Count=%d ExpireTime=%d", id, it.Count, exp))
	}

	body := make([]byte, 4+len(rows)*16)
	binary.BigEndian.PutUint32(body[0:4], uint32(len(rows)))
	off := 4
	for _, r := range rows {
		binary.BigEndian.PutUint32(body[off:off+4], r.id)
		binary.BigEndian.PutUint32(body[off+4:off+8], r.count)
		binary.BigEndian.PutUint32(body[off+8:off+12], r.expireTime)
		binary.BigEndian.PutUint32(body[off+12:off+16], 0)
		off += 16
	}

	logger.Info(fmt.Sprintf("[2605] 返回物品列表: 数量=%d BodyLen=%d", len(rows), len(body)))
	ctx.GameServer.SendResponse(ctx.ClientData, 2605, ctx.UserID, ctx.SeqID, body)
}

// ==================== 精灵互转 / 展示（2304） ====================

// handlePetRelease CMD 2304 释放/背包仓库互转
// 对齐 Lua: pet_handlers.handlePetRelease（返回 PetTakeOutInfo）
// 请求: catchId(4) + flag(4) [+ clientShiny(4) 可选] - flag=1: 仓库->背包, flag=0: 背包->仓库；背包->仓库时若带 clientShiny 则以此为准写入仓库，避免服务端内存未同步导致异色丢失
// 响应: homeEnergy(4) + firstPetTime(4) + flag(4) + [PetInfo]?
func handlePetRelease(ctx *gameserver.HandlerContext) {
	var catchID uint32
	var reqFlag uint32
	if len(ctx.Body) >= 8 {
		catchID = binary.BigEndian.Uint32(ctx.Body[0:4])
		reqFlag = binary.BigEndian.Uint32(ctx.Body[4:8])
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)

	// 确保StoragePets不为nil
	if user.StoragePets == nil {
		user.StoragePets = []userdb.Pet{}
	}

	var picked *userdb.Pet
	var pickedIndex int = -1
	var respFlag uint32 = 0
	var movedFromStorage bool = false

	if reqFlag == 1 {
		// 仓库 -> 背包（或「已在背包」仅取信息，如 2358 赋形后客户端收到 8004 会发 2304(catchTime,1) 取 PetInfo 弹窗）
		for i := range user.StoragePets {
			if uint32(user.StoragePets[i].CatchTime) == catchID {
				picked = &user.StoragePets[i]
				pickedIndex = i
				break
			}
		}
		if picked != nil {
			// 先拷贝一份，避免在切片删除后 picked 指向的数据被覆盖（含 Shiny，取回背包后异色不丢）
			petCopy := *picked
			// 从仓库移除
			user.StoragePets = append(user.StoragePets[:pickedIndex], user.StoragePets[pickedIndex+1:]...)
			// 添加到背包
			user.Pets = append(user.Pets, petCopy)
			movedFromStorage = true
			respFlag = 1 // 返回PetInfo（响应体用刚加入背包的精灵，见下方 petBody）
			logger.Info(fmt.Sprintf("[2304] 仓库->背包: PetID=%d CatchTime=%d Shiny=%d (异色须保留)", petCopy.ID, petCopy.CatchTime, petCopy.Shiny))
		} else {
			// 已在背包（如元神赋形 2358 刚加的精灵）：不移动，只返回 PetInfo 供客户端弹「获得精灵」
			for i := range user.Pets {
				if uint32(user.Pets[i].CatchTime) == catchID {
					picked = &user.Pets[i]
					respFlag = 1
					break
				}
			}
		}
	} else {
		// 背包 -> 仓库
		for i := range user.Pets {
			if uint32(user.Pets[i].CatchTime) == catchID {
				picked = &user.Pets[i]
				pickedIndex = i
				break
			}
		}
		if picked != nil {
			// 先拷贝一份，避免后续对切片的修改影响 picked 指针（含 Shiny，仓库/取回后异色不丢）
			petCopy := *picked
			// 从背包移除
			user.Pets = append(user.Pets[:pickedIndex], user.Pets[pickedIndex+1:]...)
			// 添加到仓库
			user.StoragePets = append(user.StoragePets, petCopy)
			respFlag = 0 // 不返回PetInfo
			logger.Info(fmt.Sprintf("[2304] 背包->仓库: PetID=%d CatchTime=%d Shiny=%d (异色须保留)", petCopy.ID, petCopy.CatchTime, petCopy.Shiny))
		}
	}

	firstPetTime := uint32(0)
	if len(user.Pets) > 0 {
		firstPetTime = uint32(user.Pets[0].CatchTime)
	}

	// 保存数据
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}

	petBody := []byte{}
	if respFlag == 1 {
		if movedFromStorage && len(user.Pets) > 0 {
			petBody = buildFullPetInfo(user.Pets[len(user.Pets)-1])
		} else if picked != nil {
			petBody = buildFullPetInfo(*picked)
		}
	}

	body := make([]byte, 12+len(petBody))
	binary.BigEndian.PutUint32(body[0:4], 0)            // homeEnergy
	binary.BigEndian.PutUint32(body[4:8], firstPetTime) // firstPetTime
	binary.BigEndian.PutUint32(body[8:12], respFlag)    // flag
	if len(petBody) > 0 {
		copy(body[12:], petBody)
	}

	ctx.GameServer.SendResponse(ctx.ClientData, 2304, ctx.UserID, ctx.SeqID, body)
	logger.Info(fmt.Sprintf("[2304] 精灵仓库互转: catchID=%d flag=%d respFlag=%d", catchID, reqFlag, respFlag))
}

// handlePetEvolveInBabin CMD 2320 精灵进化舱进化
// 请求: catchTime(4) - 需要在进化舱中进化的背包精灵唯一标识
// 行为:
//   - 仅支持背包中的精灵（user.Pets），不处理仓库精灵；
//   - 校验精灵配置：存在 EvolvesTo、等级达到 EvolvingLv 要求；
//   - 若配置了 EvolvItem/EvolvItemCount，则要求玩家背包有足够道具并扣除；
//   - 忽略 EvolveBabin 标志本身（即把“必须在进化舱进化”的限制视为当前已满足环境），执行一次进化。
// 响应:
//   - 失败: 返回 4 字节 0（对齐常见“result=0”风格），不修改任何数据；
//   - 成功: 返回 4 字节 1 + PetInfo（与 2301 相同结构），客户端可直接用来刷新精灵信息。
func handlePetEvolveInBabin(ctx *gameserver.HandlerContext) {
	if len(ctx.Body) < 8 {
		body := make([]byte, 4)
		binary.BigEndian.PutUint32(body[0:4], 0)
		ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
		return
	}

	catchTime := int(binary.BigEndian.Uint32(ctx.Body[0:4]))
	evolveIndex := int(binary.BigEndian.Uint32(ctx.Body[4:8]))
	logger.Info(fmt.Sprintf("[2314] 请求进化: UID=%d catchTime=%d evolveIndex=%d", ctx.UserID, catchTime, evolveIndex))
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)

	if user.Pets == nil || len(user.Pets) == 0 {
		body := make([]byte, 4)
		binary.BigEndian.PutUint32(body[0:4], 0)
		ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
		return
	}

	var picked *userdb.Pet
	for i := range user.Pets {
		if user.Pets[i].CatchTime == catchTime {
			picked = &user.Pets[i]
			break
		}
	}
	if picked == nil {
		body := make([]byte, 4)
		binary.BigEndian.PutUint32(body[0:4], 0)
		ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
		logger.Info(fmt.Sprintf("[2314] 进化失败: 未找到背包精灵 catchTime=%d", catchTime))
		return
	}

	petMgr := gamepets.GetInstance()
	def := petMgr.Get(picked.ID)
	if def == nil {
		body := make([]byte, 4)
		binary.BigEndian.PutUint32(body[0:4], 0)
		ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
		logger.Info(fmt.Sprintf("[2314] 进化失败: 未找到配置 PetID=%d", picked.ID))
		return
	}

	// 进化仓专属：仅允许配置了 EvolveBabin=1 或 EvolvFlag>0（分支进化）的精灵在此界面使用进化
	if def.EvolveBabin != 1 && def.EvolvFlag == 0 {
		body := make([]byte, 4)
		binary.BigEndian.PutUint32(body[0:4], 0)
		ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
		logger.Info(fmt.Sprintf("[2314] 进化失败: 非进化舱/分支进化精灵 PetID=%d EvolveBabin=%d EvolvFlag=%d", picked.ID, def.EvolveBabin, def.EvolvFlag))
		return
	}

	// ==================== 分支进化（EvolvFlag>0） ====================
	// 对于类似 悠悠(91) 这种 EvolvFlag>0 且 EvolvesTo=0 的配置，
	// 按前端 EvolveXMLInfo.getMonToIDs(flag) 的配置执行：
	// - 使用 EvolvFlag 获取分支列表（保持 XML 顺序）
	// - 根据 evolveIndex 选择目标 MonTo
	// - 校验并扣除该分支所需道具（EvolvItem/EvolvItemCount）
	// 若无法加载 evolve 配置，则退回到旧的“按 EvolvesFrom 枚举子代”的推断逻辑（会尽力去重 RealId）。
	if def.EvolvFlag > 0 && def.EvolvesTo == 0 {
		if evolveIndex <= 0 {
			body := make([]byte, 4)
			binary.BigEndian.PutUint32(body[0:4], 0)
			ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
			logger.Info(fmt.Sprintf("[2314] 进化失败: 分支索引非法 PetID=%d evolveIndex=%d", picked.ID, evolveIndex))
			return
		}

		// 1) 优先按 evolve 配置（与前端一致）
		if branches, ok := gameevolve.GetBranches(def.EvolvFlag); ok {
			if evolveIndex > len(branches) {
				body := make([]byte, 4)
				binary.BigEndian.PutUint32(body[0:4], 0)
				ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
				logger.Info(fmt.Sprintf("[2314] 进化失败: 分支索引超出范围 PetID=%d flag=%d evolveIndex=%d branches=%d", picked.ID, def.EvolvFlag, evolveIndex, len(branches)))
				return
			}

			br := branches[evolveIndex-1]
			targetID := br.MonTo
			logger.Info(fmt.Sprintf("[2314] 分支进化选择(XML): PetID=%d EvolvFlag=%d evolveIndex=%d -> targetID=%d item=%d x%d",
				picked.ID, def.EvolvFlag, evolveIndex, targetID, br.EvolvItem, br.EvolvItemCount))

			// 校验/扣除材料
			if br.EvolvItem > 0 && br.EvolvItemCount > 0 {
				if user.Items == nil {
					user.Items = make(map[string]userdb.Item)
				}
				itemKey := strconv.Itoa(br.EvolvItem)
				it, has := user.Items[itemKey]
				if !has || it.Count < br.EvolvItemCount {
					body := make([]byte, 4)
					binary.BigEndian.PutUint32(body[0:4], 0)
					ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
					logger.Info(fmt.Sprintf("[2314] 进化失败: 分支材料不足 PetID=%d flag=%d needItem=%d x%d have=%d",
						picked.ID, def.EvolvFlag, br.EvolvItem, br.EvolvItemCount, it.Count))
					return
				}
				it.Count -= br.EvolvItemCount
				if it.Count <= 0 {
					delete(user.Items, itemKey)
				} else {
					user.Items[itemKey] = it
				}
			}

			defTarget := petMgr.Get(targetID)
			if defTarget == nil {
				body := make([]byte, 4)
				binary.BigEndian.PutUint32(body[0:4], 0)
				ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
				logger.Info(fmt.Sprintf("[2314] 进化失败: 目标配置缺失 targetID=%d", targetID))
				return
			}

			// 等级校验：按目标形态 EvolvingLv（与之前逻辑一致）
			if defTarget.EvolvingLv > 0 && picked.Level < defTarget.EvolvingLv {
				body := make([]byte, 4)
				binary.BigEndian.PutUint32(body[0:4], 0)
				ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
				logger.Info(fmt.Sprintf("[2314] 进化失败: 等级不足 分支目标 targetID=%d needLv=%d curLv=%d", targetID, defTarget.EvolvingLv, picked.Level))
				return
			}

			oldID := picked.ID
			picked.ID = targetID
			picked.Exp = 0

			ev := gamepets.ClampAndCapEV(picked.GetEVStats())
			stats := petMgr.GetStats(picked.ID, picked.Level, picked.DV, ev, picked.Nature)

			if len(picked.Skills) == 0 {
				defaults := petMgr.GetSkillsForLevel(picked.ID, picked.Level)
				if len(defaults) > 0 {
					skills := make([]int, 0, 4)
					for i := 0; i < 4 && i < len(defaults); i++ {
						if defaults[i] > 0 {
							skills = append(skills, defaults[i])
						}
					}
					picked.Skills = skills
				}
			}

			if ctx.GameServer.UserDB != nil {
				ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
			}

			// NOTE_UPDATE_PROP(2508) 触发进化动画与属性面板
			propBody := buildNoteUpdateProp(uint32(picked.CatchTime), picked.ID, picked.Level, picked.Exp,
				stats.MaxHP, stats.Attack, stats.Defence, stats.SpAtk, stats.SpDef, stats.Speed, ev)
			ctx.GameServer.SendResponse(ctx.ClientData, 2508, ctx.UserID, ctx.SeqID, propBody)

			petBody := buildFullPetInfo(*picked)
			body := make([]byte, 4+len(petBody))
			binary.BigEndian.PutUint32(body[0:4], 1)
			copy(body[4:], petBody)

			logger.Info(fmt.Sprintf("[2314] 分支进化成功(XML): UID=%d CatchTime=%d PetID %d -> %d Lv=%d HP=%d",
				ctx.UserID, picked.CatchTime, oldID, picked.ID, picked.Level, stats.MaxHP))

			ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
			return
		}

		// 2) 兜底：没有 evolve 配置时，按 EvolvesFrom 枚举子代并去重 RealId（不扣材料）
		rawChildren := petMgr.All()
		if len(rawChildren) == 0 {
			body := make([]byte, 4)
			binary.BigEndian.PutUint32(body[0:4], 0)
			ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
			logger.Info(fmt.Sprintf("[2314] 进化失败: Pets.All() 返回为空 PetID=%d", picked.ID))
			return
		}
		byReal := make(map[int]int) // realID -> chosenID(最小)
		for _, p := range rawChildren {
			if p == nil || p.EvolvesFrom != picked.ID {
				continue
			}
			real := p.RealId
			if real <= 0 {
				real = p.ID
			}
			if cur, ok := byReal[real]; !ok || p.ID < cur {
				byReal[real] = p.ID
			}
		}
		if len(byReal) == 0 {
			body := make([]byte, 4)
			binary.BigEndian.PutUint32(body[0:4], 0)
			ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
			logger.Info(fmt.Sprintf("[2314] 进化失败: 未找到分支目标(兜底) PetID=%d EvolvFlag=%d", picked.ID, def.EvolvFlag))
			return
		}

		children := make([]int, 0, len(byReal))
		for _, id := range byReal {
			children = append(children, id)
		}
		sort.Ints(children)
		if evolveIndex > len(children) {
			body := make([]byte, 4)
			binary.BigEndian.PutUint32(body[0:4], 0)
			ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
			logger.Info(fmt.Sprintf("[2314] 进化失败: 分支索引超出范围(兜底) PetID=%d evolveIndex=%d children=%v", picked.ID, evolveIndex, children))
			return
		}

		targetID := children[evolveIndex-1]
		logger.Info(fmt.Sprintf("[2314] 分支进化选择(兜底): PetID=%d EvolvFlag=%d evolveIndex=%d -> targetID=%d", picked.ID, def.EvolvFlag, evolveIndex, targetID))
		defTarget := petMgr.Get(targetID)
		if defTarget == nil {
			body := make([]byte, 4)
			binary.BigEndian.PutUint32(body[0:4], 0)
			ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
			logger.Info(fmt.Sprintf("[2314] 进化失败: 目标配置缺失 targetID=%d", targetID))
			return
		}

		// 分支进化统一视为“在进化舱环境中”，等级条件仍然按原始配置 EvolvingLv 校验；
		if defTarget.EvolvingLv > 0 && picked.Level < defTarget.EvolvingLv {
			body := make([]byte, 4)
			binary.BigEndian.PutUint32(body[0:4], 0)
			ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
			logger.Info(fmt.Sprintf("[2314] 进化失败: 等级不足 分支目标 targetID=%d needLv=%d curLv=%d", targetID, defTarget.EvolvingLv, picked.Level))
			return
		}

		oldID := picked.ID
		picked.ID = targetID
		picked.Exp = 0

		ev := gamepets.ClampAndCapEV(picked.GetEVStats())
		stats := petMgr.GetStats(picked.ID, picked.Level, picked.DV, ev, picked.Nature)

		if len(picked.Skills) == 0 {
			defaults := petMgr.GetSkillsForLevel(picked.ID, picked.Level)
			if len(defaults) > 0 {
				skills := make([]int, 0, 4)
				for i := 0; i < 4 && i < len(defaults); i++ {
					if defaults[i] > 0 {
						skills = append(skills, defaults[i])
					}
				}
				picked.Skills = skills
			}
		}

		if ctx.GameServer.UserDB != nil {
			ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
		}

		// 前端 EvolvePetPanel 依赖 NOTE_UPDATE_PROP(2508) 来触发 effectMC 动画并弹出属性面板。
		propBody := buildNoteUpdateProp(uint32(picked.CatchTime), picked.ID, picked.Level, picked.Exp,
			stats.MaxHP, stats.Attack, stats.Defence, stats.SpAtk, stats.SpDef, stats.Speed, ev)
		ctx.GameServer.SendResponse(ctx.ClientData, 2508, ctx.UserID, ctx.SeqID, propBody)

		petBody := buildFullPetInfo(*picked)
		body := make([]byte, 4+len(petBody))
		binary.BigEndian.PutUint32(body[0:4], 1)
		copy(body[4:], petBody)

		logger.Info(fmt.Sprintf("[2314] 分支进化成功: UID=%d CatchTime=%d PetID %d -> %d Lv=%d HP=%d",
			ctx.UserID, picked.CatchTime, oldID, picked.ID, picked.Level, stats.MaxHP))

		ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
		return
	}

	// 检查/扣除进化所需道具
	hasItem := true
	if def.EvolvItem > 0 && def.EvolvItemCount > 0 {
		if user.Items == nil {
			hasItem = false
		} else {
			itemKey := strconv.Itoa(def.EvolvItem)
			if it, ok := user.Items[itemKey]; !ok || it.Count < def.EvolvItemCount {
				hasItem = false
			}
		}
		if !hasItem {
			body := make([]byte, 4)
			binary.BigEndian.PutUint32(body[0:4], 0)
			ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
			logger.Info(fmt.Sprintf("[2314] 进化失败: 道具不足 PetID=%d NeedItem=%d x%d", picked.ID, def.EvolvItem, def.EvolvItemCount))
			return
		}
	}

	canEvolve, _, evolveTo := petMgr.CanEvolveInBabin(picked.ID, picked.Level, hasItem)
	if !canEvolve || evolveTo <= 0 {
		body := make([]byte, 4)
		binary.BigEndian.PutUint32(body[0:4], 0)
		ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
		logger.Info(fmt.Sprintf("[2314] 进化失败: 条件未满足 PetID=%d Level=%d canEvolve=%v evolveTo=%d", picked.ID, picked.Level, canEvolve, evolveTo))
		return
	}

	// 扣除道具
	if def.EvolvItem > 0 && def.EvolvItemCount > 0 && hasItem {
		if user.Items == nil {
			user.Items = make(map[string]userdb.Item)
		}
		itemKey := strconv.Itoa(def.EvolvItem)
		if it, ok := user.Items[itemKey]; ok && it.Count > 0 {
			it.Count -= def.EvolvItemCount
			if it.Count <= 0 {
				delete(user.Items, itemKey)
			} else {
				user.Items[itemKey] = it
			}
		}
	}

	// 执行进化：更新精灵 ID，清空当前等级经验
	oldID := picked.ID
	picked.ID = evolveTo
	picked.Exp = 0

	// 进化后属性重新计算
	ev := gamepets.ClampAndCapEV(picked.GetEVStats())
	stats := petMgr.GetStats(picked.ID, picked.Level, picked.DV, ev, picked.Nature)

	// 若当前技能列表为空，则按新形态的当前等级补满 4 个默认技能
	if len(picked.Skills) == 0 {
		defaults := petMgr.GetSkillsForLevel(picked.ID, picked.Level)
		if len(defaults) > 0 {
			skills := make([]int, 0, 4)
			for i := 0; i < 4 && i < len(defaults); i++ {
				if defaults[i] > 0 {
					skills = append(skills, defaults[i])
				}
			}
			picked.Skills = skills
		}
	}

	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}

	// 前端 EvolvePetPanel 依赖 NOTE_UPDATE_PROP(2508) 来触发 effectMC 动画并弹出属性面板。
	propBody := buildNoteUpdateProp(uint32(picked.CatchTime), picked.ID, picked.Level, picked.Exp,
		stats.MaxHP, stats.Attack, stats.Defence, stats.SpAtk, stats.SpDef, stats.Speed, ev)
	ctx.GameServer.SendResponse(ctx.ClientData, 2508, ctx.UserID, ctx.SeqID, propBody)

	// 构建响应：result(4) + PetInfo（与 2301 相同体）
	petBody := buildFullPetInfo(*picked)
	body := make([]byte, 4+len(petBody))
	binary.BigEndian.PutUint32(body[0:4], 1)
	copy(body[4:], petBody)

	logger.Info(fmt.Sprintf("[2320] 进化舱进化成功: UID=%d CatchTime=%d PetID %d -> %d Lv=%d HP=%d",
		ctx.UserID, picked.CatchTime, oldID, picked.ID, picked.Level, stats.MaxHP))

	ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
}

// handleGetPetInfo CMD 2301 获取精灵完整信息（用于战斗前 PetManager 初始化 / PvP 时查看对方精灵）
// 请求: catchTime(4)；若为 0 则返回当前/第一只精灵。PvP 时客户端会用对方 catchTime 请求，需从对方用户查并返回
func handleGetPetInfo(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)

	var catch uint32
	if len(ctx.Body) >= 4 {
		catch = binary.BigEndian.Uint32(ctx.Body[0:4])
	}

	var picked *userdb.Pet
	if catch != 0 {
		for i := range user.Pets {
			if uint32(user.Pets[i].CatchTime) == catch {
				picked = &user.Pets[i]
				break
			}
		}
		if picked == nil && user.StoragePets != nil {
			for i := range user.StoragePets {
				if uint32(user.StoragePets[i].CatchTime) == catch {
					picked = &user.StoragePets[i]
					break
				}
			}
		}
		// PvP 时：若己方未找到该 catchTime，尝试从对方用户查找（用于显示对方精灵属性/模型）
		// 精灵大乱斗：禁止从 oppUser 返回对方精灵，否则客户端会误将对方精灵加入己方背包；仅从 GrandMeleePets 返回己方分配的精灵
		if picked == nil {
			ctx.GameServer.BattleMu.Lock()
			battle, ok := ctx.GameServer.BattleStates[ctx.UserID]
			if ok && battle.IsActive && battle.OpponentUserID != 0 {
				// 精灵大乱斗：优先从己方分配的 GrandMeleePets 查找；绝不从 oppUser 返回，避免战毕后客户端误加对方精灵到背包
				if battle.IsGrandMelee {
					if ctx.GameServer.GrandMeleePets != nil {
						for i := range ctx.GameServer.GrandMeleePets[ctx.UserID] {
							if uint32(ctx.GameServer.GrandMeleePets[ctx.UserID][i].CatchTime) == catch {
								picked = &ctx.GameServer.GrandMeleePets[ctx.UserID][i]
								break
							}
						}
					}
					// 大乱斗时不再从 oppUser 查找，picked 仍为 nil 则后续返回空或第一只
				} else {
					oppUser := ctx.GameServer.GetOrCreateUser(battle.OpponentUserID)
					for i := range oppUser.Pets {
						if uint32(oppUser.Pets[i].CatchTime) == catch {
							picked = &oppUser.Pets[i]
							break
						}
					}
					if picked != nil {
						ctx.GameServer.BattleMu.Unlock()
						body := buildFullPetInfo(*picked)
						logger.Info(fmt.Sprintf("[2301] PvP 对方精灵: PetID=%d Level=%d BodyLen=%d", picked.ID, picked.Level, len(body)))
						ctx.GameServer.SendResponse(ctx.ClientData, 2301, ctx.UserID, ctx.SeqID, body)
						return
					}
				}
			}
			ctx.GameServer.BattleMu.Unlock()
		}

		// 若不在战斗中或战斗中未找到，且自身/仓库都没有该 catchTime，
		// 说明请求的很可能是“场景中其它玩家跟随的精灵”。
		// 为了让点击他人跟随精灵时能看到对方精灵信息，这里在同地图其它玩家中按 catchTime 搜索一次。
		if picked == nil && user.MapID != 0 {
			clientsOnMap := ctx.GameServer.GetClientsOnMap(user.MapID)
			for _, c := range clientsOnMap {
				if c.UserID == ctx.UserID {
					continue
				}
				otherUser := ctx.GameServer.GetOrCreateUser(c.UserID)
				if otherUser == nil {
					continue
				}
				for i := range otherUser.Pets {
					if uint32(otherUser.Pets[i].CatchTime) == catch {
						p := otherUser.Pets[i]
						body := buildFullPetInfo(p)
						logger.Info(fmt.Sprintf("[2301] Map 他人精灵: Owner=%d PetID=%d Level=%d BodyLen=%d", c.UserID, p.ID, p.Level, len(body)))
						ctx.GameServer.SendResponse(ctx.ClientData, 2301, ctx.UserID, ctx.SeqID, body)
						// 对于他人精灵，仅返回 2301，不推送 2508/属性更新，避免污染本地 PetManager 状态。
						return
					}
				}
				if otherUser.StoragePets != nil {
					for i := range otherUser.StoragePets {
						if uint32(otherUser.StoragePets[i].CatchTime) == catch {
							p := otherUser.StoragePets[i]
							body := buildFullPetInfo(p)
							logger.Info(fmt.Sprintf("[2301] Map 他人仓库精灵: Owner=%d PetID=%d Level=%d BodyLen=%d", c.UserID, p.ID, p.Level, len(body)))
							ctx.GameServer.SendResponse(ctx.ClientData, 2301, ctx.UserID, ctx.SeqID, body)
							return
						}
					}
				}
			}
		}
	}
	if picked == nil && len(user.Pets) > 0 {
		picked = &user.Pets[0]
	}
	if picked == nil {
		ctx.GameServer.SendResponse(ctx.ClientData, 2301, ctx.UserID, ctx.SeqID, []byte{})
		return
	}

	body := buildFullPetInfo(*picked)
	logger.Info(fmt.Sprintf("[2301] 发送完整宠物信息: PetID=%d Level=%d BodyLen=%d", picked.ID, picked.Level, len(body)))
	ctx.GameServer.SendResponse(ctx.ClientData, 2301, ctx.UserID, ctx.SeqID, body)

	// 主动推送 2508，让客户端 UpdatePropInfo.update() 把 nextLvExp 等写回缓存，经验分配器/精灵面板才能显示「升级所需经验值」
	stats := gamepets.GetInstance().GetStats(picked.ID, picked.Level, picked.DV, picked.GetEVStats(), picked.Nature)
	propBody := buildNoteUpdateProp(uint32(picked.CatchTime), picked.ID, picked.Level, picked.Exp, stats.MaxHP,
		stats.Attack, stats.Defence, stats.SpAtk, stats.SpDef, stats.Speed, picked.GetEVStats())
	ctx.GameServer.SendResponse(ctx.ClientData, 2508, ctx.UserID, ctx.SeqID, propBody)
}

// handleSetDefaultPet CMD 2308 设置首发精灵（客户端 CommandID.PET_DEFAULT）
// 请求: catchTime(4) - 要设置为首发的精灵的捕获时间
// 响应: 4 字节 0 表示成功（客户端部分实现依赖非空包才触发回调）
func handleSetDefaultPet(ctx *gameserver.HandlerContext) {
	respBody := make([]byte, 4)
	binary.BigEndian.PutUint32(respBody[0:4], 0)
	if len(ctx.Body) < 4 {
		logger.Error("[2308] 请求体长度不足")
		ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, respBody)
		return
	}

	catchTime := binary.BigEndian.Uint32(ctx.Body[0:4])
	logger.Info(fmt.Sprintf("[2308] 收到设置首发精灵请求: UID=%d CatchTime=%d", ctx.UserID, catchTime))

	gameData := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	if gameData == nil {
		logger.Error(fmt.Sprintf("[2308] 无法获取用户数据: UID=%d", ctx.UserID))
		ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, respBody)
		return
	}

	var targetIndex = -1
	for i, pet := range gameData.Pets {
		if uint32(pet.CatchTime) == catchTime {
			targetIndex = i
			break
		}
	}

	if targetIndex == -1 {
		logger.Error(fmt.Sprintf("[2308] 未找到指定精灵: UID=%d CatchTime=%d", ctx.UserID, catchTime))
		ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, respBody)
		return
	}

	if targetIndex == 0 {
		logger.Info(fmt.Sprintf("[2308] 精灵已是首发: UID=%d PetID=%d", ctx.UserID, gameData.Pets[0].ID))
		ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, respBody)
		return
	}

	targetPet := gameData.Pets[targetIndex]
	logger.Info(fmt.Sprintf("[2308] 移动精灵到首发位置: UID=%d PetID=%d 从位置%d到位置0", ctx.UserID, targetPet.ID, targetIndex))

	newPets := make([]userdb.Pet, len(gameData.Pets))
	newPets[0] = targetPet
	newIndex := 1
	for i, pet := range gameData.Pets {
		if i != targetIndex {
			newPets[newIndex] = pet
			newIndex++
		}
	}
	gameData.Pets = newPets
	ctx.GameServer.UserDB.SaveGameData(ctx.UserID, gameData)

	logger.Info(fmt.Sprintf("[2308] 设置首发精灵成功: UID=%d PetID=%d CatchTime=%d", ctx.UserID, targetPet.ID, catchTime))
	ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, respBody)
}

// handleGetPetList CMD 2303 获取精灵列表（返回仓库中的精灵）
// 响应与前端 PetListInfo 一致: count(4) + [petId(4) + catchTime(4) + skinID(4) + shiny(4)]...
func handleGetPetList(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)

	// 返回仓库中的精灵（不是背包）
	if user.StoragePets == nil {
		user.StoragePets = []userdb.Pet{}
	}
	pets := user.StoragePets
	count := uint32(len(pets))

	body := make([]byte, 4+int(count)*16)
	binary.BigEndian.PutUint32(body[0:4], count)
	off := 4
	for _, p := range pets {
		binary.BigEndian.PutUint32(body[off:off+4], uint32(p.ID))
		off += 4
		binary.BigEndian.PutUint32(body[off:off+4], uint32(p.CatchTime))
		off += 4
		binary.BigEndian.PutUint32(body[off:off+4], 0) // skinID = 0
		off += 4
		shiny := uint32(0)
		if p.Shiny != 0 {
			shiny = 1
		}
		binary.BigEndian.PutUint32(body[off:off+4], shiny)
		off += 4
	}

	ctx.GameServer.SendResponse(ctx.ClientData, 2303, ctx.UserID, ctx.SeqID, body[:off])
}

// handlePetRoomList CMD 2324 房间精灵列表（与前端 RoomPetManager.onList 解析一致）
// 前端用 PET_ROOM_LIST(2324) 请求，解析为 count(4) + [PetListInfo: id(4)+catchTime(4)+skinID(4)+shiny(4)]...
// 返回仓库精灵列表，带 shiny，这样仓库场景里 RoomPetModel 才能显示异色
func handlePetRoomList(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	// 仅返回当前在房间中展示的精灵（由 2323 持久化的 RoomPets），而不是全部仓库精灵
	if user.StoragePets == nil {
		user.StoragePets = []userdb.Pet{}
	}
	if user.RoomPets == nil {
		user.RoomPets = []int{}
	}
	// 构建展示列表（顺序按 RoomPets，最多 5 只）
	display := make([]userdb.Pet, 0, 5)
	for _, ct := range user.RoomPets {
		if len(display) >= 5 {
			break
		}
		for _, p := range user.StoragePets {
			if p.CatchTime == ct {
				display = append(display, p)
				break
			}
		}
	}
	count := uint32(len(display))
	body := make([]byte, 4+int(count)*16)
	binary.BigEndian.PutUint32(body[0:4], count)
	off := 4
	for _, p := range display {
		binary.BigEndian.PutUint32(body[off:off+4], uint32(p.ID))
		off += 4
		binary.BigEndian.PutUint32(body[off:off+4], uint32(p.CatchTime))
		off += 4
		binary.BigEndian.PutUint32(body[off:off+4], 0) // skinID
		off += 4
		shiny := uint32(0)
		if p.Shiny != 0 {
			shiny = 1
		}
		binary.BigEndian.PutUint32(body[off:off+4], shiny)
		off += 4
	}
	logger.Info(fmt.Sprintf("[2324] 精灵仓库列表: UID=%d 数量=%d", ctx.UserID, count))
	ctx.GameServer.SendResponse(ctx.ClientData, 2324, ctx.UserID, ctx.SeqID, body[:off])
}

// buildStorageListBody 构建 2303/2324 仓库列表包体：count(4) + [id(4)+catchTime(4)+skinID(4)+shiny(4)]...
func buildStorageListBody(pets []userdb.Pet) []byte {
	if pets == nil {
		pets = []userdb.Pet{}
	}
	count := uint32(len(pets))
	body := make([]byte, 4+int(count)*16)
	binary.BigEndian.PutUint32(body[0:4], count)
	off := 4
	for _, p := range pets {
		binary.BigEndian.PutUint32(body[off:off+4], uint32(p.ID))
		off += 4
		binary.BigEndian.PutUint32(body[off:off+4], uint32(p.CatchTime))
		off += 4
		binary.BigEndian.PutUint32(body[off:off+4], 0) // skinID
		off += 4
		shiny := uint32(0)
		if p.Shiny != 0 {
			shiny = 1
		}
		binary.BigEndian.PutUint32(body[off:off+4], shiny)
		off += 4
	}
	return body[:off]
}

// pushShinyUpdateToClient GM 设置异色后调用：向该用户在线连接推送 2001/2303/2324/背包每只 2301（已停用，易导致地图卡住）
// 见 pushStorageListToClient：仅修改仓库时推送 2324。
func pushShinyUpdateToClient(gs *gameserver.GameServer, userID int64) {
	// 已禁用：推送 2001/2303/2301 会导致客户端地图/角色不显示
}

// pushStorageListToClient 仅推送 2324（仓库列表），用于 GM 修改仓库精灵异色后让客户端重开仓库即可看到
func pushStorageListToClient(gs *gameserver.GameServer, userID int64) {
	client := gs.GetClientByUserID(userID)
	if client == nil {
		return
	}
	user := gs.GetOrCreateUser(userID)
	body := buildStorageListBody(user.StoragePets)
	gs.SendResponse(client, 2324, userID, 0, body)
	logger.Info(fmt.Sprintf("[GM] 推送仓库列表 2324: UserID=%d 共 %d 只", userID, len(user.StoragePets)))
}

// handleBufferRecord CMD 6001 模拟 Lua 服 BUFFERRECORD：更新用户 bufferRecord 位图
// 请求体: count(4) + [index(4)+value(4)] * count
// 响应体: 可为空（客户端只看 result 是否为 0）
func handleBufferRecord(ctx *gameserver.HandlerContext) {
	if len(ctx.Body) < 4 {
		logger.Error(fmt.Sprintf("[6001] BUFFERRECORD 包体长度不足: %d", len(ctx.Body)))
		ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, []byte{})
		return
	}

	count := binary.BigEndian.Uint32(ctx.Body[0:4])
	data := ctx.Body[4:]
	if len(data) < int(count)*8 {
		logger.Error(fmt.Sprintf("[6001] BUFFERRECORD 数据长度不足: count=%d len(data)=%d", count, len(data)))
		ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, []byte{})
		return
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	if user.BufferRecord == nil {
		user.BufferRecord = make([]byte, 200) // 1600 bit
	} else if len(user.BufferRecord) < 200 {
		tmp := make([]byte, 200)
		copy(tmp, user.BufferRecord)
		user.BufferRecord = tmp
	}

	for i := uint32(0); i < count; i++ {
		offset := i * 8
		if int(offset+8) > len(data) {
			break
		}
		bitIndex := binary.BigEndian.Uint32(data[offset : offset+4])
		value := binary.BigEndian.Uint32(data[offset+4 : offset+8])

		if bitIndex >= 1600 {
			continue
		}
		bytePos := bitIndex / 8
		bitPos := bitIndex % 8

		if value != 0 {
			user.BufferRecord[bytePos] |= (1 << bitPos)
		} else {
			user.BufferRecord[bytePos] &^= (1 << bitPos)
		}
	}

	ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	firstByte := byte(0)
	if len(user.BufferRecord) > 0 {
		firstByte = user.BufferRecord[0]
	}
	logger.Info(fmt.Sprintf("[1013] 更新 BufferRecord 并落盘: UID=%d count=%d len=%d firstByte=0x%02X", ctx.UserID, count, len(user.BufferRecord), firstByte))
	ctx.GameServer.UserDB.SaveToFile()
	ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, []byte{})
}

// ==================== 玩家对战邀请（2401/2403 -> 2501/2502） ====================

// handleInviteToFight CMD 2401 邀请玩家对战（INVITE_TO_FIGHT）
// 请求: targetUserID(4) + mode(4)，FightWaitPanel.send 发送被邀请方 userID 与对战模式(1=单挑 2=多精灵)
// 向被邀请方推送 NOTE_INVITE_TO_FIGHT(2501)，body: inviterUserID(4)+inviterNick(16)+mode(4)，供其弹窗选择接受/拒绝
func handleInviteToFight(ctx *gameserver.HandlerContext) {
	targetUserID := int64(0)
	mode := uint32(0)
	if len(ctx.Body) >= 8 {
		targetUserID = int64(binary.BigEndian.Uint32(ctx.Body[0:4]))
		mode = binary.BigEndian.Uint32(ctx.Body[4:8])
	}
	inviter := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	nick := inviter.Nick
	if nick == "" {
		nick = fmt.Sprintf("用户%d", ctx.UserID)
	}
	// 响应邀请者：简单回包（可选，客户端可能不依赖）
	bodyAck := make([]byte, 4)
	binary.BigEndian.PutUint32(bodyAck[0:4], 0)
	ctx.GameServer.SendResponse(ctx.ClientData, 2401, ctx.UserID, ctx.SeqID, bodyAck)

	targetClient := ctx.GameServer.GetClientByUserID(targetUserID)
	if targetClient == nil {
		return // 被邀请方不在线，不推送
	}
	noteBody := make([]byte, 4+16+4)
	binary.BigEndian.PutUint32(noteBody[0:4], uint32(ctx.UserID))
	putFixedString(noteBody, 4, nick, 16)
	binary.BigEndian.PutUint32(noteBody[20:24], mode)
	ctx.GameServer.SendResponse(targetClient, 2501, ctx.UserID, 0, noteBody)
}

// handleHandleFightInvite CMD 2403 接受/拒绝对战邀请（HANDLE_FIGHT_INVITE）
// 请求: inviterUserID(4) + result(4) + type(4)，result=1 接受、0 拒绝；type 为对战模式
// 向邀请方推送 NOTE_HANDLE_FIGHT_INVITE(2502)，body: responderUserID(4)+responderNick(16)+result(4)
func handleHandleFightInvite(ctx *gameserver.HandlerContext) {
	inviterUserID := int64(0)
	result := uint32(0)
	fightMode := uint32(2) // 1=单精灵 2=多精灵（客户端 FightWaitPanel / InviteNoteInfo.mode）
	if len(ctx.Body) >= 12 {
		inviterUserID = int64(binary.BigEndian.Uint32(ctx.Body[0:4]))
		result = binary.BigEndian.Uint32(ctx.Body[4:8])
		fightMode = binary.BigEndian.Uint32(ctx.Body[8:12])
	}
	responder := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	nick := responder.Nick
	if nick == "" {
		nick = fmt.Sprintf("用户%d", ctx.UserID)
	}
	bodyAck := make([]byte, 4)
	binary.BigEndian.PutUint32(bodyAck[0:4], 0)
	ctx.GameServer.SendResponse(ctx.ClientData, 2403, ctx.UserID, ctx.SeqID, bodyAck)

	inviterClient := ctx.GameServer.GetClientByUserID(inviterUserID)
	if inviterClient == nil {
		return
	}
	noteBody := make([]byte, 4+16+4)
	binary.BigEndian.PutUint32(noteBody[0:4], uint32(ctx.UserID))
	putFixedString(noteBody, 4, nick, 16)
	binary.BigEndian.PutUint32(noteBody[20:24], result)
	ctx.GameServer.SendResponse(inviterClient, 2502, ctx.UserID, 0, noteBody)

	// 接受对战后：向双方推送 2503（准备对战）和 2504（开始对战），才能进入对战界面
	if result == 1 {
		inviterClient := ctx.GameServer.GetClientByUserID(inviterUserID)
		responderClient := ctx.ClientData
		if inviterClient != nil && responderClient != nil {
			// 2503：DLL 用 userInfoArray[0]=我方（左）、[1]=对方（右）。我方在前：邀请方收 [邀请方,接受方]、接受方收 [接受方,邀请方]
			// fightMode=2 时 2503 需包含双方全部精灵及其技能，供客户端预加载，否则多精灵切换后使用技能会触发 getAssetsByID 返回 null -> Error #1009
			body2503Inviter, body2503Responder := buildNoteReadyToFightInfoPvPPerClient(ctx.GameServer, inviterUserID, ctx.UserID, fightMode)
			ctx.GameServer.SendResponse(inviterClient, 2503, inviterUserID, 0, body2503Inviter)
			ctx.GameServer.SendResponse(responderClient, 2503, ctx.UserID, 0, body2503Responder)
			// 关键：此版本“核心”会在收到 2504 时立刻 getDefinitionByName(PetFightEntry) 并派发 START_FIGHT。
			// 若 2504 比 2503 的 DLL 加载更早到达，会触发 PetFightEntry 未定义 (Error #1065)。
			// 因此 PvP 的 2504 延后到客户端发 2404(READY_TO_FIGHT) 时再下发（见 handleReadyToFight 的 PvP 分支 buildPvP2504BodyForUser）。
			// PvP 初始化双方 BattleState
			setPvPBattleStates(ctx.GameServer, inviterUserID, ctx.UserID, fightMode)
			inviterPetID, responderPetID := 7, 7
			if u1 := ctx.GameServer.GetOrCreateUser(inviterUserID); len(u1.Pets) > 0 {
				inviterPetID = u1.Pets[0].ID
			}
			if u2 := ctx.GameServer.GetOrCreateUser(ctx.UserID); len(u2.Pets) > 0 {
				responderPetID = u2.Pets[0].ID
			}
			logger.Info(fmt.Sprintf("[2403] PvP 接受: UID=%d(PetID=%d) vs UID=%d(PetID=%d)，mode=%d(1单2多)，已发 2503，等待 2404 再发 2504", inviterUserID, inviterPetID, ctx.UserID, responderPetID, fightMode))
		}
	}
}

// ==================== 巅峰之战 12 匹配（2458/2567） ====================

// handleTopLevelJoin CMD 2458 巅峰之战匹配入口
// 请求体: cupId(4)，目前已知:
// - 19 (0x13): 单精灵积分赛
// - 20 (0x14): 多精灵巅峰对决
// 行为:
// - 先回一个空 2458 包体，让前端进入 FightWait.swf；
// - 然后根据 cupId 判定本次是单精灵还是多精灵模式，并放入对应的匹配队列；
//   若已有一名玩家在等，则立即与之匹配并按 PvP 流程发送 2503，并初始化 BattleState。
func handleTopLevelJoin(ctx *gameserver.HandlerContext) {
	cupId := uint32(0)
	if len(ctx.Body) >= 4 {
		cupId = binary.BigEndian.Uint32(ctx.Body[0:4])
	}

	// 先回本次 2458，请求方才能正常弹出等待面板
	ctx.GameServer.SendResponse(ctx.ClientData, 2458, ctx.UserID, ctx.SeqID, []byte{})

	// 默认视为单精灵模式；根据已知前端约定，cupId=20 代表多精灵巅峰对决
	var mode uint32 = topLevelModeSingle
	if cupId == 20 {
		mode = topLevelModeMulti
	}

	matchTopLevel(ctx.GameServer, ctx.UserID, cupId, mode)
}

// handleTopFightBeyond CMD 2567 巅峰之战·巅峰对决匹配入口
// 协议体与 2458 相同，视为多精灵模式。
func handleTopFightBeyond(ctx *gameserver.HandlerContext) {
	cupId := uint32(0)
	if len(ctx.Body) >= 4 {
		cupId = binary.BigEndian.Uint32(ctx.Body[0:4])
	}

	ctx.GameServer.SendResponse(ctx.ClientData, 2567, ctx.UserID, ctx.SeqID, []byte{})

	matchTopLevel(ctx.GameServer, ctx.UserID, cupId, topLevelModeMulti)
}

// matchTopLevel 巅峰之战匹配逻辑
// mode: 1=单精灵(巅峰单) 2=多精灵/巅峰对决
func matchTopLevel(gs *gameserver.GameServer, uid int64, cupId uint32, mode uint32) {
	if uid <= 0 {
		return
	}

	topLevelMu.Lock()
	var waitingPtr *int64
	if mode == topLevelModeSingle {
		waitingPtr = &waitingTopLevelSingle
	} else {
		waitingPtr = &waitingTopLevelMulti
	}

	var opponentUID int64
	if *waitingPtr == 0 {
		// 当前无人排队：把自己放入队列
		*waitingPtr = uid
		topLevelMu.Unlock()
		logger.Info(fmt.Sprintf("[2458/2567] UID=%d cupId=%d 进入巅峰之战匹配队列 mode=%d", uid, cupId, mode))
		return
	}

	// 队列已有一人：与之配成一对
	opponentUID = *waitingPtr
	*waitingPtr = 0
	topLevelMu.Unlock()

	if opponentUID == uid {
		// 理论上不应出现，同 UID 直接丢弃
		logger.Warning(fmt.Sprintf("[2458/2567] 巅峰之战匹配出现同 UID=%d，忽略本次匹配", uid))
		return
	}

	client1 := gs.GetClientByUserID(uid)
	client2 := gs.GetClientByUserID(opponentUID)
	if client1 == nil || client2 == nil {
		logger.Warning(fmt.Sprintf("[2458/2567] 巅峰之战匹配失败：一方离线 UID1=%d UID2=%d", uid, opponentUID))
		return
	}

	// 2503：准备对战——复用现有 PvP 构造逻辑，确保 userInfoArray[0] 是“自己”
	body2503ForUID, body2503ForOpp := buildNoteReadyToFightInfoPvPPerClient(gs, uid, opponentUID, mode)
	gs.SendResponse(client1, 2503, uid, 0, body2503ForUID)
	gs.SendResponse(client2, 2503, opponentUID, 0, body2503ForOpp)

	// 关键：与 2403 流程保持一致，这里不直接发 2504，而是只初始化 BattleState，
	// 让客户端在收到 2503 并加载 DLL 后主动发 2404(READY_TO_FIGHT)，由 handleReadyToFight 再下发 2504。
	setPvPBattleStates(gs, uid, opponentUID, mode)

	logger.Info(fmt.Sprintf("[2458/2567] 巅峰之战匹配成功: UID1=%d vs UID2=%d cupId=%d mode=%d，已发送 2503 并初始化 PvP BattleState，等待 2404 下发 2504",
		uid, opponentUID, cupId, mode))
}

// handleInviteFightCancel CMD 2402 INVITE_FIGHT_CANCEL 取消匹配/邀请
// 太空站精灵王之战与 PetWar 等模式会在关闭等待面板或超时时发送该命令。
// 这里主要用于从精灵王之战匹配队列中移除玩家，并回一个简单的 4 字节 0。
func handleInviteFightCancel(ctx *gameserver.HandlerContext) {
	gs := ctx.GameServer
	gs.PetKingQueueMu.Lock()
	for mode, uid := range gs.PetKingQueue {
		if uid == ctx.UserID {
			delete(gs.PetKingQueue, mode)
		}
	}
	gs.PetKingQueueMu.Unlock()

	resp := make([]byte, 4)
	// 对齐 Lua 写法：result=0
	binary.BigEndian.PutUint32(resp[0:4], 0)
	gs.SendResponse(ctx.ClientData, 2402, ctx.UserID, ctx.SeqID, resp)
	logger.Info(fmt.Sprintf("[2402] 取消匹配/邀请: UID=%d", ctx.UserID))
}

// getBattlePets 返回当前对战使用的精灵列表：精灵大乱斗时为 GrandMeleePets，否则为背包
func getBattlePets(gs *gameserver.GameServer, uid int64) []userdb.Pet {
	if gs.GrandMeleePets != nil && gs.GrandMeleePets[uid] != nil {
		return gs.GrandMeleePets[uid]
	}
	u := gs.GetOrCreateUser(uid)
	return u.Pets
}

// markPetKingBattle 为双方的 BattleState 标记本场为精灵王之战，用于统计胜场数。
// 仅在 2413 PET_KING_JOIN 匹配成功后调用，不影响其他 PvP 模式。
func markPetKingBattle(gs *gameserver.GameServer, uid1, uid2 int64) {
	gs.BattleMu.Lock()
	defer gs.BattleMu.Unlock()
	if b := gs.BattleStates[uid1]; b != nil {
		b.IsPetKing = true
	}
	if b := gs.BattleStates[uid2]; b != nil {
		b.IsPetKing = true
	}
}

// setPvPBattleStates 为 PvP 双方初始化 BattleState，使 2405 有状态、战斗能正常结算并发送 2506
// fightMode: 1=单精灵(不允许换宠) 2=多精灵(允许换宠)
func setPvPBattleStates(gs *gameserver.GameServer, inviterUID, responderUID int64, fightMode uint32) {
	petMgr := gamepets.GetInstance()
	pets1 := getBattlePets(gs, inviterUID)
	pets2 := getBattlePets(gs, responderUID)
	petID1, lv1, hp1, maxHp1 := 7, 5, 0, 0
	if len(pets1) > 0 {
		petID1 = pets1[0].ID
		if pets1[0].Level > 0 {
			lv1 = pets1[0].Level
		}
		ev := gamepets.EVStats{}
		ev = pets1[0].GetEVStats()
		st := petMgr.GetStats(petID1, lv1, 31, ev, 0)
		hp1, maxHp1 = st.HP, st.MaxHP
	} else {
		st := petMgr.GetStats(petID1, lv1, 31, gamepets.EVStats{}, 0)
		hp1, maxHp1 = st.HP, st.MaxHP
	}
	petID2, lv2, hp2, maxHp2 := 7, 5, 0, 0
	if len(pets2) > 0 {
		petID2 = pets2[0].ID
		if pets2[0].Level > 0 {
			lv2 = pets2[0].Level
		}
		ev := gamepets.EVStats{}
		ev = pets2[0].GetEVStats()
		st := petMgr.GetStats(petID2, lv2, 31, ev, 0)
		hp2, maxHp2 = st.HP, st.MaxHP
	} else {
		st := petMgr.GetStats(petID2, lv2, 31, gamepets.EVStats{}, 0)
		hp2, maxHp2 = st.HP, st.MaxHP
	}
	if maxHp1 <= 0 {
		maxHp1 = 1
	}
	if maxHp2 <= 0 {
		maxHp2 = 1
	}
	gs.BattleMu.Lock()
	defer gs.BattleMu.Unlock()
	total1 := len(pets1)
	total2 := len(pets2)
	if fightMode == 1 {
		total1 = 1
		total2 = 1
	}
	isGrandMelee := gs.GrandMeleePets != nil && gs.GrandMeleePets[inviterUID] != nil
	b1 := &gameserver.BattleState{
		PlayerHP:        uint32(hp1),
		PlayerMaxHP:     uint32(maxHp1),
		EnemyHP:         uint32(hp2),
		EnemyMaxHP:      uint32(maxHp2),
		EnemyID:         petID2,
		EnemyLevel:      lv2,
		TotalPlayerPets: total1,
		DeadPlayerPets:  0,
		TotalEnemyPets:  total2,
		DeadEnemyPets:   0,
		IsActive:        true,
		OpponentUserID:  responderUID,
		ActivePetIndex:  0,
		IsGrandMelee:    isGrandMelee,
	}
	b2 := &gameserver.BattleState{
		PlayerHP:        uint32(hp2),
		PlayerMaxHP:     uint32(maxHp2),
		EnemyHP:         uint32(hp1),
		EnemyMaxHP:      uint32(maxHp1),
		EnemyID:         petID1,
		EnemyLevel:      lv1,
		TotalPlayerPets: total2,
		DeadPlayerPets:  0,
		TotalEnemyPets:  total1,
		DeadEnemyPets:   0,
		IsActive:        true,
		OpponentUserID:  inviterUID,
		ActivePetIndex:  0,
		IsGrandMelee:    isGrandMelee,
	}
	if len(pets1) > 0 {
		b1.ActivePetCatchTime = pets1[0].CatchTime
		fillBattlePlayerSkills(b1, &pets1[0])
	}
	if len(pets2) > 0 {
		b2.ActivePetCatchTime = pets2[0].CatchTime
		fillBattlePlayerSkills(b2, &pets2[0])
	}
	gs.BattleStates[inviterUID] = b1
	gs.BattleStates[responderUID] = b2
}

// buildNoteReadyToFightInfoPvP 构建 PvP 的 2503 包体：userCount(4) + [FighetUserInfo(20) + petCount(4) + SimplePetInfo(76)*petCount]，与 NoteReadyToFightInfo 解析一致
// fightMode=2 时需包含双方全部精灵及其技能，供客户端 SkillAssetsManager 预加载，否则多精灵切换后使用技能会触发 Error #1009
func buildNoteReadyToFightInfoPvP(gs *gameserver.GameServer, inviterUID, responderUID int64, fightMode uint32) []byte {
	petMgr := gamepets.GetInstance()
	skillMgr := gameskills.GetInstance()
	buildFightUserInfo := func(uid uint32, nick string) []byte {
		b := make([]byte, 20)
		binary.BigEndian.PutUint32(b[0:4], uid)
		nb := []byte(nick)
		if len(nb) > 16 {
			nb = nb[:16]
		}
		copy(b[4:20], nb)
		return b
	}
	buildSimplePetInfo := func(petID uint32, level uint32, hp uint32, maxHp uint32, catchTime uint32, skills [][2]uint32, shiny uint32) []byte {
		b := make([]byte, 76)
		binary.BigEndian.PutUint32(b[0:4], petID)
		binary.BigEndian.PutUint32(b[4:8], level)
		binary.BigEndian.PutUint32(b[8:12], hp)
		binary.BigEndian.PutUint32(b[12:16], maxHp)
		valid := uint32(0)
		for _, s := range skills {
			if s[0] != 0 {
				valid++
			}
		}
		binary.BigEndian.PutUint32(b[16:20], valid)
		off := 20
		for i := 0; i < 4; i++ {
			var sid, pp uint32
			if i < len(skills) {
				sid, pp = skills[i][0], skills[i][1]
			}
			binary.BigEndian.PutUint32(b[off:off+4], sid)
			binary.BigEndian.PutUint32(b[off+4:off+8], pp)
			off += 8
		}
		binary.BigEndian.PutUint32(b[52:56], catchTime)
		binary.BigEndian.PutUint32(b[56:60], 0)
		binary.BigEndian.PutUint32(b[60:64], 0)
		binary.BigEndian.PutUint32(b[64:68], petID)   // skinID 使用精灵 ID，避免对方蓝格/不显示
		binary.BigEndian.PutUint32(b[68:72], petID)   // 兼容旧客户端：若按 72 字节解析，最后一格为 skinID=petID
		binary.BigEndian.PutUint32(b[72:76], shiny)   // 我方/对方真实异色
		return b
	}
	getPetInfo := func(p *userdb.Pet) (petID int, level int, catchTime uint32, hp, maxHp int, skills [][2]uint32, shiny uint32) {
		petID = 7
		level = 5
		catchTime = 0x69686700 + uint32(7)
		ev := gamepets.EVStats{}
		nature := 0
		if p != nil {
			petID = p.ID
			if p.Level > 0 {
				level = p.Level
			}
			catchTime = uint32(p.CatchTime)
			nature = p.Nature
			ev = p.GetEVStats()
			if p.Shiny != 0 {
				shiny = 1
			}
			if catchTime == 0 {
				catchTime = 0x69686700 + uint32(petID)
			}
		}
		stats := petMgr.GetStats(petID, level, 31, ev, nature)
		hp, maxHp = stats.HP, stats.MaxHP
		raw := petMgr.GetSkillsForLevel(petID, level)
		for _, sid := range raw {
			if sid <= 0 {
				continue
			}
			pp := uint32(20)
			if sk := skillMgr.Get(sid); sk != nil {
				if sk.PP > 0 {
					pp = uint32(sk.PP)
				} else if sk.MaxPP > 0 {
					pp = uint32(sk.MaxPP)
				}
			}
			skills = append(skills, [2]uint32{uint32(sid), pp})
			if len(skills) >= 4 {
				break
			}
		}
		if len(skills) == 0 {
			skills = append(skills, [2]uint32{10001, 20})
		}
		return
	}
	appendUserPets := func(out []byte, uid int64) ([]byte, int) {
		u := gs.GetOrCreateUser(uid)
		nick := u.Nick
		if nick == "" {
			nick = fmt.Sprintf("用户%d", uid)
		}
		out = append(out, buildFightUserInfo(uint32(uid), nick)...)
		pets := getBattlePets(gs, uid)
		petCount := 1
		if fightMode == 2 && len(pets) > 0 {
			petCount = len(pets)
		}
		tmp4 := make([]byte, 4)
		binary.BigEndian.PutUint32(tmp4, uint32(petCount))
		out = append(out, tmp4...)
		for i := 0; i < petCount; i++ {
			var p *userdb.Pet
			if i < len(pets) {
				p = &pets[i]
			}
			petID, lv, ct, hp, maxHp, sk, shiny := getPetInfo(p)
			out = append(out, buildSimplePetInfo(uint32(petID), uint32(lv), uint32(hp), uint32(maxHp), ct, sk, shiny)...)
		}
		return out, petCount
	}
	tmp4 := make([]byte, 4)
	binary.BigEndian.PutUint32(tmp4, 2)
	out := append([]byte(nil), tmp4...)
	out, _ = appendUserPets(out, inviterUID)
	out, _ = appendUserPets(out, responderUID)
	return out
}

// buildNoteReadyToFightInfoPvPPerClient 构建 PvP 的 2503 包体，按接收方分别返回：邀请方收 [邀请方,接受方]，接受方收 [接受方,邀请方]
// 保证各方 userInfoArray[0]=自己，客户端用其区分“我方/对方”并加载对应精灵模型（petArray/skinID），避免双方都显示同一模型
func buildNoteReadyToFightInfoPvPPerClient(gs *gameserver.GameServer, inviterUID, responderUID int64, fightMode uint32) (inviterBody, responderBody []byte) {
	bodyInviterFirst := buildNoteReadyToFightInfoPvP(gs, inviterUID, responderUID, fightMode) // [邀请方, 接受方]
	// 块结构：FighterUserInfo(20) + petCount(4) + SimplePetInfo(76)*petCount
	const simplePetSize = 76
	const headerSize = 20 + 4
	if len(bodyInviterFirst) < 4+headerSize*2 {
		return bodyInviterFirst, bodyInviterFirst
	}
	petCount1 := int(binary.BigEndian.Uint32(bodyInviterFirst[4+20 : 4+24]))
	block1Size := headerSize + simplePetSize*petCount1
	if len(bodyInviterFirst) < 4+block1Size+headerSize {
		return bodyInviterFirst, bodyInviterFirst
	}
	petCount2 := int(binary.BigEndian.Uint32(bodyInviterFirst[4+block1Size+20 : 4+block1Size+24]))
	block2Size := headerSize + simplePetSize*petCount2
	if len(bodyInviterFirst) < 4+block1Size+block2Size {
		return bodyInviterFirst, bodyInviterFirst
	}
	inviterBody = bodyInviterFirst
	responderBody = make([]byte, len(bodyInviterFirst))
	copy(responderBody[0:4], bodyInviterFirst[0:4])
	copy(responderBody[4:4+block2Size], bodyInviterFirst[4+block1Size:4+block1Size+block2Size])
	copy(responderBody[4+block2Size:4+block2Size+block1Size], bodyInviterFirst[4:4+block1Size])
	// 日志：验证 2503 userInfoArray 顺序，确保各方 userInfoArray[0]=自己
	logger.Info(fmt.Sprintf("[2503-PvP] 邀请方(UID=%d)包: userInfoArray[0] userID=%d, userInfoArray[1] userID=%d",
		inviterUID,
		binary.BigEndian.Uint32(inviterBody[4:8]), binary.BigEndian.Uint32(inviterBody[4+block1Size:4+block1Size+4])))
	logger.Info(fmt.Sprintf("[2503-PvP] 接受方(UID=%d)包: userInfoArray[0] userID=%d, userInfoArray[1] userID=%d",
		responderUID,
		binary.BigEndian.Uint32(responderBody[4:8]), binary.BigEndian.Uint32(responderBody[4+block2Size:4+block2Size+4])))
	return inviterBody, responderBody
}

// buildNoteStartFightPvP 构建 PvP 的 2504 包体：isCanAuto(4) + FightPetInfo(50)*2；首条为当前客户端的“我方”精灵，与 FightStartInfo 解析一致
// 返回 (inviter 的 2504 body, responder 的 2504 body)
func buildNoteStartFightPvP(gs *gameserver.GameServer, inviterUID, responderUID int64) (inviterBody, responderBody []byte) {
	petMgr := gamepets.GetInstance()
	// FightPetInfo 固定 50 字节，与客户端 FightPetInfo.as 解析一致：userID(4)+petID(4)+petName(16)+catchTime(4)+hp(4)+maxHP(4)+lv(4)+catchable(4)+battleLv(6)
	const fightPetInfoSize = 50
	buildFightPetInfo := func(uid uint32, petID int, name string, ct uint32, hp, maxHp, lv int, catchable uint32) []byte {
		if petID <= 0 {
			petID = 7
		}
		if maxHp <= 0 {
			maxHp = 1
		}
		if hp < 0 {
			hp = 0
		}
		if hp > maxHp {
			hp = maxHp
		}
		buf := make([]byte, fightPetInfoSize)
		binary.BigEndian.PutUint32(buf[0:4], uid)
		binary.BigEndian.PutUint32(buf[4:8], uint32(petID))
		nb := []byte(name)
		if len(nb) > 16 {
			nb = nb[:16]
		}
		copy(buf[8:24], nb) // 未写满的字节保持为 0
		binary.BigEndian.PutUint32(buf[24:28], ct)
		binary.BigEndian.PutUint32(buf[28:32], uint32(hp))
		binary.BigEndian.PutUint32(buf[32:36], uint32(maxHp))
		binary.BigEndian.PutUint32(buf[36:40], uint32(lv))
		binary.BigEndian.PutUint32(buf[40:44], catchable)
		for i := 44; i < 50; i++ {
			buf[i] = 0
		}
		return buf
	}
	u1 := gs.GetOrCreateUser(inviterUID)
	u2 := gs.GetOrCreateUser(responderUID)
	petID1, lv1, ct1, hp1, maxHp1 := 7, 5, uint32(0), 0, 0
	if len(u1.Pets) > 0 {
		petID1 = u1.Pets[0].ID
		if u1.Pets[0].Level > 0 {
			lv1 = u1.Pets[0].Level
		}
		ct1 = uint32(u1.Pets[0].CatchTime)
		if ct1 == 0 {
			ct1 = 0x69686700 + uint32(petID1)
		}
	}
	ev1 := gamepets.EVStats{}
	if len(u1.Pets) > 0 {
		ev1 = u1.Pets[0].GetEVStats()
	}
	st1 := petMgr.GetStats(petID1, lv1, 31, ev1, 0)
	hp1, maxHp1 = st1.HP, st1.MaxHP
	name1 := petMgr.GetName(petID1)
	if name1 == "" {
		name1 = "精灵"
	}
	petID2, lv2, ct2, hp2, maxHp2 := 7, 5, uint32(0), 0, 0
	if len(u2.Pets) > 0 {
		petID2 = u2.Pets[0].ID
		if u2.Pets[0].Level > 0 {
			lv2 = u2.Pets[0].Level
		}
		ct2 = uint32(u2.Pets[0].CatchTime)
		if ct2 == 0 {
			ct2 = 0x69686700 + uint32(petID2)
		}
	}
	ev2 := gamepets.EVStats{}
	if len(u2.Pets) > 0 {
		ev2 = u2.Pets[0].GetEVStats()
	}
	st2 := petMgr.GetStats(petID2, lv2, 31, ev2, 0)
	hp2, maxHp2 = st2.HP, st2.MaxHP
	name2 := petMgr.GetName(petID2)
	if name2 == "" {
		name2 = "精灵"
	}
	info1 := buildFightPetInfo(uint32(inviterUID), petID1, name1, ct1, hp1, maxHp1, lv1, 0)
	info2 := buildFightPetInfo(uint32(responderUID), petID2, name2, ct2, hp2, maxHp2, lv2, 0)
	// 客户端 FightStartInfo 按首条 userID==MainManager.actorInfo.userID 区分 myInfo/otherInfo；包内顺序 [我方,对方]
	inviterBody = make([]byte, 4+len(info1)+len(info2))
	binary.BigEndian.PutUint32(inviterBody[0:4], 0)
	copy(inviterBody[4:4+len(info1)], info1)
	copy(inviterBody[4+len(info1):], info2)
	responderBody = make([]byte, 4+len(info1)+len(info2))
	binary.BigEndian.PutUint32(responderBody[0:4], 0)
	copy(responderBody[4:4+len(info2)], info2)
	copy(responderBody[4+len(info2):], info1)
	// 日志：验证两段 FightPetInfo 的 userID/petID 不同，便于排查“两边都显示对方模型”
	// 注意：客户端 FightStartInfo 用 actorInfo.userID 区分 myInfo/otherInfo，FighterModeFactory 用 actorID 区分 PlayerMode/EnemyMode
	// 若两者不一致，可能导致 myInfo 被误判为 EnemyMode，出现"两边都显示对方模型"
	inviterFirstUID := binary.BigEndian.Uint32(inviterBody[4:8])
	inviterFirstPetID := binary.BigEndian.Uint32(inviterBody[8:12])
	inviterSecondUID := binary.BigEndian.Uint32(inviterBody[54:58])
	inviterSecondPetID := binary.BigEndian.Uint32(inviterBody[58:62])
	responderFirstUID := binary.BigEndian.Uint32(responderBody[4:8])
	responderFirstPetID := binary.BigEndian.Uint32(responderBody[8:12])
	responderSecondUID := binary.BigEndian.Uint32(responderBody[54:58])
	responderSecondPetID := binary.BigEndian.Uint32(responderBody[58:62])

	// 验证：第一条 userID 必须等于接收方 userID
	if inviterFirstUID != uint32(inviterUID) {
		logger.Warning(fmt.Sprintf("[2504-PvP] ⚠️ 邀请方包第1段 userID(%d) != 接收方UID(%d)，可能导致客户端识别错误！", inviterFirstUID, inviterUID))
	}
	if responderFirstUID != uint32(responderUID) {
		logger.Warning(fmt.Sprintf("[2504-PvP] ⚠️ 接受方包第1段 userID(%d) != 接收方UID(%d)，可能导致客户端识别错误！", responderFirstUID, responderUID))
	}

	logger.Info(fmt.Sprintf("[2504-PvP] 邀请方(UID=%d)包: 第1段 userID=%d petID=%d(name=%s), 第2段 userID=%d petID=%d(name=%s)",
		inviterUID, inviterFirstUID, inviterFirstPetID, name1, inviterSecondUID, inviterSecondPetID, name2))
	logger.Info(fmt.Sprintf("[2504-PvP] 接受方(UID=%d)包: 第1段 userID=%d petID=%d(name=%s), 第2段 userID=%d petID=%d(name=%s)",
		responderUID, responderFirstUID, responderFirstPetID, name2, responderSecondUID, responderSecondPetID, name1))
	return
}

// buildPvP2504BodyForUser 为 PvP 下 2404 构建当前用户的 2504 包体：我方 FightPetInfo + 对方 FightPetInfo（不修改 BattleState）
func buildPvP2504BodyForUser(gs *gameserver.GameServer, myUID, opponentUID int64, battle *gameserver.BattleState) []byte {
	petMgr := gamepets.GetInstance()
	const fightPetInfoSize = 50
	buildFightPetInfo := func(uid uint32, petID int, name string, ct uint32, hp, maxHp, lv int, catchable uint32) []byte {
		if petID <= 0 {
			petID = 7
		}
		if maxHp <= 0 {
			maxHp = 1
		}
		if hp < 0 {
			hp = 0
		}
		if hp > maxHp {
			hp = maxHp
		}
		buf := make([]byte, fightPetInfoSize)
		binary.BigEndian.PutUint32(buf[0:4], uid)
		binary.BigEndian.PutUint32(buf[4:8], uint32(petID))
		nb := []byte(name)
		if len(nb) > 16 {
			nb = nb[:16]
		}
		copy(buf[8:24], nb)
		binary.BigEndian.PutUint32(buf[24:28], ct)
		binary.BigEndian.PutUint32(buf[28:32], uint32(hp))
		binary.BigEndian.PutUint32(buf[32:36], uint32(maxHp))
		binary.BigEndian.PutUint32(buf[36:40], uint32(lv))
		binary.BigEndian.PutUint32(buf[40:44], catchable)
		for i := 44; i < 50; i++ {
			buf[i] = 0
		}
		return buf
	}
	// 我方：当前用户（大乱斗时用分配的精灵列表）
	petsMe := getBattlePets(gs, myUID)
	petID1, lv1, ct1, hp1, maxHp1 := 7, 5, uint32(0), int(battle.PlayerHP), int(battle.PlayerMaxHP)
	if len(petsMe) > 0 {
		idx := 0
		if battle.ActivePetIndex >= 0 && battle.ActivePetIndex < len(petsMe) {
			idx = battle.ActivePetIndex
		}
		petID1 = petsMe[idx].ID
		if petsMe[idx].Level > 0 {
			lv1 = petsMe[idx].Level
		}
		ct1 = uint32(petsMe[idx].CatchTime)
		if ct1 == 0 {
			ct1 = 0x69686700 + uint32(petID1)
		}
	}
	name1 := petMgr.GetName(petID1)
	if name1 == "" {
		name1 = "精灵"
	}
	// 对方：对手（大乱斗时用分配的精灵列表）
	petsOpp := getBattlePets(gs, opponentUID)
	petID2, lv2, ct2 := battle.EnemyID, battle.EnemyLevel, uint32(0)
	hp2, maxHp2 := int(battle.EnemyHP), int(battle.EnemyMaxHP)
	if len(petsOpp) > 0 {
		ct2 = uint32(petsOpp[0].CatchTime)
		if ct2 == 0 {
			ct2 = 0x69686700 + uint32(petID2)
		}
	}
	name2 := petMgr.GetName(petID2)
	if name2 == "" {
		name2 = "精灵"
	}
	info1 := buildFightPetInfo(uint32(myUID), petID1, name1, ct1, hp1, maxHp1, lv1, 0)
	info2 := buildFightPetInfo(uint32(opponentUID), petID2, name2, ct2, hp2, maxHp2, lv2, 0)
	// 与 2403 推送的 2504 一致：包内顺序 [我方,对方]
	out := make([]byte, 4+len(info1)+len(info2))
	binary.BigEndian.PutUint32(out[0:4], 0)
	copy(out[4:4+len(info1)], info1)
	copy(out[4+len(info1):], info2)
	// 确保第1段 userID 始终为接收方(myUID)，第2段为对方(opponentUID)，便于客户端正确识别"我方/对方"
	firstUID := binary.BigEndian.Uint32(out[4:8])
	firstPetID := binary.BigEndian.Uint32(out[8:12])
	secondUID := binary.BigEndian.Uint32(out[54:58])
	secondPetID := binary.BigEndian.Uint32(out[58:62])

	// 验证：第一条 userID 必须等于接收方 userID
	if firstUID != uint32(myUID) {
		logger.Warning(fmt.Sprintf("[2504-2404] ⚠️ 包第1段 userID(%d) != 接收方UID(%d)，可能导致客户端识别错误！", firstUID, myUID))
	}

	logger.Info(fmt.Sprintf("[2504-2404] UID=%d 包体: 第1段 userID=%d petID=%d(name=%s), 第2段 userID=%d petID=%d(name=%s)",
		myUID, firstUID, firstPetID, name1, secondUID, secondPetID, name2))
	return out
}

// ==================== 新手战斗（2411 -> 2503） ====================

// 克洛斯星密林(12) SPT/脚本战斗：客户端 fightWithBoss 第二个参数 0=蘑菇怪(47)、1=依依(83)
const (
	mapIDKlothForest  = 12
	petIDMushroomBoss = 47 // 蘑菇怪 SPTBOSS
	petIDYiyi         = 83 // 依依（密林脚本 NPC）
)

// handleChallengeBoss CMD 2411 挑战BOSS（新手战斗触发 / SPT / 地图脚本 BOSS）
// 客户端 FightInviteManager.fightWithBoss(name, param2)：只发送 param2(4)，不发送名字。
// 全地图 SPT 按 sptboss 配置：(mapID, param2) → bossPetID、等级；未配置时 body 可为 bossID 或默认 13。
func handleChallengeBoss(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	mapID := user.MapID
	if mapID == 0 {
		mapID = 1
	}

	bossID := uint32(13)
	enemyLevel := 5
	isLeiyiAkexiyaSkillTrain := false
	isLeiyiSkillTrain := false

	param2 := uint32(0)
	if len(ctx.Body) >= 4 {
		param2 = binary.BigEndian.Uint32(ctx.Body[0:4])
	}

	if ctx.CmdID == 2421 {
		logger.Info(fmt.Sprintf("[2421] 盖亚挑战请求: mapID=%d param2=%d bodyLen=%d", mapID, param2, len(ctx.Body)))
	} else {
		logger.Info(fmt.Sprintf("[2411] 收到挑战BOSS请求: mapID=%d param2=%d bodyLen=%d", mapID, param2, len(ctx.Body)))
	}

	// 雷伊体能/最终对战特训：FightInviteManager.fightWithBoss("雷伊幻影", 10000+index)
	// index=0..6：
	// - 0..5 分别对应 体力/防御/特防/攻击/特攻/速度
	// - 6 为 水银湖倒影中的雷伊（LeiyiTrainController.fightJiaLeiyi 使用 param2=10006）
	if ctx.CmdID == 2411 && param2 >= 10000 && param2 <= 10006 {
		// 体能特训的 6 项为普通雷伊；最终对战（倒影/镜像）使用专用怪物ID以加载不同模型
		if param2 == 10006 {
			bossID = 5005 // 雷伊的幻影（客户端应加载不同模型）
			enemyLevel = 100
		} else {
			bossID = 70
			enemyLevel = 50
		}
		logger.Info(fmt.Sprintf("[2411] 雷伊特训挑战: mapID=%d param2=%d -> bossID=%d level=%d", mapID, param2, bossID, enemyLevel))
	} else
	// 火山星(15) 自定义 BOSS：扎克斯的分身(5008)
	// 前端通过 CHALLENGE_BOSS(2411) 将 param2=5008 传入；这里直接把 param2 视为 bossPetID，
	// 避免后续 “param2 未命中回退到 0” 导致仍然打到地图默认 BOSS（如 261 盖亚）。
	if mapID == 15 && param2 == 5008 {
		bossID = 5008
		enemyLevel = 150
		logger.Info(fmt.Sprintf("[2411] 火山星自定义BOSS: mapID=%d param2=%d -> bossID=%d level=%d", mapID, param2, bossID, enemyLevel))
	} else if e, ok := sptboss.GetByMapAndParam(mapID, param2); ok {
		// 玄武空间(401)：守护兽必须依次挑战，param2=k 时要求 0..k-1 已击败
		if mapID == sptboss.MapIDXuanWuSpace && param2 >= 1 && param2 <= 5 {
			requiredMask := uint32((1 << param2) - 1)
			if user.DefeatedXuanWuGuardianMask&requiredMask != requiredMask {
				logger.Info(fmt.Sprintf("[2411] 玄武空间：须依次击败守护兽，param2=%d 要求 mask&%d==%d，当前 mask=%d UID=%d", param2, requiredMask, requiredMask, user.DefeatedXuanWuGuardianMask, ctx.UserID))
				overBody := buildFightOverInfo(4, 0)
				ctx.GameServer.SendResponse(ctx.ClientData, 2506, ctx.UserID, ctx.SeqID, overBody)
				return
			}
		}
		// 玄武空间(401)：必须先击败六守护兽(param2=0~5)才能挑战玄武真身(param2=6)
		if mapID == sptboss.MapIDXuanWuSpace && param2 == 6 && user.DefeatedXuanWuGuardianMask != sptboss.XuanWuGuardianMask {
			logger.Info(fmt.Sprintf("[2411] 玄武空间：未击败全部六守护兽(mask=%d)，拒绝挑战玄武真身 UID=%d", user.DefeatedXuanWuGuardianMask, ctx.UserID))
			overBody := buildFightOverInfo(4, 0)
			ctx.GameServer.SendResponse(ctx.ClientData, 2506, ctx.UserID, ctx.SeqID, overBody)
			return
		}
		// 谱尼封印(514/108/500)：封印必须一层层依次开启，param2=k 时要求上一层封印(k-1)已首次击败
		// 这里沿用 DefeatedSPTBossIds 中的 30000+region 记录，每个封印/真身首次击败时都会写入。
		if sptboss.IsPuniSealBoss(mapID, param2) && param2 >= 2 && param2 <= 7 {
			requiredRegion := int(param2 - 1)
			requiredKey := 30000 + requiredRegion
			hasPrevSeal := false
			for _, id := range user.DefeatedSPTBossIds {
				if id == requiredKey {
					hasPrevSeal = true
					break
				}
			}
			if !hasPrevSeal {
				logger.Info(fmt.Sprintf("[2411] 谱尼封印：须依次击败封印，param2=%d 要求先击败 region=%d (key=%d)，当前 DefeatedSPTBossIds=%v UID=%d",
					param2, requiredRegion, requiredKey, user.DefeatedSPTBossIds, ctx.UserID))
				overBody := buildFightOverInfo(4, 0)
				ctx.GameServer.SendResponse(ctx.ClientData, 2506, ctx.UserID, ctx.SeqID, overBody)
				return
			}
		}
		// 青龙空间(403)：守护兽必须依次挑战，param2=k 时要求 0..k-1 已击败
		if mapID == sptboss.MapIDQingLongSpace && param2 >= 1 && param2 <= 3 {
			requiredMask := uint32((1 << param2) - 1)
			if user.DefeatedQingLongGuardianMask&requiredMask != requiredMask {
				logger.Info(fmt.Sprintf("[2411] 青龙空间：须依次击败守护兽，param2=%d 要求 mask&%d==%d，当前 mask=%d UID=%d", param2, requiredMask, requiredMask, user.DefeatedQingLongGuardianMask, ctx.UserID))
				overBody := buildFightOverInfo(4, 0)
				ctx.GameServer.SendResponse(ctx.ClientData, 2506, ctx.UserID, ctx.SeqID, overBody)
				return
			}
		}
		// 青龙空间(403)：必须先击败四守护兽(param2=0~3)才能挑战朵拉格(param2=4)
		if mapID == sptboss.MapIDQingLongSpace && param2 == 4 && user.DefeatedQingLongGuardianMask != sptboss.QingLongGuardianMask {
			logger.Info(fmt.Sprintf("[2411] 青龙空间：未击败全部四守护兽(mask=%d)，拒绝挑战朵拉格 UID=%d", user.DefeatedQingLongGuardianMask, ctx.UserID))
			overBody := buildFightOverInfo(4, 0)
			ctx.GameServer.SendResponse(ctx.ClientData, 2506, ctx.UserID, ctx.SeqID, overBody)
			return
		}
		bossID = uint32(e.BossPetID)
		enemyLevel = e.Level
		logger.Info(fmt.Sprintf("[2411] 从SPT配置获取BOSS: mapID=%d param2=%d -> bossID=%d level=%d", mapID, param2, bossID, enemyLevel))
	} else if e, ok := sptboss.GetByMapAndParam(mapID, 0); ok {
		// 该地图在 SPT 中但 param2 不匹配（如客户端误传 param2=1）：按单 BOSS 地图回退到 param2=0，避免把 param2 当 bossID 导致变成布布种子等
		bossID = uint32(e.BossPetID)
		enemyLevel = e.Level
		logger.Info(fmt.Sprintf("[2411] SPT param2 未命中，回退到 mapID=%d param2=0 -> bossID=%d level=%d", mapID, bossID, enemyLevel))
	} else {
		logger.Info(fmt.Sprintf("[2411] SPT配置未找到: mapID=%d param2=%d，尝试从body或盖亚回退", mapID, param2))
		// 调试辅助：用于排查“已配置但运行时未生效/未重启”的情况
		if mapID == 486 {
			if e0, ok0 := sptboss.GetByMapAndParam(486, 0); ok0 {
				logger.Info(fmt.Sprintf("[2411][debug] mapBossConfig 已包含 486/0: bossID=%d level=%d", e0.BossPetID, e0.Level))
			} else {
				logger.Warning("[2411][debug] mapBossConfig 未包含 486/0（请确认已重启并运行最新编译的 gameserver）")
			}
		}
		if len(ctx.Body) >= 4 {
			// 兼容：body 直接传 bossID（如从 SPT 面板发起时）
			if v := binary.BigEndian.Uint32(ctx.Body[0:4]); v != 0 {
				bossID = v
				enemyLevel = 50
				logger.Info(fmt.Sprintf("[2411] 从body读取BOSS ID: bossID=%d", bossID))
			}
		}
		// 2421(FIGHT_SPECIAL_PET) 为盖亚挑战：未匹配到地图时仍按盖亚处理，保证点击「我接受对战」能进战
		if ctx.CmdID == 2421 && bossID == 13 {
			bossID = petIDGaiya
			enemyLevel = 70
			logger.Info(fmt.Sprintf("[2421] 盖亚挑战回退: bossID=%d level=%d", bossID, enemyLevel))
		}
	}

	// 雷伊技能特训-阿克希亚（任务122 第3步）：
	// 前端在地图40调用 FightInviteManager.fightWithBoss("阿克希亚", 1)，只会发 param2=1。
	// 若任务122处于进行中且已完成前两步，则该战斗应强制“单雷伊出战”，并用于后续 2510 学习 20364 判定。
	if ctx.CmdID == 2411 && mapID == 40 && bossID == 50 && param2 == 1 {
		if user.Tasks != nil {
			if t, ok := user.Tasks["122"]; ok && t.Status == "1" && t.Buf != nil {
				// buf[0]=提亚斯 buf[1]=雷纳多 buf[2]=阿克希亚 buf[3]=倒影雷伊
				if t.Buf[0] != 0 && t.Buf[1] != 0 && t.Buf[2] == 0 {
					isLeiyiAkexiyaSkillTrain = true
					// 等级不强制改动（官服可能是65），这里只负责规则与包体一致性
					logger.Info(fmt.Sprintf("[2411] 雷伊技能特训(阿克希亚): mapID=%d param2=%d -> bossID=%d level=%d (force single leiyi)", mapID, param2, bossID, enemyLevel))
				}
			}
		}
	}

	// 雷伊技能特训（里奥斯/提亚斯/雷纳多）：仅允许单雷伊出战
	// 前端均使用 FightInviteManager.fightWithBoss(xxx, 1) 触发，因此以 (mapID,param2,bossID) 识别。
	if ctx.CmdID == 2411 && param2 == 1 {
		if (mapID == 17 && bossID == 42) || (mapID == 27 && bossID == 69) || (mapID == 49 && bossID == 113) {
			isLeiyiSkillTrain = true
		}
	}

	// 写入 BattleStates，供 2503/2504 使用正确的敌方等级
	// 谱尼(300) 需 BattleMapID=514，否则 IsPuniSealBoss 等逻辑不生效（user.MapID 常为 108/500 别名）
	battleMapID := mapID
	if int(bossID) == sptboss.PetIDPuni {
		battleMapID = sptboss.MapIDPuniTower
	}
	ctx.GameServer.BattleMu.Lock()
	battle, ok := ctx.GameServer.BattleStates[ctx.UserID]
	if !ok || battle == nil {
		battle = &gameserver.BattleState{}
	}
	battle.EnemyID = int(bossID)
	battle.EnemyLevel = enemyLevel
	battle.EnemyShiny = 0 // SPT BOSS 显示为普通（不异色）
	battle.EnemyDisplayName = ""
	if mapID == 461 {
		battle.EnemyDisplayName = "二当家"
	}
	if param2 >= 10000 && param2 <= 10006 {
		battle.EnemyDisplayName = "雷伊幻影"
	}
	if isLeiyiAkexiyaSkillTrain {
		battle.EnemyDisplayName = "阿克希亚"
	}
	battle.IsActive = true
	battle.BattleMapID = battleMapID
	battle.BossRegion = param2
	battle.RoundCount = 0
	battle.LastHitWasCrit = false
	battle.PlayerUsedSkills = nil
	// 首发精灵 HP 为 0 时跳过，使用第一个有血精灵出战
	activeIdx := getFirstAvailablePetIndex(user)
	// 雷伊特训（含最终对战）：强制用雷伊出战（没有雷伊或雷伊0血则拒绝进入）
	if param2 >= 10000 && param2 <= 10006 || isLeiyiAkexiyaSkillTrain || isLeiyiSkillTrain {
		leiyiIdx := -1
		for i := range user.Pets {
			if user.Pets[i].ID == 70 {
				// 允许 CurrentHP=0 表示“满血未初始化”，因此只要不是明确 0 且 MaxHP>0 才视为不可用；
				// 这里直接复用 getPetEffectiveHP 判定是否可用。
				petMgr := gamepets.GetInstance()
				st := petMgr.GetStats(user.Pets[i].ID, user.Pets[i].Level, user.Pets[i].DV, user.Pets[i].GetEVStats(), user.Pets[i].Nature)
				if getPetEffectiveHP(&user.Pets[i], st.MaxHP) > 0 {
					leiyiIdx = i
					break
				}
			}
		}
		if leiyiIdx < 0 {
			ctx.GameServer.BattleMu.Unlock()
			if isLeiyiAkexiyaSkillTrain {
				logger.Info(fmt.Sprintf("[2411] 雷伊技能特训(阿克希亚)：无可用雷伊，拒绝进入战斗 UID=%d", ctx.UserID))
			} else {
				logger.Info(fmt.Sprintf("[2411] 雷伊特训：无可用雷伊，拒绝进入战斗 UID=%d", ctx.UserID))
			}
			overBody := buildFightOverInfo(4, 0) // reason=REASON_ERROR
			ctx.GameServer.SendResponse(ctx.ClientData, 2506, ctx.UserID, ctx.SeqID, overBody)
			return
		}
		activeIdx = leiyiIdx
	}
	if activeIdx < 0 {
		ctx.GameServer.BattleMu.Unlock()
		logger.Info(fmt.Sprintf("[2411] 无可用精灵（无精灵或全部0血），拒绝进入战斗: UID=%d", ctx.UserID))
		overBody := buildFightOverInfo(4, 0) // reason=REASON_ERROR
		ctx.GameServer.SendResponse(ctx.ClientData, 2506, ctx.UserID, ctx.SeqID, overBody)
		return
	}
	battle.ActivePetIndex = activeIdx
	if activeIdx >= 0 && activeIdx < len(user.Pets) {
		battle.ActivePetCatchTime = user.Pets[activeIdx].CatchTime
	}
	ctx.GameServer.BattleStates[ctx.UserID] = battle
	ctx.GameServer.BattleMu.Unlock()

	body := buildNoteReadyToFightInfo(ctx, bossID)
	logger.Info(fmt.Sprintf("[2411] 即将发送 2503 准备对战 bossID=%d", bossID))
	// 2421：用请求的 SEQ 回 2503，便于客户端把 2503 当作本次请求的响应并进入对战
	ctx.GameServer.SendResponse(ctx.ClientData, 2503, ctx.UserID, ctx.SeqID, body)
}

// buildNoteReadyToFightInfo 构建 2503 回包（最小可用版本）
// body: userCount(4) + [FighetUserInfo(20) + petCount(4) + SimplePetInfo] * 2
// 客户端用 2503 的 petArray（来自 SimplePetInfo 的 petId）预加载对战模型，petID 必须与 2504 一致且非 0
func buildNoteReadyToFightInfo(ctx *gameserver.HandlerContext, enemyID uint32) []byte {
	if enemyID == 0 {
		enemyID = 13
	}
	userID := uint32(ctx.UserID)
	petMgr := gamepets.GetInstance()
	skillMgr := gameskills.GetInstance()

	// 玩家使用第一个有血精灵出战，与 2504 一致；2411 已写入 ActivePetIndex
	playerPetID := 7
	playerLevel := 5
	playerCatch := uint32(0x69686700 + uint32(playerPetID))
	playerNature := 0
	playerEV := gamepets.EVStats{}
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	activeIdx := 0
	ctx.GameServer.BattleMu.RLock()
	if b, ok := ctx.GameServer.BattleStates[ctx.UserID]; ok && b.ActivePetIndex >= 0 && b.ActivePetIndex < len(user.Pets) {
		activeIdx = b.ActivePetIndex
	}
	ctx.GameServer.BattleMu.RUnlock()
	if len(user.Pets) > 0 {
		p := &user.Pets[activeIdx]
		playerPetID = p.ID
		if p.Level > 0 {
			playerLevel = p.Level
		}
		playerCatch = uint32(p.CatchTime)
		playerNature = p.Nature
		playerEV = p.GetEVStats()
	}
	if playerCatch == 0 {
		playerCatch = 0x69686700 + uint32(playerPetID)
	}
	logger.Info(fmt.Sprintf("[2503] NoteReadyToFight: playerPetID=%d playerLevel=%d enemyID=%d (与2504一致才能正确显示对战)", playerPetID, playerLevel, enemyID))

	playerStats := petMgr.GetStats(playerPetID, playerLevel, 31, playerEV, playerNature)
	if len(user.Pets) > 0 {
		playerStats = applyPetStatBonus(playerStats, &user.Pets[activeIdx])
	}
	playerSkillsRaw := petMgr.GetSkillsForLevel(playerPetID, playerLevel)
	playerSkills := make([][2]uint32, 0, 4)
	for _, sid := range playerSkillsRaw {
		if sid <= 0 {
			continue
		}
		pp := uint32(20)
		if sk := skillMgr.Get(sid); sk != nil {
			if sk.PP > 0 {
				pp = uint32(sk.PP)
			} else if sk.MaxPP > 0 {
				pp = uint32(sk.MaxPP)
			}
		}
		playerSkills = append(playerSkills, [2]uint32{uint32(sid), pp})
		if len(playerSkills) >= 4 {
			break
		}
	}
	if len(playerSkills) == 0 {
		playerSkills = append(playerSkills, [2]uint32{10001, 20})
	}

	// 敌人属性（敌人默认性格0，EV 为 0）；2411 已写入 BattleStates.EnemyLevel（如密林蘑菇怪10级、依依5级）
	enemyLevel := 5
	enemyShiny := uint32(0)
	ctx.GameServer.BattleMu.RLock()
	var battleForBoss *gameserver.BattleState
	if b, ok := ctx.GameServer.BattleStates[ctx.UserID]; ok && b.IsActive {
		// 即使 EnemyLevel 尚未写入，也需要 battleForBoss 用于“单精灵对战/多Boss预加载”等判定。
		battleForBoss = b
		if b.EnemyLevel > 0 {
			enemyLevel = b.EnemyLevel
		}
		if b.EnemyShiny != 0 {
			enemyShiny = 1
		}
	}
	ctx.GameServer.BattleMu.RUnlock()
	enemyEV := gamepets.EVStats{} // 敌人 EV 默认为 0
	// 扎克斯分身(5008)：固定等级 150
	if int(enemyID) == 5008 && enemyLevel < 150 {
		enemyLevel = 150
	}
	enemyStats := petMgr.GetStats(int(enemyID), enemyLevel, 15, enemyEV, 0)
	// 地图 BOSS 使用固定血量（只影响敌方战斗体力，不改种族值与玩家拥有的同名精灵）
	enemyStats.HP = applyBossHPOverride(int(enemyID), enemyStats.HP, battleForBoss)
	enemyStats.MaxHP = applyBossHPOverride(int(enemyID), enemyStats.MaxHP, battleForBoss)
	// 扎克斯分身(5008)：固定血量 10000，确保 2503 展示与实际战斗一致
	if int(enemyID) == 5008 {
		enemyStats.HP = 10000
		enemyStats.MaxHP = 10000
	}
	// 谱尼厉害形态（未全破封印时点击真身）：展示用血量同样使用 65000，以便与实际战斗保持一致。
	if battleForBoss != nil && int(enemyID) == sptboss.PetIDPuni &&
		battleForBoss.BattleMapID == sptboss.MapIDPuniTower &&
		battleForBoss.BossRegion == sptboss.PuniTrueForm && battleForBoss.PuniTotalLives == 0 {
		enemyStats.HP = sptboss.PuniSuperHP
		enemyStats.MaxHP = sptboss.PuniSuperHP
	}
	// 使用 getEnemySkillsForPet 以支持 BOSS 技能覆盖（如闪光波克尔的"全力一击"）
	enemySkillsRaw := getEnemySkillsForPet(int(enemyID), enemyLevel)
	enemySkills := make([][2]uint32, 0, 4)
	enemyPPInfinite := sptboss.IsInfinitePPBoss(int(enemyID)) ||
		(battleForBoss != nil && int(enemyID) == sptboss.PetIDPuni && sptboss.PuniHasInfinitePP(battleForBoss.BossRegion))
	for _, sid := range enemySkillsRaw {
		if sid <= 0 {
			continue
		}
		pp := uint32(20)
		if enemyPPInfinite {
			pp = 99
		} else if sk := skillMgr.Get(sid); sk != nil {
			if sk.PP > 0 {
				pp = uint32(sk.PP)
			} else if sk.MaxPP > 0 {
				pp = uint32(sk.MaxPP)
			}
		}
		enemySkills = append(enemySkills, [2]uint32{uint32(sid), pp})
		if len(enemySkills) >= 4 {
			break
		}
	}
	if len(enemySkills) == 0 {
		defaultPP := uint32(20)
		if enemyPPInfinite {
			defaultPP = 99
		}
		enemySkills = append(enemySkills, [2]uint32{10001, defaultPP})
	}

	buildFightUserInfo := func(uid uint32, nick string) []byte {
		b := make([]byte, 20)
		binary.BigEndian.PutUint32(b[0:4], uid)
		nb := []byte(nick)
		if len(nb) > 16 {
			nb = nb[:16]
		}
		copy(b[4:20], nb)
		return b
	}

	buildSimplePetInfo := func(petID uint32, level uint32, hp uint32, maxHp uint32, catchTime uint32, skills [][2]uint32, shiny uint32) []byte {
		// 与前端 PetInfo(param1,false) 解析一致：...skinID(4)+shiny(4)
		b := make([]byte, 76)
		binary.BigEndian.PutUint32(b[0:4], petID)
		binary.BigEndian.PutUint32(b[4:8], level)
		binary.BigEndian.PutUint32(b[8:12], hp)
		binary.BigEndian.PutUint32(b[12:16], maxHp)
		valid := uint32(0)
		for _, s := range skills {
			if s[0] != 0 {
				valid++
			}
		}
		binary.BigEndian.PutUint32(b[16:20], valid)
		off := 20
		for i := 0; i < 4; i++ {
			var sid, pp uint32
			if i < len(skills) {
				sid, pp = skills[i][0], skills[i][1]
			}
			binary.BigEndian.PutUint32(b[off:off+4], sid)
			binary.BigEndian.PutUint32(b[off+4:off+8], pp)
			off += 8
		}
		binary.BigEndian.PutUint32(b[52:56], catchTime)
		binary.BigEndian.PutUint32(b[56:60], 0)
		binary.BigEndian.PutUint32(b[60:64], 0)
		binary.BigEndian.PutUint32(b[64:68], 0)
		binary.BigEndian.PutUint32(b[68:72], 0)   // skinID
		binary.BigEndian.PutUint32(b[72:76], shiny) // 我方/敌方真实异色
		return b
	}

	// 拼装 body
	// CRITICAL FIX: 发送玩家所有精灵的信息，而不仅仅是第一只
	// 这样 _petInfoMap 中会包含所有精灵，切换时不会出现空指针异常
	// 盖亚(261)对战：仅允许单精灵出战，只发一只
	playerPetCount := len(user.Pets)
	// 雷伊体能/最终对战特训：仅允许单精灵出战（首发雷伊），只发一只
	isLeiyiTrain := battleForBoss != nil && battleForBoss.BossRegion >= 10000 && battleForBoss.BossRegion <= 10006
	// 雷伊技能特训（里奥斯/提亚斯/雷纳多）：同样仅允许单雷伊出战
	isLeiyiSkillTrain := battleForBoss != nil && battleForBoss.BossRegion == 1 &&
		((battleForBoss.BattleMapID == 17 && battleForBoss.EnemyID == 42) ||
			(battleForBoss.BattleMapID == 27 && battleForBoss.EnemyID == 69) ||
			(battleForBoss.BattleMapID == 49 && battleForBoss.EnemyID == 113))
	// 雷伊技能特训-阿克希亚：同样仅允许单雷伊出战
	isLeiyiAkexiyaSkillTrain := battleForBoss != nil && battleForBoss.BattleMapID == 40 && battleForBoss.BossRegion == 1 && battleForBoss.EnemyID == 50
	if enemyID == petIDGaiya || isLeiyiTrain || isLeiyiSkillTrain || isLeiyiAkexiyaSkillTrain {
		playerPetCount = 1
	}
	if playerPetCount == 0 {
		playerPetCount = 1 // 至少发送一只默认精灵
	}
	out := make([]byte, 0, 4+20+4+76*playerPetCount+20+4+76)
	tmp4 := make([]byte, 4)
	binary.BigEndian.PutUint32(tmp4, 2)
	out = append(out, tmp4...)

	// Player - 发送所有精灵
	out = append(out, buildFightUserInfo(userID, "Seer")...)
	binary.BigEndian.PutUint32(tmp4, uint32(playerPetCount))
	out = append(out, tmp4...)

	if len(user.Pets) > 0 {
		// 首发精灵放第一位，客户端用第一个作为出场精灵；order 为 user.Pets 的索引
		order := make([]int, 0, len(user.Pets))
		if activeIdx >= 0 && activeIdx < len(user.Pets) {
			order = append(order, activeIdx)
		}
		if enemyID != petIDGaiya && !isLeiyiTrain && !isLeiyiSkillTrain && !isLeiyiAkexiyaSkillTrain {
			for i := 0; i < len(user.Pets); i++ {
				if i != activeIdx {
					order = append(order, i)
				}
			}
		}
		for _, idx := range order {
			if idx >= len(user.Pets) {
				continue
			}
			pet := &user.Pets[idx]
			petID := pet.ID
			if petID <= 0 {
				petID = 7
			}
			petLevel := pet.Level
			if petLevel <= 0 {
				petLevel = 5
			}
			petCatch := uint32(pet.CatchTime)
			if petCatch == 0 {
				petCatch = 0x69686700 + uint32(petID)
			}
			petShiny := uint32(0)
			if pet.Shiny != 0 {
				petShiny = 1
			}
			petEV := pet.GetEVStats()
			petStats := petMgr.GetStats(petID, petLevel, pet.DV, petEV, pet.Nature)
			petStats = applyPetStatBonus(petStats, pet)

			// 获取精灵技能
			var petSkillsRaw []int
			if len(pet.Skills) > 0 {
				petSkillsRaw = pet.Skills
			} else {
				petSkillsRaw = petMgr.GetSkillsForLevel(petID, petLevel)
			}
			petSkills := make([][2]uint32, 0, 4)
			for idx, sid := range petSkillsRaw {
				if sid <= 0 {
					continue
				}
				maxPP := uint32(20)
				if sk := skillMgr.Get(sid); sk != nil {
					if sk.MaxPP > 0 {
						maxPP = uint32(sk.MaxPP)
					} else if sk.PP > 0 {
						maxPP = uint32(sk.PP)
					}
				}
				pp := maxPP
				if len(pet.SkillPP) > idx && pet.SkillPP[idx] >= 0 {
					p := uint32(pet.SkillPP[idx])
					if p > maxPP {
						p = maxPP
					}
					pp = p
				}
				petSkills = append(petSkills, [2]uint32{uint32(sid), pp})
				if len(petSkills) >= 4 {
					break
				}
			}
			if len(petSkills) == 0 {
				petSkills = append(petSkills, [2]uint32{10001, 20})
			}

			effectiveHP := getPetEffectiveHP(pet, petStats.MaxHP)
			out = append(out, buildSimplePetInfo(uint32(petID), uint32(petLevel), uint32(effectiveHP), uint32(petStats.MaxHP), petCatch, petSkills, petShiny)...)
			logger.Info(fmt.Sprintf("[2503] 我方精灵: PetID=%d CatchTime=%d Shiny=%d (0=普通 1=异色)", petID, petCatch, petShiny))
		}
	} else {
		// 没有精灵时发送默认精灵
		playerShiny := uint32(0)
		out = append(out, buildSimplePetInfo(uint32(playerPetID), uint32(playerLevel), uint32(playerStats.HP), uint32(playerStats.MaxHP), playerCatch, playerSkills, playerShiny)...)
	}

	// Enemy；谱尼封印/真身显示 ?? 级（传 0）
	enemyDisplayLv := uint32(enemyLevel)
	if battleForBoss != nil && sptboss.PuniNeedsDisplayLevelZero(battleForBoss.BattleMapID, battleForBoss.BossRegion) {
		enemyDisplayLv = uint32(sptboss.PuniDisplayLv)
	}
	enemyNick := ""
	if battleForBoss != nil && battleForBoss.EnemyDisplayName != "" {
		enemyNick = battleForBoss.EnemyDisplayName
	}
	out = append(out, buildFightUserInfo(0, enemyNick)...)
	// 玄武/青龙空间守护兽、勇者之塔多精灵层：2503 敌方需包含全部 petID，供客户端 PetAssetsManager 预加载，否则 2407 换宠时 getAssetsByID 取不到模型
	enemyPetCount := uint32(1)
	guardianPetIDs := ([]int)(nil)
	guardianHP := 0
	braveTowerBossIDs := ([]int)(nil)
	if battleForBoss != nil && sptboss.IsXuanWuGuardianBoss(battleForBoss.BattleMapID, battleForBoss.BossRegion) {
		enemyPetCount = uint32(len(sptboss.XuanWuGuardianPetIDs))
		guardianPetIDs = sptboss.XuanWuGuardianPetIDs
		guardianHP = sptboss.XuanWuGuardianHP
	} else if battleForBoss != nil && sptboss.IsQingLongGuardianBoss(battleForBoss.BattleMapID, battleForBoss.BossRegion) {
		enemyPetCount = uint32(len(sptboss.QingLongGuardianPetIDs))
		guardianPetIDs = sptboss.QingLongGuardianPetIDs
		guardianHP = sptboss.QingLongGuardianHP
	} else if battleForBoss != nil && battleForBoss.BattleMapID == 500 && len(battleForBoss.BraveTowerBossIDs) > 0 {
		enemyPetCount = uint32(len(battleForBoss.BraveTowerBossIDs))
		braveTowerBossIDs = battleForBoss.BraveTowerBossIDs
	}
	binary.BigEndian.PutUint32(tmp4, enemyPetCount)
	out = append(out, tmp4...)
	if guardianPetIDs != nil {
		for i, gid := range guardianPetIDs {
			geLv := uint32(enemyLevel)
			geHP := uint32(guardianHP)
			geSkills := enemySkills
			if uint32(gid) != enemyID {
				geSkills = make([][2]uint32, 0, 4)
				for _, sk := range getEnemySkillsForPet(gid, int(geLv)) {
					if sk > 0 {
						geSkills = append(geSkills, [2]uint32{uint32(sk), 20})
					}
				}
				if len(geSkills) == 0 {
					geSkills = append(geSkills, [2]uint32{10001, 20})
				}
			} else {
				geHP = uint32(enemyStats.HP)
			}
			out = append(out, buildSimplePetInfo(uint32(gid), geLv, geHP, uint32(guardianHP), 0, geSkills, enemyShiny)...)
			if i == 0 {
				logger.Info(fmt.Sprintf("[2503] 守护兽: 预加载全部%d只 petIDs=%v", len(guardianPetIDs), guardianPetIDs))
			}
		}
	} else if braveTowerBossIDs != nil {
		for i, gid := range braveTowerBossIDs {
			geLv := uint32(enemyLevel)
			geStats := petMgr.GetStats(gid, int(geLv), 15, enemyEV, 0)
			geHP := uint32(geStats.HP)
			geMaxHP := uint32(geStats.MaxHP)
			geSkills := enemySkills
			if gid != int(enemyID) {
				geSkills = make([][2]uint32, 0, 4)
				for _, sk := range getEnemySkillsForPet(gid, int(geLv)) {
					if sk > 0 {
						geSkills = append(geSkills, [2]uint32{uint32(sk), 20})
					}
				}
				if len(geSkills) == 0 {
					geSkills = append(geSkills, [2]uint32{10001, 20})
				}
			} else {
				geHP = uint32(enemyStats.HP)
				geMaxHP = uint32(enemyStats.MaxHP)
			}
			out = append(out, buildSimplePetInfo(uint32(gid), geLv, geHP, geMaxHP, 0, geSkills, enemyShiny)...)
			if i == 0 {
				logger.Info(fmt.Sprintf("[2503] 勇者之塔: 预加载全部%d只 petIDs=%v", len(braveTowerBossIDs), braveTowerBossIDs))
			}
		}
	} else {
		out = append(out, buildSimplePetInfo(enemyID, enemyDisplayLv, uint32(enemyStats.HP), uint32(enemyStats.MaxHP), 0, enemySkills, enemyShiny)...)
	}

	logger.Info(fmt.Sprintf("[2503] ========== 发送 2503 准备对战 玩家精灵数=%d 敌人ID=%d ==========", playerPetCount, enemyID))
	return out
}

// ==================== READY_TO_FIGHT (2404) ====================

// handleReadyToFight CMD 2404 战斗初始化
// 对齐 Lua: fight_handlers.handleReadyToFight
// PvP：若已有 BattleState 且 OpponentUserID != 0，仅回 2504（我方/对方顺序）+ 2301，不覆盖状态，否则 2505/2506 无法发给对方。
// PvE：在服务端记录战斗状态，向客户端发送 2504 + 2301。
func handleReadyToFight(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)

	// PvP 已由 2403 推送 2503+2504 并 setPvPBattleStates；客户端仍会发 2404 确认。此时不可覆盖 BattleState，否则 OpponentUserID 丢失，2505/2506 无法发给对方。
	ctx.GameServer.BattleMu.RLock()
	battle, pvpExists := ctx.GameServer.BattleStates[ctx.UserID]
	if pvpExists && battle.IsActive && battle.OpponentUserID != 0 {
		opponentUID := battle.OpponentUserID
		ctx.GameServer.BattleMu.RUnlock()
		// 仅回 2504（我方第一条、对方第二条）+ 2301，不写 BattleState
		body2504 := buildPvP2504BodyForUser(ctx.GameServer, ctx.UserID, opponentUID, battle)
		if len(body2504) > 0 {
			ctx.GameServer.SendResponse(ctx.ClientData, 2504, ctx.UserID, ctx.SeqID, body2504)
		}
		if len(user.Pets) > 0 {
			petInfoBody := buildFullPetInfo(user.Pets[0])
			ctx.GameServer.SendResponse(ctx.ClientData, 2301, ctx.UserID, ctx.SeqID, petInfoBody)
		}
		logger.Info(fmt.Sprintf("[2404] PvP 已就绪: UID=%d 仅回 2504+2301，不覆盖 BattleState", ctx.UserID))
		return
	}
	ctx.GameServer.BattleMu.RUnlock()

	// 选择玩家出战精灵：使用第一个有血精灵，若无可用精灵则拒绝进入战斗
	activeIdx := getFirstAvailablePetIndex(user)
	if activeIdx < 0 {
		logger.Info(fmt.Sprintf("[2404] 无可用精灵（无精灵或全部0血），拒绝进入战斗: UID=%d", ctx.UserID))
		overBody := buildFightOverInfo(4, 0) // reason=REASON_ERROR
		ctx.GameServer.SendResponse(ctx.ClientData, 2506, ctx.UserID, ctx.SeqID, overBody)
		return
	}
	activePet := &user.Pets[activeIdx]
	playerPetID := activePet.ID
	playerLevel := 5
	if activePet.Level > 0 {
		playerLevel = activePet.Level
	}
	catchTime := uint32(activePet.CatchTime)
	if catchTime == 0 {
		catchTime = 0x69686700 + uint32(playerPetID)
	}

	// 玩家属性
	petMgr := gamepets.GetInstance()
	playerNature := activePet.Nature
	playerEV := activePet.GetEVStats()
	playerStats := petMgr.GetStats(playerPetID, playerLevel, activePet.DV, playerEV, playerNature)
	playerStats = applyPetStatBonus(playerStats, activePet)
	playerHP := getPetEffectiveHP(activePet, playerStats.MaxHP)

	// 敌人 ID/等级：优先取上一次 2411/2408 记录在 BattleStates 里的 EnemyID、BossRegion 等
	bossID := uint32(13)
	enemyLevel := 5
	enemyEV := gamepets.EVStats{} // 敌人 EV 默认为 0
	var currentBossBattle *gameserver.BattleState

	ctx.GameServer.BattleMu.RLock()
	if battle, ok := ctx.GameServer.BattleStates[ctx.UserID]; ok && battle.IsActive && battle.EnemyID > 0 {
		bossID = uint32(battle.EnemyID)
		if battle.EnemyLevel > 0 {
			enemyLevel = battle.EnemyLevel
		}
		currentBossBattle = battle
	}
	ctx.GameServer.BattleMu.RUnlock()

	// 兜底：若客户端未先发 2411/2408（或 BattleState 未写入），在“有地图 BOSS 配置”的地图上自动选择该地图默认 BOSS（param2=0）
	// 典型场景：哈莫雷特等 SPT BOSS 地图，客户端直接发 2404，导致落到默认比比鼠(13)。
	mapID := user.MapID
	if mapID == 0 {
		mapID = 1
	}
	// 试炼之塔（地图 600）：以 CurFreshStage 为准确定当前层 Boss，避免 BattleState 与进度不同步导致对战时仍为上一层
	if mapID == 600 {
		curLevel := user.CurFreshStage
		if curLevel < 1 {
			curLevel = 1
		}
		if curLevel > freshTowerMaxLevel {
			curLevel = freshTowerMaxLevel
		}
		idx := curLevel - 1
		if idx < 0 {
			idx = 0
		}
		bossID = freshTowerBossIDs[idx]
		enemyLevel = 5 + (curLevel-1)/2
		if enemyLevel > 50 {
			enemyLevel = 50
		}
		ctx.GameServer.BattleMu.Lock()
		if b, ok := ctx.GameServer.BattleStates[ctx.UserID]; ok && b != nil {
			b.EnemyID = int(bossID)
			b.EnemyLevel = enemyLevel
			b.BattleMapID = 600
		}
		ctx.GameServer.BattleMu.Unlock()
	} else if bossID == 13 {
		if e, ok := sptboss.GetByMapAndParam(mapID, 0); ok && e.BossPetID > 0 {
			bossID = uint32(e.BossPetID)
			enemyLevel = e.Level
			logger.Info(fmt.Sprintf("[2404] 未找到 BattleState 敌人信息，回退到地图BOSS: mapID=%d param2=0 -> bossID=%d level=%d", mapID, bossID, enemyLevel))
		}
	}

	// 确保敌方 petID 非 0，否则客户端无法加载模型（PetAssetsManager 依赖非零 ID）
	if bossID == 0 {
		bossID = 13
	}
	// 扎克斯分身(5008)：固定等级 150（仅 PvE 敌方）
	if int(bossID) == 5008 && enemyLevel < 150 {
		enemyLevel = 150
	}
	enemyStats := petMgr.GetStats(int(bossID), enemyLevel, 15, enemyEV, 0)

	// 地图 BOSS 固定血量：只影响敌方战斗体力，不改种族值与玩家拥有的同名精灵（谱尼封印用 currentBossBattle）
	enemyStats.HP = applyBossHPOverride(int(bossID), enemyStats.HP, currentBossBattle)
	enemyStats.MaxHP = applyBossHPOverride(int(bossID), enemyStats.MaxHP, currentBossBattle)

	// 谱尼厉害形态（未全破封印时点击真身）：一命 65000 血，死后无限复活
	isPuniTrueForm := int(bossID) == sptboss.PetIDPuni &&
		currentBossBattle != nil &&
		currentBossBattle.BattleMapID == sptboss.MapIDPuniTower &&
		currentBossBattle.BossRegion == sptboss.PuniTrueForm
	isSuperPuni := isPuniTrueForm && !hasClearedAllPuniSeals(user)
	if isSuperPuni {
		enemyStats.HP = sptboss.PuniSuperHP
		enemyStats.MaxHP = sptboss.PuniSuperHP
	}

	// 特殊：克洛斯星 BOSS 闪光波克尔（mapID=10, petID=166）固定血量 2000（沿用原逻辑）
	if int(bossID) == 166 && user.MapID == 10 {
		enemyStats.HP = 2000
		enemyStats.MaxHP = 2000
	}
	// 扎克斯分身(5008)：固定血量 10000
	if int(bossID) == 5008 {
		enemyStats.HP = 10000
		enemyStats.MaxHP = 10000
	}

	// SPTBOSS 开局自带强化：攻击等级 +2（盖亚与其他 BOSS 一致；盖亚伤害翻倍在 2405 敌方伤害计算处单独乘 2）
	// 这里同时写入 2504 的 battleLv 和 BattleState.EnemyBattleLv，保证客户端显示与服务端计算一致。
	initialEnemyBattleLv := [6]int8{}
	if _, ok := sptboss.GetByPetID(int(bossID)); ok || int(bossID) == 589 {
		initialEnemyBattleLv[gameskills.StatAttack] = 2
	}
	// 扎克斯分身(5008)：天生攻击 +2
	if int(bossID) == 5008 {
		initialEnemyBattleLv[gameskills.StatAttack] = 2
	}
	// 暗黑第十一门三只 BOSS：按你的要求定制天生强化
	// 11-1 萨洛奇斯：工具（这里按防御处理）+2
	// 11-2 查迪斯：攻击+2
	// 11-3 奈尼迪亚：工具（这里按防御处理）+2
	if mapID == 513 {
		switch int(bossID) {
		case 1403: // 萨洛奇斯
			initialEnemyBattleLv = [6]int8{}
			initialEnemyBattleLv[gameskills.StatDefence] = 2
		case 1397: // 查迪斯
			initialEnemyBattleLv = [6]int8{}
			initialEnemyBattleLv[gameskills.StatAttack] = 2
		case 1400: // 奈尼迪亚
			initialEnemyBattleLv = [6]int8{}
			initialEnemyBattleLv[gameskills.StatDefence] = 2
		}
	}

	// FightPetInfo: userID(4)+petID(4)+petName(16)+catchTime(4)+hp(4)+maxHP(4)+lv(4)+catchable(4)+battleLv(6)
	// 客户端用 petID 从 PetAssetsManager.getAssetsByID(petID) 取对战模型，petID 必须与 2503 中一致且非 0
	buildFightPetInfo := func(uid uint32, petID int, name string, ct uint32, hp, maxHP, lv int, catchable uint32, battleLv [6]int8) []byte {
		if petID <= 0 {
			petID = 7
		}
		if maxHP <= 0 {
			maxHP = 1
		}
		if hp < 0 {
			hp = 0
		}
		if hp > maxHP {
			hp = maxHP
		}
		buf := make([]byte, 4+4+16+4+4+4+4+4+6)
		off := 0
		binary.BigEndian.PutUint32(buf[off:off+4], uid)
		off += 4
		binary.BigEndian.PutUint32(buf[off:off+4], uint32(petID))
		off += 4
		nb := []byte(name)
		if len(nb) > 16 {
			nb = nb[:16]
		}
		copy(buf[off:off+16], nb)
		off += 16
		binary.BigEndian.PutUint32(buf[off:off+4], ct)
		off += 4
		binary.BigEndian.PutUint32(buf[off:off+4], uint32(hp))
		off += 4
		binary.BigEndian.PutUint32(buf[off:off+4], uint32(maxHP))
		off += 4
		binary.BigEndian.PutUint32(buf[off:off+4], uint32(lv))
		off += 4
		binary.BigEndian.PutUint32(buf[off:off+4], catchable)
		off += 4
		for i := 0; i < 6; i++ {
			buf[off+i] = byte(uint8(battleLv[i]))
		}
		off += 6
		return buf[:off]
	}

	// 使用 gamepets 中的精灵中文名，便于客户端显示；客户端也会用 petID 加载模型
	playerName := petMgr.GetName(playerPetID)
	if playerName == "" {
		playerName = "精灵"
	}
	enemyName := petMgr.GetName(int(bossID))
	if enemyName == "" {
		enemyName = "野生精灵"
	}

	// 捕捉标记：
	// - SPT BOSS 不可捕捉
	// - 进化形态（EvolvesFrom>0）作为野生出现时不可捕捉，只允许第一形态/单形态精灵被捕捉。
	enemyCatchable := uint32(1)
	if _, ok := sptboss.GetByPetID(int(bossID)); ok {
		enemyCatchable = 0
	} else {
		if def := petMgr.Get(int(bossID)); def != nil && def.EvolvesFrom > 0 {
			enemyCatchable = 0
		}
	}
	// 扎克斯分身(5008)：不可捕捉
	if int(bossID) == 5008 {
		enemyCatchable = 0
	}
	// 谱尼封印/真身显示 ?? 级（lv=0）
	enemyDisplayLevel := enemyLevel
	if currentBossBattle != nil && sptboss.PuniNeedsDisplayLevelZero(currentBossBattle.BattleMapID, currentBossBattle.BossRegion) {
		enemyDisplayLevel = sptboss.PuniDisplayLv
	}
	// 扎克斯分身(5008)：显示等级固定 150
	if int(bossID) == 5008 {
		enemyDisplayLevel = 150
	}
	playerInfo := buildFightPetInfo(uint32(ctx.UserID), playerPetID, playerName, catchTime, playerHP, playerStats.MaxHP, playerLevel, 0, [6]int8{})
	enemyInfo := buildFightPetInfo(0, int(bossID), enemyName, 0, enemyStats.HP, enemyStats.MaxHP, enemyDisplayLevel, enemyCatchable, initialEnemyBattleLv)

	body := make([]byte, 4+len(playerInfo)+len(enemyInfo))
	binary.BigEndian.PutUint32(body[0:4], 0) // isCanAuto = 0
	copy(body[4:4+len(playerInfo)], playerInfo)
	copy(body[4+len(playerInfo):], enemyInfo)

	ctx.GameServer.SendResponse(ctx.ClientData, 2504, ctx.UserID, ctx.SeqID, body)

	// 初始化战斗状态（盖亚等需记录 BattleMapID 用于战斗结束精元判定；mapID 已在上文设定）
	// 单精灵对战：仅允许单精灵出战，且我方精灵被击败应立刻结束战斗（不进入换宠流程）
	totalPlayerPets := len(user.Pets)
	if bossID == petIDGaiya {
		totalPlayerPets = 1
	}
	// 雷伊体能/最终对战特训（2411 param2=10000..10006）：单精灵（雷伊）
	if currentBossBattle != nil && currentBossBattle.BossRegion >= 10000 && currentBossBattle.BossRegion <= 10006 {
		totalPlayerPets = 1
	}
	// 雷伊技能特训（里奥斯/提亚斯/雷纳多/阿克希亚）：单精灵（雷伊）
	if currentBossBattle != nil && currentBossBattle.BossRegion == 1 {
		if (currentBossBattle.BattleMapID == 17 && currentBossBattle.EnemyID == 42) ||
			(currentBossBattle.BattleMapID == 27 && currentBossBattle.EnemyID == 69) ||
			(currentBossBattle.BattleMapID == 49 && currentBossBattle.EnemyID == 113) ||
			(currentBossBattle.BattleMapID == 40 && currentBossBattle.EnemyID == 50) {
			totalPlayerPets = 1
		}
	}
	// 谱尼(300) 需 BattleMapID=514 才能正确应用封印/真身特性与血量
	battleMapID := mapID
	if int(bossID) == sptboss.PetIDPuni {
		battleMapID = sptboss.MapIDPuniTower
	}
	ctx.GameServer.BattleMu.Lock()
	existingBattle := ctx.GameServer.BattleStates[ctx.UserID]
	// 试炼之塔（地图 600）、勇者之塔（地图 500）为本地子图，user.MapID 常为父图；2415/2429 写入 BattleMapID，此处需保留
	if existingBattle != nil && (existingBattle.BattleMapID == 600 || existingBattle.BattleMapID == 500) {
		battleMapID = existingBattle.BattleMapID
	}
	battleState := &gameserver.BattleState{
		EnemyHP:             uint32(enemyStats.HP),
		EnemyMaxHP:          uint32(enemyStats.MaxHP),
		PlayerHP:            uint32(playerHP),
		PlayerMaxHP:         uint32(playerStats.MaxHP),
		EnemyID:             int(bossID),
		EnemyLevel:          enemyLevel,
		ActivePetIndex:      activeIdx,
		TotalPlayerPets:     totalPlayerPets,
		DeadPlayerPets:      0,
		IsActive:            true,
		EnemyBattleLv:       initialEnemyBattleLv,
		BattleMapID:         battleMapID,
		BossRegion:             0,
		PuniTotalLives:         0,
		PuniCurrentLife:        0,
		PuniTrueFormSuppressed: false,
		RoundCount:          0,
		LastHitWasCrit:      false,
	}
	// 扎克斯分身(5008)：免疫异常 + 所有攻击必中（仅 PvE 敌方）
	if battleState.OpponentUserID == 0 && battleState.EnemyID == 5008 {
		battleState.EnemyStatusImmuneRounds = 255
		battleState.EnemyMustHitRounds = 255
		// 先制 +6：每回合必定先手
		battleState.EnemyFirstPriorityRounds = 255
	}
	// 哈默雷特(216) 敌方技能 PP：进入战斗按技能最大 PP 初始化；用尽则不出招
	if sptboss.IsHaMoLeiTeOrderBoss(battleState.EnemyID) {
		fillBattleEnemySkills(battleState)
	}
	if currentBossBattle != nil {
		battleState.BossRegion = currentBossBattle.BossRegion
		if int(bossID) == sptboss.PetIDPuni && battleState.BattleMapID == sptboss.MapIDPuniTower {
			totalLives := sptboss.PuniLivesForRegion(battleState.BossRegion)
			// 只有在已通过七大封印（region 1~7 全部首次击败）后，点击真身才启用 6 命真身规则；
			// 否则视为“厉害谱尼”一命无限复活形态（PuniTotalLives=0）。
			if battleState.BossRegion == sptboss.PuniTrueForm && totalLives == 6 && !hasClearedAllPuniSeals(user) {
				totalLives = 0
			}
			battleState.PuniTotalLives = totalLives
			if battleState.PuniTotalLives > 0 {
				battleState.PuniCurrentLife = 1
			}
			// 真身每命血量不同：在 2404 初始化时直接覆盖 EnemyHP/EnemyMaxHP
			if battleState.BossRegion == sptboss.PuniTrueForm {
				if battleState.PuniTotalLives == 6 && battleState.PuniCurrentLife == 1 {
					maxHP := sptboss.PuniTrueFormLifeMaxHP(1)
					if maxHP > 0 {
						battleState.EnemyHP = uint32(maxHP)
						battleState.EnemyMaxHP = uint32(maxHP)
					}
				} else if battleState.PuniTotalLives == 0 {
					// 厉害谱尼形态：一命 65000 血，但可无限复活
					battleState.EnemyHP = uint32(sptboss.PuniSuperHP)
					battleState.EnemyMaxHP = uint32(sptboss.PuniSuperHP)
				}
			}
		}
		// 地图 677 终极试炼(region=6)：5 条命，复用 PuniTotalLives/PuniCurrentLife
		if battleState.BattleMapID == sptboss.MapIDYiNengWang && sptboss.IsYiNengUltimateBoss(battleState.BattleMapID, battleState.BossRegion) && int(bossID) == 1000 {
			battleState.PuniTotalLives = sptboss.YiNengUltimateLives
			battleState.PuniCurrentLife = 1
			maxHP := sptboss.YiNengUltimateLifeMaxHP(1)
			if maxHP > 0 {
				battleState.EnemyHP = uint32(maxHP)
				battleState.EnemyMaxHP = uint32(maxHP)
			}
		}
	}
	// 谱尼真身：首命沿用 SPT 统一的“攻击+2”开局，其余命切换时会在 2405 中清空 EnemyBattleLv。
	// 保留 2408 设置的野生战斗标记与异色，否则击败野怪不会加学习力、捕捉到的异色会变成普通
	if existingBattle != nil && existingBattle.IsWildMonster {
		battleState.IsWildMonster = true
		battleState.EnemyShiny = existingBattle.EnemyShiny
	}
	// 勇者之塔（地图 500）多精灵层：保留 2415 写入的 BraveTowerBossIDs/BraveTowerBossIndex，供 2405 击败后切换下一只
	if existingBattle != nil && existingBattle.BattleMapID == 500 && len(existingBattle.BraveTowerBossIDs) > 0 {
		battleState.BraveTowerBossIDs = existingBattle.BraveTowerBossIDs
		battleState.BraveTowerBossIndex = existingBattle.BraveTowerBossIndex
	}
	if len(user.Pets) > 0 {
		fillBattlePlayerSkills(battleState, activePet)
	}
	// 若首发精灵就是尼尔(77)，则在进入战斗时就视为“已见尼尔”，避免仅使用胶囊也被误判为未登场。
	if activeIdx >= 0 && activeIdx < len(user.Pets) && user.Pets[activeIdx].ID == 77 {
		battleState.HasSeenNeal = true
	}
	ctx.GameServer.BattleStates[ctx.UserID] = battleState
	ctx.GameServer.BattleMu.Unlock()
	logger.Info(fmt.Sprintf("[2404] 战斗状态初始化: PlayerHP=%d/%d EnemyHP=%d/%d EnemyID=%d EnemyLevel=%d ActivePetIndex=%d",
		battleState.PlayerHP, battleState.PlayerMaxHP, battleState.EnemyHP, battleState.EnemyMaxHP, battleState.EnemyID, battleState.EnemyLevel, activeIdx))

	// 对齐 Lua：确保客户端 PetManager 有当前精灵信息（技能面板依赖）
	// 发送完整宠物信息（2301），这样客户端才能显示技能列表
	if len(user.Pets) > 0 {
		petInfoBody := buildFullPetInfo(*activePet)
		ctx.GameServer.SendResponse(ctx.ClientData, 2301, ctx.UserID, ctx.SeqID, petInfoBody)
	}
}

// ==================== FIGHT_NPC_MONSTER (2408) ====================

// handleFightNpcMonster CMD 2408 地图野怪战斗
// 对齐 Lua: fight_handlers.handleFightNpcMonster
// 请求体: monsterSlot(4) — 槽位 0~8，客户端点哪只怪就发哪一格
// 行为: 根据当前地图与槽位，选出一个怪物ID，然后调用 buildNoteReadyToFightInfo 回推 2503。
func handleFightNpcMonster(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	petMgr := gamepets.GetInstance()

	// 解析请求中的槽位索引
	var slotIndex int
	if len(ctx.Body) >= 4 {
		slotIndex = int(binary.BigEndian.Uint32(ctx.Body[0:4]))
	}

	mapID := user.MapID
	if mapID == 0 {
		mapID = 1
	}

	// 与 2004 一致：只用该玩家当前地图的槽位（进图/定时刷新维护），避免用全图缓存导致“点了 A 变成 B”
	slots := ctx.GameServer.GetPlayerOgreSlots(ctx.UserID, mapID)
	if slots == nil || len(slots) == 0 {
		slots = gameogres.GenerateNewSlotsNoCache(mapID)
		if len(slots) > 0 {
			ctx.GameServer.SetPlayerOgreSlots(ctx.UserID, mapID, slots)
		}
	}
	enemyID := 0
	enemyLevel := 5

	// 2004 包体为 9 格，实际只填前 4 格；若客户端发 4~8，按 4 格取模与显示对齐
	if len(slots) > 0 && (slotIndex < 0 || slotIndex >= len(slots)) {
		if slotIndex >= 4 && slotIndex <= 8 {
			slotIndex = slotIndex % 4
		} else if slotIndex < 0 {
			slotIndex = 0
		} else {
			slotIndex = slotIndex % len(slots)
		}
	}
	// 若仍越界或该槽为空，取第一个有效槽
	if len(slots) > 0 && (slotIndex < 0 || slotIndex >= len(slots) || slots[slotIndex].PetID == 0) {
		for i, s := range slots {
			if s.PetID > 0 {
				slotIndex = i
				break
			}
		}
	}

	// 按槽位取怪物ID与等级
	if slotIndex >= 0 && slotIndex < len(slots) && slots[slotIndex].PetID > 0 {
		enemyID = slots[slotIndex].PetID
		if slots[slotIndex].Level > 0 {
			enemyLevel = slots[slotIndex].Level
		} else if mapID == 461 {
			// 赫鲁卡城(461)：野生精灵等级 = 玩家对战首发精灵等级 - 5，最低 1（首发=第一个未战败的精灵，CurrentHP==-1 表示战败）
			firstLevel := 5
			if len(user.Pets) > 0 {
				for _, p := range user.Pets {
					if p.CurrentHP != -1 {
						if p.Level > 0 {
							firstLevel = p.Level
						}
						break
					}
				}
			}
			enemyLevel = firstLevel - 5
			if enemyLevel < 1 {
				enemyLevel = 1
			}
		}
		logger.Info(fmt.Sprintf("[2408] 使用槽位 %d 的精灵: PetID=%d Level=%d Shiny=%v", slotIndex, enemyID, enemyLevel, slots[slotIndex].Shiny))
	} else {
		// 如果指定槽位无效，找第一个有效的槽位
		for i, s := range slots {
			if s.PetID > 0 {
				enemyID = s.PetID
				if s.Level > 0 {
					enemyLevel = s.Level
				} else if mapID == 461 {
					firstLevel := 5
					if len(user.Pets) > 0 {
						for _, p := range user.Pets {
							if p.CurrentHP != -1 {
								if p.Level > 0 {
									firstLevel = p.Level
								}
								break
							}
						}
					}
					enemyLevel = firstLevel - 5
					if enemyLevel < 1 {
						enemyLevel = 1
					}
				}
				logger.Info(fmt.Sprintf("[2408] 回退到槽位 %d 的精灵: PetID=%d Level=%d", i, enemyID, enemyLevel))
				break
			}
		}
	}

	if enemyID == 0 {
		// 回退到新手 BOSS（比比鼠），避免客户端卡死
		enemyID = 13
		enemyLevel = 5
		logger.Warning(fmt.Sprintf("[2408] 未找到有效精灵，回退到比比鼠: mapID=%d", mapID))
	}

	// 预先在 BattleStates 中记录这次点击对应的敌人ID/等级/异色，
	// 方便后续 2404 READY_TO_FIGHT 与 2503 使用同一个敌人（对齐 Lua: getOgresForMap → FIGHT_NPC_MONSTER）。
	// 需在此处之后考虑尼尔/尼奥替换逻辑，否则 enemyShiny 无法覆盖。
	enemyShiny := uint32(0)
	if slotIndex >= 0 && slotIndex < len(slots) && slots[slotIndex].Shiny {
		enemyShiny = 1
	}

	// 触发尼尔/尼奥特殊遭遇：在进入对战前，将任意野怪替换为尼尔(77)或尼奥(416)。
	// - 稀有精灵(IsRareMon!=0) 与异色精灵(当前槽位 Shiny=true) 不会被替换。
	// - 当前概率：尼尔约 3%，尼奥约 2%（总共 5%）。
	if enemyID != 77 && enemyID != 416 {
		isShinySlot := slotIndex >= 0 && slotIndex < len(slots) && slots[slotIndex].Shiny
		isRare := false
		if def := petMgr.Get(enemyID); def != nil && def.IsRareMon != 0 {
			isRare = true
		}
		if !isShinySlot && !isRare {
			r := rand.Intn(100) // 0-99
			if r < 3 {
				enemyID = 77 // 尼尔 3%
			} else if r < 5 {
				enemyID = 416 // 尼奥 2%
			}
			if enemyID == 77 || enemyID == 416 {
				// 重新设定等级与异色状态：尼尔16-17级，尼奥17-18级，5% 概率异色
				if enemyID == 77 {
					enemyLevel = 16 + rand.Intn(2) // 16-17
				} else {
					enemyLevel = 17 + rand.Intn(2) // 17-18
				}
				if rand.Intn(100) < 5 {
					enemyShiny = 1
				} else {
					enemyShiny = 0
				}
				logger.Info(fmt.Sprintf("[2408] 尼尔/尼奥事件触发: PetID=%d Level=%d Shiny=%d", enemyID, enemyLevel, enemyShiny))
			}
		}
	}
	ctx.GameServer.BattleMu.Lock()
	battle, ok := ctx.GameServer.BattleStates[ctx.UserID]
	if !ok || battle == nil {
		battle = &gameserver.BattleState{}
	}
	battle.EnemyID = enemyID
	battle.EnemyLevel = enemyLevel
	battle.EnemyShiny = enemyShiny
	battle.IsActive = true
	battle.IsWildMonster = true // 野生精灵战斗，击败后给学习力
	battle.BattleMapID = mapID  // 供 2409 判断地图 461 使用 1% 捕捉率
	// 重置尼尔/尼奥状态
	battle.HasSeenNeal = false
	battle.NiaoEscapeCountdown = 0
	ctx.GameServer.BattleStates[ctx.UserID] = battle
	ctx.GameServer.BattleMu.Unlock()

	logger.Info(fmt.Sprintf("[2408] FIGHT_NPC_MONSTER: mapId=%d slot=%d enemyID=%d level=%d 即将发送 2503", mapID, slotIndex, enemyID, enemyLevel))

	body := buildNoteReadyToFightInfo(ctx, uint32(enemyID))
	ctx.GameServer.SendResponse(ctx.ClientData, 2503, ctx.UserID, ctx.SeqID, body)
}

// ==================== ATTACK_BOSS (2412) ====================

// handleAttackBoss CMD 2412 攻击 SPT BOSS（破除防护罩）
// 客户端 BossModel.aimatState 在玩家用火焰喷射器等击中 BOSS 后发送 region(4)。
// 服务端扣减该 BOSS 防护罩血量并推送 2021 更新显示；响应 2412 返回当前 hp(4)。
// 克洛斯星密林(12) region=0 为蘑菇怪，每次命中扣约 25% 满血，hp 归零后可点击进入战斗。
func handleAttackBoss(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	mapID := user.MapID
	if mapID == 0 {
		mapID = 1
	}
	var region uint32
	if len(ctx.Body) >= 4 {
		region = binary.BigEndian.Uint32(ctx.Body[0:4])
	}

	// 仅处理有防护罩的 SPT BOSS（如克洛斯星密林 12 蘑菇怪）
	e, ok := sptboss.GetByMapAndParam(mapID, region)
	if !ok || !e.HasShield {
		ctx.GameServer.SendResponse(ctx.ClientData, 2412, ctx.UserID, ctx.SeqID, make([]byte, 4))
		return
	}

	petMgr := gamepets.GetInstance()
	stats := petMgr.GetStats(e.BossPetID, e.Level, 15, gamepets.EVStats{}, 0)
	maxHP := stats.MaxHP
	currentHP := getBossHp(ctx.UserID, mapID, region)
	if currentHP <= 0 {
		currentHP = maxHP
	}
	// 每次命中扣约 25% 满血，至少扣 1
	damage := maxHP / 4
	if damage <= 0 {
		damage = 1
	}
	newHP := currentHP - damage
	if newHP < 0 {
		newHP = 0
	}
	setBossHp(ctx.UserID, mapID, region, newHP)

	// 响应 2412：返回当前 BOSS 血量(4)，供客户端更新显示
	respBody := make([]byte, 4)
	binary.BigEndian.PutUint32(respBody, uint32(newHP))
	ctx.GameServer.SendResponse(ctx.ClientData, 2412, ctx.UserID, ctx.SeqID, respBody)

	// 推送 2021 更新 BOSS；hp 归零时先移除再重加，客户端才会真正去掉防护罩（FungusBoss 需重新 add 且 show(0) 才不加载 film）
	if newHP == 0 {
		removeBody := buildMapBossRemoveRegion(mapID, region)
		if len(removeBody) > 0 {
			ctx.GameServer.SendResponse(ctx.ClientData, cmdMapBoss, ctx.UserID, 0, removeBody)
		}
	}
	bossBody := buildMapBossList(mapID, uint32(newHP))
	if len(bossBody) > 0 {
		ctx.GameServer.SendResponse(ctx.ClientData, cmdMapBoss, ctx.UserID, 0, bossBody)
		logger.Info(fmt.Sprintf("[2412] ATTACK_BOSS: UID=%d MapID=%d region=%d hp %d->%d", ctx.UserID, mapID, region, currentHP, newHP))
	}
}

// hasCompletedTutorial 检查玩家是否完成新手任务（任务85-88）
// 参考 Lua: hasCompletedTutorial(user) in seer_login_response.lua
func hasCompletedTutorial(gameData *userdb.GameData) bool {
	if gameData == nil || gameData.Tasks == nil {
		return false
	}
	for id := 85; id <= 88; id++ {
		key := strconv.Itoa(id)
		task, ok := gameData.Tasks[key]
		if !ok {
			return false
		}
		status := task.Status
		if status == "" {
			return false
		}
		// 数值 3 或字符串 "completed" 视为已完成
		if status == "completed" || status == "3" {
			continue
		}
		return false
	}
	return true
}

// handleLogin 处理登录命令
func handleLogin(ctx *gameserver.HandlerContext) {
	// 同一账户只允许一个在线会话：若该 UID 已在其他连接登录，踢掉旧连接
	ctx.GameServer.KickOtherSessionsOfUser(ctx.ClientData, ctx.UserID)

	// 获取用户游戏数据
	gameData := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	// 登录时按自然日重置精灵训练营相关的每日任务（401/402/403），保证第二天可以重新接任务
	resetDailyTasksIfNewDay(gameData)
	// 每次登录清除上次会话残留的跟随精灵，进入游戏默认不跟随
	gameData.FollowPetCatchTime = 0
	// 兼容老玄武空间：根据已记录的 SPT 击败列表补齐相关任务完成状态（适配历史存档）
	for _, bossID := range gameData.DefeatedSPTBossIds {
		autoCompleteXuanWuTasksIfNeeded(gameData, bossID)
	}

	// 所有用户上线时地图ID固定为1（传送舱）
	gameData.MapID = 1

	// 获取账号基础数据（用于 userId / regTime 等字段）
	var account *userdb.User
	if ctx.GameServer.UserDB != nil {
		account = ctx.GameServer.UserDB.FindByUserID(ctx.UserID)
	}

	// 处理登录请求包体
	if len(ctx.Body) > 0 {
		// 这里可以添加对登录请求包体的处理逻辑
		// 例如解密、验证会话等
		logger.Info("处理登录请求包体")
	}

	// 构建登录响应包（严格对齐 Lua 版 SeerLoginResponse.makeLoginResponse）
	body := buildLoginResponse(ctx.UserID, account, gameData)

	// 记录登录响应关键字段与长度，便于和 Lua 抓包对比
	logger.Info(fmt.Sprintf(
		"登录响应数据: UID=%d Nick=%s MapID=%d Pets=%d Clothes=%d Tasks=%d BodyLen=%d",
		ctx.UserID,
		gameData.Nick,
		gameData.MapID,
		len(gameData.Pets),
		len(gameData.Clothes),
		len(gameData.Tasks),
		len(body),
	))

	// 标记用户已登录
	ctx.ClientData.LoggedIn = true

	// 发送响应
	ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)

	// 地图677 异能王：若服务端已记录六重试炼任务(758~763)完成，推送 2202 同步客户端，以便面板显示「挑战真身」按钮
	for _, tid := range []int{758, 759, 760, 761, 762, 763} {
		if gameData.Tasks != nil {
			if t, ok := gameData.Tasks[strconv.Itoa(tid)]; ok && t.Status == "3" {
				body2202 := buildTaskCompleteResponse(uint32(tid), 0, gameData)
				ctx.GameServer.SendResponse(ctx.ClientData, 2202, ctx.UserID, 0, body2202)
			}
		}
	}

	// 启动心跳
	ctx.GameServer.StartHeartbeat(ctx.ClientData, ctx.UserID)

	// 登录完成后主动推送一次进图相关封包：2001 / 2003 / 2004 / 9003
	// 等价于客户端立刻发送一次 ENTER_MAP(2001)，避免“刚登录双方在传送舱互相看不见，必须先切一次地图”。
	pushInitialMapEnter(ctx)
}

// pushChannelList 推送频道列表
func pushChannelList(ctx *gameserver.HandlerContext) {
	// 构建频道列表响应
	// 这里可以根据实际情况构建频道列表
	channelList := make([]byte, 2048) // 增加容量到2048
	index := 0

	// 频道数量
	binary.BigEndian.PutUint32(channelList[index:], 29) // 29个频道
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
		binary.BigEndian.PutUint32(channelList[index:], 100) // 模拟在线人数
		index += 4
	}

	// 发送频道列表响应
	logger.Info(fmt.Sprintf("推送频道列表: 命令ID=80001, 频道数量=29, 数据长度=%d", index))
	ctx.GameServer.SendResponse(ctx.ClientData, 80001, ctx.UserID, 0, channelList[:index])
}

// pushServerList 推送服务器列表
func pushServerList(ctx *gameserver.HandlerContext) {
	// 构建服务器列表响应
	// 这里可以根据实际情况构建服务器列表
	serverList := make([]byte, 2048) // 增加容量到2048
	index := 0

	// 服务器数量
	binary.BigEndian.PutUint32(serverList[index:], 29) // 29个服务器
	index += 4

	// 填充服务器数据
	for i := 1; i <= 29; i++ {
		// 服务器ID
		binary.BigEndian.PutUint32(serverList[index:], uint32(i))
		index += 4
		// 服务器名称
		serverName := fmt.Sprintf("服务器%d", i)
		nameBytes := []byte(serverName)
		copy(serverList[index:index+32], nameBytes)
		index += 32
		// 在线人数
		binary.BigEndian.PutUint32(serverList[index:], 1000) // 模拟在线人数
		index += 4
		// 服务器状态
		binary.BigEndian.PutUint32(serverList[index:], 1) // 1表示正常
		index += 4
	}

	// 发送服务器列表响应
	ctx.GameServer.SendResponse(ctx.ClientData, 80002, ctx.UserID, 0, serverList[:index])
}

// buildLoginResponse 构建登录响应包（对齐 Lua 版 SeerLoginResponse.makeLoginResponse）
func buildLoginResponse(userID int64, account *userdb.User, gameData *userdb.GameData) []byte {
	// 为避免频繁分配，预留一个大概的容量，最终长度由实际写入决定
	buffer := make([]byte, 0, 2048)

	writeU32 := func(v uint32) {
		tmp := make([]byte, 4)
		binary.BigEndian.PutUint32(tmp, v)
		buffer = append(buffer, tmp...)
	}
	writeU8 := func(v byte) {
		buffer = append(buffer, v)
	}
	writeFixedString := func(s string, n int) {
		b := []byte(s)
		if len(b) > n {
			b = b[:n]
		}
		buffer = append(buffer, b...)
		if len(b) < n {
			buffer = append(buffer, make([]byte, n-len(b))...)
		}
	}

	// ---- 1. 账号基本信息 ----
	regTime := time.Now().Unix() - 86400*365
	if account != nil && account.RegisterTime > 0 {
		regTime = account.RegisterTime
	}
	writeU32(uint32(userID))            // userID
	writeU32(uint32(regTime))           // regTime
	writeFixedString(gameData.Nick, 16) // nick

	// ---- 2. VIP Flags ----
	vipFlags := uint32(0)
	if gameData.Nono.SuperNono > 0 {
		vipFlags = 3 // 1 | 2
	}
	writeU32(vipFlags)

	// ---- 3. 基础属性 ----
	writeU32(uint32(gameData.DsFlag))  // dsFlag
	writeU32(uint32(gameData.Color))   // color
	writeU32(uint32(gameData.Texture)) // texture

	energy := gameData.Energy
	if energy == 0 && gameData.Nono.Energy > 0 {
		energy = gameData.Nono.Energy
	}
	if energy == 0 {
		energy = 100
	}
	writeU32(uint32(energy))         // energy
	writeU32(uint32(gameData.Coins)) // coins
	writeU32(uint32(gameData.FightBadge))

	// 地图/出生逻辑：默认进入 ID=1 传送舱（用户要求“玩家登录默认进入地址位置在ID=1的传送舱”）
	gameData.MapID = 1 // 所有用户上线时地图ID固定为1
	if gameData.PosX == 0 {
		gameData.PosX = 300
	}
	if gameData.PosY == 0 {
		gameData.PosY = 270
	}
	writeU32(uint32(gameData.MapID))
	writeU32(uint32(gameData.PosX))
	writeU32(uint32(gameData.PosY))
	writeU32(uint32(gameData.TimeToday))
	if gameData.TimeLimit == 0 {
		gameData.TimeLimit = 86400
	}
	writeU32(uint32(gameData.TimeLimit))

	// ---- 4. Flags （4 个 Byte）----
	writeU8(0) // isClothHalfDay
	writeU8(0) // isRoomHalfDay
	writeU8(0) // iFortressHalfDay
	writeU8(0) // isHQHalfDay

	// ---- 5. 统计信息 ----
	writeU32(uint32(gameData.LoginCnt))
	writeU32(uint32(gameData.Inviter))
	writeU32(uint32(gameData.NewInviteeCnt))
	writeU32(uint32(gameData.Nono.VipLevel))
	writeU32(uint32(gameData.Nono.VipValue))
	writeU32(uint32(gameData.Nono.VipStage))
	writeU32(uint32(gameData.Nono.AutoCharge))

	endTime := uint32(gameData.Nono.VipEndTime)
	isSuper := gameData.Nono.SuperNono > 0
	if isSuper && endTime == 0 {
		endTime = 0x7FFFFFFF
	}
	writeU32(endTime)
	writeU32(uint32(gameData.Nono.FreshManBonus))

	// ---- 6. 固定长度列表 ----
	// nonoChipList: 80 字节，每字节 1=已开启 0=未开启，对应 700001..700080；客户端 UserInfo.setForPeoleInfo 据此恢复
	// 超能NoNo：自动获得所有芯片功能（含飞行模式），无需芯片开启
	nonoChipList := make([]byte, 80)
	if gameData.Nono.SuperNono > 0 {
		for i := range nonoChipList {
			nonoChipList[i] = 1
		}
	} else {
		for i := 0; i < 80 && i < 160; i++ {
			if len(gameData.Nono.Func) > i/8 {
				if (gameData.Nono.Func[i/8] & (1 << uint(i%8))) != 0 {
					nonoChipList[i] = 1
				}
			}
		}
	}
	buffer = append(buffer, nonoChipList...)

	// dailyResArr: 50 字节，这里统一填 0
	writeFixedString("", 50)

	// ---- 6.1 bufferRecordArr：200 字节（1600 bit），来自 GameData.BufferRecord ----
	bufferRecord := gameData.BufferRecord
	if bufferRecord == nil {
		bufferRecord = make([]byte, 200)
	}
	if len(bufferRecord) < 200 {
		tmp := make([]byte, 200)
		copy(tmp, bufferRecord)
		bufferRecord = tmp
	}
	// 诊断：登录包下发的 buffer 首字节（bit0=1 表示 433 动画已播）
	if len(gameData.BufferRecord) > 0 {
		logger.Info(fmt.Sprintf("[1001] 登录包 BufferRecord: UID=%d len=%d firstByte=0x%02X", userID, len(gameData.BufferRecord), gameData.BufferRecord[0]))
	} else {
		logger.Info(fmt.Sprintf("[1001] 登录包 BufferRecord: UID=%d 使用默认 200 零字节（未持久化或新号）", userID))
	}
	buffer = append(buffer, bufferRecord[:200]...)

	// ---- 7. 更多统计字段 ----
	writeU32(uint32(gameData.TeacherID))
	writeU32(uint32(gameData.StudentID))
	writeU32(uint32(gameData.GraduationCount))
	writeU32(uint32(gameData.MaxPuniLv))
	writeU32(uint32(gameData.PetMaxLev))
	writeU32(uint32(gameData.PetAllNum))
	writeU32(uint32(gameData.MonKingWin))
	writeU32(uint32(gameData.CurStage))
	writeU32(uint32(gameData.MaxStage))
	writeU32(uint32(gameData.CurFreshStage))
	writeU32(uint32(gameData.MaxFreshStage))
	writeU32(uint32(gameData.MaxArenaWins))
	writeU32(uint32(gameData.TwoTimes))
	writeU32(uint32(gameData.ThreeTimes))
	writeU32(uint32(gameData.AutoFight))
	writeU32(uint32(gameData.AutoFightTimes))
	writeU32(uint32(gameData.EnergyTimes))
	writeU32(uint32(gameData.LearnTimes))
	writeU32(uint32(gameData.MonBtlMedal))

	// recordCnt, obtainTm, soulBeadItemID, expireTm, fuseTimes
	// 客户端用 obtainTm/soulBeadItemID 判断是否有元神珠正在赋形，有则打开面板时直接显示「进行中」并查 2356
	writeU32(0) // recordCnt
	var loginObtainTm, loginSoulBeadItemID uint32
	for _, b := range gameData.SoulBeads {
		if b.TransformStartTime > 0 {
			loginObtainTm = b.ObtainTime
			loginSoulBeadItemID = uint32(b.ItemID)
			break
		}
	}
	writeU32(loginObtainTm)
	writeU32(loginSoulBeadItemID)
	writeU32(0) // expireTm
	writeU32(0) // fuseTimes

	// ---- 8. NoNo 详细信息 ----
	hasNono := gameData.Nono.HasNono > 0
	superNono := gameData.Nono.SuperNono

	if hasNono {
		writeU32(1)
	} else {
		writeU32(0)
	}
	if superNono > 0 {
		writeU32(1)
	} else {
		writeU32(0)
	}

	// nonoState / nonoColor / nonoNick
	flag := gameData.Nono.Flag
	if flag == 0 {
		flag = -1 // 0xFFFFFFFF
	}
	writeU32(uint32(flag))
	writeU32(uint32(gameData.Nono.Color))
	writeFixedString(gameData.Nono.Nick, 16)

	// ---- 9. TeamInfo (24 bytes) ----
	for i := 0; i < 6; i++ {
		writeU32(0)
	}

	// ---- 10. TeamPKInfo (8 bytes) ----
	writeU32(0)
	writeU32(0)

	// ---- 11. Badge & Reserved ----
	writeU8(0) // padding/flag
	writeU32(uint32(gameData.Badge))
	writeFixedString("", 27)

	// ---- 12. Task List (500 bytes, 每个任务 1 字节状态) ----
	// 塞西利亚星第一层（地图 40）卡加载：init() 里若任务 8 为已接受会调 getProStatusList 等异步，
	// 若回调未正确执行或客户端等其才关加载界面会一直停在“快去探索!”。
	// 任务 121/122（雷伊特训/雷神极限修行）会影响 NPC 显示，因此不再在登录时隐藏它们。
	const (
		taskIDSeerCecilia  = 8   // 塞西利亚星相关（防寒服/晶体/阿克西亚平静），地图 40 init 依赖 getProStatusList(8)
		taskIDLeiyiLiAosi  = 121 // 雷神极限修行之里奥斯，地图 17
		taskIDLeiyiAkexiya = 122 // 雷伊特训之阿克希亚，地图 40
	)
	taskCount := 0
	if gameData.Tasks != nil {
		for i := 1; i <= 500; i++ {
			key := strconv.Itoa(i)
			statusByte := byte(0)
			if t, ok := gameData.Tasks[key]; ok {
				switch t.Status {
				case "completed", "3":
					statusByte = 3
				case "accepted", "doing", "in_progress", "1":
					statusByte = 1
				default:
					if n, err := strconv.Atoi(t.Status); err == nil {
						if n < 0 {
							n = 0
						} else if n > 255 {
							n = 255
						}
						statusByte = byte(n)
					}
				}
				// 任务 8：登录时不下发“已接受”，避免地图 40 卡加载
				if i == taskIDSeerCecilia && statusByte == 1 {
					statusByte = 0
				}
				if statusByte != 0 {
					taskCount++
				}
			}
			writeU8(statusByte)
		}
	} else {
		for i := 0; i < 500; i++ {
			writeU8(0)
		}
	}
	logger.Info(fmt.Sprintf("登录任务状态编码完成: totalTasks=%d", taskCount))

	// ---- 13. Pet List ----
	writeU32(uint32(len(gameData.Pets)))
	if len(gameData.Pets) > 0 {
		for _, pet := range gameData.Pets {
			petBody := buildFullPetInfo(pet)
			buffer = append(buffer, petBody...)
		}
	}

	// ---- 14. Clothes ----
	writeU32(uint32(len(gameData.Clothes)))
	for _, clothID := range gameData.Clothes {
		// Lua: 每件服装写入 (clothId, level)，我们只有 ID，默认等级为 1
		writeU32(uint32(clothID))
		writeU32(1)
	}

	// ---- 15. Title & Achievements ----
	writeU32(uint32(gameData.CurTitle))
	buffer = append(buffer, sptboss.BuildBossAchievement(gameData.DefeatedSPTBossIds)...) // bossAchievement 200 字节

	return buffer
}

// natureToClientID 将后端性格 ID 转为客户端 NatureXMLInfo 的 id
// 后端顺序(0-24): 孤独,勇敢,固执,调皮,胆小,急躁,开朗,天真,大胆,悠闲,顽皮,无虑,保守,稳重,冷静,马虎,沉着,温顺,狂妄,慎重,害羞,浮躁,坦率,实干,认真
// 客户端顺序(0-24): 孤独,固执,调皮,勇敢,大胆,顽皮,无虑,悠闲,保守,稳重,马虎,冷静,沉着,温顺,慎重,狂妄,胆小,急躁,开朗,天真,害羞,实干,坦率,浮躁,认真
var natureToClientIDTable = [25]int{
	0, 3, 1, 2, 16, 17, 18, 19, 4, 7, 5, 6, 8, 9, 11, 10, 12, 13, 15, 14, 20, 23, 22, 21, 24,
}

func natureToClientID(backendNature int) int {
	if backendNature >= 0 && backendNature < 25 {
		return natureToClientIDTable[backendNature]
	}
	return 0
}

// getFirstAvailablePetIndex 返回第一个体力>0的精灵索引，用于首发；若无精灵或全部0血返回-1
func getFirstAvailablePetIndex(user *userdb.GameData) int {
	if user == nil || len(user.Pets) == 0 {
		return -1
	}
	petMgr := gamepets.GetInstance()
	for i := range user.Pets {
		pet := &user.Pets[i]
		stats := petMgr.GetStats(pet.ID, pet.Level, pet.DV, pet.GetEVStats(), pet.Nature)
		if getPetEffectiveHP(pet, stats.MaxHP) > 0 {
			return i
		}
	}
	return -1
}

// getPetEffectiveHP 精灵当前体力：CurrentHP==-1 表示战败（显示 0）；0 或未设置表示满血；>0 为实际值（不超过 maxHP）。
// 用于战斗外展示与进入战斗时的初始 HP；战斗结束后写回 CurrentHP（战败写 -1），仅 9007/2306/2310 恢复时回满（写 0）。
func getPetEffectiveHP(pet *userdb.Pet, maxHP int) int {
	if pet == nil || maxHP <= 0 {
		return maxHP
	}
	if pet.CurrentHP == -1 {
		return 0 // 战败，显示 0 血
	}
	if pet.CurrentHP <= 0 {
		return maxHP // 未设置或已恢复，满血
	}
	if pet.CurrentHP > maxHP {
		return maxHP
	}
	return pet.CurrentHP
}

// syncBattleHPToUser 战斗结束时将当前出战精灵的战斗 HP 与技能 PP 写回 user.Pets[]，不自动回满；仅用户点击「恢复精灵状态」才回满。
func syncBattleHPToUser(gs *gameserver.GameServer, userID int64, battle *gameserver.BattleState) {
	if gs == nil || battle == nil {
		return
	}
	// 精灵大乱斗：只更新内存中的 GrandMeleePets，不写回背包
	if gs.GrandMeleePets != nil && gs.GrandMeleePets[userID] != nil {
		pets := gs.GrandMeleePets[userID]
		if battle.ActivePetIndex >= 0 && battle.ActivePetIndex < len(pets) {
			p := &pets[battle.ActivePetIndex]
			if battle.PlayerHP <= 0 {
				p.CurrentHP = -1
			} else {
				p.CurrentHP = int(battle.PlayerHP)
			}
			p.SkillPP = make([]int, 4)
			for i := 0; i < 4; i++ {
				p.SkillPP[i] = battle.PlayerSkillPP[i]
			}
		}
		return
	}
	user := gs.GetOrCreateUser(userID)
	if battle.ActivePetIndex < 0 || battle.ActivePetIndex >= len(user.Pets) {
		return
	}
	p := &user.Pets[battle.ActivePetIndex]
	if battle.PlayerHP <= 0 {
		p.CurrentHP = -1 // 战败/0 血，用 -1 表示以便 2301 显示为 0 血而非满血
	} else {
		p.CurrentHP = int(battle.PlayerHP)
	}
	p.SkillPP = make([]int, 4)
	for i := 0; i < 4; i++ {
		p.SkillPP[i] = battle.PlayerSkillPP[i]
	}
}

// fillBattlePlayerSkills 从精灵数据填充 BattleState 的 PlayerSkillIDs 与 PlayerSkillPP（进入战斗/切换精灵时调用）
func fillBattlePlayerSkills(battle *gameserver.BattleState, pet *userdb.Pet) {
	if battle == nil || pet == nil {
		return
	}
	petMgr := gamepets.GetInstance()
	skillMgr := gameskills.GetInstance()
	rawSkills := pet.Skills
	if len(rawSkills) == 0 {
		rawSkills = petMgr.GetSkillsForLevel(pet.ID, pet.Level)
	}
	for i := 0; i < 4; i++ {
		sid := 0
		if i < len(rawSkills) {
			sid = rawSkills[i]
		}
		battle.PlayerSkillIDs[i] = sid
		maxPP := 20
		if sid > 0 {
			if sk := skillMgr.Get(sid); sk != nil && sk.MaxPP > 0 {
				maxPP = sk.MaxPP
			}
		}
		pp := maxPP
		if len(pet.SkillPP) > i && pet.SkillPP[i] >= 0 {
			pp = pet.SkillPP[i]
			if pp > maxPP {
				pp = maxPP
			}
		}
		battle.PlayerSkillPP[i] = pp
	}
}

// buildFullPetInfo 构建完整版 PetInfo
// 对齐 Lua 版 PetHandlers.buildFullPetInfo / UserInfo.setForLoginInfo 的读取结构
func buildFullPetInfo(p userdb.Pet) []byte {
	buf := make([]byte, 0, 200)

	writeU32 := func(v uint32) {
		tmp := make([]byte, 4)
		binary.BigEndian.PutUint32(tmp, v)
		buf = append(buf, tmp...)
	}
	writeU16 := func(v uint16) {
		tmp := make([]byte, 2)
		binary.BigEndian.PutUint16(tmp, v)
		buf = append(buf, tmp...)
	}
	writeFixedString := func(s string, n int) {
		b := []byte(s)
		if len(b) > n {
			b = b[:n]
		}
		buf = append(buf, b...)
		if len(b) < n {
			buf = append(buf, make([]byte, n-len(b))...)
		}
	}

	petID := p.ID
	if petID <= 0 {
		petID = 1
	}
	level := p.Level
	if level <= 0 {
		level = 5
	}
	dv := p.DV
	if dv <= 0 {
		dv = 31
	}
	nature := p.Nature
	exp := p.Exp
	name := p.Name

	petMgr := gamepets.GetInstance()
	skillMgr := gameskills.GetInstance()

	// 从 Pet 结构体中获取 EV（学习力）
	ev := p.GetEVStats()

	// 基础属性 & 战斗属性（使用精灵的性格）
	stats := petMgr.GetStats(petID, level, dv, ev, nature)
	stats = applyPetStatBonus(stats, &p)
	expInfo := petMgr.GetExpInfo(petID, level, exp)

	// 技能列表：最多 4 个；优先使用技能唤醒仪保存的自定义技能
	var rawSkills []int
	if len(p.Skills) > 0 {
		rawSkills = make([]int, 4)
		for i := 0; i < 4 && i < len(p.Skills); i++ {
			rawSkills[i] = p.Skills[i]
		}
	} else {
		rawSkills = petMgr.GetSkillsForLevel(petID, level)
	}
	skillIDs := make([]int, 4)
	skillPPs := make([]int, 4)
	validCount := 0

	// 确保至少有一个技能（默认技能 10001 = 撞击）
	hasAnySkill := false
	for i := 0; i < len(rawSkills) && i < 4; i++ {
		if rawSkills[i] > 0 {
			hasAnySkill = true
			break
		}
	}
	if !hasAnySkill {
		// 如果没有技能，添加默认技能 10001（撞击）
		rawSkills = []int{10001, 0, 0, 0}
	}

	for i := 0; i < 4; i++ {
		sid := 0
		if i < len(rawSkills) {
			sid = rawSkills[i]
		}
		pp := 0
		maxPP := 20
		if sid > 0 {
			if sk := skillMgr.Get(sid); sk != nil {
				maxPP = sk.MaxPP
				if maxPP == 0 {
					maxPP = sk.PP
				}
				if maxPP == 0 {
					maxPP = 20
				}
				pp = maxPP
			}
			if pp == 0 {
				pp = 20
				maxPP = 20
			}
			// 持久化 PP：若有 SkillPP[i] 则用其（不超过 maxPP），否则满 PP
			if len(p.SkillPP) > i && p.SkillPP[i] >= 0 {
				pp = p.SkillPP[i]
				if pp > maxPP {
					pp = maxPP
				}
			}
			validCount++
		}
		skillIDs[i] = sid
		skillPPs[i] = pp
	}
	// 调试日志：输出技能信息
	logger.Info(fmt.Sprintf("[buildFullPetInfo] PetID=%d Level=%d Skills: validCount=%d", petID, level, validCount))
	for i := 0; i < 4; i++ {
		if skillIDs[i] > 0 {
			logger.Info(fmt.Sprintf("  Skill[%d]: ID=%d PP=%d", i, skillIDs[i], skillPPs[i]))
		}
	}

	// 1. 基础信息
	// nature: 后端与客户端 NatureXMLInfo 的性格顺序不同，需查表转换
	// 后端: 0孤独,1勇敢,2固执,3调皮,4胆小... | 客户端: 0孤独,1固执,2调皮,3勇敢,4大胆...
	writeU32(uint32(petID))    // id
	writeFixedString(name, 16) // name
	writeU32(uint32(dv))       // dv
	writeU32(uint32(natureToClientID(nature))) // nature（转客户端 ID 供正确显示）
	writeU32(uint32(level))    // level
	// 对齐前端语义（经验分配器“升级所需经验值”= nextLvExp - exp，须非负）：
	// - exp: 当前等级已获得经验，客户端用 nextLvExp - exp 显示“升级所需经验值”
	// - lvExp: 同上，当前等级经验（与 exp 一致）
	// - nextLvExp: 当前等级升到下一等级所需经验（至少为 1，避免经验分配器不显示）
	nextLvExp := expInfo.NextLevelExp
	if nextLvExp <= 0 {
		nextLvExp = 1
	}
	writeU32(uint32(expInfo.CurrentLevelExp)) // exp（当前等级经验，避免显示负数）
	writeU32(uint32(expInfo.CurrentLevelExp)) // lvExp（当前等级经验）
	writeU32(uint32(nextLvExp))               // nextLvExp

	// 2. 战斗属性（当前体力：持久化 CurrentHP 或满血，仅 9007 恢复精灵状态才回满）
	effectiveHP := getPetEffectiveHP(&p, stats.MaxHP)
	writeU32(uint32(effectiveHP))   // hp
	writeU32(uint32(stats.MaxHP))   // maxHp
	writeU32(uint32(stats.Attack))  // atk
	writeU32(uint32(stats.Defence)) // def
	writeU32(uint32(stats.SpAtk))   // sa
	writeU32(uint32(stats.SpDef))   // sd
	writeU32(uint32(stats.Speed))   // spd

	// 3. 努力值（当前全部 0）
	writeU32(uint32(ev.HP))
	writeU32(uint32(ev.Atk))
	writeU32(uint32(ev.Def))
	writeU32(uint32(ev.SpAtk))
	writeU32(uint32(ev.SpDef))
	writeU32(uint32(ev.Spd))

	// 4. 技能列表
	writeU32(uint32(validCount))
	for i := 0; i < 4; i++ {
		writeU32(uint32(skillIDs[i]))
		writeU32(uint32(skillPPs[i]))
	}

	// 5. 捕获信息
	writeU32(uint32(p.CatchTime)) // catchTime
	writeU32(uint32(301))         // catchMap，默认 301
	writeU32(0)                   // catchRect
	writeU32(uint32(level))       // catchLevel

	// 6. 特效列表
	if p.Trait > 0 {
		// 目前仅在服务器侧实现“融合精灵特性”这一类永久特性：
		// - itemId: 对应 PetEffectXMLInfo 中的 NewSeIdx.Idx（1006-1045）
		// - status: 2 表示激活/永久效果
		// 其余字段（leftCount/effectID/args）当前 Go 端战斗逻辑未使用，按 0 填充即可让前端正常解析与展示特性文案。
		writeU16(1) // effectCount

		// 对应 com.robot.core.info.pet.PetEffectInfo 的读取顺序：
		// itemId(U32) + status(U8) + leftCount(U8) + effectID(U16) +
		// param1(U8) + reserved(U8) + param2(U8) + paddingUTF(13)
		writeU32(uint32(p.Trait)) // itemId = NewSeIdx.Idx
		buf = append(buf, byte(2)) // status: 2 = 常驻/生效中
		buf = append(buf, byte(0)) // leftCount: 0 = 无次数限制
		writeU16(0)                // effectID: 暂未在 Go 端使用，置 0
		// 简化处理：参数与 13 字节描述区全部填 0，前端仅依赖 itemId/Idx 展示特性名称与说明。
		buf = append(buf, byte(0))           // param1
		buf = append(buf, byte(0))           // reserved
		buf = append(buf, byte(0))           // param2
		buf = append(buf, make([]byte, 13)...) // padding / args 占位
	} else {
		writeU16(0) // effectCount
	}

	// 7. Skin + Shiny（与前端 PetInfo 读包顺序一致，背包/跟随/2301 用真实异色）
	writeU32(0) // skinID
	shiny := uint32(0)
	if p.Shiny != 0 {
		shiny = 1
	}
	writeU32(shiny)

	return buf
}

// buildUsePetItemOutOfFightInfo 构建 CMD 2326 UsePetItemOutOfFightInfo 结构体
// 对齐前端 com.robot.core.info.UsePetItemOutOfFightInfo 的读取顺序：
// catchTime(4) + id(4) + nick(16字节定长) + nature(4) + dv(4) + lv(4)
// hp(4) + maxHp(4) + exp(4)
// ev_hp(4) ev_attack(4) ev_defence(4) ev_sa(4) ev_sd(4) ev_sp(4)
// a(4) sa(4) d(4) sd(4) sp(4)
// skillCount(4) + [skillId(4) pp(4)]*N
func buildUsePetItemOutOfFightInfo(p *userdb.Pet) []byte {
	buf := make([]byte, 0, 160)

	writeU32 := func(v uint32) {
		tmp := make([]byte, 4)
		binary.BigEndian.PutUint32(tmp, v)
		buf = append(buf, tmp...)
	}
	writeFixedString := func(s string, n int) {
		b := []byte(s)
		if len(b) > n {
			b = b[:n]
		}
		buf = append(buf, b...)
		if len(b) < n {
			buf = append(buf, make([]byte, n-len(b))...)
		}
	}

	if p == nil {
		return make([]byte, 4)
	}

	petID := p.ID
	if petID <= 0 {
		petID = 1
	}
	level := p.Level
	if level <= 0 {
		level = 1
	}
	dv := p.DV
	if dv <= 0 {
		dv = 31
	}
	nature := p.Nature
	name := p.Name

	petMgr := gamepets.GetInstance()
	skillMgr := gameskills.GetInstance()

	ev := p.GetEVStats()
	stats := petMgr.GetStats(petID, level, dv, ev, nature)

	// 技能：最多 4 个，同 2301 逻辑
	var rawSkills []int
	if len(p.Skills) > 0 {
		rawSkills = make([]int, 4)
		for i := 0; i < 4 && i < len(p.Skills); i++ {
			rawSkills[i] = p.Skills[i]
		}
	} else {
		rawSkills = petMgr.GetSkillsForLevel(petID, level)
	}
	skillIDs := make([]int, 0, 4)
	skillPPs := make([]int, 0, 4)

	for i := 0; i < 4; i++ {
		sid := 0
		if i < len(rawSkills) {
			sid = rawSkills[i]
		}
		if sid <= 0 {
			continue
		}
		pp := 0
		maxPP := 20
		if sk := skillMgr.Get(sid); sk != nil {
			maxPP = sk.MaxPP
			if maxPP == 0 {
				maxPP = sk.PP
			}
			if maxPP == 0 {
				maxPP = 20
			}
			pp = maxPP
		}
		if len(p.SkillPP) > i && p.SkillPP[i] >= 0 {
			pp = p.SkillPP[i]
			if pp > maxPP {
				pp = maxPP
			}
		}
		if pp == 0 {
			pp = maxPP
		}
		skillIDs = append(skillIDs, sid)
		skillPPs = append(skillPPs, pp)
	}

	// 写入字段
	writeU32(uint32(p.CatchTime))              // catchTime
	writeU32(uint32(petID))                    // id
	writeFixedString(name, 16)                 // nick
	writeU32(uint32(natureToClientID(nature))) // nature（转客户端 ID）
	writeU32(uint32(dv))                       // dv
	writeU32(uint32(level))                    // lv
	writeU32(uint32(stats.HP))                 // hp
	writeU32(uint32(stats.MaxHP))              // maxHp
	writeU32(uint32(p.Exp))                    // exp

	// EV
	writeU32(uint32(ev.HP))
	writeU32(uint32(ev.Atk))
	writeU32(uint32(ev.Def))
	writeU32(uint32(ev.SpAtk))
	writeU32(uint32(ev.SpDef))
	writeU32(uint32(ev.Spd))

	// 六维属性
	writeU32(uint32(stats.Attack))  // a
	writeU32(uint32(stats.SpAtk))   // sa
	writeU32(uint32(stats.Defence)) // d
	writeU32(uint32(stats.SpDef))   // sd
	writeU32(uint32(stats.Speed))   // sp

	// 技能
	writeU32(uint32(len(skillIDs))) // skillCount
	for i := 0; i < len(skillIDs); i++ {
		writeU32(uint32(skillIDs[i]))
		writeU32(uint32(skillPPs[i]))
	}

	return buf
}

// handleEnterMap 处理进入地图命令，与 Lua 相同进图规则：
// 请求 body：mapType(4) + mapId(4) + x(4) + y(4)；mapId==515 为教程地图，不更新服务器侧地图与列表。
// 响应：2001(peopleInfo) + 2003(同图玩家列表) + 2004(空精灵列表，5 秒后定时器发真实精灵)。
func handleEnterMap(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)

	mapId := uint32(0)
	x := uint32(500)
	y := uint32(300)

	if len(ctx.Body) >= 16 {
		// 与 Lua 一致：body 布局 mapType(4) + mapId(4) + x(4) + y(4)
		mapId = binary.BigEndian.Uint32(ctx.Body[4:8])
		x = binary.BigEndian.Uint32(ctx.Body[8:12])
		y = binary.BigEndian.Uint32(ctx.Body[12:16])
	}

	logger.Info(fmt.Sprintf(
		"进入地图请求: UID=%d ReqMapID=%d CurMapID=%d X=%d Y=%d BodyLen=%d",
		ctx.UserID,
		mapId,
		user.MapID,
		x,
		y,
		len(ctx.Body),
	))

	// 验证与设置默认地图（默认 ID=1 传送舱，新号也可进入）
	if mapId == 0 {
		mapId = uint32(user.MapID)
		if mapId == 0 {
			mapId = 1
		}
	}

	oldMapID := user.MapID
	// 与 Lua 一致：教程地图 515 不更新服务器侧 mapId/pos，也不做地图列表增删
	if mapId != 515 {
		// 青龙空间(403)：离开时清零守护兽击败进度，保证每次进入都需先打四只守护兽再打青龙
		if oldMapID == sptboss.MapIDQingLongSpace {
			user.DefeatedQingLongGuardianMask = 0
		}
		user.MapID = int(mapId)
		user.PosX = int(x)
		user.PosY = int(y)
		ctx.GameServer.RemoveUserFromMap(oldMapID, ctx.UserID)
		if oldMapID > 0 {
			oldListBody := buildMapPlayerListForMap(ctx.GameServer, oldMapID)
			ctx.GameServer.BroadcastToMap(oldMapID, 0, 2003, oldListBody)
			oldCount := uint32(0)
			if len(oldListBody) >= 4 {
				oldCount = binary.BigEndian.Uint32(oldListBody[0:4])
			}
			logger.Info(fmt.Sprintf("[2003] 向旧地图广播玩家列表(有人离开): 离开者UID=%d 旧地图ID=%d 剩余人数=%d", ctx.UserID, oldMapID, oldCount))
		}
		ctx.GameServer.AddUserToMap(int(mapId), ctx.UserID, ctx.ClientData)
		ctx.GameServer.SetOgreEnterMapTime(ctx.UserID)
	}

	// 构建用户信息响应（格式对齐 UserInfo.setForPeoleInfo / Lua map_handlers.buildPeopleInfo）
	body := buildPeopleInfo(ctx.UserID, user, time.Now().Unix(), int(x), int(y))

	logger.Info(fmt.Sprintf(
		"进入地图响应2001: UID=%d MapID=%d BodyLen=%d",
		ctx.UserID,
		user.MapID,
		len(body),
	))

	// 发送进入地图响应
	ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)

	// 推送地图玩家列表（同地图所有玩家含自己），使客户端能显示其他玩家
	listBody := buildMapPlayerListForMap(ctx.GameServer, int(mapId))
	ctx.GameServer.SendResponse(ctx.ClientData, 2003, ctx.UserID, ctx.SeqID, listBody)
	ctx.GameServer.BroadcastToMap(int(mapId), ctx.UserID, 2003, listBody)
	logger.Info(fmt.Sprintf(
		"推送地图玩家列表2003: UID=%d MapID=%d Count=%d",
		ctx.UserID,
		user.MapID,
		binary.BigEndian.Uint32(listBody[0:4]),
	))

	// 推送地图怪物列表：
	// 进入地图时先发送一包空精灵列表（9 格全 0），真实精灵由定时刷新在 5 秒后首次生成并推送，
	// 避免“进图立即刷一批 + 定时器很快又刷一批”导致 2004 与 2408 使用的槽位不同步。
	emptyOgreBody := ctx.GameServer.BuildMapOgreListFromSlots(nil)
	ctx.GameServer.SendResponse(ctx.ClientData, 2004, ctx.UserID, ctx.SeqID, emptyOgreBody)
	logger.Info(fmt.Sprintf("推送空精灵列表2004(进图初始): UID=%d MapID=%d", ctx.UserID, user.MapID))

	// 有防护罩的 SPT 地图：重置 BOSS 防护罩满血并推送 MAP_BOSS(2021)
	// 推送两次 2021：第二次时客户端 BOSS 已存在会调用 show()，BossModel 会执行 _walk.execute 从而启动每 3 秒走动
	if e, ok := sptboss.GetByMapAndParam(int(mapId), 0); ok && e.HasShield {
		petMgr := gamepets.GetInstance()
		stats := petMgr.GetStats(e.BossPetID, e.Level, 15, gamepets.EVStats{}, 0)
		setBossHp(ctx.UserID, int(mapId), 0, stats.MaxHP)
		bossBody := buildMapBossList(int(mapId), uint32(stats.MaxHP))
		if len(bossBody) > 0 {
			ctx.GameServer.SendResponse(ctx.ClientData, cmdMapBoss, ctx.UserID, ctx.SeqID, bossBody)
			ctx.GameServer.SendResponse(ctx.ClientData, cmdMapBoss, ctx.UserID, 0, bossBody)
			logger.Info(fmt.Sprintf("[2021] 推送地图 BOSS 列表: UID=%d MapID=%d PetID=%d hp=%d (x2 触发走动)", ctx.UserID, user.MapID, e.BossPetID, stats.MaxHP))
		}
	}
	// 赫尔卡星荒地(32) 雷雨天：推送 2021 含雷伊(id=70)，触发前端 BossCmdListener 派发 LY_OUT，播放雷伊出场动画
	if int(mapId) == gameogres.MapIDHelcarWasteland() && gameogres.IsLeiyiWeather() {
		leiyiBody := buildLeiyiBossBody()
		if len(leiyiBody) > 0 {
			ctx.GameServer.SendResponse(ctx.ClientData, cmdMapBoss, ctx.UserID, ctx.SeqID, leiyiBody)
			ctx.GameServer.SendResponse(ctx.ClientData, cmdMapBoss, ctx.UserID, 0, leiyiBody)
			logger.Info(fmt.Sprintf("[2021] 推送雷伊出场(触发 LY_OUT 动画): UID=%d MapID=%d", ctx.UserID, user.MapID))
		}
	}
	// 盖亚按周几出现在三张地图之一：进入“当日盖亚地图”时推送 2022(SPECIAL_PET_NOTE)，客户端显示可点击盖亚及对话今日条件
	if int(mapId) == getGaiyaMapIDForToday() {
		gaiyaNote := make([]byte, 8)
		binary.BigEndian.PutUint32(gaiyaNote[0:4], 1)   // show=1
		binary.BigEndian.PutUint32(gaiyaNote[4:8], petIDGaiya)
		ctx.GameServer.SendResponse(ctx.ClientData, cmdSpecialPetNote, ctx.UserID, 0, gaiyaNote)
		logger.Info(fmt.Sprintf("[2022] 推送盖亚出场(当日地图): UID=%d MapID=%d", ctx.UserID, user.MapID))
	}

	// 家园地图特殊处理：仅当 NoNo 未处于跟随状态时才下发 9003，在家园场景生成基地中的 NoNo
	// 若当前已跟随（IsFollowing=true），避免再次在基地生成一只“普通 NoNo”导致一跟一站两个造型
	if (mapId > 10000 || mapId == uint32(ctx.UserID)) && !user.Nono.IsFollowing {
		nonoBody := buildNonoInfo90(ctx.UserID, user)
		ctx.GameServer.SendResponse(ctx.ClientData, 9003, ctx.UserID, ctx.SeqID, nonoBody)
	}
}

// pushInitialMapEnter 在登录完成后主动推送一次进入默认地图/房间的相关封包
// 相当于模拟客户端发了一次 CMD=2001，然后服务器按同样逻辑回复 2001/2003/2004/9003
func pushInitialMapEnter(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)

	// 使用与 1001 一致的地图：默认 ID=1 传送舱
	mapId := uint32(user.MapID)
	x := uint32(user.PosX)
	y := uint32(user.PosY)
	if mapId == 0 {
		mapId = 1
	}

	if x == 0 {
		x = 500
	}
	if y == 0 {
		y = 300
	}

	logger.Info(fmt.Sprintf(
		"自动进入默认地图: UID=%d MapID=%d X=%d Y=%d",
		ctx.UserID,
		mapId,
		x,
		y,
	))

	// 更新用户状态（与 handleEnterMap 一致）
	user.MapID = int(mapId)
	user.PosX = int(x)
	user.PosY = int(y)
	ctx.GameServer.AddUserToMap(int(mapId), ctx.UserID, ctx.ClientData)
	ctx.GameServer.SetOgreEnterMapTime(ctx.UserID)

	// 构建并发送 2001 / 2003 / 2004 / 9003；2003 含同地图所有人使其他玩家可见
	body := buildPeopleInfo(ctx.UserID, user, time.Now().Unix(), int(x), int(y))
	ctx.GameServer.SendResponse(ctx.ClientData, 2001, ctx.UserID, 0, body)
	listBody := buildMapPlayerListForMap(ctx.GameServer, int(mapId))
	ctx.GameServer.SendResponse(ctx.ClientData, 2003, ctx.UserID, 0, listBody)
	ctx.GameServer.BroadcastToMap(int(mapId), ctx.UserID, 2003, listBody)

	// 推送地图精灵列表：有槽位则发真实 2004，与 handleEnterMap 一致
	slots := gameogres.GenerateNewSlotsNoCache(int(mapId))
	if len(slots) > 0 {
		ctx.GameServer.SetPlayerOgreSlots(ctx.UserID, int(mapId), slots)
		ogreBody := ctx.GameServer.BuildMapOgreListFromSlots(slots)
		ctx.GameServer.SendResponse(ctx.ClientData, 2004, ctx.UserID, 0, ogreBody)
		ctx.GameServer.SetOgreLastRefreshTime(ctx.UserID)
	} else {
		emptyOgreBody := ctx.GameServer.BuildMapOgreListFromSlots(nil)
		ctx.GameServer.SendResponse(ctx.ClientData, 2004, ctx.UserID, 0, emptyOgreBody)
	}

	// 赫尔卡星荒地(32) 雷雨天：推送 2021 触发雷伊出场动画
	if int(mapId) == gameogres.MapIDHelcarWasteland() && gameogres.IsLeiyiWeather() {
		leiyiBody := buildLeiyiBossBody()
		if len(leiyiBody) > 0 {
			ctx.GameServer.SendResponse(ctx.ClientData, cmdMapBoss, ctx.UserID, 0, leiyiBody)
			ctx.GameServer.SendResponse(ctx.ClientData, cmdMapBoss, ctx.UserID, 0, leiyiBody)
		}
	}
	if int(mapId) == getGaiyaMapIDForToday() {
		gaiyaNote := make([]byte, 8)
		binary.BigEndian.PutUint32(gaiyaNote[0:4], 1)
		binary.BigEndian.PutUint32(gaiyaNote[4:8], petIDGaiya)
		ctx.GameServer.SendResponse(ctx.ClientData, cmdSpecialPetNote, ctx.UserID, 0, gaiyaNote)
	}

	if mapId > 10000 || mapId == uint32(ctx.UserID) {
		nonoBody := buildNonoInfo(ctx.UserID, user)
		ctx.GameServer.SendResponse(ctx.ClientData, 9003, ctx.UserID, 0, nonoBody)
	}
}

// buildPeopleInfo 构建 2001/2003 用的用户信息体，严格对齐
// 前端 UserInfo.setForPeoleInfo 与 Lua map_handlers.buildPeopleInfo 的读写顺序
func buildPeopleInfo(userID int64, user *userdb.GameData, sysTime int64, posX, posY int) []byte {
	buf := make([]byte, 0, 256)
	writeU32 := func(v uint32) {
		t := make([]byte, 4)
		binary.BigEndian.PutUint32(t, v)
		buf = append(buf, t...)
	}
	writeI32 := func(v int32) {
		writeU32(uint32(v))
	}
	writeU16 := func(v uint16) {
		t := make([]byte, 2)
		binary.BigEndian.PutUint16(t, v)
		buf = append(buf, t...)
	}
	writeFixed := func(s string, n int) {
		b := []byte(s)
		if len(b) > n {
			b = b[:n]
		}
		buf = append(buf, b...)
		for i := len(b); i < n; i++ {
			buf = append(buf, 0)
		}
	}

	if posX == 0 {
		posX = 500
	}
	if posY == 0 {
		posY = 300
	}

	// 1. 基本信息
	writeI32(int32(sysTime))
	writeU32(uint32(userID))
	nick := user.Nick
	if nick == "" {
		nick = fmt.Sprintf("Seer%d", userID)
	}
	writeFixed(nick, 16)
	writeU32(uint32(user.Color))
	if user.Texture == 0 {
		writeU32(0)
	} else {
		writeU32(uint32(user.Texture))
	}

	// 2. VIP (bit0=vip, bit1=viped)
	vipFlags := uint32(0)
	if user.Nono.SuperNono > 0 {
		vipFlags = 3
	}
	writeU32(vipFlags)
	writeU32(uint32(user.Nono.VipStage))

	// 3. 动作与坐标
	actionType := uint32(0) // flyMode 未在 GameData，固定 0
	writeU32(actionType)
	writeU32(uint32(posX))
	writeU32(uint32(posY))
	writeU32(0) // action
	writeU32(0) // direction
	writeU32(0) // changeShape

	// 4. 精灵（spiritTime=catchTime, spiritID=petId, petDV, petShiny, petSkin, fightFlag）
	// 规则：
	// - 如果 FollowPetCatchTime>0，则使用玩家选择的跟随精灵
	// - 否则不携带精灵信息（全部填 0），避免像官服那样“默认第一只精灵自动跟随”
	//   这样就能保证：首次登录 / 手动取消跟随后，切换地图也不会突然跟随一只精灵
	var petID, catchTime, petDV, petShiny uint32 = 0, 0, 31, 0
	if user.FollowPetCatchTime > 0 {
		for _, p := range user.Pets {
			if p.CatchTime == user.FollowPetCatchTime {
				petID = uint32(p.ID)
				catchTime = uint32(p.CatchTime)
				if p.DV > 0 {
					petDV = uint32(p.DV)
				}
				if p.Shiny != 0 {
					petShiny = 1
				}
				break
			}
		}
		if petID == 0 && user.StoragePets != nil {
			for _, p := range user.StoragePets {
				if p.CatchTime == user.FollowPetCatchTime {
					petID = uint32(p.ID)
					catchTime = uint32(p.CatchTime)
					if p.DV > 0 {
						petDV = uint32(p.DV)
					}
					if p.Shiny != 0 {
						petShiny = 1
					}
					break
				}
			}
		}
	}
	// 注意：当 FollowPetCatchTime==0 时，此处保持 petID/catchTime 为 0，
	// 由客户端按“没有跟随精灵”处理；不再自动附带第一只精灵。
	writeU32(catchTime)
	writeU32(petID)
	writeU32(petDV)
	writeU32(petShiny) // 跟随精灵异色，客户端 setForPeoleInfo 读 petShiny，切换地图后显示正确
	writeU32(0)       // petSkin
	writeU32(0)       // fightFlag

	// 5. 师徒
	writeU32(uint32(user.TeacherID))
	writeU32(uint32(user.StudentID))

	// 6. NoNo
	nonoFlag := user.Nono.Flag
	writeU32(uint32(nonoFlag))
	writeU32(uint32(user.Nono.Color))
	if user.Nono.SuperNono > 0 {
		writeU32(1)
	} else {
		writeU32(0)
	}
	writeU32(0) // playerForm
	writeU32(0) // transTime

	// 7. TeamInfo：id, coreCount, isShow, logoBg, logoIcon, logoColor, txtColor, logoWord(4)
	writeU32(0)
	writeU32(0)
	writeU32(0)
	writeU16(0)
	writeU16(0)
	writeU16(0)
	writeU16(0)
	writeFixed("", 4)

	// 8. Clothes：count + [id,level]
	writeU32(uint32(len(user.Clothes)))
	for _, cid := range user.Clothes {
		writeU32(uint32(cid))
		writeU32(0) // level，无则 0，与 Lua 一致
	}

	// 9. curTitle
	writeU32(uint32(user.CurTitle))

	// peopleInfo 在官服/多数客户端中为“可变长度”：
	// - 固定字段后带 clothesCount + N*(id,level) + curTitle
	// 若强制截断会导致 clothes 永远为 0，从而地图上看不到对方装备。
	return buf
}

// buildMapPlayerList 构建地图玩家列表响应（单条 peopleInfo，兼容旧调用）
func buildMapPlayerList(peopleInfo []byte) []byte {
	buffer := make([]byte, 4+len(peopleInfo))
	binary.BigEndian.PutUint32(buffer[0:4], 1)
	copy(buffer[4:], peopleInfo)
	return buffer
}

// buildMapPlayerListForMap 构建当前地图上所有玩家的 2003 列表（含自己），用于同地图其他玩家可见
func buildMapPlayerListForMap(gs *gameserver.GameServer, mapID int) []byte {
	clients := gs.GetClientsOnMap(mapID)
	if len(clients) == 0 {
		return make([]byte, 4) // count=0
	}
	var parts [][]byte
	now := time.Now().Unix()
	for _, c := range clients {
		user := gs.GetOrCreateUser(c.UserID)
		x, y := user.PosX, user.PosY
		if x == 0 {
			x = 500
		}
		if y == 0 {
			y = 300
		}
		parts = append(parts, buildPeopleInfo(c.UserID, user, now, x, y))
	}
	total := 0
	for _, p := range parts {
		total += len(p)
	}
	buffer := make([]byte, 4+total)
	binary.BigEndian.PutUint32(buffer[0:4], uint32(len(parts)))
	off := 4
	for _, p := range parts {
		copy(buffer[off:], p)
		off += len(p)
	}
	return buffer
}

// CMD 2021 MAP_BOSS：客户端用此包在地图上显示 SPT BOSS（如蘑菇怪），BossCmdListener 解析后调用 BossController.add
const cmdMapBoss = 2021

// bossHpKey 返回 BOSS 血量缓存的 key
func bossHpKey(mapID int, region uint32) string {
	return fmt.Sprintf("%d_%d", mapID, region)
}

func getBossHp(userID int64, mapID int, region uint32) int {
	bossHpCacheMu.RLock()
	defer bossHpCacheMu.RUnlock()
	if m, ok := bossHpCache[userID]; ok {
		if hp, ok := m[bossHpKey(mapID, region)]; ok {
			return hp
		}
	}
	return 0 // 0 表示使用满血
}

func setBossHp(userID int64, mapID int, region uint32, hp int) {
	bossHpCacheMu.Lock()
	defer bossHpCacheMu.Unlock()
	if bossHpCache[userID] == nil {
		bossHpCache[userID] = make(map[string]int)
	}
	bossHpCache[userID][bossHpKey(mapID, region)] = hp
}

// pos=200 时客户端会移除该 region 的 BOSS（BossCmdListener）
const mapBossPosRemove = 200

// buildMapBossRemoveRegion 构建 2021 包体：移除指定 region 的 BOSS（pos=200）
func buildMapBossRemoveRegion(mapID int, region uint32) []byte {
	e, ok := sptboss.GetByMapAndParam(mapID, region)
	if !ok || !e.HasShield {
		return nil
	}
	body := make([]byte, 4+16)
	binary.BigEndian.PutUint32(body[0:4], 1)
	binary.BigEndian.PutUint32(body[4:8], uint32(e.BossPetID))
	binary.BigEndian.PutUint32(body[8:12], region)
	binary.BigEndian.PutUint32(body[12:16], 0)
	binary.BigEndian.PutUint32(body[16:20], mapBossPosRemove)
	return body
}

// buildLeiyiBossBody 构建雷伊的 MAP_BOSS(2021) 包体，用于触发前端 LY_OUT 出场动画
// 客户端 BossCmdListener 收到 id=70 后会派发 LY_OUT，MapProcess_32 播放 bossMc 动画
func buildLeiyiBossBody() []byte {
	body := make([]byte, 4+16)
	binary.BigEndian.PutUint32(body[0:4], 1)
	binary.BigEndian.PutUint32(body[4:8], 70)  // 雷伊 petID
	binary.BigEndian.PutUint32(body[8:12], 0)  // region=0
	binary.BigEndian.PutUint32(body[12:16], 0) // hp=0 无防护罩
	binary.BigEndian.PutUint32(body[16:20], 0) // pos=0
	return body
}

// buildMapBossList 构建 MAP_BOSS(2021) 响应体，用于在地图上显示 SPT BOSS
// 格式：len(4) + [id(4)+region(4)+hp(4)+pos(4)]*len；pos=200 表示移除该 region
// hp=0 表示无防护罩。仅对 OgreXMLInfo 有 boss 配置的地图推送（如克洛斯星密林 12 蘑菇怪）
func buildMapBossList(mapID int, hp uint32) []byte {
	e, ok := sptboss.GetByMapAndParam(mapID, 0)
	if !ok {
		return nil
	}
	// 仅有防护罩的 BOSS 需要推送 MAP_BOSS；无防护罩的由地图脚本直接触发战斗
	if !e.HasShield {
		return nil
	}
	body := make([]byte, 4+16)
	binary.BigEndian.PutUint32(body[0:4], 1)
	binary.BigEndian.PutUint32(body[4:8], uint32(e.BossPetID))
	binary.BigEndian.PutUint32(body[8:12], 0)   // region=0
	binary.BigEndian.PutUint32(body[12:16], hp) // 当前血量（0=无防护罩）
	binary.BigEndian.PutUint32(body[16:20], 0)  // pos=0
	return body
}

// buildMapOgreList 构建地图怪物列表响应
func buildMapOgreList(mapId int) []byte {
	// 结构：9 个槽位 * petId(4) = 36 字节（与Lua后端一致）
	body := make([]byte, 36)
	index := 0

	slots := gameogres.GetSlots(mapId)

	for i := 0; i < 9; i++ {
		var petID uint32
		if i < len(slots) && slots[i].PetID > 0 {
			petID = uint32(slots[i].PetID)
		}
		binary.BigEndian.PutUint32(body[index:], petID)
		index += 4
	}

	logger.Info(fmt.Sprintf("[2004] MapOgreList: mapId=%d slots=%v", mapId, slots))
	return body
}

// buildNonoInfo 构建NoNo信息响应
func buildNonoInfo(userID int64, user *userdb.GameData) []byte {
	// 创建响应缓冲区
	buffer := make([]byte, 128)
	index := 0

	// 写入用户ID
	binary.BigEndian.PutUint32(buffer[index:], uint32(userID))
	index += 4

	// 写入NoNo标志
	binary.BigEndian.PutUint32(buffer[index:], uint32(user.Nono.Flag))
	index += 4

	// 写入NoNo状态
	binary.BigEndian.PutUint32(buffer[index:], 1) // 在家状态
	index += 4

	// 写入NoNo昵称
	nonoNameBytes := []byte(user.Nono.Nick)
	copy(buffer[index:index+16], nonoNameBytes)
	index += 16

	// 写入SuperNoNo标志
	binary.BigEndian.PutUint32(buffer[index:], uint32(user.Nono.SuperNono))
	index += 4

	// 写入颜色
	binary.BigEndian.PutUint32(buffer[index:], uint32(user.Nono.Color))
	index += 4

	// 写入能量
	binary.BigEndian.PutUint32(buffer[index:], uint32(user.Nono.Power))
	index += 4

	// 写入亲密度
	binary.BigEndian.PutUint32(buffer[index:], uint32(user.Nono.Mate))
	index += 4

	// 写入IQ
	binary.BigEndian.PutUint32(buffer[index:], uint32(user.Nono.IQ))
	index += 4

	// 写入AI
	binary.BigEndian.PutUint16(buffer[index:], uint16(user.Nono.AI))
	index += 2

	// 填充剩余空间为0
	for i := index; i < 128; i++ {
		buffer[i] = 0
	}

	return buffer
}

// handleLeaveMap 与 Lua 相同进图规则：只回显 2002（body 为 4 字节 userId），不更新用户地图、不推送 2001。
// 客户端收到 2002 后自行发 2001(ENTER_MAP)，由 handleEnterMap 处理并回 2001+2003+2004。
func handleLeaveMap(ctx *gameserver.HandlerContext) {
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body[0:4], uint32(ctx.UserID))
	ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	logger.Info(fmt.Sprintf("[2002] 离开地图: UID=%d MapID=%d", ctx.UserID, user.MapID))
}

// handleListMapPlayer 处理地图玩家列表命令
func handleListMapPlayer(ctx *gameserver.HandlerContext) {
	// 客户端在地图初始化后会主动请求 LIST_MAP_PLAYER(2003)；返回同地图所有玩家（含自己）
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	if user.MapID == 0 {
		user.MapID = 1
	}
	body := buildMapPlayerListForMap(ctx.GameServer, user.MapID)
	ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
}

// handleMapHot CMD 1004 地图热点（宇宙地图热点数据）
// 客户端 MapHotInfo: count(4) + [id(4)+value(4)]*count；空热点则 count=0
// 若请求体含 4 字节，视为星系/子图索引，仅做日志便于排查第三/第四星系是否传参不同
func handleMapHot(ctx *gameserver.HandlerContext) {
	if len(ctx.Body) >= 4 {
		galaxyOrIndex := binary.BigEndian.Uint32(ctx.Body[0:4])
		logger.Info(fmt.Sprintf("[MAP_HOT] 1004 请求体 4 字节: galaxy/index=%d", galaxyOrIndex))
	}
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body, 0) // 热点数量 0，客户端不显示热点
	ctx.GameServer.SendResponse(ctx.ClientData, 1004, ctx.UserID, ctx.SeqID, body)
}

// handleMapOgreList 处理地图怪物列表命令（与 Lua 一致：优先返回该玩家当前地图的槽位）
func handleMapOgreList(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	mapID := user.MapID
	if mapID == 0 {
		mapID = 1
	}
	slots := ctx.GameServer.GetPlayerOgreSlots(ctx.UserID, mapID)
	if slots == nil {
		slots = gameogres.GenerateNewSlotsNoCache(mapID)
		if len(slots) > 0 {
			ctx.GameServer.SetPlayerOgreSlots(ctx.UserID, mapID, slots)
		}
	}
	var body []byte
	if len(slots) > 0 {
		body = ctx.GameServer.BuildMapOgreListFromSlots(slots)
	} else {
		body = buildMapOgreList(mapID) // 无配置地图回退到旧逻辑
	}
	ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
}

// handleChat CMD 2102 聊天
// 请求: toID(4) + msgLen(4) + msg(msgLen 字节，UTF-8)
// 响应: ChatInfo = senderID(4) + senderNickName(16) + toID(4) + msgLen(4) + msg(msgLen)
func handleChat(ctx *gameserver.HandlerContext) {
	toID := uint32(0)
	msgLen := uint32(0)
	msgBytes := []byte(nil)
	if len(ctx.Body) >= 8 {
		toID = binary.BigEndian.Uint32(ctx.Body[0:4])
		msgLen = binary.BigEndian.Uint32(ctx.Body[4:8])
		if msgLen > 0 && len(ctx.Body) >= 8+int(msgLen) {
			msgBytes = ctx.Body[8 : 8+msgLen]
		} else if len(ctx.Body) > 8 {
			msgBytes = ctx.Body[8:]
			msgLen = uint32(len(msgBytes))
		}
	}
	// 客户端 ChatAction.as 会在每条消息末尾主动写入字符 "0"，回显时去掉避免显示成 "Bye0"
	if len(msgBytes) > 0 && msgBytes[len(msgBytes)-1] == '0' {
		msgBytes = msgBytes[:len(msgBytes)-1]
		msgLen = uint32(len(msgBytes))
	}
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	nick := user.Nick
	if nick == "" {
		nick = "赛尔"
	}
	nickBytes := []byte(nick)
	if len(nickBytes) > 16 {
		nickBytes = nickBytes[:16]
	}
	// ChatInfo: senderID(4) + senderNickName(16) + toID(4) + msgLen(4) + msg
	body := make([]byte, 0, 4+16+4+4+len(msgBytes))
	buf4 := make([]byte, 4)
	binary.BigEndian.PutUint32(buf4, uint32(ctx.UserID))
	body = append(body, buf4...)
	nickPad := make([]byte, 16)
	copy(nickPad, nickBytes)
	body = append(body, nickPad...)
	binary.BigEndian.PutUint32(buf4, toID)
	body = append(body, buf4...)
	binary.BigEndian.PutUint32(buf4, msgLen)
	body = append(body, buf4...)
	body = append(body, msgBytes...)
	ctx.GameServer.SendResponse(ctx.ClientData, 2102, ctx.UserID, ctx.SeqID, body)
}

// handleAimat CMD 2104 射击/瞄准（AIMAT）
// 对齐 AS3: BasePeoleModel.aimatAction 发送 itemID, type, x, y；AimatCmdListener.onAimat 期望响应 userID(4)+itemID(4)+type(4)+x(4)+y(4)
// 响应并广播给同地图其他玩家，使其他玩家能看到射击动作
func handleAimat(ctx *gameserver.HandlerContext) {
	itemID := uint32(0)
	aimType := uint32(0)
	x := uint32(0)
	y := uint32(0)
	if len(ctx.Body) >= 16 {
		itemID = binary.BigEndian.Uint32(ctx.Body[0:4])
		aimType = binary.BigEndian.Uint32(ctx.Body[4:8])
		x = binary.BigEndian.Uint32(ctx.Body[8:12])
		y = binary.BigEndian.Uint32(ctx.Body[12:16])
	}
	// 响应: userID(4) + itemID(4) + type(4) + x(4) + y(4)，供客户端 dispatchAction 显示射击
	body := make([]byte, 20)
	binary.BigEndian.PutUint32(body[0:4], uint32(ctx.UserID))
	binary.BigEndian.PutUint32(body[4:8], itemID)
	binary.BigEndian.PutUint32(body[8:12], aimType)
	binary.BigEndian.PutUint32(body[12:16], x)
	binary.BigEndian.PutUint32(body[16:20], y)
	ctx.GameServer.SendResponse(ctx.ClientData, 2104, ctx.UserID, ctx.SeqID, body)
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	if user.MapID > 0 {
		ctx.GameServer.BroadcastToMap(user.MapID, ctx.UserID, 2104, body)
	}
}

// handleTransformUser CMD 2107 射击命中后变身（TRANSFORM_USER）
// 请求: targetUserID(4) + transformId(4) = 8 字节（AimatCmdListener.send(TRANSFORM_USER, _loc2_.info.userID, uint(_loc3_[0]))）
// 响应: 回显相同 8 字节；并向同地图广播 CMD 2108 NOTE_TRANSFORM_USER，body 为 targetUserID(4)+transformId(4)+0(4)，供其他客户端显示变身
func handleTransformUser(ctx *gameserver.HandlerContext) {
	targetUserID := uint32(0)
	transformId := uint32(0)
	if len(ctx.Body) >= 8 {
		targetUserID = binary.BigEndian.Uint32(ctx.Body[0:4])
		transformId = binary.BigEndian.Uint32(ctx.Body[4:8])
	}
	body := make([]byte, 8)
	binary.BigEndian.PutUint32(body[0:4], targetUserID)
	binary.BigEndian.PutUint32(body[4:8], transformId)
	ctx.GameServer.SendResponse(ctx.ClientData, 2107, ctx.UserID, ctx.SeqID, body)
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	if user.MapID > 0 {
		noteBody := make([]byte, 12)
		binary.BigEndian.PutUint32(noteBody[0:4], targetUserID)
		binary.BigEndian.PutUint32(noteBody[4:8], transformId)
		binary.BigEndian.PutUint32(noteBody[8:12], 0)
		ctx.GameServer.BroadcastToMap(user.MapID, 0, 2108, noteBody)
	}
}

// handleHeartbeat 处理心跳包
func handleHeartbeat(ctx *gameserver.HandlerContext) {
	// 实现心跳包逻辑
}

// ==================== 战斗命令（最小可用版本）====================

// buildAttackValue 构建攻击值（CMD 2505）
// 对齐客户端 AttackValue：userId(4)+skillId(4)+atkTimes(4)+lostHP(4)+gainHP(4)+remainHp(4)+maxHp(4)+state(4)+
// skillListCount(4)+[PetSkillInfo]*N+isCrit(4)+status(20)+battleLv(6)+maxShield(4)+curShield(4)+petType(4)，N=0 时共 82 字节
// status/battleLv 用于技能附加效果（烧伤、中毒、属性升降等），与 Lua buildAttackValue 一致
func buildAttackValue(userID uint32, skillID uint32, atkTimes uint32, lostHP uint32, gainHP int32, remainHP int32, maxHP uint32, state uint32, isCrit uint32, petType uint32, status [20]byte, battleLv [6]int8) []byte {
	if maxHP == 0 {
		maxHP = 1
	}
	maxHP32 := int32(maxHP)
	if remainHP < 0 {
		remainHP = 0
	}
	if remainHP > maxHP32 {
		remainHP = maxHP32
	}
	// 回血数值（gainHP）不在服务端做上限裁剪：
	// - 客户端会自行执行 hp += gainHP 并钳位到 maxHP；
	// - 为满足“满血也要飘字 +X”以及避免在多段行动（敌我同回合各出招）时产生错误裁剪（例如 +169 被裁成 +19），
	//   这里保持 gainHP 原值下发。
	const attackValueSize = 82 // 9*4 + skillListCount(4) + isCrit(4) + status(20) + battleLv(6) + maxShield(4) + curShield(4) + petType(4)
	buf := make([]byte, attackValueSize)
	off := 0
	binary.BigEndian.PutUint32(buf[off:off+4], userID)
	off += 4
	binary.BigEndian.PutUint32(buf[off:off+4], skillID)
	off += 4
	binary.BigEndian.PutUint32(buf[off:off+4], atkTimes)
	off += 4
	binary.BigEndian.PutUint32(buf[off:off+4], lostHP)
	off += 4
	// gainHP 和 remainHP 是有符号整数，需要正确转换
	if gainHP < 0 {
		binary.BigEndian.PutUint32(buf[off:off+4], uint32(math.MaxUint32+int64(gainHP)+1))
	} else {
		binary.BigEndian.PutUint32(buf[off:off+4], uint32(gainHP))
	}
	off += 4
	if remainHP < 0 {
		binary.BigEndian.PutUint32(buf[off:off+4], uint32(math.MaxUint32+int64(remainHP)+1))
	} else {
		binary.BigEndian.PutUint32(buf[off:off+4], uint32(remainHP))
	}
	off += 4
	binary.BigEndian.PutUint32(buf[off:off+4], maxHP)
	off += 4
	binary.BigEndian.PutUint32(buf[off:off+4], state)
	off += 4
	binary.BigEndian.PutUint32(buf[off:off+4], 0) // skillListCount = 0
	off += 4
	binary.BigEndian.PutUint32(buf[off:off+4], isCrit)
	off += 4
	// status 20 bytes（异常状态：0=无 1=中毒 2=烧伤等）
	copy(buf[off:off+20], status[:])
	off += 20
	// battleLv 6 bytes 有符号（攻击/防御/特攻/特防/速度/命中 等级 -6~+6）
	for i := 0; i < 6; i++ {
		buf[off] = byte(uint8(battleLv[i]))
		off++
	}
	binary.BigEndian.PutUint32(buf[off:off+4], 0) // maxShield
	off += 4
	binary.BigEndian.PutUint32(buf[off:off+4], 0) // curShield
	off += 4
	binary.BigEndian.PutUint32(buf[off:off+4], petType)
	off += 4
	return buf[:off]
}

// buildAttackValueWithSkills 与 buildAttackValue 相同，但会额外下发技能列表（skillID + 当前 PP），
// 以便客户端在 2505 后立刻刷新 PP（例如 EffectID=87“恢复自身所有PP值”）。
// PetSkillInfo: id(uint32)+pp(uint32) 共 8 字节；count 为实际下发条数（通常为 4）。
func buildAttackValueWithSkills(userID uint32, skillID uint32, atkTimes uint32, lostHP uint32, gainHP int32, remainHP int32, maxHP uint32, state uint32, isCrit uint32, petType uint32, status [20]byte, battleLv [6]int8, skillIDs [4]int, skillPP [4]int) []byte {
	if maxHP == 0 {
		maxHP = 1
	}
	maxHP32 := int32(maxHP)
	if remainHP < 0 {
		remainHP = 0
	}
	if remainHP > maxHP32 {
		remainHP = maxHP32
	}

	// 只下发有效技能槽
	type pair struct {
		id uint32
		pp uint32
	}
	list := make([]pair, 0, 4)
	for i := 0; i < 4; i++ {
		if skillIDs[i] <= 0 {
			continue
		}
		pp := skillPP[i]
		if pp < 0 {
			pp = 0
		}
		list = append(list, pair{id: uint32(skillIDs[i]), pp: uint32(pp)})
	}

	const baseSize = 82
	buf := make([]byte, baseSize+len(list)*8)
	off := 0
	binary.BigEndian.PutUint32(buf[off:off+4], userID)
	off += 4
	binary.BigEndian.PutUint32(buf[off:off+4], skillID)
	off += 4
	binary.BigEndian.PutUint32(buf[off:off+4], atkTimes)
	off += 4
	binary.BigEndian.PutUint32(buf[off:off+4], lostHP)
	off += 4
	if gainHP < 0 {
		binary.BigEndian.PutUint32(buf[off:off+4], uint32(math.MaxUint32+int64(gainHP)+1))
	} else {
		binary.BigEndian.PutUint32(buf[off:off+4], uint32(gainHP))
	}
	off += 4
	if remainHP < 0 {
		binary.BigEndian.PutUint32(buf[off:off+4], uint32(math.MaxUint32+int64(remainHP)+1))
	} else {
		binary.BigEndian.PutUint32(buf[off:off+4], uint32(remainHP))
	}
	off += 4
	binary.BigEndian.PutUint32(buf[off:off+4], maxHP)
	off += 4
	binary.BigEndian.PutUint32(buf[off:off+4], state)
	off += 4
	binary.BigEndian.PutUint32(buf[off:off+4], uint32(len(list))) // skillListCount
	off += 4
	for _, it := range list {
		binary.BigEndian.PutUint32(buf[off:off+4], it.id)
		off += 4
		binary.BigEndian.PutUint32(buf[off:off+4], it.pp)
		off += 4
	}
	binary.BigEndian.PutUint32(buf[off:off+4], isCrit)
	off += 4
	copy(buf[off:off+20], status[:])
	off += 20
	for i := 0; i < 6; i++ {
		buf[off] = byte(uint8(battleLv[i]))
		off++
	}
	binary.BigEndian.PutUint32(buf[off:off+4], 0) // maxShield
	off += 4
	binary.BigEndian.PutUint32(buf[off:off+4], 0) // curShield
	off += 4
	binary.BigEndian.PutUint32(buf[off:off+4], petType)
	off += 4
	return buf[:off]
}

// 盖亚精元物品 ID；COMPLETE_TASK(2202) / SPRINT_GIFT_NOTICE(8010) / SPECIAL_PET_NOTE(2022) 协议号
const (
	itemIDGaiyaEssence   = 400126
	cmdCompleteTask      = 2202
	cmdSprintGiftNotice  = 8010
	cmdSpecialPetNote    = 2022   // 客户端显示/隐藏盖亚出场，body: show(4)+petID(4)
	taskIDGaiya          = 99
	petIDGaiya           = 261
)

// getGaiyaMapIDForToday 返回当日盖亚出现的地图 ID（15 火山星 / 54 露西欧星 / 105 双子阿尔法星），供进入地图时推送 2022 显示盖亚
func getGaiyaMapIDForToday() int {
	weekday := int(time.Now().Weekday()) // 0=Sun, 1=Mon, ..., 6=Sat
	switch weekday {
	case 0, 2, 4:
		return 54
	case 1, 5:
		return 15
	case 3, 6:
		return 105
	default:
		return 54
	}
}

// pushGaiyaRewardOrNotice 盖亚战斗胜利后：按周几+地图+条件决定推送 COMPLETE_TASK(含精元) 或 SPRINT_GIFT_NOTICE(未按规则)
// 三地图：火山星15=两回合内击败(周一、周五)，露西欧星54=致命一击击败(周二、周四、周日)，双子阿尔法星105=十回合后击败(周三、周六)
func pushGaiyaRewardOrNotice(ctx *gameserver.HandlerContext, battle *gameserver.BattleState, user *userdb.GameData) {
	requiredMap := getGaiyaMapIDForToday()
	if battle.BattleMapID != requiredMap {
		ctx.GameServer.SendResponse(ctx.ClientData, cmdSprintGiftNotice, ctx.UserID, 0, []byte{})
		logger.Info(fmt.Sprintf("[盖亚] 地图不匹配: mapID=%d 今日应为 map=%d，推送 8010", battle.BattleMapID, requiredMap))
		return
	}
	conditionOK := false
	switch requiredMap {
	case 15:
		conditionOK = battle.RoundCount <= 2
	case 54:
		conditionOK = battle.LastHitWasCrit
	case 105:
		conditionOK = battle.RoundCount > 10
	}
	if !conditionOK {
		ctx.GameServer.SendResponse(ctx.ClientData, cmdSprintGiftNotice, ctx.UserID, 0, []byte{})
		logger.Info(fmt.Sprintf("[盖亚] 条件未满足: map=%d Round=%d Crit=%v，推送 8010", requiredMap, battle.RoundCount, battle.LastHitWasCrit))
		return
	}
	// SPT 精元只发一次：已领取过盖亚精元则不再发放，与雷伊/纳多雷等首次击败逻辑一致
	alreadyReceived := false
	for _, id := range user.DefeatedSPTBossIds {
		if id == petIDGaiya {
			alreadyReceived = true
			break
		}
	}
	if alreadyReceived {
		logger.Info(fmt.Sprintf("[盖亚] 已领取过精元(261)，本次不再发放 map=%d", requiredMap))
		return
	}
	user.DefeatedSPTBossIds = append(user.DefeatedSPTBossIds, petIDGaiya)
	// 发放盖亚精元并推送 COMPLETE_TASK(2202) 供前端弹窗
	if user.Items == nil {
		user.Items = make(map[string]userdb.Item)
	}
	itemKey := strconv.FormatUint(uint64(itemIDGaiyaEssence), 10)
	if existing, ok := user.Items[itemKey]; ok {
		existing.Count++
		user.Items[itemKey] = existing
	} else {
		user.Items[itemKey] = userdb.Item{Count: 1, ExpireTime: 0x057E40}
	}
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}
	body := buildGaiyaCompleteTaskBody()
	ctx.GameServer.SendResponse(ctx.ClientData, cmdCompleteTask, ctx.UserID, 0, body)
	logger.Info(fmt.Sprintf("[盖亚] 条件满足: map=%d 发放精元 400126，推送 2202 taskID=99", requiredMap))
}

// buildGaiyaCompleteTaskBody NoviceFinishInfo: taskID(4)+petID(4)+captureTm(4)+itemCount(4)+[itemID(4)+itemCnt(4)]...
func buildGaiyaCompleteTaskBody() []byte {
	buf := make([]byte, 16+8) // 16 + 1*8
	binary.BigEndian.PutUint32(buf[0:4], taskIDGaiya)
	binary.BigEndian.PutUint32(buf[4:8], 0)
	binary.BigEndian.PutUint32(buf[8:12], 0)
	binary.BigEndian.PutUint32(buf[12:16], 1)
	binary.BigEndian.PutUint32(buf[16:20], itemIDGaiyaEssence)
	binary.BigEndian.PutUint32(buf[20:24], 1)
	return buf
}

// buildBossMonster8004Body 构建 CMD 8004 (GET_BOSS_MONSTER) 响应体，用于首次击败 SPT BOSS 后推送奖励通知
// 客户端 BossCmdListener 收到后弹窗显示"获得精元/精灵"。格式：bonusID(4)+petID(4)+captureTm(4)+itemCount(4)+[itemID(4)+itemCnt(4)]*n
// itemID/itemCnt 为 0 时 itemCount=0（仅精灵奖励）；否则 itemCount=1
func buildBossMonster8004Body(bonusID, petID, captureTm, itemID, itemCnt uint32) []byte {
	itemCount := uint32(0)
	if itemCnt > 0 {
		itemCount = 1
	}
	body := make([]byte, 16+int(itemCount)*8)
	binary.BigEndian.PutUint32(body[0:4], bonusID)
	binary.BigEndian.PutUint32(body[4:8], petID)
	binary.BigEndian.PutUint32(body[8:12], captureTm)
	binary.BigEndian.PutUint32(body[12:16], itemCount)
	if itemCount > 0 {
		binary.BigEndian.PutUint32(body[16:20], itemID)
		binary.BigEndian.PutUint32(body[20:24], itemCnt)
	}
	return body
}

// handleGaiyaEffectQuery CMD 2149 盖亚魂印状态查询
// 请求一般无包体，响应为 GaiyaEffectInfo 结构。
func handleGaiyaEffectQuery(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	body := buildGaiyaEffectInfoBody(user)
	ctx.GameServer.SendResponse(ctx.ClientData, ctx.CmdID, ctx.UserID, ctx.SeqID, body)
}

// buildFightOverInfo 构建战斗结束信息（CMD 2506）
// 对齐 Lua: buildFightOverInfo
// reason(4) + winnerId(4) + twoTimes(4) + threeTimes(4) + autoFightTimes(4) + energyTimes(4) + learnTimes(4) = 28 bytes
func buildFightOverInfo(reason uint32, winnerID uint32) []byte {
	buf := make([]byte, 28)
	binary.BigEndian.PutUint32(buf[0:4], reason)
	binary.BigEndian.PutUint32(buf[4:8], winnerID)
	binary.BigEndian.PutUint32(buf[8:12], 0)  // twoTimes
	binary.BigEndian.PutUint32(buf[12:16], 0) // threeTimes
	binary.BigEndian.PutUint32(buf[16:20], 0) // autoFightTimes
	binary.BigEndian.PutUint32(buf[20:24], 0) // energyTimes
	binary.BigEndian.PutUint32(buf[24:28], 0) // learnTimes
	return buf
}

// unlockGaiyaEffectIfNeeded 根据战斗结果解锁盖亚魂印特训效果：
// 1=嗜血之力：423 地图对战雷伊(70)，以满血结束战斗；
// 2=邪气凌然：61 地图对战厄尔塞拉(421)，整场战斗中受到敌方攻击扣血次数 >=10；
// 3=石破天惊：423 地图对战哈莫雷特(216)，最后一击使用技能“石破天惊”(10715) 且为致命一击。
func unlockGaiyaEffectIfNeeded(ctx *gameserver.HandlerContext, battle *gameserver.BattleState, user *userdb.GameData) {
	if battle == nil || user == nil {
		return
	}

	effectID := 0

	switch battle.EnemyID {
	case 70: // 雷伊 —— 嗜血之力
		if battle.BattleMapID == 423 && battle.PlayerHP == battle.PlayerMaxHP {
			effectID = 1
		}
	case 421: // 厄尔塞拉 —— 邪气凌然
		if battle.BattleMapID == 61 && battle.HitsTakenFromEnemy >= 10 {
			effectID = 2
		}
	case 216: // 哈莫雷特 —— 石破天惊
		if battle.BattleMapID == 423 &&
			battle.LastPlayerSkillID == skillIDShiPoTianJing &&
			battle.LastHitWasCrit {
			effectID = 3
		}
	default:
		return
	}

	if effectID == 0 {
		return
	}

	if user.GaiyaEffects == nil {
		user.GaiyaEffects = make([]int, 0, 3)
	}
	for _, id := range user.GaiyaEffects {
		if id == effectID {
			return
		}
	}
	user.GaiyaEffects = append(user.GaiyaEffects, effectID)
	if user.GaiyaDefaultEffect == 0 {
		user.GaiyaDefaultEffect = effectID
	}

	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}

	logger.Info(fmt.Sprintf("[盖亚魂印] 解锁 effectID=%d UID=%d EnemyID=%d MapID=%d Hits=%d Round=%d Crit=%v Skill=%d",
		effectID, ctx.UserID, battle.EnemyID, battle.BattleMapID,
		battle.HitsTakenFromEnemy, battle.RoundCount, battle.LastHitWasCrit, battle.LastPlayerSkillID))
}

// pushMapOgreListAfterFightOver 对战/捕捉/逃跑结束后：记录战毕时间、清空该玩家该地图槽位，推送空精灵列表；12 秒后定时器会重新生成 3 只并推送（与 Lua 一致）
func pushMapOgreListAfterFightOver(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	mapID := user.MapID
	if mapID == 0 {
		mapID = 1
	}
	ctx.GameServer.SetOgreFightEndTime(ctx.UserID)
	ctx.GameServer.ClearPlayerOgreSlots(ctx.UserID, mapID)
	ogreBody := ctx.GameServer.BuildMapOgreListFromSlots(nil) // nil 或空切片得到 9 格全 0
	ctx.GameServer.SendResponse(ctx.ClientData, 2004, ctx.UserID, 0, ogreBody)
	logger.Info(fmt.Sprintf("[2004] 对战结束推送空精灵列表，12秒后定时刷新: UID=%d MapID=%d", ctx.UserID, mapID))
}

// handleUseSkill CMD 2405 使用技能（带战斗状态管理）
// 对齐 Lua: fight_handlers.handleUseSkill
func handleUseSkill(ctx *gameserver.HandlerContext) {
	var skillID uint32
	if len(ctx.Body) >= 4 {
		skillID = binary.BigEndian.Uint32(ctx.Body[0:4])
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)

	// ACK
	ctx.GameServer.SendResponse(ctx.ClientData, 2405, ctx.UserID, ctx.SeqID, []byte{})

	// 获取战斗状态
	ctx.GameServer.BattleMu.Lock()
	battle, exists := ctx.GameServer.BattleStates[ctx.UserID]
	if !exists || !battle.IsActive {
		ctx.GameServer.BattleMu.Unlock()
		logger.Warning(fmt.Sprintf("[2405] 战斗状态不存在或已结束，无法使用技能"))
		// PvP 场景：败方 2405 先到会先结算并已向双方发送 2506+2004；胜方 2405 后到时状态已删。
		// 若再发 2506(winnerID=0)，客户端可能以最后一次为准，导致胜方误显示“输”。故此处不再发送 2506。
		return
	}
	// 注意：保持锁定直到更新完战斗状态
	if battle.PlayerUsedSkills == nil {
		battle.PlayerUsedSkills = make(map[int]bool, 8)
	}
	if skillID != 0 {
		battle.PlayerUsedSkills[int(skillID)] = true
	}

	// PvP：双方都选好技能后再统一结算一整个回合。
	// 逻辑：
	// - 第一位玩家发送 2405 时，只在自己的 BattleState 上记录本回合选择的技能，并等待对方；
	// - 当第二位玩家也发送 2405 时，检查到双方均已选招，再继续向下执行一次完整的回合结算，
	//   其中“Player”始终为当前 ctx.UserID，“Enemy” 为 OpponentUserID。
	if battle.OpponentUserID != 0 {
		// 记录我方本回合选择的技能（允许在对方未选前多次点击，以最后一次为准）
		battle.PvPSelectedSkillID = skillID
		battle.PvPHasSelected = true

		opponentUID := battle.OpponentUserID
		opponentBattle, ok := ctx.GameServer.BattleStates[opponentUID]
		if ok && opponentBattle.IsActive {
			if !opponentBattle.PvPHasSelected {
				// 对方本回合已切换精灵（PvPChangedPetThisRound）：切换算作对方本回合行动，可立即结算，不需等待。
				if opponentBattle.PvPChangedPetThisRound {
					logger.Info(fmt.Sprintf("[2405-PvP] UID=%d 出招，对方 UID=%d 已切换精灵，直接结算", ctx.UserID, opponentUID))
				} else {
					// 对方尚未选择技能：仅记录本方选择，等待对方 2405。
					ctx.GameServer.BattleMu.Unlock()
					logger.Info(fmt.Sprintf("[2405-PvP] UID=%d 已选择技能 %d，等待对方 UID=%d 选择技能", ctx.UserID, skillID, opponentUID))
					return
				}
			}
			// 若双方均已选招，或对方本回合已切换精灵，则继续向下执行一次完整的回合结算。
		} else {
			// 对方战斗状态已失效（例如中途断线），后续按 PvE 单人逻辑继续结算。
			logger.Warning(fmt.Sprintf("[2405-PvP] 对方 UID=%d 战斗状态已失效，按 PvE 逻辑结算本回合", opponentUID))
		}
	}

	// 盖亚挑战条件：每使用一次技能计为一回合
	battle.LastPlayerSkillID = int(skillID)
	battle.RoundCount++
	// 每个回合开始时，清空上一回合记录的“敌方技能对我造成的伤害”
	// 后续若本回合敌人成功用技能打中我，会重新写入该字段，供本回合的“克制”等技能使用。
	battle.LastDamageFromEnemySkill = 0
	battle.LastDamageFromEnemySkillRound = battle.RoundCount
	// 尼奥(416)特殊逻辑：
	// - 若本场战斗敌方为尼奥且我方从未让尼尔(77)登场过，
	//   当 RoundCount 达到 2 及以上时，尼奥立刻逃跑，结束战斗并弹窗“精灵已逃跑”（客户端按 2506 结果自行显示）。
	if battle.IsWildMonster && battle.EnemyID == 416 {
		activeIdx := 0
		if battle.ActivePetIndex >= 0 && battle.ActivePetIndex < len(user.Pets) {
			activeIdx = battle.ActivePetIndex
		}
		if len(user.Pets) > 0 && activeIdx >= 0 && activeIdx < len(user.Pets) {
			if user.Pets[activeIdx].ID == 77 {
				battle.HasSeenNeal = true
			}
		}
		if !battle.HasSeenNeal && battle.RoundCount >= 2 {
			// 尼奥自动逃跑：结束战斗，不视为胜负。
			logger.Info(fmt.Sprintf("[2405] 尼奥逃跑事件触发: UID=%d Round=%d", ctx.UserID, battle.RoundCount))
			// 先标记战斗结束并清理 BattleState，避免定时刷新误判“仍在战斗中”而永远不刷新野怪（重登也会受影响）。
			battle.IsActive = false
			syncBattleHPToUser(ctx.GameServer, ctx.UserID, battle)
			delete(ctx.GameServer.BattleStates, ctx.UserID)
			ctx.GameServer.BattleMu.Unlock()

			overBody := buildFightOverInfo(0, 0)
			ctx.GameServer.SendResponse(ctx.ClientData, 2506, ctx.UserID, ctx.SeqID, overBody)
			pushMapOgreListAfterFightOver(ctx)
			return
		}
	}
	logger.Info(fmt.Sprintf("[2405] 使用技能前: PlayerHP=%d/%d EnemyHP=%d/%d Round=%d",
		battle.PlayerHP, battle.PlayerMaxHP, battle.EnemyHP, battle.EnemyMaxHP, battle.RoundCount))

	// 回合开始：结算异常状态伤害（烧伤/中毒每回合扣 1/8 最大HP，对齐 Lua processStatusEffects）
	gameskills.ProcessStatusEffects(
		&battle.PlayerHP, &battle.EnemyHP,
		battle.PlayerMaxHP, battle.EnemyMaxHP,
		&battle.PlayerStatus, &battle.EnemyStatus)
	// 回合开始：百变随心等效果的持续回合数递减（仅作用于“免疫能力下降”标记）
	if battle.PlayerStatDropImmuneRounds > 0 {
		battle.PlayerStatDropImmuneRounds--
	}
	// 尤纳斯(132) phase 1：强制锁 1 血，仅里奥斯幻影可击杀（固定伤害/异常 DoT 不能致死）
	if sptboss.IsYouNaSiBoss(battle.EnemyID) && battle.YouNaSiPhase == 1 && battle.EnemyHP == 0 {
		battle.EnemyHP = 1
	}
	// 谱尼封印/真身：回合初效果
	if battle.EnemyID == sptboss.PetIDPuni && sptboss.IsPuniSealBoss(battle.BattleMapID, battle.BossRegion) && battle.EnemyHP > 0 {
		// 真身按“当前命”套用不同规则
		if battle.BossRegion == sptboss.PuniTrueForm && battle.PuniTotalLives == 6 && battle.PuniCurrentLife >= 1 {
			life := battle.PuniCurrentLife
			// 第4/6命：每回合回 2000
			if sptboss.PuniTrueFormLifeHasHeal2000(life) {
				battle.EnemyHP += 2000
				if battle.EnemyHP > battle.EnemyMaxHP {
					battle.EnemyHP = battle.EnemyMaxHP
				}
			}
			// 第6命：准轮回——血量 < 1000 自动回满
			if life == 6 && battle.EnemyHP > 0 && battle.EnemyHP < 1000 {
				battle.EnemyHP = battle.EnemyMaxHP
			}
		} else {
			switch battle.BossRegion {
			case sptboss.PuniSealLife:
				battle.EnemyHP += 2000
				if battle.EnemyHP > battle.EnemyMaxHP {
					battle.EnemyHP = battle.EnemyMaxHP
				}
			case sptboss.PuniSealVoid, sptboss.PuniSealSamsara:
				battle.EnemyHP += 200
				if battle.EnemyHP > battle.EnemyMaxHP {
					battle.EnemyHP = battle.EnemyMaxHP
				}
			}
		}
	}
	// 朵拉格(502) 青龙神兽：每回合恢复 1000 血
	if battle.EnemyID == sptboss.PetIDDuoLaGe && battle.BattleMapID == sptboss.MapIDQingLongSpace && battle.BossRegion == 4 && battle.EnemyHP > 0 {
		battle.EnemyHP += sptboss.QingLongBossRegenHP
		if battle.EnemyHP > battle.EnemyMaxHP {
			battle.EnemyHP = battle.EnemyMaxHP
		}
	}
	// 机械塔克林(1337)：每回合恢复 1000 血（贝塔星 mapID=48）
	if battle.BattleMapID == 48 && battle.EnemyID == 1337 && battle.EnemyHP > 0 {
		battle.EnemyHP += sptboss.QingLongBossRegenHP
		if battle.EnemyHP > battle.EnemyMaxHP {
			battle.EnemyHP = battle.EnemyMaxHP
		}
	}
	// 暗黑第十一门 11-3 奈尼迪亚：每回合额外恢复 500 体力
	if battle.BattleMapID == 513 && battle.EnemyID == 1400 && battle.EnemyHP > 0 {
		if battle.EnemyHP+500 >= battle.EnemyMaxHP {
			battle.EnemyHP = battle.EnemyMaxHP
		} else {
			battle.EnemyHP += 500
		}
	}
	// 亚伦斯(672/5012)：每回合自身恢复100血，我方损失100血
	if sptboss.IsYaLunSiBoss(battle.EnemyID) && battle.EnemyHP > 0 {
		battle.EnemyHP += sptboss.YaLunSiRegenPerTurn
		if battle.EnemyHP > battle.EnemyMaxHP {
			battle.EnemyHP = battle.EnemyMaxHP
		}
		if battle.PlayerHP > 0 {
			if battle.PlayerHP <= sptboss.YaLunSiDrainPerTurn {
				battle.PlayerHP = 0
			} else {
				battle.PlayerHP -= sptboss.YaLunSiDrainPerTurn
			}
		}
	}

	// 我方当前出战精灵 HP 已为 0 时，不允许再出招：只发 2505（双方 0 伤害），然后判定结束或等待 2407 换宠
	battlePetsForCount := getBattlePets(ctx.GameServer, ctx.UserID)
	if battle.PlayerHP == 0 {
		// 多精灵剩余判定改为“遍历实际 HP>0 的精灵”，而不是仅依赖 TotalPlayerPets/DeadPlayerPets，
		// 保证所有精灵实际都 0 血时必然结束战斗；否则交给前端/玩家通过 2407 手动切换。
		hasAlivePet := false
		petMgr := gamepets.GetInstance()
		activeIdxForRemain := battle.ActivePetIndex
		if activeIdxForRemain < 0 || activeIdxForRemain >= len(battlePetsForCount) {
			activeIdxForRemain = 0
		}
		for i := range battlePetsForCount {
			if i == activeIdxForRemain {
				continue
			}
			p := &battlePetsForCount[i]
			stats := petMgr.GetStats(p.ID, p.Level, p.DV, p.GetEVStats(), p.Nature)
			effHP := getPetEffectiveHP(p, stats.MaxHP)
			if effHP > 0 {
				hasAlivePet = true
				logger.Info(fmt.Sprintf("[2405] 发现存活精灵: index=%d PetID=%d HP=%d/%d", i, p.ID, effHP, stats.MaxHP))
				break
			}
		}
		enemyUserID := uint32(0)
		if battle.OpponentUserID != 0 {
			enemyUserID = uint32(battle.OpponentUserID)
		}
		enemyAv := buildAttackValue(enemyUserID, 0, 0, 0, 0, int32(battle.EnemyHP), battle.EnemyMaxHP, 0, 0, 0, battle.EnemyStatus, battle.EnemyBattleLv)
		playerAv := buildAttackValue(uint32(ctx.UserID), 0, 0, 0, 0, 0, battle.PlayerMaxHP, 0, 0, 0, battle.PlayerStatus, battle.PlayerBattleLv)
		body := make([]byte, 0, 164)
		if sptboss.IsFirstStrikeBoss(battle.EnemyID) {
			body = append(body, enemyAv...)
			body = append(body, playerAv...)
		} else {
			body = append(body, playerAv...)
			body = append(body, enemyAv...)
		}
		ctx.GameServer.BattleMu.Unlock()
		ctx.GameServer.SendResponse(ctx.ClientData, 2505, ctx.UserID, ctx.SeqID, body)
		if !hasAlivePet {
			winnerIDForOver := uint32(0)
			if battle.OpponentUserID != 0 {
				winnerIDForOver = uint32(battle.OpponentUserID) // PvP：我方全灭，对方获胜
			}
			overBody := buildFightOverInfo(0, winnerIDForOver)
			// 先 sync 再发 2506：
			// - 普通 PvP：败方需先收 2301 再收 2506 才能正确显示 0 血
			// - 精灵大乱斗：禁止推送 2301/2508 等“背包精灵信息包”，否则客户端会把临时精灵当成背包新增精灵缓存
			var loserPetInfoBody []byte
			ctx.GameServer.BattleMu.Lock()
			syncBattleHPToUser(ctx.GameServer, ctx.UserID, battle)
			activeIdx := battle.ActivePetIndex
			if activeIdx < 0 {
				activeIdx = 0
			}
			if !battle.IsGrandMelee && activeIdx < len(battlePetsForCount) {
				loserPetInfoBody = buildFullPetInfo(battlePetsForCount[activeIdx])
			}
			if battle.OpponentUserID != 0 {
				if ob := ctx.GameServer.BattleStates[battle.OpponentUserID]; ob != nil {
					syncBattleHPToUser(ctx.GameServer, battle.OpponentUserID, ob)
				}
				delete(ctx.GameServer.BattleStates, battle.OpponentUserID)
			}
			delete(ctx.GameServer.BattleStates, ctx.UserID)
			ctx.GameServer.BattleMu.Unlock()
			if battle.IsGrandMelee && battle.OpponentUserID != 0 {
				winnerUID := battle.OpponentUserID
				loserUID := ctx.UserID
				if u := ctx.GameServer.GetOrCreateUser(winnerUID); u != nil {
					u.ExpPool += grandMeleeExpReward
					// 精灵大乱斗胜方胜场 +1
					u.MessWin++
					if ctx.GameServer.UserDB != nil {
						ctx.GameServer.UserDB.SaveGameData(winnerUID, u)
					}
					if winnerClient := ctx.GameServer.GetClientByUserID(winnerUID); winnerClient != nil {
						body8004 := buildBossMonster8004Body(0, 0, 0, 3, uint32(grandMeleeExpReward))
						ctx.GameServer.SendResponse(winnerClient, 8004, winnerUID, 0, body8004)
					}
					logger.Info(fmt.Sprintf("[2431] 精灵大乱斗胜方 UID=%d 获得 %d 可分配经验(败方触发)", winnerUID, grandMeleeExpReward))
				}
				if u := ctx.GameServer.GetOrCreateUser(loserUID); u != nil {
					u.ExpPool += grandMeleeLoserExpReward
					if ctx.GameServer.UserDB != nil {
						ctx.GameServer.UserDB.SaveGameData(loserUID, u)
					}
					if loserClient := ctx.GameServer.GetClientByUserID(loserUID); loserClient != nil {
						body8004 := buildBossMonster8004Body(0, 0, 0, 3, uint32(grandMeleeLoserExpReward))
						ctx.GameServer.SendResponse(loserClient, 8004, loserUID, 0, body8004)
					}
					logger.Info(fmt.Sprintf("[2431] 精灵大乱斗败方 UID=%d 获得 %d 可分配经验", loserUID, grandMeleeLoserExpReward))
				}
				delete(ctx.GameServer.GrandMeleePets, ctx.UserID)
				delete(ctx.GameServer.GrandMeleePets, battle.OpponentUserID)
			}
			if !battle.IsGrandMelee && len(loserPetInfoBody) > 0 {
				ctx.GameServer.SendResponse(ctx.ClientData, 2301, ctx.UserID, 0, loserPetInfoBody)
			}
			ctx.GameServer.SendResponse(ctx.ClientData, 2506, ctx.UserID, ctx.SeqID, overBody)
			pushMapOgreListAfterFightOver(ctx)
			if battle.EnemyID == petIDGaiya {
				gaiyaMap := getGaiyaMapIDForToday()
				if user.MapID != 0 && user.MapID == gaiyaMap {
					gaiyaNote := make([]byte, 8)
					binary.BigEndian.PutUint32(gaiyaNote[0:4], 1)
					binary.BigEndian.PutUint32(gaiyaNote[4:8], petIDGaiya)
					ctx.GameServer.SendResponse(ctx.ClientData, cmdSpecialPetNote, ctx.UserID, 0, gaiyaNote)
				}
			}
			if battle.OpponentUserID != 0 {
				if otherClient := ctx.GameServer.GetClientByUserID(battle.OpponentUserID); otherClient != nil {
					ctx.GameServer.SendResponse(otherClient, 2506, battle.OpponentUserID, 0, overBody)
				}
			}
		}
		return
	}

	// 疲惫 / 睡眠 / 麻痹：可以同时存在，图标各自显示，本回合是否能行动由三者“或”决定
	skipPlayerAction := false
	// 疲惫（status[7]）：使用后若持续>0，每回合结算一次“无法行动”，并递减回合数
	if battle.PlayerStatus[gameskills.StatusIndexFatigue] > 0 {
		skipPlayerAction = true
		battle.PlayerStatus[gameskills.StatusIndexFatigue]--
	}
	// 睡眠（status[8]）：效果与麻痹类似，本回合无法行动，并递减回合数
	if battle.PlayerStatus[gameskills.StatusIndexSleep] > 0 {
		skipPlayerAction = true
		battle.PlayerStatus[gameskills.StatusIndexSleep]--
	}
	// 麻痹（status[0]）：本回合无法行动，并递减回合数
	if battle.PlayerStatus[gameskills.StatusIndexParalysis] > 0 {
		skipPlayerAction = true
		battle.PlayerStatus[gameskills.StatusIndexParalysis]--
	}
	// 害怕（status[6]）：本回合无法行动，并递减回合数
	if battle.PlayerStatus[gameskills.StatusIndexFear] > 0 {
		skipPlayerAction = true
		battle.PlayerStatus[gameskills.StatusIndexFear]--
	}

	// 玩家攻击：计算伤害（完整版）
	petMgr := gamepets.GetInstance()
	skillMgr := gameskills.GetInstance()
	// 当前出战精灵：PvP 时可能使用大乱斗分配的精灵列表（getBattlePets），否则用背包
	battlePets := getBattlePets(ctx.GameServer, ctx.UserID)
	activeIdx := 0
	if battle.ActivePetIndex >= 0 && battle.ActivePetIndex < len(battlePets) {
		activeIdx = battle.ActivePetIndex
	}
	playerPetID := 7
	playerLevel := 5
	playerDV := 31
	if len(battlePets) > 0 && activeIdx < len(battlePets) {
		playerPetID = battlePets[activeIdx].ID
		if battlePets[activeIdx].Level > 0 {
			playerLevel = battlePets[activeIdx].Level
		}
		playerDV = battlePets[activeIdx].DV
	}
	// 获取玩家精灵的性格、EV 与特性
	playerNature := 0
	playerEV := gamepets.EVStats{}
	playerTrait := 0
	if len(battlePets) > 0 && activeIdx < len(battlePets) {
		playerNature = battlePets[activeIdx].Nature
		playerEV = battlePets[activeIdx].GetEVStats()
		playerTrait = battlePets[activeIdx].Trait
	}
	playerStats := petMgr.GetStats(playerPetID, playerLevel, playerDV, playerEV, playerNature)

	// 每当一方使用完技能（造成伤害和附加效果后），结算一次该方此前施加的“多回合固定伤害”
	// 例如“不灭之火”：配置 rounds=5, damage=30，则首次使用那一回合 + 接下来4个回合，
	// 每次自己出手后都会再扣 30 点固定伤害。
	applyFixedDotAfterAttack := func(hp *uint32, dotDamage *uint32, rounds *byte) {
		if hp == nil || dotDamage == nil || rounds == nil {
			return
		}
		if *hp == 0 || *rounds == 0 || *dotDamage == 0 {
			return
		}
		dmg := *dotDamage
		if dmg > *hp {
			dmg = *hp
		}
		*hp -= dmg
		if *rounds > 0 {
			*rounds--
		}
		if *rounds == 0 {
			*dotDamage = 0
		}
	}

	// 获取技能数据
	skill := skillMgr.Get(int(skillID))
	if skill == nil {
		skill = skillMgr.Get(10001) // 默认技能：撞击
	}
	// 百变随心等（SideEffect 47）：自身在若干回合内免疫能力下降效果
	if skill.EffectID == 47 {
		rounds := byte(5)
		if args := gameskills.ParseSideEffectArg(skill.SideEffectArg); len(args) >= 1 && args[0] > 0 {
			if args[0] > 10 {
				rounds = 10
			} else {
				rounds = byte(args[0])
			}
		}
		battle.PlayerStatDropImmuneRounds = rounds
	}

	// 计算玩家本回合是否命中（考虑命中等级与必中）
	//
	// 命中约定：
	// - MustHit=1 或 “下 n 回合必中”(effect 81) 期间：必定命中
	// - 其余情况（包括 Category=4 的变化/属性技能）：按技能 Accuracy + 命中等级计算
	//   （因此像「雷祭」这类 Accuracy=50 的变化技能需要能 MISS）
	playerHit := !skipPlayerAction
	if playerHit {
		// effect 86：n 回合内，属性/变化技能对自身必定 miss（仅限制“对对手施加的属性技能”）
		if skill.Category == 4 && battle.EnemyAttrMoveImmuneRounds > 0 && skill.MustHit != 1 {
			playerHit = false
		} else if skill.MustHit == 1 || battle.PlayerMustHitRounds > 0 {
			playerHit = true
		} else if skill.Category == 1 && battle.EnemyPhysImmuneRounds > 0 {
			// effect 78 敌方：物理攻击对敌方必 miss
			playerHit = false
		} else if skill.Category == 2 && battle.EnemySpecImmuneRounds > 0 {
			// effect 106 敌方：特殊攻击对敌方必 miss
			playerHit = false
		} else {
			baseAcc := skill.Accuracy
			if baseAcc == 0 {
				baseAcc = 100
			}
			// battle.PlayerBattleLv[5] 为命中等级（-6~+6），由技能附加效果维护
			accStage := int(battle.PlayerBattleLv[gameskills.StatAccuracy])
			finalAcc := gamebattle.CalcHitChance(baseAcc, accStage, 0)
			// 特性：精准(1022) —— 命中率 +5%
			if playerTrait == 1022 {
				finalAcc += 5
			}
			if finalAcc > 100 {
				finalAcc = 100
			}
			if finalAcc < 1 {
				finalAcc = 1
			}
			if rand.Intn(100) >= finalAcc {
				playerHit = false
			}
		}
	}

	// 获取敌人精灵数据（敌人 EV 默认为 0）
	enemyPet := petMgr.Get(battle.EnemyID)
	enemyEV := gamepets.EVStats{}
	enemyStats := petMgr.GetStats(battle.EnemyID, battle.EnemyLevel, 15, enemyEV, 0)
	// 勇者之塔（地图 500）：仅第一只精灵攻击+2，多只精灵层仅首只加成
	if battle.BattleMapID == 500 && battle.BraveTowerBossIndex == 0 {
		enemyStats.Attack += 2
	}

	// 先手判定：按技能先制 + 速度比较（雷伊/盖亚 +6；魔狮迪露 体力低于一半时 +6）
	var enemySkillForTurn *gameskills.Skill
	var enemySkillIDForTurn uint32
	if battle.OpponentUserID != 0 {
		// PvP：敌方技能由对方玩家在其 BattleState 上通过 2405 选择，
		// 此处在双方均已选招后，直接读取对方记录的 PvPSelectedSkillID，
		// 而不再调用怪物 AI 的 pickEnemySkill。
		if opponentBattle, ok := ctx.GameServer.BattleStates[battle.OpponentUserID]; ok && opponentBattle.IsActive {
			enemySkillIDForTurn = opponentBattle.PvPSelectedSkillID
			if enemySkillIDForTurn != 0 {
				enemySkillForTurn = skillMgr.Get(int(enemySkillIDForTurn))
			}
		}
	} else {
		// PvE：怪物 AI 逻辑自动选择敌方技能。
		// 哈默雷特(216) 额外：按技能最大 PP 进入战斗并消耗，PP 用尽则不出招。
		if sptboss.IsHaMoLeiTeOrderBoss(battle.EnemyID) {
			enemySkillForTurn, enemySkillIDForTurn = pickEnemySkillWithPP(battle, skillMgr)
		} else {
			enemySkillForTurn, enemySkillIDForTurn = pickEnemySkill(skillMgr, battle.EnemyID, battle.EnemyLevel)
		}
	}
	playerPriority := skill.Priority
	enemyPriority := 0
	isSuperPuni := battle.EnemyID == sptboss.PetIDPuni && battle.BattleMapID == sptboss.MapIDPuniTower &&
		battle.BossRegion == sptboss.PuniTrueForm && battle.PuniTotalLives == 0
	// 暗黑第十一门 11-1 萨洛奇斯：无论使用何种技能，都在先制比较中额外 +20，确保基本先手
	isDark11FirstBoss := battle.BattleMapID == 513 && battle.EnemyID == 1403
	if enemySkillForTurn != nil {
		enemyPriority = enemySkillForTurn.Priority
		// 敌人先制加成仅对 SPT BOSS 生效；PvP 时敌方是玩家精灵，不享受雷伊/盖亚/魔狮迪露等 BOSS 先制+6。
		// 盖亚特训副本(423)中的雷伊战斗，希望去掉先制+6 特性，使用普通先手规则。
		if battle.OpponentUserID == 0 &&
			!(battle.BattleMapID == 423 && battle.EnemyID == 70 && battle.BossRegion == 4) {
			enemyPriority += sptboss.GetPriorityBonusWithHP(battle.EnemyID, battle.EnemyHP, battle.EnemyMaxHP)
		}
		// 谱尼厉害形态：在先手比较中额外 +20，确保必先出手
		if isSuperPuni {
			enemyPriority += 20
		}
		// 暗黑第十一门 11-1 萨洛奇斯：额外 +20，几乎必定先手
		if isDark11FirstBoss {
			enemyPriority += 20
		}
	}
	playerSpeed := int(float64(playerStats.Speed) * gamebattle.GetStatMultiplier(int(battle.PlayerBattleLv[gameskills.StatSpeed])))
	if playerSpeed < 1 {
		playerSpeed = 1
	}
	if battle.PlayerStatus[gameskills.StatusIndexParalysis] > 0 {
		playerSpeed /= 2
		if playerSpeed < 1 {
			playerSpeed = 1
		}
	}
	enemySpeed := int(float64(enemyStats.Speed) * gamebattle.GetStatMultiplier(int(battle.EnemyBattleLv[gameskills.StatSpeed])))
	if enemySpeed < 1 {
		enemySpeed = 1
	}
	if battle.EnemyStatus[gameskills.StatusIndexParalysis] > 0 {
		enemySpeed /= 2
		if enemySpeed < 1 {
			enemySpeed = 1
		}
	}
	enemyFirst := (enemyPriority > playerPriority) ||
		(enemyPriority == playerPriority && enemySpeed > playerSpeed) ||
		(enemyPriority == playerPriority && enemySpeed == playerSpeed && rand.Intn(2) == 1)

	// 记录先手判定日志（便于排查 PvP 先手异常）
	logger.Info(fmt.Sprintf("[Battle-Priority] UID=%d OppUID=%d PlayerPetID=%d EnemyID=%d SkillID=%d EnemySkillID=%d playerPrio=%d enemyPrio=%d playerSpd=%d enemySpd=%d PFirst=%d EFirst=%d enemyFirst=%v",
		ctx.UserID, battle.OpponentUserID, playerPetID, battle.EnemyID, skillID, enemySkillIDForTurn,
		playerPriority, enemyPriority, playerSpeed, enemySpeed,
		battle.PlayerFirstPriorityRounds, battle.EnemyFirstPriorityRounds, enemyFirst))
	// effect 83：自身雄性，下 N 回合必定先手；自身雌性，下 N 回合必定致命一击。
	// 先手强化优先覆盖普通先制判定：仅当一方有“必先手”效果、另一方没有时生效。
	if battle.PlayerFirstPriorityRounds > 0 && battle.EnemyFirstPriorityRounds == 0 {
		enemyFirst = false
	} else if battle.EnemyFirstPriorityRounds > 0 && battle.PlayerFirstPriorityRounds == 0 {
		enemyFirst = true
	}

	// 若使用的是“回避类”技能（SideEffect 52 系列），且玩家在本回合先手且速度 > 敌人，
	// 则标记 EnemyNextAttackMustMiss=true，使敌方下一个攻击技能（非 MustHit）必定 MISS。
	if skill.EffectID == 52 && enemySkillForTurn != nil && !skipPlayerAction && playerHit &&
		!enemyFirst && playerSpeed > enemySpeed {
		battle.EnemyNextAttackMustMiss = true
	}
	isGaiyaFirst := enemyFirst
	// 敌方先手且本回合敌方已将我方击至 0 HP 时，我方未出招，2505 中不显示我方本回合出招
	playerTurnSkippedBecauseDead := false

	// 记录玩家出手前敌人的 HP，用于某些依赖“血量差”的技能（如同生共死）在效果生效后回写真实伤害
	enemyHPBeforeAction := battle.EnemyHP
	// 尤纳斯：本回合是否由里奥斯幻影合法击杀（避免后续锁 1 血误恢复，必须在 goto 前声明）
	var youNaSiKilledByPhantom bool

	// 伤害计算（对齐你的规则）：
	// Category 4 = 变化/状态技能，无威力，不造成伤害；其余 [(Lv*0.4+2)*Power*Atk/Def/50+2] * STAB * TypeMod * Rand
	attackerPet := petMgr.Get(playerPetID)
	power := uint32(skill.Power)
	if power == 0 && skill.Category != 4 {
		power = 40
	}
	// effect 61：威力随机 50~150
	if skill.EffectID == 61 && skill.Category != 4 {
		effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
		minP, maxP := 50, 150
		if len(effArgs) >= 1 {
			minP = effArgs[0]
		}
		if len(effArgs) >= 2 {
			maxP = effArgs[1]
		}
		if maxP < minP {
			maxP = minP
		}
		power = uint32(rand.Intn(maxP-minP+1) + minP)
	}
	// effect 70：威力随机 140~220
	if skill.EffectID == 70 && skill.Category != 4 {
		power = uint32(rand.Intn(81) + 140)
	}
	// effect 118：威力随机 150~220
	if skill.EffectID == 118 && skill.Category != 4 {
		power = uint32(rand.Intn(220-150+1) + 150)
	}

	// damageCalc 为公式计算得到的理论伤害（不考虑“血量上限”这一物理限制）
	// damage      为实际扣掉的 HP（不会超过目标当前 HP，且考虑“手下留情”等效果）
	damageCalc := uint32(0)
	damage := uint32(0)
	// 玩家本次攻击是否暴击标记（用于 2505 isCrit 字段）
	isCritPlayer := false
	if skill.Category != 4 && playerHit && !skipPlayerAction {
		// 选择物理/特殊攻防，并应用强化弱化倍率（battleLv 跟随精灵）
		atk := float64(playerStats.Attack)
		def := float64(enemyStats.Defence)
		atkStage, defStage := 0, 1 // 物理：攻击/防御
		if skill.Category == 2 {
			atk = float64(playerStats.SpAtk)
			def = float64(enemyStats.SpDef)
			atkStage, defStage = 2, 3 // 特殊：特攻/特防
		}
		// effect 51：若自身处于“攻击力与对手相同”，则物理攻击力视为对手当前攻击力（含强化/弱化）
		if skill.Category == 1 && battle.PlayerAttackEqualRounds > 0 {
			atk = float64(enemyStats.Attack) * gamebattle.GetStatMultiplier(int(battle.EnemyBattleLv[gameskills.StatAttack]))
		} else {
			atk *= gamebattle.GetStatMultiplier(int(battle.PlayerBattleLv[atkStage]))
		}
		// effect 45：若对方处于“防御力与对手相同”，则把对方的防御值视为我方当前防御值（仅物理防御）。
		if skill.Category == 1 && battle.EnemyDefenceEqualRounds > 0 {
			def = float64(playerStats.Defence) * gamebattle.GetStatMultiplier(int(battle.PlayerBattleLv[gameskills.StatDefence]))
		} else {
			def *= gamebattle.GetStatMultiplier(int(battle.EnemyBattleLv[defStage]))
		}
		if def < 1 {
			def = 1
		}
		// 基础伤害（向下取整）
		baseDamage := math.Floor(((float64(playerLevel)*0.4 + 2.0) * float64(power) * atk / def / 50.0) + 2.0)

		// 计算当前回合用于 STAB/克制判定的临时属性（支持 effect 55/56 的属性交换/复制）
		attType1, attType2 := 0, 0
		if attackerPet != nil {
			attType1, attType2 = attackerPet.Type, attackerPet.Type2
		}
		defType1, defType2 := 0, 0
		if enemyPet != nil {
			defType1, defType2 = enemyPet.Type, enemyPet.Type2
		}
		// 我方临时属性覆盖
		if battle.PlayerTypeOverrideRounds > 0 {
			if battle.PlayerTypeOverride1 > 0 {
				attType1 = int(battle.PlayerTypeOverride1)
			}
			if battle.PlayerTypeOverride2 > 0 {
				attType2 = int(battle.PlayerTypeOverride2)
			}
		}
		// 敌方法临时属性覆盖
		if battle.EnemyTypeOverrideRounds > 0 {
			if battle.EnemyTypeOverride1 > 0 {
				defType1 = int(battle.EnemyTypeOverride1)
			}
			if battle.EnemyTypeOverride2 > 0 {
				defType2 = int(battle.EnemyTypeOverride2)
			}
		}

		// STAB（同属性加成，1.5倍）
		stab := 1.0
		if attType1 > 0 && skill.Type == attType1 || (attType2 > 0 && skill.Type == attType2) {
			stab = 1.5
		}

		// 属性克制（双属性防守相乘）
		typeMod := 1.0
		if defType1 > 0 {
			typeMod = gamebattle.GetTypeMultiplierWithAttackerDual(skill.Type, attType1, attType2, defType1, defType2)
		}

		// 随机（217~255）
		randomMod := float64(rand.Intn(255-217+1)+217) / 255.0

		// 暴击：1/16 基础概率（CritRate 表示几分之十六，默认 1）
		critRate := skill.CritRate
		if critRate == 0 {
			critRate = 1
		}
		// effect 95：对手睡眠时致命一击率 +n/16
		if skill.EffectID == 95 && battle.EnemyStatus[gameskills.StatusIndexSleep] > 0 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			bonus := 4
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				bonus = effArgs[0]
			}
			critRate += bonus
			if critRate > 16 {
				critRate = 16
			}
		}
		// 暴击强化：
		// 1) 当存在“下 N 回合攻击技能必定致命一击”效果时（PlayerCritBuffRounds），直接视为 100% 暴击率。
		// 2) 当存在“后续 N 次攻击技能必定致命一击”效果时（PlayerNextGuaranteedCritHits），本次攻击直接必暴击，并在命中后消耗 1 次。
		// 属性技能（Category=4）本身不走该分支，因此不会暴击。
		hasGuaranteedCritThisHit := battle.PlayerNextGuaranteedCritHits > 0
		if battle.PlayerCritBuffRounds > 0 || hasGuaranteedCritThisHit {
			critRate = 16
		} else {
			// 暴击率提升（effect 32）：下 N 回合暴击率 +1 阶（+1/16），在 Buff 不生效时叠加。
			if battle.PlayerCritBonusRounds > 0 && critRate < 16 {
				critRate++
			}
		}
		// 特性：会心(1023) —— 额外 +1/16 暴击率（上限 16）
		if playerTrait == 1023 && critRate < 16 {
			critRate++
		}
		isCritPlayer = rand.Intn(16) < critRate
		if hasGuaranteedCritThisHit {
			isCritPlayer = true
		}
		// 暴击伤害：对齐“正常攻击伤害的两倍”规则
		critMod := 1.0
		if isCritPlayer {
			critMod = 2.0
			// 暴击后清除对方的防御/特防提升状态（仅清除 >0 的强化，保留减防 debuff）
			if skill.Category == 1 {
				// 物理攻击：清空对方防御提升
				if battle.EnemyBattleLv[gameskills.StatDefence] > 0 {
					battle.EnemyBattleLv[gameskills.StatDefence] = 0
				}
			} else if skill.Category == 2 {
				// 特殊攻击：清空对方特防提升
				if battle.EnemyBattleLv[gameskills.StatSpDef] > 0 {
					battle.EnemyBattleLv[gameskills.StatSpDef] = 0
				}
			}
		}
		// 若本次暴击来自“后续 N 次必暴击”（跨精灵）效果，则在本次攻击确实命中且为攻击技能时消耗 1 次。
		// 这里位于 playerHit==true 的分支中，因此不会因为 MISS 消耗次数。
		if hasGuaranteedCritThisHit && battle.PlayerNextGuaranteedCritHits > 0 {
			battle.PlayerNextGuaranteedCritHits--
		}

		// 理论伤害（包含各种加成与随机，但还未考虑“血量上限”等裁剪）
		damageCalc = uint32(baseDamage * stab * typeMod * randomMod * critMod)
		// effect 113：个体越高威力越大（按照“DV*5”作为技能基础威力）
		if skill.EffectID == 113 && playerDV > 0 {
			power = uint32(playerDV * 5)
			if power == 0 {
				power = 1
			}
			// 重新计算基础伤害（使用新的 power）
			baseDamage = math.Floor(((float64(playerLevel)*0.4 + 2.0) * float64(power) * atk / def / 50.0) + 2.0)
		}

		// 若目标处于“易燃”状态，则火系技能额外乘固定倍率（1.5 倍）
		if damageCalc > 0 && skill.Type == sptboss.TypeFire &&
			battle.EnemyStatus[gameskills.StatusIndexFlammable] > 0 {
			mul := uint64(damageCalc) * 3 / 2
			if mul > math.MaxUint32 {
				damageCalc = math.MaxUint32
			} else {
				damageCalc = uint32(mul)
			}
		}

		// effect 100：自身体力越少则威力越大。
		// 这里按照“(1 + 缺血比例) 倍伤害”实现：满血=1倍，半血≈1.5倍，残血接近2倍。
		if skill.EffectID == 100 && battle.PlayerMaxHP > 0 {
			missingRatio := 1.0 - float64(battle.PlayerHP)/float64(battle.PlayerMaxHP)
			if missingRatio < 0 {
				missingRatio = 0
			}
			if missingRatio > 1 {
				missingRatio = 1
			}
			scale := 1.0 + missingRatio
			if scale < 1.0 {
				scale = 1.0
			}
			scaled := float64(damageCalc) * scale
			if scaled > float64(math.MaxUint32) {
				damageCalc = math.MaxUint32
			} else {
				damageCalc = uint32(scaled)
			}
		}
		// effect 111：附加额外伤害，自身等级越高，附加的伤害越高（这里按 2×等级 追加固定伤害）
		if skill.EffectID == 111 && damageCalc > 0 && playerLevel > 0 {
			add := uint64(playerLevel) * 2
			sum := uint64(damageCalc) + add
			if sum > math.MaxUint32 {
				damageCalc = math.MaxUint32
			} else {
				damageCalc = uint32(sum)
			}
		}
		// effect 129：对方为指定性别则技能威力翻倍（按伤害翻倍处理）
		if skill.EffectID == 129 && enemyPet != nil {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			if len(effArgs) >= 1 && enemyPet.Gender == effArgs[0] && damageCalc > 0 {
				mul := uint64(damageCalc) * 2
				if mul > math.MaxUint32 {
					damageCalc = math.MaxUint32
				} else {
					damageCalc = uint32(mul)
				}
			}
		}
		// effect 102：对手处于麻痹状态时威力翻倍
		if skill.EffectID == 102 && battle.EnemyStatus[gameskills.StatusIndexParalysis] > 0 && damageCalc > 0 {
			mul := uint64(damageCalc) * 2
			if mul > math.MaxUint32 {
				damageCalc = math.MaxUint32
			} else {
				damageCalc = uint32(mul)
			}
		}
		// effect 130：对方为指定性别则附加固定伤害
		if skill.EffectID == 130 && enemyPet != nil {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			if len(effArgs) >= 2 && enemyPet.Gender == effArgs[0] && effArgs[1] > 0 {
				add := uint64(damageCalc) + uint64(effArgs[1])
				if add > math.MaxUint32 {
					damageCalc = math.MaxUint32
				} else {
					damageCalc = uint32(add)
				}
			}
		}
		// effect 131：对方为指定性别则免疫当前回合伤害
		if skill.EffectID == 131 && enemyPet != nil {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			if len(effArgs) >= 1 && enemyPet.Gender == effArgs[0] {
				damageCalc = 0
			}
		}
		// effect 42：n 回合电系技能伤害×2
		if battle.PlayerElectricPowerRounds > 0 && skill.Type == 5 && damageCalc > 0 {
			mul := uint64(damageCalc) * 2
			if mul > math.MaxUint32 {
				damageCalc = math.MaxUint32
			} else {
				damageCalc = uint32(mul)
			}
		}
		// effect 65：n 回合内 elemtype 系技能威力×m
		if battle.PlayerTypePowerRounds > 0 && battle.PlayerTypePowerMul > 1 &&
			skill.Type == int(battle.PlayerTypePowerType) && damageCalc > 0 {
			mul := uint64(damageCalc) * uint64(battle.PlayerTypePowerMul)
			if mul > math.MaxUint32 {
				damageCalc = math.MaxUint32
			} else {
				damageCalc = uint32(mul)
			}
		}
		// effect 82：目标为雄性伤害 200%，雌性 50%（无性别不变）
		if skill.EffectID == 82 && damageCalc > 0 && enemyPet != nil {
			switch enemyPet.Gender {
			case 1: // 雄性
				mul := uint64(damageCalc) * 2
				if mul > math.MaxUint32 {
					damageCalc = math.MaxUint32
				} else {
					damageCalc = uint32(mul)
				}
			case 2: // 雌性
				damageCalc = damageCalc / 2
				if damageCalc < 1 {
					damageCalc = 1
				}
			}
		}
		// effect 98：n 回合内，对雄性精灵的伤害为 m 倍
		if battle.PlayerMaleDmgMulRounds > 0 && battle.PlayerMaleDmgMul > 1 &&
			enemyPet != nil && enemyPet.Gender == 1 && damageCalc > 0 {
			mul := uint64(damageCalc) * uint64(battle.PlayerMaleDmgMul)
			if mul > math.MaxUint32 {
				damageCalc = math.MaxUint32
			} else {
				damageCalc = uint32(mul)
			}
		}

		// 自身体力低于 1/n 时威力 m 倍（effect 37）
		if damageCalc > 0 && skill.EffectID == 37 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			n, m := 2, 2
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				n = effArgs[0]
			}
			if len(effArgs) >= 2 && effArgs[1] > 0 {
				m = effArgs[1]
			}
			if battle.PlayerMaxHP > 0 && battle.PlayerHP*uint32(n) < battle.PlayerMaxHP {
				mul := uint64(damageCalc) * uint64(m)
				if mul > math.MaxUint32 {
					damageCalc = math.MaxUint32
				} else {
					damageCalc = uint32(mul)
				}
			}
		}

		// 先/后手威力翻倍（effect 40/30）：
		// - effect 40：先出手威力为 2 倍
		// - effect 30：后出手威力为 2 倍
		// enemyFirst=true 表示敌方先手；因此玩家先手 = !enemyFirst
		if damageCalc > 0 {
			if skill.EffectID == 40 && !enemyFirst {
				mul := uint64(damageCalc) * 2
				if mul > math.MaxUint32 {
					damageCalc = math.MaxUint32
				} else {
					damageCalc = uint32(mul)
				}
			} else if skill.EffectID == 30 && enemyFirst {
				mul := uint64(damageCalc) * 2
				if mul > math.MaxUint32 {
					damageCalc = math.MaxUint32
				} else {
					damageCalc = uint32(mul)
				}
			}
		}

		// effect 73：若先手攻击，则在后续 n 回合内受到攻击时 2 倍反击给对手
		// enemyFirst=true 表示敌方先手；因此玩家先手 = !enemyFirst
		if skill.EffectID == 73 && !enemyFirst {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds := 1
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			battle.PlayerCounterDoubleRounds = byte(rounds + 1)
		}

		// 特性：叶绿/流水/炎火/.../圣灵 (1006-1021)
		// 对应 Args: type 5%，当技能属性与特性匹配时，伤害额外 +5%
		if playerTrait >= 1006 && playerTrait <= 1021 && skill.Category != 4 {
			targetType := playerTrait - 1005 // 1006->1, 1007->2, ..., 1021->16
			if skill.Type == targetType {
				mul := uint64(damageCalc) * 105
				mul /= 100
				if mul > math.MaxUint32 {
					damageCalc = math.MaxUint32
				} else {
					damageCalc = uint32(mul)
				}
			}
		}

		// 多段攻击（effect 31）：按参数随机命中次数，直接折算为单次伤害倍数
		// 标准配置：SideEffect="31" SideEffectArg="minHits maxHits"
		// 复合配置：SideEffect="422 31" SideEffectArg="X minHits maxHits"
		if skill.EffectID == 31 || strings.Contains(skill.SideEffect, "31") {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			minHits, maxHits := 2, 5
			
			if skill.EffectID == 31 {
				// 纯 31 效果：min/max 直接取前两个参数
				if len(effArgs) >= 1 {
					minHits = effArgs[0]
				}
				if len(effArgs) >= 2 {
					maxHits = effArgs[1]
				}
			} else {
				// 复合效果（如 "422 31"）：第一个参数通常留给其它效果（如 422 的 X%），
				// 多段命中次数从第二、三个参数读取。
				if len(effArgs) >= 2 {
					minHits = effArgs[1]
				}
				if len(effArgs) >= 3 {
					maxHits = effArgs[2]
				}
			}
			
			if minHits < 1 {
				minHits = 1
			}
			if maxHits < minHits {
				maxHits = minHits
			}
			hits := rand.Intn(maxHits-minHits+1) + minHits
			// 折算倍数，避免溢出
			if hits > 1 {
				mul := uint64(damageCalc) * uint64(hits)
				if mul > math.MaxUint32 {
					damageCalc = math.MaxUint32
				} else {
					damageCalc = uint32(mul)
				}
			}
		}

		// effect 36 秒杀（极度冰点等）：命中时 n% 概率秒杀对方；除魔狮迪露外所有 BOSS 免疫
		// 特性 1027“瞬杀”：进攻类技能有 3% 几率秒杀对方（仅 PvE，对战赛尔间无效）。
		instantKill := false
		
		// 【测试用】擒九域(10712)技能：100%秒杀效果（用于测试，后续可移除）
		if int(skillID) == 10712 && battle.EnemyHP > 0 {
			damageCalc = battle.EnemyHP
			instantKill = true
			logger.Info(fmt.Sprintf("[2405] 擒九域技能触发：100%%秒杀 EnemyHP=%d", battle.EnemyHP))
		} else if skill.EffectID == 36 {
			if !sptboss.IsExtremeFreezeImmune(battle.EnemyID) {
				effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
				chance := 5
				if len(effArgs) >= 1 {
					chance = effArgs[0]
				}
				if rand.Intn(100) < chance && battle.EnemyHP > 0 {
					damageCalc = battle.EnemyHP
					instantKill = true
				}
			}
		} else if playerTrait == 1027 && skill.Category != 4 {
			// 特性：瞬杀(1027) —— 进攻类技能有 3% 几率秒杀对方（物理/特殊技能，变化技无效；PVE/PVP 均生效）
			if battle.EnemyHP > 0 && rand.Intn(100) < 3 {
				damageCalc = battle.EnemyHP
				instantKill = true
			}
		}

		// 从理论伤害得到实际扣血值
		finalDamage := damageCalc
		if instantKill {
			finalDamage = battle.EnemyHP
		} else if skill.EffectID == 34 {
			// 克制：在这里不结算反弹伤害，只将本次公式伤害置为 0，
			// 真正的反弹逻辑放到敌人攻击结束之后统一处理，
			// 以便拿到本回合敌人实际对我造成的伤害。
			finalDamage = 0
			damageCalc = 0
		} else {
			// 惩罚（effect 35）：对方能力等级越高伤害越高，附加 sum(正能力等级)*20
			if skill.EffectID == 35 {
				bonus := 0
				for i := 0; i < 6; i++ {
					if battle.EnemyBattleLv[i] > 0 {
						bonus += int(battle.EnemyBattleLv[i]) * 20
					}
				}
				finalDamage += uint32(bonus)
			}
			// 伤害倍增（effect 88）：n% 几率伤害为 m 倍
			if skill.EffectID == 88 && !instantKill && finalDamage > 0 {
				effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
				chance, mult := 20, 2
				if len(effArgs) >= 1 && effArgs[0] > 0 {
					chance = effArgs[0]
				}
				if len(effArgs) >= 2 && effArgs[1] > 0 {
					mult = effArgs[1]
				}
				if chance > 0 && mult > 1 && rand.Intn(100) < chance {
					mul := uint64(finalDamage) * uint64(mult)
					if mul > math.MaxUint32 {
						finalDamage = math.MaxUint32
					} else {
						finalDamage = uint32(mul)
					}
					// 公式伤害也按相同倍数放大，便于日志统计与后续效果使用
					mul2 := uint64(damageCalc) * uint64(mult)
					if mul2 > math.MaxUint32 {
						damageCalc = math.MaxUint32
					} else {
						damageCalc = uint32(mul2)
					}
				}
			}
			// 额外固定伤害（effect 93）：n% 几率额外附加 m 点固定伤害
			if skill.EffectID == 93 && !instantKill && finalDamage > 0 {
				effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
				chance, extra := 30, 30
				if len(effArgs) >= 1 && effArgs[0] > 0 {
					chance = effArgs[0]
				}
				if len(effArgs) >= 2 && effArgs[1] > 0 {
					extra = effArgs[1]
				}
				if chance > 0 && extra > 0 && rand.Intn(100) < chance {
					inc := uint32(extra)
					// 实际伤害不能超过敌人当前 HP
					if inc > battle.EnemyHP-finalDamage {
						inc = battle.EnemyHP - finalDamage
					}
					finalDamage += inc
					damageCalc += inc
				}
			}
			// effect 64：自身烧伤/冻伤/中毒时伤害加倍
			if damageCalc > 0 && skill.EffectID == 64 {
				hasDoT := battle.PlayerStatus[gameskills.StatusIndexBurn] > 0 ||
					battle.PlayerStatus[gameskills.StatusIndexFreeze] > 0 ||
					battle.PlayerStatus[gameskills.StatusIndexPoison] > 0
				if hasDoT {
					mul := uint64(damageCalc) * 2
					if mul > math.MaxUint32 {
						damageCalc = math.MaxUint32
					} else {
						damageCalc = uint32(mul)
					}
				}
			}
			// 对手处于烧伤/冻伤时威力翻倍（effect 96/97）
			if damageCalc > 0 {
				if skill.EffectID == 96 && battle.EnemyStatus[gameskills.StatusIndexBurn] > 0 {
					mul := uint64(damageCalc) * 2
					if mul > math.MaxUint32 {
						damageCalc = math.MaxUint32
					} else {
						damageCalc = uint32(mul)
					}
				}
				if skill.EffectID == 97 && battle.EnemyStatus[gameskills.StatusIndexFreeze] > 0 {
					mul := uint64(damageCalc) * 2
					if mul > math.MaxUint32 {
						damageCalc = math.MaxUint32
					} else {
						damageCalc = uint32(mul)
					}
				}
			}
			// 烧伤效果：被烧伤方造成的伤害减半
			if battle.PlayerStatus[gameskills.StatusIndexBurn] > 0 {
				finalDamage = finalDamage / 2
				if finalDamage < 1 {
					finalDamage = 1
				}
			}
			// 特性：坚硬(1024) —— 受到的伤害减少5%（仅作用于敌人对我方造成的伤害；此处为我方进攻，不处理）
			if finalDamage < 1 {
				finalDamage = 1
			}
			// effect 54：n 回合对方所受伤害为 1/m（当我方使用 54 时）或
			// 敌方有 54 时：EnemyDamageHalved 由我方施放，作用于敌方
			if battle.EnemyDamageHalvedRounds > 0 && battle.EnemyDamageHalvedDenom > 1 {
				finalDamage = finalDamage / uint32(battle.EnemyDamageHalvedDenom)
				if finalDamage < 1 && battle.EnemyHP > 0 {
					finalDamage = 1
				}
			}
			// effect 90：n 回合自身造成伤害×m
			if battle.PlayerDmgMulRounds > 0 && battle.PlayerDmgMul > 1 {
				mul := uint64(finalDamage) * uint64(battle.PlayerDmgMul)
				if mul > math.MaxUint32 {
					finalDamage = math.MaxUint32
				} else {
					finalDamage = uint32(mul)
				}
			}
			// 敌方 effect 46：抵挡 N 次攻击
			if battle.EnemyShieldRounds > 0 && finalDamage > 0 {
				battle.EnemyShieldRounds--
				finalDamage = 0
			}
			// 敌方 effect 41：火系伤害减半（我方对敌方的火系伤害）
			if battle.EnemyFireResistRounds > 0 && skill.Type == 3 && finalDamage > 0 {
				finalDamage = finalDamage / 2
				if finalDamage < 1 && battle.EnemyHP > 0 {
					finalDamage = 1
				}
			}
			// 实际扣血不能超过目标当前 HP
			if finalDamage > battle.EnemyHP {
				finalDamage = battle.EnemyHP
			}
			// 同生共死（effect 7）：仅当对方血量高于我方时生效，否则造成 0 伤害（不把对方打死）
			if skill.EffectID == 7 && battle.EnemyHP <= battle.PlayerHP {
				finalDamage = 0
				damageCalc = 0
			}
			// 手下留情（effect 8）：伤害正常判定，但若伤害≥对方剩余体力则改为剩余体力-1（留 1 滴血）
			// 同时把 damageCalc 改为实际伤害，否则 2505 里用 damageCalc 作 lostHP 会显示成公式伤害（如 1000+）
			if skill.EffectID == 8 && finalDamage >= battle.EnemyHP && battle.EnemyHP > 0 {
				finalDamage = battle.EnemyHP - 1
				damageCalc = finalDamage
			}
		}
		damage = finalDamage
	}
	// 疲惫或未命中时本回合不造成直接伤害
	if skipPlayerAction || !playerHit {
		damageCalc = 0
		damage = 0
	}
	// effect 110 敌方处于“躲避提升”状态时：若敌方成功躲避我方本次攻击，有 m% 几率提升敌方对应能力 1 级
	if !skipPlayerAction && !playerHit && skill.Category != 4 &&
		battle.EnemyOnDodgeBuffRounds > 0 && battle.EnemyOnDodgeBuffChance > 0 {
		if rand.Intn(100) < int(battle.EnemyOnDodgeBuffChance) {
			statIdx := int(battle.EnemyOnDodgeBuffStat)
			if statIdx >= 0 && statIdx < len(battle.EnemyBattleLv) {
				if battle.EnemyBattleLv[statIdx] < 6 {
					battle.EnemyBattleLv[statIdx]++
				}
			}
		}
	}
	// 敌方本回合变量（盖亚先手时在“我方出招”前结算，非盖亚时在“我方出招”后结算）
	enemyDamage := uint32(0)
	enemyDamageCalc := uint32(0)
	enemySkillID := uint32(0)
	enemyAttempted := false
	enemyHitFor2505 := false
	isCritEnemyFor2505 := false
	playerHPBeforeEnemy := battle.PlayerHP
	effectGainHP := int32(0)
	enemyEffectGainHP := int32(0)
	// 自杀/牺牲类技能标记：若我方在出招后因自身效果导致 HP=0，则本回合不再执行敌方反击
	playerFaintedFromOwnSkillThisTurn := false

	// effect 72：若没有命中对手则自己失去全部体力（仅当本回合尝试出招且为攻击技能）
	if !skipPlayerAction && skill.Category != 4 && skill.EffectID == 72 && !playerHit {
		battle.PlayerHP = 0
		playerFaintedFromOwnSkillThisTurn = true
	}

	// 盖亚先手：先执行敌方回合，再执行我方（若仍存活）；非盖亚则先执行我方再执行敌方
	fromGaiyaFirst := false
	var doEnemyTurn func() // 定义在 runEnemyTurnFirst，此处仅声明避免 goto 跳过声明
	if isGaiyaFirst {
		goto runEnemyTurnFirst
	}
doPlayerTurn:
	// 非被控制状态时才扣 PP 并结算技能；盖亚战时仅当我方仍存活才结算我方出招
	if !skipPlayerAction && skillID > 0 {
		// 扣除本回合使用技能的 PP，并持久化到战毕写回
		for i := 0; i < 4; i++ {
			if battle.PlayerSkillIDs[i] == int(skillID) {
				if battle.PlayerSkillPP[i] > 0 {
					battle.PlayerSkillPP[i]--
				}
				break
			}
		}
	}
	if !skipPlayerAction && (!isGaiyaFirst || battle.PlayerHP > 0) {
		if playerHit {
			// 谱尼虚无：除必中技能外 100% MISS（封印1；真身第1/5命拥有，且可被时空感应消除）
			hasVoid := battle.EnemyID == sptboss.PetIDPuni && battle.BattleMapID == sptboss.MapIDPuniTower &&
				((battle.BossRegion == sptboss.PuniSealVoid) ||
					(battle.BossRegion == sptboss.PuniTrueForm && battle.PuniTotalLives == 6 && sptboss.PuniTrueFormLifeHasVoidEternal(battle.PuniCurrentLife) && !battle.PuniTrueFormSuppressed))
			if hasVoid && skill.MustHit != 1 {
				damage = 0
				damageCalc = 0
				playerHit = false
			}
			// 谱尼元素：仅光(12)暗影(13)交替或异常 DoT 有效，普通攻击伤害为 0（封印2；真身第2命）
			isElement := playerHit && battle.EnemyID == sptboss.PetIDPuni && battle.BattleMapID == sptboss.MapIDPuniTower &&
				((battle.BossRegion == sptboss.PuniSealElement) ||
					(battle.BossRegion == sptboss.PuniTrueForm && battle.PuniTotalLives == 6 && sptboss.PuniTrueFormLifeIsElement(battle.PuniCurrentLife)))
			if isElement && skill.Category != 4 {
				if skill.Type != 12 && skill.Type != 13 {
					damage = 0
					damageCalc = 0
				} else if battle.PuniElementLastType == byte(skill.Type) {
					damage = 0
					damageCalc = 0
				} else {
					battle.PuniElementLastType = byte(skill.Type)
				}
			}
			// 谱尼能量：单次伤害超过阈值则攻击者死亡（封印3=1000；真身第3命=100）
			if playerHit && damage > 0 && battle.EnemyID == sptboss.PetIDPuni && sptboss.IsPuniSealBoss(battle.BattleMapID, battle.BossRegion) {
				isEnergySeal := battle.BossRegion == sptboss.PuniSealEnergy
				isEnergyLife3 := battle.BossRegion == sptboss.PuniTrueForm && battle.PuniTotalLives == 6 && sptboss.PuniTrueFormLifeIsEnergy(battle.PuniCurrentLife)
				if isEnergySeal || isEnergyLife3 {
					thr := uint32(1000)
					if isEnergyLife3 {
						thr = sptboss.PuniTrueFormLifeEnergyThreshold(battle.PuniCurrentLife)
					}
					if damage > thr {
						damage = thr
						damageCalc = thr
						battle.PlayerHP = 0
					}
				}
			}
			// 谱尼永恒/圣洁：我方攻击伤害减半（永恒 + 圣洁 都减半）
			if playerHit && damage > 0 && battle.EnemyID == sptboss.PetIDPuni && sptboss.IsPuniSealBoss(battle.BattleMapID, battle.BossRegion) &&
				(battle.BossRegion == sptboss.PuniSealEternal || battle.BossRegion == sptboss.PuniSealHoly ||
					(battle.BossRegion == sptboss.PuniTrueForm && battle.PuniTotalLives == 6 && sptboss.PuniTrueFormLifeHasEternal(battle.PuniCurrentLife))) {
				damage = damage / 2
				if damage == 0 && damageCalc > 0 {
					damage = 1
				}
				damageCalc = damage
			}
			// 谱尼真身第1/5命：时空感应(20300) 可消除“虚无+永恒”
			if playerHit && battle.EnemyID == sptboss.PetIDPuni && battle.BattleMapID == sptboss.MapIDPuniTower &&
				battle.BossRegion == sptboss.PuniTrueForm && battle.PuniTotalLives == 6 && sptboss.PuniTrueFormLifeHasVoidEternal(battle.PuniCurrentLife) &&
				skillID == 20300 {
				battle.PuniTrueFormSuppressed = true
			}
			// 哈莫雷特(216)：必须始终按 水系(2)→火系(3)→草系(1) 循环命中才能受伤，否则伤害为 0（每击都校验顺序）
			if playerHit && sptboss.IsHaMoLeiTeOrderBoss(battle.EnemyID) && skill.Category != 4 {
				required := sptboss.HaMoLeiTeRequiredType(battle.HaMoLeiTePhase)
				if skill.Type != required {
					damage = 0
					damageCalc = 0
				} else {
					battle.HaMoLeiTePhase = (battle.HaMoLeiTePhase + 1) % 3
				}
			}
			// 尤纳斯(132)：未受贯穿水枪前所受攻击伤害为 0；受贯穿水枪后正常受伤但保留 1 血，仅里奥斯(42)的幻影(10100)可击杀
			if sptboss.IsYouNaSiBoss(battle.EnemyID) && skill.Category != 4 {
				if battle.YouNaSiPhase == 0 {
					if skill.ID != sptboss.SkillIDPiercingWater {
						damage = 0
						damageCalc = 0
					} else {
						battle.YouNaSiPhase = 1
					}
				} else if battle.YouNaSiPhase == 1 && damage >= battle.EnemyHP {
					// 本击会致死：仅里奥斯幻影允许击杀
					if skill.ID != sptboss.SkillIDPhantom || playerPetID != sptboss.PetIDLiAoS {
						damage = battle.EnemyHP - 1
						if damage > battle.EnemyHP {
							damage = 0
						}
						damageCalc = damage
					} else {
						youNaSiKilledByPhantom = true // 本回合幻影击杀，后续不再执行锁 1 血
					}
				}
			}
			// 魔狮迪露(187) 等：受到的攻击伤害（非异常）乘 N 倍
			mult := sptboss.GetDamageTakenMultiplier(battle.EnemyID)
			if mult > 1 && damage > 0 {
				damage = damage * uint32(mult)
				if damage > battle.EnemyHP {
					damage = battle.EnemyHP
				}
				damageCalc = damage
			}
			// effect 68 敌方：致死攻击时余下 1 点体力（PvP 等）
			if battle.EnemyEndureRounds > 0 && damage >= battle.EnemyHP && battle.EnemyHP > 0 {
				damage = battle.EnemyHP - 1
				if battle.EnemyHP == 1 {
					damage = 0
				}
				damageCalc = damage
				battle.EnemyEndureRounds--
			}
			battle.EnemyHP -= damage
			if battle.EnemyHP > battle.EnemyMaxHP {
				battle.EnemyHP = 0 // uint 下溢时变为极大值，统一置 0
			}
			// 敌方处于“反击2倍”状态时，把本次受到的实际伤害 *2 反击给我方
			if battle.EnemyCounterDoubleRounds > 0 && damage > 0 {
				reflect := uint64(damage) * 2
				if reflect > uint64(battle.PlayerHP) {
					reflect = uint64(battle.PlayerHP)
				}
				if reflect > 0 {
					battle.PlayerHP -= uint32(reflect)
					if battle.PlayerHP > battle.PlayerMaxHP {
						battle.PlayerHP = 0
					}
				}
			}
			// effect 112：暗影自爆类 —— 牺牲自身全部体力，随机造成 250~300 固定伤害；若本应致死则保留对方 1 点体力
			if skill.EffectID == 112 {
				if battle.EnemyHP > 0 {
					randDelta := rand.Intn(51) // 0~50
					raw := uint32(250 + randDelta)
					if raw >= battle.EnemyHP {
						if battle.EnemyHP > 1 {
							damage = battle.EnemyHP - 1
							damageCalc = damage
						} else {
							// 对方只剩 1 血时，自爆不造成伤害
							damage = 0
							damageCalc = 0
						}
					} else {
						damage = raw
						damageCalc = raw
					}
				}
				// 自身体力清零，本回合不再执行敌方行动
				battle.PlayerHP = 0
				playerFaintedFromOwnSkillThisTurn = true
			}
			// effect 109：n 回合内，对手受到本次伤害时，有 m% 几率被冻伤
			if damage > 0 && battle.PlayerOnDamageFreezeRounds > 0 && battle.PlayerOnDamageFreezeChance > 0 &&
				battle.EnemyStatus[gameskills.StatusIndexFreeze] == 0 {
				if rand.Intn(100) < int(battle.PlayerOnDamageFreezeChance) {
					battle.EnemyStatus[gameskills.StatusIndexFreeze] = 2
				}
			}
			// effect 107：若本次造成的实际伤害小于阈值，则自身对应能力提升 1 级
			if skill.EffectID == 107 && enemyHPBeforeAction > battle.EnemyHP {
				actualLoss := enemyHPBeforeAction - battle.EnemyHP
				effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
				if len(effArgs) >= 2 {
					threshold := effArgs[0]
					statIdx := effArgs[1]
					if threshold > 0 && int(actualLoss) < threshold && statIdx >= 0 && statIdx < len(battle.PlayerBattleLv) {
						if battle.PlayerBattleLv[statIdx] < 6 {
							battle.PlayerBattleLv[statIdx]++
						}
					}
				}
			}
			// effect 101：按伤害百分比吸血（伤害数值的 n% 恢复自身体力）
			if skill.EffectID == 101 && damage > 0 && battle.PlayerMaxHP > 0 {
				effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
				percent := 0
				if len(effArgs) >= 1 {
					percent = effArgs[0]
				}
				if percent > 0 {
					heal := uint32(uint64(damage) * uint64(percent) / 100)
					if heal > 0 {
						newHP := battle.PlayerHP + heal
						if newHP > battle.PlayerMaxHP {
							newHP = battle.PlayerMaxHP
						}
						battle.PlayerHP = newHP
						effectGainHP += int32(heal)
					}
				}
			}
			// effect 105：按伤害的 1/n 吸血
			if skill.EffectID == 105 && damage > 0 && battle.PlayerMaxHP > 0 {
				effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
				denom := 0
				if len(effArgs) >= 1 {
					denom = effArgs[0]
				}
				if denom > 0 {
					heal := damage / uint32(denom)
					if heal > 0 {
						newHP := battle.PlayerHP + heal
						if newHP > battle.PlayerMaxHP {
							newHP = battle.PlayerMaxHP
						}
						battle.PlayerHP = newHP
						effectGainHP += int32(heal)
					}
				}
			}
			if battle.EnemyHP == 0 {
				// 谱尼轮回封印(5)：需击败 2 次
				if battle.EnemyID == sptboss.PetIDPuni && battle.BattleMapID == sptboss.MapIDPuniTower &&
					battle.BossRegion == sptboss.PuniSealSamsara && battle.PuniTotalLives == 2 && battle.PuniCurrentLife < 2 {
					battle.PuniCurrentLife = 2
					battle.EnemyHP = battle.EnemyMaxHP
				} else if battle.BattleMapID == sptboss.MapIDYiNengWang && battle.BossRegion == 6 && battle.EnemyID == 1000 &&
					battle.PuniTotalLives == sptboss.YiNengUltimateLives && battle.PuniCurrentLife < sptboss.YiNengUltimateLives {
					// 地图 677 终极试炼：5 条命，击败当前命后切下一命并重置血量
					battle.PuniCurrentLife++
					maxHP := sptboss.YiNengUltimateLifeMaxHP(battle.PuniCurrentLife)
					if maxHP > 0 {
						battle.EnemyMaxHP = uint32(maxHP)
						battle.EnemyHP = uint32(maxHP)
					} else {
						battle.EnemyHP = battle.EnemyMaxHP
					}
				} else if battle.EnemyID == sptboss.PetIDPuni && battle.BattleMapID == sptboss.MapIDPuniTower &&
					battle.BossRegion == sptboss.PuniTrueForm && battle.PuniTotalLives == 6 && battle.PuniCurrentLife < 6 {
					// 谱尼真身：6 条命，切换到下一命并重置血量/强化/消除标记
					battle.PuniCurrentLife++
					battle.PuniTrueFormSuppressed = false
					battle.PuniElementLastType = 0
					// 每命血量不同
					maxHP := sptboss.PuniTrueFormLifeMaxHP(battle.PuniCurrentLife)
					if maxHP > 0 {
						battle.EnemyMaxHP = uint32(maxHP)
						battle.EnemyHP = uint32(maxHP)
					} else {
						battle.EnemyHP = battle.EnemyMaxHP
					}
					// 只有第一命沿用 SPT 默认攻击+2；切到第2命及之后全部清空强化
					if battle.PuniCurrentLife > 1 {
						battle.EnemyBattleLv = [6]int8{}
					}
				} else if battle.EnemyID == sptboss.PetIDPuni && battle.BattleMapID == sptboss.MapIDPuniTower &&
					battle.BossRegion == sptboss.PuniTrueForm && battle.PuniTotalLives == 0 {
					// 谱尼厉害形态：一命 65000 血，但死后会立刻满血复活（无限复活）
					battle.EnemyMaxHP = uint32(sptboss.PuniSuperHP)
					battle.EnemyHP = uint32(sptboss.PuniSuperHP)
				} else {
					battle.LastHitWasCrit = isCritPlayer
				}
			}

			// 特性：汲取(1039) —— 攻击命中后吸取命中伤害的8%体力
			if playerTrait == 1039 && damage > 0 {
				heal := uint32(math.Floor(float64(damage) * 0.08))
				if heal > 0 {
					newHP := battle.PlayerHP + heal
					if newHP > battle.PlayerMaxHP {
						newHP = battle.PlayerMaxHP
					}
					battle.PlayerHP = newHP
					effectGainHP += int32(heal)
				}
			}
			// effect 89：n 回合内造成伤害的 1/m 恢复自身体力
			if battle.PlayerVampRounds > 0 && battle.PlayerVampDenom > 0 && damage > 0 {
				heal := damage / uint32(battle.PlayerVampDenom)
				if heal > 0 {
					newHP := battle.PlayerHP + heal
					if newHP > battle.PlayerMaxHP {
						newHP = battle.PlayerMaxHP
					}
					battle.PlayerHP = newHP
					effectGainHP += int32(heal)
				}
			}
			// effect 116：在持续期内，若自己先手且本次攻击命中并造成伤害，则回复本次实际伤害的 20% 体力。
			if battle.PlayerFirstVampRounds > 0 && !enemyFirst && damage > 0 {
				heal := uint32(math.Floor(float64(damage) * 0.2))
				if heal > 0 {
					newHP := battle.PlayerHP + heal
					if newHP > battle.PlayerMaxHP {
						newHP = battle.PlayerMaxHP
					}
					battle.PlayerHP = newHP
					effectGainHP += int32(heal)
				}
			}
			// effect 117：在持续期内，若自己先手且本次攻击命中，则以配置概率令目标在下回合“害怕 1 回合”。
			if battle.PlayerFirstFearRounds > 0 && !enemyFirst && playerHit {
				chance := int(battle.PlayerFirstFearChance)
				if chance <= 0 || chance > 100 {
					chance = 50
				}
				if rand.Intn(100) < chance {
					// 内部写入 2 回合：下一次对方行动时减为 1 并无法行动，表现为“害怕 1 回合”
					if battle.EnemyStatus[gameskills.StatusIndexFear] < 2 {
						battle.EnemyStatus[gameskills.StatusIndexFear] = 2
					}
				}
			}

			// EffectID=115：15% 几率追加 floor(自身速度/3) 的固定伤害到本次伤害结果。
			if skill.EffectID == 115 && damage > 0 && battle.EnemyHP > 0 {
				if rand.Intn(100) < 15 {
					// 计算当前实际速度（含能力等级修正）
					speedStage := int(battle.PlayerBattleLv[gameskills.StatSpeed])
					realSpeed := int(float64(playerStats.Speed) * gamebattle.GetStatMultiplier(speedStage))
					if realSpeed < 0 {
						realSpeed = 0
					}
					extra := uint32(realSpeed / 3)
					if extra > 0 {
						if extra > battle.EnemyHP {
							extra = battle.EnemyHP
						}
						battle.EnemyHP -= extra
					}
				}
			}

			// EffectID=119：根据最终伤害奇偶触发疲惫或自身速度能力提升。
			if skill.EffectID == 119 && damage > 0 {
				if damage%2 == 1 {
					// 奇数伤害：30% 令对手疲惫 1 回合
					if rand.Intn(100) < 30 {
						if battle.EnemyStatus[gameskills.StatusIndexFatigue] < 1 {
							battle.EnemyStatus[gameskills.StatusIndexFatigue] = 1
						}
					}
				} else {
					// 偶数伤害：30% 令自身速度能力等级 +1
					if rand.Intn(100) < 30 {
						cur := int(battle.PlayerBattleLv[gameskills.StatSpeed])
						if cur < 6 {
							cur++
							if cur > 6 {
								cur = 6
							}
							battle.PlayerBattleLv[gameskills.StatSpeed] = int8(cur)
						}
					}
				}
			}

			// EffectID=120：命中后 50% 对方扣 maxHP/n，50% 自己扣 maxHP/n（不足 1 则不扣）。
			if skill.EffectID == 120 && battle.PlayerMaxHP > 0 && battle.EnemyMaxHP > 0 {
				args120 := gameskills.ParseSideEffectArg(skill.SideEffectArg)
				if len(args120) >= 1 && args120[0] > 0 {
					n := args120[0]
					// 抛硬币：0=对方扣血，1=自己扣血
					if rand.Intn(2) == 0 {
						share := battle.EnemyMaxHP / uint32(n)
						if share > 0 && battle.EnemyHP > 0 {
							if share > battle.EnemyHP {
								share = battle.EnemyHP
							}
							battle.EnemyHP -= share
						}
					} else {
						share := battle.PlayerMaxHP / uint32(n)
						if share > 0 && battle.PlayerHP > 0 {
							if share > battle.PlayerHP {
								share = battle.PlayerHP
							}
							battle.PlayerHP -= share
						}
					}
				}
			}

			// EffectID=122：先手时按概率改变对方能力；SideEffectArg=[propIndex,chance,delta]。
			if skill.EffectID == 122 && !enemyFirst {
				args122 := gameskills.ParseSideEffectArg(skill.SideEffectArg)
				if len(args122) >= 3 {
					propIndex := args122[0]
					chance := args122[1]
					delta := args122[2]
					if propIndex >= 0 && propIndex < 6 && chance > 0 {
						if rand.Intn(100) < chance {
							cur := int(battle.EnemyBattleLv[propIndex])
							cur += delta
							if cur < -6 {
								cur = -6
							}
							if cur > 6 {
								cur = 6
							}
							battle.EnemyBattleLv[propIndex] = int8(cur)
						}
					}
				}
			}

			// 不灭之火（技能ID 11023）：5回合内令对手每回合受到30点固定伤害。
			// 这里不走通用 EffectID，而是直接按技能 ID 识别，避免误伤其它 SideEffect=60 的老技能。
			// SideEffectArg: rounds damagePerTurn（例如 "5 30" 表示当前回合 + 之后4回合，每次自己出手后都额外扣30点固定伤害）
			if skill.ID == 11023 {
				effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
				rounds, perTurn := 0, 0
				if len(effArgs) >= 1 {
					rounds = effArgs[0]
				}
				if len(effArgs) >= 2 {
					perTurn = effArgs[1]
				}
				if rounds < 0 {
					rounds = 0
				}
				if perTurn < 0 {
					perTurn = 0
				}
				if rounds > 0 && perTurn > 0 {
					battle.EnemyFixedDotRounds = byte(rounds)
					battle.EnemyFixedDotDamage = uint32(perTurn)
				}
			}
			// 多回合固定伤害（effect 60 / 76）：
			// - 60：n 回合内，每回合附加 m 点固定伤害
			// - 76：m% 几率在 n 回合内，每回合造成 k 点固定伤害
			// 注：当前服的“固定伤害回合结算点”在每次出手后（applyFixedDotAfterAttack），
			// 这样既覆盖当前回合也覆盖后续回合（与不灭之火行为一致）。
			if skill.EffectID == 60 {
				effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
				rounds, perTurn := 0, 0
				if len(effArgs) >= 1 {
					rounds = effArgs[0]
				}
				if len(effArgs) >= 2 {
					perTurn = effArgs[1]
				}
				if rounds > 0 && perTurn > 0 {
					battle.EnemyFixedDotRounds = byte(rounds)
					battle.EnemyFixedDotDamage = uint32(perTurn)
				}
			} else if skill.EffectID == 76 {
				effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
				chance, rounds, perTurn := 0, 0, 0
				if len(effArgs) >= 1 {
					chance = effArgs[0]
				}
				if len(effArgs) >= 2 {
					rounds = effArgs[1]
				}
				if len(effArgs) >= 3 {
					perTurn = effArgs[2]
				}
				if chance > 0 && rounds > 0 && perTurn > 0 && rand.Intn(100) < chance {
					battle.EnemyFixedDotRounds = byte(rounds)
					battle.EnemyFixedDotDamage = uint32(perTurn)
				}
			}

		var effectRecoil uint32
		enemyControlImmune := sptboss.IsControlImmune(battle.EnemyID)
		if battle.EnemyID == sptboss.PetIDPuni && battle.BattleMapID == sptboss.MapIDPuniTower &&
			(battle.BossRegion == sptboss.PuniSealEnergy || (battle.BossRegion == sptboss.PuniTrueForm && battle.PuniTotalLives == 6 && battle.PuniCurrentLife == 3)) {
			enemyControlImmune = false // 能量封印/真身第三命可被控场（麻痹/睡眠/畏缩等）
		}
		// 仅当敌人为 NPC/BOSS（非 PvP 对手）时，才按 BOSS 特性处理异常免疫/能力免疫等；
		// PvP 中对手携带的同名精灵（如盖亚）应视为普通精灵，不享受 BOSS 免疫。
		defenderIDForEffect := 0
		if battle.OpponentUserID == 0 {
			defenderIDForEffect = battle.EnemyID
		}
		// 谱尼厉害形态：免疫除寄生种子(EffectID=13)外由技能附加的异常状态。
		// 这里通过把 defenderPetID 映射到一个“完全异常免疫”的 BOSS（例如盖亚），
		// 让 ApplyEffect 内部的 statusImmune 判定生效；寄生种子本身不依赖 statusImmune，因此仍可正常工作。
		if defenderIDForEffect != 0 && isSuperPuni && skill.EffectID != 13 {
			defenderIDForEffect = 261 // 盖亚：已在 StatusImmuneBossIDs 中，视为异常免疫占位
		}
		// effect 91：若我方处于“同步期”，则对手受到的状态变化也会同步到我方。
		enemyStatusBeforeSync := battle.EnemyStatus
		enemyBattleLvBeforeSync := battle.EnemyBattleLv

		gainFromEffect, effectRecoil := gameskills.ApplyEffect(skill, damage,
			&battle.PlayerHP, &battle.EnemyHP,
			battle.PlayerMaxHP, battle.EnemyMaxHP,
			&battle.PlayerBattleLv, &battle.EnemyBattleLv,
			&battle.PlayerStatus, &battle.EnemyStatus, defenderIDForEffect, enemyControlImmune)
		effectGainHP += gainFromEffect
		_ = effectRecoil

		if battle.PlayerSyncOpponentChangesRounds > 0 {
			// 同步异常状态变化
			for i := 0; i < len(battle.EnemyStatus); i++ {
				if battle.EnemyStatus[i] != enemyStatusBeforeSync[i] {
					battle.PlayerStatus[i] = battle.EnemyStatus[i]
				}
			}
			// 同步能力等级变化
			for i := 0; i < len(battle.EnemyBattleLv); i++ {
				if battle.EnemyBattleLv[i] != enemyBattleLvBeforeSync[i] {
					battle.PlayerBattleLv[i] = battle.EnemyBattleLv[i]
				}
			}
		}
		// EffectID=59：传承（自身体力清零，使下一只出战精灵的某两项能力+1 级）
		// SideEffectArg: statIndex1 statIndex2（0..5 分别对应攻、防、特攻、特防、速度、命中）
		if skill.EffectID == 59 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			stats := []int{}
			if len(effArgs) >= 1 {
				stats = append(stats, effArgs[0])
			}
			if len(effArgs) >= 2 {
				stats = append(stats, effArgs[1])
			}
			for _, sIdx := range stats {
				if sIdx < 0 || sIdx >= len(battle.PlayerNextPetBattleLvBoost) {
					continue
				}
				cur := int(battle.PlayerNextPetBattleLvBoost[sIdx]) + 1
				if cur > 6 {
					cur = 6
				}
				battle.PlayerNextPetBattleLvBoost[sIdx] = int8(cur)
			}
			// 消耗自身全部体力（体力降到 0）
			battle.PlayerHP = 0
			playerFaintedFromOwnSkillThisTurn = true
		}
		// EffectID=77：n 回合内每用技能恢复 m 点体力（固定值）。
		// 对于“生命”（ID=20606，SideEffectArg="5 100"），表现为“5 回合内，每回合（每次出招）恢复 100 固定体力值”，
		// 并且从释放技能当回合就开始生效。
		if skill.EffectID == 77 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds, points := 3, 30
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			if len(effArgs) >= 2 && effArgs[1] > 0 {
				points = effArgs[1]
			}
			// 与其它“回合类效果”保持一致：内部多存 1 回合以配合回合结束时的自动递减。
			battle.PlayerHealPerSkillPointsRounds = byte(rounds + 1)
			battle.PlayerHealPerSkillPoints = uint32(points)
		}
		// EffectID=71：电闪光华阵等——自身体力清零，使后续 N 次“攻击技能”必定致命一击（跨精灵继承，直到次数用完）。
		// 当前技能未配置 SideEffectArg，按描述固定为 2 次；若将来配置参数，则取 args[0] 为次数。
		if skill.EffectID == 71 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			hits := 2
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				hits = effArgs[0]
			}
			if hits < 0 {
				hits = 0
			}
			if hits > 0 {
				// 叠加：若之前已有剩余次数，则继续累加（避免重复触发覆盖）
				sum := int(battle.PlayerNextGuaranteedCritHits) + hits
				if sum > 255 {
					sum = 255
				}
				battle.PlayerNextGuaranteedCritHits = byte(sum)
			}
			// 自己牺牲（体力降到 0），本回合不再执行敌方出招
			battle.PlayerHP = 0
			playerFaintedFromOwnSkillThisTurn = true
		}
		// effect 77：n 回合内每用技能恢复 m 点体力（固定值）
		if battle.PlayerHealPerSkillPointsRounds > 0 && battle.PlayerHealPerSkillPoints > 0 {
			heal := battle.PlayerHealPerSkillPoints
			newHP := battle.PlayerHP + heal
			if newHP > battle.PlayerMaxHP {
				newHP = battle.PlayerMaxHP
			}
			battle.PlayerHP = newHP
			effectGainHP += int32(heal)
		}
		// effect 57：n 回合内每使用技能恢复自身最大体力的 1/m
		if battle.PlayerHealPerSkillRounds > 0 && battle.PlayerHealPerSkillDenom > 0 && battle.PlayerMaxHP > 0 {
			heal := battle.PlayerMaxHP / uint32(battle.PlayerHealPerSkillDenom)
			if heal > 0 {
				newHP := battle.PlayerHP + heal
				if newHP > battle.PlayerMaxHP {
					newHP = battle.PlayerMaxHP
				}
				battle.PlayerHP = newHP
				effectGainHP += int32(heal)
			}
		}

		// PP 削减效果（SideEffect 39）：命中时有 n% 几率，使对手所有技能 PP 各减少 m 点。
		if skill.EffectID == 39 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			chance := 100
			amount := 1
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				chance = effArgs[0]
			}
			if len(effArgs) >= 2 && effArgs[1] > 0 {
				amount = effArgs[1]
			}
			if chance < 0 {
				chance = 0
			}
			if chance > 100 {
				chance = 100
			}
			if amount < 1 {
				amount = 1
			}
			if rand.Intn(100) < chance {
				for i := 0; i < 4; i++ {
					if battle.EnemySkillIDs[i] <= 0 || battle.EnemySkillPP[i] <= 0 {
						continue
					}
					battle.EnemySkillPP[i] -= amount
					if battle.EnemySkillPP[i] < 0 {
						battle.EnemySkillPP[i] = 0
					}
				}
			}
		}

		// 暴击率提升效果（SideEffect 58 系列）：SideEffectArg[0] = 持续回合数
		// 表现：下 n 回合内，自身使用“攻击技能”时必定打出致命一击。
		if skill.EffectID == 58 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			if len(effArgs) >= 1 {
				rounds := effArgs[0]
				if rounds < 0 {
					rounds = 0
				}
				if rounds > 0 {
					// +1 是为了配合“每个战斗回合结束时自动减 1”的机制，
					// 确保玩家在之后能获得 param1[0] 个完整回合的必定暴击效果。
					battle.PlayerCritBuffRounds = byte(rounds + 1)
				}
			}
		}
		// 暴击率+1/16 效果（SideEffect 32 系列）：SideEffectArg[0] = 持续回合数
		// 表现：下 n 回合内，自身攻击技能的暴击率在原基础上 +1/16。
		if skill.EffectID == 32 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			if len(effArgs) >= 1 {
				rounds := effArgs[0]
				if rounds < 0 {
					rounds = 0
				}
				if rounds > 0 {
					battle.PlayerCritBonusRounds = byte(rounds + 1)
				}
			}
		}
		// 多回合效果设置
		if skill.EffectID == 41 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds := 3
			if len(effArgs) >= 2 && effArgs[1] > 0 {
				rounds = effArgs[1]
			}
			battle.PlayerFireResistRounds = byte(rounds + 1)
		}
		if skill.EffectID == 42 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds := 2
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			battle.PlayerElectricPowerRounds = byte(rounds + 1)
		}
		if skill.EffectID == 44 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds := 3
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			battle.PlayerSpecDefRounds = byte(rounds + 1)
		}
		if skill.EffectID == 46 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds := 1
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			battle.PlayerShieldRounds = byte(rounds + 1)
		}
		if skill.EffectID == 48 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds := 3
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			battle.PlayerStatusImmuneRounds = byte(rounds + 1)
		}
		if skill.EffectID == 50 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds := 3
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			battle.PlayerPhysDefRounds = byte(rounds + 1)
		}
		if skill.EffectID == 45 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds := 1
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			battle.PlayerDefenceEqualRounds = byte(rounds + 1)
		}
		if skill.EffectID == 51 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds := 1
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			battle.PlayerAttackEqualRounds = byte(rounds + 1)
		}
		if skill.EffectID == 67 {
			// 直接击败对方出战精灵时，削减对方下1只出战精灵最大体力 1/n
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			denom := 0
			if len(effArgs) >= 1 {
				denom = effArgs[0]
			}
			if denom > 0 {
				battle.EnemyNextPetMaxHPDropDenom = byte(denom)
			}
		}
		if skill.EffectID == 55 {
			// n 回合内，令自身的属性与对方交换
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds := 1
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			if attackerPet != nil && enemyPet != nil {
				battle.PlayerTypeOverride1 = byte(enemyPet.Type)
				battle.PlayerTypeOverride2 = byte(enemyPet.Type2)
				battle.EnemyTypeOverride1 = byte(attackerPet.Type)
				battle.EnemyTypeOverride2 = byte(attackerPet.Type2)
				battle.PlayerTypeOverrideRounds = byte(rounds + 1)
				battle.EnemyTypeOverrideRounds = byte(rounds + 1)
			}
		}
		if skill.EffectID == 56 {
			// n 回合内，复制对方的属性（仅改变自身）
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds := 1
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			if attackerPet != nil && enemyPet != nil {
				battle.PlayerTypeOverride1 = byte(enemyPet.Type)
				battle.PlayerTypeOverride2 = byte(enemyPet.Type2)
				battle.PlayerTypeOverrideRounds = byte(rounds + 1)
			}
		}
		if skill.EffectID == 86 {
			// n 回合内，属性/变化技能对自身必定 miss
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds := 1
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			battle.PlayerAttrMoveImmuneRounds = byte(rounds + 1)
		}
		if skill.EffectID == 106 {
			// n 回合内，特殊攻击对自身必定 miss
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds := 1
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			battle.PlayerSpecImmuneRounds = byte(rounds + 1)
		}
		if skill.EffectID == 109 {
			// n 回合内，给对手造成伤害时，有 m% 几率令对手冻伤
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds, chance := 1, 0
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			if len(effArgs) >= 2 && effArgs[1] > 0 {
				chance = effArgs[1]
			}
			if chance > 100 {
				chance = 100
			}
			battle.PlayerOnDamageFreezeRounds = byte(rounds + 1)
			battle.PlayerOnDamageFreezeChance = byte(chance)
		}
		if skill.EffectID == 110 {
			// n 回合内，每次躲避攻击都有 m% 几率使自身某项能力 +1 级
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds, chance, stat := 1, 0, 0
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			if len(effArgs) >= 2 && effArgs[1] > 0 {
				chance = effArgs[1]
			}
			if len(effArgs) >= 3 {
				stat = effArgs[2]
			}
			if chance > 100 {
				chance = 100
			}
			if stat < 0 {
				stat = 0
			}
			if stat > 5 {
				stat = 5
			}
			battle.PlayerOnDodgeBuffRounds = byte(rounds + 1)
			battle.PlayerOnDodgeBuffChance = byte(chance)
			battle.PlayerOnDodgeBuffStat = byte(stat)
		}
		if skill.EffectID == 91 {
			// n 回合内，对手的状态变化会同时作用在自己身上
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds := 1
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			battle.PlayerSyncOpponentChangesRounds = byte(rounds + 1)
		}
		if skill.EffectID == 84 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds, chance := 1, 0
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			if len(effArgs) >= 2 && effArgs[1] > 0 {
				chance = effArgs[1]
			}
			if chance > 100 {
				chance = 100
			}
			battle.PlayerOnHitParalysisRounds = byte(rounds + 1)
			battle.PlayerOnHitParalysisChance = byte(chance)
		}
		if skill.EffectID == 92 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds, chance := 1, 0
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			if len(effArgs) >= 2 && effArgs[1] > 0 {
				chance = effArgs[1]
			}
			if chance > 100 {
				chance = 100
			}
			battle.PlayerOnHitFreezeRounds = byte(rounds + 1)
			battle.PlayerOnHitFreezeChance = byte(chance)
		}
		if skill.EffectID == 108 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds, chance := 1, 0
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			if len(effArgs) >= 2 && effArgs[1] > 0 {
				chance = effArgs[1]
			}
			if chance > 100 {
				chance = 100
			}
			battle.PlayerOnHitBurnRounds = byte(rounds + 1)
			battle.PlayerOnHitBurnChance = byte(chance)
		}
		if skill.EffectID == 54 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds, denom := 2, 2
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			if len(effArgs) >= 2 && effArgs[1] > 0 {
				denom = effArgs[1]
			}
			battle.EnemyDamageHalvedRounds = byte(rounds + 1)
			battle.EnemyDamageHalvedDenom = byte(denom)
		}
		if skill.EffectID == 57 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds, denom := 3, 10
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			if len(effArgs) >= 2 && effArgs[1] > 0 {
				denom = effArgs[1]
			}
			// 记录本次施放前是否已经存在持续回复效果；用于避免同类效果叠加。
			wasActive := battle.PlayerHealPerSkillRounds > 0 && battle.PlayerHealPerSkillDenom > 0
			battle.PlayerHealPerSkillRounds = byte(rounds + 1)
			battle.PlayerHealPerSkillDenom = byte(denom)
			// 若之前没有该效果，则从本回合开始立即回复一次；若是刷新效果，仅重置回合/比例，不额外多跳。
			if !wasActive && battle.PlayerMaxHP > 0 && denom > 0 {
				heal := battle.PlayerMaxHP / uint32(denom)
				if heal > 0 {
					newHP := battle.PlayerHP + heal
					if newHP > battle.PlayerMaxHP {
						newHP = battle.PlayerMaxHP
					}
					battle.PlayerHP = newHP
					effectGainHP += int32(heal)
				}
			}
		}
		if skill.EffectID == 68 {
			battle.PlayerEndureRounds = 2
		}
		if skill.EffectID == 78 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds := 2
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			battle.PlayerPhysImmuneRounds = byte(rounds + 1)
		}
		if skill.EffectID == 81 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds := 1
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			battle.PlayerMustHitRounds = byte(rounds + 1)
		}
		if skill.EffectID == 89 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds, denom := 3, 8
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			if len(effArgs) >= 2 && effArgs[1] > 0 {
				denom = effArgs[1]
			}
			battle.PlayerVampRounds = byte(rounds + 1)
			battle.PlayerVampDenom = byte(denom)
		}
		// EffectID=116：n 回合内，若自己先手且本次攻击命中并造成伤害，则按本次实际伤害的 20% 回复体力。
		if skill.EffectID == 116 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds := 3
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			if rounds < 1 {
				rounds = 1
			}
			// 与其它“回合类效果”一致，内部多存 1 回合作为缓冲
			battle.PlayerFirstVampRounds = byte(rounds + 1)
		}
		// EffectID=117：n 回合内，若自己先手且命中，则以给定几率令目标陷入 1 回合“害怕”状态（内部存 2 回合作为缓冲）。
		if skill.EffectID == 117 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds := 5
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			if rounds < 1 {
				rounds = 1
			}
			battle.PlayerFirstFearRounds = byte(rounds + 1)
			// 触发几率（百分比），SideEffectArg[1]，默认 50%
			chance := 50
			if len(effArgs) >= 2 && effArgs[1] > 0 && effArgs[1] <= 100 {
				chance = effArgs[1]
			}
			battle.PlayerFirstFearChance = byte(chance)
		}
		if skill.EffectID == 53 || skill.EffectID == 90 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds, mul := 3, 2
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			if len(effArgs) >= 2 && effArgs[1] > 0 {
				mul = effArgs[1]
			}
			battle.PlayerDmgMulRounds = byte(rounds + 1)
			battle.PlayerDmgMul = byte(mul)
		}
		if skill.EffectID == 21 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds, denom := 3, 2
			if len(effArgs) >= 1 {
				rounds = effArgs[0]
			}
			if len(effArgs) >= 2 && effArgs[1] >= effArgs[0] {
				rounds = effArgs[1]
			}
			if len(effArgs) >= 3 && effArgs[2] > 0 {
				denom = effArgs[2]
			}
			battle.PlayerReflectRounds = byte(rounds + 1)
			battle.PlayerReflectDenom = byte(denom)
		}
		if skill.EffectID == 128 {
			// 伤害转治疗类技能（异界之光、烈火烟花等）：在 n 回合内，
			// 对我方造成的攻击伤害不再扣血，而是等量恢复体力。
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds := 1
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			// 仅使用专门的回合字段，避免与 effect 77 共用状态导致叠加错误
			battle.PlayerDamageConvertRounds = byte(rounds + 1)
		}
		if skill.EffectID == 65 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds, elemType, mul := 3, 0, 2
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			if len(effArgs) >= 2 {
				elemType = effArgs[1]
			}
			if len(effArgs) >= 3 && effArgs[2] > 0 {
				mul = effArgs[2]
			}
			battle.PlayerTypePowerRounds = byte(rounds + 1)
			battle.PlayerTypePowerType = byte(elemType)
			battle.PlayerTypePowerMul = byte(mul)
		}
		// EffectID=123：受伤时提升自身能力；SideEffectArg=[rounds, statIndex, delta]
		if skill.EffectID == 123 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			if len(effArgs) >= 3 {
				rounds := effArgs[0]
				stat := effArgs[1]
				delta := effArgs[2]
				if rounds < 1 {
					rounds = 1
				}
				if stat < 0 {
					stat = 0
				}
				if stat > 5 {
					stat = 5
				}
				battle.PlayerOnDamagedBuffRounds = byte(rounds + 1)
				battle.PlayerOnDamagedBuffStat = byte(stat)
				battle.PlayerOnDamagedBuffDelta = int8(delta)
			}
		}
		if skill.EffectID == 49 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			absorb := 50
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				absorb = effArgs[0]
			}
			battle.PlayerDamageAbsorb += uint32(absorb)
		}
		if skill.EffectID == 83 {
			// 自身雄性：下 N 回合必定先手；自身雌性：下 N 回合必定致命一击
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds := 2
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			if attackerPet != nil {
				if attackerPet.Gender == 1 {
					battle.PlayerFirstPriorityRounds = byte(rounds + 1)
				} else if attackerPet.Gender == 2 {
					// 复用必暴击效果
					if rounds > 0 {
						battle.PlayerCritBuffRounds = byte(rounds + 1)
					}
				}
			}
		}
		if skill.EffectID == 98 {
			// n 回合内，对雄性精灵的伤害为 m 倍
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			rounds, mul := 1, 2
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				rounds = effArgs[0]
			}
			if len(effArgs) >= 2 && effArgs[1] > 0 {
				mul = effArgs[1]
			}
			battle.PlayerMaleDmgMulRounds = byte(rounds + 1)
			battle.PlayerMaleDmgMul = byte(mul)
		}
		if skill.EffectID == 87 {
			// 恢复自身所有 PP
			for i := 0; i < 4; i++ {
				sid := battle.PlayerSkillIDs[i]
				if sid > 0 {
					if sk := skillMgr.Get(sid); sk != nil && sk.MaxPP > 0 {
						battle.PlayerSkillPP[i] = sk.MaxPP
					}
				}
			}
		}

			// 对于“同生共死”一类基于血量差的技能，附加效果可能在普通伤害结算后再次大幅修改 enemyHP。
			// 为了让客户端看到的 lostHP/日志中的 Damage 与真实扣血一致，
			// 在效果应用后，用“出手前 HP - 当前 HP”重写 damage 与 damageCalc。
			if skill.EffectID == 7 {
				if enemyHPBeforeAction > battle.EnemyHP {
					actualLoss := enemyHPBeforeAction - battle.EnemyHP
					damage = actualLoss
					damageCalc = actualLoss
				} else {
					// 未造成额外伤害时，视为 0（例如敌方 HP <= 我方 HP）
					damage = 0
					damageCalc = 0
				}
			}
		}

		// 若本次攻击击败了对方出战精灵且技能具有“击杀回复”效果（effect 66），
		// 则按配置恢复自身最大体力的 1/n。
		if skill.EffectID == 66 && enemyHPBeforeAction > 0 && battle.EnemyHP == 0 && battle.PlayerHP > 0 && battle.PlayerMaxHP > 0 {
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			denom := 2
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				denom = effArgs[0]
			}
			heal := battle.PlayerMaxHP / uint32(denom)
			if heal > 0 {
				// 不超过当前缺口
				maxHeal := battle.PlayerMaxHP - battle.PlayerHP
				if heal > maxHeal {
					heal = maxHeal
				}
				if heal > 0 {
					battle.PlayerHP += heal
					effectGainHP += int32(heal)
				}
			}
		}

		// 当前回合（包括施放带多回合固伤效果的这一回合），在对方普通伤害结算完之后，
		// 立即结算一跳固定伤害（若之前已有剩余回合，则同样在本次出手后结算）。
		applyFixedDotAfterAttack(&battle.EnemyHP, &battle.EnemyFixedDotDamage, &battle.EnemyFixedDotRounds)
		// 尤纳斯(132) phase 1：固定伤害不能致死，强制锁 1 血；若本回合已由里奥斯幻影击杀则不再恢复 1 血
		if sptboss.IsYouNaSiBoss(battle.EnemyID) && battle.YouNaSiPhase == 1 && battle.EnemyHP == 0 && !youNaSiKilledByPhantom {
			battle.EnemyHP = 1
		}
	}
	if fromGaiyaFirst {
		fromGaiyaFirst = false
		goto afterEnemyTurn
	}

runEnemyTurnFirst:
	// 敌人反击（如果敌人还活着）；畏缩/睡眠/麻痹 时本回合无法行动
	// 封装为闭包以便盖亚先手时在“我方出招”前调用一次，非盖亚时在“我方出招”后调用一次
	doEnemyTurn = func() {
		playerHPBeforeEnemy = battle.PlayerHP
		enemyFlinched := false
		// 疲惫（status[7]）：本回合无法行动，并递减回合数
		if battle.EnemyStatus[gameskills.StatusIndexFatigue] > 0 {
			enemyFlinched = true
			battle.EnemyStatus[gameskills.StatusIndexFatigue]--
		}
		// 害怕（status[6]）：本回合无法行动，并递减回合数
		if battle.EnemyStatus[gameskills.StatusIndexFear] > 0 {
			enemyFlinched = true
			battle.EnemyStatus[gameskills.StatusIndexFear]--
		}
		// 敌方睡眠（status[8]）：效果与麻痹一致，本回合无法行动，并递减回合数
		if battle.EnemyStatus[gameskills.StatusIndexSleep] > 0 {
			enemyFlinched = true
			battle.EnemyStatus[gameskills.StatusIndexSleep]--
		}
		// 敌方麻痹（status[0]）：本回合无法行动，并递减回合数
		enemyParalyzed := false
		if battle.EnemyStatus[gameskills.StatusIndexParalysis] > 0 {
			enemyParalyzed = true
			battle.EnemyStatus[gameskills.StatusIndexParalysis]--
		}

		// 敌人反击（如果敌人还活着且未畏缩、未因麻痹无法行动）
		enemyDamage = 0
		enemyDamageCalc = 0
		enemySkillID = 0
		enemyAttempted = false
		enemyHitFor2505 = false
		if battle.EnemyHP > 0 && !enemyFlinched && !enemyParalyzed {
		if enemySkillForTurn != nil && enemySkillIDForTurn != 0 {
			enemySkillID = enemySkillIDForTurn
			enemyAttempted = true
			// 哈默雷特(216) 敌方技能 PP：仅在本回合确实行动时消耗 1 点 PP
			if sptboss.IsHaMoLeiTeOrderBoss(battle.EnemyID) {
				for i := 0; i < 4; i++ {
					if battle.EnemySkillIDs[i] == int(enemySkillIDForTurn) && battle.EnemySkillPP[i] > 0 {
						battle.EnemySkillPP[i]--
						break
					}
				}
			}

			// 魔狮迪露(187) SPT BOSS 体力低于一半时：任意技能必定秒杀我方；PvP 时对方玩家精灵不享受此 BOSS 特性
			moShiDiLuOneShot := battle.OpponentUserID == 0 &&
				sptboss.IsHalfHPOneShotBoss(battle.EnemyID) && battle.EnemyMaxHP > 0 && battle.EnemyHP*2 < battle.EnemyMaxHP
			if moShiDiLuOneShot {
				enemyDamageCalc = battle.PlayerHP
				battle.PlayerHP = 0
				enemyHitFor2505 = true
			} else if battle.OpponentUserID == 0 && battle.EnemyID == 5008 && enemySkillForTurn.Category != 4 && rand.Intn(100) < 50 {
				// 扎克斯分身(5008)：攻击技能 50% 概率秒杀（仅 PvE 敌方）
				enemyDamageCalc = battle.PlayerHP
				battle.PlayerHP = 0
				enemyHitFor2505 = true
			} else {
			// 计算敌人本回合是否命中（考虑命中等级与必中）
			enemyHit := true
			if enemySkillForTurn.MustHit != 1 {
				// 若上一回合我方成功使用了“回避类”技能，并满足先手/速度条件，
				// 则本回合敌方第一次攻击必定 MISS（效果触发一次后立即清除）。
				if battle.EnemyNextAttackMustMiss {
					enemyHit = false
					battle.EnemyNextAttackMustMiss = false
				} else if enemySkillForTurn.Category == 4 && battle.PlayerAttrMoveImmuneRounds > 0 {
					// effect 86：n 回合内，属性/变化技能对自身必定 miss
					enemyHit = false
				} else {
					baseAcc := enemySkillForTurn.Accuracy
					if baseAcc == 0 {
						baseAcc = 100
					}
					accStage := int(battle.EnemyBattleLv[gameskills.StatAccuracy])
					finalAcc := gamebattle.CalcHitChance(baseAcc, accStage, 0)
					// 特性：回避(1025) —— 被技能命中的几率减少5%
					if playerTrait == 1025 {
						finalAcc -= 5
					}
					if finalAcc > 100 {
						finalAcc = 100
					}
					if finalAcc < 1 {
						finalAcc = 1
					}
					if rand.Intn(100) >= finalAcc {
						enemyHit = false
					}
				}
			}
			// 谱尼厉害形态：敌方攻击必定命中（无视普通命中率与回避）
			if isSuperPuni {
				enemyHit = true
			}
			// effect 78：n 回合内物理攻击对自身必定 miss
			if enemyHit && enemySkillForTurn.Category == 1 && battle.PlayerPhysImmuneRounds > 0 {
				enemyHit = false
			}
			// effect 106：n 回合内特殊攻击对自身必定 miss
			if enemyHit && enemySkillForTurn.Category == 2 && battle.PlayerSpecImmuneRounds > 0 {
				enemyHit = false
			}
			// effect 81 敌方：下 n 回合敌方攻击必定命中
			if battle.EnemyMustHitRounds > 0 {
				enemyHit = true
			}
			enemyHitFor2505 = enemyHit

			// effect 110 敌方版本：若我方成功“躲避”本次攻击，则有 m% 几率按配置能力 +1 级
			if enemyAttempted && !enemyHit && enemySkillForTurn.Category != 4 &&
				battle.PlayerOnDodgeBuffRounds > 0 && battle.PlayerOnDodgeBuffChance > 0 {
				if rand.Intn(100) < int(battle.PlayerOnDodgeBuffChance) {
					statIdx := int(battle.PlayerOnDodgeBuffStat)
					if statIdx >= 0 && statIdx < len(battle.PlayerBattleLv) {
						if battle.PlayerBattleLv[statIdx] < 6 {
							battle.PlayerBattleLv[statIdx]++
						}
					}
				}
			}

			// 敌人伤害计算（仅在命中且为攻击技能时）
			enemyPower := uint32(enemySkillForTurn.Power)
			// Category=4 为纯变化/属性技能（红韵、魅惑等），不应造成直接伤害
			if enemySkillForTurn.Category != 4 && enemyPower == 0 {
				enemyPower = 40
			}
			// 敌方 effect 118：威力随机 150~220
			if enemySkillForTurn.EffectID == 118 && enemySkillForTurn.Category != 4 {
				enemyPower = uint32(rand.Intn(220-150+1) + 150)
			}
			// 敌方攻防（按技能类别），并应用强化弱化倍率
			enemyAtk := float64(enemyStats.Attack)
			enemyDef := float64(playerStats.Defence)
			enemyAtkStage, enemyDefStage := 0, 1 // 物理：攻击/防御
			if enemySkillForTurn.Category == 2 {
				enemyAtk = float64(enemyStats.SpAtk)
				enemyDef = float64(playerStats.SpDef)
				enemyAtkStage, enemyDefStage = 2, 3 // 特殊：特攻/特防
			}
			enemyAtk *= gamebattle.GetStatMultiplier(int(battle.EnemyBattleLv[enemyAtkStage]))
			// effect 45：若我方处于“防御力与对手相同”，则我方防御值视为敌方当前防御值（仅物理防御）。
			if enemySkillForTurn.Category == 1 && battle.PlayerDefenceEqualRounds > 0 {
				enemyDef = float64(enemyStats.Defence) * gamebattle.GetStatMultiplier(int(battle.EnemyBattleLv[gameskills.StatDefence]))
			} else {
				enemyDef *= gamebattle.GetStatMultiplier(int(battle.PlayerBattleLv[enemyDefStage]))
			}
			// effect 51：若敌方处于“攻击力与对手相同”，则敌方物理攻击力视为我方当前攻击力（含强化/弱化）
			if enemySkillForTurn.Category == 1 && battle.EnemyAttackEqualRounds > 0 {
				enemyAtk = float64(playerStats.Attack) * gamebattle.GetStatMultiplier(int(battle.PlayerBattleLv[gameskills.StatAttack]))
			}
			if enemyDef < 1 {
				enemyDef = 1
			}
				enemyBaseDamage := 0.0
				if enemySkillForTurn.Category != 4 && enemyPower > 0 {
					// 敌方 Effect 113：个体越高威力越大（按 DV*5 计算威力；敌方 DV 统一视为 15）
					effectivePower := enemyPower
					if enemySkillForTurn.EffectID == 113 {
						effectivePower = uint32(15 * 5)
					}
					enemyBaseDamage = math.Floor(((float64(battle.EnemyLevel)*0.4 + 2.0) * float64(effectivePower) * enemyAtk / enemyDef / 50.0) + 2.0)
				}
			enemyFinalDamage := uint32(0)
			if enemyHit && enemySkillForTurn.Category != 4 && enemyBaseDamage > 0 {
				// 计算当前回合用于 STAB/克制判定的临时属性（支持 effect 55/56）
				enemyType1, enemyType2 := 0, 0
				if enemyPet != nil {
					enemyType1, enemyType2 = enemyPet.Type, enemyPet.Type2
				}
				playerType1, playerType2 := 0, 0
				if attackerPet != nil {
					playerType1, playerType2 = attackerPet.Type, attackerPet.Type2
				}
				if battle.EnemyTypeOverrideRounds > 0 {
					if battle.EnemyTypeOverride1 > 0 {
						enemyType1 = int(battle.EnemyTypeOverride1)
					}
					if battle.EnemyTypeOverride2 > 0 {
						enemyType2 = int(battle.EnemyTypeOverride2)
					}
				}
				if battle.PlayerTypeOverrideRounds > 0 {
					if battle.PlayerTypeOverride1 > 0 {
						playerType1 = int(battle.PlayerTypeOverride1)
					}
					if battle.PlayerTypeOverride2 > 0 {
						playerType2 = int(battle.PlayerTypeOverride2)
					}
				}
				enemyStab := 1.0
				if enemyType1 > 0 && enemySkillForTurn.Type == enemyType1 || (enemyType2 > 0 && enemySkillForTurn.Type == enemyType2) {
					enemyStab = 1.5
				}
				enemyTypeMod := 1.0
				if playerType1 > 0 {
					enemyTypeMod = gamebattle.GetTypeMultiplierWithAttackerDual(enemySkillForTurn.Type, enemyType1, enemyType2, playerType1, playerType2)
				}
				enemyRandomMod := float64(rand.Intn(255-217+1)+217) / 255.0
				// 敌人暴击：1/16 基础概率
				enemyCritRate := enemySkillForTurn.CritRate
				if enemyCritRate == 0 {
					enemyCritRate = 1
				}
				// 克瑞斯（赫鲁卡城二当家定制 BOSS）：固定 50% 暴击率
				if battle.EnemyID == 589 && enemyCritRate < 8 {
					enemyCritRate = 8
				}
				// 敌方暴击强化：
				// 1) EnemyCritBuffRounds：下 N 回合攻击技能必定致命一击 -> 100% 暴击率
				// 2) EnemyNextGuaranteedCritHits：后续 N 次攻击技能必定致命一击（跨精灵继承）-> 本次直接必暴击，命中后消耗 1 次
				hasGuaranteedEnemyCritThisHit := battle.EnemyNextGuaranteedCritHits > 0
				if battle.EnemyCritBuffRounds > 0 || hasGuaranteedEnemyCritThisHit {
					enemyCritRate = 16
				} else {
					// 暴击率提升（effect 32）：下 N 回合暴击率 +1 阶（+1/16），在 Buff 不生效时叠加。
					if battle.EnemyCritBonusRounds > 0 && enemyCritRate < 16 {
						enemyCritRate++
					}
				}
				enemyCritMod := 1.0
				isCritEnemyThisHit := false
				if rand.Intn(16) < enemyCritRate || hasGuaranteedEnemyCritThisHit {
					// 暴击：伤害为正常攻击伤害的两倍
					enemyCritMod = 2.0
					isCritEnemyThisHit = true
					// 敌方暴击时，同样清除我方防御/特防的强化状态
					if enemySkillForTurn.Category == 1 {
						// 物理暴击：清空我方防御提升
						if battle.PlayerBattleLv[gameskills.StatDefence] > 0 {
							battle.PlayerBattleLv[gameskills.StatDefence] = 0
						}
					} else if enemySkillForTurn.Category == 2 {
						// 特殊暴击：清空我方特防提升
						if battle.PlayerBattleLv[gameskills.StatSpDef] > 0 {
							battle.PlayerBattleLv[gameskills.StatSpDef] = 0
						}
					}
				}
				// 若本次暴击来自“后续 N 次必暴击”（跨精灵）效果，则在本次攻击确实命中且为攻击技能时消耗 1 次。
				// 这里位于 enemyHit==true 的分支中，因此不会因为 MISS 消耗次数。
				if hasGuaranteedEnemyCritThisHit && battle.EnemyNextGuaranteedCritHits > 0 {
					battle.EnemyNextGuaranteedCritHits--
				}
				enemyFinalDamage = uint32(enemyBaseDamage * enemyStab * enemyTypeMod * enemyRandomMod * enemyCritMod)
				// 若目标处于“易燃”状态，则火系技能额外乘固定倍率（1.5 倍）
				if enemyFinalDamage > 0 && enemySkillForTurn.Type == sptboss.TypeFire &&
					battle.PlayerStatus[gameskills.StatusIndexFlammable] > 0 {
					mul := uint64(enemyFinalDamage) * 3 / 2
					if mul > math.MaxUint32 {
						enemyFinalDamage = math.MaxUint32
					} else {
						enemyFinalDamage = uint32(mul)
					}
				}
				// effect 111 敌方版本：附加额外伤害，随敌方等级线性提升（按 2×等级 追加固定伤害）
				if enemySkillForTurn.EffectID == 111 && enemyFinalDamage > 0 && battle.EnemyLevel > 0 {
					add := uint64(battle.EnemyLevel) * 2
					sum := uint64(enemyFinalDamage) + add
					if sum > math.MaxUint32 {
						enemyFinalDamage = math.MaxUint32
					} else {
						enemyFinalDamage = uint32(sum)
					}
				}
			// effect 129：若目标为指定性别则技能威力翻倍
			if enemySkillForTurn.EffectID == 129 && attackerPet != nil {
				effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
				if len(effArgs) >= 1 && attackerPet.Gender == effArgs[0] && enemyFinalDamage > 0 {
					mul := uint64(enemyFinalDamage) * 2
					if mul > math.MaxUint32 {
						enemyFinalDamage = math.MaxUint32
					} else {
						enemyFinalDamage = uint32(mul)
					}
				}
			}
			// effect 102：目标处于麻痹状态时威力翻倍
			if enemySkillForTurn.EffectID == 102 && battle.PlayerStatus[gameskills.StatusIndexParalysis] > 0 && enemyFinalDamage > 0 {
				mul := uint64(enemyFinalDamage) * 2
				if mul > math.MaxUint32 {
					enemyFinalDamage = math.MaxUint32
				} else {
					enemyFinalDamage = uint32(mul)
				}
			}
			// effect 130：目标为指定性别则附加固定伤害
			if enemySkillForTurn.EffectID == 130 && attackerPet != nil {
				effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
				if len(effArgs) >= 2 && attackerPet.Gender == effArgs[0] && effArgs[1] > 0 {
					add := uint64(enemyFinalDamage) + uint64(effArgs[1])
					if add > math.MaxUint32 {
						enemyFinalDamage = math.MaxUint32
					} else {
						enemyFinalDamage = uint32(add)
					}
				}
			}
			// effect 131：目标为指定性别则免疫当前回合伤害
			if enemySkillForTurn.EffectID == 131 && attackerPet != nil {
				effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
				if len(effArgs) >= 1 && attackerPet.Gender == effArgs[0] {
					enemyFinalDamage = 0
				}
			}
				// 敌方 effect 82：目标为雄性伤害 200%，雌性 50%
				if enemySkillForTurn.EffectID == 82 && enemyFinalDamage > 0 && attackerPet != nil {
					switch attackerPet.Gender {
					case 1:
						mul := uint64(enemyFinalDamage) * 2
						if mul > math.MaxUint32 {
							enemyFinalDamage = math.MaxUint32
						} else {
							enemyFinalDamage = uint32(mul)
						}
					case 2:
						enemyFinalDamage = enemyFinalDamage / 2
						if enemyFinalDamage < 1 && battle.PlayerHP > 0 {
							enemyFinalDamage = 1
						}
					}
				}
				// 敌方 effect 98：n 回合内，对雄性精灵的伤害为 m 倍
				if battle.EnemyMaleDmgMulRounds > 0 && battle.EnemyMaleDmgMul > 1 &&
					attackerPet != nil && attackerPet.Gender == 1 && enemyFinalDamage > 0 {
					mul := uint64(enemyFinalDamage) * uint64(battle.EnemyMaleDmgMul)
					if mul > math.MaxUint32 {
						enemyFinalDamage = math.MaxUint32
					} else {
						enemyFinalDamage = uint32(mul)
					}
				}
				// 敌方 effect 54：n 回合使我方（玩家）所受伤害为 1/m
				if battle.PlayerDamageHalvedRounds > 0 && battle.PlayerDamageHalvedDenom > 1 {
					enemyFinalDamage = enemyFinalDamage / uint32(battle.PlayerDamageHalvedDenom)
					if enemyFinalDamage < 1 && playerHPBeforeEnemy > 0 {
						enemyFinalDamage = 1
					}
				}
				// 盖亚(261) SPT BOSS 伤害翻倍；PvP 时对方玩家手中的盖亚不享受此 BOSS 特性
				if battle.OpponentUserID == 0 && battle.EnemyID == petIDGaiya && enemyFinalDamage > 0 {
					enemyFinalDamage *= 2
				}
				// 先/后手威力翻倍（effect 40/30）同样对敌方技能生效：
				// enemyFirst=true 表示敌方先手；因此敌方后手 = !enemyFirst
				if enemyFinalDamage > 0 {
					if enemySkillForTurn.EffectID == 40 && enemyFirst {
						mul := uint64(enemyFinalDamage) * 2
						if mul > math.MaxUint32 {
							enemyFinalDamage = math.MaxUint32
						} else {
							enemyFinalDamage = uint32(mul)
						}
					} else if enemySkillForTurn.EffectID == 30 && !enemyFirst {
						mul := uint64(enemyFinalDamage) * 2
						if mul > math.MaxUint32 {
							enemyFinalDamage = math.MaxUint32
						} else {
							enemyFinalDamage = uint32(mul)
						}
					}
				}
				// effect 73：若敌方先手攻击，则在后续 n 回合内受到攻击时 2 倍反击给对手
				if enemySkillForTurn.EffectID == 73 && enemyFirst {
					effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
					rounds := 1
					if len(effArgs) >= 1 && effArgs[0] > 0 {
						rounds = effArgs[0]
					}
					battle.EnemyCounterDoubleRounds = byte(rounds + 1)
				}
				if isCritEnemyThisHit {
					isCritEnemyFor2505 = true
				}
			}
			// 敌人多段攻击（effect 31），同样折算为单次伤害倍数
			if enemyFinalDamage > 0 && enemySkillForTurn.EffectID == 31 {
				effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
				minHits, maxHits := 2, 5
				if len(effArgs) >= 1 {
					minHits = effArgs[0]
				}
				if len(effArgs) >= 2 {
					maxHits = effArgs[1]
				}
				if minHits < 1 {
					minHits = 1
				}
				if maxHits < minHits {
					maxHits = minHits
				}
				hits := rand.Intn(maxHits-minHits+1) + minHits
				if hits > 1 {
					mul := uint64(enemyFinalDamage) * uint64(hits)
					if mul > math.MaxUint32 {
						enemyFinalDamage = math.MaxUint32
					} else {
						enemyFinalDamage = uint32(mul)
					}
				}
			}
			// 烧伤效果：被烧伤方（敌人）造成的伤害减半
			if enemyFinalDamage > 0 && battle.EnemyStatus[gameskills.StatusIndexBurn] > 0 {
				enemyFinalDamage = enemyFinalDamage / 2
				if enemyFinalDamage < 1 {
					enemyFinalDamage = 1
				}
			}
			// effect 41：火系伤害减半（玩家受到的火系伤害）
			if enemyFinalDamage > 0 && battle.PlayerFireResistRounds > 0 && enemySkillForTurn.Type == 3 {
				enemyFinalDamage = enemyFinalDamage / 2
				if enemyFinalDamage < 1 {
					enemyFinalDamage = 1
				}
			}
			// effect 44：特殊攻击伤害减半
			if enemyFinalDamage > 0 && battle.PlayerSpecDefRounds > 0 && enemySkillForTurn.Category == 2 {
				enemyFinalDamage = enemyFinalDamage / 2
				if enemyFinalDamage < 1 {
					enemyFinalDamage = 1
				}
			}
			// effect 50：物理攻击伤害减半
			if enemyFinalDamage > 0 && battle.PlayerPhysDefRounds > 0 && enemySkillForTurn.Category == 1 {
				enemyFinalDamage = enemyFinalDamage / 2
				if enemyFinalDamage < 1 {
					enemyFinalDamage = 1
				}
			}
			if enemyFinalDamage < 1 {
				enemyFinalDamage = 0
			}
			if enemyHit && enemyFinalDamage > 0 {
				// effect 46：抵挡 N 次攻击伤害
				if battle.PlayerShieldRounds > 0 {
					battle.PlayerShieldRounds--
					enemyFinalDamage = 0
				}
				// effect 49：抵挡 n 点伤害（吸收盾）
				if battle.PlayerDamageAbsorb > 0 && enemyFinalDamage > 0 {
					absorb := enemyFinalDamage
					if absorb > battle.PlayerDamageAbsorb {
						absorb = battle.PlayerDamageAbsorb
					}
					battle.PlayerDamageAbsorb -= absorb
					if enemyFinalDamage <= absorb {
						enemyFinalDamage = 0
					} else {
						enemyFinalDamage -= absorb
					}
				}
			}
			if enemyHit && enemyFinalDamage > 0 {
				// 谱尼厉害形态：当前血量低于 20000 时，本次命中直接秒杀我方当前精灵
				if isSuperPuni && battle.EnemyHP < 20000 {
					enemyFinalDamage = uint32(battle.PlayerHP)
					enemyDamageCalc = enemyFinalDamage
				}
				// EffectID=123 敌方版本：在持续期内，每当敌方因技能伤害实际扣血时，按配置提升自身能力等级
				if battle.EnemyOnDamagedBuffRounds > 0 && enemyDamage > 0 {
					stat := int(battle.EnemyOnDamagedBuffStat)
					if stat >= 0 && stat < len(battle.EnemyBattleLv) {
						cur := int(battle.EnemyBattleLv[stat]) + int(battle.EnemyOnDamagedBuffDelta)
						if cur < -6 {
							cur = -6
						}
						if cur > 6 {
							cur = 6
						}
						battle.EnemyBattleLv[stat] = int8(cur)
					}
				}

				// EffectID=116 敌方版本：在持续期内，若敌方先手且本次攻击命中并造成伤害，则按本次实际伤害的 20% 回复体力。
				if battle.EnemyFirstVampRounds > 0 && enemyFirst && enemyDamage > 0 {
					heal := uint32(math.Floor(float64(enemyDamage) * 0.2))
					if heal > 0 {
						newHP := battle.EnemyHP + heal
						if newHP > battle.EnemyMaxHP {
							newHP = battle.EnemyMaxHP
						}
						battle.EnemyHP = newHP
						enemyEffectGainHP += int32(heal)
					}
				}
				// EffectID=117 敌方版本：在持续期内，若敌方先手且本次攻击命中，则以配置概率令玩家在下回合“害怕 1 回合”。
				if battle.EnemyFirstFearRounds > 0 && enemyFirst && enemyHit {
					chance := int(battle.EnemyFirstFearChance)
					if chance <= 0 || chance > 100 {
						chance = 50
					}
					if rand.Intn(100) < chance {
						if battle.PlayerStatus[gameskills.StatusIndexFear] < 2 {
							battle.PlayerStatus[gameskills.StatusIndexFear] = 2
						}
					}
				}

				// 特性：坚硬(1024) —— 受到的伤害减少5%
				if playerTrait == 1024 {
					reduced := uint32(math.Floor(float64(enemyFinalDamage) * 0.95))
					if reduced < 1 {
						reduced = 1
					}
					enemyFinalDamage = reduced
				}
				// 顽强(1026)：受到致死攻击时有3%几率余下1点体力
				if playerTrait == 1026 && playerHPBeforeEnemy > 1 && enemyFinalDamage >= playerHPBeforeEnemy {
					if rand.Intn(100) < 3 {
						enemyFinalDamage = playerHPBeforeEnemy - 1
					}
				}
				// effect 109：敌方处于“对我造成伤害时几率冻伤”状态时，本次受伤有 m% 几率令我方冻伤
				if battle.EnemyOnDamageFreezeRounds > 0 && battle.EnemyOnDamageFreezeChance > 0 &&
					battle.PlayerStatus[gameskills.StatusIndexFreeze] == 0 {
					if rand.Intn(100) < int(battle.EnemyOnDamageFreezeChance) {
						battle.PlayerStatus[gameskills.StatusIndexFreeze] = 2
					}
				}
				// effect 68：致死攻击时余下 1 点体力
				if battle.PlayerEndureRounds > 0 && enemyFinalDamage >= battle.PlayerHP && battle.PlayerHP > 0 {
					enemyFinalDamage = battle.PlayerHP - 1
					if battle.PlayerHP == 1 {
						enemyFinalDamage = 0
					}
					battle.PlayerEndureRounds--
				}
				// 记录用于显示的理论伤害（可能大于当前体力，用于 2405 日志与飘字）
				displayDamage := enemyFinalDamage
				// 实际扣血值不能超过当前剩余体力，避免出现负血或下溢
				appliedDamage := enemyFinalDamage
				if appliedDamage > battle.PlayerHP {
					appliedDamage = battle.PlayerHP
				}
				// 先处理“伤害转化为回复”的效果（effect 128 等），再结算真实扣血/显示伤害
				if battle.PlayerDamageConvertRounds > 0 && appliedDamage > 0 {
					// 本应造成的伤害不扣血，而是等量恢复体力；显示伤害为 0
					newHP := battle.PlayerHP + appliedDamage
					if newHP > battle.PlayerMaxHP {
						newHP = battle.PlayerMaxHP
					}
					battle.PlayerHP = newHP
					enemyDamage = 0
					enemyDamageCalc = 0
				} else {
					enemyDamage = appliedDamage
					enemyDamageCalc = displayDamage
					battle.PlayerHP -= enemyDamage
					if battle.PlayerHP > battle.PlayerMaxHP {
						battle.PlayerHP = 0 // uint 下溢时置 0
					}

					// effect 101：按伤害百分比吸血（敌方）
					if enemySkillForTurn.EffectID == 101 && enemyDamage > 0 && battle.EnemyMaxHP > 0 {
						effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
						percent := 0
						if len(effArgs) >= 1 {
							percent = effArgs[0]
						}
						if percent > 0 {
							heal := uint32(uint64(enemyDamage) * uint64(percent) / 100)
							if heal > 0 {
								newHP := battle.EnemyHP + heal
								if newHP > battle.EnemyMaxHP {
									newHP = battle.EnemyMaxHP
								}
								battle.EnemyHP = newHP
								enemyEffectGainHP += int32(heal)
							}
						}
					}
				// effect 105：按伤害的 1/n 吸血（敌方）
					if enemySkillForTurn.EffectID == 105 && enemyDamage > 0 && battle.EnemyMaxHP > 0 {
						effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
						denom := 0
						if len(effArgs) >= 1 {
							denom = effArgs[0]
						}
						if denom > 0 {
							heal := enemyDamage / uint32(denom)
							if heal > 0 {
								newHP := battle.EnemyHP + heal
								if newHP > battle.EnemyMaxHP {
									newHP = battle.EnemyMaxHP
								}
								battle.EnemyHP = newHP
								enemyEffectGainHP += int32(heal)
							}
						}
					}

					// EffectID=119 敌方版本：根据最终伤害奇偶触发疲惫或自身速度能力提升。
					if enemySkillForTurn.EffectID == 119 && enemyDamage > 0 {
						if enemyDamage%2 == 1 {
							// 奇数伤害：30% 令玩家疲惫 1 回合
							if rand.Intn(100) < 30 {
								if battle.PlayerStatus[gameskills.StatusIndexFatigue] < 1 {
									battle.PlayerStatus[gameskills.StatusIndexFatigue] = 1
								}
							}
						} else {
							// 偶数伤害：30% 令敌方速度能力等级 +1
							if rand.Intn(100) < 30 {
								cur := int(battle.EnemyBattleLv[gameskills.StatSpeed])
								if cur < 6 {
									cur++
									if cur > 6 {
										cur = 6
									}
									battle.EnemyBattleLv[gameskills.StatSpeed] = int8(cur)
								}
							}
						}
					}

					// EffectID=120 敌方版本：命中后 50% 玩家扣 maxHP/n，50% 敌方扣 maxHP/n。
					if enemySkillForTurn.EffectID == 120 && battle.PlayerMaxHP > 0 && battle.EnemyMaxHP > 0 {
						args120 := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
						if len(args120) >= 1 && args120[0] > 0 {
							n := args120[0]
							if rand.Intn(2) == 0 {
								share := battle.PlayerMaxHP / uint32(n)
								if share > 0 && battle.PlayerHP > 0 {
									if share > battle.PlayerHP {
										share = battle.PlayerHP
									}
									battle.PlayerHP -= share
								}
							} else {
								share := battle.EnemyMaxHP / uint32(n)
								if share > 0 && battle.EnemyHP > 0 {
									if share > battle.EnemyHP {
										share = battle.EnemyHP
									}
									battle.EnemyHP -= share
								}
							}
						}
					}

					// effect 21：反弹所受伤害的 1/k 给对手
					if battle.PlayerReflectRounds > 0 && battle.PlayerReflectDenom > 0 && enemyDamage > 0 {
						reflect := enemyDamage / uint32(battle.PlayerReflectDenom)
						if reflect > 0 {
							if reflect > battle.EnemyHP {
								reflect = battle.EnemyHP
							}
							battle.EnemyHP -= reflect
							if battle.EnemyHP > battle.EnemyMaxHP {
								battle.EnemyHP = 0
							}
						}
					}
					// EffectID=123：在持续期内，每当自身因技能伤害实际扣血时，按配置提升自身能力等级
					if battle.PlayerOnDamagedBuffRounds > 0 && enemyDamage > 0 {
						stat := int(battle.PlayerOnDamagedBuffStat)
						if stat >= 0 && stat < len(battle.PlayerBattleLv) {
							cur := int(battle.PlayerBattleLv[stat]) + int(battle.PlayerOnDamagedBuffDelta)
							if cur < -6 {
								cur = -6
							}
							if cur > 6 {
								cur = 6
							}
							battle.PlayerBattleLv[stat] = int8(cur)
						}
					}

					// effect 73：若处于“反击2倍”状态，则把本次受到的实际伤害 *2 反击给对手
					if battle.PlayerCounterDoubleRounds > 0 && enemyDamage > 0 {
						reflect := uint64(enemyDamage) * 2
						if reflect > uint64(battle.EnemyHP) {
							reflect = uint64(battle.EnemyHP)
						}
						if reflect > 0 {
							battle.EnemyHP -= uint32(reflect)
							if battle.EnemyHP > battle.EnemyMaxHP {
								battle.EnemyHP = 0
							}
						}
					}
				}
				// 记录本场战斗中玩家受到敌方攻击并实际扣血的次数（盖亚特训-邪气凌然判定用）
				if enemyDamage > 0 {
					battle.HitsTakenFromEnemy++
					// 记录“本回合敌方最近一次通过技能对我造成的实际伤害”（用于克制等技能）。
					// 具体反弹逻辑在我方出招阶段的 EffectID=34 分支中统一处理。
					battle.LastDamageFromEnemySkill = enemyDamage
				}

				// 回神(1028)：精灵体力降至 1/8 以下时，有3%几率体力回满
				if playerTrait == 1028 && playerHPBeforeEnemy > battle.PlayerMaxHP/8 &&
					battle.PlayerHP > 0 && battle.PlayerHP <= battle.PlayerMaxHP/8 {
					if rand.Intn(100) < 3 {
						battle.PlayerHP = battle.PlayerMaxHP
					}
				}
				// 受到普通攻击时有3%几率使对方进入异常状态（1029-1034）；仅 SPT BOSS 中雷伊/哈莫雷特等免疫，PvP 对方精灵不免疫
				if enemySkillForTurn.Category != 4 && playerTrait >= 1029 && playerTrait <= 1034 &&
					(battle.OpponentUserID != 0 || !sptboss.IsStatusImmune(battle.EnemyID)) {
					if rand.Intn(100) < 3 {
						statusIndex := -1
						switch playerTrait {
						case 1029:
							statusIndex = gameskills.StatusIndexParalysis
						case 1030:
							statusIndex = gameskills.StatusIndexPoison
						case 1031:
							statusIndex = gameskills.StatusIndexBurn
						case 1032:
							statusIndex = gameskills.StatusIndexFreeze
						case 1033:
							statusIndex = gameskills.StatusIndexFear
						case 1034:
							statusIndex = gameskills.StatusIndexSleep
						}
						if statusIndex >= 0 && statusIndex < len(battle.EnemyStatus) {
							if battle.EnemyStatus[statusIndex] == 0 {
								battle.EnemyStatus[statusIndex] = 2
							}
						}
					}
				}

				// effect 84/92/108：n 回合内受到物理攻击时，有 m% 几率令对手进入异常状态
				if enemySkillForTurn.Category == 1 && enemyDamage > 0 {
					// 我方被打：触发我方“受物理攻击反制异常”
					if battle.PlayerOnHitParalysisRounds > 0 && battle.PlayerOnHitParalysisChance > 0 &&
						(battle.OpponentUserID != 0 || !sptboss.IsStatusImmune(battle.EnemyID)) {
						if rand.Intn(100) < int(battle.PlayerOnHitParalysisChance) && battle.EnemyStatus[gameskills.StatusIndexParalysis] == 0 {
							battle.EnemyStatus[gameskills.StatusIndexParalysis] = 2
						}
					}
					if battle.PlayerOnHitFreezeRounds > 0 && battle.PlayerOnHitFreezeChance > 0 &&
						(battle.OpponentUserID != 0 || !sptboss.IsStatusImmune(battle.EnemyID)) {
						if rand.Intn(100) < int(battle.PlayerOnHitFreezeChance) && battle.EnemyStatus[gameskills.StatusIndexFreeze] == 0 {
							battle.EnemyStatus[gameskills.StatusIndexFreeze] = 2
						}
					}
					if battle.PlayerOnHitBurnRounds > 0 && battle.PlayerOnHitBurnChance > 0 &&
						(battle.OpponentUserID != 0 || !sptboss.IsStatusImmune(battle.EnemyID)) {
						if rand.Intn(100) < int(battle.PlayerOnHitBurnChance) && battle.EnemyStatus[gameskills.StatusIndexBurn] == 0 {
							battle.EnemyStatus[gameskills.StatusIndexBurn] = 2
						}
					}
				}
				// 受到特殊攻击时 5% 几率降低对方能力等级（1035-1038,1040）
				if enemySkillForTurn.Category == 2 {
					statIndex := -1
					switch playerTrait {
					case 1035:
						statIndex = gameskills.StatAttack
					case 1036:
						statIndex = gameskills.StatDefence
					case 1037:
						statIndex = gameskills.StatSpAtk
					case 1038:
						statIndex = gameskills.StatSpDef
					case 1040:
						statIndex = gameskills.StatSpeed
					}
					if statIndex >= 0 && statIndex < len(battle.EnemyBattleLv) {
						if rand.Intn(100) < 5 {
							cur := int(battle.EnemyBattleLv[statIndex])
							cur--
							// 若敌方为“免疫能力下降”的 BOSS，能力等级不低于 0；否则下限为 -6
							if sptboss.IsStatDropImmune(battle.EnemyID) && cur < 0 {
								cur = 0
							} else if cur < -6 {
								cur = -6
							}
							battle.EnemyBattleLv[statIndex] = int8(cur)
						}
					}
				}
				// 受到任何攻击时 5% 几率提升自身能力等级（1041-1045）
				boostStat := -1
				switch playerTrait {
				case 1041:
					boostStat = gameskills.StatAttack
				case 1042:
					boostStat = gameskills.StatDefence
				case 1043:
					boostStat = gameskills.StatSpAtk
				case 1044:
					boostStat = gameskills.StatSpDef
				case 1045:
					boostStat = gameskills.StatSpeed
				}
				if boostStat >= 0 && boostStat < len(battle.PlayerBattleLv) && enemySkillForTurn.Category != 4 {
					if rand.Intn(100) < 5 {
						cur := int(battle.PlayerBattleLv[boostStat])
						cur++
						if cur > 6 {
							cur = 6
						}
						battle.PlayerBattleLv[boostStat] = int8(cur)
					}
				}
			}

			// 敌方技能附加效果（红韵/魅惑等）：对敌方自身和我方的能力等级、异常状态等进行修改
			// 这里沿用 ApplyEffect 语义：
			//   - 第一个 HP/status/battleLv 参数 = 出手方（此处为敌人）
			//   - 第二个 HP/status/battleLv 参数 = 被攻击方（此处为玩家）
			// 仅当本次命中或为自身必中强化类技能时才应用效果；后者由 MustHit 标记保证。
			if enemyHit && enemySkillForTurn != nil {
				var enemyEffectRecoil uint32
				// 敌方技能命中玩家时：不根据玩家精灵 ID 启用任何 BOSS 免疫，只保留技能自身带来的临时“异常免疫”效果。
				defenderPetID := 0
				// effect 48：n 回合免疫异常状态；用占位 BOSS 使 ApplyEffect 视为异常免疫
				if battle.PlayerStatusImmuneRounds > 0 {
					defenderPetID = 261
				}
				// effect 91：若敌方处于“同步期”，则我方受到的状态变化也会同步到敌方。
				playerStatusBeforeSync := battle.PlayerStatus
				playerBattleLvBeforeSync := battle.PlayerBattleLv

				_, enemyEffectRecoil = gameskills.ApplyEffect(enemySkillForTurn, enemyDamage,
					&battle.EnemyHP, &battle.PlayerHP,
					battle.EnemyMaxHP, battle.PlayerMaxHP,
					&battle.EnemyBattleLv, &battle.PlayerBattleLv,
					&battle.EnemyStatus, &battle.PlayerStatus, defenderPetID, false)
				_ = enemyEffectRecoil

				if battle.EnemySyncOpponentChangesRounds > 0 {
					for i := 0; i < len(battle.PlayerStatus); i++ {
						if battle.PlayerStatus[i] != playerStatusBeforeSync[i] {
							battle.EnemyStatus[i] = battle.PlayerStatus[i]
						}
					}
					for i := 0; i < len(battle.PlayerBattleLv); i++ {
						if battle.PlayerBattleLv[i] != playerBattleLvBeforeSync[i] {
							battle.EnemyBattleLv[i] = battle.PlayerBattleLv[i]
						}
					}
				}

				// EffectID=87：恢复自身所有 PP（敌方版本）
				if enemySkillForTurn.EffectID == 87 {
					for i := 0; i < 4; i++ {
						sid := battle.EnemySkillIDs[i]
						if sid > 0 {
							if sk := skillMgr.Get(sid); sk != nil && sk.MaxPP > 0 {
								battle.EnemySkillPP[i] = sk.MaxPP
							}
						}
					}
				}
				// EffectID=45：敌方版本（n 回合内自身防御力与对手相同）
				if enemySkillForTurn.EffectID == 45 {
					effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
					rounds := 1
					if len(effArgs) >= 1 && effArgs[0] > 0 {
						rounds = effArgs[0]
					}
					battle.EnemyDefenceEqualRounds = byte(rounds + 1)
				}
				if enemySkillForTurn.EffectID == 86 {
					effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
					rounds := 1
					if len(effArgs) >= 1 && effArgs[0] > 0 {
						rounds = effArgs[0]
					}
					battle.EnemyAttrMoveImmuneRounds = byte(rounds + 1)
				}
				if enemySkillForTurn.EffectID == 106 {
					effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
					rounds := 1
					if len(effArgs) >= 1 && effArgs[0] > 0 {
						rounds = effArgs[0]
					}
					battle.EnemySpecImmuneRounds = byte(rounds + 1)
				}
				if enemySkillForTurn.EffectID == 109 {
					effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
					rounds, chance := 1, 0
					if len(effArgs) >= 1 && effArgs[0] > 0 {
						rounds = effArgs[0]
					}
					if len(effArgs) >= 2 && effArgs[1] > 0 {
						chance = effArgs[1]
					}
					if chance > 100 {
						chance = 100
					}
					battle.EnemyOnDamageFreezeRounds = byte(rounds + 1)
					battle.EnemyOnDamageFreezeChance = byte(chance)
				}
				if enemySkillForTurn.EffectID == 110 {
					effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
					rounds, chance, stat := 1, 0, 0
					if len(effArgs) >= 1 && effArgs[0] > 0 {
						rounds = effArgs[0]
					}
					if len(effArgs) >= 2 && effArgs[1] > 0 {
						chance = effArgs[1]
					}
					if len(effArgs) >= 3 {
						stat = effArgs[2]
					}
					if chance > 100 {
						chance = 100
					}
					if stat < 0 {
						stat = 0
					}
					if stat > 5 {
						stat = 5
					}
					battle.EnemyOnDodgeBuffRounds = byte(rounds + 1)
					battle.EnemyOnDodgeBuffChance = byte(chance)
					battle.EnemyOnDodgeBuffStat = byte(stat)
				}
				if enemySkillForTurn.EffectID == 91 {
					effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
					rounds := 1
					if len(effArgs) >= 1 && effArgs[0] > 0 {
						rounds = effArgs[0]
					}
					battle.EnemySyncOpponentChangesRounds = byte(rounds + 1)
				}

				// EffectID=59：敌方版本的“传承”技能（若 BOSS/对手拥有此类技能）
				// 逻辑与玩家相同：牺牲当前出战精灵，使下一只出战精灵的若干能力等级+1。
				if enemySkillForTurn.EffectID == 59 {
					effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
					stats := []int{}
					if len(effArgs) >= 1 {
						stats = append(stats, effArgs[0])
					}
					if len(effArgs) >= 2 {
						stats = append(stats, effArgs[1])
					}
					for _, sIdx := range stats {
						if sIdx < 0 || sIdx >= len(battle.EnemyNextPetBattleLvBoost) {
							continue
						}
						cur := int(battle.EnemyNextPetBattleLvBoost[sIdx]) + 1
						if cur > 6 {
							cur = 6
						}
						battle.EnemyNextPetBattleLvBoost[sIdx] = int8(cur)
					}
					// 敌方当前精灵体力清零
					battle.EnemyHP = 0
				}
				// EffectID=55：敌方版本的“属性反转”（属性交换）
				if enemySkillForTurn.EffectID == 55 {
					effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
					rounds := 1
					if len(effArgs) >= 1 && effArgs[0] > 0 {
						rounds = effArgs[0]
					}
					if enemyPet != nil && attackerPet != nil {
						battle.EnemyTypeOverride1 = byte(attackerPet.Type)
						battle.EnemyTypeOverride2 = byte(attackerPet.Type2)
						battle.PlayerTypeOverride1 = byte(enemyPet.Type)
						battle.PlayerTypeOverride2 = byte(enemyPet.Type2)
						battle.EnemyTypeOverrideRounds = byte(rounds + 1)
						battle.PlayerTypeOverrideRounds = byte(rounds + 1)
					}
				}
				// EffectID=56：敌方版本的“属性复制”（复制玩家属性到敌方）
				if enemySkillForTurn.EffectID == 56 {
					effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
					rounds := 1
					if len(effArgs) >= 1 && effArgs[0] > 0 {
						rounds = effArgs[0]
					}
					if enemyPet != nil && attackerPet != nil {
						battle.EnemyTypeOverride1 = byte(attackerPet.Type)
						battle.EnemyTypeOverride2 = byte(attackerPet.Type2)
						battle.EnemyTypeOverrideRounds = byte(rounds + 1)
					}
				}
				// EffectID=71：敌方版本的“电闪光华阵”类技能——牺牲当前出战精灵，
				// 使后续 N 次攻击技能必定致命一击（跨精灵继承，直到次数用完）。
				if enemySkillForTurn.EffectID == 71 {
					effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
					hits := 2
					if len(effArgs) >= 1 && effArgs[0] > 0 {
						hits = effArgs[0]
					}
					if hits < 0 {
						hits = 0
					}
					if hits > 0 {
						sum := int(battle.EnemyNextGuaranteedCritHits) + hits
						if sum > 255 {
							sum = 255
						}
						battle.EnemyNextGuaranteedCritHits = byte(sum)
					}
					battle.EnemyHP = 0
				}

				// 敌方 PP 削减效果（SideEffect 39）：命中时有 n% 几率，使我方所有技能 PP 各减少 m 点。
				if enemySkillForTurn.EffectID == 39 {
					effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
					chance := 100
					amount := 1
					if len(effArgs) >= 1 && effArgs[0] > 0 {
						chance = effArgs[0]
					}
					if len(effArgs) >= 2 && effArgs[1] > 0 {
						amount = effArgs[1]
					}
					if chance < 0 {
						chance = 0
					}
					if chance > 100 {
						chance = 100
					}
					if amount < 1 {
						amount = 1
					}
					if rand.Intn(100) < chance {
						for i := 0; i < 4; i++ {
							if battle.PlayerSkillIDs[i] <= 0 || battle.PlayerSkillPP[i] <= 0 {
								continue
							}
							battle.PlayerSkillPP[i] -= amount
							if battle.PlayerSkillPP[i] < 0 {
								battle.PlayerSkillPP[i] = 0
							}
						}
					}
				}

				// 敌方暴击率提升效果（SideEffect 58 系列）
				if enemySkillForTurn.EffectID == 58 {
					effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
					if len(effArgs) >= 1 {
						rounds := effArgs[0]
						if rounds < 0 {
							rounds = 0
						}
						if rounds > 0 {
							// +1 同理，保证敌方也能获得 param1[0] 个完整回合的必定暴击效果
							battle.EnemyCritBuffRounds = byte(rounds + 1)
						}
					}
				}
				// 敌方暴击率+1/16 效果（SideEffect 32 系列）
				if enemySkillForTurn.EffectID == 32 {
					effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
					if len(effArgs) >= 1 {
						rounds := effArgs[0]
						if rounds < 0 {
							rounds = 0
						}
						if rounds > 0 {
							battle.EnemyCritBonusRounds = byte(rounds + 1)
						}
					}
				}
				// 敌方多回合效果（41/42/44/46/50/54/78/81/83/98 等）
				if enemySkillForTurn.EffectID == 41 {
					effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
					rounds := 3
					if len(effArgs) >= 2 && effArgs[1] > 0 {
						rounds = effArgs[1]
					}
					battle.EnemyFireResistRounds = byte(rounds + 1)
				}
				if enemySkillForTurn.EffectID == 44 {
					effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
					rounds := 3
					if len(effArgs) >= 1 && effArgs[0] > 0 {
						rounds = effArgs[0]
					}
					battle.EnemySpecDefRounds = byte(rounds + 1)
				}
				if enemySkillForTurn.EffectID == 46 {
					effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
					rounds := 1
					if len(effArgs) >= 1 && effArgs[0] > 0 {
						rounds = effArgs[0]
					}
					battle.EnemyShieldRounds = byte(rounds + 1)
				}
				if enemySkillForTurn.EffectID == 50 {
					effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
					rounds := 3
					if len(effArgs) >= 1 && effArgs[0] > 0 {
						rounds = effArgs[0]
					}
					battle.EnemyPhysDefRounds = byte(rounds + 1)
				}
				if enemySkillForTurn.EffectID == 54 {
					effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
					rounds, denom := 2, 2
					if len(effArgs) >= 1 && effArgs[0] > 0 {
						rounds = effArgs[0]
					}
					if len(effArgs) >= 2 && effArgs[1] > 0 {
						denom = effArgs[1]
					}
					battle.PlayerDamageHalvedRounds = byte(rounds + 1)
					battle.PlayerDamageHalvedDenom = byte(denom)
				}
				if enemySkillForTurn.EffectID == 78 {
					effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
					rounds := 2
					if len(effArgs) >= 1 && effArgs[0] > 0 {
						rounds = effArgs[0]
					}
					battle.EnemyPhysImmuneRounds = byte(rounds + 1)
				}
				if enemySkillForTurn.EffectID == 81 {
					effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
					rounds := 1
					if len(effArgs) >= 1 && effArgs[0] > 0 {
						rounds = effArgs[0]
					}
					battle.EnemyMustHitRounds = byte(rounds + 1)
				}
				if enemySkillForTurn.EffectID == 83 {
					// 敌方雄性：下 N 回合必定先手；雌性：下 N 回合必定暴击
					effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
					rounds := 2
					if len(effArgs) >= 1 && effArgs[0] > 0 {
						rounds = effArgs[0]
					}
					if enemyPet != nil {
						if enemyPet.Gender == 1 {
							battle.EnemyFirstPriorityRounds = byte(rounds + 1)
						} else if enemyPet.Gender == 2 {
							if rounds > 0 {
								battle.EnemyCritBuffRounds = byte(rounds + 1)
							}
						}
					}
				}
				if enemySkillForTurn.EffectID == 98 {
					effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
					rounds, mul := 1, 2
					if len(effArgs) >= 1 && effArgs[0] > 0 {
						rounds = effArgs[0]
					}
					if len(effArgs) >= 2 && effArgs[1] > 0 {
						mul = effArgs[1]
					}
					battle.EnemyMaleDmgMulRounds = byte(rounds + 1)
					battle.EnemyMaleDmgMul = byte(mul)
				}
				if enemySkillForTurn.EffectID == 68 {
					battle.EnemyEndureRounds = 2
				}
			}

			// 敌方若也配置了“不灭之火”效果，同样能在若干回合内令我方每回合受到固定伤害
			if enemySkillForTurn.ID == 11023 {
				effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
				rounds, perTurn := 0, 0
				if len(effArgs) >= 1 {
					rounds = effArgs[0]
				}
				if len(effArgs) >= 2 {
					perTurn = effArgs[1]
				}
				if rounds < 0 {
					rounds = 0
				}
				if perTurn < 0 {
					perTurn = 0
				}
				if rounds > 0 && perTurn > 0 {
					battle.PlayerFixedDotRounds = byte(rounds)
					battle.PlayerFixedDotDamage = uint32(perTurn)
				}
			}
			// 敌方多回合固定伤害（effect 60 / 76）同样生效：目标为我方
			if enemySkillForTurn.EffectID == 60 {
				effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
				rounds, perTurn := 0, 0
				if len(effArgs) >= 1 {
					rounds = effArgs[0]
				}
				if len(effArgs) >= 2 {
					perTurn = effArgs[1]
				}
				if rounds > 0 && perTurn > 0 {
					battle.PlayerFixedDotRounds = byte(rounds)
					battle.PlayerFixedDotDamage = uint32(perTurn)
				}
			} else if enemySkillForTurn.EffectID == 76 {
				effArgs := gameskills.ParseSideEffectArg(enemySkillForTurn.SideEffectArg)
				chance, rounds, perTurn := 0, 0, 0
				if len(effArgs) >= 1 {
					chance = effArgs[0]
				}
				if len(effArgs) >= 2 {
					rounds = effArgs[1]
				}
				if len(effArgs) >= 3 {
					perTurn = effArgs[2]
				}
				if chance > 0 && rounds > 0 && perTurn > 0 && rand.Intn(100) < chance {
					battle.PlayerFixedDotRounds = byte(rounds)
					battle.PlayerFixedDotDamage = uint32(perTurn)
				}
			}

			// 敌方出手结束后，同样结算一次多回合固定伤害（若存在）
			applyFixedDotAfterAttack(&battle.PlayerHP, &battle.PlayerFixedDotDamage, &battle.PlayerFixedDotRounds)
			}
		}
	}
	}
	// 盖亚/雷伊先手：先执行敌方回合，若我方仍存活再执行我方出招；否则我方本回合未出招
	if isGaiyaFirst {
		doEnemyTurn()
		// 敌方先手时，若敌方本回合先手技能新施加了“控制类异常”（害怕/睡眠/麻痹/疲惫），
		// 需要在我方出招前再次判定，使控制当回合立刻生效。
		// 注意：回合开始处已对“已有控制状态”做过递减；这里仅处理“本回合新挂上”的情况，避免双重递减。
		if battle.PlayerHP > 0 && !skipPlayerAction {
			newlyControlled := false
			if battle.PlayerStatus[gameskills.StatusIndexFear] > 0 {
				newlyControlled = true
				battle.PlayerStatus[gameskills.StatusIndexFear]--
			}
			if battle.PlayerStatus[gameskills.StatusIndexSleep] > 0 {
				newlyControlled = true
				battle.PlayerStatus[gameskills.StatusIndexSleep]--
			}
			if battle.PlayerStatus[gameskills.StatusIndexParalysis] > 0 {
				newlyControlled = true
				battle.PlayerStatus[gameskills.StatusIndexParalysis]--
			}
			if battle.PlayerStatus[gameskills.StatusIndexFatigue] > 0 {
				newlyControlled = true
				battle.PlayerStatus[gameskills.StatusIndexFatigue]--
			}
			if newlyControlled {
				skipPlayerAction = true
			}
		}
		if battle.PlayerHP > 0 && !skipPlayerAction {
			fromGaiyaFirst = true
			goto doPlayerTurn
		}
		playerTurnSkippedBecauseDead = true
		goto afterEnemyTurn
	}
	if !isGaiyaFirst {
		// 若我方已在本回合出招后死亡（如“传承”牺牲自身），则不再执行敌方反击。
		// 同时也避免“已死亡仍被攻击一次”导致客户端体验不一致。
		if battle.PlayerHP > 0 && !playerFaintedFromOwnSkillThisTurn {
			doEnemyTurn()
		}
	}
afterEnemyTurn:

	// 一整个战斗回合结束：暴击强化/提升回合数及多回合效果自动减 1
	if battle.PlayerCritBuffRounds > 0 {
		battle.PlayerCritBuffRounds--
	}
	if battle.EnemyCritBuffRounds > 0 {
		battle.EnemyCritBuffRounds--
	}
	if battle.PlayerCritBonusRounds > 0 {
		battle.PlayerCritBonusRounds--
	}
	if battle.EnemyCritBonusRounds > 0 {
		battle.EnemyCritBonusRounds--
	}
	if battle.PlayerFireResistRounds > 0 {
		battle.PlayerFireResistRounds--
	}
	if battle.PlayerElectricPowerRounds > 0 {
		battle.PlayerElectricPowerRounds--
	}
	if battle.PlayerSpecDefRounds > 0 {
		battle.PlayerSpecDefRounds--
	}
	if battle.PlayerStatusImmuneRounds > 0 {
		battle.PlayerStatusImmuneRounds--
	}
	if battle.PlayerPhysDefRounds > 0 {
		battle.PlayerPhysDefRounds--
	}
	if battle.PlayerDefenceEqualRounds > 0 {
		battle.PlayerDefenceEqualRounds--
	}
	if battle.PlayerAttackEqualRounds > 0 {
		battle.PlayerAttackEqualRounds--
	}
	if battle.PlayerTypeOverrideRounds > 0 {
		battle.PlayerTypeOverrideRounds--
	}
	if battle.PlayerSyncOpponentChangesRounds > 0 {
		battle.PlayerSyncOpponentChangesRounds--
	}
	if battle.PlayerCounterDoubleRounds > 0 {
		battle.PlayerCounterDoubleRounds--
	}
	if battle.PlayerAttrMoveImmuneRounds > 0 {
		battle.PlayerAttrMoveImmuneRounds--
	}
	if battle.PlayerOnDamageFreezeRounds > 0 {
		battle.PlayerOnDamageFreezeRounds--
	}
	if battle.PlayerOnDodgeBuffRounds > 0 {
		battle.PlayerOnDodgeBuffRounds--
	}
	if battle.PlayerSpecImmuneRounds > 0 {
		battle.PlayerSpecImmuneRounds--
	}
	if battle.PlayerOnHitParalysisRounds > 0 {
		battle.PlayerOnHitParalysisRounds--
	}
	if battle.PlayerOnHitFreezeRounds > 0 {
		battle.PlayerOnHitFreezeRounds--
	}
	if battle.PlayerOnHitBurnRounds > 0 {
		battle.PlayerOnHitBurnRounds--
	}
	if battle.EnemyDamageHalvedRounds > 0 {
		battle.EnemyDamageHalvedRounds--
	}
	if battle.PlayerHealPerSkillRounds > 0 {
		battle.PlayerHealPerSkillRounds--
	}
	if battle.PlayerEndureRounds > 0 {
		battle.PlayerEndureRounds--
	}
	if battle.PlayerPhysImmuneRounds > 0 {
		battle.PlayerPhysImmuneRounds--
	}
	if battle.PlayerMustHitRounds > 0 {
		battle.PlayerMustHitRounds--
	}
	if battle.PlayerVampRounds > 0 {
		battle.PlayerVampRounds--
	}
	if battle.PlayerDmgMulRounds > 0 {
		battle.PlayerDmgMulRounds--
	}
	if battle.EnemyFireResistRounds > 0 {
		battle.EnemyFireResistRounds--
	}
	if battle.EnemySpecDefRounds > 0 {
		battle.EnemySpecDefRounds--
	}
	if battle.EnemyShieldRounds > 0 {
		battle.EnemyShieldRounds--
	}
	if battle.EnemyPhysDefRounds > 0 {
		battle.EnemyPhysDefRounds--
	}
	if battle.EnemyDefenceEqualRounds > 0 {
		battle.EnemyDefenceEqualRounds--
	}
	if battle.EnemyAttackEqualRounds > 0 {
		battle.EnemyAttackEqualRounds--
	}
	if battle.EnemyTypeOverrideRounds > 0 {
		battle.EnemyTypeOverrideRounds--
	}
	if battle.EnemySyncOpponentChangesRounds > 0 {
		battle.EnemySyncOpponentChangesRounds--
	}
	if battle.EnemyCounterDoubleRounds > 0 {
		battle.EnemyCounterDoubleRounds--
	}
	if battle.EnemyAttrMoveImmuneRounds > 0 {
		battle.EnemyAttrMoveImmuneRounds--
	}
	if battle.EnemyOnDamageFreezeRounds > 0 {
		battle.EnemyOnDamageFreezeRounds--
	}
	if battle.EnemyOnDodgeBuffRounds > 0 {
		battle.EnemyOnDodgeBuffRounds--
	}
	if battle.EnemySpecImmuneRounds > 0 {
		battle.EnemySpecImmuneRounds--
	}
	if battle.EnemyOnHitParalysisRounds > 0 {
		battle.EnemyOnHitParalysisRounds--
	}
	if battle.EnemyOnHitFreezeRounds > 0 {
		battle.EnemyOnHitFreezeRounds--
	}
	if battle.EnemyOnHitBurnRounds > 0 {
		battle.EnemyOnHitBurnRounds--
	}
	if battle.PlayerDamageHalvedRounds > 0 {
		battle.PlayerDamageHalvedRounds--
	}
	if battle.EnemyPhysImmuneRounds > 0 {
		battle.EnemyPhysImmuneRounds--
	}
	if battle.EnemyMustHitRounds > 0 {
		battle.EnemyMustHitRounds--
	}
	if battle.EnemyEndureRounds > 0 {
		battle.EnemyEndureRounds--
	}
	if battle.PlayerHealPerSkillPointsRounds > 0 {
		battle.PlayerHealPerSkillPointsRounds--
	}
	if battle.PlayerDamageConvertRounds > 0 {
		battle.PlayerDamageConvertRounds--
	}
	if battle.PlayerReflectRounds > 0 {
		battle.PlayerReflectRounds--
	}
	if battle.PlayerTypePowerRounds > 0 {
		battle.PlayerTypePowerRounds--
	}
	if battle.PlayerFirstPriorityRounds > 0 {
		battle.PlayerFirstPriorityRounds--
	}
	if battle.EnemyFirstPriorityRounds > 0 {
		battle.EnemyFirstPriorityRounds--
	}
	if battle.PlayerMaleDmgMulRounds > 0 {
		battle.PlayerMaleDmgMulRounds--
	}
	if battle.EnemyMaleDmgMulRounds > 0 {
		battle.EnemyMaleDmgMulRounds--
	}
	// 先手吸血 / 先手害怕 持续回合递减
	if battle.PlayerFirstVampRounds > 0 {
		battle.PlayerFirstVampRounds--
	}
	if battle.EnemyFirstVampRounds > 0 {
		battle.EnemyFirstVampRounds--
	}
	if battle.PlayerFirstFearRounds > 0 {
		battle.PlayerFirstFearRounds--
	}
	if battle.EnemyFirstFearRounds > 0 {
		battle.EnemyFirstFearRounds--
	}

	// 保存敌人反击后的玩家HP（用于 2505 的 remainHP，buildAttackValue 内会再钳位到 [0,maxHP]）
	playerHPAfterCounter := battle.PlayerHP

	// ---------- 克制（EffectID=34）最终反弹结算 ----------
	// 在敌人攻击结束后，此时 battle.LastDamageFromEnemySkill 已记录了
	// 本回合敌人最近一次通过技能对我造成的实际伤害（不含异常/固伤）。
	if skill != nil && skill.EffectID == 34 && !playerTurnSkippedBecauseDead {
		// 仅在记录仍是当前回合且确实受到了伤害时生效
		if battle.LastDamageFromEnemySkill > 0 && battle.LastDamageFromEnemySkillRound == battle.RoundCount {
			mult := 2
			effArgs := gameskills.ParseSideEffectArg(skill.SideEffectArg)
			if len(effArgs) >= 1 && effArgs[0] > 0 {
				mult = effArgs[0]
			}
			reflect := battle.LastDamageFromEnemySkill * uint32(mult)
			if reflect > battle.EnemyHP {
				reflect = battle.EnemyHP
			}
			// 将本回合玩家造成的直接伤害视为这次反弹伤害
			damage = reflect
			damageCalc = reflect
			// 扣除敌人 HP
			battle.EnemyHP -= reflect
			if battle.EnemyHP > battle.EnemyMaxHP {
				// uint 下溢保护
				battle.EnemyHP = 0
			}
			// 若反弹伤害 >0，则视为本回合玩家命中一次，用于 2505 atkTimes 与前端表现
			if reflect > 0 {
				playerHit = true
			}
		} else {
			// 敌人本回合未对我造成伤害时，克制出招但伤害为 0
			damage = 0
			damageCalc = 0
		}
		// 使用一次后清空记录，避免跨回合误用
		battle.LastDamageFromEnemySkill = 0
	}

	// PvP 时需用对方 userID 填第二个 AttackValue，客户端才能正确匹配"对方"血条与图标
	opponentUID := battle.OpponentUserID

	// 若为 PvP，本回合结算完毕后需清除双方“已选择技能”标记与“本回合是否切换精灵”标记，等待下一回合重新选招/换宠。
	if opponentUID != 0 {
		if opponentBattle, ok := ctx.GameServer.BattleStates[opponentUID]; ok {
			// ==================== PvP 状态镜像同步（关键修复）====================
			// 说明：
			// - 当前回合的结算只会在“第二个到达的 2405”所在协程内执行一次；
			// - 结算时我们修改的是当前 ctx.UserID 的 BattleState（battle）中的 Player/Enemy HP、状态与 battleLv；
			// - 若不把结果同步回对手 BattleState，则下一回合若由对手触发结算，会用到旧的 PlayerBattleLv/Status，
			//   表现为“属性技能只第一次生效、第二次不叠加”等不同步问题。
			// 映射规则（对手视角）：
			// - opponentBattle.Player*   <-> battle.Enemy*
			// - opponentBattle.Enemy*    <-> battle.Player*
			opponentBattle.PlayerHP = battle.EnemyHP
			opponentBattle.PlayerMaxHP = battle.EnemyMaxHP
			opponentBattle.PlayerStatus = battle.EnemyStatus
			opponentBattle.PlayerBattleLv = battle.EnemyBattleLv
			opponentBattle.PlayerCritBuffRounds = battle.EnemyCritBuffRounds
			opponentBattle.PlayerCritBonusRounds = battle.EnemyCritBonusRounds
			opponentBattle.PlayerFireResistRounds = battle.EnemyFireResistRounds
			opponentBattle.PlayerElectricPowerRounds = battle.EnemyElectricPowerRounds
			opponentBattle.PlayerSpecDefRounds = battle.EnemySpecDefRounds
			opponentBattle.PlayerShieldRounds = battle.EnemyShieldRounds
			opponentBattle.PlayerStatusImmuneRounds = battle.EnemyStatusImmuneRounds
			opponentBattle.PlayerPhysDefRounds = battle.EnemyPhysDefRounds
			opponentBattle.PlayerDefenceEqualRounds = battle.EnemyDefenceEqualRounds
			opponentBattle.PlayerAttackEqualRounds = battle.EnemyAttackEqualRounds
			opponentBattle.PlayerTypeOverride1 = battle.EnemyTypeOverride1
			opponentBattle.PlayerTypeOverride2 = battle.EnemyTypeOverride2
			opponentBattle.PlayerTypeOverrideRounds = battle.EnemyTypeOverrideRounds
			opponentBattle.PlayerHealPerSkillRounds = battle.EnemyHealPerSkillRounds
			opponentBattle.PlayerHealPerSkillDenom = battle.EnemyHealPerSkillDenom
			opponentBattle.PlayerEndureRounds = battle.EnemyEndureRounds
			opponentBattle.PlayerPhysImmuneRounds = battle.EnemyPhysImmuneRounds
			opponentBattle.PlayerMustHitRounds = battle.EnemyMustHitRounds
			opponentBattle.PlayerVampRounds = battle.EnemyVampRounds
			opponentBattle.PlayerVampDenom = battle.EnemyVampDenom
			opponentBattle.PlayerDmgMulRounds = battle.EnemyDmgMulRounds
			opponentBattle.PlayerDmgMul = battle.EnemyDmgMul
			// 先手吸血 / 先手害怕 状态在 PvP 中镜像同步：对手视角下“我方”的 First* 来源于我这边的 EnemyFirst*。
			opponentBattle.PlayerFirstVampRounds = battle.EnemyFirstVampRounds
			opponentBattle.PlayerFirstFearRounds = battle.EnemyFirstFearRounds
			opponentBattle.EnemyDamageHalvedRounds = battle.PlayerDamageHalvedRounds
			opponentBattle.EnemyDamageHalvedDenom = battle.PlayerDamageHalvedDenom

			opponentBattle.EnemyHP = battle.PlayerHP
			opponentBattle.EnemyMaxHP = battle.PlayerMaxHP
			opponentBattle.EnemyStatus = battle.PlayerStatus
			opponentBattle.EnemyBattleLv = battle.PlayerBattleLv
			opponentBattle.EnemyCritBuffRounds = battle.PlayerCritBuffRounds
			opponentBattle.EnemyCritBonusRounds = battle.PlayerCritBonusRounds
			opponentBattle.EnemyFireResistRounds = battle.PlayerFireResistRounds
			opponentBattle.EnemySpecDefRounds = battle.PlayerSpecDefRounds
			opponentBattle.EnemyShieldRounds = battle.PlayerShieldRounds
			opponentBattle.EnemyPhysDefRounds = battle.PlayerPhysDefRounds
			opponentBattle.EnemyDefenceEqualRounds = battle.PlayerDefenceEqualRounds
			opponentBattle.EnemyAttackEqualRounds = battle.PlayerAttackEqualRounds
			opponentBattle.EnemyTypeOverride1 = battle.PlayerTypeOverride1
			opponentBattle.EnemyTypeOverride2 = battle.PlayerTypeOverride2
			opponentBattle.EnemyTypeOverrideRounds = battle.PlayerTypeOverrideRounds
			opponentBattle.PlayerAttrMoveImmuneRounds = battle.EnemyAttrMoveImmuneRounds
			opponentBattle.EnemyAttrMoveImmuneRounds = battle.PlayerAttrMoveImmuneRounds
			opponentBattle.PlayerSpecImmuneRounds = battle.EnemySpecImmuneRounds
			opponentBattle.EnemySpecImmuneRounds = battle.PlayerSpecImmuneRounds
			opponentBattle.PlayerOnDamageFreezeRounds = battle.EnemyOnDamageFreezeRounds
			opponentBattle.PlayerOnDamageFreezeChance = battle.EnemyOnDamageFreezeChance
			opponentBattle.EnemyOnDamageFreezeRounds = battle.PlayerOnDamageFreezeRounds
			opponentBattle.EnemyOnDamageFreezeChance = battle.PlayerOnDamageFreezeChance
			opponentBattle.PlayerOnDodgeBuffRounds = battle.EnemyOnDodgeBuffRounds
			opponentBattle.PlayerOnDodgeBuffChance = battle.EnemyOnDodgeBuffChance
			opponentBattle.PlayerOnDodgeBuffStat = battle.EnemyOnDodgeBuffStat
			opponentBattle.EnemyOnDodgeBuffRounds = battle.PlayerOnDodgeBuffRounds
			opponentBattle.EnemyOnDodgeBuffChance = battle.PlayerOnDodgeBuffChance
			opponentBattle.EnemyOnDodgeBuffStat = battle.PlayerOnDodgeBuffStat
			opponentBattle.EnemyPhysImmuneRounds = battle.PlayerPhysImmuneRounds
			opponentBattle.EnemyMustHitRounds = battle.PlayerMustHitRounds
			opponentBattle.PlayerOnHitParalysisRounds = battle.EnemyOnHitParalysisRounds
			opponentBattle.PlayerOnHitParalysisChance = battle.EnemyOnHitParalysisChance
			opponentBattle.PlayerOnHitFreezeRounds = battle.EnemyOnHitFreezeRounds
			opponentBattle.PlayerOnHitFreezeChance = battle.EnemyOnHitFreezeChance
			opponentBattle.PlayerOnHitBurnRounds = battle.EnemyOnHitBurnRounds
			opponentBattle.PlayerOnHitBurnChance = battle.EnemyOnHitBurnChance
			opponentBattle.EnemyOnHitParalysisRounds = battle.PlayerOnHitParalysisRounds
			opponentBattle.EnemyOnHitParalysisChance = battle.PlayerOnHitParalysisChance
			opponentBattle.EnemyOnHitFreezeRounds = battle.PlayerOnHitFreezeRounds
			opponentBattle.EnemyOnHitFreezeChance = battle.PlayerOnHitFreezeChance
			opponentBattle.EnemyOnHitBurnRounds = battle.PlayerOnHitBurnRounds
			opponentBattle.EnemyOnHitBurnChance = battle.PlayerOnHitBurnChance
			opponentBattle.PlayerDamageHalvedRounds = battle.EnemyDamageHalvedRounds
			opponentBattle.PlayerDamageHalvedDenom = battle.EnemyDamageHalvedDenom
			opponentBattle.EnemyEndureRounds = battle.PlayerEndureRounds
			// 对手视角下“敌方”的 First* 来源于我这边的 PlayerFirst*
			opponentBattle.EnemyFirstVampRounds = battle.PlayerFirstVampRounds
			opponentBattle.EnemyFirstFearRounds = battle.PlayerFirstFearRounds

			// 回合数保持一致（用于日志/部分按回合计数的机制）
			opponentBattle.RoundCount = battle.RoundCount

			opponentBattle.PvPHasSelected = false
			opponentBattle.PvPSelectedSkillID = 0
			opponentBattle.PvPChangedPetThisRound = false
		}
		battle.PvPHasSelected = false
		battle.PvPSelectedSkillID = 0
		battle.PvPChangedPetThisRound = false
	}

	ctx.GameServer.BattleMu.Unlock()

	// 构建 2505 响应：两个 AttackValue（出手顺序：盖亚261 时敌人先手，否则玩家先手）
	// 对齐 Lua / 2407 的约定：
	// - AttackValue.userID 表示“出手方”
	// - lostHP 表示“被攻击方本次损失的血量”
	// - remainHP/maxHP 表示“被攻击方当前血量/最大血量”（用于客户端更新血条）
	enemyUserID := uint32(0)
	if opponentUID != 0 {
		enemyUserID = uint32(opponentUID)
	}
	isCritU32 := uint32(0)
	if isCritPlayer {
		isCritU32 = 1
	}
	playerAtkTimesU32 := uint32(1)
	if !playerHit || playerTurnSkippedBecauseDead {
		playerAtkTimesU32 = 0
		damageCalc = 0
		isCritU32 = 0
		if playerTurnSkippedBecauseDead {
			effectGainHP = 0
		}
	}
	enemyAtkTimesU32 := uint32(1)
	isCritEnemyU32 := uint32(0)
	if isCritEnemyFor2505 {
		isCritEnemyU32 = 1
	}
	if !enemyAttempted || !enemyHitFor2505 {
		enemyAtkTimesU32 = 0
		enemyDamageCalc = 0
		isCritEnemyU32 = 0
	}
	playerSkillIDFor2505 := skillID
	// 若本回合因死亡或控制类异常（睡眠/麻痹/害怕/疲惫）无法行动，则在 2505 中不显示技能，
	// 避免客户端出现“已使用某技能”的误导提示。
	if playerTurnSkippedBecauseDead || skipPlayerAction {
		playerSkillIDFor2505 = 0
	}
	// 设置 2505 中用于显示的精灵属性类型（petType），避免客户端在战斗中错误清空属性图标。
	playerPetTypeU32 := uint32(0)
	if def := petMgr.Get(playerPetID); def != nil {
		playerPetTypeU32 = uint32(def.Type)
	}
	enemyPetTypeU32 := uint32(0)
	if def := petMgr.Get(battle.EnemyID); def != nil {
		enemyPetTypeU32 = uint32(def.Type)
	}

	playerAv := buildAttackValueWithSkills(
		uint32(ctx.UserID), playerSkillIDFor2505, playerAtkTimesU32,
		damageCalc, effectGainHP,
		int32(playerHPAfterCounter), battle.PlayerMaxHP,
		0, isCritU32, playerPetTypeU32,
		battle.PlayerStatus, battle.PlayerBattleLv,
		battle.PlayerSkillIDs, battle.PlayerSkillPP,
	)
	enemyAv := buildAttackValue(
		enemyUserID, enemySkillID, enemyAtkTimesU32,
		enemyDamageCalc, enemyEffectGainHP,
		int32(battle.EnemyHP), battle.EnemyMaxHP,
		0, isCritEnemyU32, enemyPetTypeU32,
		battle.EnemyStatus, battle.EnemyBattleLv,
	)
	body := make([]byte, 0, 160)
	// 先手由速度+先制比较决定（雷伊/盖亚 技能先制+6）：2505 按实际出手顺序
	if isGaiyaFirst {
		body = append(body, enemyAv...)
		body = append(body, playerAv...)
	} else {
		body = append(body, playerAv...)
		body = append(body, enemyAv...)
	}

	ctx.GameServer.SendResponse(ctx.ClientData, 2505, ctx.UserID, ctx.SeqID, body)
	// PvP：向对方也发送 2505（同一 body：first=攻击方，second=对方），对方客户端用 userID 匹配更新我方/对方血条
	if opponentUID != 0 {
		if otherClient := ctx.GameServer.GetClientByUserID(opponentUID); otherClient != nil {
			ctx.GameServer.SendResponse(otherClient, 2505, opponentUID, 0, body)
		}
	}

	logger.Info(fmt.Sprintf("[2405] 使用技能后: PlayerHP=%d/%d EnemyHP=%d/%d Damage=%d EnemyDamage=%d",
		battle.PlayerHP, battle.PlayerMaxHP, battle.EnemyHP, battle.EnemyMaxHP, damage, enemyDamage))

	// 对战过程中每回合都写回当前精灵 HP/PP，不依赖战败或逃跑时才同步（逃跑/断线后背包也能看到正确血量与 PP）
	syncBattleHPToUser(ctx.GameServer, ctx.UserID, battle)

	// 检查战斗是否结束
	isOver := false
	winnerID := uint32(0)

	// 敌方 HP 为 0：PvP 多精灵时仅当对方全部被击败才结束；PvE 或对方全灭则玩家获胜
	if battle.EnemyHP == 0 {
		if battle.OpponentUserID != 0 {
			// PvP：对方当前精灵被击败，同步到对方 BattleState 并写回背包（避免败方显示满血）
			// 同时更新对方 DeadPlayerPets，否则若本回合先处理的是己方 2405，对方视角的“已死精灵数”会少计，导致对方全灭时仍提示换宠
			opponentUID := battle.OpponentUserID
			if ob := ctx.GameServer.BattleStates[opponentUID]; ob != nil {
				ob.PlayerHP = 0
				ob.DeadPlayerPets++
				if ob.TotalPlayerPets <= 0 {
					ob.TotalPlayerPets = 1
				}
				syncBattleHPToUser(ctx.GameServer, opponentUID, ob)
			}
			battle.DeadEnemyPets++
			if battle.TotalEnemyPets <= 0 {
				battle.TotalEnemyPets = 1 // 兼容单精灵/旧数据
			}
			if battle.DeadEnemyPets >= battle.TotalEnemyPets {
				isOver = true
				winnerID = uint32(ctx.UserID)
				logger.Info(fmt.Sprintf("[2405] 战斗结束: 玩家获胜(对方全部精灵被击败) TotalEnemy=%d DeadEnemy=%d", battle.TotalEnemyPets, battle.DeadEnemyPets))
			} else {
				logger.Info(fmt.Sprintf("[2405] 对方当前精灵被击败，等待对方 2407 换宠 (TotalEnemy=%d DeadEnemy=%d)", battle.TotalEnemyPets, battle.DeadEnemyPets))
				battle.PvPHasSelected = false
				if ob2 := ctx.GameServer.BattleStates[opponentUID]; ob2 != nil {
					ob2.PvPHasSelected = false
				}
			}
		} else {
			// PvE：玄武/青龙空间守护兽多精灵一场时，先换下一只不结束战斗
			// 玄武空间(401) 守护兽：同一场战斗内对方多精灵，击败当前只后换下一只上场
			if sptboss.IsXuanWuGuardianBoss(battle.BattleMapID, battle.BossRegion) && battle.BossRegion < 5 {
			bit := uint32(1) << battle.BossRegion
			user.DefeatedXuanWuGuardianMask |= bit
			nextRegion := battle.BossRegion + 1
			nextEntry, ok := sptboss.GetByMapAndParam(sptboss.MapIDXuanWuSpace, nextRegion)
			if ok {
				// 先发本回合 2505（我方出招+敌方 HP=0），客户端据此执行 execute() 并 npcIsdied=true，再收到 2407 后会调 nextRound() 更新对方模型
				damageFor2505 := damageCalc
				if damageFor2505 > battle.EnemyMaxHP {
					damageFor2505 = battle.EnemyMaxHP
				}
				playerAv := buildAttackValueWithSkills(
					uint32(ctx.UserID), skillID, 1,
					damageFor2505, 0,
					int32(battle.PlayerHP), battle.PlayerMaxHP,
					0, 0, 0,
					battle.PlayerStatus, battle.PlayerBattleLv,
					battle.PlayerSkillIDs, battle.PlayerSkillPP,
				)
				enemyAv := buildAttackValue(
					0, 0, 0,
					0, 0,
					0, battle.EnemyMaxHP,
					0, 0, 0,
					battle.EnemyStatus, battle.EnemyBattleLv,
				)
				body2505 := make([]byte, 0, 164)
				body2505 = append(body2505, playerAv...)
				body2505 = append(body2505, enemyAv...)
				ctx.GameServer.SendResponse(ctx.ClientData, 2505, ctx.UserID, ctx.SeqID, body2505)

				ctx.GameServer.BattleMu.Lock()
				battle.BossRegion = nextRegion
				battle.EnemyID = nextEntry.BossPetID
				battle.EnemyLevel = nextEntry.Level
				battle.EnemyHP = uint32(sptboss.XuanWuGuardianHP)
				battle.EnemyMaxHP = uint32(sptboss.XuanWuGuardianHP)
				battle.EnemyStatus = [20]byte{}
				battle.EnemyBattleLv = [6]int8{}
				ctx.GameServer.BattleMu.Unlock()

				// 推送 2407（对方换宠），格式与 ChangePetInfo 一致：userID(4)+petID(4)+petName(16)+level(4)+hp(4)+maxHp(4)+catchTime(4)=40 字节
				petMgr := gamepets.GetInstance()
				enemyPet := petMgr.Get(nextEntry.BossPetID)
				petName := ""
				if enemyPet != nil {
					petName = enemyPet.DefName
				}
				if len(petName) == 0 {
					petName = "玄武守护兽"
				}
				body2407 := make([]byte, 40)
				binary.BigEndian.PutUint32(body2407[0:4], 0) // 敌方 userID=0（与 2503 一致）
				binary.BigEndian.PutUint32(body2407[4:8], uint32(nextEntry.BossPetID))
				nb := []byte(petName)
				if len(nb) > 16 {
					nb = nb[:16]
				}
				copy(body2407[8:24], nb)
				binary.BigEndian.PutUint32(body2407[24:28], uint32(nextEntry.Level))
				binary.BigEndian.PutUint32(body2407[28:32], uint32(sptboss.XuanWuGuardianHP))
				binary.BigEndian.PutUint32(body2407[32:36], uint32(sptboss.XuanWuGuardianHP))
				binary.BigEndian.PutUint32(body2407[36:40], 0)
				ctx.GameServer.SendResponse(ctx.ClientData, 2407, ctx.UserID, 0, body2407)
				logger.Info(fmt.Sprintf("[2405] 玄武守护兽 param2=%d 击败，换下一只 param2=%d PetID=%d，先 2505 再 2407", nextRegion-1, nextRegion, nextEntry.BossPetID))
				return
			}
		}
		// 青龙空间(403) 守护兽：同一场战斗内对方多精灵，击败当前只后换下一只上场
		if sptboss.IsQingLongGuardianBoss(battle.BattleMapID, battle.BossRegion) && battle.BossRegion < 3 {
			bit := uint32(1) << battle.BossRegion
			user.DefeatedQingLongGuardianMask |= bit
			nextRegion := battle.BossRegion + 1
			nextEntry, ok := sptboss.GetByMapAndParam(sptboss.MapIDQingLongSpace, nextRegion)
			if ok {
				damageFor2505 := damageCalc
				if damageFor2505 > battle.EnemyMaxHP {
					damageFor2505 = battle.EnemyMaxHP
				}
				playerAv := buildAttackValue(
					uint32(ctx.UserID), skillID, 1,
					damageFor2505, 0,
					int32(battle.PlayerHP), battle.PlayerMaxHP,
					0, 0, 0,
					battle.PlayerStatus, battle.PlayerBattleLv,
				)
				enemyAv := buildAttackValue(
					0, 0, 0,
					0, 0,
					0, battle.EnemyMaxHP,
					0, 0, 0,
					battle.EnemyStatus, battle.EnemyBattleLv,
				)
				body2505 := make([]byte, 0, 164)
				body2505 = append(body2505, playerAv...)
				body2505 = append(body2505, enemyAv...)
				ctx.GameServer.SendResponse(ctx.ClientData, 2505, ctx.UserID, ctx.SeqID, body2505)

				ctx.GameServer.BattleMu.Lock()
				battle.BossRegion = nextRegion
				battle.EnemyID = nextEntry.BossPetID
				battle.EnemyLevel = nextEntry.Level
				battle.EnemyHP = uint32(sptboss.QingLongGuardianHP)
				battle.EnemyMaxHP = uint32(sptboss.QingLongGuardianHP)
				battle.EnemyStatus = [20]byte{}
				battle.EnemyBattleLv = [6]int8{}
				ctx.GameServer.BattleMu.Unlock()

				petMgr := gamepets.GetInstance()
				enemyPet := petMgr.Get(nextEntry.BossPetID)
				petName := ""
				if enemyPet != nil {
					petName = enemyPet.DefName
				}
				if len(petName) == 0 {
					petName = "青龙守护兽"
				}
				body2407 := make([]byte, 40)
				binary.BigEndian.PutUint32(body2407[0:4], 0)
				binary.BigEndian.PutUint32(body2407[4:8], uint32(nextEntry.BossPetID))
				nb := []byte(petName)
				if len(nb) > 16 {
					nb = nb[:16]
				}
				copy(body2407[8:24], nb)
				binary.BigEndian.PutUint32(body2407[24:28], uint32(nextEntry.Level))
				binary.BigEndian.PutUint32(body2407[28:32], uint32(sptboss.QingLongGuardianHP))
				binary.BigEndian.PutUint32(body2407[32:36], uint32(sptboss.QingLongGuardianHP))
				binary.BigEndian.PutUint32(body2407[36:40], 0)
				ctx.GameServer.SendResponse(ctx.ClientData, 2407, ctx.UserID, 0, body2407)
				logger.Info(fmt.Sprintf("[2405] 青龙守护兽 param2=%d 击败，换下一只 param2=%d PetID=%d，先 2505 再 2407", nextRegion-1, nextRegion, nextEntry.BossPetID))
				return
			}
		}
		// 勇者之塔（地图 500）多精灵层：击败当前只后换下一只
		if battle.BattleMapID == 500 && len(battle.BraveTowerBossIDs) > 0 && battle.BraveTowerBossIndex+1 < len(battle.BraveTowerBossIDs) {
			nextIdx := battle.BraveTowerBossIndex + 1
			nextBossID := battle.BraveTowerBossIDs[nextIdx]
			damageFor2505 := damageCalc
			if damageFor2505 > battle.EnemyMaxHP {
				damageFor2505 = battle.EnemyMaxHP
			}
			playerAv := buildAttackValue(
				uint32(ctx.UserID), skillID, 1,
				damageFor2505, 0,
				int32(battle.PlayerHP), battle.PlayerMaxHP,
				0, 0, 0,
				battle.PlayerStatus, battle.PlayerBattleLv,
			)
			enemyAv := buildAttackValue(
				0, 0, 0,
				0, 0,
				0, battle.EnemyMaxHP,
				0, 0, 0,
				battle.EnemyStatus, battle.EnemyBattleLv,
			)
			body2505 := make([]byte, 0, 164)
			body2505 = append(body2505, playerAv...)
			body2505 = append(body2505, enemyAv...)
			ctx.GameServer.SendResponse(ctx.ClientData, 2505, ctx.UserID, ctx.SeqID, body2505)

			petMgr := gamepets.GetInstance()
			enemyStats := petMgr.GetStats(nextBossID, battle.EnemyLevel, 15, gamepets.EVStats{}, 0)
			ctx.GameServer.BattleMu.Lock()
			battle.BraveTowerBossIndex = nextIdx
			battle.EnemyID = nextBossID
			battle.EnemyHP = uint32(enemyStats.HP)
			battle.EnemyMaxHP = uint32(enemyStats.MaxHP)
			battle.EnemyStatus = [20]byte{}
			battle.EnemyBattleLv = [6]int8{}
			ctx.GameServer.BattleMu.Unlock()

			enemyPet := petMgr.Get(nextBossID)
			petName := ""
			if enemyPet != nil {
				petName = enemyPet.DefName
			}
			if len(petName) == 0 {
				petName = "勇者之塔守卫"
			}
			body2407 := make([]byte, 40)
			binary.BigEndian.PutUint32(body2407[0:4], 0)
			binary.BigEndian.PutUint32(body2407[4:8], uint32(nextBossID))
			nb := []byte(petName)
			if len(nb) > 16 {
				nb = nb[:16]
			}
			copy(body2407[8:24], nb)
			binary.BigEndian.PutUint32(body2407[24:28], uint32(battle.EnemyLevel))
			binary.BigEndian.PutUint32(body2407[28:32], uint32(enemyStats.HP))
			binary.BigEndian.PutUint32(body2407[32:36], uint32(enemyStats.MaxHP))
			binary.BigEndian.PutUint32(body2407[36:40], 0)
			ctx.GameServer.SendResponse(ctx.ClientData, 2407, ctx.UserID, 0, body2407)
			logger.Info(fmt.Sprintf("[2405] 勇者之塔 Boss[%d] 击败，换下一只 Boss[%d] PetID=%d", nextIdx-1, nextIdx, nextBossID))
			return
		}
			isOver = true
			winnerID = uint32(ctx.UserID)
			logger.Info(fmt.Sprintf("[2405] 战斗结束: 玩家获胜"))
		}
	} else if battle.PlayerHP == 0 {
		// 我方当前出战精灵 HP 为 0：
		// 依据 BattleState 中记录的“本场战斗开始时可用精灵总数”和“已被击败的精灵数量”来判断：
		// - 若所有精灵都已被击败，则直接判定战斗失败；
		// - 否则仍有后备精灵，交给前端弹出“换宠”面板。
		if battle.TotalPlayerPets <= 0 {
			// 兼容旧数据：若未初始化总数，则退回到 len(user.Pets) 判定
			battle.TotalPlayerPets = len(user.Pets)
		}
		// 当前这只精灵刚刚被击败，将其计入 DeadPlayerPets
		battle.DeadPlayerPets++
		// PvP：同步对方视角——我方当前精灵被击败即对方击败了我方当前只，需更新对方 BattleState 的 EnemyHP=0 与 DeadEnemyPets，否则若本回合先处理的是败方 2405，胜方视角的击倒数会少计，导致对方全灭时仍提示换宠
		if battle.OpponentUserID != 0 {
			if ob := ctx.GameServer.BattleStates[battle.OpponentUserID]; ob != nil {
				ob.EnemyHP = 0
				ob.DeadEnemyPets++
				if ob.TotalEnemyPets <= 0 {
					ob.TotalEnemyPets = 1
				}
			}
		}
		remaining := battle.TotalPlayerPets - battle.DeadPlayerPets
		if remaining <= 0 {
			isOver = true
			if battle.OpponentUserID != 0 {
				winnerID = uint32(battle.OpponentUserID) // PvP：对方获胜，客户端用 winnerID==MainManager.actorID 判断胜负
			} else {
				winnerID = 0 // PvE：敌人获胜
			}
			logger.Info(fmt.Sprintf("[2405] 战斗结束: 敌人获胜（无可用后备精灵）winnerID=%d", winnerID))
		} else {
			logger.Info(fmt.Sprintf("[2405] 当前精灵被击败，但玩家还有其它精灵可用，等待 2407 切换精灵 (Total=%d Dead=%d Remaining=%d)",
				battle.TotalPlayerPets, battle.DeadPlayerPets, remaining))
		}
	}

	if isOver {
		// 如果玩家获胜，给予经验奖励
		if winnerID == uint32(ctx.UserID) && len(user.Pets) > 0 {
			// 雷伊体能特训：2411 param2=10000+index（存于 battle.BossRegion）
			if battle.BossRegion >= 10000 && battle.BossRegion <= 10005 {
				if applyLeiyiTrainReward(ctx, user, battle) && ctx.GameServer.UserDB != nil {
					ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
				}
			}
			// 试炼之塔（地图 600）：战胜当层后更新当前层与最高层，并发放层奖励；立即持久化
			if battle.BattleMapID == 600 {
				user.CurFreshStage++
				if user.CurFreshStage > freshTowerMaxLevel {
					user.CurFreshStage = freshTowerMaxLevel
				}
				user.MaxFreshStage++
				if user.MaxFreshStage > freshTowerMaxLevel {
					user.MaxFreshStage = freshTowerMaxLevel
				}
				clearedLayer := user.CurFreshStage - 1
				if clearedLayer < 1 {
					clearedLayer = 1
				}
				// 每层除经验外奖励仅可领取一次，可重复打试炼之塔
				rewardAlreadyClaimed := false
				for _, L := range user.FreshTowerRewardClaimed {
					if L == clearedLayer {
						rewardAlreadyClaimed = true
						break
					}
				}
				if !rewardAlreadyClaimed {
					if user.Items == nil {
						user.Items = make(map[string]userdb.Item)
					}
					addTowerItem := func(itemID string, count int) {
						if ex, ok := user.Items[itemID]; ok {
							ex.Count += count
							user.Items[itemID] = ex
						} else {
							user.Items[itemID] = userdb.Item{Count: count, ExpireTime: 0x057E40}
						}
					}
					if clearedLayer <= 10 {
						addTowerItem("300011", 10)
						addTowerItem("300016", 10)
						addTowerItem("300001", 10)
					} else if clearedLayer <= 20 {
						addTowerItem("300012", 10)
						addTowerItem("300017", 10)
						addTowerItem("300002", 10)
					} else {
						addTowerItem("300013", 10)
						addTowerItem("300018", 10)
						addTowerItem("300003", 10)
					}
					user.Coins += 1000
					if clearedLayer == 10 || clearedLayer == 20 || clearedLayer == 30 {
						user.Gold += 100
					}
					// 推送 8004 触发客户端奖励弹窗（BossCmdListener：itemID 1=赛尔豆，其余为物品）
					ctx.GameServer.SendResponse(ctx.ClientData, 8004, ctx.UserID, 0, buildBossMonster8004Body(0, 0, 0, 1, 1000))
					var hpID, ppID, capID uint32
					if clearedLayer <= 10 {
						hpID, ppID, capID = 300011, 300016, 300001
					} else if clearedLayer <= 20 {
						hpID, ppID, capID = 300012, 300017, 300002
					} else {
						hpID, ppID, capID = 300013, 300018, 300003
					}
					ctx.GameServer.SendResponse(ctx.ClientData, 8004, ctx.UserID, 0, buildBossMonster8004Body(0, 0, 0, hpID, 10))
					ctx.GameServer.SendResponse(ctx.ClientData, 8004, ctx.UserID, 0, buildBossMonster8004Body(0, 0, 0, ppID, 10))
					ctx.GameServer.SendResponse(ctx.ClientData, 8004, ctx.UserID, 0, buildBossMonster8004Body(0, 0, 0, capID, 10))
					if clearedLayer == 10 || clearedLayer == 20 || clearedLayer == 30 {
						// 金豆弹窗：客户端 items.xml 中金豆 Item ID=5
						ctx.GameServer.SendResponse(ctx.ClientData, 8004, ctx.UserID, 0, buildBossMonster8004Body(0, 0, 0, 5, 100))
					}
					user.FreshTowerRewardClaimed = append(user.FreshTowerRewardClaimed, clearedLayer)
				}
				if ctx.GameServer.UserDB != nil {
					ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
				}
			} else if battle.BattleMapID == 500 {
				// 勇者之塔（地图 500）：经验每层层数*200，除经验外所有奖励仅首次通关获得
				user.CurStage++
				if user.CurStage < 1 {
					user.CurStage = 1
				}
				if user.CurStage > braveTowerMaxLevel {
					user.CurStage = braveTowerMaxLevel
				}
				if user.CurStage > user.MaxStage {
					user.MaxStage = user.CurStage
				}
				clearedLayer := user.CurStage - 1
				if clearedLayer < 1 {
					clearedLayer = 1
				}
				rewardAlreadyClaimed := false
				for _, L := range user.BraveTowerRewardClaimed {
					if L == clearedLayer {
						rewardAlreadyClaimed = true
						break
					}
				}
				if !rewardAlreadyClaimed {
					if user.Items == nil {
						user.Items = make(map[string]userdb.Item)
					}
					addBTItem := func(itemID string, count int) {
						if ex, ok := user.Items[itemID]; ok {
							ex.Count += count
							user.Items[itemID] = ex
						} else {
							user.Items[itemID] = userdb.Item{Count: count, ExpireTime: 0x057E40}
						}
					}
					var coins int
					var capID, potionID string
					switch {
					case clearedLayer <= 10:
						coins, capID, potionID = 1000, "300001", "300014" // 初级胶囊, 超级体力药剂
					case clearedLayer <= 20:
						coins, capID, potionID = 2000, "300002", "300018" // 中级胶囊, 高级活力药剂
					case clearedLayer <= 30:
						coins, capID, potionID = 3000, "300003", "300014" // 高级胶囊, 超级体力药剂
					case clearedLayer <= 40:
						coins, capID, potionID = 4000, "300004", "300014" // 超级精灵胶囊, 超级体力药剂
					case clearedLayer <= 50:
						coins, capID, potionID = 5000, "300004", "300019" // 超级精灵胶囊, 超级活力药剂
					case clearedLayer <= 60:
						coins, capID, potionID = 6000, "300004", "300019"
					case clearedLayer <= 70:
						coins, capID, potionID = 8000, "300004", "" // 61-70 无药剂
					default:
						coins, capID, potionID = 7000, "300004", "300019" // 71-80 超级活力药剂
					}
					user.Coins += coins
					ctx.GameServer.SendResponse(ctx.ClientData, 8004, ctx.UserID, 0, buildBossMonster8004Body(0, 0, 0, 1, uint32(coins)))
					capCnt := 5
					if clearedLayer >= 31 {
						capCnt = 3
					}
					addBTItem(capID, capCnt)
					if capN, err := strconv.ParseUint(capID, 10, 32); err == nil {
						ctx.GameServer.SendResponse(ctx.ClientData, 8004, ctx.UserID, 0, buildBossMonster8004Body(0, 0, 0, uint32(capN), uint32(capCnt)))
					}
					if potionID != "" {
						addBTItem(potionID, 5)
						if pn, err := strconv.ParseUint(potionID, 10, 32); err == nil {
							ctx.GameServer.SendResponse(ctx.ClientData, 8004, ctx.UserID, 0, buildBossMonster8004Body(0, 0, 0, uint32(pn), 5))
						}
					}
					if clearedLayer == 10 || clearedLayer == 20 || clearedLayer == 30 || clearedLayer == 40 || clearedLayer == 50 || clearedLayer == 60 || clearedLayer == 70 || clearedLayer == 80 {
						user.Gold += 50
						ctx.GameServer.SendResponse(ctx.ClientData, 8004, ctx.UserID, 0, buildBossMonster8004Body(0, 0, 0, 5, 50))
					}
					if clearedLayer == 30 {
						for _, sid := range []string{"100096", "100097", "100098", "100099"} {
							addBTItem(sid, 1)
							if sn, err := strconv.ParseUint(sid, 10, 32); err == nil {
								ctx.GameServer.SendResponse(ctx.ClientData, 8004, ctx.UserID, 0, buildBossMonster8004Body(0, 0, 0, uint32(sn), 1))
							}
						}
					}
					if clearedLayer == 60 {
						newCatchTime := int(time.Now().Unix())
						rand.Seed(time.Now().UnixNano() + int64(newCatchTime))
						newPet := userdb.Pet{
							ID:        141,
							CatchTime: newCatchTime,
							Level:     1,
							Exp:       0,
							Nature:    0,
							DV:        0,
						}
						if len(user.Pets) < 6 {
							user.Pets = append(user.Pets, newPet)
						} else {
							if user.StoragePets == nil {
								user.StoragePets = []userdb.Pet{}
							}
							user.StoragePets = append(user.StoragePets, newPet)
						}
						ctx.GameServer.SendResponse(ctx.ClientData, 8004, ctx.UserID, 0, buildBossMonster8004Body(0, 141, uint32(newCatchTime), 0, 0))
						logger.Info(fmt.Sprintf("[2405] 勇者之塔第60层奖励精灵肯 PetID=141 CatchTime=%d", newCatchTime))
					}
					user.BraveTowerRewardClaimed = append(user.BraveTowerRewardClaimed, clearedLayer)
				}
				if ctx.GameServer.UserDB != nil {
					ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
				}
			}
			petMgr := gamepets.GetInstance()
			enemyPet := petMgr.Get(battle.EnemyID)
			// 给予经验（试炼之塔第 N 层为 N*1000，勇者之塔第 N 层为 N*200，否则从敌人配置或默认 50）
			expGain := 50
			if battle.BattleMapID == 600 {
				cl := user.CurFreshStage - 1
				if cl < 1 {
					cl = 1
				}
				expGain = cl * 1000
			} else if battle.BattleMapID == 500 {
				cl := user.CurStage - 1
				if cl < 1 {
					cl = 1
				}
				expGain = cl * 200
				user.ExpPool += expGain
				if user.ExpPool < 0 {
					user.ExpPool = 0
				}
			} else if enemyPet != nil && enemyPet.YieldingExp > 0 {
				expGain = enemyPet.YieldingExp
			}

			// 野外精灵额外材料掉落：70% 概率掉落 1~2 个材料，并通过 8004 弹窗提示
			// 仅在地图中的野生精灵战斗（IsWildMonster）中触发，试炼之塔 600、勇者之塔 500、PvP 等不掉落
			if enemyPet != nil && battle.IsWildMonster && battle.BattleMapID != 600 && battle.BattleMapID != 500 && ctx.GameServer.UserDB != nil {
				dropMap := map[int]int{
					30:  400006, // 贝尔 -> 水之精华
					33:  400029, // 利牙鱼 -> 海洋能量
					10:  400004, // 皮皮 -> 空气结晶
					22:  400004, // 毛毛 -> 空气结晶
					25:  400023, // 幽浮 -> 纯净能源（若实际 ID 不同，可按 items.xml 调整）
					249: 400023, // 浮空苗 -> 纯净能源
					13:  400008, // 比比鼠 -> 电容球
					43:  400008, // 罗奇 -> 电容球
					16:  400005, // 仙人球 -> 光合能量
					27:  400005, // 小豆芽 -> 光合能量
					51:  400003, // 玄冰兽 -> 玄冰
					52:  400003, // 急冻兽 -> 玄冰
					38:  400007, // 火炎贝 -> 火焰元素
					35:  400030, // 吉尔 -> 熔岩晶体
					318: 400030, // 晶岩兽 -> 熔岩晶体
					208: 400027, // 火晶兽 -> 露希之核
					210: 400027, // 多鲁姆 -> 露希之核
					105: 400031, // 丁格 -> 大地之核
					106: 400031, // 丁加鲁 -> 大地之核
					53:  400031, // 莫比 -> 大地之核
					119: 400032, // 莱尼 -> 贝壳精华
					120: 400032, // 特鲁尼 -> 贝壳精华
					121: 400032, // 伊卡罗尼 -> 贝壳精华
					205: 400033, // 阿兹 -> 晶化气泡
					206: 400033, // 科利 -> 晶化气泡
					211: 400034, // 水草蛙 -> 水生海草
					198: 400029, // 小鳍鱼 -> 海洋能量
				}
				if itemID, ok := dropMap[battle.EnemyID]; ok {
					r := rand.New(rand.NewSource(time.Now().UnixNano()))
					if r.Intn(100) < 70 { // 70% 概率掉落
						cnt := 1 + r.Intn(2) // 1~2 个
						ctx.GameServer.UserDB.AddItem(ctx.UserID, itemID, cnt)
						// 通过 8004 触发客户端「获得物品」弹窗
						ctx.GameServer.SendResponse(ctx.ClientData, 8004, ctx.UserID, 0, buildBossMonster8004Body(0, 0, 0, uint32(itemID), uint32(cnt)))
						logger.Info(fmt.Sprintf("[2405] 野外战斗掉落材料: EnemyID=%d ItemID=%d Count=%d", battle.EnemyID, itemID, cnt))
					}
				}
			}
			active := &user.Pets[0]
			if battle.BattleMapID == 500 && battle.ActivePetIndex >= 0 && battle.ActivePetIndex < len(user.Pets) {
				active = &user.Pets[battle.ActivePetIndex]
			}

			// Pet.Exp 语义：当前等级已获得经验（不是总经验）
			// 这里复用 2318（经验池分配）里的自动升级逻辑，避免经验始终为 0 的问题
			if active.Level <= 0 {
				active.Level = 1
			}
			if active.Level > 100 {
				active.Level = 100
				active.Exp = 0
			}

			oldLevel := active.Level
			oldExp := active.Exp

			remain := expGain
			for remain > 0 && active.Level < 100 {
				expInfo := petMgr.GetExpInfo(active.ID, active.Level, active.Exp)

				// 防御：如果 nextLevelExp 计算异常为 0，避免死循环，直接把剩余经验累加到当前等级
				if expInfo.NextLevelExp <= 0 {
					active.Exp += remain
					remain = 0
					break
				}

				need := expInfo.NextLevelExp - active.Exp
				if need > remain {
					// 本级别升不了级，只增加当前等级经验
					active.Exp += remain
					remain = 0
				} else {
					// 升级：扣除当前等级所需经验，等级+1，当前等级经验清零
					active.Exp = 0
					active.Level++
					remain -= need
					if active.Level >= 100 {
						active.Level = 100
						active.Exp = 0
						break
					}
				}
			}

			// 检查是否可以进化（只处理“直接进化”场景：配置里有 EvolvesTo，且不需要道具/进化舱）
			canEvolve, _, evolveTo := petMgr.CanEvolve(active.ID, active.Level, false)
			if canEvolve && evolveTo > 0 {
				logger.Info(fmt.Sprintf("[2405] 精灵进化触发: PetID %d -> %d (Level=%d)", active.ID, evolveTo, active.Level))
				active.ID = evolveTo
				// 进化后当前等级经验清零，重新开始积累
				active.Exp = 0
			}

			logger.Info(fmt.Sprintf(
				"[2405] 战斗胜利: 获得经验 %d (Level %d->%d, Exp %d->%d, PetID=%d)",
				expGain, oldLevel, active.Level, oldExp, active.Exp, active.ID,
			))

			// 击败野生/非 BOSS 精灵时，给当前获得经验的出战精灵增加学习力（YieldingEV）
			// 条件：敌方在配置中有 YieldingEV，且（本场为野生战斗 或 敌方不是 SPT BOSS）
			// 若出战精灵学习力总和已满 510 则不再增加；增加时只加不减，总和不超过 510
			_, isSPTBoss := sptboss.GetByPetID(battle.EnemyID)
			isWildOrNonBoss := battle.IsWildMonster || !isSPTBoss
			if isWildOrNonBoss && enemyPet != nil && enemyPet.YieldingEV != "" {
				yieldEV := petMgr.ParseYieldingEV(enemyPet.YieldingEV)
				if yieldEV.HP != 0 || yieldEV.Atk != 0 || yieldEV.Def != 0 || yieldEV.SpAtk != 0 || yieldEV.SpDef != 0 || yieldEV.Spd != 0 {
					curEV := active.GetEVStats()
					if curEV.TotalEV() < 510 {
						ev := gamepets.AddEVWithCap(curEV, yieldEV)
						active.EVHP = ev.HP
						active.EVAttack = ev.Atk
						active.EVDefence = ev.Def
						active.EVSpAtk = ev.SpAtk
						active.EVSpDef = ev.SpDef
						active.EVSpeed = ev.Spd
						logger.Info(fmt.Sprintf("[2405] 野生精灵击败学习力加成: EnemyID=%d 出战精灵 PetID=%d EV +%v -> HP=%d Atk=%d Def=%d SpA=%d SpD=%d Spd=%d",
							battle.EnemyID, active.ID, yieldEV, active.EVHP, active.EVAttack, active.EVDefence, active.EVSpAtk, active.EVSpDef, active.EVSpeed))
					}
				}
			}

			// 玄武空间(401) 击败守护兽(param2=0~5)：标记对应 bit，六守护兽无精元
			if sptboss.IsXuanWuGuardianBoss(battle.BattleMapID, battle.BossRegion) {
				bit := uint32(1) << battle.BossRegion
				user.DefeatedXuanWuGuardianMask |= bit
				logger.Info(fmt.Sprintf("[2405] 玄武守护兽 param2=%d 击败，mask=%d UID=%d", battle.BossRegion, user.DefeatedXuanWuGuardianMask, ctx.UserID))
			}
			// 青龙空间(403) 击败守护兽(param2=0~3)：标记对应 bit，四守护兽无精元
			if sptboss.IsQingLongGuardianBoss(battle.BattleMapID, battle.BossRegion) {
				bit := uint32(1) << battle.BossRegion
				user.DefeatedQingLongGuardianMask |= bit
				logger.Info(fmt.Sprintf("[2405] 青龙守护兽 param2=%d 击败，mask=%d UID=%d", battle.BossRegion, user.DefeatedQingLongGuardianMask, ctx.UserID))
			}

			// ==================== SPT BOSS 奖励与成就记录 ====================
			// 仅在 PvE 对战中触发：若为 PvP（OpponentUserID != 0），敌方是玩家携带的普通精灵，
			// 即使种族与 SPT BOSS 相同也不得发放任何精元/精灵或记录 SPT 击败成就。
			if battle.OpponentUserID != 0 {
				logger.Info(fmt.Sprintf("[2405] PvP 对战击败玩家精灵 PetID=%d，跳过 SPT BOSS 精元/精灵奖励与成就记录", battle.EnemyID))
			} else if entry, ok := sptboss.GetByPetID(battle.EnemyID); ok && entry.RewardPetID > 0 {
				// 首次击败 SPT BOSS 奖励：按 sptboss 配置发放对应精灵（如蘑菇怪→小蘑菇、里奥斯→胡里亚等）
				alreadyDefeated := false
				for _, id := range user.DefeatedSPTBossIds {
					if id == battle.EnemyID {
						alreadyDefeated = true
						break
					}
				}
				if !alreadyDefeated {
					user.DefeatedSPTBossIds = append(user.DefeatedSPTBossIds, battle.EnemyID)
					// 兼容老玄武/青龙空间：首次击败对应 SPT BOSS 时，自动补齐相关任务完成状态
					autoCompleteXuanWuTasksIfNeeded(user, battle.EnemyID)
					autoCompleteQingLongTasksIfNeeded(user, battle.EnemyID)
					newCatchTime := int(time.Now().Unix())
					rand.Seed(time.Now().UnixNano() + int64(newCatchTime))
					newPet := userdb.Pet{
						ID:        entry.RewardPetID,
						CatchTime: newCatchTime,
						Level: func() int {
							if entry.RewardPetID == 166 {
								return 80
							}
							return 1
						}(),
						DV: func() int {
							if entry.RewardPetID == 166 {
								return 31
							}
							return rand.Intn(32)
						}(),
						Nature: rand.Intn(25),
						Exp:    0,
						Name:   "",
						Shiny:  0, // 默认 SPT 奖励精灵为普通色，异色另行配置/活动发放
					}
					// SPT 奖励精灵不自动分配特性；后续可通过“特性开启芯片”等道具获得特性
					// 若背包已满 6 只，则自动放入仓库，避免新增精灵丢失
					if len(user.Pets) >= 6 {
						if user.StoragePets == nil {
							user.StoragePets = []userdb.Pet{}
						}
						user.StoragePets = append(user.StoragePets, newPet)
					} else {
						user.Pets = append(user.Pets, newPet)
					}
					if ctx.GameServer.UserDB != nil {
						ctx.GameServer.UserDB.RecordCatch(ctx.UserID, entry.RewardPetID)
					}
					logger.Info(fmt.Sprintf("[2405] 首次击败 SPT BOSS PetID=%d，奖励精灵 PetID=%d CatchTime=%d", battle.EnemyID, entry.RewardPetID, newCatchTime))
					// 推送 CMD 8004 通知客户端弹窗显示获得精灵
					body8004 := buildBossMonster8004Body(0, uint32(entry.RewardPetID), uint32(newCatchTime), 0, 0)
					ctx.GameServer.SendResponse(ctx.ClientData, 8004, ctx.UserID, 0, body8004)
				}
			} else if ((entry.RewardItemID > 0 && battle.EnemyID != petIDGaiya) || battle.EnemyID == sptboss.PetIDPuni) &&
				!sptboss.IsXuanWuGuardianBoss(battle.BattleMapID, battle.BossRegion) &&
				!sptboss.IsQingLongGuardianBoss(battle.BattleMapID, battle.BossRegion) {
				// 精元奖励（雷伊、纳多雷等）；盖亚(261)由 pushGaiyaRewardOrNotice 单独处理
				// 谱尼(300)特殊处理：每个封印/真身给对应碎片，集齐8个碎片后自动合成精元
				// 玄武六守护兽(401/param2 0~5)不给精元，仅真身(401/param2=6)501 给
				isPuni := battle.EnemyID == sptboss.PetIDPuni
				
				if isPuni {
					// 谱尼碎片系统：每个封印/真身给对应的碎片
					fragmentItemID := sptboss.GetPuniFragmentItemID(battle.BossRegion)
					if fragmentItemID > 0 {
						// 检查是否已获得过该碎片（通过检查DefeatedSPTBossIds中是否有对应的记录）
						// 使用特殊标记：30000 + region 来区分不同的封印/真身
						sealKey := 30000 + int(battle.BossRegion)
						alreadyDefeated := false
						for _, id := range user.DefeatedSPTBossIds {
							if id == sealKey {
								alreadyDefeated = true
								break
							}
						}
						
						if !alreadyDefeated {
							// 记录该封印/真身已击败
							user.DefeatedSPTBossIds = append(user.DefeatedSPTBossIds, sealKey)
							
							if user.Items == nil {
								user.Items = make(map[string]userdb.Item)
							}
							
							// 发放碎片
							fragmentKey := strconv.Itoa(fragmentItemID)
							if it, has := user.Items[fragmentKey]; has {
								it.Count++
								user.Items[fragmentKey] = it
							} else {
								user.Items[fragmentKey] = userdb.Item{Count: 1, ExpireTime: 0x057E40}
							}
							
							logger.Info(fmt.Sprintf("[2405] 首次击败谱尼 BossRegion=%d，奖励碎片 ItemID=%d", battle.BossRegion, fragmentItemID))
							
							// 推送 CMD 8004 通知客户端弹窗显示获得碎片
							body8004 := buildBossMonster8004Body(0, 0, 0, uint32(fragmentItemID), 1)
							ctx.GameServer.SendResponse(ctx.ClientData, 8004, ctx.UserID, 0, body8004)
							
							// 检查是否集齐8个碎片（400651-400658）
							allFragmentsCollected := true
							for region := uint32(1); region <= 8; region++ {
								fragID := sptboss.GetPuniFragmentItemID(region)
								if fragID > 0 {
									fragKey := strconv.Itoa(fragID)
									if it, has := user.Items[fragKey]; !has || it.Count < 1 {
										allFragmentsCollected = false
										break
									}
								}
							}
							
							// 集齐8个碎片，自动合成精元
							if allFragmentsCollected {
								essenceKey := strconv.Itoa(sptboss.PuniEssenceItemID)
								// 检查是否已有精元（避免重复合成）
								if _, hasEssence := user.Items[essenceKey]; !hasEssence {
									// 消耗8个碎片
									for region := uint32(1); region <= 8; region++ {
										fragID := sptboss.GetPuniFragmentItemID(region)
										if fragID > 0 {
											fragKey := strconv.Itoa(fragID)
											if it, has := user.Items[fragKey]; has && it.Count > 0 {
												it.Count--
												if it.Count <= 0 {
													delete(user.Items, fragKey)
												} else {
													user.Items[fragKey] = it
												}
											}
										}
									}
									
									// 发放谱尼精元
									user.Items[essenceKey] = userdb.Item{Count: 1, ExpireTime: 0x057E40}
									
									logger.Info(fmt.Sprintf("[2405] 集齐8个谱尼碎片，自动合成精元 ItemID=%d", sptboss.PuniEssenceItemID))
									
									// 推送 CMD 8004 通知客户端弹窗显示获得精元（触发合成动画）
									body8004Essence := buildBossMonster8004Body(0, 0, 0, uint32(sptboss.PuniEssenceItemID), 1)
									ctx.GameServer.SendResponse(ctx.ClientData, 8004, ctx.UserID, 0, body8004Essence)
								}
							}
						}
					}
				} else {
					// 其他BOSS的精元奖励逻辑（非谱尼）
					alreadyDefeated := false
					for _, id := range user.DefeatedSPTBossIds {
						if id == battle.EnemyID {
							alreadyDefeated = true
							break
						}
					}
					
					if !alreadyDefeated {
						user.DefeatedSPTBossIds = append(user.DefeatedSPTBossIds, battle.EnemyID)
						// 兼容老玄武/青龙空间任务线
						autoCompleteXuanWuTasksIfNeeded(user, battle.EnemyID)
						autoCompleteQingLongTasksIfNeeded(user, battle.EnemyID)
						if user.Items == nil {
							user.Items = make(map[string]userdb.Item)
						}
						itemKey := strconv.Itoa(entry.RewardItemID)
						if it, has := user.Items[itemKey]; has {
							it.Count++
							user.Items[itemKey] = it
						} else {
							user.Items[itemKey] = userdb.Item{Count: 1, ExpireTime: 0x057E40}
						}
						logger.Info(fmt.Sprintf("[2405] 首次击败 SPT BOSS PetID=%d，奖励精元 ItemID=%d", battle.EnemyID, entry.RewardItemID))
						// 推送 CMD 8004 通知客户端弹窗显示获得精元
						body8004 := buildBossMonster8004Body(0, 0, 0, uint32(entry.RewardItemID), 1)
						ctx.GameServer.SendResponse(ctx.ClientData, 8004, ctx.UserID, 0, body8004)
					}
				}
			} else if battle.BattleMapID == sptboss.MapIDYiNengWang && battle.EnemyID == 1000 {
				// 地图 677 异能王：击败六重试炼(region 0~5)或终极试炼(region 6)后完成任务并发放对应之魂/精元（首次）
				region := battle.BossRegion
				var taskID int
				var itemID int
				if sptboss.IsYiNengSealBoss(battle.BattleMapID, region) {
					taskID = sptboss.YiNengSealTaskID(region)
					itemID = sptboss.YiNengSealItemID(region)
				} else if sptboss.IsYiNengUltimateBoss(battle.BattleMapID, region) {
					taskID = sptboss.YiNengUltimateTaskID
					itemID = sptboss.YiNengUltimateItemID
				} else {
					taskID = 0
					itemID = 0
				}
				if taskID != 0 && itemID != 0 {
					taskKey := strconv.Itoa(taskID)
					wasComplete := false
					if user.Tasks != nil {
						if t, ok := user.Tasks[taskKey]; ok {
							wasComplete = t.Status == "3"
						}
					}
					completeTaskIfNeeded(user, taskID)
					if !wasComplete {
						if user.Items == nil {
							user.Items = make(map[string]userdb.Item)
						}
						itemKey := strconv.Itoa(itemID)
						if it, has := user.Items[itemKey]; has {
							it.Count++
							user.Items[itemKey] = it
						} else {
							user.Items[itemKey] = userdb.Item{Count: 1, ExpireTime: 0x057E40}
						}
						body8004 := buildBossMonster8004Body(0, 0, 0, uint32(itemID), 1)
						ctx.GameServer.SendResponse(ctx.ClientData, 8004, ctx.UserID, 0, body8004)
						// 推送 2202 任务完成，客户端 TasksManager 更新后面板才会显示「挑战真身」按钮
						body2202 := buildTaskCompleteResponse(uint32(taskID), 0, user)
						ctx.GameServer.SendResponse(ctx.ClientData, 2202, ctx.UserID, 0, body2202)
						logger.Info(fmt.Sprintf("[2405] 地图677 击败 region=%d，完成任务 %d，发放道具 %d，推送 2202", region, taskID, itemID))
					}
				}
			} else if _, ok := sptboss.GetByPetID(battle.EnemyID); ok &&
				!sptboss.IsXuanWuGuardianBoss(battle.BattleMapID, battle.BossRegion) &&
				!sptboss.IsQingLongGuardianBoss(battle.BattleMapID, battle.BossRegion) {
				// SPT BOSS 无奖励精灵/精元，仅记录击败成就
				// 玄武六守护兽(401/0~5)不记录到 DefeatedSPTBossIds，避免影响其他地图同精灵首次奖励
				alreadyDefeated := false
				for _, id := range user.DefeatedSPTBossIds {
					if id == battle.EnemyID {
						alreadyDefeated = true
						break
					}
				}
				if !alreadyDefeated {
					user.DefeatedSPTBossIds = append(user.DefeatedSPTBossIds, battle.EnemyID)
					// 兼容老玄武/青龙空间任务线
					autoCompleteXuanWuTasksIfNeeded(user, battle.EnemyID)
					autoCompleteQingLongTasksIfNeeded(user, battle.EnemyID)
				}
			}

			// 保存数据
			if ctx.GameServer.UserDB != nil {
				ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
			}

			// 发送 NOTE_UPDATE_PROP(2508)，驱动“战斗结束属性结算面板”
			// 使用本场战斗最后的出战精灵（ActivePetIndex），而不是固定 user.Pets[0]，
			// 避免出现“场上盖亚，但结算面板显示背包第一只巴鲁斯”的错位。
			// 优先根据 BattleState.ActivePetCatchTime 精确匹配当前出战精灵，
			// 若未设置则回退到 ActivePetIndex，再不行才使用下标 0。
			// 精灵大乱斗：禁止推送 2508/2301 等“背包精灵信息包”，否则客户端会把临时精灵当成背包新增精灵缓存
			if battle != nil && battle.IsGrandMelee {
				// 仍继续走 2506 结束战斗与经验弹窗逻辑
			} else {
			var activePet *userdb.Pet
			if battle != nil && battle.ActivePetCatchTime != 0 {
				for i := range user.Pets {
					if user.Pets[i].CatchTime == battle.ActivePetCatchTime {
						activePet = &user.Pets[i]
						break
					}
				}
			}
			if activePet == nil {
				activeIdx := 0
				if battle != nil && battle.ActivePetIndex >= 0 && battle.ActivePetIndex < len(user.Pets) {
					activeIdx = battle.ActivePetIndex
				}
				if activeIdx >= 0 && activeIdx < len(user.Pets) {
					activePet = &user.Pets[activeIdx]
				}
			}
			if activePet == nil && len(user.Pets) > 0 {
				activePet = &user.Pets[0]
			}
			if activePet == nil {
				// 无可用精灵，跳过 2508
				return
			}
			ev := gamepets.ClampAndCapEV(activePet.GetEVStats())
			stats := petMgr.GetStats(activePet.ID, activePet.Level, activePet.DV, ev, activePet.Nature)
			stats = applyPetStatBonus(stats, activePet)
			// 这里的 currentLevelExp 直接使用 Pet.Exp（“当前等级已获得经验”），
			// 前端会用 NOTE_UPDATE_PROP 与之前的 PetInfo 做差得到“获得经验”数值
			propBody := buildNoteUpdateProp(uint32(activePet.CatchTime), activePet.ID, activePet.Level, activePet.Exp,
				stats.MaxHP, stats.Attack, stats.Defence, stats.SpAtk, stats.SpDef, stats.Speed, ev)
			ctx.GameServer.SendResponse(ctx.ClientData, 2508, ctx.UserID, ctx.SeqID, propBody)
			}
		}

		overBody := buildFightOverInfo(0, winnerID)
		// 雷伊技能特训：满足条件时推送 2510 触发客户端学习面板
		if winnerID == uint32(ctx.UserID) {
			pushLearnSpecialSkillNoticeIfNeeded(ctx, user, battle)
		}
		// PvP 胜方：先 sync 写回 HP，再在 2506 前推送 2301 同步我方当前精灵，避免击杀对方最后一只时我方血条误显示回满
		winnerBattlePets := getBattlePets(ctx.GameServer, ctx.UserID)
		if winnerID == uint32(ctx.UserID) && battle.OpponentUserID != 0 && len(winnerBattlePets) > 0 && !battle.IsGrandMelee {
			syncBattleHPToUser(ctx.GameServer, ctx.UserID, battle)
			activeIdx := 0
			if battle.ActivePetIndex >= 0 && battle.ActivePetIndex < len(winnerBattlePets) {
				activeIdx = battle.ActivePetIndex
			}
			petInfoBody := buildFullPetInfo(winnerBattlePets[activeIdx])
			ctx.GameServer.SendResponse(ctx.ClientData, 2301, ctx.UserID, 0, petInfoBody)
		}
		ctx.GameServer.SendResponse(ctx.ClientData, 2506, ctx.UserID, ctx.SeqID, overBody)
		pushMapOgreListAfterFightOver(ctx)
		// 玩家战胜特定 BOSS 时，根据战斗数据解锁盖亚魂印效果
		if winnerID == uint32(ctx.UserID) {
			// 精灵王之战胜场统计：仅在 PvP 且标记为精灵王之战时累计胜场
			if battle != nil && battle.OpponentUserID != 0 && battle.IsPetKing {
				if u := ctx.GameServer.GetOrCreateUser(ctx.UserID); u != nil {
					u.MonKingWin++
					if ctx.GameServer.UserDB != nil {
						ctx.GameServer.UserDB.SaveGameData(ctx.UserID, u)
					}
				}
			}
			unlockGaiyaEffectIfNeeded(ctx, battle, user)
		}
		// 盖亚（261）在三地图按周几+条件才给精元（400126）；否则推送 SPRINT_GIFT_NOTICE(8010) 提示未按规则
		if battle.EnemyID == 261 && winnerID == uint32(ctx.UserID) {
			pushGaiyaRewardOrNotice(ctx, battle, user)
		}
		// 盖亚战结束后：若当前在当日盖亚地图，重发 2022 使盖亚仍显示在地图中，无需重新进图
		if battle.EnemyID == petIDGaiya {
			gaiyaMap := getGaiyaMapIDForToday()
			if user.MapID != 0 && user.MapID == gaiyaMap {
				gaiyaNote := make([]byte, 8)
				binary.BigEndian.PutUint32(gaiyaNote[0:4], 1)
				binary.BigEndian.PutUint32(gaiyaNote[4:8], petIDGaiya)
				ctx.GameServer.SendResponse(ctx.ClientData, cmdSpecialPetNote, ctx.UserID, 0, gaiyaNote)
				logger.Info(fmt.Sprintf("[2022] 盖亚战后重推盖亚出场: UID=%d MapID=%d", ctx.UserID, user.MapID))
			}
		}
		// PvP：向对方也发送 2506 并清理状态，否则对方对战不退出；战毕将当前精灵 HP 写回，不自动回满
		opponentUID := battle.OpponentUserID
		// sync 后、delete 前构建：
		// - 普通 PvP：供败方客户端 2301 显示 0 血
		// - 精灵大乱斗：禁止推送 2301/2508 等“背包精灵信息包”，否则客户端会把临时精灵当成背包新增精灵缓存
		var loserPetInfoBody []byte
		ctx.GameServer.BattleMu.Lock()
		syncBattleHPToUser(ctx.GameServer, ctx.UserID, battle)
		if opponentUID != 0 {
			if ob := ctx.GameServer.BattleStates[opponentUID]; ob != nil {
				syncBattleHPToUser(ctx.GameServer, opponentUID, ob)
				oppPets := getBattlePets(ctx.GameServer, opponentUID)
				activeIdx := ob.ActivePetIndex
				if activeIdx < 0 {
					activeIdx = 0
				}
				if !battle.IsGrandMelee && activeIdx < len(oppPets) {
					loserPetInfoBody = buildFullPetInfo(oppPets[activeIdx])
				}
			}
			delete(ctx.GameServer.BattleStates, opponentUID)
		}
		delete(ctx.GameServer.BattleStates, ctx.UserID)
		ctx.GameServer.BattleMu.Unlock()
		// 精灵大乱斗：胜方 10000、败方 5000 可分配经验并弹窗，清理临时精灵列表
		if battle.IsGrandMelee && winnerID != 0 {
			winnerUID := int64(winnerID)
			var loserUID int64
			if winnerID == uint32(ctx.UserID) {
				loserUID = opponentUID
			} else {
				loserUID = ctx.UserID
			}
			if u := ctx.GameServer.GetOrCreateUser(winnerUID); u != nil {
				u.ExpPool += grandMeleeExpReward
				// 精灵大乱斗胜方胜场 +1
				u.MessWin++
				if ctx.GameServer.UserDB != nil {
					ctx.GameServer.UserDB.SaveGameData(winnerUID, u)
				}
				if winnerClient := ctx.GameServer.GetClientByUserID(winnerUID); winnerClient != nil {
					body8004 := buildBossMonster8004Body(0, 0, 0, 3, uint32(grandMeleeExpReward))
					ctx.GameServer.SendResponse(winnerClient, 8004, winnerUID, 0, body8004)
				}
				logger.Info(fmt.Sprintf("[2431] 精灵大乱斗胜方 UID=%d 获得 %d 可分配经验", winnerUID, grandMeleeExpReward))
			}
			if loserUID != 0 {
				if u := ctx.GameServer.GetOrCreateUser(loserUID); u != nil {
					u.ExpPool += grandMeleeLoserExpReward
					if ctx.GameServer.UserDB != nil {
						ctx.GameServer.UserDB.SaveGameData(loserUID, u)
					}
					if loserClient := ctx.GameServer.GetClientByUserID(loserUID); loserClient != nil {
						body8004 := buildBossMonster8004Body(0, 0, 0, 3, uint32(grandMeleeLoserExpReward))
						ctx.GameServer.SendResponse(loserClient, 8004, loserUID, 0, body8004)
					}
					logger.Info(fmt.Sprintf("[2431] 精灵大乱斗败方 UID=%d 获得 %d 可分配经验", loserUID, grandMeleeLoserExpReward))
				}
			}
			delete(ctx.GameServer.GrandMeleePets, ctx.UserID)
			delete(ctx.GameServer.GrandMeleePets, opponentUID)
		}
		if opponentUID != 0 {
			if otherClient := ctx.GameServer.GetClientByUserID(opponentUID); otherClient != nil {
				if !battle.IsGrandMelee && len(loserPetInfoBody) > 0 {
					ctx.GameServer.SendResponse(otherClient, 2301, opponentUID, 0, loserPetInfoBody)
				}
				ctx.GameServer.SendResponse(otherClient, 2506, opponentUID, 0, overBody)
				userOpp := ctx.GameServer.GetOrCreateUser(opponentUID)
				mapID := userOpp.MapID
				if mapID == 0 {
					mapID = 1
				}
				ctx.GameServer.SetOgreFightEndTime(opponentUID)
				ctx.GameServer.ClearPlayerOgreSlots(opponentUID, mapID)
				ogreBody := ctx.GameServer.BuildMapOgreListFromSlots(nil)
				ctx.GameServer.SendResponse(otherClient, 2004, opponentUID, 0, ogreBody)
			}
		}
	}
}

// getBattleItemHealHP 战斗中使用的道具：回血药返回恢复体力值，回 PP 药返回 0（PP 药由 getBattleItemHealPP 处理）
// 与 ItemTipXMLInfo/items.xml 对照：HP="N" 为回血药
var battleItemHealHP = map[uint32]int32{
	300001: 20,   // 初级体力药剂 20
	300011: 20,   // 恢复精灵20体力
	300012: 50,   // 恢复精灵50体力
	300013: 100,  // 恢复精灵100体力
	300014: 150,  // 恢复精灵150体力
	300015: 200,  // 恢复精灵200体力
	300020: 3000, // 万能体力药剂 3000
	300022: 150,  // 赫星体力药剂 150（items.xml HP="150"）
	300071: 200,   // 巅峰体力药剂 200
	300072: 250,   // 巅峰对决 250
	300078: 300,
	300154: 200, 300155: 150, 300156: 100, 300157: 200,
	300656: 250, 300664: 250, 300724: 250, 300732: 250,
	300861: 300,
}

// battleItemPPOnly 战斗中仅回 PP、不回血的道具（items.xml ItemType="2" 或 PP="N"）；使用后扣道具、算一回合并实际加 PP
var battleItemPPOnly = map[uint32]bool{
	300021: true, 300023: true, 300024: true, 300025: true, // 300022 为赫星体力药剂 HP150，不在此列
	300016: true, 300017: true, 300018: true, 300019: true, // 初级/中级/高级/超级活力药剂
}

// getBattleItemHealPP 战斗中使用的 PP 药：返回恢复的 PP 点数（每技能槽加 N，上限 MaxPP）；非 PP 药返回 0
var battleItemHealPP = map[uint32]int{
	300016: 5, 300017: 10, 300018: 20, 300019: 40, // 初级/中级/高级/超级活力药剂（items.xml PP="5/10/20/40"）
	300023: 10, // 赫星活力药剂 PP="10"
}

func getBattleItemHealHP(itemID uint32) int32 {
	if battleItemPPOnly[itemID] {
		return 0
	}
	if v, ok := battleItemHealHP[itemID]; ok {
		return v
	}
	return 0
}

func getBattleItemHealPP(itemID uint32) int {
	if v, ok := battleItemHealPP[itemID]; ok {
		return v
	}
	return 0
}

// isNonFuseTraitResetItem 判断是否为“非融合精灵特性重组”相关道具（特性重组剂及其变体）
func isNonFuseTraitResetItem(itemID uint32) bool {
	switch itemID {
	case 300054, // 特性重组剂
		300065,   // 特性重组剂Ω
		300662,   // 特性重组剂折扣版
		300730,   // 特性重组剂折扣版Ω
		300689,   // 专属特性重组剂（部分精灵）
		300693,   // 专属特性重组剂（部分精灵）
		300849:   // 专属特性重组剂
		return true
	default:
		return false
	}
}

// applyNonFuseTraitReset 为精灵随机重置特性（已拥有特性的前提下）
// 规则：
// - 目标精灵必须已有特性 Trait>0；
// - 新特性在 [traitMinIdxLocal, traitMaxIdxLocal] 范围内随机，且：
//   * 不等于旧特性
//   * 不为“吸血”(1039) 与高阶吸血/反弹特性（2188/2187）
func applyNonFuseTraitReset(user *userdb.GameData, catchTime int) bool {
	if user == nil || len(user.Pets) == 0 {
		return false
	}
	// 查找对应精灵
	idx := -1
	for i := range user.Pets {
		if user.Pets[i].CatchTime == catchTime {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	p := user.Pets[idx]
	if p.Trait == 0 {
		// 没有特性不能使用
		return false
	}

	// 排除列表：旧特性 + 吸血/反弹
	exclude := map[int]struct{}{
		p.Trait: {},
		1039:    {},  // 汲取（吸血）
		2187:    {},  // 高阶反弹特性
		2188:    {},  // 高阶吸血特性
	}
	if traitMaxIdxLocal <= traitMinIdxLocal {
		return false
	}
	newTrait := 0
	for tries := 0; tries < 128; tries++ {
		t := traitMinIdxLocal + rand.Intn(traitMaxIdxLocal-traitMinIdxLocal+1)
		if _, forbidden := exclude[t]; forbidden {
			continue
		}
		newTrait = t
		break
	}
	if newTrait == 0 {
		return false
	}
	p.Trait = newTrait
	user.Pets[idx] = p
	return true
}

// handleUsePetItem CMD 2406 使用道具（最小可用版本）
// 对齐 Lua: fight_handlers.handleUsePetItem
func handleUsePetItem(ctx *gameserver.HandlerContext) {
	var itemID uint32
	var catchTime uint32
	if len(ctx.Body) >= 8 {
		catchTime = binary.BigEndian.Uint32(ctx.Body[0:4])
		itemID = binary.BigEndian.Uint32(ctx.Body[4:8])
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)

	// 检查道具是否存在
	itemKey := strconv.FormatUint(uint64(itemID), 10)
	item, hasItem := user.Items[itemKey]
	if !hasItem || item.Count <= 0 {
		// 错误响应：errorCode = 10301（道具不足）
		ctx.GameServer.SendResponse(ctx.ClientData, 2406, ctx.UserID, ctx.SeqID, []byte{})
		return
	}

	// 特性重组类道具：直接在这里处理，不走后续 HP/PP 药逻辑
	if isNonFuseTraitResetItem(itemID) {
		if !applyNonFuseTraitReset(user, int(catchTime)) {
			// 目标不符合条件（无特性/融合精灵/不存在），不消耗道具，返回空响应
			ctx.GameServer.SendResponse(ctx.ClientData, 2406, ctx.UserID, ctx.SeqID, []byte{})
			return
		}
		// 成功重置特性后再扣除道具
		item.Count--
		if item.Count <= 0 {
			delete(user.Items, itemKey)
		} else {
			user.Items[itemKey] = item
		}
		if ctx.GameServer.UserDB != nil {
			ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
		}
		// 对于特性重组，客户端通常会通过 2301/2305 等刷新精灵信息，这里 2406 响应体可为空
		ctx.GameServer.SendResponse(ctx.ClientData, 2406, ctx.UserID, ctx.SeqID, []byte{})
		return
	}

	// 普通 HP/PP 道具：扣除道具
	item.Count--
	if item.Count <= 0 {
		delete(user.Items, itemKey)
	} else {
		user.Items[itemKey] = item
	}

	// 回血药按 itemID 区分效果；回 PP 药扣道具、算一回合并给当前出战精灵 4 个技能槽加 PP（上限 MaxPP）
	healHP := getBattleItemHealHP(itemID)

	// 在战斗中优先使用 BattleState 的 HP；否则退回到宠物当前 HP
	currentHP := int32(0)
	maxHP := uint32(0)

	ctx.GameServer.BattleMu.Lock()
	battle, inBattle := ctx.GameServer.BattleStates[ctx.UserID]
	if inBattle && battle.IsActive {
		currentHP = int32(battle.PlayerHP)
		maxHP = battle.PlayerMaxHP
	} else if len(user.Pets) > 0 {
		petMgr := gamepets.GetInstance()
		// 默认使用首发精灵（与战斗外逻辑一致）；使用持久化 CurrentHP 或满血
		petStats := petMgr.GetStats(user.Pets[0].ID, user.Pets[0].Level, user.Pets[0].DV, user.Pets[0].GetEVStats(), user.Pets[0].Nature)
		currentHP = int32(getPetEffectiveHP(&user.Pets[0], petStats.MaxHP))
		maxHP = uint32(petStats.MaxHP)
	}

	if maxHP > 0 {
		if currentHP+healHP > int32(maxHP) {
			healHP = int32(maxHP) - currentHP
			currentHP = int32(maxHP)
		} else {
			currentHP += healHP
		}
	}

	// 回写 HP：战斗中写回 BattleState；战斗外写回 user.Pets[0].CurrentHP
	if inBattle && battle.IsActive {
		if currentHP < 0 {
			currentHP = 0
		}
		battle.PlayerHP = uint32(currentHP)
		battle.PlayerMaxHP = maxHP
		// PP 药：给当前出战精灵 4 个技能槽各加 N 点 PP（上限为技能 MaxPP）
		if healPP := getBattleItemHealPP(itemID); healPP > 0 {
			skillMgr := gameskills.GetInstance()
			for i := 0; i < 4; i++ {
				maxPP := 20
				if sid := battle.PlayerSkillIDs[i]; sid > 0 {
					if sk := skillMgr.Get(sid); sk != nil && sk.MaxPP > 0 {
						maxPP = sk.MaxPP
					}
				}
				battle.PlayerSkillPP[i] += healPP
				if battle.PlayerSkillPP[i] > maxPP {
					battle.PlayerSkillPP[i] = maxPP
				}
			}
		}
	} else if len(user.Pets) > 0 && maxHP > 0 {
		if currentHP < 0 {
			currentHP = 0
		}
		user.Pets[0].CurrentHP = int(currentHP)
	}
	ctx.GameServer.BattleMu.Unlock()

	// 响应：userId(4) + itemId(4) + hp(4) + changeHp(4)
	body := make([]byte, 16)
	binary.BigEndian.PutUint32(body[0:4], uint32(ctx.UserID))
	binary.BigEndian.PutUint32(body[4:8], itemID)
	binary.BigEndian.PutUint32(body[8:12], uint32(currentHP))
	// changeHp 是有符号整数，需要转换
	if healHP < 0 {
		binary.BigEndian.PutUint32(body[12:16], uint32(math.MaxUint32+int64(healHP)+1))
	} else {
		binary.BigEndian.PutUint32(body[12:16], uint32(healHP))
	}

	ctx.GameServer.SendResponse(ctx.ClientData, 2406, ctx.UserID, ctx.SeqID, body)

	// 保存数据
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}

	// 若在战斗中，使用 HP 药也视为本回合行动，立刻结算敌方一次攻击（与 2407 切换精灵一致）
	if inBattle && battle.IsActive {
		petMgr := gamepets.GetInstance()

		ctx.GameServer.BattleMu.Lock()
		curBattle, ok := ctx.GameServer.BattleStates[ctx.UserID]
		if !ok || !curBattle.IsActive {
			ctx.GameServer.BattleMu.Unlock()
			return
		}

		skillMgr := gameskills.GetInstance()
		var enemySkill *gameskills.Skill
		var enemySkillID uint32
		if sptboss.IsHaMoLeiTeOrderBoss(curBattle.EnemyID) {
			enemySkill, enemySkillID = pickEnemySkillWithPP(curBattle, skillMgr)
		} else {
			enemySkill, enemySkillID = pickEnemySkill(skillMgr, curBattle.EnemyID, curBattle.EnemyLevel)
		}
		if enemySkill == nil || enemySkillID == 0 {
			// 敌人本回合不出招
			ctx.GameServer.BattleMu.Unlock()
			return
		}

		// 控制类异常：疲惫/畏缩/睡眠/麻痹 时敌方本回合无法行动（与 2405 一致）
		enemyFlinched := false
		if curBattle.EnemyStatus[gameskills.StatusIndexFatigue] > 0 {
			enemyFlinched = true
			curBattle.EnemyStatus[gameskills.StatusIndexFatigue]--
		}
		if curBattle.EnemyStatus[gameskills.StatusIndexFear] > 0 {
			enemyFlinched = true
			curBattle.EnemyStatus[gameskills.StatusIndexFear]--
		}
		if curBattle.EnemyStatus[gameskills.StatusIndexSleep] > 0 {
			enemyFlinched = true
			curBattle.EnemyStatus[gameskills.StatusIndexSleep]--
		}
		enemyParalyzed := false
		if curBattle.EnemyStatus[gameskills.StatusIndexParalysis] > 0 {
			enemyParalyzed = true
			curBattle.EnemyStatus[gameskills.StatusIndexParalysis]--
		}
		if enemyFlinched || enemyParalyzed {
			enemySkillID = 0
		}

		enemyDamageCalc := uint32(0)
		enemyDamage := uint32(0)
		// Category 4 = 变化/属性技能（如红韵），不造成伤害；否则会出现“使用 HP 药后对方放属性技能我方扣 30 血”
		if !enemyFlinched && !enemyParalyzed && enemySkill.Category != 4 {
			// 敌人属性
			enemyEV := gamepets.EVStats{}
			enemyStats := petMgr.GetStats(curBattle.EnemyID, curBattle.EnemyLevel, 15, enemyEV, 0)

			// 我方当前出战精灵完整属性（防御/特防用于敌方伤害计算，否则 Defence=0 导致伤害过高/我方直接死亡）
			activeIdx := curBattle.ActivePetIndex
			if activeIdx < 0 || activeIdx >= len(user.Pets) {
				activeIdx = 0
			}
			var playerStatsAfterHeal gamepets.Stats
			if len(user.Pets) > activeIdx {
				p := &user.Pets[activeIdx]
				playerStatsAfterHeal = petMgr.GetStats(p.ID, p.Level, p.DV, p.GetEVStats(), p.Nature)
				playerStatsAfterHeal = applyPetStatBonus(playerStatsAfterHeal, p)
				playerStatsAfterHeal.HP = int(curBattle.PlayerHP)
				playerStatsAfterHeal.MaxHP = int(curBattle.PlayerMaxHP)
			} else {
				playerStatsAfterHeal = gamepets.Stats{MaxHP: int(curBattle.PlayerMaxHP), HP: int(curBattle.PlayerHP)}
			}

			enemyPower := uint32(enemySkill.Power)
			if enemyPower == 0 {
				enemyPower = 40
			}

			// 敌方攻防（按技能类别），并应用强化弱化倍率
			enemyAtk := float64(enemyStats.Attack)
			enemyDef := float64(playerStatsAfterHeal.Defence)
			enemyAtkStage, enemyDefStage := 0, 1
			if enemySkill.Category == 2 {
				enemyAtk = float64(enemyStats.SpAtk)
				enemyDef = float64(playerStatsAfterHeal.SpDef)
				enemyAtkStage, enemyDefStage = 2, 3
			}
			enemyAtk *= gamebattle.GetStatMultiplier(int(curBattle.EnemyBattleLv[enemyAtkStage]))
			enemyDef *= gamebattle.GetStatMultiplier(int(curBattle.PlayerBattleLv[enemyDefStage]))
			if enemyDef < 1 {
				enemyDef = 1
			}

			enemyBaseDamage := math.Floor(((float64(curBattle.EnemyLevel)*0.4 + 2.0) * float64(enemyPower) * enemyAtk / enemyDef / 50.0) + 2.0)

			enemyStab := 1.0
			if enemyPetDef := petMgr.Get(curBattle.EnemyID); enemyPetDef != nil && (enemySkill.Type == enemyPetDef.Type || (enemyPetDef.Type2 > 0 && enemySkill.Type == enemyPetDef.Type2)) {
				enemyStab = 1.5
			}

			enemyTypeMod := 1.0
			if len(user.Pets) > 0 {
				if attackerPetDef := petMgr.Get(user.Pets[0].ID); attackerPetDef != nil {
					enemyPetForType := petMgr.Get(curBattle.EnemyID)
					et1, et2 := 0, 0
					if enemyPetForType != nil {
						et1, et2 = enemyPetForType.Type, enemyPetForType.Type2
					}
					enemyTypeMod = gamebattle.GetTypeMultiplierWithAttackerDual(enemySkill.Type, et1, et2, attackerPetDef.Type, attackerPetDef.Type2)
				}
			}

			enemyRandomMod := float64(rand.Intn(255-217+1)+217) / 255.0
			// 敌人暴击：1/16 基础概率，支持“下 N 回合必定暴击”（EffectID=58 系列）与克瑞斯/盖亚等 BOSS 特性
			enemyCritRate := enemySkill.CritRate
			if enemyCritRate == 0 {
				enemyCritRate = 1
			}
			// 克瑞斯（赫鲁卡城二当家定制 BOSS）：固定 50% 暴击率
			if curBattle.EnemyID == 589 && enemyCritRate < 8 {
				enemyCritRate = 8
			}
			// 若敌方处于“下 N 回合必定暴击”效果中，则直接视为 100% 暴击率
			if curBattle.EnemyCritBuffRounds > 0 {
				enemyCritRate = 16
			}
			enemyCritMod := 1.0
			if rand.Intn(16) < enemyCritRate {
				enemyCritMod = 2.0
				// 敌方暴击时，同样清除我方防御/特防的强化状态
				if enemySkill.Category == 1 {
					// 物理暴击：清空我方防御提升
					if curBattle.PlayerBattleLv[gameskills.StatDefence] > 0 {
						curBattle.PlayerBattleLv[gameskills.StatDefence] = 0
					}
				} else if enemySkill.Category == 2 {
					// 特殊暴击：清空我方特防提升
					if curBattle.PlayerBattleLv[gameskills.StatSpDef] > 0 {
						curBattle.PlayerBattleLv[gameskills.StatSpDef] = 0
					}
				}
			}
			enemyFinalDamage := uint32(enemyBaseDamage * enemyStab * enemyTypeMod * enemyRandomMod * enemyCritMod)
			// 盖亚(261) SPT BOSS 伤害翻倍；PvP 时对方玩家手中的盖亚不享受此 BOSS 特性（此处仅 PvE）
			if curBattle.OpponentUserID == 0 && curBattle.EnemyID == petIDGaiya && enemyFinalDamage > 0 {
				enemyFinalDamage *= 2
			}
			if curBattle.EnemyStatus[gameskills.StatusIndexBurn] > 0 {
				enemyFinalDamage = enemyFinalDamage / 2
				if enemyFinalDamage < 1 {
					enemyFinalDamage = 1
				}
			}
			if enemyFinalDamage < 1 {
				enemyFinalDamage = 1
			}
			enemyDamageCalc = enemyFinalDamage // 理论伤害，用于客户端显示
			if enemyFinalDamage > curBattle.PlayerHP {
				enemyFinalDamage = curBattle.PlayerHP
			}

			enemyDamage = enemyFinalDamage

			// 更新玩家 HP
			curBattle.PlayerHP -= enemyDamage
			if curBattle.PlayerHP > curBattle.PlayerMaxHP {
				curBattle.PlayerHP = 0
			}
		}

		// 我方使用道具也视为本回合的一次出手：
		// 若敌方处于异常状态（烧伤/中毒/冻伤/寄生种子等），
		// 额外结算一次只作用于敌方的持续伤害。
		if curBattle.EnemyHP > 0 {
			gameskills.ProcessEnemyStatusOnPlayerAction(
				&curBattle.PlayerHP, &curBattle.EnemyHP,
				curBattle.PlayerMaxHP, curBattle.EnemyMaxHP,
				&curBattle.PlayerStatus, &curBattle.EnemyStatus,
			)
		}

		opponentUID := curBattle.OpponentUserID
		enemyStatusFor2505 := curBattle.EnemyStatus
		enemyBattleLvFor2505 := curBattle.EnemyBattleLv
		playerStatusFor2505 := curBattle.PlayerStatus
		playerBattleLvFor2505 := curBattle.PlayerBattleLv
		playerHPFor2505 := curBattle.PlayerHP
		playerMaxHPFor2505 := curBattle.PlayerMaxHP
		enemyHPFor2505 := curBattle.EnemyHP
		enemyMaxHPFor2505 := curBattle.EnemyMaxHP
		ctx.GameServer.BattleMu.Unlock()

		// 2505 必须发两条 AttackValue（先我方用道具，再敌方回合），否则客户端会卡住
		enemyUserID := uint32(0)
		if opponentUID != 0 {
			enemyUserID = uint32(opponentUID)
		}
		playerAv := buildAttackValue(
			uint32(ctx.UserID), 0, 0, 0, healHP,
			int32(playerHPFor2505), playerMaxHPFor2505,
			0, 0, 0, playerStatusFor2505, playerBattleLvFor2505,
		)
		enemyAv := buildAttackValue(
			enemyUserID, enemySkillID, 1, enemyDamageCalc, 0,
			int32(enemyHPFor2505), enemyMaxHPFor2505,
			0, 0, 0, enemyStatusFor2505, enemyBattleLvFor2505,
		)
		body2505 := make([]byte, 0, 164)
		body2505 = append(body2505, playerAv...)
		body2505 = append(body2505, enemyAv...)

		ctx.GameServer.SendResponse(ctx.ClientData, 2505, ctx.UserID, ctx.SeqID, body2505)
		if opponentUID != 0 {
			if otherClient := ctx.GameServer.GetClientByUserID(opponentUID); otherClient != nil {
				ctx.GameServer.SendResponse(otherClient, 2505, opponentUID, 0, body2505)
			}
		}
	}
}

// buildNoteUpdateProp 构建 CMD 2508 NOTE_UPDATE_PROP
// 对齐前端解析：
// PetUpdatePropInfo:
//
//	addition(4) + count(4) + UpdatePropInfo * count
//
// UpdatePropInfo:
//
//	catchTime(4) + id(4) + level(4) +
//	exp(4) + currentLvExp(4) + nextLvExp(4) +   // exp=总经验, currentLvExp=当前等级经验
//	maxHp(4) + atk(4) + def(4) + sa(4) + sd(4) + sp(4) +
//	ev_hp(4) + ev_a(4) + ev_d(4) + ev_sa(4) + ev_sd(4) + ev_sp(4)
func buildNoteUpdateProp(catchTime uint32, petID int, level int, currentLevelExp int, maxHP int, attack, defence, spAtk, spDef, speed int, ev gamepets.EVStats) []byte {
	petMgr := gamepets.GetInstance()
	expInfo := petMgr.GetExpInfo(petID, level, currentLevelExp)

	addition := uint32(0) // /100 为加成倍率，先按 0（无额外加成）
	count := uint32(1)

	body := make([]byte, 0, 8+72)
	putU32 := func(v uint32) {
		tmp := make([]byte, 4)
		binary.BigEndian.PutUint32(tmp, v)
		body = append(body, tmp...)
	}

	// PetUpdatePropInfo header
	putU32(addition)
	putU32(count)

	// UpdatePropInfo
	putU32(catchTime)
	putU32(uint32(petID))
	putU32(uint32(level))
	// 与 2301 保持语义一致：
	// - exp: 当前等级已获得经验（与 2301 一致，避免经验分配器“升级所需经验值”为负）
	// - currentLvExp: 当前等级已获得经验
	// - nextLvExp: 升级到下一等级所需经验
	putU32(uint32(expInfo.CurrentLevelExp)) // exp（当前等级经验）
	putU32(uint32(expInfo.CurrentLevelExp)) // currentLvExp（当前等级经验）
	putU32(uint32(expInfo.NextLevelExp))    // nextLvExp
	putU32(uint32(maxHP))
	putU32(uint32(attack))
	putU32(uint32(defence))
	putU32(uint32(spAtk))
	putU32(uint32(spDef))
	putU32(uint32(speed))
	putU32(uint32(ev.HP))
	putU32(uint32(ev.Atk))
	putU32(uint32(ev.Def))
	putU32(uint32(ev.SpAtk))
	putU32(uint32(ev.SpDef))
	putU32(uint32(ev.Spd))

	return body
}

// buildNoteUpdateSkill 构建 CMD 2507 NOTE_UPDATE_SKILL
// 对齐前端 PetUpdateSkillInfo + UpdateSkillInfo 解析：
//
//	PetUpdateSkillInfo: count(4) + [UpdateSkillInfo]*count
//	UpdateSkillInfo: catchTime(4) + activeCount(4) + unactiveCount(4) + [activeId(4)]*activeCount + [unactiveId(4)]*unactiveCount
//
// activeSkills=当前技能(可被替换)，unactiveSkills=新学会的技能
func buildNoteUpdateSkill(catchTime uint32, petID int, oldLevel int, newSkillIDs []int) []byte {
	petMgr := gamepets.GetInstance()
	currentSkills := petMgr.GetSkillsForLevel(petID, oldLevel)
	var activeIDs []int
	for _, sid := range currentSkills {
		if sid > 0 {
			activeIDs = append(activeIDs, sid)
		}
	}
	body := make([]byte, 0, 20+len(activeIDs)*4+len(newSkillIDs)*4)
	putU32 := func(v uint32) {
		tmp := make([]byte, 4)
		binary.BigEndian.PutUint32(tmp, v)
		body = append(body, tmp...)
	}
	putU32(1) // count=1，一个 UpdateSkillInfo
	putU32(catchTime)
	putU32(uint32(len(activeIDs)))
	putU32(uint32(len(newSkillIDs)))
	for _, id := range activeIDs {
		putU32(uint32(id))
	}
	for _, id := range newSkillIDs {
		putU32(uint32(id))
	}
	return body
}

// handleChangePet CMD 2407 切换精灵
// 对齐 Lua: fight_handlers.handleChangePet
func handleChangePet(ctx *gameserver.HandlerContext) {
	var reqCatchTime uint32
	if len(ctx.Body) >= 4 {
		reqCatchTime = binary.BigEndian.Uint32(ctx.Body[0:4])
	}

	logger.Info(fmt.Sprintf("[2407] 收到切换精灵请求: reqCatchTime=%d", reqCatchTime))

	// 前端 2407 被强制按 ChangePetInfo(userID+petID+name16+level+hp+maxHp+catchTime) 解析，
	// 因此“禁止切换”也必须回 40 字节，且不能把 petID 设为 0（否则前端会去加载 pet 0 的资源）。
	// 这里返回“当前出战精灵”的 ChangePetInfo（不发生切换），并在 petName 填入短提示，便于前端弹窗/提示。
	sendSingleOnlyNoSwitch := func(tip string) {
		user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
		list := getBattlePets(ctx.GameServer, ctx.UserID)
		activeIdx := 0
		ctx.GameServer.BattleMu.RLock()
		if b, ok := ctx.GameServer.BattleStates[ctx.UserID]; ok && b != nil && b.IsActive && b.ActivePetIndex >= 0 && b.ActivePetIndex < len(list) {
			activeIdx = b.ActivePetIndex
		}
		ctx.GameServer.BattleMu.RUnlock()
		if activeIdx < 0 || activeIdx >= len(list) {
			activeIdx = 0
		}
		p := userdb.Pet{ID: 7, Level: 5, CatchTime: 0}
		if len(list) > 0 {
			p = list[activeIdx]
		}
		petMgr := gamepets.GetInstance()
		ev := p.GetEVStats()
		st := petMgr.GetStats(p.ID, p.Level, p.DV, ev, p.Nature)
		hp := getPetEffectiveHP(&p, st.MaxHP)
		if hp < 0 {
			hp = st.MaxHP
		}
		body := make([]byte, 40)
		binary.BigEndian.PutUint32(body[0:4], uint32(ctx.UserID))
		binary.BigEndian.PutUint32(body[4:8], uint32(p.ID))
		nameBytes := []byte(tip)
		if len(nameBytes) > 16 {
			nameBytes = nameBytes[:16]
		}
		copy(body[8:24], nameBytes)
		binary.BigEndian.PutUint32(body[24:28], uint32(p.Level))
		binary.BigEndian.PutUint32(body[28:32], uint32(hp))
		binary.BigEndian.PutUint32(body[32:36], uint32(st.MaxHP))
		binary.BigEndian.PutUint32(body[36:40], uint32(p.CatchTime))
		_ = user
		ctx.GameServer.SendResponse(ctx.ClientData, 2407, ctx.UserID, ctx.SeqID, body)
	}

	// 盖亚(261)特训对战（PvE）：仅允许单精灵，禁止切换。
	// 注意：这里只限制 PvE（OpponentUserID==0）的盖亚战斗；
	// PvP 对战中就算对面是 261，也允许按 fightMode=2 进行多精灵切换。
	ctx.GameServer.BattleMu.RLock()
	if battle, ok := ctx.GameServer.BattleStates[ctx.UserID]; ok && battle.IsActive && battle.EnemyID == petIDGaiya && battle.OpponentUserID == 0 {
		ctx.GameServer.BattleMu.RUnlock()
		logger.Warning("[2407] 盖亚对战不允许切换精灵")
		sendSingleOnlyNoSwitch("SINGLE_ONLY")
		return
	}
	ctx.GameServer.BattleMu.RUnlock()

	// 雷伊体能/最终对战特训（PvE）：仅允许单精灵（雷伊），禁止切换。
	ctx.GameServer.BattleMu.RLock()
	if battle, ok := ctx.GameServer.BattleStates[ctx.UserID]; ok && battle.IsActive && battle.OpponentUserID == 0 &&
		battle.BossRegion >= 10000 && battle.BossRegion <= 10006 {
		ctx.GameServer.BattleMu.RUnlock()
		logger.Warning("[2407] 雷伊特训不允许切换精灵")
		sendSingleOnlyNoSwitch("SINGLE_ONLY")
		return
	}
	ctx.GameServer.BattleMu.RUnlock()

	// 雷伊技能特训（里奥斯/提亚斯/雷纳多）（PvE）：仅允许单精灵（雷伊），禁止切换。
	ctx.GameServer.BattleMu.RLock()
	if battle, ok := ctx.GameServer.BattleStates[ctx.UserID]; ok && battle.IsActive && battle.OpponentUserID == 0 &&
		battle.BossRegion == 1 &&
		((battle.BattleMapID == 17 && battle.EnemyID == 42) || (battle.BattleMapID == 27 && battle.EnemyID == 69) || (battle.BattleMapID == 49 && battle.EnemyID == 113)) {
		ctx.GameServer.BattleMu.RUnlock()
		logger.Warning("[2407] 雷伊技能特训不允许切换精灵")
		sendSingleOnlyNoSwitch("SINGLE_ONLY")
		return
	}
	ctx.GameServer.BattleMu.RUnlock()

	// 雷伊技能特训-阿克希亚（PvE）：仅允许单精灵（雷伊），禁止切换。
	ctx.GameServer.BattleMu.RLock()
	if battle, ok := ctx.GameServer.BattleStates[ctx.UserID]; ok && battle.IsActive && battle.OpponentUserID == 0 &&
		battle.BattleMapID == 40 && battle.BossRegion == 1 && battle.EnemyID == 50 {
		ctx.GameServer.BattleMu.RUnlock()
		logger.Warning("[2407] 雷伊技能特训(阿克希亚)不允许切换精灵")
		sendSingleOnlyNoSwitch("SINGLE_ONLY")
		return
	}
	ctx.GameServer.BattleMu.RUnlock()

	_ = ctx.GameServer.GetOrCreateUser(ctx.UserID)
	listForSwitch := getBattlePets(ctx.GameServer, ctx.UserID)

	// 打印所有精灵信息（大乱斗时为分配的 3 只）
	logger.Info(fmt.Sprintf("[2407] 玩家精灵列表: 共%d只", len(listForSwitch)))
	for i, pet := range listForSwitch {
		logger.Info(fmt.Sprintf("[2407]   [%d] PetID=%d CatchTime=%d Level=%d", i, pet.ID, pet.CatchTime, pet.Level))
	}

	// 查找精灵（从当前对战使用的列表中找）
	var picked *userdb.Pet
	var pickedIndex int = -1
	if reqCatchTime != 0 {
		for i := range listForSwitch {
			if uint32(listForSwitch[i].CatchTime) == reqCatchTime {
				picked = &listForSwitch[i]
				pickedIndex = i
				logger.Info(fmt.Sprintf("[2407] 找到匹配的精灵: index=%d PetID=%d", i, listForSwitch[i].ID))
				break
			}
		}
	}
	if picked == nil && len(listForSwitch) > 0 {
		picked = &listForSwitch[0]
		pickedIndex = 0
		logger.Info(fmt.Sprintf("[2407] 未找到匹配精灵，使用第一只: PetID=%d", listForSwitch[0].ID))
	}

	if picked == nil {
		// 错误响应（使用空包）
		logger.Warning("[2407] 没有可用的精灵")
		ctx.GameServer.SendResponse(ctx.ClientData, 2407, ctx.UserID, ctx.SeqID, []byte{})
		return
	}

	petMgr := gamepets.GetInstance()
	petEV := picked.GetEVStats()
	petStats := petMgr.GetStats(picked.ID, picked.Level, picked.DV, petEV, picked.Nature)
	effectiveHP := getPetEffectiveHP(picked, petStats.MaxHP)

	logger.Info(fmt.Sprintf("[2407] 选中精灵: PetID=%d HP=%d/%d", picked.ID, effectiveHP, petStats.MaxHP))

	// 检查精灵是否还有HP
	if effectiveHP <= 0 {
		// 错误响应：精灵已死亡
		logger.Warning(fmt.Sprintf("[2407] 精灵HP为0，无法切换: PetID=%d", picked.ID))
		errorBody := make([]byte, 8)
		binary.BigEndian.PutUint32(errorBody[0:4], uint32(ctx.UserID))
		binary.BigEndian.PutUint32(errorBody[4:8], 1) // 错误码：精灵已死亡
		ctx.GameServer.SendResponse(ctx.ClientData, 2407, ctx.UserID, ctx.SeqID, errorBody)
		return
	}

	// 响应 2407（严格对齐前端 ChangePetInfo 的解析顺序）:
	// userId(4) + petId(4) + petName(16) + level(4) + hp(4) + maxHp(4) + catchTime(4)
	body := make([]byte, 40)
	binary.BigEndian.PutUint32(body[0:4], uint32(ctx.UserID))
	binary.BigEndian.PutUint32(body[4:8], uint32(picked.ID))
	nameBytes := []byte(picked.Name)
	if len(nameBytes) > 16 {
		nameBytes = nameBytes[:16]
	}
	copy(body[8:24], nameBytes)
	// 字段顺序必须与 AS3 `ChangePetInfo` 一致，否则前端血条/等级会错乱
	binary.BigEndian.PutUint32(body[24:28], uint32(picked.Level))
	binary.BigEndian.PutUint32(body[28:32], uint32(effectiveHP))
	binary.BigEndian.PutUint32(body[32:36], uint32(petStats.MaxHP))
	binary.BigEndian.PutUint32(body[36:40], uint32(picked.CatchTime))

	// 调试：打印 2407 包体的字段值与十六进制，方便对照前端 ChangePetInfo
	logger.Info(fmt.Sprintf("[2407] RESP body fields: userID=%d petID=%d level=%d hp=%d maxHp=%d catchTime=%d",
		ctx.UserID, picked.ID, picked.Level, effectiveHP, petStats.MaxHP, picked.CatchTime))
	hexStr := ""
	for i, b := range body {
		if i > 0 && i%4 == 0 {
			hexStr += " "
		}
		hexStr += fmt.Sprintf("%02X", b)
	}
	logger.Info(fmt.Sprintf("[2407] RESP raw hex: %s", hexStr))

	// 保存选中的精灵ID和等级（目前仅用于日志调试）
	selectedPetID := picked.ID
	selectedPetLevel := picked.Level

	// 更新战斗状态中的当前出战精灵下标，而不改变 user.Pets 的顺序，
	// 这样战斗结束后背包首发仍然与客户端显示一致。
	playerWasDead := false // 上一只精灵是否被击败（强制切换 vs 主动切换）
	ctx.GameServer.BattleMu.Lock()
	if battle, exists := ctx.GameServer.BattleStates[ctx.UserID]; exists && battle != nil && battle.IsActive {
		playerWasDead = (battle.PlayerHP == 0) // 在更新前保存：被击败后切换视为新战斗开始
		// 切换前：将当前出战精灵的 HP/PP 写回持久化（含战败写 -1），这样所有曾出战的精灵在战败后都会显示 0 血
		syncBattleHPToUser(ctx.GameServer, ctx.UserID, battle)
		battle.ActivePetIndex = pickedIndex
		if pickedIndex >= 0 && pickedIndex < len(listForSwitch) {
			battle.ActivePetCatchTime = listForSwitch[pickedIndex].CatchTime
		}
		// 切换精灵时重置我方异常状态和强化弱化（新精灵上场为干净状态）；敌方/野生精灵的 status 和 battleLv 不重置
		battle.PlayerStatus = [20]byte{}
		battle.PlayerBattleLv = [6]int8{}
		// 若存在“下一只出战精灵能力+N级”的预存增益（如传承），在新精灵上场时一次性应用。
		for i := 0; i < len(battle.PlayerNextPetBattleLvBoost); i++ {
			if battle.PlayerNextPetBattleLvBoost[i] == 0 {
				continue
			}
			cur := int(battle.PlayerBattleLv[i]) + int(battle.PlayerNextPetBattleLvBoost[i])
			if cur > 6 {
				cur = 6
			}
			if cur < -6 {
				cur = -6
			}
			battle.PlayerBattleLv[i] = int8(cur)
		}
		battle.PlayerNextPetBattleLvBoost = [6]int8{}
		// effect 67：若存在“削减下一只出战精灵最大体力 1/n”的预存效果，在新精灵上场时一次性削减其最大体力并清零
		if battle.PlayerNextPetMaxHPDropDenom > 0 {
			denom := uint32(battle.PlayerNextPetMaxHPDropDenom)
			if denom > 0 && petStats.MaxHP > 0 {
				maxDrop := petStats.MaxHP / int(denom)
				if maxDrop < 0 {
					maxDrop = 0
				}
				if maxDrop > 0 {
					petStats.MaxHP -= maxDrop
					if petStats.MaxHP < 1 {
						petStats.MaxHP = 1
					}
				}
			}
			battle.PlayerNextPetMaxHPDropDenom = 0
		}
		// 注意：电闪光华阵等“自杀后给后续若干次攻击必暴击”的效果是跨精灵的，
		// 因此不在 2407 切宠时转移到 PlayerCritBuffRounds，而是在 2405 的暴击判定处按次数消耗。
		// 使用持久化/满血作为新出战精灵的 HP
		hp := effectiveHP
		if hp < 0 {
			hp = 0
		}
		if hp > petStats.MaxHP {
			hp = petStats.MaxHP
		}
		battle.PlayerHP = uint32(hp)
		battle.PlayerMaxHP = uint32(petStats.MaxHP)
		fillBattlePlayerSkills(battle, picked)
	}
	ctx.GameServer.BattleMu.Unlock()

	// 发送 2407 响应
	// 注意：不再发送 2301、2508、2504，因为：
	// 1. _petInfoMap 在战斗开始时已经包含了所有精灵信息（通过 buildNoteReadyToFightInfo）
	// 2. 2504 会导致前端重新创建精灵模型，造成精灵重叠
	// 3. 2508 属性更新在切换时不需要
	ctx.GameServer.SendResponse(ctx.ClientData, 2407, ctx.UserID, ctx.SeqID, body)

	// PvP：向对手推送 2407（对方换宠），使 B 端界面能显示 A 切换后的新精灵
	ctx.GameServer.BattleMu.RLock()
	if battle, ok := ctx.GameServer.BattleStates[ctx.UserID]; ok && battle.IsActive && battle.OpponentUserID != 0 {
		opponentUID := battle.OpponentUserID
		ctx.GameServer.BattleMu.RUnlock()
		if otherClient := ctx.GameServer.GetClientByUserID(opponentUID); otherClient != nil {
			ctx.GameServer.SendResponse(otherClient, 2407, opponentUID, 0, body)
			logger.Info(fmt.Sprintf("[2407-PvP] 推送对方换宠至对手 UID=%d，新宠 PetID=%d", opponentUID, selectedPetID))
		}
	} else {
		ctx.GameServer.BattleMu.RUnlock()
	}

	// 更新战斗状态，并保存敌方当前 HP 供后续 2505 第二条使用（避免误发“满血”导致客户端显示虚假回血 +N）
	var enemyHPFor2505, enemyMaxHPFor2505 uint32
	ctx.GameServer.BattleMu.Lock()
	battle, exists := ctx.GameServer.BattleStates[ctx.UserID]
	if exists && battle.IsActive {
		enemyHPFor2505 = battle.EnemyHP
		enemyMaxHPFor2505 = battle.EnemyMaxHP
		// 更新玩家HP，钳位到 [0, MaxHP] 保证血条范围有效（与上面一致用 effectiveHP）
		hp := effectiveHP
		if hp < 0 {
			hp = 0
		}
		if hp > petStats.MaxHP {
			hp = petStats.MaxHP
		}
		battle.PlayerHP = uint32(hp)
		battle.PlayerMaxHP = uint32(petStats.MaxHP)
		// 切换精灵视为“自身”变化，下 N 回合必定暴击（SideEffect 58 系列）不应继承到新精灵
		battle.PlayerCritBuffRounds = 0
		battle.PlayerFirstPriorityRounds = 0
		battle.PlayerFirstVampRounds = 0
		battle.PlayerFirstFearRounds = 0
		battle.PlayerFirstFearChance = 0
		logger.Info(fmt.Sprintf("[2407] 更新战斗状态: PlayerHP=%d/%d PetID=%d 敌方HP=%d/%d(供2505)", battle.PlayerHP, battle.PlayerMaxHP, selectedPetID, enemyHPFor2505, enemyMaxHPFor2505))
		// PvP 模式：同时更新对方 BattleState 中的 EnemyID（我方切换后的 petID）
		// 谁切换则谁的新精灵为干净状态；对方视角的 Enemy 就是切换方的新精灵，需重置 EnemyStatus/EnemyBattleLv
		if battle.OpponentUserID != 0 {
			if opponentBattle, opponentExists := ctx.GameServer.BattleStates[battle.OpponentUserID]; opponentExists && opponentBattle.IsActive {
				opponentBattle.EnemyID = selectedPetID
				opponentBattle.EnemyLevel = selectedPetLevel
				opponentBattle.EnemyHP = uint32(hp)
				opponentBattle.EnemyMaxHP = uint32(petStats.MaxHP)
				opponentBattle.EnemyStatus = [20]byte{}  // 切换方新精灵上场，重置其异常状态
				opponentBattle.EnemyBattleLv = [6]int8{} // 重置其强化弱化
				opponentBattle.EnemyFirstPriorityRounds = 0
				opponentBattle.EnemyFirstVampRounds = 0
				opponentBattle.EnemyFirstFearRounds = 0
				opponentBattle.EnemyFirstFearChance = 0
				logger.Info(fmt.Sprintf("[2407] 更新对方战斗状态: OpponentUID=%d EnemyID=%d EnemyHP=%d/%d", battle.OpponentUserID, selectedPetID, petStats.HP, petStats.MaxHP))

				// ==================== PvP: 双方都只“主动”切换精灵时，补发一次零伤害 2505 结束本回合 ====================
				// 说明：
				// - 若上一只精灵已被击败（playerWasDead==true），本次为“强制换宠”，视为新战斗开始，不应计作本回合行动。
				// - 只有在双方当前精灵仍有 HP、且都主动发送 2407 换宠时，才算作本回合各自的一次行动。
				if !playerWasDead {
					// 1. 标记当前玩家本回合已主动切换精灵；
					// 2. 若对方 BattleState 也在本回合主动切换过精灵（PvPChangedPetThisRound=true），
					//    则构造一条“双方 atkTimes=0、lostHP=0、remainHP=当前血量”的 2505，发给双方，
					//    让前端执行一次 executeTurn()/nextRound()，重新开始倒计时与选招。
					battle.PvPChangedPetThisRound = true
					if opponentBattle.PvPChangedPetThisRound {
						// 复制当前双方状态，避免解锁后被修改
						playerHP := battle.PlayerHP
						playerMaxHP := battle.PlayerMaxHP
						playerStatus := battle.PlayerStatus
						playerBattleLv := battle.PlayerBattleLv

						enemyPlayerHP := opponentBattle.PlayerHP
						enemyPlayerMaxHP := opponentBattle.PlayerMaxHP
						enemyPlayerStatus := opponentBattle.PlayerStatus
						enemyPlayerBattleLv := opponentBattle.PlayerBattleLv

						// 清除本回合“已切换精灵”标记，准备下一回合
						battle.PvPChangedPetThisRound = false
						opponentBattle.PvPChangedPetThisRound = false

						opponentUID := battle.OpponentUserID
						ctx.GameServer.BattleMu.Unlock()

						// 使用 buildAttackValue 构造双方“零伤害” AttackValue
						playerAv := buildAttackValue(
							uint32(ctx.UserID), 0, 0,
							0, 0,
							int32(playerHP), playerMaxHP,
							0, 0, 0,
							playerStatus, playerBattleLv,
						)
						enemyAv := buildAttackValue(
							uint32(opponentUID), 0, 0,
							0, 0,
							int32(enemyPlayerHP), enemyPlayerMaxHP,
							0, 0, 0,
							enemyPlayerStatus, enemyPlayerBattleLv,
						)
						body := make([]byte, 0, len(playerAv)+len(enemyAv))
						// 顺序固定为 [当前玩家, 对方]，本回合双方都未攻击，客户端仅用于进入下一回合
						body = append(body, playerAv...)
						body = append(body, enemyAv...)

						ctx.GameServer.SendResponse(ctx.ClientData, 2505, ctx.UserID, 0, body)
						if otherClient := ctx.GameServer.GetClientByUserID(opponentUID); otherClient != nil {
							ctx.GameServer.SendResponse(otherClient, 2505, opponentUID, 0, body)
						}
						logger.Info(fmt.Sprintf("[2407-PvP] 双方本回合都仅主动切换精灵，发送零伤害 2505 结束回合: UID1=%d UID2=%d", ctx.UserID, opponentUID))
						return
					}
				}
			}
		}
	}
	ctx.GameServer.BattleMu.Unlock()

	// 如果战斗中，且为主动切换（上一只精灵未死）：
	// - PvE：敌方立刻行动一回合（换宠消耗回合）；
	// - PvP：切换视为本回合动作，但不在这里触发敌方 AI，真实对手会通过自己的 2405 出招。
	// 若为强制切换（上一只精灵被击败）：视为新战斗开始，敌方本回合已出招击杀，不再攻击新上场的精灵。
	if playerWasDead && exists && battle.IsActive {
		logger.Info("[2407] 强制切换（上一只精灵被击败），视为新战斗开始，敌方本回合不再攻击")
	}
	// 注意：不再发送 2508 和 2504，因为：
	// 1. 2504 会导致前端重新创建精灵模型，造成精灵重叠
	// 2. 2508 属性更新在切换时不需要
	// 只发送 2505（敌方攻击）
	if exists && battle.IsActive && !playerWasDead {
		// PvP：只更新出战精灵，本回合攻击由对方玩家的 2405 决定，这里不触发 AI 反击。
		if battle.OpponentUserID != 0 {
			logger.Info("[2407-PvP] 主动切换精灵，记作本回合动作，不触发敌方 AI 反击，等待对方 2405/2407")
			return
		}

		petMgr := gamepets.GetInstance()

		// ==================== 敌方立刻行动一回合（换宠消耗回合，Lua 版 2407 也会触发一次 executeTurn）====================
		// 这里只实现 PvE 简化逻辑：敌人使用默认技能 10001 攻击玩家一次，构造 2505。

		// 重新加锁，使用最新的战斗状态并写回本回合结果
		ctx.GameServer.BattleMu.Lock()
		curBattle, ok := ctx.GameServer.BattleStates[ctx.UserID]
		if !ok || !curBattle.IsActive {
			ctx.GameServer.BattleMu.Unlock()
			return
		}

		skillMgr := gameskills.GetInstance()
		var enemySkill *gameskills.Skill
		var enemySkillID uint32
		if sptboss.IsHaMoLeiTeOrderBoss(curBattle.EnemyID) {
			enemySkill, enemySkillID = pickEnemySkillWithPP(curBattle, skillMgr)
		} else {
			enemySkill, enemySkillID = pickEnemySkill(skillMgr, curBattle.EnemyID, curBattle.EnemyLevel)
		}
		if enemySkill == nil || enemySkillID == 0 {
			// 敌人本回合不出招：不做任何反击，直接返回
			ctx.GameServer.BattleMu.Unlock()
			return
		}

		// 控制类异常：疲惫/畏缩/睡眠/麻痹 时敌方本回合无法行动（与 2405 一致）
		enemyFlinched := false
		if curBattle.EnemyStatus[gameskills.StatusIndexFatigue] > 0 {
			enemyFlinched = true
			curBattle.EnemyStatus[gameskills.StatusIndexFatigue]--
		}
		if curBattle.EnemyStatus[gameskills.StatusIndexFear] > 0 {
			enemyFlinched = true
			curBattle.EnemyStatus[gameskills.StatusIndexFear]--
		}
		if curBattle.EnemyStatus[gameskills.StatusIndexSleep] > 0 {
			enemyFlinched = true
			curBattle.EnemyStatus[gameskills.StatusIndexSleep]--
		}
		enemyParalyzed := false
		if curBattle.EnemyStatus[gameskills.StatusIndexParalysis] > 0 {
			enemyParalyzed = true
			curBattle.EnemyStatus[gameskills.StatusIndexParalysis]--
		}
		if enemyFlinched || enemyParalyzed {
			enemySkillID = 0 // 受控制未出招，不发送技能 ID
		}

		// 敌人属性（与 2405 中一致）
		enemyEV := gamepets.EVStats{}
		enemyStats := petMgr.GetStats(curBattle.EnemyID, curBattle.EnemyLevel, 15, enemyEV, 0)

		// 玩家属性：使用当前切换后的精灵战斗属性
		playerStatsAfterSwitch := petStats

		enemyDamage := uint32(0)
		enemyDamageCalc := uint32(0)
		// Category 4 = 变化/属性技能（如红韵），不造成伤害；否则会出现“切换精灵对方放属性技能我方血量掉一半”
		if !enemyFlinched && !enemyParalyzed && enemySkill.Category != 4 {
			enemyPower := uint32(enemySkill.Power)
			if enemyPower == 0 {
				enemyPower = 40
			}

			// 敌方攻防（按技能类别），并应用强化弱化倍率（切换后我方 battleLv 已重置为 0）
			enemyAtk := float64(enemyStats.Attack)
			enemyDef := float64(playerStatsAfterSwitch.Defence)
			enemyAtkStage, enemyDefStage := 0, 1
			if enemySkill.Category == 2 {
				enemyAtk = float64(enemyStats.SpAtk)
				enemyDef = float64(playerStatsAfterSwitch.SpDef)
				enemyAtkStage, enemyDefStage = 2, 3
			}
			enemyAtk *= gamebattle.GetStatMultiplier(int(curBattle.EnemyBattleLv[enemyAtkStage]))
			enemyDef *= gamebattle.GetStatMultiplier(int(curBattle.PlayerBattleLv[enemyDefStage]))
			if enemyDef < 1 {
				enemyDef = 1
			}

			enemyBaseDamage := math.Floor(((float64(curBattle.EnemyLevel)*0.4 + 2.0) * float64(enemyPower) * enemyAtk / enemyDef / 50.0) + 2.0)

			enemyStab := 1.0
			if enemyPetDef := petMgr.Get(curBattle.EnemyID); enemyPetDef != nil && (enemySkill.Type == enemyPetDef.Type || (enemyPetDef.Type2 > 0 && enemySkill.Type == enemyPetDef.Type2)) {
				enemyStab = 1.5
			}

			enemyTypeMod := 1.0
			if attackerPetDef := petMgr.Get(selectedPetID); attackerPetDef != nil {
				enemyPetForType := petMgr.Get(curBattle.EnemyID)
				et1, et2 := 0, 0
				if enemyPetForType != nil {
					et1, et2 = enemyPetForType.Type, enemyPetForType.Type2
				}
				enemyTypeMod = gamebattle.GetTypeMultiplierWithAttackerDual(enemySkill.Type, et1, et2, attackerPetDef.Type, attackerPetDef.Type2)
			}

			enemyRandomMod := float64(rand.Intn(255-217+1)+217) / 255.0
			enemyFinalDamage := uint32(enemyBaseDamage * enemyStab * enemyTypeMod * enemyRandomMod)
			if curBattle.EnemyStatus[gameskills.StatusIndexBurn] > 0 {
				enemyFinalDamage = enemyFinalDamage / 2
				if enemyFinalDamage < 1 {
					enemyFinalDamage = 1
				}
			}
			if enemyFinalDamage < 1 {
				enemyFinalDamage = 1
			}
			enemyDamageCalc = enemyFinalDamage // 理论伤害，用于客户端显示
			if enemyFinalDamage > curBattle.PlayerHP {
				enemyFinalDamage = curBattle.PlayerHP
			}

			enemyDamage = enemyFinalDamage

			// 更新玩家 HP
			curBattle.PlayerHP -= enemyDamage
			if curBattle.PlayerHP > curBattle.PlayerMaxHP {
				curBattle.PlayerHP = 0
			}
		}

		// 记录更新后的 HP、敌方 status/battleLv（用于构造 2505），然后解锁
		opponentUID := curBattle.OpponentUserID
		enemyStatusFor2505 := curBattle.EnemyStatus // 敌方异常状态和强化弱化跟随精灵，不重置，需正确下发
		enemyBattleLvFor2505 := curBattle.EnemyBattleLv
		ctx.GameServer.BattleMu.Unlock()

		// 构造 2505：第一条为敌人攻击玩家，第二条为占位（使用切换前保存的敌方 HP，避免客户端误显示“+N”回血）
		body2505 := make([]byte, 0, 160)

		// 敌人 userID：PvE 为 0，PvP 为对方 UID
		enemyUserID := uint32(0)
		if opponentUID != 0 {
			enemyUserID = uint32(opponentUID)
		}

		// 第一条：敌人攻击玩家（status/battleLv 用敌方实际值，客户端才能正确显示对方异常与强化弱化）
		body2505 = append(body2505, buildAttackValue(enemyUserID, enemySkillID, 1, enemyDamageCalc, 0, int32(curBattle.EnemyHP), curBattle.EnemyMaxHP, 0, 0, 0, enemyStatusFor2505, enemyBattleLvFor2505)...)

		// 第二条：占位，同步敌方血条。必须用 enemyUserID（敌方），因前端按 attackValue.userID 更新对应方血条；status/battleLv 同样下发敌方实际值
		if enemyMaxHPFor2505 == 0 {
			enemyMaxHPFor2505 = 1
		}
		body2505 = append(body2505, buildAttackValue(enemyUserID, 0, 0, 0, 0, int32(enemyHPFor2505), enemyMaxHPFor2505, 0, 0, 0, enemyStatusFor2505, enemyBattleLvFor2505)...)

		ctx.GameServer.SendResponse(ctx.ClientData, 2505, ctx.UserID, ctx.SeqID, body2505)
		if opponentUID != 0 {
			if otherClient := ctx.GameServer.GetClientByUserID(opponentUID); otherClient != nil {
				ctx.GameServer.SendResponse(otherClient, 2505, opponentUID, 0, body2505)
			}
		}
	}
}

// capsuleCatchMod 胶囊 ID -> 捕捉成功率修正（0.5~1.0）。无敌精灵胶囊(300006)为 100%
var capsuleCatchMod = map[int]float64{
	300001: 0.5,   // 普通精灵胶囊
	300002: 0.65,  // 中级精灵胶囊
	300003: 0.8,   // 高级精灵胶囊
	300004: 0.95,  // 超级精灵胶囊
	300005: 0.9,   // 其他
	300006: 1.0,   // 无敌精灵胶囊（100%）
	300009: 1.0,   // 时空精灵胶囊
}

// handleCatchMonster CMD 2409 捕捉精灵
// 请求: itemID(4) 胶囊物品 ID，客户端 CatchPetItemCategory 发送
// 响应成功: catchTime(4)+petId(4)；失败: 空 body
func handleCatchMonster(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)

	// 解析胶囊 ID
	capsuleID := 300001
	if len(ctx.Body) >= 4 {
		capsuleID = int(binary.BigEndian.Uint32(ctx.Body[0:4]))
	}
	if capsuleID <= 0 {
		capsuleID = 300001
	}

	// 校验胶囊：必须在有效列表中且有库存
	capsuleMod, validCapsule := capsuleCatchMod[capsuleID]
	if !validCapsule {
		capsuleMod = 0.5
		capsuleID = 300001
	}
	if user.Items == nil {
		user.Items = make(map[string]userdb.Item)
	}
	itemKey := strconv.Itoa(capsuleID)
	it, hasCapsule := user.Items[itemKey]
	if !hasCapsule || it.Count < 1 {
		ctx.GameServer.SendResponse(ctx.ClientData, 2409, ctx.UserID, ctx.SeqID, []byte{})
		logger.Info(fmt.Sprintf("[2409] 胶囊不足: UID=%d itemID=%d", ctx.UserID, capsuleID))
		return
	}

	ctx.GameServer.BattleMu.Lock()
	battle, exists := ctx.GameServer.BattleStates[ctx.UserID]
	if exists && battle.IsActive && battle.OpponentUserID != 0 {
		ctx.GameServer.BattleMu.Unlock()
		// PvP 模式下不可捕捉对方精灵：返回空/失败，客户端会显示“赛尔间对战无法捕捉”等
		ctx.GameServer.SendResponse(ctx.ClientData, 2409, ctx.UserID, ctx.SeqID, []byte{})
		logger.Info(fmt.Sprintf("[2409] PvP 模式拒绝捕捉: UID=%d", ctx.UserID))
		return
	}
	bossID := 13
	bossLevel := 5
	enemyHP, enemyMaxHP := uint32(0), uint32(1)
	battleMapID := 0
	if exists && battle.IsActive {
		bossID = battle.EnemyID
		bossLevel = battle.EnemyLevel
		enemyHP, enemyMaxHP = battle.EnemyHP, battle.EnemyMaxHP
		battleMapID = battle.BattleMapID
		if enemyMaxHP == 0 {
			enemyMaxHP = 1
		}
		logger.Info(fmt.Sprintf("[2409] 从战斗状态获取: EnemyID=%d EnemyLevel=%d HP=%d/%d", bossID, bossLevel, enemyHP, enemyMaxHP))

		// 捕捉也算一回合行动：用于尼奥(416)逃跑判定。
		if battle.IsWildMonster && battle.EnemyID == 416 {
			// 如果当前出战的是尼尔(77)，标记已见尼尔
			activeIdx := 0
			if battle.ActivePetIndex >= 0 && battle.ActivePetIndex < len(user.Pets) {
				activeIdx = battle.ActivePetIndex
			}
			if len(user.Pets) > 0 && activeIdx >= 0 && activeIdx < len(user.Pets) {
				if user.Pets[activeIdx].ID == 77 {
					battle.HasSeenNeal = true
				}
			}
			if !battle.HasSeenNeal {
				battle.RoundCount++
				if battle.RoundCount >= 2 {
					// 尼奥在多次尝试捕捉且从未见到尼尔时逃跑
					logger.Info(fmt.Sprintf("[2409] 尼奥逃跑事件触发(捕捉): UID=%d Round=%d", ctx.UserID, battle.RoundCount))
					battle.IsActive = false
					syncBattleHPToUser(ctx.GameServer, ctx.UserID, battle)
					delete(ctx.GameServer.BattleStates, ctx.UserID)
					ctx.GameServer.BattleMu.Unlock()

					overBody := buildFightOverInfo(0, 0)
					ctx.GameServer.SendResponse(ctx.ClientData, 2506, ctx.UserID, ctx.SeqID, overBody)
					pushMapOgreListAfterFightOver(ctx)
					return
				}
			}
		}
	} else {
		logger.Warning(fmt.Sprintf("[2409] 未找到战斗状态，使用默认值: EnemyID=%d EnemyLevel=%d", bossID, bossLevel))
	}
	ctx.GameServer.BattleMu.Unlock()

	if _, ok := sptboss.GetByPetID(bossID); ok {
		ctx.GameServer.SendResponse(ctx.ClientData, 2409, ctx.UserID, ctx.SeqID, []byte{})
		logger.Info(fmt.Sprintf("[2409] SPT BOSS 不可捕捉，拒绝: UID=%d PetID=%d", ctx.UserID, bossID))
		return
	}

	// 进化形态（第二/第三形态等）作为野生刷新时不可捕捉：仅允许进化链的第一形态/单形态精灵被捕捉。
	// 这里只依赖 pets.xml 的 EvolvesFrom 字段，与 CatchRate 无关。
	if petMgr := gamepets.GetInstance(); petMgr != nil {
		if def := petMgr.Get(bossID); def != nil && def.EvolvesFrom > 0 {
			ctx.GameServer.SendResponse(ctx.ClientData, 2409, ctx.UserID, ctx.SeqID, []byte{})
			logger.Info(fmt.Sprintf("[2409] 进化形态不可捕捉，拒绝: UID=%d PetID=%d EvolvesFrom=%d", ctx.UserID, bossID, def.EvolvesFrom))
			return
		}
	}

	// 精灵太空站(9515) 布鲁：每日仅可捕捉一次
	if battleMapID == 9515 && bossID == 108 {
		today := time.Now().Unix() / 86400
		key := "9515:108"
		if user.DailyBossCaptureMap != nil {
			if lastDate := user.DailyBossCaptureMap[key]; lastDate == today {
				ctx.GameServer.SendResponse(ctx.ClientData, 2409, ctx.UserID, ctx.SeqID, []byte{})
				logger.Info(fmt.Sprintf("[2409] 布鲁每日仅可捕捉一次，今日已捕捉: UID=%d", ctx.UserID))
				return
			}
		}
	}

	// 消耗胶囊（无论成功失败，使用即消耗）
	it.Count--
	if it.Count <= 0 {
		delete(user.Items, itemKey)
	} else {
		user.Items[itemKey] = it
	}

	// 计算捕捉概率：基础率(petCatchRate/255) * HP因子(血量越低越易捉) * 胶囊修正
	petMgr := gamepets.GetInstance()
	petDef := petMgr.Get(bossID)
	petCatchRate := 255
	if battleMapID == 461 {
		// 赫鲁卡城(461)：所有野生精灵捕捉率 1%
		petCatchRate = 3 // 3/255 ≈ 1.18%
	} else if petDef != nil && petDef.CatchRate > 0 {
		petCatchRate = petDef.CatchRate
	}
	hpFactor := 0.3 + 0.7*(1.0-float64(enemyHP)/float64(enemyMaxHP))
	baseRate := float64(petCatchRate) / 255.0
	catchProb := baseRate * hpFactor * capsuleMod
	if catchProb > 0.99 && capsuleMod < 1.0 {
		catchProb = 0.99
	}
	if capsuleMod >= 1.0 {
		catchProb = 1.0
	}
	roll := rand.Float64()
	caught := roll < catchProb

	logger.Info(fmt.Sprintf("[2409] 捕捉判定: itemID=%d petCatchRate=%d hpFactor=%.2f mod=%.2f prob=%.2f roll=%.2f -> %v",
		capsuleID, petCatchRate, hpFactor, capsuleMod, catchProb, roll, caught))

	if !caught {
		ctx.GameServer.SendResponse(ctx.ClientData, 2409, ctx.UserID, ctx.SeqID, []byte{})
		if ctx.GameServer.UserDB != nil {
			ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
		}
		logger.Info(fmt.Sprintf("[2409] 捕捉失败: UID=%d PetID=%d (胶囊已消耗)，推送敌方回合", ctx.UserID, bossID))

		// 捕捉失败后推送敌方回合（CMD 2505），避免客户端卡在“等待对方”
		petMgr := gamepets.GetInstance()
		skillMgr := gameskills.GetInstance()
		ctx.GameServer.BattleMu.Lock()
		curBattle, ok := ctx.GameServer.BattleStates[ctx.UserID]
		if !ok || !curBattle.IsActive {
			ctx.GameServer.BattleMu.Unlock()
			return
		}
		var enemySkill *gameskills.Skill
		var enemySkillID uint32
		if sptboss.IsHaMoLeiTeOrderBoss(curBattle.EnemyID) {
			enemySkill, enemySkillID = pickEnemySkillWithPP(curBattle, skillMgr)
		} else {
			enemySkill, enemySkillID = pickEnemySkill(skillMgr, curBattle.EnemyID, curBattle.EnemyLevel)
		}
		enemyFlinched := false
		// 控制类异常：疲惫/畏缩/睡眠/麻痹 时敌方本回合无法行动（与 2405 一致）
		if curBattle.EnemyStatus[gameskills.StatusIndexFatigue] > 0 {
			enemyFlinched = true
			curBattle.EnemyStatus[gameskills.StatusIndexFatigue]--
		}
		if curBattle.EnemyStatus[gameskills.StatusIndexFear] > 0 {
			enemyFlinched = true
			curBattle.EnemyStatus[gameskills.StatusIndexFear]--
		}
		if curBattle.EnemyStatus[gameskills.StatusIndexSleep] > 0 {
			enemyFlinched = true
			curBattle.EnemyStatus[gameskills.StatusIndexSleep]--
		}
		enemyParalyzed := curBattle.EnemyStatus[gameskills.StatusIndexParalysis] > 0
		if enemyParalyzed {
			curBattle.EnemyStatus[gameskills.StatusIndexParalysis]--
		}
		if enemyFlinched || enemyParalyzed {
			enemySkillID = 0
		}
		enemyDamageCalc := uint32(0)
		enemyDamage := uint32(0)
		if enemySkill != nil && enemySkillID != 0 && !enemyFlinched && !enemyParalyzed {
			// 非攻击型技能（如 Category=4、纯属性技）不应在这里造成伤害，只影响命中/能力等
			if enemySkill.Category != 4 && enemySkill.Power > 0 {
				// 使用当前出战精灵的真实防御/特防，而不是 0，保证这一下伤害“正常”
				activeIdx := curBattle.ActivePetIndex
				if activeIdx < 0 || activeIdx >= len(user.Pets) {
					activeIdx = 0
				}
				playerDefStat := float64(1)
				playerSpDefStat := float64(1)
				if len(user.Pets) > 0 && activeIdx >= 0 && activeIdx < len(user.Pets) {
					activePet := user.Pets[activeIdx]
					playerEV := activePet.GetEVStats()
					playerStatsFull := petMgr.GetStats(activePet.ID, activePet.Level, activePet.DV, playerEV, activePet.Nature)
					playerStatsFull = applyPetStatBonus(playerStatsFull, &activePet)
					playerDefStat = float64(playerStatsFull.Defence)
					playerSpDefStat = float64(playerStatsFull.SpDef)
					if playerDefStat < 1 {
						playerDefStat = 1
					}
					if playerSpDefStat < 1 {
						playerSpDefStat = 1
					}
				}

				enemyEV := gamepets.EVStats{}
				enemyStats := petMgr.GetStats(curBattle.EnemyID, curBattle.EnemyLevel, 15, enemyEV, 0)
				enemyPower := uint32(enemySkill.Power)
				enemyAtk := float64(enemyStats.Attack)
				enemyDef := playerDefStat
				enemyAtkStage, enemyDefStage := 0, 1
				if enemySkill.Category == 2 {
					enemyAtk = float64(enemyStats.SpAtk)
					enemyDef = playerSpDefStat
					enemyAtkStage, enemyDefStage = 2, 3
				}
				enemyAtk *= gamebattle.GetStatMultiplier(int(curBattle.EnemyBattleLv[enemyAtkStage]))
				enemyDef *= gamebattle.GetStatMultiplier(int(curBattle.PlayerBattleLv[enemyDefStage]))
				if enemyDef < 1 {
					enemyDef = 1
				}
				enemyBaseDamage := math.Floor(((float64(curBattle.EnemyLevel)*0.4+2.0)*float64(enemyPower)*enemyAtk/enemyDef/50.0) + 2.0)
				enemyStab := 1.0
				if enemyPetDef := petMgr.Get(curBattle.EnemyID); enemyPetDef != nil && (enemySkill.Type == enemyPetDef.Type || (enemyPetDef.Type2 > 0 && enemySkill.Type == enemyPetDef.Type2)) {
					enemyStab = 1.5
				}
				enemyTypeMod := 1.0
				if len(user.Pets) > 0 && activeIdx >= 0 && activeIdx < len(user.Pets) {
					if attackerPetDef := petMgr.Get(user.Pets[activeIdx].ID); attackerPetDef != nil {
						enemyTypeMod = gamebattle.GetTypeMultiplierDual(enemySkill.Type, attackerPetDef.Type, attackerPetDef.Type2)
					}
				}
				enemyRandomMod := float64(rand.Intn(255-217+1)+217) / 255.0
				enemyFinalDamage := uint32(enemyBaseDamage * enemyStab * enemyTypeMod * enemyRandomMod)
				if curBattle.EnemyStatus[gameskills.StatusIndexBurn] > 0 {
					enemyFinalDamage = enemyFinalDamage / 2
					if enemyFinalDamage < 1 {
						enemyFinalDamage = 1
					}
				}
				if enemyFinalDamage < 1 {
					enemyFinalDamage = 1
				}
				enemyDamageCalc = enemyFinalDamage
				if enemyFinalDamage > curBattle.PlayerHP {
					enemyFinalDamage = curBattle.PlayerHP
				}
				enemyDamage = enemyFinalDamage
				curBattle.PlayerHP -= enemyDamage
				if curBattle.PlayerHP > curBattle.PlayerMaxHP {
					curBattle.PlayerHP = 0
				}
			}
			// TODO：如需完全对齐 Lua，可在此调用技能效果系统，应用命中下降等属性效果（不改 HP）
		}
		opponentUID := curBattle.OpponentUserID
		enemyStatusFor2505 := curBattle.EnemyStatus
		enemyBattleLvFor2505 := curBattle.EnemyBattleLv
		playerHPAfter := curBattle.PlayerHP
		playerMaxHP := curBattle.PlayerMaxHP
		playerStatusFor2505 := curBattle.PlayerStatus
		playerBattleLvFor2505 := curBattle.PlayerBattleLv
		ctx.GameServer.BattleMu.Unlock()

		// 与 2405 一致：2505 发两条 AttackValue（我方行动 + 对方行动），否则客户端可能卡在“等待对方”
		playerAv := buildAttackValue(
			uint32(ctx.UserID), 0, 1, // 我方本回合“捕捉”，skillID=0 表示无攻击技能
			0, 0, int32(playerHPAfter), uint32(playerMaxHP),
			0, 0, 0,
			playerStatusFor2505, playerBattleLvFor2505,
		)
		enemyUserID := uint32(0)
		if opponentUID != 0 {
			enemyUserID = uint32(opponentUID)
		}
		enemyAv := buildAttackValue(
			enemyUserID, enemySkillID, 1,
			enemyDamageCalc, 0,
			int32(curBattle.EnemyHP), curBattle.EnemyMaxHP,
			0, 0, 0,
			enemyStatusFor2505, enemyBattleLvFor2505,
		)
		body2505 := make([]byte, 0, 164)
		body2505 = append(body2505, playerAv...)
		body2505 = append(body2505, enemyAv...)
		ctx.GameServer.SendResponse(ctx.ClientData, 2505, ctx.UserID, ctx.SeqID, body2505)
		if opponentUID != 0 {
			if otherClient := ctx.GameServer.GetClientByUserID(opponentUID); otherClient != nil {
				ctx.GameServer.SendResponse(otherClient, 2505, opponentUID, 0, body2505)
			}
		}
		return
	}

	newCatchTime := uint32(time.Now().Unix())
	rand.Seed(time.Now().UnixNano() + int64(newCatchTime))
	catchLevel := bossLevel
	randomDV := rand.Intn(32)
	randomNature := rand.Intn(25)

	// 捕捉到的精灵是否异色：来自当前战斗的敌方（野生=槽位异色，BOSS=1）；默认 0，仅当战斗状态明确为异色时才为 1
	caughtShiny := 0
	ctx.GameServer.BattleMu.RLock()
	if b := ctx.GameServer.BattleStates[ctx.UserID]; b != nil && b.EnemyShiny != 0 {
		caughtShiny = 1
	}
	ctx.GameServer.BattleMu.RUnlock()

	shinyVal := uint32(0)
	if caughtShiny != 0 {
		shinyVal = 1
	}
	body := make([]byte, 12)
	binary.BigEndian.PutUint32(body[0:4], newCatchTime)
	binary.BigEndian.PutUint32(body[4:8], uint32(bossID))
	binary.BigEndian.PutUint32(body[8:12], shinyVal)
	ctx.GameServer.SendResponse(ctx.ClientData, 2409, ctx.UserID, ctx.SeqID, body)

	newPet := userdb.Pet{
		ID:        bossID,
		CatchTime: int(newCatchTime),
		Level:     catchLevel,
		DV:        randomDV,
		Nature:    randomNature,
		Exp:       0,
		Name:      "",
		Shiny:     caughtShiny, // 与战斗敌方一致：野生=槽位是否异色，BOSS=1
	}
	// 野外/副本捕捉的精灵默认不带特性；后续需通过“特性开启芯片”等道具获得特性
	// 背包已满 6 只则放入仓库，否则放入背包（与 SPT 奖励精灵一致）
	if len(user.Pets) >= 6 {
		if user.StoragePets == nil {
			user.StoragePets = []userdb.Pet{}
		}
		user.StoragePets = append(user.StoragePets, newPet)
		logger.Info(fmt.Sprintf("[2409] 捕捉成功: PetID=%d Level=%d Capsule=%d CatchTime=%d -> 仓库(背包已满)", bossID, catchLevel, capsuleID, newCatchTime))
	} else {
		user.Pets = append(user.Pets, newPet)
		logger.Info(fmt.Sprintf("[2409] 捕捉成功: PetID=%d Level=%d Capsule=%d CatchTime=%d -> 背包", bossID, catchLevel, capsuleID, newCatchTime))
	}

	// 精灵太空站(9515) 布鲁：记录今日已捕捉
	if battleMapID == 9515 && bossID == 108 {
		today := time.Now().Unix() / 86400
		if user.DailyBossCaptureMap == nil {
			user.DailyBossCaptureMap = make(map[string]int64)
		}
		user.DailyBossCaptureMap["9515:108"] = today
	}

	// 战毕将当前精灵 HP 写回，再清理战斗状态
	ctx.GameServer.BattleMu.Lock()
	if b := ctx.GameServer.BattleStates[ctx.UserID]; b != nil {
		syncBattleHPToUser(ctx.GameServer, ctx.UserID, b)
	}
	delete(ctx.GameServer.BattleStates, ctx.UserID)
	ctx.GameServer.BattleMu.Unlock()

	// 结束战斗
	overBody := buildFightOverInfo(0, uint32(ctx.UserID))
	ctx.GameServer.SendResponse(ctx.ClientData, 2506, ctx.UserID, ctx.SeqID, overBody)
	pushMapOgreListAfterFightOver(ctx)

	// 保存数据
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}
}

// ==================== NONO系统命令处理器 ====================

// registerNonoHandlers 注册NONO系统命令处理器
func registerNonoHandlers(gs *gameserver.GameServer) {
	gs.RegisterCommandHandler(9001, handleNonoOpen)          // 开启NONO
	gs.RegisterCommandHandler(9002, handleNonoChangeName)    // 修改NONO名称
	gs.RegisterCommandHandler(9003, handleNonoInfo)          // NONO信息
	gs.RegisterCommandHandler(9004, handleNonoChipMixture)   // NONO芯片合成
	gs.RegisterCommandHandler(9007, handleNonoCure)          // NONO治疗
	gs.RegisterCommandHandler(9008, handleNonoExpadm)        // NONO经验管理
	gs.RegisterCommandHandler(9010, handleNonoImplementTool) // 使用NONO道具/芯片
	gs.RegisterCommandHandler(9012, handleNonoChangeColor)   // 修改NONO颜色
	gs.RegisterCommandHandler(9013, handleNonoPlay)          // NONO玩耍
	gs.RegisterCommandHandler(9014, handleNonoCloseOpen)     // NONO开关
	gs.RegisterCommandHandler(9015, handleNonoExeList)       // NONO执行列表
	gs.RegisterCommandHandler(9016, handleNonoCharge)        // NONO充电
	gs.RegisterCommandHandler(9017, handleNonoStartExe)      // 开始执行
	gs.RegisterCommandHandler(9018, handleNonoEndExe)        // 结束执行
	gs.RegisterCommandHandler(9019, handleNonoFollowOrHoom)  // 跟随或回家
	gs.RegisterCommandHandler(9020, handleNonoOpenSuper)     // 开启超级NONO
	gs.RegisterCommandHandler(9021, handleNonoHelpExp)       // NONO帮助经验
	gs.RegisterCommandHandler(9022, handleNonoMateChange)    // NONO心情变化
	gs.RegisterCommandHandler(9023, handleNonoGetChip)       // 获取芯片
	gs.RegisterCommandHandler(9024, handleNonoAddEnergyMate) // 增加能量心情
	gs.RegisterCommandHandler(9025, handleGetDiamond)        // 获取钻石
	gs.RegisterCommandHandler(9026, handleNonoAddExp)        // 增加NONO经验
	gs.RegisterCommandHandler(9027, handleNonoIsInfo)        // NONO是否有信息
	gs.RegisterCommandHandler(80001, handleNieoLogin)        // 超能NONO登录
}

// handleNonoOpen CMD 9001 开启NONO
// 仅开启普通 NoNo（HasNono=1）。只有开通超能 NoNo（9020/80001）才设置 SuperNono=1 并解锁全部功能
func handleNonoOpen(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	user.Nono.HasNono = 1
	// 不在此处设置 SuperNono，普通 NoNo 需用芯片激活功能；超能 NoNo 通过 9020 或 80001 开通
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}
	ctx.GameServer.SendResponse(ctx.ClientData, 9001, ctx.UserID, ctx.SeqID, []byte{})
}

// handleNonoChangeName CMD 9002 修改NONO名称
func handleNonoChangeName(ctx *gameserver.HandlerContext) {
	var name string
	if len(ctx.Body) >= 16 {
		name = string(ctx.Body[:16])
		// 移除末尾的null字符
		for i := len(name) - 1; i >= 0 && name[i] == 0; i-- {
			name = name[:i]
		}
	}
	if name == "" {
		name = "NoNo"
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	user.Nono.Nick = name
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}

	body := make([]byte, 16)
	copy(body, []byte(name))
	ctx.GameServer.SendResponse(ctx.ClientData, 9002, ctx.UserID, ctx.SeqID, body)
}

// handleNonoChipMixture CMD 9004 NONO芯片合成
func handleNonoChipMixture(ctx *gameserver.HandlerContext) {
	ctx.GameServer.SendResponse(ctx.ClientData, 9004, ctx.UserID, ctx.SeqID, []byte{})
}

// handleNonoCure CMD 9007 NONO治疗
// handleNonoCure CMD 9007 超能Nono精灵治疗：恢复背包内所有精灵 HP 和所有技能 PP 满状态
func handleNonoCure(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	for i := range user.Pets {
		user.Pets[i].CurrentHP = 0
		user.Pets[i].SkillPP = nil
	}
	_ = gamepets.GetInstance()
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}
	ctx.GameServer.SendResponse(ctx.ClientData, 9007, ctx.UserID, ctx.SeqID, []byte{})
}

// handlePetCureAll CMD 2306 精灵恢复（全部）：恢复背包内所有精灵 HP/PP 满状态（客户端 Nono 精灵治疗 / cureAll 发送）
func handlePetCureAll(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	for i := range user.Pets {
		user.Pets[i].CurrentHP = 0
		user.Pets[i].SkillPP = nil
	}
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}
	ctx.GameServer.SendResponse(ctx.ClientData, 2306, ctx.UserID, ctx.SeqID, []byte{})
}

// handlePetOneCure CMD 2310 精灵恢复（单只）：恢复选中精灵 HP 和所有技能 PP 满状态；请求 body: catchTime(4)，响应: catchTime(4)
// 行为与 2306 一致，但只作用于一只；CurrentHP 写 0 / SkillPP 置 nil 表示“满血满 PP”，与战斗结束/全体恢复逻辑保持一致。
func handlePetOneCure(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	var catchTime uint32
	if len(ctx.Body) >= 4 {
		catchTime = binary.BigEndian.Uint32(ctx.Body[0:4])
	}
	idx := -1
	for i := range user.Pets {
		if user.Pets[i].CatchTime == int(catchTime) {
			idx = i
			break
		}
	}
	if idx < 0 {
		// 未找到这只精灵：按 Lua 行为返回 4 字节 0，避免客户端卡死
		ctx.GameServer.SendResponse(ctx.ClientData, 2310, ctx.UserID, ctx.SeqID, []byte{0, 0, 0, 0})
		return
	}
	// 约定：CurrentHP=0 & SkillPP=nil 表示“满血满 PP”，与 2306/9007 相同
	user.Pets[idx].CurrentHP = 0
	user.Pets[idx].SkillPP = nil
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}
	// 回包带回 catchTime，客户端据此更新本地 PetInfo（hp/maxHp、pp）并派发 CURE_ONE_COMPLETE 事件
	resp := make([]byte, 4)
	binary.BigEndian.PutUint32(resp, catchTime)
	ctx.GameServer.SendResponse(ctx.ClientData, 2310, ctx.UserID, ctx.SeqID, resp)
}

// handleNonoExpadm CMD 9008 NONO经验管理
func handleNonoExpadm(ctx *gameserver.HandlerContext) {
	ctx.GameServer.SendResponse(ctx.ClientData, 9008, ctx.UserID, ctx.SeqID, []byte{})
}

// handleNonoImplementTool CMD 9010 使用NONO道具/芯片
func handleNonoImplementTool(ctx *gameserver.HandlerContext) {
	var itemID uint32
	if len(ctx.Body) >= 4 {
		itemID = binary.BigEndian.Uint32(ctx.Body[0:4])
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)

	// 消耗背包中的道具
	itemKey := strconv.FormatUint(uint64(itemID), 10)
	if item, hasItem := user.Items[itemKey]; hasItem {
		item.Count--
		if item.Count <= 0 {
			delete(user.Items, itemKey)
		} else {
			user.Items[itemKey] = item
		}
	}

	// 芯片（700001-700060）使用后永久解锁，持久化到 Nono.Func 位图
	if itemID >= 700001 && itemID <= 700060 {
		idx := int(itemID - 700001)
		for len(user.Nono.Func) < 20 {
			user.Nono.Func = append(user.Nono.Func, 0)
		}
		if idx < 160 {
			byteIdx := idx / 8
			bitIdx := uint(idx % 8)
			user.Nono.Func[byteIdx] |= 1 << bitIdx
		}
	}

	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}

	// 响应: userId(4) + itemId(4) + power(4, *1000) + ai(2) + mate(4, *1000) + iq(4)
	body := make([]byte, 22)
	binary.BigEndian.PutUint32(body[0:4], uint32(ctx.UserID))
	binary.BigEndian.PutUint32(body[4:8], itemID)
	binary.BigEndian.PutUint32(body[8:12], uint32(user.Nono.Power*1000))
	binary.BigEndian.PutUint16(body[12:14], uint16(user.Nono.AI))
	binary.BigEndian.PutUint32(body[14:18], uint32(user.Nono.Mate*1000))
	binary.BigEndian.PutUint32(body[18:22], uint32(user.Nono.IQ))
	ctx.GameServer.SendResponse(ctx.ClientData, 9010, ctx.UserID, ctx.SeqID, body)
}

// handleNonoChangeColor CMD 9012 修改NONO颜色
func handleNonoChangeColor(ctx *gameserver.HandlerContext) {
	var color uint32
	if len(ctx.Body) >= 4 {
		color = binary.BigEndian.Uint32(ctx.Body[0:4])
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	user.Nono.Color = int(color)
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}
	ctx.GameServer.SendResponse(ctx.ClientData, 9012, ctx.UserID, ctx.SeqID, []byte{})
}

// handleNonoPlay CMD 9013 NONO玩耍
func handleNonoPlay(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	// 玩耍增加心情
	if user.Nono.Mate+5000 > 100000 {
		user.Nono.Mate = 100000
	} else {
		user.Nono.Mate += 5000
	}
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}

	// 响应: result(4) + itemId(4) + power(4) + ai(2) + mate(4) + iq(4)
	body := make([]byte, 22)
	binary.BigEndian.PutUint32(body[0:4], 0)
	binary.BigEndian.PutUint32(body[4:8], 0)
	binary.BigEndian.PutUint32(body[8:12], uint32(user.Nono.Power))
	binary.BigEndian.PutUint16(body[12:14], uint16(user.Nono.AI))
	binary.BigEndian.PutUint32(body[14:18], uint32(user.Nono.Mate))
	binary.BigEndian.PutUint32(body[18:22], uint32(user.Nono.IQ))
	ctx.GameServer.SendResponse(ctx.ClientData, 9013, ctx.UserID, ctx.SeqID, body)
}

// handleNonoCloseOpen CMD 9014 NONO开关
func handleNonoCloseOpen(ctx *gameserver.HandlerContext) {
	var action uint32
	if len(ctx.Body) >= 4 {
		action = binary.BigEndian.Uint32(ctx.Body[0:4])
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	user.Nono.State = int(action)
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}
	ctx.GameServer.SendResponse(ctx.ClientData, 9014, ctx.UserID, ctx.SeqID, []byte{})
}

// handleNonoExeList CMD 9015 NONO执行列表
func handleNonoExeList(ctx *gameserver.HandlerContext) {
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body[0:4], 0) // count = 0
	ctx.GameServer.SendResponse(ctx.ClientData, 9015, ctx.UserID, ctx.SeqID, body)
}

// handleNonoCharge CMD 9016 NONO充电
func handleNonoCharge(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	if user.Nono.SuperEnergy+1000 > 99999 {
		user.Nono.SuperEnergy = 99999
	} else {
		user.Nono.SuperEnergy += 1000
	}
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}
	ctx.GameServer.SendResponse(ctx.ClientData, 9016, ctx.UserID, ctx.SeqID, []byte{})
}

// handleNonoStartExe CMD 9017 开始执行
func handleNonoStartExe(ctx *gameserver.HandlerContext) {
	ctx.GameServer.SendResponse(ctx.ClientData, 9017, ctx.UserID, ctx.SeqID, []byte{})
}

// handleNonoEndExe CMD 9018 结束执行
func handleNonoEndExe(ctx *gameserver.HandlerContext) {
	ctx.GameServer.SendResponse(ctx.ClientData, 9018, ctx.UserID, ctx.SeqID, []byte{})
}

// handleNonoFollowOrHoom CMD 9019 跟随或回家
func handleNonoFollowOrHoom(ctx *gameserver.HandlerContext) {
	var action uint32
	if len(ctx.Body) >= 4 {
		action = binary.BigEndian.Uint32(ctx.Body[0:4])
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)

	var body []byte
	if action == 1 {
		// 跟随前确保形态根据超能等级更新，避免客户端把超能 NoNo 当普通 NoNo
		if user.Nono.SuperLevel > 0 {
			updateSuperNonoTypeByLevel(user)
		}
		// 记录跟随状态：state 的第 1 位表示“是否跟随”（与前端 NonoInfo.state[1] 对齐）
		user.Nono.State |= 1 << 1
		user.Nono.IsFollowing = true
		if ctx.GameServer.UserDB != nil {
			ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
		}

		// 跟随: 返回完整NONO信息 (36 bytes)
		body = make([]byte, 36)
		binary.BigEndian.PutUint32(body[0:4], uint32(ctx.UserID))
		binary.BigEndian.PutUint32(body[4:8], 0)  // flag=0
		binary.BigEndian.PutUint32(body[8:12], 1) // state=1 跟随状态
		nickBytes := []byte(user.Nono.Nick)
		if len(nickBytes) > 16 {
			nickBytes = nickBytes[:16]
		}
		copy(body[12:28], nickBytes)
		binary.BigEndian.PutUint32(body[28:32], uint32(user.Nono.Color))
		binary.BigEndian.PutUint32(body[32:36], uint32(user.Nono.Power))
		// 自己收到 9019，更新本地跟随状态
		ctx.GameServer.SendResponse(ctx.ClientData, 9019, ctx.UserID, ctx.SeqID, body)
		// 同地图其他玩家也要看到跟随状态变化（显示/更新该玩家的 NoNo）
		ctx.GameServer.BroadcastToMap(user.MapID, ctx.UserID, 9019, body)

		// 基地点击「跟随我」后补发 9003，让客户端拿到完整 NonoInfo（含 SuperNono/funcBits），避免被当成普通 NoNo 丢失超能功能
		nonoBody := buildNonoInfo90(ctx.UserID, user)
		ctx.GameServer.SendResponse(ctx.ClientData, 9003, ctx.UserID, 0, nonoBody)
		return
	}

	// 回家：更新跟随状态并发送 9019+9003，确保客户端仍保留超能 NONO 功能
	user.Nono.State &^= 1 << 1
	user.Nono.IsFollowing = false
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}

	// 回家: 只返回12 bytes 的 9019
	body = make([]byte, 12)
	binary.BigEndian.PutUint32(body[0:4], uint32(ctx.UserID))
	binary.BigEndian.PutUint32(body[4:8], 0)  // flag=0
	binary.BigEndian.PutUint32(body[8:12], 0) // state=0 已回家
	// 自己收到 9019，更新本地状态
	ctx.GameServer.SendResponse(ctx.ClientData, 9019, ctx.UserID, ctx.SeqID, body)
	// 广播给同地图其他玩家，让他们隐藏该玩家的 NoNo
	ctx.GameServer.BroadcastToMap(user.MapID, ctx.UserID, 9019, body)

	// 再补发一包 9003，刷新前端 NONO 信息，防止回基地后被降级为普通 NoNo
	nonoBody := buildNonoInfo90(ctx.UserID, user)
	ctx.GameServer.SendResponse(ctx.ClientData, 9003, ctx.UserID, 0, nonoBody)
}

// handleNonoOpenSuper CMD 9020 开启超级NONO
func handleNonoOpenSuper(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	user.Nono.SuperNono = 1
	if user.Nono.SuperLevel < 1 {
		user.Nono.SuperLevel = 1
	}
	if user.Nono.SuperStage < 1 {
		user.Nono.SuperStage = 1
	}
	// 开启超能 NONO 时，根据超能等级更新形态
	updateSuperNonoTypeByLevel(user)
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}
	ctx.GameServer.SendResponse(ctx.ClientData, 9020, ctx.UserID, ctx.SeqID, []byte{})
}

// handleNonoHelpExp CMD 9021 NONO帮助经验（NoNo 照顾精灵积累的经验，可在发明室经验接收器领取）
func handleNonoHelpExp(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	if user.ExpPool < 0 {
		user.ExpPool = 0
	}
	const addPerCall = 3000
	user.ExpPool += addPerCall
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}
	ctx.GameServer.SendResponse(ctx.ClientData, 9021, ctx.UserID, ctx.SeqID, []byte{})
	logger.Info(fmt.Sprintf("[9021] NONO帮助经验: 经验池 +%d 当前=%d", addPerCall, user.ExpPool))
}

// handleNonoMateChange CMD 9022 NONO心情变化
func handleNonoMateChange(ctx *gameserver.HandlerContext) {
	ctx.GameServer.SendResponse(ctx.ClientData, 9022, ctx.UserID, ctx.SeqID, []byte{})
}

// superNonoChipList 发明室「超能NoNo芯片领取」面板顺序：每页 3 个，共 4 页，按 (page-1)*3+slot 取
// 第 1 页：时空穿梭(700019)、经验加成(700007)、超能雷达(700009)，其余按常见芯片顺序
var superNonoChipList = []uint32{
	700019, 700007, 700009, // 第1页
	700001, 700002, 700003, // 第2页
	700004, 700005, 700006, // 第3页
	700008, 700010, 700011, // 第4页
}

// handleNonoGetChip CMD 9023 获取芯片（发明室超能NoNo芯片领取 / 任务送芯片）
// 请求: 4 字节时 body[0:4]=chipType（或 itemId-700000）；20 字节时 body[12:16]=页(1-based)，body[16:20]=槽位(0-based) 或全局序号
// 响应: 0(4)*3 + len(4) + [itemId(4)+count(4)]... 客户端 onGetChip 按此解析
func handleNonoGetChip(ctx *gameserver.HandlerContext) {
	var chipItemID uint32
	if len(ctx.Body) >= 20 {
		page := binary.BigEndian.Uint32(ctx.Body[12:16])
		slotOrIndex := binary.BigEndian.Uint32(ctx.Body[16:20])
		// 发明室面板：每页 3 个，全局序号 = (page-1)*3 + slot（点击第几页第几个）
		globalIndex := (page-1)*3 + slotOrIndex
		if globalIndex < uint32(len(superNonoChipList)) {
			chipItemID = superNonoChipList[globalIndex]
		} else if slotOrIndex >= 1 && slotOrIndex <= 60 {
			chipItemID = 700000 + slotOrIndex
		} else {
			chipItemID = superNonoChipList[0]
		}
	} else if len(ctx.Body) >= 4 {
		chipType := binary.BigEndian.Uint32(ctx.Body[0:4])
		if chipType >= 1 && chipType <= 60 {
			chipItemID = 700000 + chipType
		} else {
			chipItemID = 700005 // 任务送跟随模式芯片等默认
		}
	} else {
		chipItemID = 700005
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	// 添加到背包
	itemKey := strconv.FormatUint(uint64(chipItemID), 10)
	if item, hasItem := user.Items[itemKey]; hasItem {
		item.Count++
		user.Items[itemKey] = item
	} else {
		user.Items[itemKey] = userdb.Item{
			Count:      1,
			ExpireTime: 0x057E40,
		}
	}

	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}

	// 响应: 0(4)*3 + len(4) + [itemId(4) + count(4)] 与客户端 onGetChip 一致（itemId 为 70000x）
	body := make([]byte, 24)
	binary.BigEndian.PutUint32(body[0:4], 0)
	binary.BigEndian.PutUint32(body[4:8], 0)
	binary.BigEndian.PutUint32(body[8:12], 0)
	binary.BigEndian.PutUint32(body[12:16], 1)
	binary.BigEndian.PutUint32(body[16:20], uint32(chipItemID))
	binary.BigEndian.PutUint32(body[20:24], 1)
	ctx.GameServer.SendResponse(ctx.ClientData, 9023, ctx.UserID, ctx.SeqID, body)
}

// handleNonoAddEnergyMate CMD 9024 增加能量心情
func handleNonoAddEnergyMate(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	if user.Nono.Power+10000 > 100000 {
		user.Nono.Power = 100000
	} else {
		user.Nono.Power += 10000
	}
	if user.Nono.Mate+10000 > 100000 {
		user.Nono.Mate = 100000
	} else {
		user.Nono.Mate += 10000
	}
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}
	ctx.GameServer.SendResponse(ctx.ClientData, 9024, ctx.UserID, ctx.SeqID, []byte{})
}

// handleGetDiamond CMD 9025 获取钻石
func handleGetDiamond(ctx *gameserver.HandlerContext) {
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body[0:4], 9999) // 钻石数量
	ctx.GameServer.SendResponse(ctx.ClientData, 9025, ctx.UserID, ctx.SeqID, body)
}

// handleNonoAddExp CMD 9026 增加NONO经验
func handleNonoAddExp(ctx *gameserver.HandlerContext) {
	ctx.GameServer.SendResponse(ctx.ClientData, 9026, ctx.UserID, ctx.SeqID, []byte{})
}

// handleNonoIsInfo CMD 9027 NONO是否有信息
func handleNonoIsInfo(ctx *gameserver.HandlerContext) {
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body[0:4], 1) // 有NONO
	ctx.GameServer.SendResponse(ctx.ClientData, 9027, ctx.UserID, ctx.SeqID, body)
}

// handleNieoLogin CMD 80001 超能NONO登录/状态检查
func handleNieoLogin(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	currentTime := uint32(time.Now().Unix())

	// 检查是否需要激活超能NONO
	needActivate := false
	if user.Nono.SuperNono == 0 {
		needActivate = true
	} else if user.Nono.VipEndTime > 0 && user.Nono.VipEndTime < int64(currentTime) {
		needActivate = true
	}

	if needActivate {
		// 激活超能NONO（默认30天）
		durationDays := 30
		endTime := currentTime + uint32(durationDays*24*60*60)
		user.Nono.SuperNono = 1
		user.Nono.VipEndTime = int64(endTime)
		if user.Nono.SuperLevel < 1 {
			user.Nono.SuperLevel = 1
		}
		if user.Nono.SuperStage < 1 {
			user.Nono.SuperStage = 1
		}
		// 激活时根据超能等级更新形态
		updateSuperNonoTypeByLevel(user)
		if ctx.GameServer.UserDB != nil {
			ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
		}

		// 发送80002激活成功通知
		message := fmt.Sprintf("成功激活超能NONO！\n到期时间:%s", time.Unix(int64(endTime), 0).Format("2006-01-02"))
		msgBytes := []byte(message)
		notifyBody := make([]byte, 4+len(msgBytes))
		binary.BigEndian.PutUint32(notifyBody[0:4], uint32(len(msgBytes)))
		copy(notifyBody[4:], msgBytes)
		ctx.GameServer.SendResponse(ctx.ClientData, 80002, ctx.UserID, 0, notifyBody)

		// 发送VIP_CO (8006) 更新
		vipBody := make([]byte, 16)
		binary.BigEndian.PutUint32(vipBody[0:4], uint32(ctx.UserID))
		binary.BigEndian.PutUint32(vipBody[4:8], 2) // vipFlag=2 (激活超能NONO)
		binary.BigEndian.PutUint32(vipBody[8:12], uint32(user.Nono.AutoCharge))
		binary.BigEndian.PutUint32(vipBody[12:16], endTime)
		ctx.GameServer.SendResponse(ctx.ClientData, 8006, ctx.UserID, 0, vipBody)
	}

	// 发送80001状态响应
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body[0:4], 0) // status=0 正常/已激活
	ctx.GameServer.SendResponse(ctx.ClientData, 80001, ctx.UserID, ctx.SeqID, body)
}

// ==================== 物品/背包系统命令处理器 ====================

// registerItemHandlers 注册物品/背包系统命令处理器
func registerItemHandlers(gs *gameserver.GameServer) {
	gs.RegisterCommandHandler(2601, handleItemBuy)               // 购买物品
	gs.RegisterCommandHandler(2602, handleItemSale)              // 出售物品
	gs.RegisterCommandHandler(2604, handleChangeCloth)           // 更换服装
	gs.RegisterCommandHandler(2605, handleItemList)              // 物品列表
	gs.RegisterCommandHandler(2606, handleMultiItemBuy)          // 批量购买
	gs.RegisterCommandHandler(2607, handleItemExpend)            // 消耗物品
	gs.RegisterCommandHandler(2609, handleEquipUpdate)           // 装备升级
	gs.RegisterCommandHandler(2901, handleExchangeClothComplete) // 兑换服装完成
}

// ========= 服装判定（基于 items.xml 的 Sort=2） =========
//
// 旧逻辑只认 100000-199999，但很多套装部件（尤其 hand/waist）在 items.xml 中是 130xxxxxx 这类 7 位 ID，
// 仍属于服装（Sort=2）。若不写入 user.Clothes，客户端穿戴时会“不显示/会消失”。

type itemsXMLForClothes struct {
	Cats []struct {
		Items []struct {
			ID   int `xml:"ID,attr"`
			Sort int `xml:"Sort,attr"`
		} `xml:"Item"`
	} `xml:"Cat"`
}

var (
	clothesOnce sync.Once
	clothesSet  map[int]bool
)

func loadClothesSetOnce() {
	clothesOnce.Do(func() {
		clothesSet = make(map[int]bool, 4096)

		readAndParse := func(p string) bool {
			data, err := os.ReadFile(p)
			if err != nil {
				return false
			}
			var root itemsXMLForClothes
			if err := xml.Unmarshal(data, &root); err != nil {
				logger.Warning(fmt.Sprintf("[Clothes] 解析 items.xml 失败: %v", err))
				return false
			}
			cnt := 0
			for _, cat := range root.Cats {
				for _, it := range cat.Items {
					if it.ID <= 0 {
						continue
					}
					// Sort=2：服装（手/腰/脚/头/眼等部件都在此分类）
					if it.Sort == 2 {
						clothesSet[it.ID] = true
						cnt++
					}
				}
			}
			logger.Info(fmt.Sprintf("[Clothes] 已加载服装ID集合: count=%d path=%s", cnt, p))
			return cnt > 0
		}

		var candidates []string
		if exePath, err := os.Executable(); err == nil && exePath != "" {
			exeDir := filepath.Dir(exePath)
			candidates = append(candidates,
				filepath.Join(exeDir, "..", "xml", "items.xml"),
				filepath.Join(exeDir, "..", "..", "golang_version", "xml", "items.xml"),
				filepath.Join(exeDir, "golang_version", "xml", "items.xml"),
			)
		}
		candidates = append(candidates,
			filepath.Join("golang_version", "xml", "items.xml"),
			filepath.Join("xml", "items.xml"),
			filepath.Join("..", "golang_version", "xml", "items.xml"),
		)
		for _, c := range candidates {
			if readAndParse(c) {
				return
			}
		}
		logger.Warning("[Clothes] 未能加载 items.xml，服装判定将回退到旧ID范围(100000-199999)")
	})
}

func isClothItemID(itemID int) bool {
	// 优先按 items.xml Sort=2
	loadClothesSetOnce()
	if clothesSet != nil && clothesSet[itemID] {
		return true
	}
	// 兜底：老范围
	return itemID >= 100000 && itemID <= 199999
}

// addClothIfNeeded 若 itemID 为服装，则加入 user.Clothes，避免“我的服装/穿戴”中不显示或穿戴后丢失
func addClothIfNeeded(user *userdb.GameData, itemID int) {
	if user == nil || itemID <= 0 {
		return
	}
	if !isClothItemID(itemID) {
		return
	}
	for _, cid := range user.Clothes {
		if cid == itemID {
			return
		}
	}
	user.Clothes = append(user.Clothes, itemID)
}

// handleItemBuy CMD 2601 购买物品
func handleItemBuy(ctx *gameserver.HandlerContext) {
	var itemID, count uint32
	if len(ctx.Body) >= 8 {
		itemID = binary.BigEndian.Uint32(ctx.Body[0:4])
		count = binary.BigEndian.Uint32(ctx.Body[4:8])
	}
	if count == 0 {
		count = 1
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	itemKey := strconv.FormatUint(uint64(itemID), 10)

	// 检查唯一性（简化：如果已拥有唯一物品，返回错误）
	// 这里应该检查items.xml中的唯一性配置，暂时简化处理

	// 价格检查（简化：默认价格100）
	unitPrice := 100
	totalCost := unitPrice * int(count)

	if user.Coins < totalCost {
		// 返回错误码 103107 (赛尔豆余额不足)
		ctx.GameServer.SendResponse(ctx.ClientData, 2601, ctx.UserID, ctx.SeqID, []byte{})
		return
	}

	// 扣钱
	user.Coins -= totalCost

	// 添加物品
	if item, hasItem := user.Items[itemKey]; hasItem {
		item.Count += int(count)
		user.Items[itemKey] = item
	} else {
		user.Items[itemKey] = userdb.Item{
			Count:      int(count),
			ExpireTime: 0x057E40,
		}
	}
	// 服装/套装（100000-199999）需同时加入 Clothes，否则“我的服装”中不显示
	addClothIfNeeded(user, int(itemID))
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}
	// 响应: cash(4) + itemID(4) + itemNum(4) + itemLevel(4)
	body := make([]byte, 16)
	binary.BigEndian.PutUint32(body[0:4], uint32(user.Coins))
	binary.BigEndian.PutUint32(body[4:8], itemID)
	binary.BigEndian.PutUint32(body[8:12], count)
	binary.BigEndian.PutUint32(body[12:16], 0)
	ctx.GameServer.SendResponse(ctx.ClientData, 2601, ctx.UserID, ctx.SeqID, body)
}

// handleItemSale CMD 2602 出售物品
func handleItemSale(ctx *gameserver.HandlerContext) {
	var itemID, count uint32
	if len(ctx.Body) >= 8 {
		itemID = binary.BigEndian.Uint32(ctx.Body[0:4])
		count = binary.BigEndian.Uint32(ctx.Body[4:8])
	}
	if count == 0 {
		count = 1
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	itemKey := strconv.FormatUint(uint64(itemID), 10)

	if item, hasItem := user.Items[itemKey]; hasItem {
		item.Count -= int(count)
		if item.Count <= 0 {
			delete(user.Items, itemKey)
		} else {
			user.Items[itemKey] = item
		}
		// 给予金币（简化：默认价格50）
		user.Coins += int(count) * 50
	}

	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}

	ctx.GameServer.SendResponse(ctx.ClientData, 2602, ctx.UserID, ctx.SeqID, []byte{})
}

// handleChangeCloth CMD 2604 更换服装
func handleChangeCloth(ctx *gameserver.HandlerContext) {
	var clothCount uint32
	if len(ctx.Body) >= 4 {
		clothCount = binary.BigEndian.Uint32(ctx.Body[0:4])
	}

	clothIDs := make([]uint32, 0, clothCount)
	for i := uint32(0); i < clothCount && len(ctx.Body) >= int(4+4*(i+1)); i++ {
		clothID := binary.BigEndian.Uint32(ctx.Body[4+i*4 : 8+i*4])
		clothIDs = append(clothIDs, clothID)
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	user.Clothes = make([]int, len(clothIDs))
	for i, id := range clothIDs {
		user.Clothes[i] = int(id)
	}

	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}

	// 响应: userID(4) + clothCount(4) + [clothId(4) + clothType(4)]...
	body := make([]byte, 8+len(clothIDs)*8)
	binary.BigEndian.PutUint32(body[0:4], uint32(ctx.UserID))
	binary.BigEndian.PutUint32(body[4:8], uint32(len(clothIDs)))
	off := 8
	for _, clothID := range clothIDs {
		binary.BigEndian.PutUint32(body[off:off+4], clothID)
		binary.BigEndian.PutUint32(body[off+4:off+8], 0) // clothType
		off += 8
	}
	ctx.GameServer.SendResponse(ctx.ClientData, 2604, ctx.UserID, ctx.SeqID, body)

	// 更换服装后通知同地图玩家刷新外观（客户端通常依赖 2003 的 peopleInfo.clothes）
	if user.MapID > 0 {
		listBody := buildMapPlayerListForMap(ctx.GameServer, user.MapID)
		ctx.GameServer.BroadcastToMap(user.MapID, 0, 2003, listBody)
	}
}

// handleMultiItemBuy CMD 2606 批量购买
func handleMultiItemBuy(ctx *gameserver.HandlerContext) {
	var itemCount uint32
	if len(ctx.Body) >= 4 {
		itemCount = binary.BigEndian.Uint32(ctx.Body[0:4])
	}

	itemIDs := make([]uint32, 0, itemCount)
	for i := uint32(0); i < itemCount && len(ctx.Body) >= int(4+4*(i+1)); i++ {
		itemID := binary.BigEndian.Uint32(ctx.Body[4+i*4 : 8+i*4])
		itemIDs = append(itemIDs, itemID)
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)

	// 计算总价
	totalCost := 0
	validItems := make([]uint32, 0)
	for _, itemID := range itemIDs {
		itemKey := strconv.FormatUint(uint64(itemID), 10)
		// 唯一性检查（简化）
		if _, hasItem := user.Items[itemKey]; !hasItem {
			totalCost += 100 // 默认价格
			validItems = append(validItems, itemID)
		}
	}

	if user.Coins < totalCost {
		// 返回错误码
		body := make([]byte, 8)
		binary.BigEndian.PutUint32(body[0:4], 10016) // 错误码
		binary.BigEndian.PutUint32(body[4:8], uint32(user.Coins))
		ctx.GameServer.SendResponse(ctx.ClientData, 2606, ctx.UserID, ctx.SeqID, body)
		return
	}

	// 扣钱
	user.Coins -= totalCost

	// 添加物品
	for _, itemID := range validItems {
		itemKey := strconv.FormatUint(uint64(itemID), 10)
		if item, hasItem := user.Items[itemKey]; hasItem {
			item.Count++
			user.Items[itemKey] = item
		} else {
			user.Items[itemKey] = userdb.Item{
				Count:      1,
				ExpireTime: 0x057E40,
			}
		}
		// 服装/套装（100000-199999）需同时加入 Clothes
		addClothIfNeeded(user, int(itemID))
	}
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}
	// 响应: result(4) + remainCoins(4)
	body := make([]byte, 8)
	binary.BigEndian.PutUint32(body[0:4], 0) // result=0 成功
	binary.BigEndian.PutUint32(body[4:8], uint32(user.Coins))
	ctx.GameServer.SendResponse(ctx.ClientData, 2606, ctx.UserID, ctx.SeqID, body)
}

// handleItemExpend CMD 2607 消耗物品
func handleItemExpend(ctx *gameserver.HandlerContext) {
	var itemID, count uint32
	if len(ctx.Body) >= 8 {
		itemID = binary.BigEndian.Uint32(ctx.Body[0:4])
		count = binary.BigEndian.Uint32(ctx.Body[4:8])
	}
	if count == 0 {
		count = 1
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	itemKey := strconv.FormatUint(uint64(itemID), 10)

	if item, hasItem := user.Items[itemKey]; hasItem {
		item.Count -= int(count)
		if item.Count <= 0 {
			delete(user.Items, itemKey)
		} else {
			user.Items[itemKey] = item
		}
	}

	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}

	ctx.GameServer.SendResponse(ctx.ClientData, 2607, ctx.UserID, ctx.SeqID, []byte{})
}

// handleEquipUpdate CMD 2609 装备升级
func handleEquipUpdate(ctx *gameserver.HandlerContext) {
	ctx.GameServer.SendResponse(ctx.ClientData, 2609, ctx.UserID, ctx.SeqID, []byte{})
}

// handleExchangeClothComplete CMD 2901 兑换服装完成
func handleExchangeClothComplete(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	// 解析 exchangeID(4) + itemId(4)，将兑换得到的服装加入 Clothes
	itemID := 0
	if len(ctx.Body) >= 4 {
		_ = binary.BigEndian.Uint32(ctx.Body[0:4]) // exchangeID
	}
	if len(ctx.Body) >= 8 {
		itemID = int(binary.BigEndian.Uint32(ctx.Body[4:8]))
	}
	if itemID > 0 {
		addClothIfNeeded(user, itemID)
		// 若为服装则也记入 Items 数量（部分客户端按物品显示）
		if itemID >= 100000 && itemID <= 199999 {
			itemKey := strconv.Itoa(itemID)
			if it, has := user.Items[itemKey]; has {
				it.Count++
				user.Items[itemKey] = it
			} else {
				user.Items[itemKey] = userdb.Item{Count: 1, ExpireTime: 0x057E40}
			}
		}
		if ctx.GameServer.UserDB != nil {
			ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
		}
	}
	// 响应: ret(4) + itemId(4) + count(4)
	body := make([]byte, 12)
	binary.BigEndian.PutUint32(body[0:4], 0)
	binary.BigEndian.PutUint32(body[4:8], uint32(itemID))
	binary.BigEndian.PutUint32(body[8:12], 1)
	ctx.GameServer.SendResponse(ctx.ClientData, 2901, ctx.UserID, ctx.SeqID, body)
}

// handleEscapeFight CMD 2410 逃跑（最小可用版本）
// 对齐 Lua: fight_handlers.handleEscapeFight
// PvP 对战（邀请/精灵王之战/精灵大乱斗）双方不准逃跑，收到 2410 时返回失败。
func handleEscapeFight(ctx *gameserver.HandlerContext) {
	ctx.GameServer.BattleMu.RLock()
	battle, exists := ctx.GameServer.BattleStates[ctx.UserID]
	active := exists && battle != nil && battle.IsActive
	isPvP := active && battle.OpponentUserID != 0
	ctx.GameServer.BattleMu.RUnlock()

	if isPvP {
		// PvP 不准逃跑：返回 result=0 表示失败
		body := make([]byte, 4)
		binary.BigEndian.PutUint32(body[0:4], 0)
		ctx.GameServer.SendResponse(ctx.ClientData, 2410, ctx.UserID, ctx.SeqID, body)
		logger.Info(fmt.Sprintf("[2410] PvP 对战不允许逃跑: UID=%d", ctx.UserID))
		return
	}

	// 响应 2410: result(4) = 1（成功）
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body[0:4], 1)
	ctx.GameServer.SendResponse(ctx.ClientData, 2410, ctx.UserID, ctx.SeqID, body)

	if !active {
		return
	}

	// 在逃跑结束战斗前，补一发 NOTE_UPDATE_PROP(2508)，用当前出战精灵驱动前端结算面板，
	// 避免客户端沿用上一场战斗的 2508 导致“场上鳞甲龙鱼，结算显示雷伊”等错位。
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	petMgr := gamepets.GetInstance()

	ctx.GameServer.BattleMu.RLock()
	battleForPlayer := ctx.GameServer.BattleStates[ctx.UserID]
	ctx.GameServer.BattleMu.RUnlock()
	if battleForPlayer != nil && len(user.Pets) > 0 {
		var activePet *userdb.Pet
		// 1) 优先按 ActivePetCatchTime 精确匹配
		if battleForPlayer.ActivePetCatchTime != 0 {
			for i := range user.Pets {
				if user.Pets[i].CatchTime == battleForPlayer.ActivePetCatchTime {
					activePet = &user.Pets[i]
					break
				}
			}
		}
		// 2) 回退到 ActivePetIndex
		if activePet == nil {
			activeIdx := 0
			if battleForPlayer.ActivePetIndex >= 0 && battleForPlayer.ActivePetIndex < len(user.Pets) {
				activeIdx = battleForPlayer.ActivePetIndex
			}
			if activeIdx >= 0 && activeIdx < len(user.Pets) {
				activePet = &user.Pets[activeIdx]
			}
		}
		// 3) 最后兜底：用背包第一只
		if activePet == nil {
			activePet = &user.Pets[0]
		}

		if activePet != nil {
			ev := gamepets.ClampAndCapEV(activePet.GetEVStats())
			stats := petMgr.GetStats(activePet.ID, activePet.Level, activePet.DV, ev, activePet.Nature)
			propBody := buildNoteUpdateProp(
				uint32(activePet.CatchTime),
				activePet.ID,
				activePet.Level,
				activePet.Exp,
				stats.MaxHP, stats.Attack, stats.Defence, stats.SpAtk, stats.SpDef, stats.Speed,
				ev,
			)
			ctx.GameServer.SendResponse(ctx.ClientData, 2508, ctx.UserID, ctx.SeqID, propBody)
		}
	}

	// 结束战斗：PvP 逃跑时对方获胜，需向双方发送 2506 且 winnerID=对方
	var opponentUID int64 = 0
	if battleForPlayer != nil {
		opponentUID = battleForPlayer.OpponentUserID
	}
	winnerIDForEscape := uint32(0)
	if opponentUID != 0 {
		winnerIDForEscape = uint32(opponentUID) // PvP：逃跑视为对方获胜
		// 精灵王之战逃跑：若当前 BattleState 标记为精灵王之战，则对方胜场 +1
		if battleForPlayer != nil && battleForPlayer.IsPetKing {
			if u := ctx.GameServer.GetOrCreateUser(opponentUID); u != nil {
				u.MonKingWin++
				if ctx.GameServer.UserDB != nil {
					ctx.GameServer.UserDB.SaveGameData(opponentUID, u)
				}
			}
		}
	}
	overBody := buildFightOverInfo(0, winnerIDForEscape)
	ctx.GameServer.SendResponse(ctx.ClientData, 2506, ctx.UserID, ctx.SeqID, overBody)
	pushMapOgreListAfterFightOver(ctx)

	// 战毕将当前精灵 HP 写回，再清理战斗状态
	ctx.GameServer.BattleMu.Lock()
	if battle := ctx.GameServer.BattleStates[ctx.UserID]; battle != nil {
		syncBattleHPToUser(ctx.GameServer, ctx.UserID, battle)
	}
	if opponentUID != 0 {
		if ob := ctx.GameServer.BattleStates[opponentUID]; ob != nil {
			syncBattleHPToUser(ctx.GameServer, opponentUID, ob)
		}
		delete(ctx.GameServer.BattleStates, opponentUID)
	}
	delete(ctx.GameServer.BattleStates, ctx.UserID)
	ctx.GameServer.BattleMu.Unlock()

	// 精灵大乱斗逃跑：对方获胜并获得 10000 可分配经验，清理临时精灵列表，同时记录胜场
	if battleForPlayer != nil && battleForPlayer.IsGrandMelee && opponentUID != 0 {
		if u := ctx.GameServer.GetOrCreateUser(opponentUID); u != nil {
			u.ExpPool += grandMeleeExpReward
			u.MessWin++
			if ctx.GameServer.UserDB != nil {
				ctx.GameServer.UserDB.SaveGameData(opponentUID, u)
			}
			if winnerClient := ctx.GameServer.GetClientByUserID(opponentUID); winnerClient != nil {
				body8004 := buildBossMonster8004Body(0, 0, 0, 3, uint32(grandMeleeExpReward))
				ctx.GameServer.SendResponse(winnerClient, 8004, opponentUID, 0, body8004)
			}
			logger.Info(fmt.Sprintf("[2431] 精灵大乱斗逃跑: 胜方 UID=%d 获得 %d 可分配经验", opponentUID, grandMeleeExpReward))
		}
		delete(ctx.GameServer.GrandMeleePets, ctx.UserID)
		delete(ctx.GameServer.GrandMeleePets, opponentUID)
	}

	// PvP：向对方也发送 2506 并推送 2004，否则对方对战不退出
	if opponentUID != 0 {
		if otherClient := ctx.GameServer.GetClientByUserID(opponentUID); otherClient != nil {
			ctx.GameServer.SendResponse(otherClient, 2506, opponentUID, 0, overBody)
			userOpp := ctx.GameServer.GetOrCreateUser(opponentUID)
			mapID := userOpp.MapID
			if mapID == 0 {
				mapID = 1
			}
			ctx.GameServer.SetOgreFightEndTime(opponentUID)
			ctx.GameServer.ClearPlayerOgreSlots(opponentUID, mapID)
			ogreBody := ctx.GameServer.BuildMapOgreListFromSlots(nil)
			ctx.GameServer.SendResponse(otherClient, 2004, opponentUID, 0, ogreBody)
		}
	}

	user = ctx.GameServer.GetOrCreateUser(ctx.UserID)
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}
}

// handlePeopleWalk CMD 2101 人物移动
// 对齐 Lua: map_handlers.handlePeopleWalk
// 请求: walkType(4) + x(4) + y(4) + amfLen(4) + amfData
// 响应: walkType(4) + userID(4) + x(4) + y(4) + amfLen(4) + amfData
func handlePeopleWalk(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)

	var walkType, x, y, amfLen uint32
	var amfData []byte

	if len(ctx.Body) >= 16 {
		walkType = binary.BigEndian.Uint32(ctx.Body[0:4])
		x = binary.BigEndian.Uint32(ctx.Body[4:8])
		y = binary.BigEndian.Uint32(ctx.Body[8:12])
		amfLen = binary.BigEndian.Uint32(ctx.Body[12:16])

		if len(ctx.Body) >= 16+int(amfLen) {
			amfData = ctx.Body[16 : 16+int(amfLen)]
		}
	}

	// 更新用户位置
	user.PosX = int(x)
	user.PosY = int(y)
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}

	// 构建响应
	body := make([]byte, 20+len(amfData))
	binary.BigEndian.PutUint32(body[0:4], walkType)
	binary.BigEndian.PutUint32(body[4:8], uint32(ctx.UserID))
	binary.BigEndian.PutUint32(body[8:12], x)
	binary.BigEndian.PutUint32(body[12:16], y)
	binary.BigEndian.PutUint32(body[16:20], amfLen)
	if len(amfData) > 0 {
		copy(body[20:], amfData)
	}

	// 发送响应给请求者，并广播给同地图其他玩家
	ctx.GameServer.SendResponse(ctx.ClientData, 2101, ctx.UserID, ctx.SeqID, body)
	if user.MapID > 0 {
		ctx.GameServer.BroadcastToMap(user.MapID, ctx.UserID, 2101, body)
	}
	logger.Info(fmt.Sprintf("[2101] 人物移动: UID=%d X=%d Y=%d", ctx.UserID, x, y))
}

// handlePetShow CMD 2305 展示精灵（跟随面板）
// 对齐前端 PetShowInfo: userID(4)+catchTime(4)+petID(4)+flag(4)+dv(4)+shiny(4)+skinID(4)
// 请求: catchTime(4) + flag(4)
func handlePetShow(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)

	var reqCatchTime, reqFlag uint32
	if len(ctx.Body) >= 8 {
		reqCatchTime = binary.BigEndian.Uint32(ctx.Body[0:4])
		reqFlag = binary.BigEndian.Uint32(ctx.Body[4:8])
	}

	// 确定要展示的精灵
	// 规则：
	// - 前端主动指定 catchTime>0 时，视为“设置 / 取消”跟随精灵
	// - catchTime==0 时，仅返回当前已保存的跟随精灵（若有），否则不展示任何精灵
	//   不再自动使用第一只精灵，避免“未操作时默认跟随第一只”的行为
	var petID, catchTime, petDV, petShiny uint32 = 0, 0, 31, 0

	// 如果指定了 catchTime，查找对应的精灵
	if reqCatchTime > 0 {
		catchTime = reqCatchTime
		// 先从背包查找
		for _, pet := range user.Pets {
			if uint32(pet.CatchTime) == reqCatchTime {
				petID = uint32(pet.ID)
				petDV = uint32(pet.DV)
				if petDV == 0 {
					petDV = 31
				}
				if pet.Shiny != 0 {
					petShiny = 1
				}
				break
			}
		}
		// 如果背包没找到，从仓库查找
		if petID == 7 && user.StoragePets != nil {
			for _, pet := range user.StoragePets {
				if uint32(pet.CatchTime) == reqCatchTime {
					petID = uint32(pet.ID)
					petDV = uint32(pet.DV)
					if petDV == 0 {
						petDV = 31
					}
					if pet.Shiny != 0 {
						petShiny = 1
					}
					break
				}
			}
		}
	} else {
		// 未指定 catchTime：仅返回当前已保存的跟随精灵（如果有）
		if user.FollowPetCatchTime > 0 {
			for _, pet := range user.Pets {
				if pet.CatchTime == user.FollowPetCatchTime {
					petID = uint32(pet.ID)
					catchTime = uint32(pet.CatchTime)
					petDV = uint32(pet.DV)
					if petDV == 0 {
						petDV = 31
					}
					if pet.Shiny != 0 {
						petShiny = 1
					}
					break
				}
			}
			if petID == 0 && user.StoragePets != nil {
				for _, pet := range user.StoragePets {
					if pet.CatchTime == user.FollowPetCatchTime {
						petID = uint32(pet.ID)
						catchTime = uint32(pet.CatchTime)
						petDV = uint32(pet.DV)
						if petDV == 0 {
							petDV = 31
						}
						if pet.Shiny != 0 {
							petShiny = 1
						}
						break
					}
				}
			}
		}
		// 若 FollowPetCatchTime==0，则保持 petID/catchTime 为 0，表示当前没有展示精灵
	}

	// 保存当前跟随精灵，供 buildPeopleInfo 与 2003 同步给其他玩家
	if reqCatchTime > 0 {
		if reqFlag == 1 {
			user.FollowPetCatchTime = int(reqCatchTime)
		} else {
			user.FollowPetCatchTime = 0
		}
		if ctx.GameServer.UserDB != nil {
			ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
		}
	}

	// 构建响应（与前端 PetShowInfo 一致：+shiny(4)）
	body := make([]byte, 28)
	binary.BigEndian.PutUint32(body[0:4], uint32(ctx.UserID))
	binary.BigEndian.PutUint32(body[4:8], catchTime)
	binary.BigEndian.PutUint32(body[8:12], petID)
	binary.BigEndian.PutUint32(body[12:16], reqFlag)
	binary.BigEndian.PutUint32(body[16:20], petDV)
	binary.BigEndian.PutUint32(body[20:24], petShiny)
	binary.BigEndian.PutUint32(body[24:28], 0) // skinID = 0

	ctx.GameServer.SendResponse(ctx.ClientData, 2305, ctx.UserID, ctx.SeqID, body)
	if user.MapID > 0 {
		ctx.GameServer.BroadcastToMap(user.MapID, ctx.UserID, 2305, body)
	}
	logger.Info(fmt.Sprintf("[2305] 展示精灵: PetID=%d CatchTime=%d Shiny=%d (0=普通 1=异色)", petID, catchTime, petShiny))
}

// 分子转化仪孵化时长（秒），测试用 1 分钟；正式可改为 24*3600
const hatchDurationSec = 60

// handlePetHatch CMD 2315 分子转化仪：放入精元开始孵化
// 请求：至少 4 字节 itemID(4) 大端；消耗 1 个该精元并开始孵化
// 响应：4 字节 0 成功，非 0 可表示失败（当前统一 0）
func handlePetHatch(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body[0:4], 0)

	if len(ctx.Body) < 4 {
		ctx.GameServer.SendResponse(ctx.ClientData, 2315, ctx.UserID, ctx.SeqID, body)
		return
	}
	itemID := int(binary.BigEndian.Uint32(ctx.Body[0:4]))
	petID := sptboss.GetPetIDByEssenceItem(itemID)
	if petID == 0 {
		logger.Info(fmt.Sprintf("[2315] 分子转化仪精元不可转化: ItemID=%d 未在精元映射中，请确认 items.xml 已加载且含 BreedTime+BreedMonID", itemID))
		binary.BigEndian.PutUint32(body[0:4], 1) // 非 0 表示失败
		ctx.GameServer.SendResponse(ctx.ClientData, 2315, ctx.UserID, ctx.SeqID, body)
		return
	}
	if ctx.GameServer.UserDB == nil {
		ctx.GameServer.SendResponse(ctx.ClientData, 2315, ctx.UserID, ctx.SeqID, body)
		return
	}
	if ctx.GameServer.UserDB.GetItemCount(ctx.UserID, itemID) < 1 {
		ctx.GameServer.SendResponse(ctx.ClientData, 2315, ctx.UserID, ctx.SeqID, body)
		return
	}
	ctx.GameServer.UserDB.RemoveItem(ctx.UserID, itemID, 1)
	user.HatchingPetID = petID
	user.HatchingStartTime = time.Now().Unix()
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}
	ctx.GameServer.SendResponse(ctx.ClientData, 2315, ctx.UserID, ctx.SeqID, body)
	logger.Info(fmt.Sprintf("[2315] 分子转化仪放入精元: ItemID=%d -> PetID=%d", itemID, petID))
}

// handlePetHatchGet CMD 2316 分子转化仪：查询孵化状态 / 领取已孵化精灵
// 响应 16 字节：status(4)+petID(4)+hatchTime(4)+remainingTime(4)。无孵化中则全 0；孵化完成时领取精灵并推送 8004，再回 0,0,0,0
func handlePetHatchGet(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	body := make([]byte, 16)

	if user.HatchingPetID == 0 {
		ctx.GameServer.SendResponse(ctx.ClientData, 2316, ctx.UserID, ctx.SeqID, body)
		return
	}

	now := time.Now().Unix()
	elapsed := now - user.HatchingStartTime
	remaining := int64(hatchDurationSec) - elapsed

	if remaining <= 0 {
		// 孵化完成：发放精灵并清除孵化状态；精元应获得对应 SPT 的小形态（进化链最初形态）
		bossPetID := user.HatchingPetID
		user.HatchingPetID = 0
		user.HatchingStartTime = 0
		if ctx.GameServer.UserDB != nil {
			ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
		}
		petIDToGive := bossPetID
		if chain := gamepets.GetInstance().GetEvolutionChain(bossPetID); len(chain) > 0 {
			petIDToGive = chain[0] // 进化链第一个为小形态（如里奥斯精元→胡里亚）
		}
		newCatchTime := int(time.Now().Unix())
		rand.Seed(time.Now().UnixNano() + int64(newCatchTime))
		newPet := userdb.Pet{
			ID:        petIDToGive,
			CatchTime: newCatchTime,
			Level:     1,
			DV:        rand.Intn(32),
			Nature:    rand.Intn(25),
			Exp:       0,
			Name:      "",
			Shiny:     1, // 测试：百分百异色
		}
		if len(user.Pets) >= 6 {
			if user.StoragePets == nil {
				user.StoragePets = []userdb.Pet{}
			}
			user.StoragePets = append(user.StoragePets, newPet)
		} else {
			user.Pets = append(user.Pets, newPet)
		}
		if ctx.GameServer.UserDB != nil {
			ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
			ctx.GameServer.UserDB.RecordCatch(ctx.UserID, petIDToGive)
		}
		// 先推送 8004 供客户端 BossCmdListener 弹窗「获得精灵」
		body8004 := buildBossMonster8004Body(0, uint32(petIDToGive), uint32(newCatchTime), 0, 0)
		ctx.GameServer.SendResponse(ctx.ClientData, 8004, ctx.UserID, 0, body8004)
		// 再回 2316：status=2 表示「刚领取」，带 petID+hatchTime 便于客户端在分子转化仪界面弹窗提示
		body = make([]byte, 16)
		binary.BigEndian.PutUint32(body[0:4], 2)                           // status=2 孵化完成已领取
		binary.BigEndian.PutUint32(body[4:8], uint32(petIDToGive))
		binary.BigEndian.PutUint32(body[8:12], uint32(newCatchTime))
		binary.BigEndian.PutUint32(body[12:16], 0)                          // remaining=0
		ctx.GameServer.SendResponse(ctx.ClientData, 2316, ctx.UserID, ctx.SeqID, body)
		logger.Info(fmt.Sprintf("[2316] 分子转化仪领取精灵: BossPetID=%d -> 小形态 PetID=%d CatchTime=%d", bossPetID, petIDToGive, newCatchTime))
		return
	}

	binary.BigEndian.PutUint32(body[0:4], 1)
	binary.BigEndian.PutUint32(body[4:8], uint32(user.HatchingPetID))
	binary.BigEndian.PutUint32(body[8:12], uint32(hatchDurationSec))
	binary.BigEndian.PutUint32(body[12:16], uint32(remaining))
	ctx.GameServer.SendResponse(ctx.ClientData, 2316, ctx.UserID, ctx.SeqID, body)
}

// handlePetGetExp CMD 2319 获取经验池经验
// 对齐 Lua: pet_advanced_handlers.handlePetGetExp
// 响应: expPool(4)
func handlePetGetExp(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)

	expPool := user.ExpPool
	if expPool < 0 {
		expPool = 0
	}

	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body[0:4], uint32(expPool))

	ctx.GameServer.SendResponse(ctx.ClientData, 2319, ctx.UserID, ctx.SeqID, body)
	logger.Info(fmt.Sprintf("[2319] 获取经验池经验: ExpPool=%d", expPool))
}

// handleGetMyExperienceComplete CMD 3011 发明室经验接收器 - 教官查看未领取经验（GetExperienceInfo.getExp）
// 响应: getExp(4) 未领取的经验值
func handleGetMyExperienceComplete(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	expPool := user.ExpPool
	if expPool < 0 {
		expPool = 0
	}
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body[0:4], uint32(expPool))
	ctx.GameServer.SendResponse(ctx.ClientData, 3011, ctx.UserID, ctx.SeqID, body)
	logger.Info(fmt.Sprintf("[3011] 教官查看未领取经验: ExpPool=%d", expPool))
}

// handleMyExperiencePondComplete CMD 3009 发明室经验接收器 - 学员查询教官积累经验值（MyExperiencePondInfo.getMyExp）
// 响应: getMyExp(4) 教官积累的经验值（简化：当前用户经验池；若为 0 则首次赠送测试经验以便能领取）
func handleMyExperiencePondComplete(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	expPool := user.ExpPool
	if expPool < 0 {
		expPool = 0
	}
	// 经验池为 0 时赠送一次可领取经验，避免玩家点经验接收器永远领不到（NoNo 积累 / 任务奖励未触发时）
	const welcomeExp = 5000
	if expPool == 0 {
		user.ExpPool = welcomeExp
		expPool = welcomeExp
		if ctx.GameServer.UserDB != nil {
			ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
		}
		logger.Info(fmt.Sprintf("[3009] 经验池为空，赠送首次可领取经验: %d", welcomeExp))
	}
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body[0:4], uint32(expPool))
	ctx.GameServer.SendResponse(ctx.ClientData, 3009, ctx.UserID, ctx.SeqID, body)
	logger.Info(fmt.Sprintf("[3009] 查询经验池可领取: ExpPool=%d", expPool))
}

// handleExperienceSharedComplete CMD 3007 发明室经验接收器 - 领取经验并平均分配给背包所有精灵（ExperienceSharedInfo.getFraction）
// 响应: getFraction(4) 本次共分配给精灵的总经验
func handleExperienceSharedComplete(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	totalPool := user.ExpPool
	if totalPool < 0 {
		totalPool = 0
	}
	n := len(user.Pets)
	if n == 0 {
		user.ExpPool = 0
		if ctx.GameServer.UserDB != nil {
			ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
		}
		body := make([]byte, 4)
		binary.BigEndian.PutUint32(body[0:4], 0)
		ctx.GameServer.SendResponse(ctx.ClientData, 3007, ctx.UserID, ctx.SeqID, body)
		logger.Info("[3007] 领取经验但背包无精灵，已清空经验池")
		return
	}
	perPet := totalPool / n
	user.ExpPool = 0
	petMgr := gamepets.GetInstance()
	for idx := range user.Pets {
		if perPet <= 0 {
			continue
		}
		p := &user.Pets[idx]
		p.Exp += perPet
		if p.Level <= 0 {
			p.Level = 1
		}
		if p.Level > 100 {
			p.Level = 100
			p.Exp = 0
			continue
		}
		oldLevel := p.Level
		for {
			expInfo := petMgr.GetExpInfo(p.ID, p.Level, p.Exp)
			if p.Level >= 100 {
				p.Level = 100
				p.Exp = 0
				break
			}
			if p.Exp < expInfo.NextLevelExp {
				break
			}
			p.Exp -= expInfo.NextLevelExp
			p.Level++
		}
		canEvolve, _, evolveTo := petMgr.CanEvolve(p.ID, p.Level, false)
		if canEvolve && evolveTo > 0 {
			p.ID = evolveTo
			p.Exp = 0
		}
		if p.Level > oldLevel {
			// 升级后先补满技能槽：若当前携带技能不足 4 个，则用已学会技能的前几个补足到 4 个，只做补充不替换已有技能。
			ensurePetSkillsFilledUpToFour(p, petMgr)

			// 再计算本次升级区间内「新学会」的技能，仅用于 2507 弹出替换界面，实际替换由 2312 完成。
			var newSkillIDs []int
			seen := make(map[int]bool)
			for lv := oldLevel + 1; lv <= p.Level; lv++ {
				for _, sid := range petMgr.GetSkillsLearnedAtLevel(p.ID, lv) {
					if sid > 0 && !seen[sid] {
						seen[sid] = true
						newSkillIDs = append(newSkillIDs, sid)
					}
				}
			}
			if len(newSkillIDs) > 0 {
				skillBody := buildNoteUpdateSkill(uint32(p.CatchTime), p.ID, oldLevel, newSkillIDs)
				ctx.GameServer.SendResponse(ctx.ClientData, 2507, ctx.UserID, ctx.SeqID, skillBody)
			}
		}
		ev := gamepets.ClampAndCapEV(p.GetEVStats())
		stats := petMgr.GetStats(p.ID, p.Level, p.DV, ev, p.Nature)
		propBody := buildNoteUpdateProp(uint32(p.CatchTime), p.ID, p.Level, p.Exp,
			stats.MaxHP, stats.Attack, stats.Defence, stats.SpAtk, stats.SpDef, stats.Speed, ev)
		ctx.GameServer.SendResponse(ctx.ClientData, 2508, ctx.UserID, ctx.SeqID, propBody)
		fullBody := buildFullPetInfo(*p)
		ctx.GameServer.SendResponse(ctx.ClientData, 2301, ctx.UserID, ctx.SeqID, fullBody)
	}
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}
	totalGiven := perPet * n
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body[0:4], uint32(totalGiven))
	ctx.GameServer.SendResponse(ctx.ClientData, 3007, ctx.UserID, ctx.SeqID, body)
	logger.Info(fmt.Sprintf("[3007] 经验接收器领取: 总分配=%d 每只=%d 精灵数=%d", totalGiven, perPet, n))
}

// handleTalkCount CMD 2701 对话计数（矿物挖掘/气体收集今日次数；发明室经验接收器 CateId=2002）
// 请求: cateId(4)，前端 DayOreCount.sendToServer(_type) 发明室经验接收器发 2002
// 响应: MiningCountInfo 仅 miningCount(4)，0=今日未领 1=今日已领
func handleTalkCount(ctx *gameserver.HandlerContext) {
	var cateId uint32
	if len(ctx.Body) >= 4 {
		cateId = binary.BigEndian.Uint32(ctx.Body[0:4])
	}

	count := uint32(0)
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	today := int64(time.Now().Unix() / 86400)
	if user.MiningDate == today && user.MiningCount != nil {
		if c, ok := user.MiningCount[int(cateId)]; ok {
			count = uint32(c)
		}
	}

	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body[0:4], count)
	ctx.GameServer.SendResponse(ctx.ClientData, 2701, ctx.UserID, ctx.SeqID, body)
	logger.Info(fmt.Sprintf("[2701] 对话计数: CateId=%d Count=%d", cateId, count))
}

// 矿物/气体 cateId 对应物品 ID。bMiningStr "1"=黄晶矿(黄金矿)：map10/21/15/325 对应 1/2/3/17 一律发黄晶矿 400001；bMiningStr "2"=甲烷：4/5/6/18 发甲烷 400002
var miningCateToItemID = map[uint32]uint32{
	1: 400001, 2: 400001, 3: 400001, 4: 400002, 5: 400002, 6: 400002, // 1 2 3 17 黄晶矿(黄金矿) 4 5 6 18 甲烷
	7: 400009, 8: 400009, 9: 400009, 10: 400009, 11: 400009, 12: 400009,
	13: 400001, 14: 400009, 15: 400009, 16: 400009, 17: 400001, 18: 400002,
	19: 400001, 20: 400001, 21: 400001, 22: 400001, // 海洋星二层等黄金矿(黄晶矿 400001)，cateId 19-22 发黄金矿
}

// handleTalkCate CMD 2702 对话分类（发放领取物品 / 矿物挖掘、气体收集结果）
// 请求: cateId(4)。响应: DayTalkInfo = cateCount(4) + cateCount×CateInfo(id,count) + outCount(4) + outCount×CateInfo(id,count)
func handleTalkCate(ctx *gameserver.HandlerContext) {
	var cateId uint32
	if len(ctx.Body) >= 4 {
		cateId = binary.BigEndian.Uint32(ctx.Body[0:4])
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	if user.Items == nil {
		user.Items = map[string]userdb.Item{}
	}

	var outItemID, outCount uint32

	// 发明室(107) 经验接收器 CateId=2002：每日一次 10 万经验放入 NoNo 经验存储器(ExpPool)，客户端提示“恭喜你获得了 X 积累经验”
	// 船长室 超能 NONO 赛尔豆领取 CateId=2001：每日一次 10 万赛尔豆
	const captainRoomCoinsCateId = 2001
	const captainRoomCoinsPerDay = 100000
	const inventionRoomExpCateId = 2002
	const inventionRoomExpPerDay = 100000
	if cateId == captainRoomCoinsCateId {
		today := int64(time.Now().Unix() / 86400)
		if user.MiningDate != today {
			user.MiningCount = map[int]int{}
			user.MiningDate = today
		}
		if user.MiningCount == nil {
			user.MiningCount = map[int]int{}
		}
		if user.MiningCount[captainRoomCoinsCateId] >= 1 {
			outItemID = 0
			outCount = 0
			logger.Info("[2702] 船长室每日赛尔豆: 今日已领过，不再发放")
		} else {
			user.MiningCount[captainRoomCoinsCateId] = 1
			user.Coins += captainRoomCoinsPerDay
			// 客户端用 outList[0].id/count 显示；id=1 作为“赛尔豆/金币”类型，count 为数值用于提示
			outItemID = 1
			outCount = uint32(captainRoomCoinsPerDay)
			logger.Info(fmt.Sprintf("[2702] 船长室每日赛尔豆: 发放 %d 当前 Coins=%d", captainRoomCoinsPerDay, user.Coins))
		}
	} else if cateId == inventionRoomExpCateId {
		today := int64(time.Now().Unix() / 86400)
		if user.MiningDate != today {
			user.MiningCount = map[int]int{}
			user.MiningDate = today
		}
		if user.MiningCount == nil {
			user.MiningCount = map[int]int{}
		}
		if user.MiningCount[inventionRoomExpCateId] >= 1 {
			outItemID = 0
			outCount = 0
			logger.Info("[2702] 发明室经验接收器: 今日已领过，不再发放")
		} else {
			user.MiningCount[inventionRoomExpCateId] = 1
			if user.ExpPool < 0 {
				user.ExpPool = 0
			}
			user.ExpPool += inventionRoomExpPerDay
			// 客户端用 outList[0].id/count 显示；id=3 表示“积累经验”，count 为数值用于提示
			outItemID = 3
			outCount = uint32(inventionRoomExpPerDay)
			logger.Info(fmt.Sprintf("[2702] 发明室经验接收器: 发放 %d 经验到 NoNo 经验存储器，当前 ExpPool=%d", inventionRoomExpPerDay, user.ExpPool))
		}
	} else if cateId >= 1 && cateId <= 22 {
		today := int64(time.Now().Unix() / 86400)
		if user.MiningDate != today {
			user.MiningCount = map[int]int{}
			user.MiningDate = today
		}
		if user.MiningCount == nil {
			user.MiningCount = map[int]int{}
		}
		user.MiningCount[int(cateId)]++
		itemID := miningCateToItemID[cateId]
		if itemID == 0 {
			itemID = 400001
		}
		// 每次采集数量随机 2-5
		n := uint32(rand.Intn(4) + 2) // [2,5]
		itemKey := strconv.FormatUint(uint64(itemID), 10)
		if it, ok := user.Items[itemKey]; ok {
			it.Count += int(n)
			user.Items[itemKey] = it
		} else {
			user.Items[itemKey] = userdb.Item{Count: int(n), ExpireTime: 0}
		}
		outItemID = itemID
		outCount = n
		logger.Info(fmt.Sprintf("[2702] 矿物/气体: CateId=%d 今日次数+1 发放 itemId=%d x%d", cateId, itemID, n))
	} else if cateId == 2051 {
		// Map 103: 神奇扭蛋牌，每日仅可领取一次，每次 5 个
		const magicGachaCateId = 2051
		const magicGachaItemID = 400501
		const magicGachaCountPerDay = 5
		today := int64(time.Now().Unix() / 86400)
		if user.MiningDate != today {
			user.MiningCount = map[int]int{}
			user.MiningDate = today
		}
		if user.MiningCount == nil {
			user.MiningCount = map[int]int{}
		}
		if user.MiningCount[magicGachaCateId] >= 1 {
			outItemID = 0
			outCount = 0
			logger.Info("[2702] 神奇扭蛋牌: 今日已领过，不再发放")
		} else {
			user.MiningCount[magicGachaCateId] = 1
			itemKey := strconv.FormatUint(uint64(magicGachaItemID), 10)
			if it, ok := user.Items[itemKey]; ok {
				it.Count += magicGachaCountPerDay
				user.Items[itemKey] = it
			} else {
				user.Items[itemKey] = userdb.Item{Count: magicGachaCountPerDay, ExpireTime: 0x057E40}
			}
			outItemID = magicGachaItemID
			outCount = magicGachaCountPerDay
			logger.Info(fmt.Sprintf("[2702] 神奇扭蛋牌: 每日领取一次，发放 itemId=%d x%d", magicGachaItemID, magicGachaCountPerDay))
		}
	}

	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}

	// DayTalkInfo: cateCount(4) + cateCount×CateInfo(8) + outCount(4) + outCount×CateInfo(8)
	// 客户端 outCount=条目数，每条 CateInfo(id,count) 表示一种物品及数量；采集只发 1 条，count 为 2-5
	body := make([]byte, 0, 24)
	body = append(body, 0, 0, 0, 0) // cateCount = 0
	tmp := make([]byte, 4)
	if outCount > 0 {
		binary.BigEndian.PutUint32(tmp, 1) // outCount=1 条 CateInfo，客户端据此显示完成提示
		body = append(body, tmp...)
		binary.BigEndian.PutUint32(tmp, outItemID)
		body = append(body, tmp...)
		binary.BigEndian.PutUint32(tmp, outCount) // 该条数量 2-5
		body = append(body, tmp...)
	} else {
		binary.BigEndian.PutUint32(tmp, 0)
		body = append(body, tmp...)
	}
	ctx.GameServer.SendResponse(ctx.ClientData, 2702, ctx.UserID, ctx.SeqID, body)
	logger.Info(fmt.Sprintf("[2702] 对话分类: CateId=%d", cateId))
}

// handlePetSetExp CMD 2318 从经验池分配经验给精灵
// 对齐 Lua: pet_advanced_handlers.handlePetSetExp
// 请求: catchTime(4) + expAmount(4)
// 响应: newPoolExp(4)
// 只从经验池扣除“实际加到精灵身上”的经验，超出升级所需的部分保留在池中（返还经验分配器）
func handlePetSetExp(ctx *gameserver.HandlerContext) {
	var catchTime, expAmount uint32
	if len(ctx.Body) >= 8 {
		catchTime = binary.BigEndian.Uint32(ctx.Body[0:4])
		expAmount = binary.BigEndian.Uint32(ctx.Body[4:8])
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)

	currentPool := user.ExpPool
	if currentPool < 0 {
		currentPool = 0
	}
	use := int(expAmount)
	if use < 0 {
		use = 0
	}
	if use > currentPool {
		use = currentPool
	}
	actualUse := 0 // 实际加到精灵身上的经验，仅扣减这部分，多余返还经验池

	// 给对应精灵加经验并处理升级 & 进化（Pet.Exp 表示“当前等级已获得经验”，不是总经验）
	if catchTime != 0 && use > 0 {
		petMgr := gamepets.GetInstance()
		for idx := range user.Pets {
			if uint32(user.Pets[idx].CatchTime) != catchTime {
				continue
			}

			p := &user.Pets[idx]
			if p.Level <= 0 {
				p.Level = 1
			}
			if p.Level > 100 {
				p.Level = 100
				p.Exp = 0
				break
			}

			oldLevel := p.Level
			remainingToAdd := use
			// 按“填满当前等级所需”逐次加经验，多出的部分不会从池中扣除（相当于返还）
			for remainingToAdd > 0 && p.Level < 100 {
				expInfo := petMgr.GetExpInfo(p.ID, p.Level, p.Exp)
				need := expInfo.NextLevelExp - p.Exp
				if need <= 0 {
					break
				}
				add := remainingToAdd
				if add > need {
					add = need
				}
				p.Exp += add
				actualUse += add
				remainingToAdd -= add
				if p.Exp >= expInfo.NextLevelExp {
					p.Exp -= expInfo.NextLevelExp
					p.Level++
					// 检查是否可以进化（直接进化型）
					canEvolve, _, evolveTo := petMgr.CanEvolve(p.ID, p.Level, false)
					if canEvolve && evolveTo > 0 {
						logger.Info(fmt.Sprintf("[2318] 精灵进化触发: PetID %d -> %d (Level=%d)", p.ID, evolveTo, p.Level))
						p.ID = evolveTo
						p.Exp = 0
					}
				}
			}
			if p.Level >= 100 {
				p.Level = 100
				p.Exp = 0
			}

			// 若升级了且精灵有新可学技能：
			// 1) 先按规则补满技能槽到 4 个（只补充，不替换已有技能）；
			// 2) 再发送 NOTE_UPDATE_SKILL(2507)，由前端决定是否通过 2312 进行替换。
			if p.Level > oldLevel {
				ensurePetSkillsFilledUpToFour(p, petMgr)

				var newSkillIDs []int
				seen := make(map[int]bool)
				for lv := oldLevel + 1; lv <= p.Level; lv++ {
					for _, sid := range petMgr.GetSkillsLearnedAtLevel(p.ID, lv) {
						if sid > 0 && !seen[sid] {
							seen[sid] = true
							newSkillIDs = append(newSkillIDs, sid)
						}
					}
				}
				if len(newSkillIDs) > 0 {
					skillBody := buildNoteUpdateSkill(uint32(p.CatchTime), p.ID, oldLevel, newSkillIDs)
					ctx.GameServer.SendResponse(ctx.ClientData, 2507, ctx.UserID, ctx.SeqID, skillBody)
					logger.Info(fmt.Sprintf("[2318] 升级触发技能学习: PetID=%d Level=%d->%d 新技能=%v", p.ID, oldLevel, p.Level, newSkillIDs))
				}
			}

			// 发送 NOTE_UPDATE_PROP(2508)，驱动升级/属性变化面板
			ev := gamepets.ClampAndCapEV(p.GetEVStats())
			stats := petMgr.GetStats(p.ID, p.Level, p.DV, ev, p.Nature)
			propBody := buildNoteUpdateProp(uint32(p.CatchTime), p.ID, p.Level, p.Exp,
				stats.MaxHP, stats.Attack, stats.Defence, stats.SpAtk, stats.SpDef, stats.Speed, ev)
			ctx.GameServer.SendResponse(ctx.ClientData, 2508, ctx.UserID, ctx.SeqID, propBody)

			// 同步一次完整精灵信息（2301），方便背包面板立即刷新
			fullBody := buildFullPetInfo(*p)
			ctx.GameServer.SendResponse(ctx.ClientData, 2301, ctx.UserID, ctx.SeqID, fullBody)

			break
		}
	}

	// 只扣减实际加到精灵身上的经验，多余部分保留在经验池
	user.ExpPool = currentPool - actualUse

	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}

	resp := make([]byte, 4)
	binary.BigEndian.PutUint32(resp[0:4], uint32(user.ExpPool))
	ctx.GameServer.SendResponse(ctx.ClientData, 2318, ctx.UserID, ctx.SeqID, resp)
	logger.Info(fmt.Sprintf("[2318] 分配经验: catchTime=%d 请求=%d 实际消耗=%d 返还=%d newPool=%d", catchTime, use, actualUse, use-actualUse, user.ExpPool))
}

// handlePetKingJoin CMD 2413 PET_KING_JOIN 精灵王之战匹配
// body: mode(4) + sub(4)
// - PetKingWaitPanel: mode=5(单精灵) / 6(多精灵), sub=0
// - MapProcess_111 大师杯等旧逻辑也可能使用 2413，这里统一走 PvP 匹配流程。
// 行为：
// - 若当前模式下暂无等待者，则将本玩家加入队列，简单回包 4 字节 0。
// - 若已有另一名玩家在同一模式等待，则立即与其匹配，发送 PvP 2503，初始化 BattleState，双方后续通过 2404 进入战斗。
func handlePetKingJoin(ctx *gameserver.HandlerContext) {
	var mode uint32
	if len(ctx.Body) >= 4 {
		mode = binary.BigEndian.Uint32(ctx.Body[0:4])
	}
	var sub uint32
	if len(ctx.Body) >= 8 {
		sub = binary.BigEndian.Uint32(ctx.Body[4:8])
	}
	logger.Info(fmt.Sprintf("[2413] PET_KING_JOIN: UID=%d mode=%d sub=%d", ctx.UserID, mode, sub))

	gs := ctx.GameServer

	// 将 mode 归一化为精灵王之战匹配模式键：
	// - 5: 单精灵王之战
	// - 6: 多精灵王之战
	// 其余值（如 11 的大师杯）暂统一按单精灵处理。
	matchKey := mode
	if matchKey != 5 && matchKey != 6 {
		matchKey = 5
	}

	// 5 -> fightMode=1(单精灵)，6 -> fightMode=2(多精灵)
	var fightMode uint32 = 1
	if matchKey == 6 {
		fightMode = 2
	}

	// 先尝试从队列中取出一名等待的对手
	gs.PetKingQueueMu.Lock()
	waitingUID, hasWaiting := gs.PetKingQueue[matchKey]
	if !hasWaiting || waitingUID == ctx.UserID {
		// 没有对手，或对手就是自己：将当前用户加入/更新为等待者
		gs.PetKingQueue[matchKey] = ctx.UserID
		gs.PetKingQueueMu.Unlock()

		resp := make([]byte, 4)
		binary.BigEndian.PutUint32(resp[0:4], 0)
		gs.SendResponse(ctx.ClientData, 2413, ctx.UserID, ctx.SeqID, resp)
		logger.Info(fmt.Sprintf("[2413] 加入精灵王之战匹配队列: UID=%d modeKey=%d", ctx.UserID, matchKey))
		return
	}
	// 有等待者：弹出并与之匹配
	delete(gs.PetKingQueue, matchKey)
	gs.PetKingQueueMu.Unlock()

	opponentUID := waitingUID
	inviterUID := opponentUID
	responderUID := ctx.UserID

	inviterClient := gs.GetClientByUserID(inviterUID)
	responderClient := ctx.ClientData
	if inviterClient == nil || responderClient == nil {
		// 对手已下线或不可用：当前玩家重新入队，避免卡死
		gs.PetKingQueueMu.Lock()
		gs.PetKingQueue[matchKey] = ctx.UserID
		gs.PetKingQueueMu.Unlock()

		resp := make([]byte, 4)
		binary.BigEndian.PutUint32(resp[0:4], 0)
		gs.SendResponse(ctx.ClientData, 2413, ctx.UserID, ctx.SeqID, resp)
		logger.Warning(fmt.Sprintf("[2413] 匹配对手 UID=%d 失效，UID=%d 重新入队 modeKey=%d", opponentUID, ctx.UserID, matchKey))
		return
	}

	// 为双方初始化 PvP BattleState，并构建 2503（准备对战）通知客户端加载资源
	body2503Inviter, body2503Responder := buildNoteReadyToFightInfoPvPPerClient(gs, inviterUID, responderUID, fightMode)
	gs.SendResponse(inviterClient, 2503, inviterUID, 0, body2503Inviter)
	gs.SendResponse(responderClient, 2503, responderUID, 0, body2503Responder)

	// 初始化 PvP BattleState，后续由双方 2404(READY_TO_FIGHT) 触发 2504 进入实战
	setPvPBattleStates(gs, inviterUID, responderUID, fightMode)
	// 标记本场为精灵王之战，用于结算时统计 MonKingWin
	markPetKingBattle(gs, inviterUID, responderUID)

	// 给双方回一个 2413 成功，以便前端关闭等待/匹配界面（若有依赖）
	resp := make([]byte, 4)
	binary.BigEndian.PutUint32(resp[0:4], 0)
	gs.SendResponse(inviterClient, 2413, inviterUID, ctx.SeqID, resp)
	gs.SendResponse(responderClient, 2413, responderUID, ctx.SeqID, resp)

	logger.Info(fmt.Sprintf("[2413] 精灵王之战匹配成功: inviterUID=%d responderUID=%d fightMode=%d(modeKey=%d)", inviterUID, responderUID, fightMode, matchKey))
}

// grandMeleeMinPets 精灵大乱斗至少需要的满状态精灵数
const grandMeleeMinPets = 3

// grandMeleeExpReward 精灵大乱斗胜方获得的可分配经验
const grandMeleeExpReward = 10000
// grandMeleeLoserExpReward 精灵大乱斗败方获得的可分配经验
const grandMeleeLoserExpReward = 5000

// handleGrandMeleeJoin CMD 2431 精灵大乱斗匹配
// 要求：至少 3 只精灵且全部满状态。匹配成功后从双方各随机取 3 只，再随机分配给双方进行 3v3 PvP，胜方获得 10000 可分配经验。
func handleGrandMeleeJoin(ctx *gameserver.HandlerContext) {
	gs := ctx.GameServer
	user := gs.GetOrCreateUser(ctx.UserID)
	petMgr := gamepets.GetInstance()

	// 筛选满状态精灵：CurrentHP <= 0 视为满血；否则需 CurrentHP >= MaxHP
	fullStatePets := make([]userdb.Pet, 0, len(user.Pets))
	for i := range user.Pets {
		p := &user.Pets[i]
		st := petMgr.GetStats(p.ID, p.Level, p.DV, p.GetEVStats(), p.Nature)
		maxHP := st.MaxHP
		if maxHP <= 0 {
			maxHP = 1
		}
		curHP := p.CurrentHP
		if curHP <= 0 {
			curHP = maxHP
		}
		if curHP >= maxHP {
			fullStatePets = append(fullStatePets, *p)
		}
	}
	if len(fullStatePets) < grandMeleeMinPets {
		resp := make([]byte, 4)
		binary.BigEndian.PutUint32(resp[0:4], 1)
		gs.SendResponse(ctx.ClientData, 2431, ctx.UserID, ctx.SeqID, resp)
		logger.Info(fmt.Sprintf("[2431] 精灵大乱斗: UID=%d 不满足条件（需至少%d只满状态精灵，当前满状态=%d）", ctx.UserID, grandMeleeMinPets, len(fullStatePets)))
		return
	}

	gs.GrandMeleeQueueMu.Lock()
	waitingUID := gs.GrandMeleeQueue
	if waitingUID == 0 || waitingUID == ctx.UserID {
		gs.GrandMeleeQueue = ctx.UserID
		gs.GrandMeleeQueueMu.Unlock()
		resp := make([]byte, 4)
		binary.BigEndian.PutUint32(resp[0:4], 0)
		gs.SendResponse(ctx.ClientData, 2431, ctx.UserID, ctx.SeqID, resp)
		logger.Info(fmt.Sprintf("[2431] 精灵大乱斗: UID=%d 加入匹配队列", ctx.UserID))
		return
	}
	gs.GrandMeleeQueue = 0
	gs.GrandMeleeQueueMu.Unlock()

	inviterUID := waitingUID
	responderUID := ctx.UserID
	inviterUser := gs.GetOrCreateUser(inviterUID)
	inviterFull := make([]userdb.Pet, 0, len(inviterUser.Pets))
	for i := range inviterUser.Pets {
		p := &inviterUser.Pets[i]
		st := petMgr.GetStats(p.ID, p.Level, p.DV, p.GetEVStats(), p.Nature)
		maxHP := st.MaxHP
		if maxHP <= 0 {
			maxHP = 1
		}
		curHP := p.CurrentHP
		if curHP <= 0 {
			curHP = maxHP
		}
		if curHP >= maxHP {
			inviterFull = append(inviterFull, *p)
		}
	}
	if len(inviterFull) < grandMeleeMinPets {
		gs.GrandMeleeQueueMu.Lock()
		gs.GrandMeleeQueue = ctx.UserID
		gs.GrandMeleeQueueMu.Unlock()
		resp := make([]byte, 4)
		binary.BigEndian.PutUint32(resp[0:4], 0)
		gs.SendResponse(ctx.ClientData, 2431, ctx.UserID, ctx.SeqID, resp)
		logger.Info(fmt.Sprintf("[2431] 对手 UID=%d 不满足满状态精灵数，UID=%d 重新入队", inviterUID, ctx.UserID))
		return
	}

	// 从双方各随机取 3 只，复制并设为满血
	rand.Shuffle(len(fullStatePets), func(i, j int) { fullStatePets[i], fullStatePets[j] = fullStatePets[j], fullStatePets[i] })
	rand.Shuffle(len(inviterFull), func(i, j int) { inviterFull[i], inviterFull[j] = inviterFull[j], inviterFull[i] })
	pool := make([]userdb.Pet, 0, 6)
	// 大乱斗临时 catchTime：必须与任意真实精灵 catchTime 隔离，否则客户端会把“对方精灵(真实catchTime)”缓存为己方背包精灵，
	// 进而在战斗结束后出现“对方精灵被加入我方背包”的错觉/污染。
	// 这里生成高位随机的 uint32，并确保本次匹配的 6 只各不相同。
	tmpCatchBase := uint32(0xF0000000) | uint32(rand.Intn(1<<20))<<8 | uint32(rand.Intn(1<<8))
	nextTmpCatch := func() uint32 {
		tmpCatchBase++
		if tmpCatchBase == 0 {
			tmpCatchBase = 0xF0000001
		}
		return tmpCatchBase
	}
	for i := 0; i < grandMeleeMinPets && i < len(inviterFull); i++ {
		cp := inviterFull[i]
		st := petMgr.GetStats(cp.ID, cp.Level, cp.DV, cp.GetEVStats(), cp.Nature)
		cp.CurrentHP = st.MaxHP
		if cp.CurrentHP <= 0 {
			cp.CurrentHP = 1
		}
		cp.CatchTime = int(nextTmpCatch())
		pool = append(pool, cp)
	}
	for i := 0; i < grandMeleeMinPets && i < len(fullStatePets); i++ {
		cp := fullStatePets[i]
		st := petMgr.GetStats(cp.ID, cp.Level, cp.DV, cp.GetEVStats(), cp.Nature)
		cp.CurrentHP = st.MaxHP
		if cp.CurrentHP <= 0 {
			cp.CurrentHP = 1
		}
		cp.CatchTime = int(nextTmpCatch())
		pool = append(pool, cp)
	}
	if len(pool) < 6 {
		gs.GrandMeleeQueueMu.Lock()
		gs.GrandMeleeQueue = ctx.UserID
		gs.GrandMeleeQueueMu.Unlock()
		resp := make([]byte, 4)
		binary.BigEndian.PutUint32(resp[0:4], 0)
		gs.SendResponse(ctx.ClientData, 2431, ctx.UserID, ctx.SeqID, resp)
		return
	}
	rand.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	inviterPets := append([]userdb.Pet(nil), pool[0:3]...)
	responderPets := append([]userdb.Pet(nil), pool[3:6]...)

	gs.GrandMeleePets[inviterUID] = inviterPets
	gs.GrandMeleePets[responderUID] = responderPets

	inviterClient := gs.GetClientByUserID(inviterUID)
	responderClient := ctx.ClientData
	if inviterClient == nil || responderClient == nil {
		delete(gs.GrandMeleePets, inviterUID)
		delete(gs.GrandMeleePets, responderUID)
		gs.GrandMeleeQueueMu.Lock()
		gs.GrandMeleeQueue = ctx.UserID
		gs.GrandMeleeQueueMu.Unlock()
		resp := make([]byte, 4)
		binary.BigEndian.PutUint32(resp[0:4], 0)
		gs.SendResponse(ctx.ClientData, 2431, ctx.UserID, ctx.SeqID, resp)
		logger.Warning(fmt.Sprintf("[2431] 对手已离线，UID=%d 重新入队", ctx.UserID))
		return
	}

	body2503Inviter, body2503Responder := buildNoteReadyToFightInfoPvPPerClient(gs, inviterUID, responderUID, 2)
	gs.SendResponse(inviterClient, 2503, inviterUID, 0, body2503Inviter)
	gs.SendResponse(responderClient, 2503, responderUID, 0, body2503Responder)
	setPvPBattleStates(gs, inviterUID, responderUID, 2)

	resp := make([]byte, 4)
	binary.BigEndian.PutUint32(resp[0:4], 0)
	gs.SendResponse(inviterClient, 2431, inviterUID, ctx.SeqID, resp)
	gs.SendResponse(responderClient, 2431, responderUID, ctx.SeqID, resp)
	logger.Info(fmt.Sprintf("[2431] 精灵大乱斗匹配成功: inviterUID=%d responderUID=%d", inviterUID, responderUID))
}

// handlePetStudySkill CMD 2307 学习/替换技能（包含升级学习与技能学习器）
// 典型 body 格式（20 字节，参照 MultiSkillPanel.SocketConnection.send 调用）：
//   catchTime(4) + 1(4) + 1(4) + oldSkillId(drop)(4) + newSkillId(study)(4)
// 其中两个常量 1 仅占位，实际含义忽略；不携带槽位索引，槽位需通过 oldSkillId 或空槽推断。
// 响应: ret(4)，0 表示成功，非 0 表示失败。
func handlePetStudySkill(ctx *gameserver.HandlerContext) {
	body := ctx.Body
	if len(body) < 16 {
		resp := make([]byte, 4)
		binary.BigEndian.PutUint32(resp[0:4], 1)
		ctx.GameServer.SendResponse(ctx.ClientData, 2307, ctx.UserID, ctx.SeqID, resp)
		return
	}

	catchTime := binary.BigEndian.Uint32(body[0:4])
	// body[4:8] 与 body[8:12] 为常量 1,1，占位用，可忽略
	if len(body) < 20 {
		resp := make([]byte, 4)
		binary.BigEndian.PutUint32(resp[0:4], 1)
		ctx.GameServer.SendResponse(ctx.ClientData, 2307, ctx.UserID, ctx.SeqID, resp)
		logger.Info(fmt.Sprintf("[2307] 学习技能请求体长度不足 20: bodyLen=%d", len(body)))
		return
	}
	oldSid := int(binary.BigEndian.Uint32(body[12:16])) // drop
	newSid := int(binary.BigEndian.Uint32(body[16:20])) // study

	logger.Info(fmt.Sprintf("[2307] 学习技能请求: catchTime=%d oldSid=%d newSid=%d bodyLen=%d", catchTime, oldSid, newSid, len(body)))

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	var picked *userdb.Pet
	if catchTime != 0 {
		for i := range user.Pets {
			if uint32(user.Pets[i].CatchTime) == catchTime {
				picked = &user.Pets[i]
				break
			}
		}
		if picked == nil && user.StoragePets != nil {
			for i := range user.StoragePets {
				if uint32(user.StoragePets[i].CatchTime) == catchTime {
					picked = &user.StoragePets[i]
					break
				}
			}
		}
	}
	if picked == nil {
		resp := make([]byte, 4)
		binary.BigEndian.PutUint32(resp[0:4], 1)
		ctx.GameServer.SendResponse(ctx.ClientData, 2307, ctx.UserID, ctx.SeqID, resp)
		logger.Info(fmt.Sprintf("[2307] 未找到精灵: catchTime=%d", catchTime))
		return
	}

	skillMgr := gameskills.GetInstance()
	if newSid <= 1 || !skillMgr.Exists(newSid) {
		resp := make([]byte, 4)
		binary.BigEndian.PutUint32(resp[0:4], 1)
		ctx.GameServer.SendResponse(ctx.ClientData, 2307, ctx.UserID, ctx.SeqID, resp)
		logger.Info(fmt.Sprintf("[2307] 无效新技能: newSid=%d", newSid))
		return
	}

	// 规范化当前技能：确保有 4 槽位，旧存档可能为空。
	current := make([]int, 4)
	for i := 0; i < 4 && i < len(picked.Skills); i++ {
		current[i] = picked.Skills[i]
	}

	// 根据 oldSid 推断要替换的槽位：优先匹配旧技能所在槽，其次第一个空槽，再兜底槽 0。
	targetSlot := -1
	if oldSid > 1 {
		for si := 0; si < 4; si++ {
			if current[si] == oldSid {
				targetSlot = si
				break
			}
		}
	}
	if targetSlot < 0 {
		for si := 0; si < 4; si++ {
			if current[si] == 0 {
				targetSlot = si
				break
			}
		}
	}
	if targetSlot < 0 {
		targetSlot = 0
	}

	// 同一技能只能携带一个：若其他槽已有 newSid，则先清掉（保留目标槽）。
	for i := 0; i < 4; i++ {
		if i == targetSlot {
			continue
		}
		if current[i] == newSid {
			current[i] = 0
		}
	}

	// 执行替换/学习：targetSlot 位置设置为 newSid。
	current[targetSlot] = newSid
	picked.Skills = current

	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
		ctx.GameServer.UserDB.SaveToFile()
	}

	resp := make([]byte, 4)
	binary.BigEndian.PutUint32(resp[0:4], 0)
	ctx.GameServer.SendResponse(ctx.ClientData, 2307, ctx.UserID, ctx.SeqID, resp)
	logger.Info(fmt.Sprintf("[2307] 学习技能成功: catchTime=%d petID=%d slot=%d newSid=%d skills=%v", catchTime, picked.ID, targetSlot, newSid, picked.Skills))
}

// handlePetSkillSwitch CMD 2312 精灵技能切换（技能唤醒仪替换技能）
// 请求体常见格式：
//
//	A) catchTime(4) + count(4) + [slotIndex(4), skillId(4)]*count — 只替换指定槽位，一技能只能带一个
//	B) catchTime(4) + [skillId(4)]*4 — 四槽依次填，会去重（同技能只保留第一次出现）
//
// 响应: ret(4)，0 表示成功，1 表示未找到精灵
func handlePetSkillSwitch(ctx *gameserver.HandlerContext) {
	body := ctx.Body
	bodyLen := len(body)
	logger.Info(fmt.Sprintf("[2312] 收到技能切换请求 bodyLen=%d", bodyLen))
	if bodyLen > 0 {
		dump := packet.HexDump(body, fmt.Sprintf("[PACKET] CMD=2312 包体详情"))
		logger.Info(dump)
	}

	var catchTime uint32
	if bodyLen >= 4 {
		catchTime = binary.BigEndian.Uint32(body[0:4])
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	var picked *userdb.Pet
	if catchTime != 0 {
		for i := range user.Pets {
			if uint32(user.Pets[i].CatchTime) == catchTime {
				picked = &user.Pets[i]
				break
			}
		}
		if picked == nil && user.StoragePets != nil {
			for i := range user.StoragePets {
				if uint32(user.StoragePets[i].CatchTime) == catchTime {
					picked = &user.StoragePets[i]
					break
				}
			}
		}
	}
	if picked == nil {
		logger.Info(fmt.Sprintf("[2312] 未找到精灵 catchTime=%d pets=%d", catchTime, len(user.Pets)))
		resp := make([]byte, 4)
		binary.BigEndian.PutUint32(resp[0:4], 1)
		ctx.GameServer.SendResponse(ctx.ClientData, 2312, ctx.UserID, ctx.SeqID, resp)
		return
	}

	petMgr := gamepets.GetInstance()
	skillMgr := gameskills.GetInstance()
	currentSkills := make([]int, 4)
	if len(picked.Skills) > 0 {
		for i := 0; i < 4 && i < len(picked.Skills); i++ {
			currentSkills[i] = picked.Skills[i]
		}
	}
	if currentSkills[0] == 0 && currentSkills[1] == 0 && currentSkills[2] == 0 && currentSkills[3] == 0 {
		defaults := petMgr.GetSkillsForLevel(picked.ID, picked.Level)
		for i := 0; i < 4 && i < len(defaults); i++ {
			currentSkills[i] = defaults[i]
		}
	}

	// 格式 A：catchTime(4) + count(4) + [slotIndex(4), skillId(4)]*count，只替换指定槽
	finalSkills := make([]int, 4)
	for i := 0; i < 4; i++ {
		finalSkills[i] = currentSkills[i]
	}
	if bodyLen >= 12 {
		count := binary.BigEndian.Uint32(body[4:8])
		if count >= 1 && count <= 4 && bodyLen >= 8+int(count)*8 {
			type slotPair struct {
				slot int
				sid  int
			}
			var pairs []slotPair
			for i := 0; i < int(count); i++ {
				slotIdx := int(binary.BigEndian.Uint32(body[8+8*i : 8+8*i+4]))
				sid := int(binary.BigEndian.Uint32(body[8+8*i+4 : 8+8*(i+1)]))
				oldSid := sid
				targetSlot := slotIdx
				// 兼容前端 20 字节格式：单槽时 body[16:20] 为右侧选中的新技能 ID，若有效则用其替换（避免前端误传左侧槽技能 ID）
				if count == 1 && bodyLen >= 20 && i == 0 {
					newSkillId := int(binary.BigEndian.Uint32(body[16:20]))
					if newSkillId > 1 && skillMgr.Exists(newSkillId) {
						logger.Info(fmt.Sprintf("[2312] 格式A 收到槽位/技能对(20字节): slotIndex=%d 原skillId=%d 使用newSkillId=%d", slotIdx, sid, newSkillId))
						sid = newSkillId
					} else {
						logger.Info(fmt.Sprintf("[2312] 格式A 收到槽位/技能对: slotIndex=%d skillId=%d", slotIdx, sid))
					}

					// 进一步兼容：20 字节格式里 body[12:16] 常为“被替换的旧技能ID”。
					// 若 slotIndex 与旧技能所在槽不一致，则优先用旧技能ID定位槽位，避免前端 slotIndex 传错导致换错槽。
					if oldSid > 1 {
						found := -1
						for si := 0; si < 4; si++ {
							if currentSkills[si] == oldSid {
								found = si
								break
							}
						}
						if found >= 0 && found != slotIdx {
							logger.Info(fmt.Sprintf("[2312] 20字节槽位纠正: slotIndex=%d oldSkillId=%d 实际槽=%d", slotIdx, oldSid, found))
							targetSlot = found
						}
					}
				} else {
					logger.Info(fmt.Sprintf("[2312] 格式A 收到槽位/技能对: slotIndex=%d skillId=%d", slotIdx, sid))
				}
				// 客户端传 0-based 槽位(0~3)，直接使用
				if targetSlot >= 0 && targetSlot < 4 && sid > 1 && skillMgr.Exists(sid) {
					pairs = append(pairs, slotPair{targetSlot, sid})
				}
			}
			// 格式 A 解析后若无有效槽位对，优先尝试兼容经验分配/升级学习技能时使用的 16 字节格式：
			// catchTime(4) + oldSkillId(4) + newSkillId(4) + reserved/slot(4)。
			if len(pairs) == 0 {
				if count == 1 && bodyLen >= 16 {
					old1 := int(binary.BigEndian.Uint32(body[4:8]))
					old2 := int(binary.BigEndian.Uint32(body[8:12]))
					newSid := int(binary.BigEndian.Uint32(body[12:16]))
					logger.Info(fmt.Sprintf("[2312] 格式A无显式槽位对，尝试16字节兼容解析: old1=%d old2=%d newSid=%d", old1, old2, newSid))
					if newSid > 1 && skillMgr.Exists(newSid) {
						targetSlot := -1
						for si := 0; si < 4; si++ {
							if currentSkills[si] == old1 || currentSkills[si] == old2 {
								targetSlot = si
								break
							}
						}
						if targetSlot >= 0 {
							for i := 0; i < 4; i++ {
								finalSkills[i] = currentSkills[i]
							}
							finalSkills[targetSlot] = newSid
							picked.Skills = finalSkills
							if ctx.GameServer.UserDB != nil {
								ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
								ctx.GameServer.UserDB.SaveToFile()
							}
							resp := make([]byte, 4)
							binary.BigEndian.PutUint32(resp[0:4], 0)
							ctx.GameServer.SendResponse(ctx.ClientData, 2312, ctx.UserID, ctx.SeqID, resp)
							logger.Info(fmt.Sprintf("[2312] 16字节兼容解析成功: catchTime=%d petID=%d slot=%d newSkillId=%d skills=%v", catchTime, picked.ID, targetSlot, newSid, picked.Skills))
							return
						}
					}
				}
				// 仍无法解析则直接返回成功且不修改技能，避免误入格式 B 破坏数据
				resp := make([]byte, 4)
				binary.BigEndian.PutUint32(resp[0:4], 0)
				ctx.GameServer.SendResponse(ctx.ClientData, 2312, ctx.UserID, ctx.SeqID, resp)
				logger.Info(fmt.Sprintf("[2312] 格式A无有效槽位/技能对，保持原技能: catchTime=%d petID=%d skills=%v", catchTime, picked.ID, picked.Skills))
				return
			}
			// 单槽替换且该槽已是该技能时，不做修改（客户端可能误传了左侧槽技能ID而非右侧选中的新技能ID）
			if len(pairs) == 1 && currentSkills[pairs[0].slot] == pairs[0].sid {
				resp := make([]byte, 4)
				binary.BigEndian.PutUint32(resp[0:4], 0)
				ctx.GameServer.SendResponse(ctx.ClientData, 2312, ctx.UserID, ctx.SeqID, resp)
				logger.Info(fmt.Sprintf("[2312] 槽位已是该技能，无变更(请确认客户端发送的是右侧选中的新技能ID): slot=%d skillId=%d", pairs[0].slot, pairs[0].sid))
				return
			}
			usedSid := make(map[int]bool)
			setByUser := make(map[int]bool) // 记录被用户指定替换的槽位，去重时保留这些槽的新技能
			for _, p := range pairs {
				if usedSid[p.sid] {
					continue
				}
				usedSid[p.sid] = true
				setByUser[p.slot] = true
				finalSkills[p.slot] = p.sid
			}
			// 同一技能只能带一个：若新技能与已有槽重复，重复槽放“被替换槽的原技能”（交换），避免技能丢失
			for i := 0; i < 4; i++ {
				sid := finalSkills[i]
				if sid <= 0 {
					continue
				}
				for j := i + 1; j < 4; j++ {
					if finalSkills[j] != sid {
						continue
					}
					if setByUser[i] {
						finalSkills[j] = currentSkills[i]
					} else if setByUser[j] {
						finalSkills[i] = currentSkills[j]
					} else {
						finalSkills[j] = currentSkills[j]
						if finalSkills[j] == sid {
							finalSkills[j] = 0
						}
					}
				}
			}
			picked.Skills = finalSkills
			if ctx.GameServer.UserDB != nil {
				ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
				ctx.GameServer.UserDB.SaveToFile() // 立即持久化到磁盘，确保重新进入游戏后技能不丢失
			}
			resp := make([]byte, 4)
			binary.BigEndian.PutUint32(resp[0:4], 0)
			ctx.GameServer.SendResponse(ctx.ClientData, 2312, ctx.UserID, ctx.SeqID, resp)
			logger.Info(fmt.Sprintf("[2312] 精灵技能切换成功(按槽): catchTime=%d petID=%d skills=%v", catchTime, picked.ID, picked.Skills))
			return
		}
	}

	// 格式 B：catchTime(4) + [skillId(4)]*4，四槽依次填，去重（一技能只能带一个）
	skills := make([]int, 0, 4)
	for i := 0; i < 4 && bodyLen >= 4+4*(i+1); i++ {
		sid := int(binary.BigEndian.Uint32(body[4+4*i : 4+4*(i+1)]))
		skills = append(skills, sid)
	}
	logger.Info(fmt.Sprintf("[2312] 解析 catchTime=%d skills=%v", catchTime, skills))

	for i := 0; i < 4; i++ {
		sid := 0
		if i < len(skills) {
			sid = skills[i]
		}
		if sid > 1 && skillMgr.Exists(sid) {
			finalSkills[i] = sid
		} else {
			finalSkills[i] = currentSkills[i]
		}
	}
	// 去重：同一技能只保留第一次出现的槽位，后面重复的槽位恢复为当前技能
	seen := make(map[int]int)
	for i := 0; i < 4; i++ {
		sid := finalSkills[i]
		if sid <= 0 {
			continue
		}
		if _, ok := seen[sid]; ok {
			finalSkills[i] = currentSkills[i]
			if finalSkills[i] == sid {
				finalSkills[i] = 0
			}
		} else {
			seen[sid] = i
		}
	}
	picked.Skills = finalSkills
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
		ctx.GameServer.UserDB.SaveToFile() // 立即持久化到磁盘，确保重新进入游戏后技能不丢失
	}
	resp := make([]byte, 4)
	binary.BigEndian.PutUint32(resp[0:4], 0)
	ctx.GameServer.SendResponse(ctx.ClientData, 2312, ctx.UserID, ctx.SeqID, resp)
	logger.Info(fmt.Sprintf("[2312] 精灵技能切换成功: catchTime=%d petID=%d skills=%v (已去重)", catchTime, picked.ID, picked.Skills))
}

// handleGetPetSkill CMD 2336 获取精灵技能（技能唤醒仪）
// 请求: catchTime(4)
// 响应: count(4) + [skillId(4)]*4，客户端用于 getCanStudySkill 回调
func handleGetPetSkill(ctx *gameserver.HandlerContext) {
	var catchTime uint32
	if len(ctx.Body) >= 4 {
		catchTime = binary.BigEndian.Uint32(ctx.Body[0:4])
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)

	var picked *userdb.Pet
	if catchTime != 0 {
		for i := range user.Pets {
			if uint32(user.Pets[i].CatchTime) == catchTime {
				picked = &user.Pets[i]
				break
			}
		}
		if picked == nil && user.StoragePets != nil {
			for i := range user.StoragePets {
				if uint32(user.StoragePets[i].CatchTime) == catchTime {
					picked = &user.StoragePets[i]
					break
				}
			}
		}
	}
	if picked == nil && len(user.Pets) > 0 {
		picked = &user.Pets[0]
	}

	petID := 7
	level := 5
	if picked != nil {
		petID = picked.ID
		if picked.Level > 0 {
			level = picked.Level
		}
	}

	petMgr := gamepets.GetInstance()
	var rawSkills []int

	// 1) 规范化当前携带技能：旧存档里很多精灵的 p.Skills 为空，但实际技能来自 GetSkillsForLevel。
	//    为了让“已携带技能不出现在唤醒列表中”，这里在首次请求时将默认技能写回 p.Skills。
	currentSet := make(map[int]bool)
	if picked != nil {
		if len(picked.Skills) == 0 {
			base := petMgr.GetSkillsForLevel(petID, level)
			if len(base) > 0 {
				normalized := make([]int, 4)
				for i := 0; i < 4 && i < len(base); i++ {
					normalized[i] = base[i]
				}
				picked.Skills = normalized
				if ctx.GameServer.UserDB != nil {
					ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
				}
			}
		}
		for i := 0; i < 4 && i < len(picked.Skills); i++ {
			if picked.Skills[i] > 0 {
				currentSet[picked.Skills[i]] = true
			}
		}
	}

	// 2) 基于当前等级可学技能列表，过滤掉已经携带的技能，只返回“可学习但尚未携带”的技能供选择。
	allLearnable := petMgr.GetSkillsForLevel(petID, level)
	for _, sid := range allLearnable {
		if sid <= 0 {
			continue
		}
		if currentSet[sid] {
			continue
		}
		rawSkills = append(rawSkills, sid)
	}

	// 客户端 PetManager.getCanStudySkill 解析：count(4) + count*skillId(4)。
	// 因此这里必须确保 count 与后续写入的 skillId 数量一致，避免 #2030（读到文件尾）。
	body := make([]byte, 0, 4+len(rawSkills)*4)
	tmp := make([]byte, 4)
	binary.BigEndian.PutUint32(tmp, uint32(len(rawSkills)))
	body = append(body, tmp...)
	for _, sid := range rawSkills {
		binary.BigEndian.PutUint32(tmp, uint32(sid))
		body = append(body, tmp...)
	}

	ctx.GameServer.SendResponse(ctx.ClientData, 2336, ctx.UserID, ctx.SeqID, body)
	logger.Info(fmt.Sprintf("[2336] 精灵技能(可学习列表，已过滤当前携带技能): catchTime=%d petID=%d lv=%d skills=%v", catchTime, petID, level, rawSkills))
}

// handlePetRoomInfo CMD 2325 精灵房间信息（精灵简略信息面板）
// 对齐 Lua: pet_advanced_handlers.handlePetRoomInfo
// 请求: ownerId(4) + catchTime(4)
// 响应结构（与 Lua 一致）：
// ownerId(4) catchTime(4) petId(4) nature(4) level(4)
// hp(4) atk(4) def(4) spAtk(4) spDef(4) speed(4)
// skillCount(4) + [skillId(4) pp(4)]*N
// ev_hp(4) ev_atk(4) ev_def(4) ev_sa(4) ev_sd(4) ev_sp(4)
// effNum(2)
func handlePetRoomInfo(ctx *gameserver.HandlerContext) {
	var ownerID uint32
	var catchTime uint32
	if len(ctx.Body) >= 4 {
		ownerID = binary.BigEndian.Uint32(ctx.Body[0:4])
	}
	if len(ctx.Body) >= 8 {
		catchTime = binary.BigEndian.Uint32(ctx.Body[4:8])
	}
	if ownerID == 0 {
		ownerID = uint32(ctx.UserID)
	}

	// 按 ownerID 查精灵：点别人跟随精灵时请求带 owner+catchTime，必须查该 owner 的精灵而非请求者的
	source := ctx.GameServer.GetOrCreateUser(int64(ownerID))

	if catchTime == 0 && len(source.Pets) > 0 {
		catchTime = uint32(source.Pets[0].CatchTime)
	}

	var picked *userdb.Pet
	if catchTime != 0 {
		for i := range source.Pets {
			if uint32(source.Pets[i].CatchTime) == catchTime {
				picked = &source.Pets[i]
				break
			}
		}
		if picked == nil && source.StoragePets != nil {
			for i := range source.StoragePets {
				if uint32(source.StoragePets[i].CatchTime) == catchTime {
					picked = &source.StoragePets[i]
					break
				}
			}
		}
	}
	if picked == nil && len(source.Pets) > 0 {
		picked = &source.Pets[0]
	}

	// 默认值
	petID := 7
	level := 5
	dv := 31
	nature := 0
	if picked != nil {
		petID = picked.ID
		if picked.Level > 0 {
			level = picked.Level
		}
		if picked.DV > 0 {
			dv = picked.DV
		}
		nature = picked.Nature
	}

	petMgr := gamepets.GetInstance()
	// 获取 EV（如果 Pet 存在则使用其 EV，否则为 0）
	ev := gamepets.EVStats{}
	if picked != nil {
		ev = picked.GetEVStats()
	}
	stats := petMgr.GetStats(petID, level, dv, ev, nature)

	// 技能（最多4个），优先使用技能唤醒仪自定义技能；PP 先用 20 兜底
	var rawSkills []int
	if picked != nil && len(picked.Skills) > 0 {
		rawSkills = make([]int, 4)
		for i := 0; i < 4 && i < len(picked.Skills); i++ {
			rawSkills[i] = picked.Skills[i]
		}
	} else {
		rawSkills = petMgr.GetSkillsForLevel(petID, level)
	}
	type skillEntry struct{ id, pp int }
	entries := make([]skillEntry, 0, 4)
	for i := 0; i < len(rawSkills) && len(entries) < 4; i++ {
		if rawSkills[i] > 0 {
			entries = append(entries, skillEntry{id: rawSkills[i], pp: 20})
		}
	}

	// 组包
	body := make([]byte, 0, 128)
	putU32 := func(v uint32) {
		t := make([]byte, 4)
		binary.BigEndian.PutUint32(t, v)
		body = append(body, t...)
	}
	putU16 := func(v uint16) {
		t := make([]byte, 2)
		binary.BigEndian.PutUint16(t, v)
		body = append(body, t...)
	}

	// nature: 后端与客户端 NatureXMLInfo 性格顺序不同，需查表转换
	putU32(ownerID)
	putU32(catchTime)
	putU32(uint32(petID))
	putU32(uint32(natureToClientID(nature)))
	putU32(uint32(level))
	putU32(uint32(stats.HP))
	putU32(uint32(stats.Attack))
	putU32(uint32(stats.Defence))
	putU32(uint32(stats.SpAtk))
	putU32(uint32(stats.SpDef))
	putU32(uint32(stats.Speed))
	putU32(uint32(len(entries)))
	for _, e := range entries {
		putU32(uint32(e.id))
		putU32(uint32(e.pp))
	}
	// EVs（学习力）
	ev = gamepets.ClampAndCapEV(ev)
	putU32(uint32(ev.HP))
	putU32(uint32(ev.Atk))
	putU32(uint32(ev.Def))
	putU32(uint32(ev.SpAtk))
	putU32(uint32(ev.SpDef))
	putU32(uint32(ev.Spd))
	// effNum
	putU16(0)

	ctx.GameServer.SendResponse(ctx.ClientData, 2325, ctx.UserID, ctx.SeqID, body)
	logger.Info(fmt.Sprintf("[2325] 房间精灵信息: owner=%d catch=%d petID=%d lv=%d", ownerID, catchTime, petID, level))
}

// handleUsePetItemOutOfFight CMD 2326 战斗外使用精灵道具
// 由前端 PetPropsPanel / PetPropClass_* 触发：
// 请求：catchTime(4) + itemId(4)
//
// 目前补齐“学习力清零道具”：
// 300037 atk清零, 300038 def清零, 300039 sa清零, 300040 sd清零, 300041 sp清零, 300042 hp清零
// 响应：uint32(0)（客户端不解析，存在即可）
// 并推送：2508 更新属性 + 2301 完整宠物信息（便于立即刷新面板）
func handleUsePetItemOutOfFight(ctx *gameserver.HandlerContext) {
	var catchTime uint32
	var itemID uint32
	if len(ctx.Body) >= 8 {
		catchTime = binary.BigEndian.Uint32(ctx.Body[0:4])
		itemID = binary.BigEndian.Uint32(ctx.Body[4:8])
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	if user == nil {
		ctx.GameServer.SendResponse(ctx.ClientData, 2326, ctx.UserID, ctx.SeqID, []byte{})
		return
	}

	// 找精灵（队伍优先，其次仓库）
	var picked *userdb.Pet
	if catchTime != 0 {
		for i := range user.Pets {
			if uint32(user.Pets[i].CatchTime) == catchTime {
				picked = &user.Pets[i]
				break
			}
		}
		if picked == nil && user.StoragePets != nil {
			for i := range user.StoragePets {
				if uint32(user.StoragePets[i].CatchTime) == catchTime {
					picked = &user.StoragePets[i]
					break
				}
			}
		}
	}
	if picked == nil && len(user.Pets) > 0 {
		picked = &user.Pets[0]
		catchTime = uint32(picked.CatchTime)
	}
	if picked == nil {
		ctx.GameServer.SendResponse(ctx.ClientData, 2326, ctx.UserID, ctx.SeqID, []byte{})
		return
	}

	// 特性开启晶片类道具：若精灵已经有特性，则直接返回空包，不消耗道具
	if (itemID == 300053 || itemID == 300063 || itemID == 300661) && picked.Trait != 0 {
		ctx.GameServer.SendResponse(ctx.ClientData, 2326, ctx.UserID, ctx.SeqID, []byte{})
		return
	}

	// 扣道具（items map key 是字符串）
	if user.Items == nil {
		user.Items = make(map[string]userdb.Item)
	}
	itemKey := strconv.FormatUint(uint64(itemID), 10)
	it, ok := user.Items[itemKey]
	if !ok || it.Count <= 0 {
		// 道具不足：返回空包即可（前端通常只关心是否触发刷新）
		ctx.GameServer.SendResponse(ctx.ClientData, 2326, ctx.UserID, ctx.SeqID, []byte{})
		return
	}
	it.Count--
	if it.Count <= 0 {
		delete(user.Items, itemKey)
	} else {
		user.Items[itemKey] = it
	}

	// 根据道具执行具体效果
	switch itemID {
	case 300024, 300075, 300630, 300659, 300727:
		// 精灵还原药剂系列：等级还原为1级，学习力重置，重新随机性格和个体值，
		// 但保留当前已携带的 4 个技能（不随等级回退而丢失）。

		// 1. 在修改等级/个体之前，先记录当前“实际携带”的 4 个技能。
		//    优先使用自定义保存的 picked.Skills；若为空，则按当前等级从模板表推导一次，
		//    这样可以把当前面板上显示的 4 个技能固化下来，后续无论等级如何变化都保持不变。
		petMgr := gamepets.GetInstance()
		currentSkills := make([]int, 4)
		if len(picked.Skills) > 0 {
			for i := 0; i < 4 && i < len(picked.Skills); i++ {
				currentSkills[i] = picked.Skills[i]
			}
		} else {
			defaults := petMgr.GetSkillsForLevel(picked.ID, picked.Level)
			for i := 0; i < 4 && i < len(defaults); i++ {
				currentSkills[i] = defaults[i]
			}
		}

		// 2. 按后端设定还原等级/个体/学习力/性格。
		// 精灵还原药剂系列：等级还原为1级，学习力重置，重新随机性格和个体值
		r := rand.New(rand.NewSource(time.Now().UnixNano()))
		picked.Level = 1
		picked.Exp = 0
		picked.DV = 1 + r.Intn(31)      // 1~31
		picked.Nature = 1 + r.Intn(25)  // 1~25
		picked.EVHP = 0
		picked.EVAttack = 0
		picked.EVDefence = 0
		picked.EVSpAtk = 0
		picked.EVSpDef = 0
		picked.EVSpeed = 0
		picked.CurrentHP = 0
		picked.SkillPP = nil

		// 3. 还原结束后，把刚才记录下来的技能写回 picked.Skills。
		//    这样 buildFullPetInfo / 战斗同步都会优先使用这里的技能，避免因等级回到 1 级而只剩下初始技能。
		picked.Skills = currentSkills
	case 300054, 300065, 300662, 300730, 300689, 300693, 300849:
		// 特性重组剂系列：给已有特性的精灵随机重置为新的特性（排除吸血/反弹），
		// 队伍与仓库中的 picked 已经指向正确位置，直接调用重置逻辑即可。
		if picked.Trait != 0 {
			_ = applyNonFuseTraitReset(user, int(catchTime))
		}
	case 300025, 300070, 300660, 300728:
		// 性格重塑胶囊系列：在不改变等级和学习力的前提下，随机生成新的性格，
		// 并让前端看到对应的能力值变化。
		//
		// 1) 随机一个新的性格（1~25），与当前不同则采用；若随机到相同则再随机若干次兜底。
		r := rand.New(rand.NewSource(time.Now().UnixNano()))
		oldNature := picked.Nature
		for i := 0; i < 5; i++ {
			newNature := 1 + r.Intn(25)
			if newNature != oldNature {
				picked.Nature = newNature
				break
			}
		}
		// 2) 保留等级 / 经验 / DV / 学习力不变，只是性格变了。
		// 后面统一的 EV 裁剪 + buildNoteUpdateProp/buildFullPetInfo
		// 会自动根据新性格重新计算能力值，满足“能力值发生对应变化”的描述。

	case 300026, 300117:
		// 降级秘药系列：使精灵的等级降低 1 级（最低不低于 1 级），
		// 且重新计算对应等级下的能力值，其他属性不变。
		if picked.Level > 1 {
			picked.Level--
			// 为避免高等级残余经验直接再次升级，这里将 Exp 清零。
			picked.Exp = 0
		}

	case 300668, 300673, 300919:
		// 天赋改造药剂系列（items.xml RandomDv="1"）：随机重置个体值 DV（1~31）
		r := rand.New(rand.NewSource(time.Now().UnixNano()))
		oldDV := picked.DV
		picked.DV = 1 + r.Intn(31)
		// 尽量避免“随机到一样”的观感问题
		for i := 0; i < 5 && picked.DV == oldDV; i++ {
			picked.DV = 1 + r.Intn(31)
		}

	case 300650, 300671:
		// 全能学习力遗忘剂系列：清空当前精灵的所有学习力（EVHP/EVAttack/.../EVSpeed 全部置 0）
		// 且让能力值相应下降。
		picked.EVHP = 0
		picked.EVAttack = 0
		picked.EVDefence = 0
		picked.EVSpAtk = 0
		picked.EVSpDef = 0
		picked.EVSpeed = 0

	case 300053, 300063, 300661:
		// 特性开启晶片系列：给当前没有特性的精灵随机分配一个特性。
		// 已有特性的情况在扣道具之前就已提前返回，这里只处理 Trait==0 的场景。
		userdb.AssignFusionTraitIfNeeded(picked)

	case 300037, 300109: // atk -> 0
		picked.EVAttack = 0
	case 300038, 300138: // def -> 0
		picked.EVDefence = 0
	case 300039, 300039+74: // sa -> 0  (300039, 300113)
		picked.EVSpAtk = 0
	case 300040, 300112: // sd -> 0
		picked.EVSpDef = 0
	case 300041, 300104: // sp -> 0
		picked.EVSpeed = 0
	case 300042, 300108: // hp -> 0
		picked.EVHP = 0
	default:
		// 其他道具暂不处理（但仍扣道具并成功返回）
	}

	// 统一裁剪一次，避免出现历史脏数据（总510/单项255）
	ev := picked.GetEVStats()
	ev = gamepets.ClampAndCapEV(ev)
	picked.EVHP = ev.HP
	picked.EVAttack = ev.Atk
	picked.EVDefence = ev.Def
	picked.EVSpAtk = ev.SpAtk
	picked.EVSpDef = ev.SpDef
	picked.EVSpeed = ev.Spd

	// 保存
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}

	// ACK：返回 UsePetItemOutOfFightInfo 结构体，供前端立即刷新属性面板
	petMgr := gamepets.GetInstance()
	petStats := petMgr.GetStats(picked.ID, picked.Level, picked.DV, ev, picked.Nature)
	body2326 := buildUsePetItemOutOfFightInfo(picked)
	ctx.GameServer.SendResponse(ctx.ClientData, 2326, ctx.UserID, ctx.SeqID, body2326)

	// 推送属性刷新
	propBody := buildNoteUpdateProp(uint32(picked.CatchTime), picked.ID, picked.Level, picked.Exp,
		petStats.MaxHP, petStats.Attack, petStats.Defence, petStats.SpAtk, petStats.SpDef, petStats.Speed, ev)
	ctx.GameServer.SendResponse(ctx.ClientData, 2508, ctx.UserID, ctx.SeqID, propBody)
	ctx.GameServer.SendResponse(ctx.ClientData, 2301, ctx.UserID, ctx.SeqID, buildFullPetInfo(*picked))
}

// handleUsePetItemFullAbilityOfStudy CMD 9278 使用“满学习力/注入”道具
// 前端请求：catchTime(4) + statIndex(4) + itemId(4) + flag(4)
// statIndex 约定（与前端一致）：
// 0=HP, 1=ATK, 2=DEF, 3=SA, 4=SD, 5=SP
// flag 通常为 0（固定注入）或 1（弹窗选择注入项）；服务端统一按“把该项 EV 设为 255”处理。
//
// 响应：uint32(0)（客户端不解析）
// 并推送：2508 更新属性 + 2301 完整宠物信息
func handleUsePetItemFullAbilityOfStudy(ctx *gameserver.HandlerContext) {
	var catchTime uint32
	var statIndex uint32
	var itemID uint32
	if len(ctx.Body) >= 16 {
		catchTime = binary.BigEndian.Uint32(ctx.Body[0:4])
		statIndex = binary.BigEndian.Uint32(ctx.Body[4:8])
		itemID = binary.BigEndian.Uint32(ctx.Body[8:12])
		// flag := binary.BigEndian.Uint32(ctx.Body[12:16]) // 目前无需使用
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	if user == nil {
		ctx.GameServer.SendResponse(ctx.ClientData, 9278, ctx.UserID, ctx.SeqID, []byte{})
		return
	}

	// 找精灵（队伍优先，其次仓库）
	var picked *userdb.Pet
	if catchTime != 0 {
		for i := range user.Pets {
			if uint32(user.Pets[i].CatchTime) == catchTime {
				picked = &user.Pets[i]
				break
			}
		}
		if picked == nil && user.StoragePets != nil {
			for i := range user.StoragePets {
				if uint32(user.StoragePets[i].CatchTime) == catchTime {
					picked = &user.StoragePets[i]
					break
				}
			}
		}
	}
	if picked == nil && len(user.Pets) > 0 {
		picked = &user.Pets[0]
		catchTime = uint32(picked.CatchTime)
	}
	if picked == nil {
		ctx.GameServer.SendResponse(ctx.ClientData, 9278, ctx.UserID, ctx.SeqID, []byte{})
		return
	}

	// 扣道具
	if user.Items == nil {
		user.Items = make(map[string]userdb.Item)
	}
	itemKey := strconv.FormatUint(uint64(itemID), 10)
	it, ok := user.Items[itemKey]
	if !ok || it.Count <= 0 {
		ctx.GameServer.SendResponse(ctx.ClientData, 9278, ctx.UserID, ctx.SeqID, []byte{})
		return
	}
	it.Count--
	if it.Count <= 0 {
		delete(user.Items, itemKey)
	} else {
		user.Items[itemKey] = it
	}

	// 设置单项 EV = 255（总和/单项裁剪在后面统一处理）
	switch statIndex {
	case 0:
		picked.EVHP = 255
	case 1:
		picked.EVAttack = 255
	case 2:
		picked.EVDefence = 255
	case 3:
		picked.EVSpAtk = 255
	case 4:
		picked.EVSpDef = 255
	case 5:
		picked.EVSpeed = 255
	default:
		// 非法 statIndex：不改数据，但仍回包（避免客户端卡死）
	}

	// 统一裁剪（总510/单项255）
	ev := picked.GetEVStats()
	ev = gamepets.ClampAndCapEV(ev)
	picked.EVHP = ev.HP
	picked.EVAttack = ev.Atk
	picked.EVDefence = ev.Def
	picked.EVSpAtk = ev.SpAtk
	picked.EVSpDef = ev.SpDef
	picked.EVSpeed = ev.Spd

	// 保存
	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}

	// ACK
	ctx.GameServer.SendResponse(ctx.ClientData, 9278, ctx.UserID, ctx.SeqID, make([]byte, 4))

	// 推送属性刷新
	petMgr := gamepets.GetInstance()
	petStats := petMgr.GetStats(picked.ID, picked.Level, picked.DV, ev, picked.Nature)
	propBody := buildNoteUpdateProp(uint32(picked.CatchTime), picked.ID, picked.Level, picked.Exp,
		petStats.MaxHP, petStats.Attack, petStats.Defence, petStats.SpAtk, petStats.SpDef, petStats.Speed, ev)
	ctx.GameServer.SendResponse(ctx.ClientData, 2508, ctx.UserID, ctx.SeqID, propBody)
	ctx.GameServer.SendResponse(ctx.ClientData, 2301, ctx.UserID, ctx.SeqID, buildFullPetInfo(*picked))
}

// handleGetSimUserInfo CMD 2051 获取简单用户信息
// 对齐 Lua: map_handlers.handleGetSimUserInfo
// 请求: targetId(4) (可选，默认使用当前用户)
// 响应: targetId(4) + nick(16) + color(4) + texture(4) + vip(4) + status(4) + mapType(4) + mapId(4) +
//
//	isCanBeTeacher(4) + teacherID(4) + studentID(4) + graduationCount(4) + vipLevel(4) +
//	teamId(4) + teamIsShow(4) + clothCount(4) + [clothId(4) + level(4)]...
func handleGetSimUserInfo(ctx *gameserver.HandlerContext) {
	targetID := ctx.UserID
	if len(ctx.Body) >= 4 {
		targetID = int64(binary.BigEndian.Uint32(ctx.Body[0:4]))
	}

	user := ctx.GameServer.GetOrCreateUser(targetID)
	if user == nil {
		user = ctx.GameServer.GetOrCreateUser(ctx.UserID)
		targetID = ctx.UserID
	}

	nick := user.Nick
	if nick == "" {
		nick = fmt.Sprintf("Seer%d", targetID)
	}

	// 构建响应
	body := make([]byte, 0, 128)
	putU32 := func(v uint32) {
		t := make([]byte, 4)
		binary.BigEndian.PutUint32(t, v)
		body = append(body, t...)
	}
	putFixedString := func(s string, n int) {
		b := []byte(s)
		if len(b) > n {
			b = b[:n]
		}
		body = append(body, b...)
		for i := len(b); i < n; i++ {
			body = append(body, 0)
		}
	}

	putU32(uint32(targetID))
	putFixedString(nick, 16)
	putU32(uint32(user.Color))
	// texture（涂鸦/头像）：客户端用此请求 doodle/prev/{id}.swf，0 表示默认
	putU32(uint32(user.Texture))

	// VIP 标志（根据 SuperNono 判断）
	vipFlag := uint32(0)
	if user.Nono.SuperNono > 0 {
		vipFlag = 1
	}
	putU32(vipFlag)
	putU32(0) // status
	putU32(0) // mapType
	mapID := user.MapID
	if mapID <= 0 {
		mapID = 1 // 客户端 mapID=0 可能异常，回 1
	}
	putU32(uint32(mapID))

	// 师徒相关
	isCanBeTeacher := uint32(0) // 简化：默认不可当老师
	putU32(isCanBeTeacher)
	putU32(uint32(user.TeacherID))
	putU32(uint32(user.StudentID))
	putU32(uint32(user.GraduationCount))
	putU32(uint32(user.Nono.VipLevel))

	// 战队相关（当前未实现）
	putU32(0) // teamId
	putU32(0) // teamIsShow

	// 服装列表
	putU32(uint32(len(user.Clothes)))
	for _, clothID := range user.Clothes {
		putU32(uint32(clothID))
		putU32(0) // level
	}

	ctx.GameServer.SendResponse(ctx.ClientData, 2051, ctx.UserID, ctx.SeqID, body)
	logger.Info(fmt.Sprintf("[2051] 获取简单用户信息: targetID=%d", targetID))
}

// handleGetMoreUserInfo CMD 2052 获取详细用户信息
// 对齐 Lua: map_handlers.handleGetMoreUserInfo
// 请求: targetId(4) (可选，默认使用当前用户)
// 响应: targetId(4) + nick(16) + regTime(4) + petAllNum(4) + petMaxLev(4) + bossAchievement(200) +
//
//	graduationCount(4) + monKingWin(4) + messWin(4) + maxStage(4) + maxArenaWins(4) + curTitle(4)
func handleGetMoreUserInfo(ctx *gameserver.HandlerContext) {
	targetID := ctx.UserID
	if len(ctx.Body) >= 4 {
		targetID = int64(binary.BigEndian.Uint32(ctx.Body[0:4]))
	}

	user := ctx.GameServer.GetOrCreateUser(targetID)
	if user == nil {
		user = ctx.GameServer.GetOrCreateUser(ctx.UserID)
		targetID = ctx.UserID
	}

	nick := user.Nick
	if nick == "" {
		nick = fmt.Sprintf("Seer%d", targetID)
	}

	// 获取注册时间（从 UserDB 或使用默认值）
	regTime := uint32(time.Now().Unix() - 86400*365)
	if ctx.GameServer.UserDB != nil {
		account := ctx.GameServer.UserDB.FindByUserID(targetID)
		if account != nil && account.RegisterTime > 0 {
			regTime = uint32(account.RegisterTime)
		}
	}

	// 构建响应
	body := make([]byte, 0, 256)
	putU32 := func(v uint32) {
		t := make([]byte, 4)
		binary.BigEndian.PutUint32(t, v)
		body = append(body, t...)
	}
	putFixedString := func(s string, n int) {
		b := []byte(s)
		if len(b) > n {
			b = b[:n]
		}
		body = append(body, b...)
		for i := len(b); i < n; i++ {
			body = append(body, 0)
		}
	}

	// petAllNum：无则用队伍+仓库精灵数；petMaxLev：无则用 100，对齐 Lua user.petAllNum or 0 / user.petMaxLev or 100
	petAllNum := user.PetAllNum
	if petAllNum <= 0 {
		petAllNum = len(user.Pets) + len(user.StoragePets)
	}
	petMaxLev := user.PetMaxLev
	if petMaxLev <= 0 {
		petMaxLev = 100
	}

	putU32(uint32(targetID))
	putFixedString(nick, 16)
	putU32(regTime)
	putU32(uint32(petAllNum))
	putU32(uint32(petMaxLev))
	body = append(body, sptboss.BuildBossAchievement(user.DefeatedSPTBossIds)...) // bossAchievement 200 字节
	putU32(uint32(user.GraduationCount))
	putU32(uint32(user.MonKingWin))
	putU32(uint32(user.MessWin))
	putU32(uint32(user.MaxStage))
	putU32(uint32(user.MaxArenaWins))
	putU32(uint32(user.CurTitle))

	ctx.GameServer.SendResponse(ctx.ClientData, 2052, ctx.UserID, ctx.SeqID, body)
	logger.Info(fmt.Sprintf("[2052] 获取详细用户信息: targetID=%d", targetID))
}

// handleFitmentUsering CMD 10006 正在使用的家具（基地）
// 对齐 Lua: room_handlers.handleFitmentUsering
// 请求: targetUserId(4) (可选，默认使用当前用户)
// 响应: userID(4) + roomID(4) + count(4) + [id(4) + x(4) + y(4) + dir(4) + status(4)]...
func handleFitmentUsering(ctx *gameserver.HandlerContext) {
	targetUserID := ctx.UserID
	if len(ctx.Body) >= 4 {
		targetUserID = int64(binary.BigEndian.Uint32(ctx.Body[0:4]))
	}

	user := ctx.GameServer.GetOrCreateUser(targetUserID)
	if user == nil {
		user = ctx.GameServer.GetOrCreateUser(ctx.UserID)
		targetUserID = ctx.UserID
	}

	// 获取家具列表
	fitments := user.Fitments
	if fitments == nil {
		fitments = []userdb.Fitment{}
	}

	roomID := targetUserID // 房间ID默认使用用户ID

	// 构建响应
	body := make([]byte, 0, 12+len(fitments)*20)
	putU32 := func(v uint32) {
		t := make([]byte, 4)
		binary.BigEndian.PutUint32(t, v)
		body = append(body, t...)
	}

	putU32(uint32(targetUserID))  // userID (房主)
	putU32(uint32(roomID))        // roomID
	putU32(uint32(len(fitments))) // count

	// 添加家具列表
	for _, fitment := range fitments {
		putU32(uint32(fitment.ID))
		putU32(uint32(fitment.X))
		putU32(uint32(fitment.Y))
		putU32(uint32(fitment.Dir))
		putU32(uint32(fitment.Status))
	}

	ctx.GameServer.SendResponse(ctx.ClientData, 10006, ctx.UserID, ctx.SeqID, body)
	logger.Info(fmt.Sprintf("[10006] 正在使用的家具: owner=%d visitor=%d count=%d", targetUserID, ctx.UserID, len(fitments)))
}

// handleBuyFitment CMD 10004 BUY_FITMENT 购买基地家具/房型
// 前端 BuyCmdListener.onFitmentBuy 解析：
// coins(4) + itemId(4) + unknown(4)
// 这里对齐：扣赛尔豆、将家具/房型加入仓库（items），并回包 12 字节。
func handleBuyFitment(ctx *gameserver.HandlerContext) {
	// 请求格式在不同面板可能略有差异，但至少包含 itemId(4) 与数量(4)/价格(4) 等字段。
	// 这里做兼容：优先取 body[0:4] 作为 itemId；其余字段不强依赖。
	if len(ctx.Body) < 4 {
		ctx.GameServer.SendResponse(ctx.ClientData, 10004, ctx.UserID, ctx.SeqID, []byte{})
		return
	}

	itemID := int(binary.BigEndian.Uint32(ctx.Body[0:4]))
	if itemID <= 0 {
		ctx.GameServer.SendResponse(ctx.ClientData, 10004, ctx.UserID, ctx.SeqID, []byte{})
		return
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	if user.Items == nil {
		user.Items = make(map[string]userdb.Item)
	}

	// 从 items.xml 的 ItemNames 只拿名字，不包含价格；这里用客户端发来的 price 兜底，
	// 若没有 price 字段，则不扣款直接发放（避免购买流程卡死）。
	price := 0
	if len(ctx.Body) >= 12 {
		price = int(binary.BigEndian.Uint32(ctx.Body[8:12]))
	}
	if price <= 0 && len(ctx.Body) >= 8 {
		// 有些面板第二个字段就是 price
		price = int(binary.BigEndian.Uint32(ctx.Body[4:8]))
	}

	if price > 0 {
		if user.Coins < price {
			// 余额不足：按老逻辑直接空回包，前端会提示失败/不更新
			ctx.GameServer.SendResponse(ctx.ClientData, 10004, ctx.UserID, ctx.SeqID, []byte{})
			return
		}
		user.Coins -= price
		if user.Coins < 0 {
			user.Coins = 0
		}
	}

	// 加入背包仓库 items（FitmentManager.addInStorage 走物品仓库存储）
	itemKey := strconv.Itoa(itemID)
	it := user.Items[itemKey]
	it.Count += 1
	if it.ExpireTime == 0 {
		it.ExpireTime = 0x057E40
	}
	user.Items[itemKey] = it

	// 若为服装/套装也要加入 Clothes（基地房型/摆件通常不在此范围，但顺手兼容）
	addClothIfNeeded(user, itemID)

	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}

	// 回包：coins(4) + itemId(4) + unknown(4)
	resp := make([]byte, 12)
	binary.BigEndian.PutUint32(resp[0:4], uint32(user.Coins))
	binary.BigEndian.PutUint32(resp[4:8], uint32(itemID))
	binary.BigEndian.PutUint32(resp[8:12], 1)
	ctx.GameServer.SendResponse(ctx.ClientData, 10004, ctx.UserID, ctx.SeqID, resp)
	logger.Info(fmt.Sprintf("[10004] BUY_FITMENT: UID=%d itemID=%d price=%d coins=%d", ctx.UserID, itemID, price, user.Coins))
}

// handleFitmentAll CMD 10007 FITMENT_ALL 基地家具仓库列表
// 前端 FitmentManager.getStorageInfo 解析：
// count(4) + count×(id(4)+usedCount(4)+allCount(4))
func handleFitmentAll(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	if user.Items == nil {
		user.Items = make(map[string]userdb.Item)
	}
	if user.Fitments == nil {
		user.Fitments = []userdb.Fitment{}
	}

	// Fitment 仓库来源：items 中的基地家具（500001+）以及已摆放列表 user.Fitments（用于 usedCount 统计）
	usedCountByID := make(map[int]int)
	for _, f := range user.Fitments {
		if f.ID > 0 {
			usedCountByID[f.ID]++
		}
	}

	type row struct {
		id   int
		used int
		all  int
	}
	rows := make([]row, 0)
	for k, it := range user.Items {
		id, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		// 基地房型/摆件/家具基本都在 500001+ 段（见 FitmentInfo.id->type 的判断）
		if id < 500001 || it.Count <= 0 {
			continue
		}
		rows = append(rows, row{
			id:   id,
			used: usedCountByID[id],
			all:  it.Count,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].id < rows[j].id })

	body := make([]byte, 4+len(rows)*12)
	binary.BigEndian.PutUint32(body[0:4], uint32(len(rows)))
	off := 4
	for _, r := range rows {
		binary.BigEndian.PutUint32(body[off:off+4], uint32(r.id))
		off += 4
		binary.BigEndian.PutUint32(body[off:off+4], uint32(r.used))
		off += 4
		binary.BigEndian.PutUint32(body[off:off+4], uint32(r.all))
		off += 4
	}
	ctx.GameServer.SendResponse(ctx.ClientData, 10007, ctx.UserID, ctx.SeqID, body)
	logger.Info(fmt.Sprintf("[10007] FITMENT_ALL: UID=%d count=%d", ctx.UserID, len(rows)))
}

// handleSetFitment CMD 10008 SET_FITMENT 保存基地布置/设置房型
// 前端 FitmentManager.saveInfo / saveRoomType 发送：
// roomID(4) + count(4) + count×(id(4)+x(4)+y(4)+dir(4)+status(4))
// 这里保存到 user.Fitments，并空回包即可（前端只等回包触发回调，不解析内容）。
func handleSetFitment(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	if len(ctx.Body) < 8 {
		ctx.GameServer.SendResponse(ctx.ClientData, 10008, ctx.UserID, ctx.SeqID, []byte{})
		return
	}
	count := int(binary.BigEndian.Uint32(ctx.Body[4:8]))
	if count < 0 {
		count = 0
	}
	offset := 8
	fitments := make([]userdb.Fitment, 0, count)
	for i := 0; i < count; i++ {
		if offset+20 > len(ctx.Body) {
			break
		}
		id := int(binary.BigEndian.Uint32(ctx.Body[offset : offset+4]))
		x := int(binary.BigEndian.Uint32(ctx.Body[offset+4 : offset+8]))
		y := int(binary.BigEndian.Uint32(ctx.Body[offset+8 : offset+12]))
		dir := int(binary.BigEndian.Uint32(ctx.Body[offset+12 : offset+16]))
		status := int(binary.BigEndian.Uint32(ctx.Body[offset+16 : offset+20]))
		offset += 20
		if id <= 0 {
			continue
		}
		fitments = append(fitments, userdb.Fitment{
			ID:     id,
			X:      x,
			Y:      y,
			Dir:    dir,
			Status: status,
		})
	}

	// 兼容“换房型”：前端 StoragePanel 会调用 FitmentManager.saveRoomType，
	// 该请求只包含 1 个 FRAME（房型）而不是完整 usedList。
	// 若我们直接覆盖 user.Fitments，会把玩家已摆放的所有摆件/家具清空，导致房型/摆件表现异常。
	// 因此：
	// - 若本次仅包含 1 个房型(500001~500100)，则仅替换旧房型，保留其余摆件；
	// - 否则视为完整保存，覆盖 user.Fitments。
	if len(fitments) == 1 && fitments[0].ID >= 500001 && fitments[0].ID <= 500100 {
		newFrame := fitments[0]
		merged := make([]userdb.Fitment, 0)
		if user.Fitments != nil {
			for _, f := range user.Fitments {
				// 移除旧房型
				if f.ID >= 500001 && f.ID <= 500100 {
					continue
				}
				merged = append(merged, f)
			}
		}
		merged = append(merged, newFrame)
		user.Fitments = merged
		logger.Info(fmt.Sprintf("[10008] SET_FITMENT(换房型): UID=%d frameID=%d keepOthers=%d", ctx.UserID, newFrame.ID, len(merged)-1))
	} else {
		user.Fitments = fitments
	}

	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}
	ctx.GameServer.SendResponse(ctx.ClientData, 10008, ctx.UserID, ctx.SeqID, []byte{})
	logger.Info(fmt.Sprintf("[10008] SET_FITMENT: UID=%d count=%d", ctx.UserID, len(fitments)))
}

// handleLeiyiTrainGetStatus CMD 2393 LEIYI_TRAIN_GET_STATUS
// 前端 LeiyiEnergyNewPanel.setup 解析：循环6次读取 today/current/total（每次 3*uint32）
// 因此响应体必须为 6 * 3 * 4 = 72 字节。
//
// 本地服实现：先给一个合理默认值，避免面板报错：
// - today: 0
// - current: 0
// - total: 10（每项可训练次数）
// 后续若需要持久化进度，可接入 userdb.Tasks 或单独字段。
func handleLeiyiTrainGetStatus(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)
	lt, changed := ensureLeiyiTrainProgress(user)
	if changed && ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}

	const items = 6
	body := make([]byte, items*3*4)
	off := 0
	for i := 0; i < items; i++ {
		binary.BigEndian.PutUint32(body[off:off+4], uint32(lt.Today[i]))
		off += 4
		binary.BigEndian.PutUint32(body[off:off+4], uint32(lt.Current[i]))
		off += 4
		binary.BigEndian.PutUint32(body[off:off+4], uint32(lt.Total[i]))
		off += 4
	}
	ctx.GameServer.SendResponse(ctx.ClientData, 2393, ctx.UserID, ctx.SeqID, body)
	logger.Info(fmt.Sprintf("[2393] 雷伊特训状态: UID=%d today=%v current=%v total=%v", ctx.UserID, lt.Today, lt.Current, lt.Total))
}

func ensureLeiyiTrainProgress(user *userdb.GameData) (*userdb.LeiyiTrainProgress, bool) {
	const items = 6
	// 面板显示的目标次数（对应前端6个按钮）：体力60、防御20、特防20、攻击20、特攻10、速度20
	defaultTotals := [items]int{60, 20, 20, 20, 10, 20}
	changed := false
	if user.LeiyiTrain == nil {
		user.LeiyiTrain = &userdb.LeiyiTrainProgress{}
		changed = true
	}
	lt := user.LeiyiTrain
	day := time.Now().Unix() / 86400
	if lt.Date != day {
		lt.Date = day
		lt.Today = make([]int, items)
		changed = true
	}
	ensureLen := func(s []int, fill int) []int {
		if s == nil {
			out := make([]int, items)
			for i := 0; i < items; i++ {
				out[i] = fill
			}
			changed = true
			return out
		}
		if len(s) < items {
			out := make([]int, items)
			copy(out, s)
			for i := len(s); i < items; i++ {
				out[i] = fill
			}
			changed = true
			return out
		}
		if len(s) > items {
			changed = true
			return s[:items]
		}
		return s
	}
	lt.Today = ensureLen(lt.Today, 0)
	lt.Current = ensureLen(lt.Current, 0)
	// total 需要每项不同，不能用统一 fill
	if lt.Total == nil || len(lt.Total) != items {
		lt.Total = make([]int, items)
		copy(lt.Total, defaultTotals[:])
		changed = true
	} else {
		for i := 0; i < items; i++ {
			if lt.Total[i] <= 0 {
				lt.Total[i] = defaultTotals[i]
				changed = true
			}
		}
	}
	return lt, changed
}

func applyLeiyiTrainReward(ctx *gameserver.HandlerContext, user *userdb.GameData, battle *gameserver.BattleState) bool {
	const (
		items            = 6
		baseParam        = 10000
		perCompletionBonus = 1
		maxBonusPerStat    = 255
		leiyiPetID       = 70
	)
	idx := int(battle.BossRegion) - baseParam
	if idx < 0 || idx >= items {
		return false
	}

	// 找到本场出战雷伊（优先 ActivePetCatchTime），给对应“面板能力值加成”
	var p *userdb.Pet
	if battle.ActivePetCatchTime != 0 {
		for i := range user.Pets {
			if user.Pets[i].CatchTime == battle.ActivePetCatchTime {
				p = &user.Pets[i]
				break
			}
		}
	}
	if p == nil {
		for i := range user.Pets {
			if user.Pets[i].ID == leiyiPetID {
				p = &user.Pets[i]
				break
			}
		}
	}
	if p == nil || p.ID != leiyiPetID {
		logger.Warning(fmt.Sprintf("[LeiyiTrain] 本场非雷伊出战，跳过次数与能力提升: UID=%d idx=%d activeCatch=%d", ctx.UserID, idx, battle.ActivePetCatchTime))
		return false
	}

	lt, changed := ensureLeiyiTrainProgress(user)
	if lt.Current[idx] >= lt.Total[idx] {
		// 已完成该项，不再增加
		return changed
	}
	lt.Today[idx]++
	lt.Current[idx]++
	changed = true

	addTo := func(cur *int) int {
		if *cur >= maxBonusPerStat {
			return 0
		}
		remain := maxBonusPerStat - *cur
		x := perCompletionBonus
		if x > remain {
			x = remain
		}
		*cur += x
		return x
	}

	added := 0
	switch idx {
	case 0: // 体力
		added = addTo(&p.BonusHP)
	case 1: // 防御
		added = addTo(&p.BonusDefence)
	case 2: // 特防
		added = addTo(&p.BonusSpDef)
	case 3: // 攻击
		added = addTo(&p.BonusAttack)
	case 4: // 特攻
		added = addTo(&p.BonusSpAtk)
	case 5: // 速度
		added = addTo(&p.BonusSpeed)
	}

	if added > 0 {
		logger.Info(fmt.Sprintf("[LeiyiTrain] 特训完成 idx=%d Bonus+%d (catchTime=%d) -> bHP=%d bAtk=%d bDef=%d bSpA=%d bSpD=%d bSpd=%d",
			idx, added, p.CatchTime, p.BonusHP, p.BonusAttack, p.BonusDefence, p.BonusSpAtk, p.BonusSpDef, p.BonusSpeed))
	} else {
		logger.Info(fmt.Sprintf("[LeiyiTrain] 特训完成 idx=%d 但对应Bonus已达上限或无法增加: UID=%d catchTime=%d", idx, ctx.UserID, p.CatchTime))
	}
	return changed
}

func applyPetStatBonus(stats gamepets.Stats, p *userdb.Pet) gamepets.Stats {
	if p == nil {
		return stats
	}
	stats.MaxHP += p.BonusHP
	stats.HP += p.BonusHP
	stats.Attack += p.BonusAttack
	stats.Defence += p.BonusDefence
	stats.SpAtk += p.BonusSpAtk
	stats.SpDef += p.BonusSpDef
	stats.Speed += p.BonusSpeed
	// 防止出现 0/负数
	if stats.MaxHP < 1 {
		stats.MaxHP = 1
	}
	if stats.HP < 1 {
		stats.HP = 1
	}
	if stats.Attack < 1 {
		stats.Attack = 1
	}
	if stats.Defence < 1 {
		stats.Defence = 1
	}
	if stats.SpAtk < 1 {
		stats.SpAtk = 1
	}
	if stats.SpDef < 1 {
		stats.SpDef = 1
	}
	if stats.Speed < 1 {
		stats.Speed = 1
	}
	return stats
}

func pushLearnSpecialSkillNoticeIfNeeded(ctx *gameserver.HandlerContext, user *userdb.GameData, battle *gameserver.BattleState) {
	if ctx == nil || user == nil || battle == nil {
		return
	}
	if battle.OpponentUserID != 0 {
		return
	}

	// 当前出战精灵必须是雷伊
	const leiyiID = 70
	var p *userdb.Pet
	if battle.ActivePetCatchTime != 0 {
		for i := range user.Pets {
			if user.Pets[i].CatchTime == battle.ActivePetCatchTime {
				p = &user.Pets[i]
				break
			}
		}
	}
	if p == nil && battle.ActivePetIndex >= 0 && battle.ActivePetIndex < len(user.Pets) {
		p = &user.Pets[battle.ActivePetIndex]
	}
	if p == nil || p.ID != leiyiID {
		return
	}

	hasSkill := func(skillID int) bool {
		for _, sid := range p.Skills {
			if sid == skillID {
				return true
			}
		}
		return false
	}
	used := func(skillID int) bool {
		return battle.PlayerUsedSkills != nil && battle.PlayerUsedSkills[skillID]
	}
	push2510 := func(rewardSkillID int) {
		if rewardSkillID <= 0 || hasSkill(rewardSkillID) {
			return
		}
		body := make([]byte, 16)
		binary.BigEndian.PutUint32(body[0:4], 1)
		binary.BigEndian.PutUint32(body[4:8], uint32(p.CatchTime))
		binary.BigEndian.PutUint32(body[8:12], 0)
		binary.BigEndian.PutUint32(body[12:16], uint32(rewardSkillID))
		ctx.GameServer.SendResponse(ctx.ClientData, 2510, ctx.UserID, 0, body)
		logger.Info(fmt.Sprintf("[2510] 推送特殊技能学习提示: UID=%d catchTime=%d petID=%d rewardSkill=%d map=%d region=%d enemy=%d used=%v round=%d",
			ctx.UserID, p.CatchTime, p.ID, rewardSkillID, battle.BattleMapID, battle.BossRegion, battle.EnemyID, battle.PlayerUsedSkills, battle.RoundCount))
	}

	// 雷伊技能特训（LeiyiTrainController.initTrain_0..4）
	// 规则：打指定 BOSS 胜利，并满足“使用指定技能/回合数/麻痹”等条件后，由服务端推送 2510 触发学习面板。
	switch {
	// 0) 里奥斯：用 白光刃(10170) 击败里奥斯(42) -> 学 极光刃(10823)
	case battle.BattleMapID == 17 && battle.BossRegion == 1 && battle.EnemyID == 42:
		if used(10170) {
			push2510(10823)
		}

	// 1) 提亚斯：用 雷雨天(20086) 击败提亚斯(69) -> 学 闪电斗气(20363)
	case battle.BattleMapID == 27 && battle.BossRegion == 1 && battle.EnemyID == 69:
		if used(20086) {
			push2510(20363)
		}

	// 2) 雷纳多：用 极电千鸟(10175) 击败雷纳多(113) -> 学 元气电光球(10824)
	case battle.BattleMapID == 49 && battle.BossRegion == 1 && battle.EnemyID == 113:
		if used(10175) {
			push2510(10824)
		}

	// 3) 阿克希亚：承受 10 回合攻击，并在其麻痹状态下击败阿克希亚(50) -> 学 雷神觉醒(20364)
	case battle.BattleMapID == 40 && battle.BossRegion == 1 && battle.EnemyID == 50:
		if battle.RoundCount >= 10 && battle.EnemyStatus[gameskills.StatusIndexParalysis] > 0 {
			push2510(20364)
		}

	// 4) 雷伊倒影：击败倒影雷伊(5005)，param2=10006 -> 学 雷神天明闪(10825)
	case battle.BattleMapID == 32 && battle.BossRegion == 10006 && battle.EnemyID == 5005:
		push2510(10825)
	}
}
