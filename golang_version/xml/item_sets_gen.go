package main

// 小工具：根据 items.xml 自动整理人物装扮套装，生成 item_sets.xml。
//
// 规则（尽量贴近原游戏习惯）：
// - 只处理 Cat Name="个人装扮" 的道具（DbCatID=1）
// - 仅关注 type in {head, hand, waist, foot, eye} 这些角色装备部件
// - 根据道具中文名的前缀分组，例如：
//     "青龙守卫圣盔"、"青龙守卫腰甲"、"青龙守卫圣靴"
//   去掉末尾部件词后，公共前缀 "青龙守卫" 视作一套装备
// - 只有当同一前缀至少包含 3 个不同 type 时，才认为是“套装”
// - 生成的 item_sets.xml 结构如下：
//   <ItemSets>
//     <Set Name="青龙守卫套装">
//       <ItemRef ID="123456" Type="head" Name="青龙守卫圣盔"/>
//       <ItemRef ID="123457" Type="waist" Name="青龙守卫腰甲"/>
//       <ItemRef ID="123458" Type="foot" Name="青龙守卫圣靴"/>
//     </Set>
//     ...
//   </ItemSets>
//
// 用法（在 golang_version 目录下）：
//   go run xml/item_sets_gen.go
//
// 运行后会在同级 xml 目录下生成 item_sets.xml。

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type ItemsRoot struct {
	XMLName xml.Name `xml:"Items"`
	Cats    []Cat    `xml:"Cat"`
}

type Cat struct {
	ID      int    `xml:"ID,attr"`
	DbCatID int    `xml:"DbCatID,attr"`
	Name    string `xml:"Name,attr"`
	Items   []Item `xml:"Item"`
}

type Item struct {
	ID   int    `xml:"ID,attr"`
	Name string `xml:"Name,attr"`
	Type string `xml:"type,attr"`
}

type ItemSetsRoot struct {
	XMLName xml.Name  `xml:"ItemSets"`
	Sets    []ItemSet `xml:"Set"`
}

type ItemSet struct {
	Name  string      `xml:"Name,attr"`
	Items []ItemEntry `xml:"ItemRef"`
}

type ItemEntry struct {
	ID   int    `xml:"ID,attr"`
	Type string `xml:"Type,attr"`
	Name string `xml:"Name,attr"`
}

// 部件后缀词，按长度从长到短排序，方便匹配。
var suffixes = []string{
	"重盔", "战盔", "圣盔", "头盔", "护额", "护冠", "兜帽", "头套", "头冠", "帽子", "帽", "发冠", "冠",
	"圣靴", "战靴", "军靴", "重靴", "火焰靴", "滚轮", "板鞋", "战轮", "履带", "鞋子", "鞋", "靴子", "靴", "军靴",
	"腰甲", "圣甲", "护腰", "腰封", "腰链", "护带", "弹夹", "护带", "腰扣", "腰带", "腹甲", "腰", "带",
	"护手", "利爪", "飞轮手", "手臂", "肩甲", "笼手", "护肩", "长枪", "光刃", "双剑", "护腕", "手套", "翅膀", "钳子", "掌",
	"面罩", "面具", "护眼", "眼罩",
}

// 只把常见的头/手/腰/脚/眼视作一套的有效部件类型
var validTypes = map[string]bool{
	"head": true,
	"hand": true,
	"waist": true,
	"foot": true,
	"eye":  true,
}

func main() {
	baseDir, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	xmlPath := filepath.Join(baseDir, "xml", "items.xml")
	data, err := os.ReadFile(xmlPath)
	if err != nil {
		panic(fmt.Errorf("读取 items.xml 失败: %w", err))
	}

	var root ItemsRoot
	if err := xml.Unmarshal(data, &root); err != nil {
		panic(fmt.Errorf("解析 items.xml 失败: %w", err))
	}

	// 只看“个人装扮”这一类
	var clothCat *Cat
	for i := range root.Cats {
		if root.Cats[i].DbCatID == 1 || root.Cats[i].Name == "个人装扮" {
			clothCat = &root.Cats[i]
			break
		}
	}
	if clothCat == nil {
		panic("未找到 DbCatID=1 / Name=个人装扮 的 Cat")
	}

	type itemInfo struct {
		id   int
		name string
		typ  string
	}

	groups := make(map[string][]itemInfo) // 前缀 -> 条目

	for _, it := range clothCat.Items {
		if !validTypes[it.Type] {
			continue
		}
		prefix := extractPrefix(it.Name)
		if prefix == "" {
			continue
		}
		groups[prefix] = append(groups[prefix], itemInfo{
			id:   it.ID,
			name: it.Name,
			typ:  it.Type,
		})
	}

	var sets []ItemSet
	for prefix, items := range groups {
		typeSet := make(map[string]bool)
		for _, it := range items {
			typeSet[it.typ] = true
		}
		// 至少 3 个不同部位才算套装
		if len(typeSet) < 3 {
			continue
		}
		// 按 type+id 排个序，输出稳定
		sort.Slice(items, func(i, j int) bool {
			if items[i].typ == items[j].typ {
				return items[i].id < items[j].id
			}
			return items[i].typ < items[j].typ
		})
		set := ItemSet{
			Name: prefix + "套装",
		}
		for _, it := range items {
			set.Items = append(set.Items, ItemEntry{
				ID:   it.id,
				Type: it.typ,
				Name: it.name,
			})
		}
		sets = append(sets, set)
	}

	// 套装按名字排序
	sort.Slice(sets, func(i, j int) bool { return sets[i].Name < sets[j].Name })

	out := ItemSetsRoot{Sets: sets}
	outBytes, err := xml.MarshalIndent(out, "", "    ")
	if err != nil {
		panic(fmt.Errorf("生成 item_sets.xml 出错: %w", err))
	}

	outPath := filepath.Join(baseDir, "xml", "item_sets.xml")
	if err := os.WriteFile(outPath, append([]byte(xml.Header), outBytes...), 0644); err != nil {
		panic(fmt.Errorf("写入 item_sets.xml 失败: %w", err))
	}

	fmt.Printf("已根据 %s 生成套装文件: %s，共 %d 套\n", xmlPath, outPath, len(sets))
}

// extractPrefix 从物品中文名中去掉末尾部件后缀，得到套装前缀。
func extractPrefix(name string) string {
	for _, suf := range suffixes {
		if strings.HasSuffix(name, suf) {
			p := strings.TrimSpace(strings.TrimSuffix(name, suf))
			if len([]rune(p)) >= 2 { // 至少两个汉字前缀，否则容易误判
				return p
			}
		}
	}
	return ""
}

