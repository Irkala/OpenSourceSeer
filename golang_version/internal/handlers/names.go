package handlers

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/seer-game/golang-version/internal/core/logger"
	gameskills "github.com/seer-game/golang-version/internal/game/skills"
)

// 精灵名称映射（会在启动时从 pets.xml 自动填充）
var PetNames = map[int]string{
	// 可以在这里放一些手动覆盖/修正的别名
	4150: "拂晓兔",
}

var (
	petNamesOnce  sync.Once
	itemNamesOnce sync.Once
)

// monsterXML 用于解析 pets.xml 中我们需要的最小字段
type monsterXML struct {
	ID      int    `xml:"ID,attr"`
	DefName string `xml:"DefName,attr"`
}

type monstersRoot struct {
	// pets.xml 顶层就是 <Monsters>，其子节点为 <Monster>，这里直接取所有 Monster
	Monsters []monsterXML `xml:"Monster"`
}

// loadPetNamesOnce 从 pets.xml 里加载精灵 ID 与中文名，填充到 PetNames 中
// 数据源：golang_version/xml/pets.xml（相对可执行文件或当前工作目录）
func loadPetNamesOnce() {
	petNamesOnce.Do(func() {
		_ = reloadPetNamesInternal(false)
	})
}

// reloadPetNamesInternal 从 pets.xml 重新追加精灵名称。
// 如果 reset 为 true，则在追加前清空已有的动态名称，仅保留手动写入 PetNames 的条目。
// 返回本次新增的条目数。
func reloadPetNamesInternal(reset bool) int {
	// 统计本次新增数量
	addedTotal := 0

	// 如果需要重置，则只保留 PetNames 中当前已有的键值对（一般是手动写入的别名）
	if reset {
		base := make(map[int]string, len(PetNames))
		for id, name := range PetNames {
			base[id] = name
		}
		PetNames = base
	}

	// 已经手动写在 map 里的不覆盖，只补充缺失部分
	readAndFill := func(path string) bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		var root monstersRoot
		if err := xml.Unmarshal(data, &root); err != nil {
			logger.Error(fmt.Sprintf("[GM] 解析 pets.xml 失败: %v", err))
			return false
		}
		count := 0
		for _, m := range root.Monsters {
			if m.ID <= 0 || m.DefName == "" {
				continue
			}
			if _, exists := PetNames[m.ID]; !exists {
				PetNames[m.ID] = m.DefName
				count++
			}
		}
		logger.Info(fmt.Sprintf("[GM] 已从 pets.xml 加载 %d 个精灵名称", count))
		addedTotal += count
		return true
	}

	// 1) 以可执行文件目录为基准，兼容 output/gameserver.exe 与直接 go build/gameserver.exe
	if exePath, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exePath)
		candidates := []string{
			filepath.Join(exeDir, "..", "xml", "pets.xml"),                         // golang_version/output 或 golang_version/bin
			filepath.Join(exeDir, "..", "..", "golang_version", "xml", "pets.xml"), // proj/output/..
			filepath.Join(exeDir, "golang_version", "xml", "pets.xml"),
		}
		for _, c := range candidates {
			if readAndFill(c) {
				return addedTotal
			}
		}
	}

	// 2) 以当前工作目录为基准（go run ./cmd/gameserver 或从仓库根目录运行）
	candidates := []string{
		filepath.Join("golang_version", "xml", "pets.xml"),
		filepath.Join("..", "golang_version", "xml", "pets.xml"),
	}
	for _, c := range candidates {
		if readAndFill(c) {
			return addedTotal
		}
	}

	logger.Warning("[GM] 未能找到 pets.xml，精灵中文名将仅使用内置少量映射")
	return addedTotal
}

// ReloadPetNamesFromXML 供 GM 面板调用：从 pets.xml 重新加载精灵名称映射，返回新增数量。
// 不会覆盖已有手动别名，仅为缺失的 ID 追加名称。
func ReloadPetNamesFromXML() int {
	return reloadPetNamesInternal(false)
}

// 道具名称映射（会在启动时从 items.xml 自动填充）
var ItemNames = map[int]string{
	// 这里可以放少量覆盖/别名；绝大多数由 items.xml 自动加载
}

