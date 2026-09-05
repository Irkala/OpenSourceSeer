package mapogres

import (
	"math/rand"
	"strings"
	"time"

	gamepets "github.com/seer-game/golang-version/internal/game/pets"
	"github.com/seer-game/golang-version/internal/core/logger"
)

// Slot 表示一格刷新的怪物（用于 2004 / 2408 协议）
type Slot struct {
	PetID int  // 精灵ID
	Shiny bool // 是否闪光
	Level int  // 等级，0 表示使用默认 5 级
}

// monsterEntry 带等级与稀有标记的精灵配置（克洛斯星 10/11/12 等）
type monsterEntry struct {
	Name     string // 精灵中文名，如 "皮皮", "#闪光皮皮"
	LevelMin int    // 等级下限
	LevelMax int    // 等级上限
	Rare     bool   // 是否稀有（5% 概率刷新）
}

// mapConfig 一张地图的刷新配置，使用精灵中文名（与官方 XML / Lua 表保持一致）
type mapConfig struct {
	Monsters []string // 如 {"火焰贝", "#小火猴", ...}，兼容旧配置，等级默认 5
	Slots    int      // 刷新格子数量，默认 9
	// 克洛斯星 10/11/12 专用：带等级与 5% 稀有的配置
	Common     []monsterEntry // 普通精灵 + 等级范围
	Rare       []monsterEntry // 稀有精灵(5%) + 等级
	RareInLast50Min []string   // 仅每小时最后 50 分钟（minute>=10）加入池子，如 依依、闪光依依
	SPTBoss    string          // SPTBOSS 名，如 "蘑菇怪"，会加入池子
}

// 稀有精灵两阶段抽取（与设计图一致）：
// 一阶段：9 槽抽 3（1 稀有槽 + 8 普通槽），P(抽到稀有槽)=3/9=1/3。
// 二阶段：仅当抽到稀有槽时，在稀有槽内二次抽取（1 稀有 + N 空），P(稀有)=1/(1+N)。
// P(稀有) = (1/3) × 1/(1+N)。rareEmptySlots 即 N，空数越多稀有越难出，用于调整稀有难度。
var rareEmptySlots = 20 // N=20 时 P(稀有)≈1.6%；可改为更小则更容易出稀有

// GetRareEmptySlots 返回稀有槽内空槽数 N，供 GM 或配置读取
func GetRareEmptySlots() int { return rareEmptySlots }

// SetRareEmptySlots 设置稀有槽内空槽数 N（<0 时按 0 处理），用于调整稀有难度
func SetRareEmptySlots(n int) { rareEmptySlots = n }

