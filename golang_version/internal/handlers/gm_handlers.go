package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/seer-game/golang-version/internal/core/logger"
	"github.com/seer-game/golang-version/internal/core/userdb"
	gamepets "github.com/seer-game/golang-version/internal/game/pets"
	gameskills "github.com/seer-game/golang-version/internal/game/skills"
	"github.com/seer-game/golang-version/internal/server/gameserver"
)

// RegisterGMHandlers 注册GM管理接口
func RegisterGMHandlers(mux *http.ServeMux, gs *gameserver.GameServer) {
	// 获取所有用户列表（支持 keyword 查找账号）
	mux.HandleFunc("/gm/users", func(w http.ResponseWriter, r *http.Request) {
		handleGMGetUsers(w, r, gs)
	})

	// 获取单个用户详情
	mux.HandleFunc("/gm/user/", func(w http.ResponseWriter, r *http.Request) {
		handleGMGetUser(w, r, gs)
	})

	// 更新用户信息
	mux.HandleFunc("/gm/user/update", func(w http.ResponseWriter, r *http.Request) {
		handleGMUpdateUser(w, r, gs)
	})

	// 一键开通超能 NoNo
	mux.HandleFunc("/gm/nono/activate", func(w http.ResponseWriter, r *http.Request) {
		handleGMActivateSuperNono(w, r, gs)
	})

	// 删除角色账号
	mux.HandleFunc("/gm/user/delete", func(w http.ResponseWriter, r *http.Request) {
		handleGMDeleteUser(w, r, gs)
	})

	// 新增账号
	mux.HandleFunc("/gm/user/create", func(w http.ResponseWriter, r *http.Request) {
		handleGMCreateUser(w, r, gs)
	})

	// 添加黑名单
	mux.HandleFunc("/gm/blacklist/add", func(w http.ResponseWriter, r *http.Request) {
		handleGMAddBlacklist(w, r, gs)
	})

	// 移除黑名单
	mux.HandleFunc("/gm/blacklist/remove", func(w http.ResponseWriter, r *http.Request) {
		handleGMRemoveBlacklist(w, r, gs)
	})

	// 添加物品
	mux.HandleFunc("/gm/item/add", func(w http.ResponseWriter, r *http.Request) {
		handleGMAddItem(w, r, gs)
	})

	// 删除物品
	mux.HandleFunc("/gm/item/delete", func(w http.ResponseWriter, r *http.Request) {
		handleGMDeleteItem(w, r, gs)
	})

	// 道具列表（ID + 中文名），供 GM 前端选择使用
	mux.HandleFunc("/gm/items", func(w http.ResponseWriter, r *http.Request) {
		handleGMItemList(w, r, gs)
	})

	// 添加精灵
	mux.HandleFunc("/gm/pet/add", func(w http.ResponseWriter, r *http.Request) {
		handleGMAddPet(w, r, gs)
	})

	// 精灵列表（ID + 中文名），供 GM 前端选择使用
	mux.HandleFunc("/gm/pets", func(w http.ResponseWriter, r *http.Request) {
		handleGMPetList(w, r, gs)
	})

	// 重新从 pets.xml 加载精灵名称映射（无需重启进程）
	mux.HandleFunc("/gm/pets/reload", func(w http.ResponseWriter, r *http.Request) {
		handleGMPetReload(w, r, gs)
	})

	// 修改精灵技能
	mux.HandleFunc("/gm/pet/skills", func(w http.ResponseWriter, r *http.Request) {
		handleGMUpdatePetSkills(w, r, gs)
	})

	// 修改精灵特性
	mux.HandleFunc("/gm/pet/trait", func(w http.ResponseWriter, r *http.Request) {
		handleGMUpdatePetTrait(w, r, gs)
	})

	// 修改精灵性格
	mux.HandleFunc("/gm/pet/nature", func(w http.ResponseWriter, r *http.Request) {
		handleGMUpdatePetNature(w, r, gs)
	})

	// 修改精灵学习力（单项0-255，总和≤510）
	mux.HandleFunc("/gm/pet/ev", func(w http.ResponseWriter, r *http.Request) {
		handleGMUpdatePetEV(w, r, gs)
	})

	// 删除用户精灵
	mux.HandleFunc("/gm/pet/delete", func(w http.ResponseWriter, r *http.Request) {
		handleGMDeletePet(w, r, gs)
	})
	// 设置精灵异色（背包或仓库）
	mux.HandleFunc("/gm/pet/setShiny", func(w http.ResponseWriter, r *http.Request) {
		handleGMPetSetShiny(w, r, gs)
	})

	// 经验分配器：发放经验到经验池
	mux.HandleFunc("/gm/exp/add", func(w http.ResponseWriter, r *http.Request) {
		handleGMExpPoolAdd(w, r, gs)
	})
	// 经验分配器：设置当前经验池数值
	mux.HandleFunc("/gm/exp/set", func(w http.ResponseWriter, r *http.Request) {
		handleGMExpPoolSet(w, r, gs)
	})

	// 扭蛋机：列表（带中文名）、修改、添加、删除、保存
	mux.HandleFunc("/gm/gacha/list", func(w http.ResponseWriter, r *http.Request) {
		handleGMGachaList(w, r, gs)
	})
	mux.HandleFunc("/gm/gacha/update", func(w http.ResponseWriter, r *http.Request) {
		handleGMGachaUpdate(w, r, gs)
	})
	mux.HandleFunc("/gm/gacha/add", func(w http.ResponseWriter, r *http.Request) {
		handleGMGachaAdd(w, r, gs)
	})
	mux.HandleFunc("/gm/gacha/remove", func(w http.ResponseWriter, r *http.Request) {
		handleGMGachaRemove(w, r, gs)
	})
	mux.HandleFunc("/gm/gacha/save", func(w http.ResponseWriter, r *http.Request) {
		handleGMGachaSave(w, r, gs)
	})

	// 融合 / 元神珠配置（仅内存生效，用于调试与快速配置）
	mux.HandleFunc("/gm/fusion/config", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			handleGMGetFusionConfig(w, r, gs)
		case http.MethodPost:
			handleGMUpdateFusionConfig(w, r, gs)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	// 称号：列表（ID + 中文名）、发放
	mux.HandleFunc("/gm/titles", func(w http.ResponseWriter, r *http.Request) {
		handleGMTitleList(w, r, gs)
	})
	mux.HandleFunc("/gm/title/grant", func(w http.ResponseWriter, r *http.Request) {
		handleGMTitleGrant(w, r, gs)
	})
}

// FusionConfigGM 用于 GM 面板展示和修改的融合配置
type FusionConfigGM struct {
	TransformSeconds int64           `json:"transformSeconds"`
	Formulas         []fusionFormula `json:"formulas"`
}

// GMTitleInfo 称号列表项，供 GM 前端下拉显示 ID + 中文名
type GMTitleInfo struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

var (
	gmTitleList     []GMTitleInfo
	gmTitleListOnce sync.Once
)

// loadGMTitleList 从 data/titles.json 加载称号列表（与客户端 AchieveXMLInfo 的 SpeNameBonus→title 一致），
// 失败则使用内置简短列表。可用 scripts/extract_titles.py 从客户端成就 XML 重新生成 titles.json。
func loadGMTitleList() {
	gmTitleListOnce.Do(func() {
		var candidates []string
		if exePath, err := os.Executable(); err == nil {
			exeDir := filepath.Dir(exePath)
			candidates = append(candidates,
				filepath.Join(exeDir, "data", "titles.json"),
				filepath.Join(exeDir, "..", "data", "titles.json"),
				filepath.Join(exeDir, "..", "..", "data", "titles.json"),
				filepath.Join(exeDir, "golang_version", "data", "titles.json"),
			)
		}
		candidates = append(candidates, "data/titles.json", "golang_version/data/titles.json")
		var data []byte
		var err error
		for _, p := range candidates {
			data, err = os.ReadFile(p)
			if err == nil {
				break
			}
		}
		if err != nil || len(data) == 0 {
			// 与客户端一致的少量 fallback（ID 与游戏内显示名称对应）
			gmTitleList = []GMTitleInfo{
				{0, "无称号"}, {1, "精灵爱好者"}, {2, "精灵收藏家"}, {3, "孤傲挑战者"}, {4, "龙族盟友"},
				{9, "新人"}, {10, "勇者"}, {14, "狂傲战栗者"}, {16, "龙族卫士"}, {19, "青龙终结者"},
			}
			return
		}
		var list []GMTitleInfo
		if err := json.Unmarshal(data, &list); err != nil {
			gmTitleList = []GMTitleInfo{{0, "无称号"}, {1, "精灵爱好者"}}
			return
		}
		gmTitleList = list
	})
}

// handleGMTitleList 返回称号列表（ID + 中文名），供 GM 发放称号时下拉选择
func handleGMTitleList(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	loadGMTitleList()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"data":    gmTitleList,
	})
}

