package typechart

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/seer-game/golang-version/internal/core/logger"
)

// Chart 表示“攻击属性 -> 防守属性 -> 伤害倍率”的矩阵。
// - 值域通常包含 0, 0.5, 0.75, 1, 1.5, 2 等（来自 xlsx 矩阵）
// - 若缺失则默认返回 1.0
type Chart struct {
	Version int
	Source  struct {
		Xlsx  string `json:"xlsx"`
		Sheet string `json:"sheet"`
	} `json:"source"`

	// Types: typeID(string) -> displayName
	Types map[string]string `json:"types"`

	// Chart: atkTypeID(string) -> defTypeID(string) -> multiplier
	Raw map[string]map[string]float64 `json:"chart"`

	loaded bool
	mu     sync.RWMutex

	cache map[int]map[int]float64
}

var (
	instance *Chart
	once     sync.Once
)

// GetInstance 获取全局属性克制矩阵实例
func GetInstance() *Chart {
	once.Do(func() {
		instance = &Chart{
			Types: make(map[string]string),
			Raw:   make(map[string]map[string]float64),
			cache: make(map[int]map[int]float64),
		}
	})
	return instance
}

// Load 从 data/type_chart.json 加载矩阵（与 skills.Load 类似的路径策略）
func (c *Chart) Load() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded {
		return nil
	}

	var data []byte
	var err error

	tryRead := func(path string) bool {
		bytes, e := os.ReadFile(path)
		if e != nil {
			err = e
			return false
		}
		data = bytes
		logger.Info(fmt.Sprintf("加载属性克制矩阵: %s", path))
		return true
	}

	if exePath, e := os.Executable(); e == nil {
		exeDir := filepath.Dir(exePath)
		candidates := []string{
			filepath.Join(exeDir, "..", "data", "type_chart.json"),
			filepath.Join(exeDir, "..", "..", "golang_version", "data", "type_chart.json"),
			filepath.Join(exeDir, "golang_version", "data", "type_chart.json"),
		}
		for _, p := range candidates {
			if tryRead(p) {
				break
			}
		}
	}
	if data == nil {
		candidates := []string{
			filepath.Join("data", "type_chart.json"),
			filepath.Join("golang_version", "data", "type_chart.json"),
			filepath.Join("..", "golang_version", "data", "type_chart.json"),
		}
		for _, p := range candidates {
			if tryRead(p) {
				break
			}
		}
	}
	if data == nil {
		if err != nil {
			logger.Warning(fmt.Sprintf("读取属性克制矩阵失败: %v", err))
			return err
		}
		return fmt.Errorf("type_chart.json not found")
	}

	// 使用单独的临时结构体承接 JSON，避免覆盖内部的互斥锁等状态
	var parsed struct {
		Version        int                                `json:"version"`
		Source         struct{ Xlsx, Sheet string }       `json:"source"`
		Types          map[string]string                  `json:"types"`
		Chart          map[string]map[string]float64      `json:"chart"`
		UnmappedHeader map[string]interface{}             `json:"unmapped_headers"` // 兼容字段，当前仅为调试使用
	}
	if e := json.Unmarshal(data, &parsed); e != nil {
		logger.Warning(fmt.Sprintf("解析属性克制矩阵失败: %v", e))
		return e
	}

	// 构建 int->int 缓存，加速查询
	cache := make(map[int]map[int]float64, len(parsed.Chart))
	for atkStr, inner := range parsed.Chart {
		atkID, e := strconv.Atoi(atkStr)
		if e != nil || atkID <= 0 {
			continue
		}
		row := make(map[int]float64, len(inner))
		for defStr, mult := range inner {
			defID, e := strconv.Atoi(defStr)
			if e != nil || defID <= 0 {
				continue
			}
			row[defID] = mult
		}
		cache[atkID] = row
	}

	// 将解析结果写入当前实例（不要整体赋值，避免破坏互斥锁）
	c.Version = parsed.Version
	c.Source.Xlsx = parsed.Source.Xlsx
	c.Source.Sheet = parsed.Source.Sheet
	c.Types = parsed.Types
	c.Raw = parsed.Chart
	c.cache = cache
	c.loaded = true
	return nil
}

// Multiplier 返回攻击方属性对防守方属性的伤害倍率；缺失则返回 1.0。
func (c *Chart) Multiplier(atkType, defType int) float64 {
	if atkType <= 0 || defType <= 0 {
		return 1.0
	}
	if !c.loaded {
		_ = c.Load()
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if row, ok := c.cache[atkType]; ok {
		if v, ok := row[defType]; ok {
			return v
		}
	}
	return 1.0
}