// 基于官方 220.xml / game_config.lua / PetBook 地图层分布配置（客户端地图ID）
// 每个星球有多层地图，这里按“每层最多 3 只野怪”的规则配置精灵：
// - Slots 一律为 4，实际展示 3 只+1 空（9 槽抽 3 + 稀有槽二次抽取）
// - Common 为普通精灵池，Rare 为稀有精灵池；P(稀有)=(1/3)×1/(1+rareEmptySlots)
// - LevelMin/LevelMax 指定等级范围；无固定等级则 LevelMin/LevelMax 为 0，使用默认等级逻辑
var maps = map[int]mapConfig{
	// 克洛斯星草原(10)：皮皮 Lv1-2 普通，闪光皮皮 Lv1-2 稀有
	10: {
		Slots:  4,
		Common: []monsterEntry{
			{Name: "皮皮", LevelMin: 1, LevelMax: 2, Rare: false},
		},
		Rare: []monsterEntry{
			{Name: "#闪光皮皮", LevelMin: 1, LevelMax: 2, Rare: true},
		},
	},
	// 克洛斯星沼泽(11)：仙人球 Lv11-12 普通，小豆芽 Lv11-12 稀有，达尔 稀有
	11: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "仙人球", LevelMin: 11, LevelMax: 12, Rare: false},
		},
		Rare: []monsterEntry{
			{Name: "小豆芽", LevelMin: 11, LevelMax: 12, Rare: true},
			{Name: "达尔", LevelMin: 0, LevelMax: 0, Rare: true},
		},
	},
	// 克洛斯星密林(12)：SPTBOSS 蘑菇怪，稀有精灵依依、闪光依依在每小时最后 50 分钟刷新（minute>=10）
	12: {
		Slots:           4,
		Common:          []monsterEntry{{Name: "依依", LevelMin: 1, LevelMax: 6, Rare: false}, {Name: "依丁丝", LevelMin: 1, LevelMax: 6, Rare: false}},
		Rare:            []monsterEntry{{Name: "依依", LevelMin: 1, LevelMax: 6, Rare: true}, {Name: "#闪光依依", LevelMin: 1, LevelMax: 6, Rare: true}},
		RareInLast50Min: []string{"依依", "#闪光依依"},
		SPTBoss:         "蘑菇怪",
	},
	// 云霄星：地面层(25) 毛毛（莫比为稀有）；高空层(26) 幽浮、哈尔浮；最高层(27) 浮空鱼、浮空苗
	25: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "毛毛", LevelMin: 3, LevelMax: 4, Rare: false},
		},
		Rare: []monsterEntry{
			{Name: "莫比", LevelMin: 3, LevelMax: 4, Rare: true},
		},
	},
	26: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "幽浮", LevelMin: 13, LevelMax: 14, Rare: false},
			{Name: "哈尔浮", LevelMin: 13, LevelMax: 14, Rare: false},
		},
	},
	27: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "浮空鱼", LevelMin: 4, LevelMax: 10, Rare: false},
			{Name: "浮空苗", LevelMin: 4, LevelMax: 10, Rare: false},
		},
	},
	// 海洋星：浅水区(20) 贝尔；深水区(21) 利牙鱼，布鲁 稀有；海底(22) 小鳍鱼，布鲁 稀有
	20: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "贝尔", LevelMin: 5, LevelMax: 6, Rare: false},
		},
	},
	21: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "利牙鱼", LevelMin: 15, LevelMax: 16, Rare: false},
		},
		Rare: []monsterEntry{
			{Name: "布鲁", LevelMin: 0, LevelMax: 0, Rare: true},
		},
	},
	22: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "小鳍鱼", LevelMin: 15, LevelMax: 16, Rare: false},
		},
		Rare: []monsterEntry{
			{Name: "布鲁", LevelMin: 0, LevelMax: 0, Rare: true},
		},
	},
	// 火山星：山脚下(15)、洞穴(16)、山洞深处(17)
	// 火山星表面(山脚下)：火炎贝 Lv13-14 普通，格林 无固定等级 稀有
	15: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "火炎贝", LevelMin: 13, LevelMax: 14, Rare: false},
		},
		Rare: []monsterEntry{
			// 无固定等级：LevelMin/LevelMax 为 0，后续用默认等级逻辑
			{Name: "格林", LevelMin: 0, LevelMax: 0, Rare: true},
		},
	},
	// 火山星山洞：吉尔 Lv17-18 普通，巴多 Lv15 稀有
	16: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "吉尔", LevelMin: 17, LevelMax: 18, Rare: false},
		},
		Rare: []monsterEntry{
			{Name: "巴多", LevelMin: 15, LevelMax: 15, Rare: true},
		},
	},
	// 火山星山洞深处：赤甲虫 Lv15 普通，赤西西比 Lv38 普通，格林 稀有
	17: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "赤甲虫", LevelMin: 15, LevelMax: 15, Rare: false},
			{Name: "赤西西比", LevelMin: 38, LevelMax: 38, Rare: false},
		},
		Rare: []monsterEntry{
			{Name: "格林", LevelMin: 0, LevelMax: 0, Rare: true},
		},
	},
	// 赫尔卡星：表面(30)、遗迹(31)、荒地(32)、精灵广场(34)、赫尔卡飞船(43)
	// 赫尔卡星表面(30)：比比鼠 Lv13-14 普通，西塔 稀有
	30: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "比比鼠", LevelMin: 13, LevelMax: 14, Rare: false},
		},
		Rare: []monsterEntry{
			{Name: "西塔", LevelMin: 0, LevelMax: 0, Rare: true},
		},
	},
	// 赫尔卡星遗迹(31)：罗奇 Lv19-20 普通，利利、闪光利利 无固定等级 稀有
	31: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "罗奇", LevelMin: 19, LevelMax: 20, Rare: false},
		},
		Rare: []monsterEntry{
			{Name: "利利", LevelMin: 0, LevelMax: 0, Rare: true},
			{Name: "#闪光利利", LevelMin: 0, LevelMax: 0, Rare: true},
		},
	},
	32: {Monsters: []string{"雷伊"}},                      // 赫尔卡星荒地：仅雷雨天出现，见 IsLeiyiWeather()
	34: {Monsters: []string{"果冻鸭", "波浪鸭", "水晶鸭"}},  // 精灵广场
	// 精灵圣殿(36)：吉斯 普通
	36: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "吉斯", LevelMin: 0, LevelMax: 0, Rare: false},
		},
	},
	43: {Monsters: []string{"玄冰兽", "急冻兽"}},          // 赫尔卡飞船
	// 海盗能源站(46)：鲁克 普通
	46: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "鲁克", LevelMin: 0, LevelMax: 0, Rare: false},
		},
	},
	// 塞西利亚星：地表(40)、寒冰溶洞(41)
	// 塞西利亚星表面：卡卡、利兹 普通，凯奴、基摩 稀有（15-16级）
	40: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "卡卡", LevelMin: 5, LevelMax: 10, Rare: false},
			{Name: "利兹", LevelMin: 5, LevelMax: 10, Rare: false},
		},
		Rare: []monsterEntry{
			{Name: "凯奴", LevelMin: 15, LevelMax: 16, Rare: true},
			{Name: "基摩", LevelMin: 15, LevelMax: 16, Rare: true},
		},
	},
	// 塞西利亚星溶洞：玄冰兽 Lv25-26 普通，林克 无固定等级 稀有
	41: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "玄冰兽", LevelMin: 25, LevelMax: 26, Rare: false},
		},
		Rare: []monsterEntry{
			{Name: "林克", LevelMin: 0, LevelMax: 0, Rare: true},
		},
	},
	// 双子阿尔法星(105)：温泉 / 进化区域，迪达、迪尔克（加格仅在 106 岩地刷新）
	105: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "迪达", LevelMin: 14, LevelMax: 15, Rare: false},
			{Name: "迪尔克", LevelMin: 31, LevelMax: 32, Rare: false},
		},
	},
	// 阿尔法星岩地(106)：仅加格 Lv18-19 野生刷新
	106: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "加格", LevelMin: 18, LevelMax: 19, Rare: false},
		},
	},
	// 太空站左翼(108)：使用旧逻辑，所有槽位都填满罗奇，避免空槽位导致"快去探索"
	108: {
		Monsters: []string{"罗奇"},
		Slots:    4,
	},
	// 双子贝塔星(47)：只刷新莱尼、特鲁尼
	47: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "莱尼", LevelMin: 12, LevelMax: 16, Rare: false},
			{Name: "特鲁尼", LevelMin: 17, LevelMax: 25, Rare: false},
		},
	},
	// 海盗要塞废墟(48)：只刷新古鲁、梅鲁
	48: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "古鲁", LevelMin: 15, LevelMax: 26, Rare: false},
			{Name: "梅鲁", LevelMin: 27, LevelMax: 35, Rare: false},
		},
	},
	// 贝塔星荒原(49)：只刷新丁格、丁加鲁
	49: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "丁格", LevelMin: 15, LevelMax: 16, Rare: false},
			{Name: "丁加鲁", LevelMin: 17, LevelMax: 25, Rare: false},
		},
	},
	314: {Monsters: []string{"尤纳斯", "波谷", "卢比", "#奇塔", "#卡塔"}},
	51:  {Monsters: []string{"达比拉", "魔狮迪露", "迪度", "托尼"}},
	// 露希欧星(54)：阿兹、科利 普通，莎莉 稀有（16-17级）
	54: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "阿兹", LevelMin: 12, LevelMax: 17, Rare: false},
			{Name: "科利", LevelMin: 18, LevelMax: 30, Rare: false},
		},
		Rare: []monsterEntry{
			{Name: "莎莉", LevelMin: 16, LevelMax: 17, Rare: true},
		},
	},
	// 露希欧泥潭(55)：火晶兽、多鲁姆 普通，晶岩兽 稀有（15级）
	55: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "火晶兽", LevelMin: 15, LevelMax: 26, Rare: false},
			{Name: "多鲁姆", LevelMin: 27, LevelMax: 35, Rare: false},
		},
		Rare: []monsterEntry{
			{Name: "#晶岩兽", LevelMin: 15, LevelMax: 15, Rare: true},
		},
	},
	// 露希欧之洋(56)：只刷新水草蛙
	56: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "水草蛙", LevelMin: 15, LevelMax: 28, Rare: false},
		},
	},
	// 尼古尔星(57)：只刷新米隆、米洛尼
	57: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "米隆", LevelMin: 15, LevelMax: 25, Rare: false},
			{Name: "米洛尼", LevelMin: 26, LevelMax: 40, Rare: false},
		},
	},
	// 尼古尔峭壁(58)：只刷新查斯、查尔顿
	58: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "查斯", LevelMin: 15, LevelMax: 28, Rare: false},
			{Name: "查尔顿", LevelMin: 29, LevelMax: 40, Rare: false},
		},
	},
	// 塔克星(60)：伊娃、伊娅丝 普通；眼球 Lv15-16 稀有
	60: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "伊娃", LevelMin: 12, LevelMax: 17, Rare: false},
			{Name: "伊娅丝", LevelMin: 18, LevelMax: 35, Rare: false},
		},
		Rare: []monsterEntry{
			{Name: "眼球", LevelMin: 15, LevelMax: 16, Rare: true},
		},
	},
	// 光之迷城(61)：稀有吉宝、吉娜斯
	61: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "吉娜斯", LevelMin: 20, LevelMax: 40, Rare: false},
		},
		Rare: []monsterEntry{
			{Name: "#吉宝", LevelMin: 15, LevelMax: 25, Rare: true},
		},
	},
	// 暗之迷城(62)：稀有斯内克、海德拉
	62: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "海德拉", LevelMin: 20, LevelMax: 40, Rare: false},
		},
		Rare: []monsterEntry{
			{Name: "#斯内克", LevelMin: 15, LevelMax: 25, Rare: true},
		},
	},
	// 沙漠窑洞(64)：只刷新埃闻
	64: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "埃闻", LevelMin: 15, LevelMax: 28, Rare: false},
		},
	},
	// 精灵舱(316)：卢比、波古
	316: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "卢比", LevelMin: 15, LevelMax: 28, Rare: false},
			{Name: "波古", LevelMin: 15, LevelMax: 28, Rare: false},
		},
	},
	// 拉姆世界丛林(323)：嘟咕噜、嘟噜噜 稀有
	323: {
		Slots: 4,
		Common: []monsterEntry{},
		Rare: []monsterEntry{
			{Name: "嘟咕噜", LevelMin: 15, LevelMax: 28, Rare: true},
			{Name: "嘟噜噜", LevelMin: 15, LevelMax: 28, Rare: true},
		},
	},
	// 艾迪星(325)：帕尼、帕格尼尼
	325: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "帕尼", LevelMin: 15, LevelMax: 25, Rare: false},
			{Name: "帕格尼尼", LevelMin: 26, LevelMax: 40, Rare: false},
		},
	},
	// 暮色之城(326)：乌凯、乌力朴
	326: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "乌凯", LevelMin: 15, LevelMax: 25, Rare: false},
			{Name: "乌力朴", LevelMin: 26, LevelMax: 40, Rare: false},
		},
	},
	// 地下之城/虚幻的地下城市(327)：沙顿、沙罗希瓦
	327: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "沙顿", LevelMin: 15, LevelMax: 28, Rare: false},
			{Name: "沙罗希瓦", LevelMin: 29, LevelMax: 40, Rare: false},
		},
	},
	// 斯科尔星球(328)：弗曼、弗里昂
	328: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "弗曼", LevelMin: 15, LevelMax: 28, Rare: false},
			{Name: "弗里昂", LevelMin: 29, LevelMax: 40, Rare: false},
		},
	},
	// 潘多拉宝盒(329)：卡西
	329: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "卡西", LevelMin: 15, LevelMax: 35, Rare: false},
		},
	},
	// 斯科尔浮岛(330)：里昂、迷你芽、迷你果
	330: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "里昂", LevelMin: 15, LevelMax: 25, Rare: false},
			{Name: "迷你芽", LevelMin: 15, LevelMax: 25, Rare: false},
			{Name: "迷你果", LevelMin: 26, LevelMax: 40, Rare: false},
		},
	},
	// 斯科尔高空塔(331)：幼镰鸟、巨镰鸟
	331: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "幼镰鸟", LevelMin: 15, LevelMax: 28, Rare: false},
			{Name: "巨镰鸟", LevelMin: 29, LevelMax: 40, Rare: false},
		},
	},
	// 普雷空间(333)：嗡嗡蝴
	333: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "嗡嗡蝴", LevelMin: 15, LevelMax: 35, Rare: false},
		},
	},
	// 几米塔(334)：莫顿、古利安、隆米尔
	334: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "莫顿", LevelMin: 15, LevelMax: 28, Rare: false},
			{Name: "古利安", LevelMin: 29, LevelMax: 38, Rare: false},
			{Name: "隆米尔", LevelMin: 39, LevelMax: 45, Rare: false},
		},
	},
	338: {Monsters: []string{"萨拉", "#乌普"}},
	// 比格星(404)：洛洛斯、克洛洛特 + 稀有卡拉
	404: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "洛洛斯", LevelMin: 15, LevelMax: 28, Rare: false},
			{Name: "克洛洛特", LevelMin: 29, LevelMax: 40, Rare: false},
		},
		Rare: []monsterEntry{
			{Name: "卡拉", LevelMin: 25, LevelMax: 35, Rare: true},
		},
	},
	// 陨石地带(411)：鲁格洛
	411: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "鲁格洛", LevelMin: 15, LevelMax: 35, Rare: false},
		},
	},
	// 沃尔夫洞穴(423)：丁格、仙人球
	423: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "丁格", LevelMin: 15, LevelMax: 16, Rare: false},
			{Name: "仙人球", LevelMin: 15, LevelMax: 28, Rare: false},
		},
	},
	// 空间补给站(424)：卡丹
	424: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "卡丹", LevelMin: 15, LevelMax: 35, Rare: false},
		},
	},
	// 时空罗盘(425)：阿零
	425: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "阿零", LevelMin: 15, LevelMax: 35, Rare: false},
		},
	},
	// 拓梯星(429)：咕咕芽、咕咕果
	429: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "咕咕芽", LevelMin: 15, LevelMax: 28, Rare: false},
			{Name: "咕咕果", LevelMin: 29, LevelMax: 40, Rare: false},
		},
	},
	// 精灵欢乐谷(432)：水球
	432: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "水球", LevelMin: 15, LevelMax: 35, Rare: false},
		},
	},
	// 赫鲁卡城(461)：波尼儿、丫丫、迪度、托尼、霹雳兽、尼斯克、迪卢卡、水草蛙、西塔、埃闻、嘟噜噜(橙毛)、嘟咕噜(紫毛)、查克；稀有 夏伏兽 5%；等级为首发精灵-5，捕捉率1%
	461: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "波尼儿", LevelMin: 0, LevelMax: 0, Rare: false},
			{Name: "丫丫", LevelMin: 0, LevelMax: 0, Rare: false},
			{Name: "迪度", LevelMin: 0, LevelMax: 0, Rare: false},
			{Name: "托尼", LevelMin: 0, LevelMax: 0, Rare: false},
			{Name: "霹雳兽", LevelMin: 0, LevelMax: 0, Rare: false},
			{Name: "尼斯克", LevelMin: 0, LevelMax: 0, Rare: false},
			{Name: "迪卢卡", LevelMin: 0, LevelMax: 0, Rare: false},
			{Name: "水草蛙", LevelMin: 0, LevelMax: 0, Rare: false},
			{Name: "西塔", LevelMin: 0, LevelMax: 0, Rare: false},
			{Name: "埃闻", LevelMin: 0, LevelMax: 0, Rare: false},
			{Name: "嘟噜噜", LevelMin: 0, LevelMax: 0, Rare: false},
			{Name: "嘟咕噜", LevelMin: 0, LevelMax: 0, Rare: false},
			{Name: "查克", LevelMin: 0, LevelMax: 0, Rare: false},
		},
		Rare: []monsterEntry{
			{Name: "夏伏兽", LevelMin: 0, LevelMax: 0, Rare: true}, // 两阶段抽取，P=(1/3)×1/(1+rareEmptySlots)
		},
	},
	// 格雷深湖(434)：蓝壳蟹、巨钳蟹
	434: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "蓝壳蟹", LevelMin: 15, LevelMax: 28, Rare: false},
			{Name: "巨钳蟹", LevelMin: 29, LevelMax: 40, Rare: false},
		},
	},
	435: {Monsters: []string{"紫炎虫"}},
	// 墨杜莎星(437)：丫丫、远古甲虫
	437: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "丫丫", LevelMin: 15, LevelMax: 31, Rare: false},
			{Name: "远古甲虫", LevelMin: 32, LevelMax: 40, Rare: false},
		},
	},
	// 石化城(438)：尖嘴鸟
	438: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "尖嘴鸟", LevelMin: 15, LevelMax: 35, Rare: false},
		},
	},
	// 海兹尔星(439)：波伦、波尼斯 + 稀有沃特
	439: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "波伦", LevelMin: 15, LevelMax: 28, Rare: false},
			{Name: "波尼斯", LevelMin: 29, LevelMax: 40, Rare: false},
		},
		Rare: []monsterEntry{
			{Name: "沃特", LevelMin: 20, LevelMax: 30, Rare: true},
		},
	},
	// 月影花园(63)：鲁迪、兰妮（稀有）
	63: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "鲁迪", LevelMin: 20, LevelMax: 30, Rare: false},
		},
		Rare: []monsterEntry{
			{Name: "兰妮", LevelMin: 20, LevelMax: 30, Rare: true},
		},
	},
	459: {Monsters: []string{"克里"}},
	// 般若星(481)：泰达 + 稀有扶来
	481: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "泰达", LevelMin: 30, LevelMax: 40, Rare: false},
		},
		Rare: []monsterEntry{
			{Name: "扶来", LevelMin: 30, LevelMax: 40, Rare: true},
		},
	},
	// 炫彩山山脚(445)：多特、多雷
	445: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "多特", LevelMin: 15, LevelMax: 28, Rare: false},
			{Name: "多雷", LevelMin: 29, LevelMax: 40, Rare: false},
		},
	},
	// 炫彩山岩(446)：亚丁
	446: {
		Slots: 4,
		Common: []monsterEntry{
			{Name: "亚丁", LevelMin: 15, LevelMax: 35, Rare: false},
		},
	},
}