// handleGMTitleGrant 给指定用户发放称号（加入 TitleIDs，在线则推送 70005）
func handleGMTitleGrant(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != http.MethodPost {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "只支持 POST 请求"})
		return
	}
	var req struct {
		UserID  int64 `json:"userId"`
		TitleID int   `json:"titleId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "请求参数错误"})
		return
	}
	if req.TitleID <= 0 {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "称号 ID 需大于 0（0 为无称号）"})
		return
	}
	user := gs.UserDB.FindByUserID(req.UserID)
	if user == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "用户不存在"})
		return
	}
	GrantTitle(gs, req.UserID, req.TitleID)
	logger.Info(fmt.Sprintf("[GM] 发放称号: UserID=%d TitleID=%d", req.UserID, req.TitleID))
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "发放成功，玩家在线时将收到获得称号提示",
	})
}

// handleGMGetFusionConfig 返回当前进程内的融合公式和赋形时间
func handleGMGetFusionConfig(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	cfg := FusionConfigGM{
		TransformSeconds: soulBeadTransformSec,
		Formulas:         fusionFormulas,
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"data":    cfg,
	})
}

// handleGMUpdateFusionConfig 允许通过 GM 面板在内存中调整融合公式与赋形时间（不持久化）
func handleGMUpdateFusionConfig(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	var payload FusionConfigGM
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "解析 JSON 失败",
		})
		return
	}

	if payload.TransformSeconds <= 0 {
		payload.TransformSeconds = 60
	}
	soulBeadTransformSec = payload.TransformSeconds
	if payload.Formulas != nil && len(payload.Formulas) > 0 {
		fusionFormulas = payload.Formulas
	}

	logger.Info(fmt.Sprintf("[GM] 更新融合配置：transformSeconds=%d, formulas=%d 条", soulBeadTransformSec, len(fusionFormulas)))
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
	})
}

// handleGMGetUsers 获取所有用户列表；支持 keyword 查找账号（按 userId、昵称、邮箱模糊匹配）
func handleGMGetUsers(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	keyword := strings.TrimSpace(r.URL.Query().Get("keyword"))
	logger.Info(fmt.Sprintf("[GM] 获取用户列表 keyword=%q", keyword))

	type UserInfo struct {
		UserID      int64  `json:"userId"`
		Email       string `json:"email"`
		Nickname    string `json:"nickname"`
		Coins       int    `json:"coins"`
		Gold        int    `json:"gold"`
		PetCount    int    `json:"petCount"`
		LastOnline  int64  `json:"lastOnline"`
		RoleCreated bool   `json:"roleCreated"`
	}

	var userList []UserInfo
	allGameData := gs.UserDB.GetAllGameData()

	for userID, gameData := range allGameData {
		user := gs.UserDB.FindByUserID(userID)
		if user == nil {
			continue
		}
		// 查找账号：按 userId、昵称、邮箱模糊匹配
		if keyword != "" {
			idStr := fmt.Sprintf("%d", userID)
			if !strings.Contains(idStr, keyword) &&
				!strings.Contains(strings.ToLower(user.Nickname), strings.ToLower(keyword)) &&
				!strings.Contains(strings.ToLower(user.Email), strings.ToLower(keyword)) {
				continue
			}
		}
		info := UserInfo{
			UserID:      user.UserID,
			Email:       user.Email,
			Nickname:    user.Nickname,
			RoleCreated: user.RoleCreated,
			Coins:       gameData.Coins,
			Gold:        gameData.Gold,
			PetCount:    len(gameData.Pets),
			LastOnline:  gameData.LastOnline,
		}
		userList = append(userList, info)
	}

	logger.Info(fmt.Sprintf("[GM] 返回 %d 个用户信息", len(userList)))
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"data":    userList,
	})
}

// handleGMGetUser 获取单个用户详情
func handleGMGetUser(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	
	userIDStr := r.URL.Query().Get("userId")
	userID, err := strconv.ParseInt(userIDStr, 10, 64)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "无效的用户ID",
		})
		return
	}
	
	user := gs.UserDB.FindByUserID(userID)
	if user == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "用户不存在",
		})
		return
	}
	
	gameData := gs.UserDB.GetOrCreateGameData(userID)
	
	// 为精灵添加中文名称
	for i := range gameData.Pets {
		gameData.Pets[i].Name = GetPetName(gameData.Pets[i].ID)
	}
	
	// 为仓库精灵添加中文名称
	for i := range gameData.StoragePets {
		gameData.StoragePets[i].Name = GetPetName(gameData.StoragePets[i].ID)
	}
	
	// 为道具添加中文名称
	itemsWithNames := make(map[string]interface{})
	for idStr, item := range gameData.Items {
		if itemID, err := strconv.Atoi(idStr); err == nil {
			itemData := map[string]interface{}{
				"count":      item.Count,
				"expireTime": item.ExpireTime,
				"name":       GetItemName(itemID),
			}
			itemsWithNames[idStr] = itemData
		} else {
			itemsWithNames[idStr] = item
		}
	}

	// 每个精灵的 4 个技能槽中文名（key 为 catchTime 字符串，供前端显示）
	petSkillNames := make(map[string][]string)
	for _, p := range gameData.Pets {
		names := make([]string, 4)
		for j := 0; j < 4; j++ {
			sid := 0
			if j < len(p.Skills) {
				sid = p.Skills[j]
			}
			if sid > 0 {
				names[j] = GetSkillName(sid)
			}
		}
		petSkillNames[fmt.Sprintf("%d", p.CatchTime)] = names
	}
	for _, p := range gameData.StoragePets {
		names := make([]string, 4)
		for j := 0; j < 4; j++ {
			sid := 0
			if j < len(p.Skills) {
				sid = p.Skills[j]
			}
			if sid > 0 {
				names[j] = GetSkillName(sid)
			}
		}
		petSkillNames[fmt.Sprintf("%d", p.CatchTime)] = names
	}

	// 创建带名称的游戏数据副本（含黑名单供 GM 管理）
	gameDataWithNames := map[string]interface{}{
		"nick":    gameData.Nick,
		"coins":   gameData.Coins,
		"gold":    gameData.Gold,
		"energy":  gameData.Energy,
		"expPool":  gameData.ExpPool,
		"curTitle": gameData.CurTitle,
		"titleIds": gameData.TitleIDs,
		"pets":     gameData.Pets,
		"storagePets":   gameData.StoragePets,
		"items":         itemsWithNames,
		"blacklist":     gameData.Blacklist,
		"lastOnline":    gameData.LastOnline,
		"petSkillNames": petSkillNames,
		// NoNo / 超能等级信息（供 GM 面板展示与编辑）
		"nono": map[string]interface{}{
			"level":       gameData.Nono.SuperLevel,
			"exp":         gameData.Nono.Power,
			"energy":      gameData.Nono.SuperEnergy,
			"type":        gameData.Nono.SuperNono,
			"status":      gameData.Nono.State,
			"skillPoints": gameData.Nono.IQ,
			"vipLevel":    gameData.Nono.VipLevel,
			"vipEndTime":  gameData.Nono.VipEndTime,
		},
	}
	
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"user":     user,
		"gameData": gameDataWithNames,
	})
}

// handleGMUpdateUser 更新用户信息
func handleGMUpdateUser(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	
	if r.Method != "POST" {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "只支持POST请求",
		})
		return
	}
	
	var req struct {
		UserID   int64   `json:"userId"`
		Nickname string  `json:"nickname"`
		Password *string `json:"password"` // 可选，填写则更新密码
		Coins    *int    `json:"coins"`
		Gold     *int    `json:"gold"`
		Energy   *int    `json:"energy"`
		Nono     *struct {
			Level       *int `json:"level"`
			Energy      *int `json:"energy"`
			Exp         *int `json:"exp"`
			SkillPoints *int `json:"skillPoints"`
			Status      *int `json:"status"`
		} `json:"nono"`
	}
	
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "请求参数错误",
		})
		return
	}
	
	user := gs.UserDB.FindByUserID(req.UserID)
	if user == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "用户不存在",
		})
		return
	}
	
	gameData := gs.UserDB.GetOrCreateGameData(req.UserID)
	if gameData == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "游戏数据不存在",
		})
		return
	}
	
	// 更新昵称
	if req.Nickname != "" {
		user.Nickname = req.Nickname
		gameData.Nick = req.Nickname
	}
	
	// 更新密码（GM 可显示与修改）
	if req.Password != nil && *req.Password != "" {
		user.Password = *req.Password
	}
	
	// 更新金币
	if req.Coins != nil {
		gameData.Coins = *req.Coins
	}
	
	// 更新金豆
	if req.Gold != nil {
		gameData.Gold = *req.Gold
	}
	
	// 更新体力
	if req.Energy != nil {
		gameData.Energy = *req.Energy
	}

	// 更新 NoNo / 超能信息（若有传入）
	if req.Nono != nil {
		updated := false
		// 更新超能等级
		if req.Nono.Level != nil {
			level := *req.Nono.Level
			// 限制超能等级上限为 12
			if level > 12 {
				level = 12
			}
			if level < 0 {
				level = 0
			}
			gameData.Nono.SuperLevel = level
			// 根据超能等级同步形态与 VipLevel
			updateSuperNonoTypeByLevel(gameData)
			updated = true
		}
		// 更新能量值
		if req.Nono.Energy != nil {
			gameData.Nono.SuperEnergy = *req.Nono.Energy
			updated = true
		}
		// 更新NoNo经验（Power字段）
		if req.Nono.Exp != nil {
			gameData.Nono.Power = *req.Nono.Exp
			updated = true
		}
		// 更新技能点 (IQ)
		if req.Nono.SkillPoints != nil {
			gameData.Nono.IQ = *req.Nono.SkillPoints
			updated = true
		}
		// 更新状态
		if req.Nono.Status != nil {
			gameData.Nono.State = *req.Nono.Status
			updated = true
		}
		// 如果更新了NoNo信息，更新内存缓存
		if updated {
			gs.UpdateUserCache(req.UserID, gameData)
		}
	}
	
	gs.UserDB.SaveUser(user)
	gs.UserDB.SaveGameData(req.UserID, gameData)
	
	logger.Info(fmt.Sprintf("[GM] 更新用户信息: UserID=%d", req.UserID))
	
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "更新成功",
	})
}

// handleGMActivateSuperNono 一键开通超能 NoNo
func handleGMActivateSuperNono(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	
	if r.Method != "POST" {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "只支持POST请求",
		})
		return
	}
	
	var req struct {
		UserID int `json:"userId"`
		Days   int `json:"days"` // 开通天数
	}
	
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "请求参数错误",
		})
		return
	}
	
	if req.Days <= 0 {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "开通天数必须大于0",
		})
		return
	}
	
	user := gs.UserDB.FindByUserID(int64(req.UserID))
	if user == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "用户不存在",
		})
		return
	}
	
	gameData := gs.UserDB.GetOrCreateGameData(int64(req.UserID))
	if gameData == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "游戏数据不存在",
		})
		return
	}
	
	// 计算到期时间（当前时间 + 天数）
	currentTime := uint32(time.Now().Unix())
	endTime := currentTime + uint32(req.Days*24*60*60)
	
	// 设置超能 NoNo 相关字段
	gameData.Nono.SuperNono = 1
	gameData.Nono.VipEndTime = int64(endTime)
	if gameData.Nono.SuperLevel < 1 {
		gameData.Nono.SuperLevel = 1
	}
	if gameData.Nono.SuperStage < 1 {
		gameData.Nono.SuperStage = 1
	}
	
	// 根据超能等级更新形态
	updateSuperNonoTypeByLevel(gameData)
	
	// 更新内存缓存
	gs.UpdateUserCache(int64(req.UserID), gameData)
	
	// 保存到数据库
	gs.UserDB.SaveGameData(int64(req.UserID), gameData)
	
	logger.Info(fmt.Sprintf("[GM] 一键开通超能NoNo: UserID=%d Days=%d EndTime=%d", req.UserID, req.Days, endTime))
	
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("成功开通超能NoNo %d 天，到期时间: %s", req.Days, time.Unix(int64(endTime), 0).Format("2006-01-02 15:04:05")),
		"vipEndTime": endTime,
	})
}

// handleGMAddBlacklist 添加黑名单
func handleGMAddBlacklist(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	
	if r.Method != "POST" {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "只支持POST请求",
		})
		return
	}
	
	var req struct {
		UserID       int64 `json:"userId"`
		BlockedID    int64 `json:"blockedId"`
		BlockedNick  string `json:"blockedNick"`
	}
	
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "请求参数错误",
		})
		return
	}
	
	gameData := gs.UserDB.GetOrCreateGameData(req.UserID)
	
	// 检查是否已在黑名单
	for _, entry := range gameData.Blacklist {
		if entry.UserID == req.BlockedID {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "该用户已在黑名单中",
			})
			return
		}
	}
	
	// 添加到黑名单
	gameData.Blacklist = append(gameData.Blacklist, userdb.BlacklistEntry{
		UserID:  req.BlockedID,
		AddTime: time.Now().Unix(),
	})
	
	gs.UserDB.SaveGameData(req.UserID, gameData)
	
	logger.Info(fmt.Sprintf("[GM] 添加黑名单: UserID=%d BlockedID=%d", req.UserID, req.BlockedID))
	
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "添加成功",
	})
}

// handleGMRemoveBlacklist 移除黑名单
func handleGMRemoveBlacklist(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	
	if r.Method != "POST" {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "只支持POST请求",
		})
		return
	}
	
	var req struct {
		UserID    int64 `json:"userId"`
		BlockedID int64 `json:"blockedId"`
	}
	
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "请求参数错误",
		})
		return
	}
	
	gameData := gs.UserDB.GetOrCreateGameData(req.UserID)
	
	// 从黑名单移除
	newBlacklist := []userdb.BlacklistEntry{}
	
	for _, entry := range gameData.Blacklist {
		if entry.UserID != req.BlockedID {
			newBlacklist = append(newBlacklist, entry)
		}
	}
	
	gameData.Blacklist = newBlacklist
	gs.UserDB.SaveGameData(req.UserID, gameData)
	
	logger.Info(fmt.Sprintf("[GM] 移除黑名单: UserID=%d BlockedID=%d", req.UserID, req.BlockedID))
	
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "移除成功",
	})
}

// handleGMAddItem 添加物品
func handleGMAddItem(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	
	if r.Method != "POST" {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "只支持POST请求",
		})
		return
	}
	
	var req struct {
		UserID int64 `json:"userId"`
		ItemID int   `json:"itemId"`
		Count  int   `json:"count"`
	}
	
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "请求参数错误",
		})
		return
	}
	
	gameData := gs.UserDB.GetOrCreateGameData(req.UserID)
	
	itemKey := fmt.Sprintf("%d", req.ItemID)
	if item, ok := gameData.Items[itemKey]; ok {
		item.Count += req.Count
		gameData.Items[itemKey] = item
	} else {
		gameData.Items[itemKey] = userdb.Item{
			Count:      req.Count,
			ExpireTime: 360000,
		}
	}
	
	gs.UserDB.SaveGameData(req.UserID, gameData)
	
	logger.Info(fmt.Sprintf("[GM] 添加物品: UserID=%d ItemID=%d Count=%d", req.UserID, req.ItemID, req.Count))

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "添加成功",
	})
}

// handleGMDeleteItem 删除物品
func handleGMDeleteItem(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if r.Method != "POST" {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "只支持POST请求",
		})
		return
	}

	var req struct {
		UserID int64 `json:"userId"`
		ItemID int   `json:"itemId"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "请求参数错误",
		})
		return
	}

	gameData := gs.UserDB.GetOrCreateGameData(req.UserID)
	itemKey := fmt.Sprintf("%d", req.ItemID)

	if _, ok := gameData.Items[itemKey]; !ok {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "用户未拥有该物品",
		})
		return
	}

	delete(gameData.Items, itemKey)
	gs.UserDB.SaveGameData(req.UserID, gameData)

	logger.Info(fmt.Sprintf("[GM] 删除物品: UserID=%d ItemID=%d", req.UserID, req.ItemID))

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "删除成功",
	})
}

