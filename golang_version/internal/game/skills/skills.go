package skills

import (
	"encoding/xml"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/seer-game/golang-version/internal/core/logger"
)

// Skill 技能数据（与前端 27_SkillXMLInfo 对齐：Url 用于客户端技能效果/动画）
type Skill struct {
	ID       int    `xml:"ID,attr"`
	Name     string `xml:"Name,attr"`
	Category int    `xml:"Category,attr"` // 1=物理 2=特殊 4=变化/状态
	Type     int    `xml:"Type,attr"`
	Power    int    `xml:"Power,attr"`
	MaxPP    int    `xml:"MaxPP,attr"`
	Accuracy int    `xml:"Accuracy,attr"`
	CritRate int    `xml:"CritRate,attr"`
	Priority int    `xml:"Priority,attr"`
	MustHit  int    `xml:"MustHit,attr"`
	// SideEffect 字段在 XML 中有时是类似 "422 31" 这样的复合字符串
	// 直接映射为 int 会导致 strconv.ParseInt 失败，Lua 版本是按字符串再取第一个数字。
	SideEffect     string `xml:"SideEffect,attr"`
	SideEffectArg  string `xml:"SideEffectArg,attr"`
	MonID          int    `xml:"MonID,attr"`
	CritAtkFirst   int    `xml:"CritAtkFirst,attr"`
	CritAtkSecond  int    `xml:"CritAtkSecond,attr"`
	CritSelfHalfHp int    `xml:"CritSelfHalfHp,attr"`
	CritFoeHalfHp  int    `xml:"CritFoeHalfHp,attr"`
	DmgBindLv      int    `xml:"DmgBindLv,attr"`
	PwrBindDv      int    `xml:"PwrBindDv,attr"`
	PwrDouble      int    `xml:"PwrDouble,attr"`
	Url            string `xml:"Url,attr"` // 技能效果资源名，与前端 SkillXMLInfo.getUrl(skillID) 对应

	// 计算字段
	PP         int
	EffectID   int // 解析后的效果ID（对齐 Lua: 取 SideEffect 的第一个数字）
	EffectData *EffectData
}

// EffectData 技能效果数据
type EffectData struct {
	EID  int
	Desc string
	Args map[string]interface{}
}

// Skills 技能管理器
type Skills struct {
	skills map[int]*Skill
	loaded bool
	mu     sync.RWMutex
}

var (
	instance *Skills
	once     sync.Once
)

// New 创建技能管理器实例
func New() *Skills {
	once.Do(func() {
		instance = &Skills{
			skills: make(map[int]*Skill),
			loaded: false,
		}
	})
	return instance
}

// GetInstance 获取技能管理器实例
func GetInstance() *Skills {
	if instance == nil {
		instance = New()
	}
	return instance
}

// Load 加载技能数据
func (s *Skills) Load() error {
	if s.loaded {
		return nil
	}

	logger.Info("正在加载技能数据...")

	// 读取XML文件：优先从可执行文件所在目录的 ../data/skills.xml 加载，
	// 若失败则回退到当前工作目录下的 data/skills.xml，兼容从项目根目录或 bin 目录启动。
	var data []byte
	var err error

	if exePath, e := os.Executable(); e == nil {
		exeDir := filepath.Dir(exePath)
		candidate := filepath.Join(exeDir, "..", "data", "skills.xml")
		if bytes, readErr := os.ReadFile(candidate); readErr == nil {
			data = bytes
			logger.Info(fmt.Sprintf("从可执行目录加载技能数据: %s", candidate))
		} else {
			err = readErr
		}
	}

	if data == nil {
		data, err = os.ReadFile(filepath.Join("data", "skills.xml"))
	}
	if err != nil {
		logger.Error(fmt.Sprintf("读取技能数据文件失败: %v", err))
		return err
	}

	// 解析XML
	var root struct {
		Moves []*Skill `xml:"Moves>Move"`
	}

	if err := xml.Unmarshal(data, &root); err != nil {
		logger.Error(fmt.Sprintf("解析技能数据失败: %v", err))
		return err
	}

	// 加载技能数据
	count := 0
	for _, skill := range root.Moves {
		if skill.ID > 0 {
			// 初始化PP
			skill.PP = skill.MaxPP
			if skill.MaxPP == 0 {
				skill.MaxPP = 35
				skill.PP = 35
			}

			// 设置默认值
			if skill.Category == 0 {
				skill.Category = 1
			}
			if skill.Type == 0 {
				skill.Type = 8
			}
			if skill.Accuracy == 0 {
				skill.Accuracy = 100
			}
			if skill.CritRate == 0 {
				skill.CritRate = 1
			}

			// 解析 SideEffect，兼容 "422 31" 这类复合字符串（取第一个数字）
			if skill.SideEffect != "" {
				parts := strings.Fields(skill.SideEffect)
				if len(parts) > 0 {
					if eid, err := strconv.Atoi(parts[0]); err == nil {
						skill.EffectID = eid
					}
				}
			}

			s.skills[skill.ID] = skill
			count++
		}
	}

	logger.Info(fmt.Sprintf("加载了 %d 个技能数据", count))

	// 链接技能效果
	s.linkSkillEffects()

	s.loaded = true
	return nil
}

