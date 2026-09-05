package pets

import (
	"encoding/xml"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/seer-game/golang-version/internal/core/logger"
)

// Pet 精灵数据
type Pet struct {
	ID             int             `xml:"ID,attr"`
	DefName        string          `xml:"DefName,attr"`
	Type           int             `xml:"Type,attr"`
	Type2          int             `xml:"Type2,attr"`
	HP             int             `xml:"HP,attr"`
	Atk            int             `xml:"Atk,attr"`
	Def            int             `xml:"Def,attr"`
	SpAtk          int             `xml:"SpAtk,attr"`
	SpDef          int             `xml:"SpDef,attr"`
	Spd            int             `xml:"Spd,attr"`
	EvolvesFrom    int             `xml:"EvolvesFrom,attr"`
	EvolvesTo      int             `xml:"EvolvesTo,attr"`
	EvolvingLv     int             `xml:"EvolvingLv,attr"`
	EvolvFlag      int             `xml:"EvolvFlag,attr"`
	EvolvItem      int             `xml:"EvolvItem,attr"`
	EvolvItemCount int             `xml:"EvolvItemCount,attr"`
	EvolveBabin    int             `xml:"EvolveBabin,attr"`
	CatchRate      int             `xml:"CatchRate,attr"`
	FreeForbidden  int             `xml:"FreeForbidden,attr"`
	YieldingExp    int             `xml:"YieldingExp,attr"`
	YieldingEV     string          `xml:"YieldingEV,attr"`
	GrowthType     int             `xml:"GrowthType,attr"`
	IsRareMon      int             `xml:"IsRareMon,attr"`
	IsDark         int             `xml:"IsDark,attr"`
	IsAbilityMon   int             `xml:"IsAbilityMon,attr"`
	VariationID    int             `xml:"VariationID,attr"`
	Breedingmon    int             `xml:"breedingmon,attr"`
	Supermon       int             `xml:"supermon,attr"`
	// IsFuseMon: 1 表示融合精灵（当前 golang_version/data/spt.xml 使用该字段标记融合精灵）
	IsFuseMon      int             `xml:"IsFuseMon,attr"`
	RealId         int             `xml:"RealId,attr"`
	Transform      string          `xml:"Transform,attr"`
	FormParam      float64         `xml:"FormParam,attr"`
	GradeParam     float64         `xml:"GradeParam,attr"`
	AddSeParam     int             `xml:"AddSeParam,attr"`
	ModifyPower    int             `xml:"ModifyPower,attr"`
	IsRidePet      int             `xml:"isRidePet,attr"`
	IsFlyPet       int             `xml:"isFlyPet,attr"`
	Scale          float64         `xml:"scale,attr"`
	NameY          float64         `xml:"nameY,attr"`
	Speed          float64         `xml:"speed,attr"`
	Gender         int             `xml:"Gender,attr"`
	PetClass       int             `xml:"PetClass,attr"`
	VipBtlAdj      int             `xml:"VipBtlAdj,attr"`
	Resist         int             `xml:"Resist,attr"`
	Combo          string          `xml:"Combo,attr"`
	LearnableMoves []LearnableMove `xml:"LearnableMoves>Move"`
	ExtraMoves     []int           `xml:"ExtraMoves>Move>ID"`
	RecMoves       []RecMove       `xml:"Rec>Move"`
	OthMoves       []int           `xml:"Oth>Move>ID"`
	AdvMove        []int           `xml:"AdvMove>Move>ID"`
	NaturalEnemy   []int           `xml:"NaturalEnemy>Enemy>ID"`
}

// LearnableMove 可学习技能
type LearnableMove struct {
	ID    int `xml:"ID,attr"`
	Level int `xml:"LearningLv,attr"`
}

// RecMove 推荐技能
type RecMove struct {
	ID  int `xml:"ID,attr"`
	Tag int `xml:"Tag,attr"`
}

// EVStats 努力值
type EVStats struct {
	HP    int
	Atk   int
	Def   int
	SpAtk int
	SpDef int
	Spd   int
}