// planetFallback 未单独配置的地图回退到同星球首层，保证任意层都有精灵刷新
var planetFallback = map[int]int{
	11: 10, 12: 10,                     // 克洛斯星 → 草原
	16: 15, 17: 15,                     // 火山星 → 山脚下
	31: 30, 32: 30, 34: 30, 43: 30,     // 赫尔卡星/飞船 → 建筑区
	41: 40,                             // 塞西利亚星寒冰溶洞 → 地表
	59: 57,                             // 尼古尔水帘 → 尼古尔星（米隆、米洛尼）
	430: 429,                           // 活力源泉 → 拓梯星（咕咕芽、咕咕果）
}

// nameToID 缓存：精灵中文名 -> 精灵ID
var nameToID map[string]int

// mapSlotCache 缓存：地图ID -> {生成时间, 槽位列表}，用于保证 2004 和 2408 在刷新间隔内使用同一组怪物
type cachedSlots struct {
	time  time.Time
	slots []Slot
}

var (
	mapSlotCache    = make(map[int]cachedSlots)
	refreshInterval = 10 * time.Second // 每个地图层 10 秒重新抽取一批野怪
)

// buildNameIndex 从 pets 管理器构建中文名索引
func buildNameIndex() {
	if nameToID != nil {
		return
	}
	nameToID = make(map[string]int)
	petMgr := gamepets.GetInstance()
	all := petMgr.All()
	for _, p := range all {
		name := strings.TrimSpace(p.DefName)
		if name == "" {
			continue
		}
		if _, exists := nameToID[name]; !exists {
			nameToID[name] = p.ID
		}
	}
	logger.Info("MapOgres: 已构建精灵名称索引，共计条目: %d", len(nameToID))
}