// handleGMAddPet 添加精灵
func handleGMAddPet(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	
	if r.Method != "POST" {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "只支持POST请求",
		})
		return
	}
	
	var req struct {
		UserID    int64 `json:"userId"`
		PetID     int   `json:"petId"`
		Level     int   `json:"level"`
		Shiny     *int  `json:"shiny"` // 0=普通 1=异色，不传默认0
		Trait     int   `json:"trait"`
		Nature    *int  `json:"nature"`
		// 学习力：单项 0-255，总和 ≤ 510
		EvHP    *int `json:"ev_hp"`
		EvAtk   *int `json:"ev_attack"`
		EvDef   *int `json:"ev_defence"`
		EvSpAtk *int `json:"ev_sa"`
		EvSpDef *int `json:"ev_sd"`
		EvSpd   *int `json:"ev_sp"`
	}
	
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "请求参数错误",
		})
		return
	}
	
	if req.Level <= 0 {
		req.Level = 1
	}
	if req.Level > 100 {
		req.Level = 100
	}
	
	// 学习力校验：单项 0-255，总和 ≤ 510
	evHP := 0
	if req.EvHP != nil {
		evHP = *req.EvHP
	}
	evAtk := 0
	if req.EvAtk != nil {
		evAtk = *req.EvAtk
	}
	evDef := 0
	if req.EvDef != nil {
		evDef = *req.EvDef
	}
	evSpAtk := 0
	if req.EvSpAtk != nil {
		evSpAtk = *req.EvSpAtk
	}
	evSpDef := 0
	if req.EvSpDef != nil {
		evSpDef = *req.EvSpDef
	}
	evSpd := 0
	if req.EvSpd != nil {
		evSpd = *req.EvSpd
	}
	if evHP < 0 || evHP > 255 || evAtk < 0 || evAtk > 255 || evDef < 0 || evDef > 255 ||
		evSpAtk < 0 || evSpAtk > 255 || evSpDef < 0 || evSpDef > 255 || evSpd < 0 || evSpd > 255 {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "学习力单项需在 0-255 之间",
		})
		return
	}
	if evHP+evAtk+evDef+evSpAtk+evSpDef+evSpd > 510 {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "学习力总和不能超过 510",
		})
		return
	}
	
	gameData := gs.UserDB.GetOrCreateGameData(req.UserID)
	
	// 添加精灵
	newPet := userdb.Pet{
		ID:        req.PetID,
		CatchTime: int(time.Now().Unix()),
		Level:     req.Level,
		DV:        31,
		Nature: func() int {
			if req.Nature != nil {
				return *req.Nature
			}
			return 0
		}(),
		Exp:       0,
		Name:      "",
		EVHP:      evHP,
		EVAttack:  evAtk,
		EVDefence: evDef,
		EVSpAtk:   evSpAtk,
		EVSpDef:   evSpDef,
		EVSpeed:   evSpd,
		Shiny:     func() int { if req.Shiny != nil && *req.Shiny != 0 { return 1 }; return 0 }(),
	}
	// GM 可选择是否为该精灵设置特性
	if req.Trait > 0 {
		newPet.Trait = req.Trait
	} else if req.Trait == -1 {
		userdb.AssignFusionTraitIfNeeded(&newPet)
	}

	// GM 发放精灵时也遵循“背包最多 6 只”的规则，超出则存入仓库
	if len(gameData.Pets) >= 6 {
		if gameData.StoragePets == nil {
			gameData.StoragePets = []userdb.Pet{}
		}
		gameData.StoragePets = append(gameData.StoragePets, newPet)
	} else {
		gameData.Pets = append(gameData.Pets, newPet)
	}
	gs.UserDB.SaveGameData(req.UserID, gameData)
	
	logger.Info(fmt.Sprintf("[GM] 添加精灵: UserID=%d PetID=%d Level=%d", req.UserID, req.PetID, req.Level))

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "添加成功",
	})
}

