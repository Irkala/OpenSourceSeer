// Package sptboss 赛尔先锋队 SPT BOSS 配置
// 对应前端 PioneerTaskModel + AchieveXMLInfo + PetBook，全地图 BOSS 精灵、首次击败奖励、成就
package sptboss

import (
	"encoding/xml"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// itemsXMLRoot 用于解析 items.xml 根元素 <Items>
type itemsXMLRoot struct {
	XMLName xml.Name  `xml:"Items"`
	Cats    []itemsCat `xml:"Cat"`
}

// SPTBossEntry 单个 SPT BOSS 配置
type SPTBossEntry struct {
	SPTID        int // 先锋队任务 id (1-20)
	BossPetID    int // BOSS 精灵 ID
	RewardPetID  int // 首次击败奖励精灵 ID，0 表示无奖励
	RewardItemID int // 首次击败奖励物品 ID（精元等），0 表示无物品奖励
	Level        int // BOSS 等级
	HasShield    bool // 是否有防护罩（需 2412 破除）
}

// MapBossEntry 地图上的 BOSS 配置，用于 (mapID, param2) 解析
type MapBossEntry struct {
	BossPetID  int
	Level      int
	HasShield  bool
}

// sptBossByPetID petID -> SPT 配置（用于首次击败奖励与成就）
// Level 字段按“地图 BOSS 调整表”同步更新，便于从 SPT 面板直接发起战斗时也使用统一等级。
// 精元奖励为主：仅蘑菇怪奖励1级精灵，其余 BOSS 奖励对应精元（RewardItemID）
var sptBossByPetID = map[int]SPTBossEntry{
	47:   {1, 47, 46, 0, 10, true},       // 蘑菇怪 -> 小蘑菇
	34:   {2, 34, 0, 400051, 25, false},  // 钢牙鲨 -> 黑晶矿
	42:   {3, 42, 0, 400107, 35, false},  // 里奥斯 -> 里奥斯精元
	50:   {4, 50, 0, 400101, 65, false},  // 阿克希亚 -> 阿克希亚精元
	69:   {5, 69, 0, 400102, 50, false},  // 提亚斯 -> 提亚斯精元
	70:   {6, 70, 0, 400103, 70, false},  // 雷伊 -> 雷伊精元
	88:   {7, 88, 0, 400104, 70, false},  // 纳多雷 -> 纳多雷精元
	113:  {8, 113, 0, 400105, 75, false}, // 雷纳多 -> 雷纳多精元
	132:  {9, 132, 0, 400108, 70, false}, // 尤纳斯 -> 尤纳斯精元
	187:  {10, 187, 0, 400114, 50, false}, // 魔狮迪露 -> 魔狮迪露精元
	216:  {11, 216, 0, 400118, 80, false}, // 哈莫雷特 -> 哈莫雷特精元
	264:  {12, 264, 0, 400125, 60, false}, // 奈尼芬多 -> 奈尼芬多精元
	421:  {13, 421, 0, 400139, 80, false}, // 厄尔塞拉 -> 厄尔塞拉精元
	261:  {14, 261, 0, 400126, 70, false}, // 盖亚 -> 盖亚精元（按周几+地图+条件，由 pushGaiyaRewardOrNotice 单独处理）
	274:  {15, 274, 0, 400136, 80, false}, // 塔克林 -> 塔克林精元
	391:  {16, 391, 0, 400137, 70, false}, // 塔西亚 -> 塔西亚精元
	347:  {17, 347, 0, 400127, 70, false}, // 远古鱼龙 -> 远古鱼龙精元
	393:  {18, 393, 0, 0, 75, false},      // 上古炎兽（无奖励）
	300:  {19, 300, 0, 0, 120, false}, // 谱尼 -> 碎片系统（击败封印/真身给对应碎片，集齐8个碎片合成精元400150）
	4150: {20, 4150, 0, 0, 80, false},    // 拂晓兔（无奖励）
	589:  {0, 589, 588, 0, 100, false},   // 克瑞斯（赫鲁卡城二当家）：首次击败奖励 1 级瑞克
	413:  {0, 413, 0, 0, 75, false},       // 塞维尔（龙族三巨头，成就用）
	166:  {0, 166, 166, 0, 80, false},     // 克洛斯星 BOSS 闪光波克尔 -> 奖励一只闪光波克尔
	501:  {0, 501, 0, 400140, 120, false},  // 玄武巴斯特（地图401 param2=6）-> 巴斯特的精元
	502:  {0, 502, 0, 400145, 120, false},  // 青龙朵拉格（地图403 param2=4）-> 朵拉格的精元
	// 比格星/戴斯/墨杜萨/拉铂尔/拖梯/怀特星系 SPT BOSS：劳克蒙德、克拉尼特、墨杜萨、肯佩德、亚伦斯、卡修斯、德拉萨
	490:  {0, 490, 0, 400151, 80, false},  // 劳克蒙德 -> 劳克蒙德的精元
	538:  {0, 538, 0, 400153, 80, false},  // 克拉尼特 -> 克拉尼特的精元
	587:  {0, 587, 0, 400156, 80, false},  // 墨杜萨 -> 墨杜萨的精元
	617:  {0, 617, 0, 400161, 80, false},  // 肯佩德 -> 肯佩德的精元
	672:  {0, 672, 0, 400187, 80, false},  // 亚伦斯 -> 亚伦斯的精元
	5012: {0, 5012, 0, 400187, 80, false}, // 亚伦斯（SPT 形态 MonID=5012）-> 亚伦斯的精元
	798:  {0, 798, 0, 400202, 80, false},  // 卡修斯 -> 卡修斯精元
	715:  {0, 715, 0, 400192, 80, false},  // 德拉萨 -> 德拉萨的精元
	1337: {0, 1337, 0, 400402, 120, false}, // 机械塔克林 -> 机械塔克林的精元
	1000: {0, 1000, 0, 0, 105, false},     // 奥尔德/光明异能王（地图677 六重试炼+终极试炼）
	// 暗黑武斗场 BOSS 奖励配置（首次击败奖励对应精元）
	171:  {0, 171, 0, 400171, 50, false},  // 暗黑第一门：魔牙鲨 Lv50
	174:  {0, 174, 0, 400174, 60, false},  // 暗黑第二门：贝鲁基德 Lv60
	177:  {0, 177, 0, 400177, 70, false},  // 暗黑第三门：巴弗洛 Lv70
	195:  {0, 195, 0, 400115, 80, false},  // 暗黑第四门：西萨拉斯 Lv80
	222:  {0, 222, 0, 400222, 90, false},  // 暗黑第五门：卡库 Lv90
	356:  {0, 356, 0, 400356, 100, false}, // 暗黑第六门：斯加尔卡 Lv100
	438:  {0, 438, 0, 400438, 100, false}, // 暗黑第七门：魔花使者 Lv100
	656:  {0, 656, 0, 400656, 100, false}, // 暗黑第八门：帕多尼 Lv100
	779:  {0, 779, 0, 400779, 100, false}, // 暗黑第九门：迪普利德 Lv100
	1182: {0, 1182, 0, 4001182, 100, false}, // 暗黑第十门：鳞甲魔鱼 Lv100
	1403: {0, 1403, 0, 4001403, 100, false}, // 暗黑第十一门：萨洛奇斯 Lv100

	// 暗黑武斗场子门 BOSS（仅用于等级/精元占位；具体门映射由暗黑武斗场逻辑控制）
	183:  {0, 183, 0, 400113, 75, false},  // 奇拉塔顿  第3门 3-2  Lv75
	192:  {0, 192, 0, 400116, 85, false},  // 克林卡修  第4门 4-2  Lv85
	224:  {0, 224, 0, 400121, 90, false},  // 赫德卡    第5门 5-2  Lv90
	227:  {0, 227, 0, 400122, 95, false},  // 伊兰罗尼  第5门 5-3  Lv95
	297:  {0, 297, 0, 400128, 100, false}, // 艾尔伊洛  第6门 6-2  Lv100
	359:  {0, 359, 0, 400130, 100, false}, // 布林克克  第6门 6-3  Lv100
	441:  {0, 441, 0, 400143, 100, false}, // 莫尔加斯  第7门 7-2  Lv100
	435:  {0, 435, 0, 400144, 100, false}, // 萨诺拉斯  第7门 7-3  Lv100
	659:  {0, 659, 0, 400185, 100, false}, // 加洛德    第8门 8-2  Lv100
	661:  {0, 661, 0, 400186, 100, false}, // 萨多拉尼  第8门 8-3  Lv100
	784:  {0, 784, 0, 400199, 100, false}, // 拜洛亚斯  第9门 9-2  Lv100
	782:  {0, 782, 0, 400198, 100, false}, // 阿克诺亚  第9门 9-3  Lv100
	1185: {0, 1185, 0, 400305, 100, false}, // 艾斯德克  第10门 10-1 Lv100
	1397: {0, 1397, 0, 400430, 100, false}, // 查迪斯    第11门 11-2 Lv100
	1400: {0, 1400, 0, 400431, 100, false}, // 奈尼狄亚  第11门 11-3 Lv100
}

// essenceItemToPetID 精元物品 ID -> 孵化出的精灵 ID（分子转化仪用）
var essenceItemToPetID = map[int]int{}

type itemsCat struct {
	Items []itemsItem `xml:"Item"`
}

type itemsItem struct {
	ID            int    `xml:"ID,attr"`
	BreedTime     int    `xml:"BreedTime,attr"`
	BreedMonID    string `xml:"BreedMonID,attr"`
	BreedMonIDTes string `xml:"BreedMonID_test,attr"`
}

func init() {
	// 1) 先根据 SPT BOSS 配置填充一批精元映射（兼容原有逻辑）
	for petID, e := range sptBossByPetID {
		if e.RewardItemID > 0 {
			essenceItemToPetID[e.RewardItemID] = petID
		}
	}

	// 2) 再从 items.xml 中解析所有配置了 BreedTime 的精元，补充剩余映射
	// 尝试多种路径：先相对可执行文件目录，再相对当前工作目录
	var candidates []string
	if exePath, exeErr := os.Executable(); exeErr == nil {
		exeDir := filepath.Dir(exePath)
		candidates = append(candidates,
			filepath.Join(exeDir, "xml", "items.xml"),
			filepath.Join(exeDir, "..", "xml", "items.xml"),
			filepath.Join(exeDir, "..", "..", "xml", "items.xml"), // 如 exe 在 cmd/gameserver/
			filepath.Join(exeDir, "..", "..", "golang_version", "xml", "items.xml"),
			filepath.Join(exeDir, "golang_version", "xml", "items.xml"),
		)
	}
	candidates = append(candidates,
		filepath.Join("xml", "items.xml"),
		filepath.Join("golang_version", "xml", "items.xml"),
		filepath.Join("..", "golang_version", "xml", "items.xml"),
	)
	var data []byte
	var itemsPath string
	var err error
	for _, p := range candidates {
		data, err = os.ReadFile(p)
		if err == nil {
			itemsPath = p
			break
		}
	}
	if data == nil {
		log.Printf("sptboss: failed to read items.xml for essence mapping (tried %v)", candidates)
		return
	}
	log.Printf("sptboss: loading essence mapping from %s", itemsPath)

	var root itemsXMLRoot
	if err := xml.Unmarshal(data, &root); err != nil {
		log.Printf("sptboss: failed to parse %s for essence mapping: %v", itemsPath, err)
		return
	}

	added := 0
	for _, cat := range root.Cats {
		for _, it := range cat.Items {
			if it.ID <= 0 || it.BreedTime <= 0 {
				continue
			}
			if _, exists := essenceItemToPetID[it.ID]; exists {
				continue
			}

			monStr := strings.TrimSpace(it.BreedMonID)
			if monStr == "" {
				monStr = strings.TrimSpace(it.BreedMonIDTes)
			}
			if monStr == "" {
				continue
			}
			fields := strings.Fields(monStr)
			if len(fields) == 0 {
				continue
			}
			petID, err := strconv.Atoi(fields[0])
			if err != nil || petID <= 0 {
				continue
			}
			essenceItemToPetID[it.ID] = petID
			added++
		}
	}
	log.Printf("sptboss: essence mapping: %d from SPT + %d from items.xml = %d total", len(essenceItemToPetID)-added, added, len(essenceItemToPetID))
}

// GetPetIDByEssenceItem 精元物品 ID 孵化得到的精灵 ID，0 表示非精元或未知
func GetPetIDByEssenceItem(itemID int) int {
	if petID, ok := essenceItemToPetID[itemID]; ok {
		return petID
	}
	return 0
}

// IsEssenceItem 是否为可孵化的精元物品
func IsEssenceItem(itemID int) bool {
	_, ok := essenceItemToPetID[itemID]
	return ok
}

// bossMapAlias 任务/副本地图 ID -> 正式 BOSS 地图 ID
// 雷伊技能特训等任务会使用与正式地图同名的副本地图（如 912 塞西利亚星 super=40），
// 若不做别名则 2411 用 user.MapID=912 查不到 BOSS，导致无法正常对战阿克西亚/里奥斯
var bossMapAlias = map[int]int{
	912: 40,  // 塞西利亚星任务副本 -> 正式塞西利亚星(40)，解析为阿克希亚
	500: 514, // 勇者之塔(500) 进入神秘领域时服务端可能仍为 500，需解析为 514 才能匹配谱尼
	108: 514, // 太空站左翼(108)：勇者之塔神秘领域为子场景，点击封印/真身时 mapID 常为 108
}

// mapBossConfig (mapID, param2) -> BOSS 配置
// 客户端 fightWithBoss(name, param2) 发送 param2，配合当前地图解析
var mapBossConfig = map[int]map[uint32]MapBossEntry{
	12:  {0: {47, 10, true}, 1: {83, 5, false}}, // 克洛斯星密林: 0=蘑菇怪 1=依依
	22:  {0: {34, 25, false}},                   // 海洋星海底(seatID22/enterID21): 钢牙鲨
	21:  {0: {34, 25, false}},                   // 海洋星深水区
	17:  {0: {42, 35, false}},                   // 火山星山洞深处: 里奥斯
	40:  {0: {50, 65, false}},                   // 塞西利亚星: 阿克希亚
	26:  {0: {153, 15, false}},                  // 云霄星高空层: 小莹蜂（可捕捉 BOSS，Lv 15）
	27:  {0: {69, 50, false}},                   // 云霄星最高层: 提亚斯（Lv 50）
	32:  {0: {70, 70, false}},                   // 赫尔卡星荒地: 雷伊
	34:  {0: {74, 17, false}},                   // 精灵广场: 果冻鸭（可捕捉 BOSS，Lv 17）
	342: {0: {414, 18, false}},                  // 白沙罗域: 乌普（可捕捉 BOSS，Lv 18）
	404: {0: {454, 17, false}},                  // 比格星: 霹雳兽（可捕捉 BOSS，Lv 17）
	411: {0: {463, 17, false}},                  // 陨石地带: 鲁格洛（可捕捉 BOSS，Lv 17）
	415: {0: {474, 17, false}},                  // 巨型陨石: 该伊（可捕捉 BOSS，Lv 17）
	106: {0: {88, 70, false}},                   // 阿尔法星岩地: 纳多雷（Lv 70）
	49:  {0: {113, 75, false}},                  // 贝塔星荒原: 雷纳多（Lv 75）
	48:  {0: {1337, 120, false}},                // 贝塔星机械塔克林：机械塔克林（Lv120）
	314: {0: {132, 70, false}},                  // 拜伦号: 尤纳斯
	53: {
		0: {187, 50, false}, // 斯诺岩洞: 魔狮迪露（Lv 50）
		1: {178, 17, false}, // 斯诺岩洞: 达比拉（可捕捉 BOSS，Lv 17）
	},
	57:  {0: {216, 80, false}},                  // 尼古尔星: 哈莫雷特（fightWithBoss 默认 param2=0）
	60:  {0: {216, 80, false}},                  // 哈莫雷特
	325: {0: {264, 60, false}},                  // 奈尼芬多（Lv 60）
	61:  {0: {421, 80, false}},                  // 光之迷城: 厄尔塞拉（Lv 80）
	// 怀特星系：炫彩山脚(445) 点击卡修斯；怀特星(484) 地图精灵/面板也会触发挑战
	445: {0: {798, 80, false}},                  // 炫彩山山脚: 卡修斯（Lv 80）
	484: {0: {798, 80, false}},                  // 怀特星: 卡修斯（Lv 80）
	486: {0: {715, 80, false}, 1: {715, 80, false}}, // 怀特矿场: 德拉萨（Lv 80，param2=0/1）
	348: {0: {274, 80, false}, 1: {391, 70, false}, 2: {216, 80, false}, 3: {413, 75, false}}, // 塔克林/塔西亚/哈莫雷特/塞维尔（塔克林 Lv 80）
	59:  {0: {347, 70, false}},                    // 远古鱼龙
	16:  {0: {393, 75, false}},                    // 上古炎兽
	10:  {0: {166, 80, false}},                    // 闪光波克尔
	419: {0: {261, 70, false}},                    // 暗影峭壁: 盖亚（Lv 70），PetBook Foundin=暗影峭壁 mapID=419
	// 盖亚三地图（按周几出现，对应不同挑战条件）：火山星15=两回合内击败，露西欧星54=致命一击击败，双子阿尔法星105=十回合后击败
	15:  {0: {261, 70, false}},                    // 火山星: 盖亚（周一、周五）
	54:  {0: {261, 70, false}},                    // 露西欧星: 盖亚（周二、周四、周日）
	105: {
		0: {261, 70, false}, // 双子阿尔法星: 盖亚（周三、周六）
		1: {91, 17, false},  // 双子阿尔法星: 悠悠（可捕捉 BOSS，Lv 17）
	},
	// 勇者之塔神秘领域(514)：谱尼 7 大封印 + 真身，等效 120 级、显示 ?? 级，客户端 param2=1~7 封印、8 真身、0 任务首次
	514: {
		0: {300, 120, false}, // 谱尼（任务首次/回退）
		1: {300, 120, false}, // 第一封印：虚无
		2: {300, 120, false}, // 第二封印：元素
		3: {300, 120, false}, // 第三封印：能量
		4: {300, 120, false}, // 第四封印：生命
		5: {300, 120, false}, // 第五封印：轮回
		6: {300, 120, false}, // 第六封印：永恒
		7: {300, 120, false}, // 第七封印：圣洁
		8: {300, 120, false}, // 谱尼真身
	},
	// 暗黑武斗场(110)：试炼之门守门精灵卡特斯；
	// 地图 503-513 为暗黑第一门～第十一门，param2 区分子门（如 3-1/3-2、11-1/11-2/11-3）
	110: {0: {169, 80, false}}, // 试炼之门：卡特斯
	503: { // 暗黑第一门：只有 1 个 Boss
		0: {171, 80, false}, // 1-1 魔牙鲨
	},
	504: { // 暗黑第二门：只有 1 个 Boss
		0: {174, 80, false}, // 2-1 贝鲁基德
	},
	505: { // 暗黑第三门：巴弗洛 / 奇拉塔顿
		0: {177, 80, false}, // 3-1 巴弗洛
		1: {183, 75, false}, // 3-2 奇拉塔顿
	},
	506: { // 暗黑第四门：西萨拉斯 / 克林卡修
		0: {195, 80, false}, // 4-1 西萨拉斯
		1: {192, 85, false}, // 4-2 克林卡修
	},
	507: { // 暗黑第五门：卡库 / 赫德卡 / 伊兰罗尼
		0: {222, 90, false}, // 5-1 卡库
		1: {224, 90, false}, // 5-2 赫德卡
		2: {227, 95, false}, // 5-3 伊兰罗尼
	},
	508: { // 暗黑第六门：斯加尔卡 / 艾尔伊洛 / 布林克克
		0: {356, 100, false}, // 6-1 斯加尔卡
		1: {297, 100, false}, // 6-2 艾尔伊洛
		2: {359, 100, false}, // 6-3 布林克克
	},
	509: { // 暗黑第七门：魔花使者 / 莫尔加斯 / 萨诺拉斯
		0: {438, 100, false}, // 7-1 魔花使者
		1: {441, 100, false}, // 7-2 莫尔加斯
		2: {435, 100, false}, // 7-3 萨诺拉斯
	},
	510: { // 暗黑第八门：帕多尼 / 加洛德 / 萨多拉尼
		0: {656, 100, false}, // 8-1 帕多尼
		1: {659, 100, false}, // 8-2 加洛德
		2: {661, 100, false}, // 8-3 萨多拉尼
	},
	511: { // 暗黑第九门：迪普利德 / 拜洛亚斯 / 阿克诺亚
		0: {779, 100, false}, // 9-1 迪普利德
		1: {784, 100, false}, // 9-2 拜洛亚斯
		2: {782, 100, false}, // 9-3 阿克诺亚
	},
	512: { // 暗黑第十门：鳞甲魔鱼 / 艾斯德克
		0: {1182, 100, false}, // 10-1 鳞甲魔鱼
		1: {1185, 100, false}, // 10-2 艾斯德克（你的表中标注为第10门）
	},
	513: { // 暗黑第十一门：萨洛奇斯 / 查迪斯 / 奈尼迪亚
		0: {1403, 100, false}, // 11-1 萨洛奇斯
		1: {1397, 100, false}, // 11-2 查迪斯
		2: {1400, 100, false}, // 11-3 奈尼迪亚
	},
	// 玄武空间(401)：守护兽轮流上场，全部 100 级 5000 血；全部击败后才能挑战玄武真身
	// param2=0~5 对应 蘑菇怪(47)、钢牙鲨(34)、里奥斯(42)、提亚斯(69)、纳多雷(88)、雷纳多(113)
	// param2=6 玄武巴斯特真身(501) 120级 50000血 不吃异常 不受能力下降
	401: {
		0: {47, 100, false},   // 蘑菇怪
		1: {34, 100, false},  // 钢牙鲨
		2: {42, 100, false},  // 里奥斯
		3: {69, 100, false},  // 提亚斯
		4: {88, 100, false},  // 纳多雷
		5: {113, 100, false}, // 雷纳多
		6: {501, 120, false}, // 玄武巴斯特真身
	},
	// 青龙空间(403)：守护兽轮流上场，全部 120 级 5000 血；全部击败后才能挑战朵拉格真身
	// param2=0~3 对应 奈尼芬多(264)、塔西亚(391)、塔克林(274)、厄尔塞拉(421)
	// param2=4 朵拉格真身(502) 120级 50000血 每回合回1000
	403: {
		0: {264, 120, false},  // 奈尼芬多
		1: {391, 120, false},  // 塔西亚
		2: {274, 120, false},  // 塔克林
		3: {421, 120, false},  // 厄尔塞拉
		4: {502, 120, false},  // 朵拉格真身
	},
	// 沃尔夫洞穴(423)：SPT 劳克蒙德三种规则领域 + 盖亚特训副本心魔雷伊
	// 前端 MapProcess_423 中：hours=11/14/19 分别使用 fightWithBoss("劳克蒙德",0/1/2)
	// XML 中三种领域的 SPT 劳克蒙德 Hp 均为 2500，Lv=80。
	423: {
		0: {490, 80, false}, // 劳克蒙德·愈合领域
		1: {490, 80, false}, // 劳克蒙德·极限领域
		2: {490, 80, false}, // 劳克蒙德·能量领域
		4: {70, 70, false}, // 雷伊（盖亚心魔）
	},
	// 戴斯星第一场景(435)：SPT 克拉尼特
	// MapProcess_435.fightBoss() 中直接 fightWithBoss("克拉尼特")，param2=0。
	// XML 中困难难度 SPT 克拉尼特 Hp=1500，Lv=80。
	435: {
		0: {538, 80, false}, // 克拉尼特（困难 / 先锋队挑战使用）
		1: {547, 60, false}, // 紫炎虫（可捕捉 BOSS，戴斯星）
	},
	// 墨杜萨任务场景(438)：SPT 墨杜萨
	// MapProcess_438 中使用 fightWithBoss("墨杜萨")，param2=0。
	// XML 中 SPT 墨杜萨 Hp=1200，Lv=80。
	438: {
		0: {587, 80, false}, // 墨杜萨
	},
	// 拖梯星第二场景(430)：SPT 亚伦斯
	// MapProcess_430.onSPT672Click 中使用 fightWithBoss("亚伦斯")，param2=0。
	// XML 中 SPT 亚伦斯（MonID=5012） Hp=2000，Lv=80。
	430: {
		0: {5012, 80, false}, // 亚伦斯（SPT 形态）
	},
	// 拉铂尔星(441)：SPT 肯佩德
	// MapProcess_441.sptClick 中使用 FightBossController.fightBoss("肯佩德")，param2=0。
	// XML 中 SPT 肯佩德 Hp=1200，Lv=80。
	441: {
		0: {617, 80, false}, // 肯佩德
	},
	// 赫鲁卡城(461)：二当家 -> 定制 BOSS 克瑞斯
	461: {
		0: {589, 100, false}, // 克瑞斯 Lv100
	},
	// 精灵太空站(9515)：布鲁（可捕捉 BOSS，Lv 15，每日一次由 2409 逻辑控制）
	9515: {
		0: {108, 15, false}, // 布鲁（可捕捉）
	},
	// 希尔星(677) 封印之境：光明异能王六重试炼 + 终极试炼；前端 round_1~6 对应 region=0~5，终极按钮 region=6
	// 精灵均为奥尔德(1000)，等级 105；六重试炼血量见 YiNengSealHP，终极试炼 5 条命见 YiNengUltimateLifeMaxHP
	677: {
		0: {1000, 105, false},  // 第一封印（力量试炼）
		1: {1000, 105, false},  // 第二封印（勇气试炼）
		2: {1000, 105, false},  // 第三封印（仁慈试炼）
		3: {1000, 105, false},  // 第四封印（智慧试炼）
		4: {1000, 105, false},  // 第五封印（正义试炼）
		5: {1000, 105, false},  // 第六封印（坚韧试炼）
		6: {1000, 105, false},  // 终极试炼（5 条命，每命血量见 YiNengUltimateLifeMaxHP）
	},
}

// 玄武空间(401)：六守护兽 param2=0~5，真身 param2=6；击败六守护兽后 mask 为 0x3F 才能挑战真身
const (
	MapIDXuanWuSpace   = 401
	XuanWuGuardianMask = 0x3F // 六守护兽全击败后的 mask
)

// XuanWuGuardianPetIDs 玄武空间六守护兽精灵 ID（param2 0~5 对应）
var XuanWuGuardianPetIDs = []int{47, 34, 42, 69, 88, 113}

// IsXuanWuGuardianBoss 是否为玄武空间守护兽战（401 且 param2 0~5）
func IsXuanWuGuardianBoss(mapID int, region uint32) bool {
	return mapID == MapIDXuanWuSpace && region <= 5
}

// XuanWuGuardianHP 玄武空间守护兽固定血量
const XuanWuGuardianHP = 5000

// 青龙空间(403)：四守护兽 param2=0~3，真身 param2=4；击败四守护兽后 mask 为 0xF 才能挑战朵拉格
const (
	MapIDQingLongSpace   = 403
	QingLongGuardianMask = 0xF // 四守护兽全击败后的 mask
)

// QingLongGuardianPetIDs 青龙空间四守护兽精灵 ID（param2 0~3 对应 奈尼芬多、塔西亚、塔克林、厄尔塞拉）
var QingLongGuardianPetIDs = []int{264, 391, 274, 421}

// IsQingLongGuardianBoss 是否为青龙空间守护兽战（403 且 param2 0~3）
func IsQingLongGuardianBoss(mapID int, region uint32) bool {
	return mapID == MapIDQingLongSpace && region <= 3
}

// QingLongGuardianHP 青龙空间守护兽固定血量
const QingLongGuardianHP = 10000

// PetIDDuoLaGe 朵拉格（青龙神兽）精灵 ID
const PetIDDuoLaGe = 502

// 亚伦斯（冰超能系）相关配置
// 注意：部分地图/脚本可能使用亚伦斯的“SPT 形态”怪物ID(5012)发起挑战。
const (
	PetIDYaLunSi       = 672
	PetIDYaLunSiSPT    = 5012
	YaLunSiBossHP      = 5000
	YaLunSiRegenPerTurn = 100
	YaLunSiDrainPerTurn = 100
)

// IsYaLunSiBoss 是否为亚伦斯（含 SPT 形态 ID）
func IsYaLunSiBoss(petID int) bool {
	return petID == PetIDYaLunSi || petID == PetIDYaLunSiSPT
}

// QingLongBossHP 朵拉格真身固定血量
const QingLongBossHP = 50000

// QingLongBossRegenHP 朵拉格每回合恢复血量
const QingLongBossRegenHP = 1000

// 希尔星(677) 封印之境：异能王六重试炼 region=0~5，终极试炼 region=6（5 条命）
const MapIDYiNengWang = 677

// yiNengSealHP 六重试炼各封印固定血量（与 XML Boss 配置一致：第一封印 2000，第二~六封印 10000）
var yiNengSealHP = map[uint32]int{
	0: 2000,   // 第一封印 异能王第一封印
	1: 10000,  // 第二封印
	2: 10000,  // 第三封印
	3: 10000,  // 第四封印
	4: 10000,  // 第五封印
	5: 10000,  // 第六封印
}

// YiNengSealHP 返回地图 677 六重试炼(region 0~5)的固定血量，非六重试炼返回 0
func YiNengSealHP(region uint32) int {
	if hp, ok := yiNengSealHP[region]; ok && hp > 0 {
		return hp
	}
	return 0
}

// IsYiNengSealBoss 是否为地图 677 六重试炼（region 0~5，单条命）
func IsYiNengSealBoss(mapID int, region uint32) bool {
	return mapID == MapIDYiNengWang && region <= 5
}

// YiNengUltimateLives 终极试炼(region=6)总命数
const YiNengUltimateLives = 5

// YiNengUltimateLifeMaxHP 终极试炼每条命的血量（与 XML 5 个 BossMon 一致：2000, 8000, 20000, 10000, 50000）
func YiNengUltimateLifeMaxHP(life int) int {
	switch life {
	case 1:
		return 2000
	case 2:
		return 8000
	case 3:
		return 20000
	case 4:
		return 10000
	case 5:
		return 50000
	default:
		return 2000
	}
}

// IsYiNengUltimateBoss 是否为地图 677 终极试炼（region=6，5 条命）
func IsYiNengUltimateBoss(mapID int, region uint32) bool {
	return mapID == MapIDYiNengWang && region == 6
}

// yiNengSealTaskID 六重试炼(region 0~5)对应的任务 ID（758~763），用于击败后完成任务并开放下一重/终极挑战
var yiNengSealTaskID = map[uint32]int{
	0: 758, 1: 759, 2: 760, 3: 761, 4: 762, 5: 763,
}

// yiNengSealItemID 六重试炼(region 0~5)首次击败奖励道具（力量之魂、勇气之魂等，与 43.xml 描述一致）
var yiNengSealItemID = map[uint32]int{
	0: 300376, // 第一封印 力量之魂
	1: 300377, // 第二封印 勇气之魂
	2: 300378, // 第三封印 仁慈之魂
	3: 300379, // 第四封印 智慧之魂
	4: 300380, // 第五封印 正义之魂
	5: 300381, // 第六封印 坚韧之魂
}

// YiNengSealTaskID 返回 region 0~5 对应的任务 ID，非六重试炼返回 0
func YiNengSealTaskID(region uint32) int {
	if id, ok := yiNengSealTaskID[region]; ok {
		return id
	}
	return 0
}

// YiNengSealItemID 返回 region 0~5 对应的之魂道具 ID，非六重试炼返回 0
func YiNengSealItemID(region uint32) int {
	if id, ok := yiNengSealItemID[region]; ok {
		return id
	}
	return 0
}

// YiNengUltimateTaskID 终极试炼(region=6)对应的任务 ID
const YiNengUltimateTaskID = 1237

// YiNengUltimateItemID 终极试炼首次击败奖励：异能王的精元（400417）
const YiNengUltimateItemID = 400417

// 谱尼(300) 勇者之塔神秘领域(514)：七大封印 + 真身，region 1~7 为封印、8 为真身、0 为任务首次
const (
	PetIDPuni         = 300
	MapIDPuniTower    = 514
	PuniEffectiveLv   = 120  // 战斗计算用等效等级（显示为 ?? 级）
	PuniDisplayLv     = 255  // 客户端显示 ?? 级（0 会显示 0级，255 表示未知等级）
	// PuniSuperHP 厉害谱尼（一命无限复活形态）的固定血量
	PuniSuperHP = 65000
	PuniSealVoid      = 1    // 第一封印：虚无
	PuniSealElement   = 2    // 第二封印：元素
	PuniSealEnergy    = 3    // 第三封印：能量
	PuniSealLife      = 4    // 第四封印：生命
	PuniSealSamsara   = 5    // 第五封印：轮回（2 条命）
	PuniSealEternal   = 6    // 第六封印：永恒
	PuniSealHoly      = 7    // 第七封印：圣洁
	PuniTrueForm      = 8    // 真身（多轮回命）
)

// 谱尼碎片物品ID映射：BossRegion -> 碎片物品ID
var puniFragmentItemIDs = map[uint32]int{
	PuniSealVoid:    400651, // 谱尼的虚无裂片
	PuniSealElement: 400652, // 谱尼的元素裂片
	PuniSealEnergy:  400653, // 谱尼的能量裂片
	PuniSealLife:    400654, // 谱尼的生命裂片
	PuniSealSamsara: 400655, // 谱尼的轮回裂片
	PuniSealEternal: 400656, // 谱尼的永恒裂片
	PuniSealHoly:    400657, // 谱尼的圣洁裂片
	PuniTrueForm:    400658, // 谱尼的真身裂片
}

// GetPuniFragmentItemID 根据谱尼封印/真身region返回对应的碎片物品ID，0表示无效region
func GetPuniFragmentItemID(region uint32) int {
	if itemID, ok := puniFragmentItemIDs[region]; ok {
		return itemID
	}
	return 0
}

// PuniEssenceItemID 谱尼精元物品ID（集齐8个碎片后合成）
const PuniEssenceItemID = 400150

// puniSealHP 各封印/真身固定血量（约值，与怀旧服一致）
var puniSealHP = map[uint32]int{
	0: 6500,  // 任务首次/回退
	1: 5000,  // 第一封印：虚无
	2: 6000,  // 第二封印：元素
	3: 7000,  // 第三封印：能量
	4: 8000,  // 第四封印：生命
	5: 10000, // 第五封印：轮回
	6: 13000, // 第六封印：永恒
	7: 16000, // 第七封印：圣洁
	8: 10000, // 真身（每命基础 HP 另有多命配置）
}

// IsPuniSealBoss 是否为谱尼封印/真身战（地图 514/108/500 且 region 1~8 或 0）
// 108=太空站左翼、500=勇者之塔 均为神秘领域入口别名，BattleMapID 可能为此
func IsPuniSealBoss(mapID int, region uint32) bool {
	if mapID != MapIDPuniTower && mapID != 108 && mapID != 500 {
		return false
	}
	return region <= 8
}

// PuniSealHP 谱尼封印/真身血量，非谱尼封印时返回 0（调用方用 original）
func PuniSealHP(region uint32) int {
	if hp, ok := puniSealHP[region]; ok && hp > 0 {
		return hp
	}
	return 0
}

// PuniSealEffectiveLevel 谱尼封印/真身计算用等级（120）
func PuniSealEffectiveLevel() int {
	return PuniEffectiveLv
}

// PuniNeedsDisplayLevelZero 谱尼封印/真身时客户端显示 ?? 级（传 0）
func PuniNeedsDisplayLevelZero(mapID int, region uint32) bool {
	return IsPuniSealBoss(mapID, region)
}

// PuniHasInfinitePP 第四/六/七封印与真身 PP 无限
func PuniHasInfinitePP(region uint32) bool {
	switch region {
	case PuniSealLife, PuniSealEternal, PuniSealHoly, PuniTrueForm:
		return true
	}
	return false
}

// PuniLivesForRegion 轮回(5)2 条命、真身(8)6 条命，其余 0
func PuniLivesForRegion(region uint32) int {
	switch region {
	case PuniSealSamsara:
		return 2
	case PuniTrueForm:
		return 6
	}
	return 0
}

// PuniTrueFormLifeMaxHP 真身每一命的血量配置
// 1命7000，2/3命8000，4命10000，5命20000，6命65000
func PuniTrueFormLifeMaxHP(life int) int {
	switch life {
	case 1:
		return 7000
	case 2, 3:
		return 8000
	case 4:
		return 10000
	case 5:
		return 20000
	case 6:
		return 65000
	default:
		return 7000
	}
}

// PuniTrueFormLifeHasVoidEternal 第1/5命拥有“虚无+永恒”
func PuniTrueFormLifeHasVoidEternal(life int) bool {
	return life == 1 || life == 5
}

// PuniTrueFormLifeIsElement 第2命“元素”
func PuniTrueFormLifeIsElement(life int) bool {
	return life == 2
}

// PuniTrueFormLifeIsEnergy 第3命“能量”
func PuniTrueFormLifeIsEnergy(life int) bool {
	return life == 3
}

// PuniTrueFormLifeHasHeal2000 第4命每回合回 2000；第6命也按“永恒+准轮回”附带高续航（同样每回合回 2000）
func PuniTrueFormLifeHasHeal2000(life int) bool {
	return life == 4 || life == 6
}

// PuniTrueFormLifeHasEternal 第1/5/6命具备“永恒”
func PuniTrueFormLifeHasEternal(life int) bool {
	return life == 1 || life == 5 || life == 6
}

// PuniTrueFormLifeEnergyThreshold 第3命能量的“反噬阈值”（超过阈值我方即死）；真身按 100
func PuniTrueFormLifeEnergyThreshold(life int) uint32 {
	if life == 3 {
		return 100
	}
	return 1000
}

// StatusImmuneBossIDs 免疫所有异常状态（烧伤/冻伤/睡眠/麻痹/中毒/畏缩等）的 BOSS 精灵 ID
// 雷伊、哈莫雷特、奈尼芬多、盖亚、玄武巴斯特、朵拉格、奥尔德（异能王）
var StatusImmuneBossIDs = map[int]bool{
	70:   true,  // 雷伊
	216:  true,  // 哈莫雷特
	264:  true,  // 奈尼芬多
	261:  true,  // 盖亚
	798:  true,  // 卡修斯（怀特星）
	715:  true,  // 德拉萨（怀特矿场）
	501:  true,  // 玄武巴斯特（不吃异常）
	502:  true,  // 朵拉格（青龙神兽，不吃异常）
	1000: true,  // 奥尔德/光明异能王（地图677）
}

// ControlImmuneBossIDs 免疫控制类异常（睡眠/麻痹/畏缩等）的 BOSS，包含所有 SPT/地图 BOSS。
// 阿克希亚(50) 需要在雷伊技能特训中被麻痹，因此从控制免疫名单中剔除，便于按原设定完成“麻痹状态下击败阿克希亚”的条件。
var ControlImmuneBossIDs = func() map[int]bool {
	m := make(map[int]bool)
	for id := range sptBossByPetID {
		m[id] = true
	}
	for _, byParam := range mapBossConfig {
		for _, e := range byParam {
			if e.BossPetID != 0 {
				m[e.BossPetID] = true
			}
		}
	}
	// 允许阿克希亚(50)吃控制类异常（麻痹/睡眠/畏缩等），满足雷伊技能特训的麻痹判定需求。
	delete(m, 50)
	return m
}()

// IsControlImmune 该精灵 ID 是否为免疫控制类异常的 BOSS（所有 BOSS 均免疫）
func IsControlImmune(petID int) bool {
	return ControlImmuneBossIDs[petID]
}

// IsStatusImmune 该精灵 ID 是否为免疫异常状态的 BOSS
func IsStatusImmune(petID int) bool {
	return StatusImmuneBossIDs[petID]
}

// StatDropImmuneBossIDs 免疫能力下降的 BOSS（不会出现负向能力等级，但强化可被清除或降到 0）
// 纳多雷、雷纳多、尤纳斯、哈莫雷特、盖亚、塔克林、塔西亚、谱尼（七大封印+真身）、玄武巴斯特、朵拉格、奥尔德
var StatDropImmuneBossIDs = map[int]bool{
	88:   true,  // 纳多雷
	113:  true,  // 雷纳多
	132:  true,  // 尤纳斯
	216:  true,  // 哈莫雷特
	261:  true,  // 盖亚
	274:  true,  // 塔克林
	391:  true,  // 塔西亚
	300:  true,  // 谱尼（七大封印+真身）
	715:  true,  // 德拉萨（怀特矿场）
	501:  true,  // 玄武巴斯特（不受能力下降）
	502:  true,  // 朵拉格（青龙神兽，不受能力下降）
	1000: true,  // 奥尔德/光明异能王（地图677）
	589:  true,  // 克瑞斯（赫鲁卡城二当家定制 BOSS，不吃弱化）
	1337: true,  // 机械塔克林：免疫能力下降
}

// IsStatDropImmune 该精灵 ID 是否为免疫能力下降的 BOSS（能力等级不低于 0，强化可被清除）
func IsStatDropImmune(petID int) bool {
	return StatDropImmuneBossIDs[petID]
}

// SameLifeDeathImmuneBossIDs 免疫技能「同生共死」效果的 BOSS 精灵 ID
// 尤纳斯、哈莫雷特、盖亚、塔克林、塔西亚、雷伊
var SameLifeDeathImmuneBossIDs = map[int]bool{
	132: true, // 尤纳斯
	216: true, // 哈莫雷特
	261: true, // 盖亚
	274: true, // 塔克林
	391: true, // 塔西亚
	70:  true, // 雷伊
	798: true, // 卡修斯（怀特星）
	715: true, // 德拉萨（怀特矿场）
	300: true, // 谱尼（封印+真身全部形态）
	589:  true, // 克瑞斯（赫鲁卡城二当家定制 BOSS，不受同生共死）
	1337: true, // 机械塔克林：与塔克林相同，免疫同生共死
}

// IsSameLifeDeathImmune 该精灵 ID 是否为免疫同生共死效果的 BOSS
func IsSameLifeDeathImmune(petID int) bool {
	return SameLifeDeathImmuneBossIDs[petID]
}

// 魔狮迪露（除魔师迪露）精灵 ID，唯一不免疫「极度冰点」的 BOSS
const PetIDMoShiDiLu = 187

// IsExtremeFreezeImmune 是否免疫技能「极度冰点」（effect 36 一击必杀）
// 规则：除魔狮迪露(187)外，所有 BOSS 免疫极度冰点
func IsExtremeFreezeImmune(petID int) bool {
	if petID == PetIDMoShiDiLu {
		return false
	}
	return IsControlImmune(petID)
}

// InfinitePPBossIDs 所有技能 PP 无限的 BOSS（雷伊、魔狮迪露、奈尼芬多、盖亚、尤纳斯）
var InfinitePPBossIDs = map[int]bool{
	70:  true,  // 雷伊
	187: true,  // 魔狮迪露
	264: true,  // 奈尼芬多
	261: true,  // 盖亚
	132: true,  // 尤纳斯
}

// IsInfinitePPBoss 该精灵 ID 是否为技能 PP 无限的 BOSS
func IsInfinitePPBoss(petID int) bool {
	return InfinitePPBossIDs[petID]
}

// FirstStrikeBossIDs 回合开始我方已 0 HP 时 2505 包顺序仍按“敌方先”的 BOSS。
// 原版为盖亚+雷伊；这里保留原版设定。
var FirstStrikeBossIDs = map[int]bool{
	261: true, // 盖亚
	70:  true, // 雷伊
}

// IsFirstStrikeBoss 该精灵 ID 是否在 FirstStrikeBossIDs 中（用于回合初我方已死时的 2505 顺序）
func IsFirstStrikeBoss(petID int) bool {
	return FirstStrikeBossIDs[petID]
}

// PriorityBonusBossIDs 所有技能先制+6 的 BOSS（雷伊、盖亚）；先手由速度+技能先制正常比较决定
var PriorityBonusBossIDs = map[int]bool{
	70:  true, // 雷伊
	261: true, // 盖亚
}

// GetPriorityBonus 该 BOSS 技能先制加成（雷伊/盖亚 +6，其余 0）
func GetPriorityBonus(petID int) int {
	if PriorityBonusBossIDs[petID] {
		return 6
	}
	return 0
}

// HalfHPOneShotBossIDs 体力低于一半时：先制+6，且任意技能（属性/攻击）必定秒杀我方当前精灵的 BOSS
var HalfHPOneShotBossIDs = map[int]bool{
	187: true, // 魔狮迪露
}

// IsHalfHPOneShotBoss 该精灵 ID 是否为“半血后先制+6且秒杀”的 BOSS
func IsHalfHPOneShotBoss(petID int) bool {
	return HalfHPOneShotBossIDs[petID]
}

// GetPriorityBonusWithHP 考虑当前体力的先制加成：雷伊/盖亚 恒+6；魔狮迪露 体力低于一半时 +6
func GetPriorityBonusWithHP(petID int, currentHP, maxHP uint32) int {
	if PriorityBonusBossIDs[petID] {
		return 6
	}
	if IsHalfHPOneShotBoss(petID) && maxHP > 0 && currentHP*2 < maxHP {
		return 6
	}
	return 0
}

// DamageTakenMultiplierBossIDs 受到我方攻击伤害（非异常状态伤害）乘 N 倍的 BOSS；N 由 GetDamageTakenMultiplier 返回
var DamageTakenMultiplierBossIDs = map[int]int{
	187: 10, // 魔狮迪露：受到的攻击伤害 ×10
}

// GetDamageTakenMultiplier 该 BOSS 受到攻击伤害的倍数（1=不变，10=十倍）；仅对攻击技能伤害有效，异常状态伤害不乘
func GetDamageTakenMultiplier(petID int) int {
	if n, ok := DamageTakenMultiplierBossIDs[petID]; ok && n > 0 {
		return n
	}
	return 1
}

// 属性类型（与 battle/typeChart、技能 Type 一致）：1=草 2=水 3=火
const (
	TypeGrass = 1
	TypeWater = 2
	TypeFire  = 3
)

// HaMoLeiTeRequiredType 哈莫雷特(216) 顺序破防：必须始终按 水系(2)→火系(3)→草系(1) 循环命中才能受伤，非当前所需属性伤害为 0
func HaMoLeiTeRequiredType(phase int) int {
	switch phase % 3 {
	case 0:
		return TypeWater // 水系
	case 1:
		return TypeFire  // 火系
	case 2:
		return TypeGrass // 草系
	default:
		return TypeWater
	}
}

// IsHaMoLeiTeOrderBoss 该精灵 ID 是否为哈莫雷特顺序破防 BOSS
func IsHaMoLeiTeOrderBoss(petID int) bool {
	return petID == 216
}

// 尤纳斯(132) 规则：贯穿水枪破防 → 保留 1 血 → 仅里奥斯幻影可击杀
const (
	SkillIDPiercingWater = 10323 // 贯穿水枪
	SkillIDPhantom       = 10100 // 幻影（里奥斯）
	PetIDLiAoS           = 42    // 里奥斯
)

// IsYouNaSiBoss 该精灵 ID 是否为尤纳斯（适用贯穿水枪/幻影击杀规则）
func IsYouNaSiBoss(petID int) bool {
	return petID == 132
}

// GetByPetID 根据 BOSS 精灵 ID 获取 SPT 配置
func GetByPetID(bossPetID int) (SPTBossEntry, bool) {
	e, ok := sptBossByPetID[bossPetID]
	return e, ok
}

// darkPortalDoorBoss 暗黑武斗场门索引(0-10) → BOSS 精灵 ID，与 PetBook 暗黑第X门守护者一致
// 门0魔牙鲨 门1贝鲁基德 门2巴弗洛 门3西萨拉斯 门4卡库 门5斯加尔卡 门6魔花使者 门7帕多尼 门8迪普利德 门9鳞甲魔鱼 门10萨洛奇斯
var darkPortalDoorBoss = []int{171, 174, 177, 195, 222, 356, 438, 656, 779, 1182, 1403}

// GetDarkPortalBoss 根据暗黑之门索引返回该门 BOSS 精灵 ID，用于 CMD 2424 OPEN_DARKPORTAL 响应
func GetDarkPortalBoss(doorIndex uint32) (bossPetID int, ok bool) {
	if doorIndex >= uint32(len(darkPortalDoorBoss)) {
		return 0, false
	}
	return darkPortalDoorBoss[doorIndex], true
}

// GetByMapAndParam 根据地图 ID 和 param2 获取 BOSS 配置
// 若当前地图为任务副本地图（如 912），会先解析为正式 BOSS 地图（40）再查表，避免与雷伊技能特训等任务冲突
func GetByMapAndParam(mapID int, param2 uint32) (MapBossEntry, bool) {
	if canonical, ok := bossMapAlias[mapID]; ok {
		mapID = canonical
	}
	m, ok := mapBossConfig[mapID]
	if !ok {
		return MapBossEntry{}, false
	}
	e, ok := m[param2]
	if !ok {
		return MapBossEntry{}, false
	}
	if e.BossPetID == 0 {
		return MapBossEntry{}, false
	}
	return e, true
}

// GetMapIDsWithBoss 返回有 MAP_BOSS 的地图 ID 列表（用于 buildMapBossList）
func GetMapIDsWithBoss() []int {
	ids := make([]int, 0, len(mapBossConfig))
	for id := range mapBossConfig {
		ids = append(ids, id)
	}
	return ids
}

// HasShield 该地图+region 的 BOSS 是否有防护罩（会先按 bossMapAlias 解析副本地图）
func HasShield(mapID int, region uint32) bool {
	if canonical, ok := bossMapAlias[mapID]; ok {
		mapID = canonical
	}
	m, ok := mapBossConfig[mapID]
	if !ok {
		return false
	}
	e, ok := m[region]
	return ok && e.HasShield
}

// BuildBossAchievement 从 DefeatedSPTBossIds 构建 bossAchievement 200 字节
// 前端 UserInfo.setForLoginInfo/setForMoreInfo 读取 200 个 byte 为 Boolean
// PioneerTaskModel: bossAchievement[id-1] 对应 SPT id 1..20
func BuildBossAchievement(defeatedSPTBossIds []int) []byte {
	out := make([]byte, 200)
	petIDToSPTIndex := make(map[int]int)
	for pid, e := range sptBossByPetID {
		if e.SPTID >= 1 && e.SPTID <= 200 {
			petIDToSPTIndex[pid] = e.SPTID - 1
		}
	}
	for _, pid := range defeatedSPTBossIds {
		if idx, ok := petIDToSPTIndex[pid]; ok && idx < 200 {
			out[idx] = 1
		}
	}
	return out
}
