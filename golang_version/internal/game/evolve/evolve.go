package evolve

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/seer-game/golang-version/internal/core/logger"
)

// Branch 定义某个 EvolvFlag(ID) 下的一个进化分支（与前端 EvolveXMLInfo.getMonToIDs 一致）
type Branch struct {
	MonTo          int
	EvolvItem      int
	EvolvItemCount int
}

type branchXML struct {
	MonTo          int `xml:"MonTo,attr"`
	EvolvItem      int `xml:"EvolvItem,attr"`
	EvolvItemCount int `xml:"EvolvItemCount,attr"`
}

type evolveXML struct {
	ID      int         `xml:"ID,attr"`
	Branchs []branchXML `xml:"Branch"`
}

type rootXML struct {
	Evolves []evolveXML `xml:"Evolve"`
}

var (
	once      sync.Once
	loadErr   error
	byFlag    map[int][]Branch
	loadedPath string
)

// LoadOnce 加载进化分支配置（只会执行一次）
func LoadOnce() error {
	once.Do(func() {
		byFlag = make(map[int][]Branch)
		loadErr = loadFromCandidates()
	})
	return loadErr
}

// GetBranches 返回指定 EvolvFlag 的分支列表（保持 XML 中顺序）
func GetBranches(flag int) ([]Branch, bool) {
	if err := LoadOnce(); err != nil {
		return nil, false
	}
	b, ok := byFlag[flag]
	if !ok || len(b) == 0 {
		return nil, false
	}
	return b, true
}

func loadFromCandidates() error {
	readAndParse := func(p string) bool {
		data, err := os.ReadFile(p)
		if err != nil {
			return false
		}
		var root rootXML
		if err := xml.Unmarshal(data, &root); err != nil {
			logger.Error(fmt.Sprintf("[EvolveXML] 解析失败: %s err=%v", p, err))
			return false
		}
		countEvolve := 0
		countBranch := 0
		for _, e := range root.Evolves {
			if e.ID <= 0 {
				continue
			}
			branches := make([]Branch, 0, len(e.Branchs))
			for _, b := range e.Branchs {
				if b.MonTo <= 0 {
					continue
				}
				branches = append(branches, Branch{
					MonTo:          b.MonTo,
					EvolvItem:      b.EvolvItem,
					EvolvItemCount: b.EvolvItemCount,
				})
				countBranch++
			}
			if len(branches) > 0 {
				byFlag[e.ID] = branches
				countEvolve++
			}
		}
		loadedPath = p
		logger.Info(fmt.Sprintf("[EvolveXML] 已加载进化分支: evolves=%d branches=%d path=%s", countEvolve, countBranch, p))
		return countEvolve > 0
	}

	// 1) 以可执行文件目录为基准（兼容 output/gameserver.exe）
	if exePath, err := os.Executable(); err == nil && exePath != "" {
		exeDir := filepath.Dir(exePath)
		candidates := []string{
			filepath.Join(exeDir, "..", "xml", "EvolveXMLInfo.xml"),
			filepath.Join(exeDir, "..", "..", "golang_version", "xml", "EvolveXMLInfo.xml"),
			filepath.Join(exeDir, "golang_version", "xml", "EvolveXMLInfo.xml"),
		}
		for _, c := range candidates {
			if readAndParse(c) {
				return nil
			}
		}
	}

	// 2) 以当前工作目录为基准（go run ./cmd/gameserver 或从仓库根目录运行）
	candidates := []string{
		filepath.Join("xml", "EvolveXMLInfo.xml"),
		filepath.Join("golang_version", "xml", "EvolveXMLInfo.xml"),
		filepath.Join("..", "golang_version", "xml", "EvolveXMLInfo.xml"),
	}
	for _, c := range candidates {
		if readAndParse(c) {
			return nil
		}
	}

	return fmt.Errorf("找不到 EvolveXMLInfo.xml（进化仓分支材料将无法校验/扣除）")
}

// LoadedPath 返回实际加载到的配置路径（用于诊断）
func LoadedPath() string {
	_ = LoadOnce()
	return loadedPath
}