// handleGMPetList 返回精灵 ID 与中文名称列表，供 GM 前端下拉选择与搜索
func handleGMPetList(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"data":    GetAllPetsForGM(),
	})
}

// handleGMPetReload 重新从 pets.xml 加载精灵名称映射，供 GM 面板在不重启的情况下刷新精灵数据库
func handleGMPetReload(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// 建议使用 POST 避免被浏览器预取
	if r.Method != http.MethodPost {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "只支持 POST 请求",
		})
		return
	}

	added := ReloadPetNamesFromXML()
	logger.Info(fmt.Sprintf("[GM] 手动刷新精灵名称映射，本次新增 %d 条", added))

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("已从 pets.xml 追加 %d 个精灵名称", added),
	})
}

// handleGMItemList 返回道具 ID 与中文名称列表，供 GM 前端下拉选择与搜索
func handleGMItemList(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"data":    GetAllItemsForGM(),
	})
}

// handleGMUpdatePetSkills 修改精灵技能（4 个技能槽，传技能 ID 数组）
func handleGMUpdatePetSkills(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != "POST" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "只支持POST请求"})
		return
	}
	var req struct {
		UserID    int64   `json:"userId"`
		CatchTime int     `json:"catchTime"`
		Skills    []int   `json:"skills"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "请求参数错误"})
		return
	}
	gameData := gs.UserDB.GetOrCreateGameData(req.UserID)
	var picked *userdb.Pet
	for i := range gameData.Pets {
		if gameData.Pets[i].CatchTime == req.CatchTime {
			picked = &gameData.Pets[i]
			break
		}
	}
	if picked == nil {
		if gameData.StoragePets != nil {
			for i := range gameData.StoragePets {
				if gameData.StoragePets[i].CatchTime == req.CatchTime {
					picked = &gameData.StoragePets[i]
					break
				}
			}
		}
	}
	if picked == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "未找到该精灵"})
		return
	}
	skillMgr := gameskills.GetInstance()
	petMgr := gamepets.GetInstance()
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
	finalSkills := make([]int, 4)
	for i := 0; i < 4; i++ {
		sid := 0
		if i < len(req.Skills) {
			sid = req.Skills[i]
		}
		if sid > 1 && skillMgr.Exists(sid) {
			finalSkills[i] = sid
		} else {
			finalSkills[i] = currentSkills[i]
		}
	}
	picked.Skills = finalSkills
	gs.UserDB.SaveGameData(req.UserID, gameData)
	logger.Info(fmt.Sprintf("[GM] 修改精灵技能: UserID=%d CatchTime=%d PetID=%d skills=%v", req.UserID, req.CatchTime, picked.ID, picked.Skills))
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "修改成功"})
}

// handleGMUpdatePetTrait 修改精灵特性
func handleGMUpdatePetTrait(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != "POST" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "只支持POST请求"})
		return
	}
	var req struct {
		UserID    int64 `json:"userId"`
		CatchTime int   `json:"catchTime"`
		Trait     int   `json:"trait"` // 0=清除特性，-1=随机分配，>0=指定特性ID
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "请求参数错误"})
		return
	}
	gameData := gs.UserDB.GetOrCreateGameData(req.UserID)
	var picked *userdb.Pet
	for i := range gameData.Pets {
		if gameData.Pets[i].CatchTime == req.CatchTime {
			picked = &gameData.Pets[i]
			break
		}
	}
	if picked == nil && gameData.StoragePets != nil {
		for i := range gameData.StoragePets {
			if gameData.StoragePets[i].CatchTime == req.CatchTime {
				picked = &gameData.StoragePets[i]
				break
			}
		}
	}
	if picked == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "未找到该精灵"})
		return
	}

	if req.Trait > 0 {
		picked.Trait = req.Trait
	} else if req.Trait == -1 {
		picked.Trait = 0
		userdb.AssignFusionTraitIfNeeded(picked)
	} else {
		picked.Trait = 0
	}

	gs.UserDB.SaveGameData(req.UserID, gameData)
	logger.Info(fmt.Sprintf("[GM] 修改精灵特性: UserID=%d CatchTime=%d PetID=%d Trait=%d", req.UserID, req.CatchTime, picked.ID, picked.Trait))
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "修改成功"})
}

// handleGMUpdatePetNature 修改精灵性格
func handleGMUpdatePetNature(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != "POST" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "只支持POST请求"})
		return
	}
	var req struct {
		UserID    int64 `json:"userId"`
		CatchTime int   `json:"catchTime"`
		Nature    int   `json:"nature"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "请求参数错误"})
		return
	}
	gameData := gs.UserDB.GetOrCreateGameData(req.UserID)
	var picked *userdb.Pet
	for i := range gameData.Pets {
		if gameData.Pets[i].CatchTime == req.CatchTime {
			picked = &gameData.Pets[i]
			break
		}
	}
	if picked == nil && gameData.StoragePets != nil {
		for i := range gameData.StoragePets {
			if gameData.StoragePets[i].CatchTime == req.CatchTime {
				picked = &gameData.StoragePets[i]
				break
			}
		}
	}
	if picked == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "未找到该精灵"})
		return
	}

	picked.Nature = req.Nature
	gs.UserDB.SaveGameData(req.UserID, gameData)
	logger.Info(fmt.Sprintf("[GM] 修改精灵性格: UserID=%d CatchTime=%d PetID=%d Nature=%d", req.UserID, req.CatchTime, picked.ID, picked.Nature))
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "修改成功"})
}

