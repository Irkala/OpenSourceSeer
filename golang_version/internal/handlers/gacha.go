package handlers

import (
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/seer-game/golang-version/internal/core/logger"
	"github.com/seer-game/golang-version/internal/core/userdb"
	"github.com/seer-game/golang-version/internal/server/gameserver"
)

const (
	gachaTicketID = 400501 // 神奇扭蛋牌
	gachaConfig   = "gacha_rewards.json"
)

// GachaReward 扭蛋机奖励项
type GachaReward struct {
	ItemID  int    `json:"itemID"`
	Weight  int    `json:"weight"`  // 权重，越大概率越高
	Name    string `json:"name"`    // 中文名称（可选，用于 GM 显示）
	IsGold  bool   `json:"isGold"`  // 是否金豆商城道具（金豆道具概率应低于赛尔豆）
}

var (
	gachaRewards   []GachaReward
	gachaRewardsMu sync.RWMutex
	gachaTotalW    int
)

// itemSetsXML 用于解析 item_sets.xml（扭蛋“随机套装一套”的来源）
type itemSetsXML struct {
	Sets []struct {
		Name  string `xml:"Name,attr"`
		Items []struct {
			ID int `xml:"ID,attr"`
		} `xml:"ItemRef"`
	} `xml:"Set"`
}

// gachaSetItemIDs 按套装存储装备 ID 列表（每个元素是一整套）
var gachaSetItemIDs [][]int

// gachaEssenceIDs 扭蛋中“随机一个精元”的候选道具 ID
var gachaEssenceIDs []int

func init() {
	loadDefaultGachaRewards()
	LoadGachaRewards() // 尝试加载 gacha_rewards.json，若存在则覆盖默认
	loadGachaItemSets()
	initGachaEssences()
}

// loadDefaultGachaRewards 加载默认奖励：
// - 稀有奖励：上古炎兽精元 / 始祖灵兽精元 / 宝贝鲤精元（权重 1）
// - 普通奖励：体力/活力药剂、胶囊等常见精灵道具（权重 5）
func loadDefaultGachaRewards() {
	// 稀有奖励（权重 1）
	rareItems := []GachaReward{
		{490001, 1, "上古炎兽精元", false},
		{490002, 1, "始祖灵兽精元", false},
		{490003, 1, "宝贝鲤精元", false},
	}

	// 普通奖励（权重 5），对齐精灵道具商店常见道具
	commonItems := []GachaReward{
		// 精灵胶囊
		{300001, 5, "普通精灵胶囊", false},
		{300002, 5, "中级精灵胶囊", false},
		{300003, 5, "高级精灵胶囊", false},
		{300004, 5, "超级精灵胶囊", false},
		// 体力药剂
		{300011, 5, "初级体力药剂", false},
		{300012, 5, "中级体力药剂", false},
		{300013, 5, "高级体力药剂", false},
		{300014, 5, "超级体力药剂", false},
		{300015, 5, "特级体力药剂", false},
		// 活力药剂
		{300016, 5, "初级活力药剂", false},
		{300017, 5, "中级活力药剂", false},
		{300018, 5, "高级活力药剂", false},
		{300019, 5, "超级活力药剂", false},
		// 形态胶囊
		{300152, 5, "形态固定胶囊", false},
		{300153, 5, "形态还原胶囊", false},
		// 全能学习力注入剂
		{300651, 5, "全能学习力注入剂", false},
	}

	gachaRewards = append(rareItems, commonItems...)
	recalcGachaTotalWeight()
}

func recalcGachaTotalWeight() {
	total := 0
	for _, r := range gachaRewards {
		if r.Weight > 0 {
			total += r.Weight
		}
	}
	gachaTotalW = total
}