// linkSkillEffects 链接技能效果
func (s *Skills) linkSkillEffects() {
	linkedCount := 0

	for _, skill := range s.skills {
		if skill.EffectID > 0 {
			// 这里可以添加技能效果的链接逻辑
			// 暂时使用默认效果数据
			skill.EffectData = &EffectData{
				EID:  skill.EffectID,
				Desc: fmt.Sprintf("技能效果 %d", skill.EffectID),
				Args: make(map[string]interface{}),
			}
			linkedCount++
		}
	}

	logger.Info(fmt.Sprintf("链接了 %d 个技能效果", linkedCount))
}

// Get 获取技能数据
func (s *Skills) Get(skillID int) *Skill {
	if !s.loaded {
		if err := s.Load(); err != nil {
			logger.Warning("技能数据加载失败")
			return nil
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.skills[skillID]
}

// GetFullInfo 获取技能完整信息
func (s *Skills) GetFullInfo(skillID int) map[string]interface{} {
	skill := s.Get(skillID)
	if skill == nil {
		return nil
	}

	info := make(map[string]interface{})
	info["id"] = skill.ID
	info["name"] = skill.Name
	info["category"] = skill.Category
	info["type"] = skill.Type
	info["power"] = skill.Power
	info["pp"] = skill.PP
	info["maxPP"] = skill.MaxPP
	info["accuracy"] = skill.Accuracy
	info["critRate"] = skill.CritRate
	info["priority"] = skill.Priority
	info["mustHit"] = skill.MustHit == 1

	if skill.EffectData != nil {
		info["effectDesc"] = skill.EffectData.Desc
		info["effectEid"] = skill.EffectData.EID
		info["effectArgs"] = skill.EffectData.Args
	}

	return info
}

// IsExclusiveMove 检查技能是否为专属技能
func (s *Skills) IsExclusiveMove(skillID int, petID int) bool {
	skill := s.Get(skillID)
	if skill == nil {
		return false
	}

	// 如果技能有MonID字段，则为专属技能
	if skill.MonID > 0 {
		return skill.MonID == petID
	}

	return true // 非专属技能，所有精灵都可以使用
}

// CalculateBaseDamage 计算技能基础伤害
func (s *Skills) CalculateBaseDamage(skill *Skill, attacker map[string]interface{}, defender map[string]interface{}) int {
	if skill == nil || skill.Power == 0 {
		return 0
	}

	// 获取攻击方属性
	level := getInt(attacker, "level", 50)
	attack := 100
	defense := 100

	// 根据技能类型选择攻击和防御属性
	if skill.Category == 1 {
		// 物理攻击
		attack = getInt(attacker, "attack", 100)
		defense = getInt(defender, "defence", 100)
	} else if skill.Category == 2 {
		// 特殊攻击
		attack = getInt(attacker, "spAtk", 100)
		defense = getInt(defender, "spDef", 100)
	}

	// 基础伤害公式
	baseDamage := ((level*2/5+2)*skill.Power*attack/defense)/50 + 2

	// 属性一致加成 (STAB)
	attackerType := getInt(attacker, "type", 8)
	attackerType2 := getInt(attacker, "type2", 0)
	if attackerType == skill.Type || attackerType2 == skill.Type {
		baseDamage = baseDamage * 3 / 2 // 1.5倍
	}

	// 属性克制 (暂时使用默认值)
	typeMultiplier := 1.0

	// 随机因子 (85%-100%)
	randomFactor := float64(rand.Intn(16)+85) / 100.0
	baseDamage = int(float64(baseDamage) * typeMultiplier * randomFactor)

	return baseDamage
}

// getInt 从map中获取int值
func getInt(m map[string]interface{}, key string, defaultValue int) int {
	if val, ok := m[key]; ok {
		if intVal, ok := val.(int); ok {
			return intVal
		}
	}
	return defaultValue
}

// Exists 检查技能是否存在
func (s *Skills) Exists(skillID int) bool {
	return s.Get(skillID) != nil
}

// GetName 获取技能名称
func (s *Skills) GetName(skillID int) string {
	skill := s.Get(skillID)
	if skill != nil {
		return skill.Name
	}
	return fmt.Sprintf("技能#%d", skillID)
}