// resolveMonsterName 将 XML / 配置里的名称解析为精灵ID和闪光标记
// 优先使用 nameToIDFallback，保证克洛斯星等地图野怪用主 ID（如皮皮=10）以便客户端正确显示
func resolveMonsterName(name string) Slot {
	name = strings.TrimSpace(name)
	if name == "" {
		return Slot{}
	}
	shiny := false
	if strings.HasPrefix(name, "#") {
		shiny = true
		name = strings.TrimSpace(strings.TrimPrefix(name, "#"))
	}
	if name == "" {
		return Slot{}
	}

	// 优先用兜底表，确保克洛斯星皮皮等使用主 ID(10)，避免 XML 中 RealId 变体(1400016)导致客户端不显示
	if fid, ok := nameToIDFallback[name]; ok {
		return Slot{PetID: fid, Shiny: shiny}
	}

	buildNameIndex()

	id := nameToID[name]
	if id == 0 && strings.HasPrefix(name, "闪光") {
		alt := strings.TrimPrefix(name, "闪光")
		id = nameToID[alt]
	}
	if id == 0 {
		if fid, ok := nameToIDFallback[name]; ok {
			id = fid
		}
	}
	if id == 0 {
		logger.Warning("MapOgres: 未能找到精灵名称对应的ID: %s", name)
		return Slot{}
	}
	return Slot{PetID: id, Shiny: shiny}
}