// loadGachaItemSets 读取 xml/item_sets.xml，准备扭蛋“随机一套套装”的物品 ID 池
func loadGachaItemSets() {
	// 运行目录可能是 golang_version 或 golang_version/bin，这里做两级兜底：
	// 1) ./xml/item_sets.xml
	// 2) ../xml/item_sets.xml
	var data []byte
	var err error
	tryPaths := []string{
		filepath.Join("xml", "item_sets.xml"),
		filepath.Join("..", "xml", "item_sets.xml"),
	}
	for _, p := range tryPaths {
		if dir, derr := os.Getwd(); derr == nil {
			p = filepath.Join(dir, p)
		}
		data, err = os.ReadFile(p)
		if err == nil {
			logger.Info("[扭蛋机] 从路径加载 item_sets.xml: %s", p)
			break
		}
	}
	if err != nil {
		logger.Warning(fmt.Sprintf("[扭蛋机] 读取 item_sets.xml 失败（已尝试相对路径 ./xml 和 ../xml）: %v", err))
		return
	}
	var cfg itemSetsXML
	if err := xml.Unmarshal(data, &cfg); err != nil {
		logger.Warning(fmt.Sprintf("[扭蛋机] 解析 item_sets.xml 失败: %v", err))
		return
	}
	var sets [][]int
	for _, s := range cfg.Sets {
		if len(s.Items) == 0 {
			continue
		}
		var ids []int
		for _, it := range s.Items {
			if it.ID > 0 {
				ids = append(ids, it.ID)
			}
		}
		if len(ids) > 0 {
			sets = append(sets, ids)
		}
	}
	gachaSetItemIDs = sets
	logger.Info("[扭蛋机] 已加载套装配置: item_sets.xml，套装数量=%d", len(gachaSetItemIDs))
}

// initGachaEssences 初始化“随机精元”道具 ID 列表
func initGachaEssences() {
	// 依据需求：从 items.xml 中按名称查对应 id（避免不同版本 id 不一致）。
	essenceNames := []string{
		"碧水精元",
		"托鲁克的精元",
		"阿尔达拉的精元",
		"赫尔托克的精元",
		"吉米丽娅的精元",
		"史密斯的精元",
		"始祖灵兽的精元",
		"桑诺特的精元",
		"亚梅丝的精元",
		"卡洛达斯精元",
		"蒙奇克的精元",
		"巴里修斯的精元",
		"加瑞尔的精元",
		"昼魔精元",
		"夜魔精元",
	}
	ids, missing := resolveItemIDsByName(essenceNames)
	gachaEssenceIDs = ids
	if len(missing) > 0 {
		logger.Warning(fmt.Sprintf("[扭蛋机] items.xml 未找到部分精元名称，将跳过: %v", missing))
	}
}

type itemsRootXML struct {
	Cats []struct {
		Items []struct {
			ID   int    `xml:"ID,attr"`
			Name string `xml:"Name,attr"`
		} `xml:"Item"`
	} `xml:"Cat"`
}