// handleGMUpdatePetEV 修改精灵学习力（单项 0-255，总和 ≤ 510）
func handleGMUpdatePetEV(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != "POST" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "只支持POST请求"})
		return
	}
	var req struct {
		UserID     int64 `json:"userId"`
		CatchTime  int   `json:"catchTime"`
		FromStorage bool `json:"fromStorage"`
		EvHP       int   `json:"ev_hp"`
		EvAtk      int   `json:"ev_attack"`
		EvDef      int   `json:"ev_defence"`
		EvSpAtk    int   `json:"ev_sa"`
		EvSpDef    int   `json:"ev_sd"`
		EvSpd      int   `json:"ev_sp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "请求参数错误"})
		return
	}
	if req.EvHP < 0 || req.EvHP > 255 || req.EvAtk < 0 || req.EvAtk > 255 || req.EvDef < 0 || req.EvDef > 255 ||
		req.EvSpAtk < 0 || req.EvSpAtk > 255 || req.EvSpDef < 0 || req.EvSpDef > 255 || req.EvSpd < 0 || req.EvSpd > 255 {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "学习力单项需在 0-255 之间"})
		return
	}
	if req.EvHP+req.EvAtk+req.EvDef+req.EvSpAtk+req.EvSpDef+req.EvSpd > 510 {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "学习力总和不能超过 510"})
		return
	}
	gameData := gs.UserDB.GetOrCreateGameData(req.UserID)
	var picked *userdb.Pet
	if !req.FromStorage {
		for i := range gameData.Pets {
			if gameData.Pets[i].CatchTime == req.CatchTime {
				picked = &gameData.Pets[i]
				break
			}
		}
	}
	if picked == nil && gameData.StoragePets != nil {
		for i := range gameData.StoragePets {
			if gameData.StoragePets[i].CatchTime == req.CatchTime {
				picked = &gameData.StoragePets[i]
				break
			}
		}
	}
	if picked == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "未找到该精灵"})
		return
	}
	picked.EVHP = req.EvHP
	picked.EVAttack = req.EvAtk
	picked.EVDefence = req.EvDef
	picked.EVSpAtk = req.EvSpAtk
	picked.EVSpDef = req.EvSpDef
	picked.EVSpeed = req.EvSpd
	gs.UserDB.SaveGameData(req.UserID, gameData)
	logger.Info(fmt.Sprintf("[GM] 修改精灵学习力: UserID=%d CatchTime=%d PetID=%d", req.UserID, req.CatchTime, picked.ID))
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "修改成功"})
}

// handleGMDeletePet 删除用户精灵（背包或仓库，按 catchTime 定位）
func handleGMDeletePet(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != "POST" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "只支持POST请求"})
		return
	}
	var req struct {
		UserID    int64 `json:"userId"`
		CatchTime int   `json:"catchTime"`
		FromStorage bool `json:"fromStorage"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "请求参数错误"})
		return
	}
	gameData := gs.UserDB.GetOrCreateGameData(req.UserID)
	if req.FromStorage {
		newList := make([]userdb.Pet, 0, len(gameData.StoragePets))
		for _, p := range gameData.StoragePets {
			if p.CatchTime != req.CatchTime {
				newList = append(newList, p)
			}
		}
		gameData.StoragePets = newList
	} else {
		newList := make([]userdb.Pet, 0, len(gameData.Pets))
		for _, p := range gameData.Pets {
			if p.CatchTime != req.CatchTime {
				newList = append(newList, p)
			}
		}
		gameData.Pets = newList
	}
	gs.UserDB.SaveGameData(req.UserID, gameData)
	logger.Info(fmt.Sprintf("[GM] 删除精灵: UserID=%d CatchTime=%d fromStorage=%v", req.UserID, req.CatchTime, req.FromStorage))
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "删除成功"})
}