// nameToIDFallback 名称→精灵ID 兜底映射（与 PetBook/spt.xml 的 Monster ID 一致，错 ID 会导致刷出错误精灵如贝尔→8 变成烈火猴）
var nameToIDFallback = map[string]int{
	// 克洛斯星 10/11/12
	"皮皮": 10, "闪光皮皮": 164,
	"仙人球": 16, "小豆芽": 27,
	"依依": 83, "依丁丝": 84, "闪光依依": 442,
	// 云霄星 25/26/27（ID 与 xml 一致：22=毛毛 25=幽浮 26=哈尔浮 249=浮空苗 250=浮空鱼）
	"毛毛": 22, "莫比": 53, "幽浮": 25, "小莹蜂": 18, "浮空苗": 249, "哈尔浮": 26, "浮空鱼": 250,
	// 海洋星 20/21/22（ID 与 xml 一致：30=贝尔 33=利牙鱼 198=小鳍鱼；8=烈火猴 19=利利 20=绵绵 不能混用）
	"贝尔": 30, "利牙鱼": 33, "小鳍鱼": 198,
	// 其他
	"米隆": 235, "米洛尼": 236,
	"查斯": 228, "查尔顿": 229,
	"卢比": 136, "波古": 128,
	"嘟咕噜": 254, "嘟噜噜": 252,
	"帕尼": 265, "帕格尼尼": 266,
	"乌凯": 267, "乌力朴": 268,
	"沙顿": 278, "沙罗希瓦": 280,
	"弗曼": 291, "弗里昂": 292,
	"卡西": 100,
	"里昂": 422, "迷你芽": 293, "迷你果": 295,
	"幼镰鸟": 344, "巨镰鸟": 345,
	"嗡嗡蝴": 395,
	"莫顿": 373, "古利安": 374, "隆米尔": 375,
	"洛洛斯": 491, "克洛洛特": 493,
	"鲁格洛": 463, "丁格": 105, "丁加鲁": 106,
	"莱尼": 119, "特鲁尼": 120, "伊卡罗尼": 121,
	"卡丹": 494, "阿零": 499,
	"咕咕芽": 523, "咕咕果": 525,
	"水球": 544,
	"蓝壳蟹": 847, "巨钳蟹": 848,
	"丫丫": 553, "远古甲虫": 554,
	"尖嘴鸟": 827,
	"波伦": 557, "波尼斯": 559,
	"多特": 766, "多雷": 767, "亚丁": 773,
	// 赫鲁卡城(461)（丫丫、嘟噜噜、嘟咕噜已在上方定义）
	"波尼儿": 558, "迪度": 156, "托尼": 158, "霹雳兽": 454, "尼斯克": 96, "迪卢卡": 139, "水草蛙": 211, "西塔": 59, "埃闻": 257, "查克": 150, "夏伏兽": 392,
}