// resolveItemIDsByName 从 items.xml 中按道具名称解析 ID。
// 返回 (foundIDs, missingNames)。foundIDs 顺序与输入 names 一致（未找到的会被跳过）。
func resolveItemIDsByName(names []string) ([]int, []string) {
	if len(names) == 0 {
		return nil, nil
	}

	readItemsXML := func(path string) (*itemsRootXML, bool) {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, false
		}
		var root itemsRootXML
		if err := xml.Unmarshal(data, &root); err != nil {
			logger.Warning(fmt.Sprintf("[扭蛋机] 解析 items.xml 失败: %v", err))
			return nil, false
		}
		return &root, true
	}

	var root *itemsRootXML

	// 1) 以可执行文件目录为基准
	if exePath, err := os.Executable(); err == nil && exePath != "" {
		exeDir := filepath.Dir(exePath)
		candidates := []string{
			filepath.Join(exeDir, "..", "xml", "items.xml"),
			filepath.Join(exeDir, "..", "..", "golang_version", "xml", "items.xml"),
			filepath.Join(exeDir, "golang_version", "xml", "items.xml"),
		}
		for _, c := range candidates {
			if r, ok := readItemsXML(c); ok {
				root = r
				break
			}
		}
	}

	// 2) 以当前工作目录为基准
	if root == nil {
		candidates := []string{
			filepath.Join("golang_version", "xml", "items.xml"),
			filepath.Join("xml", "items.xml"),
			filepath.Join("..", "golang_version", "xml", "items.xml"),
		}
		for _, c := range candidates {
			if r, ok := readItemsXML(c); ok {
				root = r
				break
			}
		}
	}

	if root == nil {
		// 找不到 items.xml 时返回全缺失
		missing := make([]string, 0, len(names))
		missing = append(missing, names...)
		return nil, missing
	}

	// 建 name -> id 映射（如同名取第一个）
	nameToID := make(map[string]int, 4096)
	for _, cat := range root.Cats {
		for _, it := range cat.Items {
			if it.ID <= 0 || it.Name == "" {
				continue
			}
			if _, exists := nameToID[it.Name]; !exists {
				nameToID[it.Name] = it.ID
			}
		}
	}

	ids := make([]int, 0, len(names))
	missing := make([]string, 0)
	for _, n := range names {
		if id, ok := nameToID[n]; ok && id > 0 {
			ids = append(ids, id)
		} else {
			missing = append(missing, n)
		}
	}
	return ids, missing
}

// LoadGachaRewards 从 JSON 文件加载奖励配置
func LoadGachaRewards() {
	path := gachaConfig
	if dir, err := os.Getwd(); err == nil {
		path = filepath.Join(dir, gachaConfig)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		logger.Info(fmt.Sprintf("[扭蛋机] 未找到 %s，使用默认奖励", path))
		return
	}
	var list []GachaReward
	if err := json.Unmarshal(data, &list); err != nil {
		logger.Warning(fmt.Sprintf("[扭蛋机] 解析配置失败: %v", err))
		return
	}
	if len(list) == 0 {
		logger.Info("[扭蛋机] 配置为空，保留默认奖励")
		return
	}
	gachaRewardsMu.Lock()
	gachaRewards = list
	recalcGachaTotalWeight()
	gachaRewardsMu.Unlock()
	logger.Info(fmt.Sprintf("[扭蛋机] 已加载 %d 项奖励", len(gachaRewards)))
}