// handleGMPetSetShiny 设置精灵异色（背包或仓库，按 catchTime 定位）
// POST body: userId, catchTime, shiny (0=普通 1=异色), fromStorage
// 语义优化：
// - 实际位置以服务器当前数据为准：优先按 fromStorage 在对应列表查找，若未找到会回退到另一列表；
// - 若同一 catchTime 同时出现在背包和仓库（理论不应发生），则两边都会同步为同一 shiny；
// - 修改后立刻持久化并推送 2324（若涉及仓库），保证 GM 与游戏内仓库显示一致。
func handleGMPetSetShiny(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != "POST" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "只支持POST请求"})
		return
	}
	var req struct {
		UserID     int64 `json:"userId"`
		CatchTime  int   `json:"catchTime"`
		Shiny      int   `json:"shiny"`       // 0=普通，1=异色
		FromStorage bool `json:"fromStorage"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "请求参数错误"})
		return
	}
	if req.Shiny != 0 && req.Shiny != 1 {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "shiny 只能为 0 或 1"})
		return
	}
	// 直接操作当前在线用户数据，保证与游戏内存中的 user.StoragePets 完全一致
	gameData := gs.GetOrCreateUser(req.UserID)
	if gameData == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "用户不存在或未加载"})
		return
	}

	shinyVal := req.Shiny
	updatedBag := false
	updatedStorage := false
	petIDForLog := 0

	// 1) 优先在指定来源列表查找；找不到再回退到另一列表
	if req.FromStorage {
		for i := range gameData.StoragePets {
			if gameData.StoragePets[i].CatchTime == req.CatchTime {
				gameData.StoragePets[i].Shiny = shinyVal
				petIDForLog = gameData.StoragePets[i].ID
				updatedStorage = true
				break
			}
		}
		if !updatedStorage {
			for i := range gameData.Pets {
				if gameData.Pets[i].CatchTime == req.CatchTime {
					gameData.Pets[i].Shiny = shinyVal
					petIDForLog = gameData.Pets[i].ID
					updatedBag = true
					break
				}
			}
		}
	} else {
		for i := range gameData.Pets {
			if gameData.Pets[i].CatchTime == req.CatchTime {
				gameData.Pets[i].Shiny = shinyVal
				petIDForLog = gameData.Pets[i].ID
				updatedBag = true
				break
			}
		}
		if !updatedBag {
			for i := range gameData.StoragePets {
				if gameData.StoragePets[i].CatchTime == req.CatchTime {
					gameData.StoragePets[i].Shiny = shinyVal
					petIDForLog = gameData.StoragePets[i].ID
					updatedStorage = true
					break
				}
			}
		}
	}

	// 2) 若两边都存在同一 catchTime（极少数异常情况），强制保持一致
	if updatedBag || updatedStorage {
		for i := range gameData.Pets {
			if gameData.Pets[i].CatchTime == req.CatchTime {
				gameData.Pets[i].Shiny = shinyVal
			}
		}
		for i := range gameData.StoragePets {
			if gameData.StoragePets[i].CatchTime == req.CatchTime {
				gameData.StoragePets[i].Shiny = shinyVal
			}
		}
	} else {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "未在背包或仓库中找到该精灵"})
		return
	}

	// 3) 持久化并立即落盘，确保重登后不丢失；同时刷新仓库列表给在线客户端
	gs.UserDB.SaveGameData(req.UserID, gameData)
	gs.UserDB.SaveToFile()
	logger.Info(fmt.Sprintf("[GM] 设置精灵异色: UserID=%d PetID=%d CatchTime=%d Shiny=%d (bagUpdated=%v storageUpdated=%v)", req.UserID, petIDForLog, req.CatchTime, shinyVal, updatedBag, updatedStorage))

	// 若涉及仓库（无论是显式 fromStorage 还是 fallback 找到仓库），都推一次 2324，让仓库界面和 GM 同步
	if updatedStorage {
		pushStorageListToClient(gs, req.UserID)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"message":   "设置成功",
		"petId":     petIDForLog,
		"catchTime": req.CatchTime,
	})
}

// handleGMDeleteUser 删除角色账号
func handleGMDeleteUser(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != "POST" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "只支持POST请求"})
		return
	}
	var req struct {
		UserID int64 `json:"userId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "请求参数错误"})
		return
	}
	if err := gs.UserDB.DeleteUser(req.UserID); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
		return
	}
	gs.RemoveUserCache(req.UserID)
	gs.UserDB.SaveToFile()
	logger.Info(fmt.Sprintf("[GM] 删除账号: UserID=%d", req.UserID))
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "账号已删除"})
}