// resolveEntry 将 monsterEntry 解析为带等级的 Slot（等级在 LevelMin~LevelMax 间随机）
// 地图 461 使用 LevelMin=0, LevelMax=0 表示等级由 2408 按“首发精灵等级-5”动态计算，此处保留 Level=0
// 其他地图当 LevelMin<=0 且 LevelMax<=0 时：进化形态（大形态）按 EvolvingLv 作为最低等级，否则默认 5
func resolveEntry(e monsterEntry, mapID int) Slot {
	s := resolveMonsterName(e.Name)
	if s.PetID <= 0 {
		return s
	}
	min, max := e.LevelMin, e.LevelMax
	if max < min {
		max = min
	}
	if mapID == 461 {
		s.Level = 0
		return s
	}
	if min <= 0 {
		evLv := gamepets.GetInstance().GetMinEvolutionLevel(s.PetID)
		if evLv > 1 {
			min, max = evLv, evLv
		} else {
			min, max = 5, 5
		}
	}
	if min == max {
		s.Level = min
	} else {
		s.Level = min + rand.Intn(max-min+1)
	}
	return s
}

// isInLast50Min 是否处于每小时最后 50 分钟（即 minute 10~59）
func isInLast50Min() bool {
	return time.Now().Minute() >= 10
}