// SaveGachaRewards 保存奖励配置到 JSON
func SaveGachaRewards() error {
	gachaRewardsMu.RLock()
	list := make([]GachaReward, len(gachaRewards))
	copy(list, gachaRewards)
	gachaRewardsMu.RUnlock()

	path := gachaConfig
	if dir, err := os.Getwd(); err == nil {
		path = filepath.Join(dir, gachaConfig)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// GetGachaRewards 获取当前奖励列表（供 GM 使用）
func GetGachaRewards() []GachaReward {
	gachaRewardsMu.RLock()
	defer gachaRewardsMu.RUnlock()
	list := make([]GachaReward, len(gachaRewards))
	copy(list, gachaRewards)
	return list
}

// SetGachaRewards 设置奖励列表（供 GM 使用）
func SetGachaRewards(list []GachaReward) {
	gachaRewardsMu.Lock()
	gachaRewards = list
	recalcGachaTotalWeight()
	gachaRewardsMu.Unlock()
}

// AddGachaReward 添加奖励
func AddGachaReward(r GachaReward) {
	if r.Weight <= 0 {
		r.Weight = 1
	}
	if r.Name == "" {
		r.Name = GetItemName(r.ItemID)
	}
	gachaRewardsMu.Lock()
	gachaRewards = append(gachaRewards, r)
	recalcGachaTotalWeight()
	gachaRewardsMu.Unlock()
}

// RemoveGachaReward 按索引删除奖励
func RemoveGachaReward(index int) bool {
	gachaRewardsMu.Lock()
	defer gachaRewardsMu.Unlock()
	if index < 0 || index >= len(gachaRewards) {
		return false
	}
	gachaRewards = append(gachaRewards[:index], gachaRewards[index+1:]...)
	recalcGachaTotalWeight()
	return true
}

// UpdateGachaReward 按索引修改奖励
func UpdateGachaReward(index int, r GachaReward) bool {
	gachaRewardsMu.Lock()
	defer gachaRewardsMu.Unlock()
	if index < 0 || index >= len(gachaRewards) {
		return false
	}
	if r.Weight <= 0 {
		r.Weight = 1
	}
	if r.Name == "" {
		r.Name = GetItemName(r.ItemID)
	}
	gachaRewards[index] = r
	recalcGachaTotalWeight()
	return true
}

// RollGacha 抽一次奖，返回 (itemID, count)。若奖池为空返回 (0, 0)
func RollGacha() (itemID int, count int) {
	// 新版扭蛋不再使用 gachaRewards 池，而是按照固定概率表发放奖励。
	// 这里保留签名仅为兼容旧 GM 接口，实际逻辑由 applyGachaOnce 实现，
	// RollGacha 本身仅返回“无道具”（由 handleGacha 调用 applyGachaOnce）。
	return 0, 0
}

// GiveGachaReward 给用户发放扭蛋奖励并返回用于 2601 提示的 body
func GiveGachaReward(user *userdb.GameData, itemID int, count int) []byte {
	if itemID <= 0 || count <= 0 {
		return nil
	}
	itemKey := strconv.Itoa(itemID)
	if it, has := user.Items[itemKey]; has {
		it.Count += count
		user.Items[itemKey] = it
	} else {
		user.Items[itemKey] = userdb.Item{Count: count, ExpireTime: 0x057E40}
	}
	addClothIfNeeded(user, itemID)
	// 2601 响应格式: cash(4) + itemID(4) + itemNum(4) + itemLevel(4)
	body := make([]byte, 16)
	coins := user.Coins
	if coins < 0 {
		coins = 0
	}
	binary.BigEndian.PutUint32(body[0:4], uint32(coins))
	binary.BigEndian.PutUint32(body[4:8], uint32(itemID))
	binary.BigEndian.PutUint32(body[8:12], uint32(count))
	binary.BigEndian.PutUint32(body[12:16], 0)
	return body
}

// applyGachaOnce 按新的概率表执行一次扭蛋逻辑，直接修改 user：
// - 赛尔豆 / 金豆 / 经验池增加
// - 发放指定道具（含“随机套装一套”会发整套多个部件）
// 返回值用于 3201 包体提示：统一把“非道具奖励”映射为虚拟 ItemID：
//   赛尔豆 -> ItemID=1，金豆 -> ItemID=5，可分配经验 -> ItemID=3（积累经验）。
//
// 注意：用户需求给的是百分比，但合计可能不为 100。这里使用“权重”抽取：
// 实际概率 = weight / sum(weights)。
func applyGachaOnce(user *userdb.GameData) (lastItemID int, lastCount int) {
	type entry struct {
		name   string
		weight int
		apply  func()
	}

	addCoins := func(delta int) {
		user.Coins += delta
		if user.Coins < 0 {
			user.Coins = 0
		}
	}
	addGold := func(delta int) {
		user.Gold += delta
		if user.Gold < 0 {
			user.Gold = 0
		}
	}
	addExpPool := func(delta int) {
		user.ExpPool += delta
		if user.ExpPool < 0 {
			user.ExpPool = 0
		}
	}

	grantItem := func(itemID, count int) {
		if itemID <= 0 || count <= 0 {
			return
		}
		body := GiveGachaReward(user, itemID, count)
		if body != nil {
			lastItemID = itemID
			lastCount = count
		}
	}

	entries := []entry{
		{"赛尔豆5000", 5, func() { addCoins(5000); lastItemID, lastCount = 1, 5000 }},
		{"赛尔豆10000", 5, func() { addCoins(10000); lastItemID, lastCount = 1, 10000 }},
		{"金豆5", 5, func() { addGold(5); lastItemID, lastCount = 5, 5 }},
		{"金豆10", 2, func() { addGold(10); lastItemID, lastCount = 5, 10 }},
		{"随机精元", 1, func() {
			if len(gachaEssenceIDs) > 0 {
				id := gachaEssenceIDs[rand.Intn(len(gachaEssenceIDs))]
				grantItem(id, 1)
			}
		}},
		{"可分配经验10000", 10, func() { addExpPool(10000); lastItemID, lastCount = 3, 10000 }},
		{"可分配经验20000", 5, func() { addExpPool(20000); lastItemID, lastCount = 3, 20000 }},
		{"可分配经验50000", 2, func() { addExpPool(50000); lastItemID, lastCount = 3, 50000 }},
		{"可分配经验100000", 1, func() { addExpPool(100000); lastItemID, lastCount = 3, 100000 }},
		{"套装随机一套", 3, func() {
			if len(gachaSetItemIDs) > 0 {
				set := gachaSetItemIDs[rand.Intn(len(gachaSetItemIDs))]
				for _, id := range set {
					grantItem(id, 1)
				}
			}
		}},
		{"无敌精灵胶囊x1", 5, func() { grantItem(300006, 1) }},
		{"时空精灵胶囊x1", 5, func() { grantItem(300009, 1) }},
		{"特性附加芯片x1", 3, func() { grantItem(300053, 1) }},
		{"特性重组药剂x1", 7, func() { grantItem(300054, 1) }},
		{"全能学习力注入药剂x1", 5, func() { grantItem(300651, 1) }},
		{"降级秘药x1", 5, func() { grantItem(300026, 1) }},
		{"性格重塑胶囊x1", 5, func() { grantItem(300025, 1) }},
		{"神奇扭蛋牌1x1", 5, func() { grantItem(gachaTicketID, 1) }},
		{"神奇扭蛋牌2x1", 5, func() { grantItem(gachaTicketID, 1) }},
		{"神奇扭蛋牌3x1", 5, func() { grantItem(gachaTicketID, 1) }},
		{"精灵还原药剂x1", 5, func() { grantItem(300024, 1) }},
		{"超级精灵胶囊x5", 5, func() { grantItem(300004, 5) }},
	}

	totalW := 0
	for _, e := range entries {
		if e.weight > 0 {
			totalW += e.weight
		}
	}
	if totalW <= 0 {
		return 0, 0
	}
	roll := rand.Intn(totalW)
	acc := 0
	for _, e := range entries {
		if e.weight <= 0 {
			continue
		}
		acc += e.weight
		if roll < acc {
			e.apply()
			return lastItemID, lastCount
		}
	}

	return lastItemID, lastCount
}

// handleGacha CMD 3201 精灵扭蛋机 / 9757 梦幻扭蛋机：
// - 消耗对应数量的扭蛋牌
// - 根据抽奖次数（1 / 5 / 10 连抽）多次 RollGacha 并发放奖励
// - 回包当前 CMD（用于扭蛋机界面刷新）以及 2601（复用“获得道具/精灵”提示窗口），
//   其中两个包的 body 都使用「最后一次」抽中的奖励。
func handleGacha(ctx *gameserver.HandlerContext) {
	cmd := int32(3201)
	if ctx.CmdID == 9757 {
		cmd = 9757
	}
	ticketID := gachaTicketID // 3201 用神奇扭蛋牌 400501
	if ctx.CmdID == 9757 {
		ticketID = 400505 // 梦幻扭蛋牌
	}

	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)

	// 读取抽奖次数：客户端在包体前 4 字节写入「按钮编号」：
	// 1=单抽，2=5连抽，3=10连抽。
	// 这里在日志中打印原始包体，便于分析与调试。
	logger.Info(fmt.Sprintf("[扭蛋机] 原始包体: len=%d hex=% X", len(ctx.Body), ctx.Body))

	times := 1
	if len(ctx.Body) >= 4 {
		btn := int(binary.BigEndian.Uint32(ctx.Body[0:4]))
		switch btn {
		case 1:
			times = 1
		case 2:
			times = 5
		case 3:
			times = 10
		default:
			times = 1
		}
	}
	if times <= 0 {
		times = 1
	}

	ticketKey := strconv.Itoa(ticketID)
	it, has := user.Items[ticketKey]
	if !has || it.Count < times {
		logger.Info(fmt.Sprintf("[扭蛋机] CMD=%d UID=%d 扭蛋牌不足(need=%d, have=%d)", cmd, ctx.UserID, times, it.Count))
		ctx.GameServer.SendResponse(ctx.ClientData, cmd, ctx.UserID, ctx.SeqID, []byte{})
		return
	}

	// 扣除对应数量的扭蛋牌
	it.Count -= times
	if it.Count <= 0 {
		delete(user.Items, ticketKey)
	} else {
		user.Items[ticketKey] = it
	}

	// 连抽：循环 applyGachaOnce，多次发放奖励，并在本地累加成 itemID -> count 的映射，
	// 仅统计“真正发放了道具”的奖励，用于构造 EggMechineGame 期望的 3201 包体格式：
	// cash(4) + petID(4) + catchTime(4) + itemCount(4) + [itemID(4) + itemNum(4)] * itemCount
	rewards := make(map[int]int)
	var lastItemID, lastCount int
	for i := 0; i < times; i++ {
		itemID, count := applyGachaOnce(user)
		if itemID > 0 && count > 0 {
			rewards[itemID] += count
			lastItemID, lastCount = itemID, count
		}
	}

	// 若本次连抽完全没有抽出有效奖励（极端情况），返回空包避免前端卡死
	if len(rewards) == 0 {
		ctx.GameServer.SendResponse(ctx.ClientData, cmd, ctx.UserID, ctx.SeqID, []byte{})
		return
	}

	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}

	// 根据 EggMechineGame 的解析逻辑构造 3201 包体：
	// coins(4) + petID(4) + catchTime(4) + itemCount(4) + [itemID(4) + itemNum(4)] * itemCount
	coins := user.Coins
	if coins < 0 {
		coins = 0
	}
	itemCount := len(rewards)
	body := make([]byte, 0, 16+itemCount*8)
	tmp := make([]byte, 4)

	// coins
	binary.BigEndian.PutUint32(tmp, uint32(coins))
	body = append(body, tmp...)
	// petID（当前扭蛋机只发道具，不直接发精灵，因此置 0，避免触发“获得精灵”窗口）
	binary.BigEndian.PutUint32(tmp, 0)
	body = append(body, tmp...)
	// catchTime（保留字段，置 0）
	binary.BigEndian.PutUint32(tmp, 0)
	body = append(body, tmp...)
	// itemCount
	binary.BigEndian.PutUint32(tmp, uint32(itemCount))
	body = append(body, tmp...)

	// items
	for id, c := range rewards {
		binary.BigEndian.PutUint32(tmp, uint32(id))
		body = append(body, tmp...)
		binary.BigEndian.PutUint32(tmp, uint32(c))
		body = append(body, tmp...)
	}

	// 仅回应当前 CMD（扭蛋机主流程），EggMechineGame 会根据 3201 包体自行弹出
	// “你获得了X个XXX”的道具提示；只有真正下发精灵时才需要在 body 中填充 petID。
	ctx.GameServer.SendResponse(ctx.ClientData, cmd, ctx.UserID, ctx.SeqID, body)
	logger.Info(fmt.Sprintf("[扭蛋机] CMD=%d UID=%d 连抽=%d 次，总奖励种类=%d，最后一次获得: itemID=%d count=%d 名称=%s", cmd, ctx.UserID, times, itemCount, lastItemID, lastCount, GetItemName(lastItemID)))
}