// itemXML / itemsRoot 用于解析 items.xml
type itemXML struct {
	ID   int    `xml:"ID,attr"`
	Name string `xml:"Name,attr"`
}

type itemsRoot struct {
	Cats []struct {
		Items []itemXML `xml:"Item"`
	} `xml:"Cat"`
}

// GetPetName 获取精灵中文名称（优先从 pets.xml 动态加载）
func GetPetName(id int) string {
	if id <= 0 {
		return "未知精灵"
	}
	loadPetNamesOnce()
	if name, exists := PetNames[id]; exists {
		return name
	}
	return "未知精灵"
}

// GMPetInfo 提供给 GM 前端使用的精灵信息
type GMPetInfo struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// GetAllPetsForGM 返回按 ID 升序排序的精灵列表（ID + 中文名），供 GM 下拉/搜索用
func GetAllPetsForGM() []GMPetInfo {
	loadPetNamesOnce()
	list := make([]GMPetInfo, 0, len(PetNames))
	for id, name := range PetNames {
		list = append(list, GMPetInfo{
			ID:   id,
			Name: name,
		})
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].ID < list[j].ID
	})
	return list
}

// loadItemNamesOnce 从 items.xml 里加载道具 ID 与中文名，填充到 ItemNames 中
// 数据源：golang_version/xml/items.xml
func loadItemNamesOnce() {
	itemNamesOnce.Do(func() {
		readAndFill := func(path string) bool {
			data, err := os.ReadFile(path)
			if err != nil {
				return false
			}
			var root itemsRoot
			if err := xml.Unmarshal(data, &root); err != nil {
				logger.Error(fmt.Sprintf("[GM] 解析 items.xml 失败: %v", err))
				return false
			}
			count := 0
			for _, cat := range root.Cats {
				for _, it := range cat.Items {
					if it.ID <= 0 || it.Name == "" {
						continue
					}
					if _, exists := ItemNames[it.ID]; !exists {
						ItemNames[it.ID] = it.Name
						count++
					}
				}
			}
			logger.Info(fmt.Sprintf("[GM] 已从 items.xml 加载 %d 个道具名称", count))
			return true
		}

		if exePath, err := os.Executable(); err == nil {
			exeDir := filepath.Dir(exePath)
			candidates := []string{
				filepath.Join(exeDir, "..", "xml", "items.xml"),                         // golang_version/output 或 golang_version/bin
				filepath.Join(exeDir, "..", "..", "golang_version", "xml", "items.xml"), // proj/output/..
				filepath.Join(exeDir, "golang_version", "xml", "items.xml"),
			}
			for _, c := range candidates {
				if readAndFill(c) {
					return
				}
			}
		}

		candidates := []string{
			filepath.Join("golang_version", "xml", "items.xml"),
			filepath.Join("..", "golang_version", "xml", "items.xml"),
		}
		for _, c := range candidates {
			if readAndFill(c) {
				return
			}
		}

		logger.Warning("[GM] 未能找到 items.xml，道具中文名将仅使用内置映射")
	})
}

// GetItemName 获取道具中文名称（优先从 items.xml 动态加载）
func GetItemName(id int) string {
	if id <= 0 {
		return "未知道具"
	}
	loadItemNamesOnce()
	if name, exists := ItemNames[id]; exists {
		return name
	}
	return "未知道具"
}

// GMItemInfo 提供给 GM 前端使用的道具信息
type GMItemInfo struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// GetAllItemsForGM 返回按 ID 升序排序的道具列表（ID + 中文名），供 GM 下拉/搜索用
func GetAllItemsForGM() []GMItemInfo {
	loadItemNamesOnce()
	list := make([]GMItemInfo, 0, len(ItemNames))
	for id, name := range ItemNames {
		list = append(list, GMItemInfo{
			ID:   id,
			Name: name,
		})
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].ID < list[j].ID
	})
	return list
}

// GetSkillName 获取技能中文名称（从 skills.xml 加载）
func GetSkillName(id int) string {
	if id <= 0 {
		return ""
	}
	return gameskills.GetInstance().GetName(id)
}