// ClampAndCapEV 按规则裁剪学习力：
// - 单项范围 [0,255]
// - 总和上限 510（若超过则按固定顺序扣减到 510）
//
// 说明：你给的属性公式直接使用 EV 值参与 floor(Lv*EV/100) 计算，因此这里不做 /4。
func ClampAndCapEV(ev EVStats) EVStats {
	clamp := func(v int) int {
		if v < 0 {
			return 0
		}
		if v > 255 {
			return 255
		}
		return v
	}
	ev.HP = clamp(ev.HP)
	ev.Atk = clamp(ev.Atk)
	ev.Def = clamp(ev.Def)
	ev.SpAtk = clamp(ev.SpAtk)
	ev.SpDef = clamp(ev.SpDef)
	ev.Spd = clamp(ev.Spd)

	sum := ev.HP + ev.Atk + ev.Def + ev.SpAtk + ev.SpDef + ev.Spd
	if sum <= 510 {
		return ev
	}

	// 超出则扣减（固定顺序，保证可预测）
	over := sum - 510
	dec := func(v *int) {
		if over <= 0 {
			return
		}
		d := *v
		if d > over {
			d = over
		}
		*v -= d
		over -= d
	}
	dec(&ev.Spd)
	dec(&ev.SpDef)
	dec(&ev.SpAtk)
	dec(&ev.Def)
	dec(&ev.Atk)
	dec(&ev.HP)
	return ev
}

// Stats 精灵属性
type Stats struct {
	HP      int
	MaxHP   int
	Attack  int
	Defence int
	SpAtk   int
	SpDef   int
	Speed   int
}

// ExpInfo 经验信息
type ExpInfo struct {
	CurrentLevelExp int
	NextLevelExp    int
	TotalExp        int
}

// Pets 精灵管理器
type Pets struct {
	pets   map[int]*Pet
	loaded bool
	mu     sync.RWMutex
}