// handleGMCreateUser 新增账号
func handleGMCreateUser(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != "POST" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "只支持POST请求"})
		return
	}
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Nickname string `json:"nickname"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "请求参数错误"})
		return
	}
	if req.Email == "" || req.Password == "" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "邮箱和密码不能为空"})
		return
	}
	user, err := gs.UserDB.CreateUser(req.Email, req.Password)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
		return
	}
	if req.Nickname != "" {
		user.Nickname = req.Nickname
		gs.UserDB.SaveUser(user)
	}
	_ = gs.UserDB.GetOrCreateGameData(user.UserID)
	gs.UserDB.SaveToFile()
	logger.Info(fmt.Sprintf("[GM] 新增账号: UserID=%d Email=%s", user.UserID, user.Email))
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "创建成功",
		"userId":  user.UserID,
		"email":   user.Email,
	})
}

// handleGMExpPoolAdd 发放经验到用户经验分配器（经验池）
func handleGMExpPoolAdd(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != "POST" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "只支持POST请求"})
		return
	}
	var req struct {
		UserID int64 `json:"userId"`
		Amount int   `json:"amount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "请求参数错误"})
		return
	}
	if req.Amount < 0 {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "发放经验不能为负"})
		return
	}
	gameData := gs.UserDB.GetOrCreateGameData(req.UserID)
	if gameData.ExpPool < 0 {
		gameData.ExpPool = 0
	}
	gameData.ExpPool += req.Amount
	gs.UserDB.SaveGameData(req.UserID, gameData)
	logger.Info(fmt.Sprintf("[GM] 经验池发放: UserID=%d +%d 当前=%d", req.UserID, req.Amount, gameData.ExpPool))
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "发放成功",
		"expPool": gameData.ExpPool,
	})
}