// IsLeiyiWeather 是否处于“雷雨天”（赫尔卡星雷伊出场条件，对应 SPT fightCondition="雷雨天"）
// 用时间模拟：每小时的 20~40 分钟为雷雨天，其余时间非雷雨天。
func IsLeiyiWeather() bool {
	m := time.Now().Minute()
	return m >= 20 && m < 40
}

// 赫尔卡星荒地地图 ID，雷伊仅在此地图且雷雨天出现（SPT fightCondition="雷雨天"）
const mapIDHelcarWasteland = 32

// MapIDHelcarWasteland 供 handlers 等包使用
func MapIDHelcarWasteland() int {
	return mapIDHelcarWasteland
}

// generateSlotsInternal 内部生成逻辑，不读缓存；writeCache 为 true 时写入地图缓存
func generateSlotsInternal(mapID int, writeCache bool) []Slot {
	requestedMapID := mapID
	conf, ok := maps[mapID]
	if !ok {
		if fallback, has := planetFallback[mapID]; has {
			mapID = fallback
			conf, ok = maps[mapID]
		}
	}
	if !ok {
		return nil
	}

	// 赫尔卡星荒地(32)：仅雷雨天出现雷伊，非雷雨天不刷任何野怪
	if requestedMapID == mapIDHelcarWasteland {
		now := time.Now()
		min := now.Minute()
		if !IsLeiyiWeather() {
			delete(mapSlotCache, mapIDHelcarWasteland)
			logger.Info("MapOgres: 赫尔卡星荒地(32) 非雷雨天，不刷雷伊 (当前分钟=%d，雷雨天为20~39分)", min)
			return nil
		}
		logger.Info("MapOgres: 赫尔卡星荒地(32) 雷雨天，将刷新雷伊 (当前分钟=%d)", min)
	}

	rand.Seed(time.Now().UnixNano())
	var result []Slot

	// 新逻辑：有 Common/Rare 配置的地图使用两阶段抽取（9 槽抽 3 + 稀有槽二次抽取）
	if len(conf.Common) > 0 || len(conf.Rare) > 0 {
		commonPool := make([]Slot, 0, len(conf.Common))
		for _, e := range conf.Common {
			if s := resolveEntry(e, mapID); s.PetID > 0 {
				commonPool = append(commonPool, s)
			}
		}
		rarePool := make([]Slot, 0, len(conf.Rare))
		for _, e := range conf.Rare {
			if s := resolveEntry(e, mapID); s.PetID > 0 {
				rarePool = append(rarePool, s)
			}
		}
		if len(commonPool) == 0 {
			return nil
		}

		// 一阶段：9 槽 = 1 稀有槽(下标0) + 8 普通槽(下标1~8)，不放回抽 3 个
		slotIndices := []int{0, 1, 2, 3, 4, 5, 6, 7, 8}
		for i := range slotIndices {
			j := i + rand.Intn(9-i)
			slotIndices[i], slotIndices[j] = slotIndices[j], slotIndices[i]
		}
		drawn := make([]Slot, 0, 3)
		n := rareEmptySlots
		if n < 0 {
			n = 0
		}
		rareSlotDenom := 1 + n // 稀有槽内 1 稀有 + N 空，P(稀有)=1/(1+N)
		for i := 0; i < 3; i++ {
			isRareSlot := (slotIndices[i] == 0)
			if isRareSlot && len(rarePool) > 0 {
				// 二阶段：稀有槽内 1 稀有 + N 空，抽 1
				if rand.Intn(rareSlotDenom) == 0 {
					drawn = append(drawn, rarePool[rand.Intn(len(rarePool))])
				} else {
					drawn = append(drawn, commonPool[rand.Intn(len(commonPool))])
				}
			} else {
				drawn = append(drawn, commonPool[rand.Intn(len(commonPool))])
			}
		}

		// 将 3 个结果随机放入 4 个槽位，剩余 1 槽为空
		result = make([]Slot, 4)
		indices := []int{0, 1, 2, 3}
		for i := range indices {
			j := rand.Intn(i + 1)
			indices[i], indices[j] = indices[j], indices[i]
		}
		for i, s := range drawn {
			if i >= 4 {
				break
			}
			result[indices[i]] = s
		}
	} else {
		// 兼容旧配置：仅 Monsters，等级默认 5，仍然使用最多 9 槽位的旧逻辑
		slotsCount := conf.Slots
		if slotsCount <= 0 {
			slotsCount = 9
		}
		if len(conf.Monsters) == 0 {
			return nil
		}
		pool := make([]Slot, 0, len(conf.Monsters))
		for _, name := range conf.Monsters {
			s := resolveMonsterName(name)
			if s.PetID > 0 {
				s.Level = 5
				pool = append(pool, s)
			}
		}
		if len(pool) == 0 {
			return nil
		}
		result = make([]Slot, 0, slotsCount)
		for i := 0; i < slotsCount; i++ {
			result = append(result, pool[rand.Intn(len(pool))])
		}
	}

	// 所有可捕捉的野生精灵统一按 5% 概率为异色（地图显示与战斗/捕捉一致），
	// 不再区分是否在配置中带 # 前缀或“闪光xxx”名称。
	for i := range result {
		if result[i].PetID > 0 {
			result[i].Shiny = rand.Intn(100) < 5
		}
	}

	if writeCache {
		mapSlotCache[mapID] = cachedSlots{
			time:  time.Now(),
			slots: result,
		}
	}
	logger.Info("MapOgres: 地图 %d 生成新的精灵列表，共 %d 个槽位", mapID, len(result))
	return result
}