// All 返回所有已加载的精灵定义（仅用于只读场景，如地图刷怪配置）
func (p *Pets) All() []*Pet {
	if !p.loaded {
		if err := p.Load(); err != nil {
			logger.Warning("精灵数据加载失败(All)")
			return nil
		}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()

	result := make([]*Pet, 0, len(p.pets))
	for _, pet := range p.pets {
		if pet != nil {
			result = append(result, pet)
		}
	}
	return result
}

var (
	instance *Pets
	once     sync.Once
)

// New 创建精灵管理器实例
func New() *Pets {
	once.Do(func() {
		instance = &Pets{
			pets:   make(map[int]*Pet),
			loaded: false,
		}
	})
	return instance
}

// GetInstance 获取精灵管理器实例
func GetInstance() *Pets {
	if instance == nil {
		instance = New()
	}
	return instance
}

// Load 加载精灵数据
func (p *Pets) Load() error {
	if p.loaded {
		return nil
	}

	logger.Info("正在加载精灵数据...")

	// 读取XML文件：优先使用与可执行文件同级的 ../data/spt.xml，其次回退到当前工作目录的 data/spt.xml
	var data []byte
	var err error

	if exePath, e := os.Executable(); e == nil {
		exeDir := filepath.Dir(exePath)
		candidate := filepath.Join(exeDir, "..", "data", "spt.xml")
		if bytes, readErr := os.ReadFile(candidate); readErr == nil {
			data = bytes
			logger.Info(fmt.Sprintf("从可执行目录加载精灵数据: %s", candidate))
		} else {
			err = readErr
		}
	}

	if data == nil {
		data, err = os.ReadFile(filepath.Join("data", "spt.xml"))
	}
	if err != nil {
		logger.Error(fmt.Sprintf("读取精灵数据文件失败: %v", err))
		return err
	}

	// 解析XML
	// spt.xml 的顶级结构为:
	// <Monsters>
	//   <Monster .../>
	// </Monsters>
	// 这里直接把根视为 Monsters，避免多一层嵌套导致解析不到内容。
	var root struct {
		Pets []*Pet `xml:"Monster"`
	}

	if err := xml.Unmarshal(data, &root); err != nil {
		logger.Error(fmt.Sprintf("解析精灵数据失败: %v", err))
		return err
	}

	// 加载精灵数据
	count := 0
	for _, pet := range root.Pets {
		if pet.ID > 0 {
			p.pets[pet.ID] = pet
			count++
		}
	}

	logger.Info(fmt.Sprintf("加载了 %d 个精灵数据", count))
	p.loaded = true
	return nil
}

// Get 获取精灵数据
func (p *Pets) Get(petID int) *Pet {
	if !p.loaded {
		if err := p.Load(); err != nil {
			logger.Warning("精灵数据加载失败")
			return nil
		}
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.pets[petID]
}

// GetLearnableMoves 获取精灵可学习的技能
func (p *Pets) GetLearnableMoves(petID int, level int) []LearnableMove {
	pet := p.Get(petID)
	if pet == nil {
		return []LearnableMove{}
	}

	var moves []LearnableMove
	for _, move := range pet.LearnableMoves {
		if level <= 0 || move.Level <= level {
			moves = append(moves, move)
		}
	}

	return moves
}

// CanLearnMove 检查精灵是否可以学习某个技能
func (p *Pets) CanLearnMove(petID int, moveID int) bool {
	pet := p.Get(petID)
	if pet == nil {
		return false
	}

	// 检查可学习技能
	for _, move := range pet.LearnableMoves {
		if move.ID == moveID {
			return true
		}
	}

	// 检查额外技能
	for _, id := range pet.ExtraMoves {
		if id == moveID {
			return true
		}
	}

	return false
}

// GetEvolutionChain 获取精灵进化链
func (p *Pets) GetEvolutionChain(petID int) []int {
	chain := []int{}
	current := petID

	// 向前追溯到最初形态
	for {
		pet := p.Get(current)
		if pet == nil || pet.EvolvesFrom == 0 {
			break
		}
		current = pet.EvolvesFrom
	}

	// 从最初形态开始构建进化链
	for {
		chain = append(chain, current)
		pet := p.Get(current)
		if pet == nil || pet.EvolvesTo == 0 {
			break
		}
		current = pet.EvolvesTo
	}

	return chain
}

// canEvolveInternal 统一的进化判定入口
// ignoreBabin 为 true 时，不因 EvolveBabin=1 而禁止进化（供进化舱使用）。
func (p *Pets) canEvolveInternal(petID int, level int, hasItem bool, ignoreBabin bool) (bool, string, int) {
	pet := p.Get(petID)
	if pet == nil || pet.EvolvesTo == 0 {
		return false, "无法进化", 0
	}

	// 检查等级
	if pet.EvolvingLv > 0 && level < pet.EvolvingLv {
		return false, fmt.Sprintf("需要等级 %d", pet.EvolvingLv), 0
	}

	// 检查道具
	if pet.EvolvItem > 0 && !hasItem {
		return false, fmt.Sprintf("需要道具 ID:%d", pet.EvolvItem), 0
	}

	// 检查进化舱
	if !ignoreBabin && pet.EvolveBabin == 1 {
		return false, "需要在进化舱进化", 0
	}

	return true, "可以进化", pet.EvolvesTo
}

// CanEvolve 检查精灵是否可以进化（仅处理“直接进化”场景：不在进化舱中）
func (p *Pets) CanEvolve(petID int, level int, hasItem bool) (bool, string, int) {
	return p.canEvolveInternal(petID, level, hasItem, false)
}

// CanEvolveInBabin 检查精灵是否可以在进化舱中进化（忽略 EvolveBabin 标记本身，但仍校验等级与道具）
func (p *Pets) CanEvolveInBabin(petID int, level int, hasItem bool) (bool, string, int) {
	return p.canEvolveInternal(petID, level, hasItem, true)
}

// GetRealID 获取精灵的真实ID
func (p *Pets) GetRealID(petID int) int {
	pet := p.Get(petID)
	if pet == nil {
		return petID
	}
	if pet.RealId > 0 {
		return pet.RealId
	}
	return petID
}

// GetMinEvolutionLevel 获取精灵作为野生怪的最低等级（进化形态至少为进化所需等级，否则为 1）
func (p *Pets) GetMinEvolutionLevel(petID int) int {
	pet := p.Get(petID)
	if pet == nil {
		return 1
	}
	if pet.EvolvesFrom > 0 && pet.EvolvingLv > 0 {
		return pet.EvolvingLv
	}
	return 1
}

// TotalEV 返回学习力总和
func (ev EVStats) TotalEV() int {
	return ev.HP + ev.Atk + ev.Def + ev.SpAtk + ev.SpDef + ev.Spd
}

// AddEVWithCap 在现有 EV 上增加 yield，总和不超过 510，且不减少任何已有项（只加不减）。
// 若当前总和已 >= 510 则直接返回 current，不增加。
func AddEVWithCap(current, yield EVStats) EVStats {
	sum := current.TotalEV()
	if sum >= 510 {
		return current
	}
	ev := EVStats{
		HP:    current.HP + yield.HP,
		Atk:   current.Atk + yield.Atk,
		Def:   current.Def + yield.Def,
		SpAtk: current.SpAtk + yield.SpAtk,
		SpDef: current.SpDef + yield.SpDef,
		Spd:   current.Spd + yield.Spd,
	}
	// 单项上限 255
	clamp := func(v int) int {
		if v < 0 {
			return 0
		}
		if v > 255 {
			return 255
		}
		return v
	}
	ev.HP = clamp(ev.HP)
	ev.Atk = clamp(ev.Atk)
	ev.Def = clamp(ev.Def)
	ev.SpAtk = clamp(ev.SpAtk)
	ev.SpDef = clamp(ev.SpDef)
	ev.Spd = clamp(ev.Spd)
	newSum := ev.TotalEV()
	if newSum <= 510 {
		return ev
	}
	// 超出部分从“本回合增加的量”里按固定顺序扣减，不扣到低于 current
	over := newSum - 510
	dec := func(cur, now *int) {
		if over <= 0 {
			return
		}
		added := *now - *cur
		if added <= 0 {
			return
		}
		d := added
		if d > over {
			d = over
		}
		*now -= d
		over -= d
	}
	dec(&current.Spd, &ev.Spd)
	dec(&current.SpDef, &ev.SpDef)
	dec(&current.SpAtk, &ev.SpAtk)
	dec(&current.Def, &ev.Def)
	dec(&current.Atk, &ev.Atk)
	dec(&current.HP, &ev.HP)
	return ev
}

// ParseYieldingEV 解析努力值字符串，支持逗号或空格分隔（与 spt.xml 中 YieldingEV="0 1 0 0 0 0" 格式一致）
func (p *Pets) ParseYieldingEV(evString string) EVStats {
	ev := EVStats{}
	evString = strings.TrimSpace(evString)
	if evString == "" {
		return ev
	}
	values := strings.Split(evString, ",")
	if len(values) == 1 && values[0] != "" {
		values = strings.Fields(values[0])
	}
	if len(values) >= 6 {
		ev.HP, _ = strconv.Atoi(strings.TrimSpace(values[0]))
		ev.Atk, _ = strconv.Atoi(strings.TrimSpace(values[1]))
		ev.Def, _ = strconv.Atoi(strings.TrimSpace(values[2]))
		ev.SpAtk, _ = strconv.Atoi(strings.TrimSpace(values[3]))
		ev.SpDef, _ = strconv.Atoi(strings.TrimSpace(values[4]))
		ev.Spd, _ = strconv.Atoi(strings.TrimSpace(values[5]))
	}

	return ev
}

// GetName 获取精灵名称
func (p *Pets) GetName(petID int) string {
	pet := p.Get(petID)
	if pet != nil {
		return pet.DefName
	}
	return fmt.Sprintf("精灵#%d", petID)
}

// GetStats 计算精灵属性
// nature: 性格ID (0-24)，影响属性修正（+10%/-10%）
func (p *Pets) GetStats(petID int, level int, dv int, ev EVStats, nature int) Stats {
	pet := p.Get(petID)
	if pet == nil {
		return Stats{HP: 20, MaxHP: 20, Attack: 10, Defence: 10, SpAtk: 10, SpDef: 10, Speed: 10}
	}

	if level <= 0 {
		level = 1
	}

	if dv < 0 || dv > 31 {
		dv = 31
	}

	// 学习力裁剪：单项 0~255，总和 510（与 Lua ClampAndCapEV 一致）
	ev = ClampAndCapEV(ev)

	// 对齐 Lua `seer_pet_calculator.lua` 里的公式：
	// HP     = floor((种族值*2 + dv + ev/4) * Lv / 100) + Lv + 10
	// 其他   = floor((种族值*2 + dv + ev/4) * Lv / 100) + 5
	hp := int(math.Floor((float64(pet.HP*2+dv)+float64(ev.HP)/4.0)*float64(level)/100.0)) + level + 10
	attack := int(math.Floor((float64(pet.Atk*2+dv)+float64(ev.Atk)/4.0)*float64(level)/100.0)) + 5
	defence := int(math.Floor((float64(pet.Def*2+dv)+float64(ev.Def)/4.0)*float64(level)/100.0)) + 5
	spAtk := int(math.Floor((float64(pet.SpAtk*2+dv)+float64(ev.SpAtk)/4.0)*float64(level)/100.0)) + 5
	spDef := int(math.Floor((float64(pet.SpDef*2+dv)+float64(ev.SpDef)/4.0)*float64(level)/100.0)) + 5
	speed := int(math.Floor((float64(pet.Spd*2+dv)+float64(ev.Spd)/4.0)*float64(level)/100.0)) + 5

	// 防止出现 0/负数导致战斗异常（至少为 1，HP 至少为 1）
	if hp < 1 {
		hp = 1
	}
	if attack < 1 {
		attack = 1
	}
	if defence < 1 {
		defence = 1
	}
	if spAtk < 1 {
		spAtk = 1
	}
	if spDef < 1 {
		spDef = 1
	}
	if speed < 1 {
		speed = 1
	}

	// 应用性格修正（0-24对应25种性格，25-26为平衡型）
	// 性格定义：0-24对应Lua中的1-25，其中21-25为平衡型（无修正）
	// 增益+10%，减益-10%
	if nature >= 0 && nature <= 24 {
		// 性格修正表（对应Lua的seer_natures.lua）
		// 0-3: 攻击强化类
		// 4-7: 速度强化类
		// 8-11: 防御强化类
		// 12-15: 特攻强化类
		// 16-19: 特防强化类
		// 20-24: 平衡型
		var atkMod, defMod, spaMod, spdMod, speMod float64 = 1.0, 1.0, 1.0, 1.0, 1.0

		switch nature {
		case 0: // 孤独: 攻击+10%, 防御-10%
			atkMod, defMod = 1.1, 0.9
		case 1: // 勇敢: 攻击+10%, 速度-10%
			atkMod, speMod = 1.1, 0.9
		case 2: // 固执: 攻击+10%, 特攻-10%
			atkMod, spaMod = 1.1, 0.9
		case 3: // 调皮: 攻击+10%, 特防-10%
			atkMod, spdMod = 1.1, 0.9
		case 4: // 胆小: 速度+10%, 攻击-10%
			speMod, atkMod = 1.1, 0.9
		case 5: // 急躁: 速度+10%, 防御-10%
			speMod, defMod = 1.1, 0.9
		case 6: // 开朗: 速度+10%, 特攻-10%
			speMod, spaMod = 1.1, 0.9
		case 7: // 天真: 速度+10%, 特防-10%
			speMod, spdMod = 1.1, 0.9
		case 8: // 大胆: 防御+10%, 攻击-10%
			defMod, atkMod = 1.1, 0.9
		case 9: // 悠闲: 防御+10%, 速度-10%
			defMod, speMod = 1.1, 0.9
		case 10: // 顽皮: 防御+10%, 特攻-10%
			defMod, spaMod = 1.1, 0.9
		case 11: // 无虑: 防御+10%, 特防-10%
			defMod, spdMod = 1.1, 0.9
		case 12: // 保守: 特攻+10%, 攻击-10%
			spaMod, atkMod = 1.1, 0.9
		case 13: // 稳重: 特攻+10%, 防御-10%
			spaMod, defMod = 1.1, 0.9
		case 14: // 冷静: 特攻+10%, 速度-10%
			spaMod, speMod = 1.1, 0.9
		case 15: // 马虎: 特攻+10%, 特防-10%
			spaMod, spdMod = 1.1, 0.9
		case 16: // 沉着: 特防+10%, 攻击-10%
			spdMod, atkMod = 1.1, 0.9
		case 17: // 温顺: 特防+10%, 防御-10%
			spdMod, defMod = 1.1, 0.9
		case 18: // 狂妄: 特防+10%, 速度-10%
			spdMod, speMod = 1.1, 0.9
		case 19: // 慎重: 特防+10%, 特攻-10%
			spdMod, spaMod = 1.1, 0.9
			// 20-24: 平衡型（无修正）
		}

		// 性格修正采用四舍五入，避免因直接截断导致满培养情况下少 1 点属性
		attack = int(math.Round(float64(attack) * atkMod))
		defence = int(math.Round(float64(defence) * defMod))
		spAtk = int(math.Round(float64(spAtk) * spaMod))
		spDef = int(math.Round(float64(spDef) * spdMod))
		speed = int(math.Round(float64(speed) * speMod))

		// 再次保证下限
		if attack < 1 {
			attack = 1
		}
		if defence < 1 {
			defence = 1
		}
		if spAtk < 1 {
			spAtk = 1
		}
		if spDef < 1 {
			spDef = 1
		}
		if speed < 1 {
			speed = 1
		}
	}

	return Stats{
		HP:      hp,
		MaxHP:   hp,
		Attack:  attack,
		Defence: defence,
		SpAtk:   spAtk,
		SpDef:   spDef,
		Speed:   speed,
	}
}

// GetSkillsLearnedAtLevel 获取精灵在指定等级新学会的技能（LearningLv == level）
func (p *Pets) GetSkillsLearnedAtLevel(petID int, level int) []int {
	pet := p.Get(petID)
	if pet == nil {
		return nil
	}
	var skills []int
	for _, move := range pet.LearnableMoves {
		if move.Level == level && move.ID > 0 {
			skills = append(skills, move.ID)
		}
	}
	return skills
}

// GetSkillsForLevel 获取精灵在指定等级可以学会的技能（最多4个，取当前等级已学会的最近4个）
// 若精灵不存在或无可学技能，返回默认 [10001,0,0,0]（撞击），避免对战只显示/使用撞击
func (p *Pets) GetSkillsForLevel(petID int, level int) []int {
	pet := p.Get(petID)
	if pet == nil {
		return []int{10001, 0, 0, 0}
	}

	var skills []int
	for _, move := range pet.LearnableMoves {
		if move.Level <= level && move.ID > 0 {
			skills = append(skills, move.ID)
		}
	}

	// 无可学技能时返回默认撞击，避免“很多精灵只会放撞击”实为无技能列表
	if len(skills) == 0 {
		return []int{10001, 0, 0, 0}
	}

	// 最多返回4个技能（取当前等级已学会的最近4个）
	result := make([]int, 4)
	startIdx := len(skills) - 4
	if startIdx < 0 {
		startIdx = 0
	}

	for i := 0; i < 4; i++ {
		if startIdx+i < len(skills) {
			result[i] = skills[startIdx+i]
		} else {
			result[i] = 0
		}
	}

	return result
}

// GetExpInfo 获取经验信息
func (p *Pets) GetExpInfo(petID int, level int, currentLevelExp int) ExpInfo {
	pet := p.Get(petID)
	if pet == nil {
		return ExpInfo{CurrentLevelExp: 0, NextLevelExp: 100, TotalExp: 0}
	}

	growthType := pet.GrowthType

	// 计算升到下一级所需经验（二次方公式，使 1→100 级总经验约 165 万）
	// 1^2+...+99^2=328350，系数 5 时总经验约 164 万
	coeff := 5
	switch growthType {
	case 0:
		coeff = 4 // 快速成长
	case 1:
		coeff = 5 // 中速成长
	case 2:
		coeff = 6 // 慢速成长
	case 3:
		coeff = 8 // 极慢成长
	default:
		coeff = 5
	}
	nextLevelExp := level * level * coeff

	// 计算总经验（当前等级之前各级所需经验之和）
	totalExp := 0
	for lv := 1; lv < level; lv++ {
		c := 5
		switch growthType {
		case 0:
			c = 4
		case 1:
			c = 5
		case 2:
			c = 6
		case 3:
			c = 8
		default:
			c = 5
		}
		totalExp += lv * lv * c
	}
	totalExp += currentLevelExp

	return ExpInfo{
		CurrentLevelExp: currentLevelExp,
		NextLevelExp:    nextLevelExp,
		TotalExp:        totalExp,
	}
}

// Exists 检查精灵是否存在
func (p *Pets) Exists(petID int) bool {
	return p.Get(petID) != nil
}