// handleGMExpPoolSet 设置用户经验分配器（经验池）当前数值
func handleGMExpPoolSet(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != "POST" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "只支持POST请求"})
		return
	}
	var req struct {
		UserID int64 `json:"userId"`
		Value  int   `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "请求参数错误"})
		return
	}
	if req.Value < 0 {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "经验池数值不能为负"})
		return
	}
	gameData := gs.UserDB.GetOrCreateGameData(req.UserID)
	gameData.ExpPool = req.Value
	gs.UserDB.SaveGameData(req.UserID, gameData)
	logger.Info(fmt.Sprintf("[GM] 经验池设置: UserID=%d 设为 %d", req.UserID, gameData.ExpPool))
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "设置成功",
		"expPool": gameData.ExpPool,
	})
}

// ==================== 扭蛋机 GM 模块 ====================

// handleGMGachaList 扭蛋机奖励列表（带中文名称、索引）
func handleGMGachaList(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	list := GetGachaRewards()
	items := make([]map[string]interface{}, 0, len(list))
	for i, r := range list {
		name := r.Name
		if name == "" {
			name = GetItemName(r.ItemID)
		}
		items = append(items, map[string]interface{}{
			"index":   i,
			"itemID":  r.ItemID,
			"weight":  r.Weight,
			"name":    name,
			"isGold":  r.IsGold,
		})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"data":    items,
		"total":   len(items),
	})
}

// handleGMGachaUpdate 修改扭蛋机奖励（按索引）
func handleGMGachaUpdate(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != "POST" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "只支持POST请求"})
		return
	}
	var req struct {
		Index   int    `json:"index"`
		ItemID  int    `json:"itemID"`
		Weight  int    `json:"weight"`
		Name    string `json:"name"`
		IsGold  bool   `json:"isGold"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "请求参数错误"})
		return
	}
	reward := GachaReward{ItemID: req.ItemID, Weight: req.Weight, Name: req.Name, IsGold: req.IsGold}
	if ok := UpdateGachaReward(req.Index, reward); !ok {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "索引无效或越界"})
		return
	}
	logger.Info(fmt.Sprintf("[GM] 扭蛋机修改奖励: index=%d itemID=%d weight=%d", req.Index, req.ItemID, req.Weight))
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "修改成功"})
}

// handleGMGachaAdd 添加扭蛋机奖励（任意道具均可添加）
func handleGMGachaAdd(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != "POST" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "只支持POST请求"})
		return
	}
	var req struct {
		ItemID int    `json:"itemID"`
		Weight int    `json:"weight"`
		Name   string `json:"name"`
		IsGold bool   `json:"isGold"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "请求参数错误"})
		return
	}
	if req.ItemID <= 0 {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "itemID 必须大于 0"})
		return
	}
	AddGachaReward(GachaReward{ItemID: req.ItemID, Weight: req.Weight, Name: req.Name, IsGold: req.IsGold})
	logger.Info(fmt.Sprintf("[GM] 扭蛋机添加奖励: itemID=%d weight=%d 名称=%s", req.ItemID, req.Weight, GetItemName(req.ItemID)))
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "添加成功"})
}

// handleGMGachaRemove 删除扭蛋机奖励（按索引）
func handleGMGachaRemove(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != "POST" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "只支持POST请求"})
		return
	}
	var req struct {
		Index int `json:"index"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "请求参数错误"})
		return
	}
	if ok := RemoveGachaReward(req.Index); !ok {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "索引无效或越界"})
		return
	}
	logger.Info(fmt.Sprintf("[GM] 扭蛋机删除奖励: index=%d", req.Index))
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "删除成功"})
}

// handleGMGachaSave 保存扭蛋机奖励到 gacha_rewards.json
func handleGMGachaSave(w http.ResponseWriter, r *http.Request, gs *gameserver.GameServer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != "POST" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "只支持POST请求"})
		return
	}
	if err := SaveGachaRewards(); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
		return
	}
	logger.Info("[GM] 扭蛋机配置已保存到 gacha_rewards.json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "保存成功"})
}