// GetSlots 返回给定地图应该刷新的怪物槽位（最多 9 个），用于 CMD 2004 / 2408
// 若当前地图未配置，则按星球层回退；地图 32 仅雷雨天返回雷伊。
// 使用地图级缓存，同一地图在 refreshInterval 内返回同一批槽位。
func GetSlots(mapID int) []Slot {
	if cached, ok := mapSlotCache[mapID]; ok {
		if time.Since(cached.time) < refreshInterval && len(cached.slots) > 0 {
			return cached.slots
		}
	}
	return generateSlotsInternal(mapID, true)
}

// GenerateNewSlotsNoCache 为指定地图生成一批新槽位且不写入缓存，用于每玩家每地图的定时刷新（与 Lua 一致）
func GenerateNewSlotsNoCache(mapID int) []Slot {
	return generateSlotsInternal(mapID, false)
}

// GenerateOneSlot 为指定地图随机生成一个槽位（两阶段：9 槽中 1/9 为稀有槽，稀有槽内 1/(1+rareEmptySlots) 出稀有）
func GenerateOneSlot(mapID int) Slot {
	conf, ok := maps[mapID]
	if !ok {
		if fallback, has := planetFallback[mapID]; has {
			mapID = fallback
			conf, ok = maps[mapID]
		}
	}
	if !ok {
		return Slot{}
	}
	if mapID == mapIDHelcarWasteland && !IsLeiyiWeather() {
		return Slot{}
	}
	rand.Seed(time.Now().UnixNano())
	if len(conf.Common) > 0 || len(conf.Rare) > 0 {
		commonPool := make([]Slot, 0, len(conf.Common))
		for _, e := range conf.Common {
			if s := resolveEntry(e, mapID); s.PetID > 0 {
				commonPool = append(commonPool, s)
			}
		}
		rarePool := make([]Slot, 0, len(conf.Rare))
		for _, e := range conf.Rare {
			if s := resolveEntry(e, mapID); s.PetID > 0 {
				rarePool = append(rarePool, s)
			}
		}
		if len(commonPool) == 0 {
			return Slot{}
		}
		var s Slot
		// 一阶段：9 槽抽 1，等价于 1/9 概率抽到稀有槽
		if rand.Intn(9) == 0 && len(rarePool) > 0 {
			n := rareEmptySlots
			if n < 0 {
				n = 0
			}
			if rand.Intn(1+n) == 0 {
				s = rarePool[rand.Intn(len(rarePool))]
			} else {
				s = commonPool[rand.Intn(len(commonPool))]
			}
		} else {
			s = commonPool[rand.Intn(len(commonPool))]
		}
		if s.PetID > 0 {
			s.Shiny = rand.Intn(100) < 5
		}
		return s
	}
	if len(conf.Monsters) == 0 {
		return Slot{}
	}
	pool := make([]Slot, 0, len(conf.Monsters))
	for _, name := range conf.Monsters {
		s := resolveMonsterName(name)
		if s.PetID > 0 {
			s.Level = 5
			pool = append(pool, s)
		}
	}
	if len(pool) == 0 {
		return Slot{}
	}
	s := pool[rand.Intn(len(pool))]
	if s.PetID > 0 {
		s.Shiny = rand.Intn(100) < 5
	}
	return s
}

// GetCachedSlots 获取缓存的精灵槽位，如果没有缓存则生成新的
// 这个函数确保对战时使用与地图显示相同的精灵列表
func GetCachedSlots(mapID int) []Slot {
	// 优先返回缓存的槽位，即使已过期也返回，确保对战一致性
	if cached, ok := mapSlotCache[mapID]; ok && len(cached.slots) > 0 {
		return cached.slots
	}
	// 如果没有缓存，则生成新的
	return GetSlots(mapID)
}

// InvalidateMap 清除指定地图的槽位缓存，用于对战/捕捉结束后让地图重新刷新精灵列表
func InvalidateMap(mapID int) {
	delete(mapSlotCache, mapID)
}
