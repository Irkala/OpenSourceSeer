package skills

import (
	"math"
	"math/rand"
	"strconv"
	"strings"

	gamepets "github.com/seer-game/golang-version/internal/game/pets"
	"github.com/seer-game/golang-version/internal/game/sptboss"
)

// 异常状态常量：客户端 status 数组的索引 = 状态类型，值 = 持续回合数
// 与 PetFightMsgManager.STATUS_ARRAY 对应：["麻痹","中毒","烧伤","吸取","被吸取","冻伤","害怕","疲惫","睡眠",...,"易燃",...]
const (
	StatusIndexParalysis = 0 // 麻痹
	StatusIndexPoison    = 1 // 中毒
	StatusIndexBurn      = 2 // 烧伤
	StatusIndexDrain     = 3 // 吸取对方的体力
	StatusIndexDrained   = 4 // 被对方吸取体力
	StatusIndexFreeze    = 5 // 冻伤
	StatusIndexFear      = 6 // 害怕/畏缩（用于畏缩时本回合无法行动）
	StatusIndexFatigue   = 7 // 疲惫（下回合无法行动）
	StatusIndexSleep     = 8 // 睡眠（效果与麻痹一致，但图标不同）
	StatusIndexFlammable = 13 // 易燃（火系技能额外伤害，按回合递减）
)

// randomStatusRounds 返回 2~3 回合，用于控制类异常（麻痹/害怕/睡眠/疲惫等）
// 客户端在每回合开始会显示递减后的数值，因此这里选 2~3 回合，使前端常见显示为 1~2 回合，更贴近原版体验。
func randomStatusRounds() byte {
	return byte(rand.Intn(2) + 2)
}

// randomDoTRounds 仅用于中毒/烧伤/冻伤：随机 1~2 回合，与控制类异常分开
func randomDoTRounds() byte {
	return byte(rand.Intn(2) + 1)
}

// applyNewStatusDamageOnce 本回合刚挂上中毒/烧伤/冻伤时立即扣一次血（1/8 最大HP），与 ProcessStatusEffects 单次扣量一致
func applyNewStatusDamageOnce(hp *uint32, maxHP uint32) {
	if hp == nil || maxHP == 0 {
		return
	}
	dmg := maxHP / 8
	if dmg > *hp {
		dmg = *hp
	}
	*hp -= dmg
}

// 能力项索引（与 Lua TRAIT、客户端 battleLv 对应）
// 0=攻击 1=防御 2=特攻 3=特防 4=速度 5=命中
const (
	StatAttack   = 0
	StatDefence  = 1
	StatSpAtk    = 2
	StatSpDef    = 3
	StatSpeed    = 4
	StatAccuracy = 5
)

const (
	maxStatStage = 6
	minStatStage = -6
)

// CritBuffState 暴击率强化状态（由某些技能效果设置，例如 SideEffect 58 系列）
// 该状态不直接体现在 battleLv 中，而是独立的“下 N 回合必定暴击/提高暴击率”的标记。
// 为了最小侵入，这里先定义为简单的“下 N 回合攻击技能必定暴击”：
// - N 存在于 SideEffectArg（如 1000058 的 des），由 handlers 在回合开始递减；
// - 具体存储位置在 BattleState 中扩展字段。
// Go 版 BattleState 暂未扩展该字段，因此真正的暴击强化在 handlers 中实现。

// ParseSideEffectArg 解析 SideEffectArg 字符串为整数切片（与 Lua SkillEffects.parseArgs 一致）
// 支持 "1 15 -1"、"5 100 1" 等格式
func ParseSideEffectArg(s string) []int {
	var out []int
	for _, part := range strings.Fields(s) {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

// EffectResult 单次技能效果应用结果，用于日志或扩展
type EffectResult struct {
	GainHP       int32
	RecoilDamage uint32
}

// ProcessStatusEffects 回合开始时结算异常状态伤害（对齐 Lua processStatusEffects）
// 烧伤/中毒/冻伤：每回合扣 1/8 最大血量，并递减对应 status 回合数。
func ProcessStatusEffects(playerHP, enemyHP *uint32, playerMaxHP, enemyMaxHP uint32, playerStatus, enemyStatus *[20]byte) {
	if playerHP != nil && playerStatus != nil && playerMaxHP > 0 {
		// 中毒 status[1]
		if playerStatus[StatusIndexPoison] > 0 {
			dmg := playerMaxHP / 8
			if dmg > *playerHP {
				dmg = *playerHP
			}
			*playerHP -= dmg
			playerStatus[StatusIndexPoison]--
		}
		// 烧伤 status[2]：每回合 1/8 最大HP
		if playerStatus[StatusIndexBurn] > 0 {
			dmg := playerMaxHP / 8
			if dmg > *playerHP {
				dmg = *playerHP
			}
			*playerHP -= dmg
			playerStatus[StatusIndexBurn]--
		}
		// 冻伤 status[5]：效果与烧伤/中毒相同，每回合 1/8 最大HP
		if playerStatus[StatusIndexFreeze] > 0 {
			dmg := playerMaxHP / 8
			if dmg > *playerHP {
				dmg = *playerHP
			}
			*playerHP -= dmg
			playerStatus[StatusIndexFreeze]--
		}
	}
	if enemyHP != nil && enemyStatus != nil && enemyMaxHP > 0 {
		if enemyStatus[StatusIndexPoison] > 0 {
			dmg := enemyMaxHP / 8
			if dmg > *enemyHP {
				dmg = *enemyHP
			}
			*enemyHP -= dmg
			enemyStatus[StatusIndexPoison]--
		}
		if enemyStatus[StatusIndexBurn] > 0 {
			dmg := enemyMaxHP / 8
			if dmg > *enemyHP {
				dmg = *enemyHP
			}
			*enemyHP -= dmg
			enemyStatus[StatusIndexBurn]--
		}
		if enemyStatus[StatusIndexFreeze] > 0 {
			dmg := enemyMaxHP / 8
			if dmg > *enemyHP {
				dmg = *enemyHP
			}
			*enemyHP -= dmg
			enemyStatus[StatusIndexFreeze]--
		}
	}

	// 寄生种子/吸取（status[3]/status[4]）：每回合吸取对方最大体力的 1/8，并为自己回复等量体力
	if playerHP != nil && enemyHP != nil && playerStatus != nil && enemyStatus != nil {
		// 玩家吸取敌方
		if playerStatus[StatusIndexDrain] > 0 && enemyStatus[StatusIndexDrained] > 0 && enemyMaxHP > 0 && *enemyHP > 0 {
			dmg := enemyMaxHP / 8
			if dmg == 0 {
				dmg = 1
			}
			if dmg > *enemyHP {
				dmg = *enemyHP
			}
			*enemyHP -= dmg
			sum := *playerHP + dmg
			if sum > playerMaxHP {
				sum = playerMaxHP
			}
			*playerHP = sum
			playerStatus[StatusIndexDrain]--
			enemyStatus[StatusIndexDrained]--
		}
		// 敌方吸取玩家
		if enemyStatus[StatusIndexDrain] > 0 && playerStatus[StatusIndexDrained] > 0 && playerMaxHP > 0 && *playerHP > 0 {
			dmg := playerMaxHP / 8
			if dmg == 0 {
				dmg = 1
			}
			if dmg > *playerHP {
				dmg = *playerHP
			}
			*playerHP -= dmg
			sum := *enemyHP + dmg
			if sum > enemyMaxHP {
				sum = enemyMaxHP
			}
			*enemyHP = sum
			enemyStatus[StatusIndexDrain]--
			playerStatus[StatusIndexDrained]--
		}
	}
}

// ProcessEnemyStatusOnPlayerAction 在“我方使用道具等视为一次出手”的场景下，
// 仅对敌方结算一跳异常状态伤害（烧伤/中毒/冻伤/寄生种子吸取等），
// 不再对我方自身的异常重复结算一遍回合开始的伤害。
func ProcessEnemyStatusOnPlayerAction(playerHP, enemyHP *uint32, playerMaxHP, enemyMaxHP uint32, playerStatus, enemyStatus *[20]byte) {
	// 敌方基础异常：中毒/烧伤/冻伤，每次按最大体力的 1/8 结算
	if enemyHP != nil && enemyStatus != nil && enemyMaxHP > 0 {
		// 中毒
		if enemyStatus[StatusIndexPoison] > 0 {
			dmg := enemyMaxHP / 8
			if dmg > *enemyHP {
				dmg = *enemyHP
			}
			*enemyHP -= dmg
			enemyStatus[StatusIndexPoison]--
		}
		// 烧伤
		if enemyStatus[StatusIndexBurn] > 0 {
			dmg := enemyMaxHP / 8
			if dmg > *enemyHP {
				dmg = *enemyHP
			}
			*enemyHP -= dmg
			enemyStatus[StatusIndexBurn]--
		}
		// 冻伤
		if enemyStatus[StatusIndexFreeze] > 0 {
			dmg := enemyMaxHP / 8
			if dmg > *enemyHP {
				dmg = *enemyHP
			}
			*enemyHP -= dmg
			enemyStatus[StatusIndexFreeze]--
		}
	}

	// 寄生种子/吸取：仅处理“玩家吸取敌方”的方向（敌方扣血、我方回血），
	// 不在这里处理“敌方吸取玩家”的场景，避免我方在用道具时多吃一跳 DOT。
	if playerHP != nil && enemyHP != nil && playerStatus != nil && enemyStatus != nil &&
		playerMaxHP > 0 && enemyMaxHP > 0 && *enemyHP > 0 {
		if playerStatus[StatusIndexDrain] > 0 && enemyStatus[StatusIndexDrained] > 0 {
			dmg := enemyMaxHP / 8
			if dmg == 0 {
				dmg = 1
			}
			if dmg > *enemyHP {
				dmg = *enemyHP
			}
			*enemyHP -= dmg
			sum := *playerHP + dmg
			if sum > playerMaxHP {
				sum = playerMaxHP
			}
			*playerHP = sum
			playerStatus[StatusIndexDrain]--
			enemyStatus[StatusIndexDrained]--
		}
	}
}

// ApplyEffect 根据技能附加效果修改战斗状态（对齐 Lua seer_battle + seer_skill_effects）
// 修改 playerHP, enemyHP, playerBattleLv, enemyBattleLv, playerStatus, enemyStatus 原地；
// defenderPetID 为被攻击方精灵 ID，用于 BOSS 异常免疫。
// defenderControlImmune 为 true 时对方免疫控制类异常（麻痹/畏缩/睡眠）；谱尼能量封印(3) 传 false 表示可被控场。
// 返回本回合因效果产生的 gainHP（给攻击方）和 recoilDamage（反伤给攻击方）。
func ApplyEffect(skill *Skill, damage uint32, playerHP, enemyHP *uint32, playerMaxHP, enemyMaxHP uint32,
	playerBattleLv, enemyBattleLv *[6]int8, playerStatus, enemyStatus *[20]byte, defenderPetID int, defenderControlImmune bool) (gainHP int32, recoilDamage uint32) {
	if skill == nil {
		return 0, 0
	}
	eid := skill.EffectID
	if eid <= 0 {
		return 0, 0
	}
	args := ParseSideEffectArg(skill.SideEffectArg)
	statusImmune := sptboss.IsStatusImmune(defenderPetID)
	statDropImmune := sptboss.IsStatDropImmune(defenderPetID)

	switch eid {
	case 1:
		// 吸血：恢复造成伤害的一定比例
		percent := 50
		if len(args) >= 1 {
			percent = args[0]
		}
		if percent <= 0 {
			percent = 50
		}
		heal := uint32(math.Floor(float64(damage) * float64(percent) / 100))
		if heal > 0 && playerHP != nil {
			sum := *playerHP + heal
			if sum > playerMaxHP {
				sum = playerMaxHP
			}
			*playerHP = sum
			gainHP = int32(heal)
		}

	case 2:
		// 降低对方能力等级（Lua eid=2）SideEffectArg: stat stages；免疫能力下降的 BOSS 不低于 0
		stat, stages := 1, 1
		if len(args) >= 1 {
			stat = args[0]
		}
		if len(args) >= 2 {
			stages = args[1]
		}
		if stat < 0 || stat > 5 {
			stat = 0
		}
		if enemyBattleLv != nil {
			cur := int(enemyBattleLv[stat])
			cur -= stages
			if statDropImmune && cur < 0 {
				cur = 0
			} else if cur < minStatStage {
				cur = minStatStage
			}
			enemyBattleLv[stat] = int8(cur)
		}

	case 3:
		// 提高自身能力等级（同 case 4，Lua eid=3）；SideEffectArg 可多组 stat chance stages，如 "0 100 2 2 100 2"
		// 约定：stat 仅允许 0..4（攻、防、特攻、特防、速度），不允许直接提升“命中”能力等级。
		// 特例：异空突破(12259) 描述为“解除自身所有的能力下降状态”，不带 SideEffectArg，
		// 在此按“清除自身所有负向能力等级（含命中），正向不变”实现。
		if skill != nil && skill.ID == 12259 {
			if playerBattleLv != nil {
				for i := 0; i < 6; i++ {
					if playerBattleLv[i] < 0 {
						playerBattleLv[i] = 0
					}
				}
			}
		} else {
			for i := 0; i+2 < len(args); i += 3 {
				stat, chance, stages := args[i], 100, 1
				if i+1 < len(args) {
					chance = args[i+1]
				}
				if i+2 < len(args) {
					stages = args[i+2]
				}
				if stat < 0 || stat > 4 {
					stat = 0
				}
				if rand.Intn(100) < chance && playerBattleLv != nil {
					cur := int(playerBattleLv[stat])
					cur += stages
					if cur > maxStatStage {
						cur = maxStatStage
					}
					playerBattleLv[stat] = int8(cur)
				}
			}
		}

	case 4:
		// 提升自己能力等级 SideEffectArg: 可多组 stat chance stages，如红韵 "0 100 1 1 100 1 3 100 1"、觉醒 "0 100 2 2 100 2"
		// 特例修正：袭天舞(20160) 的数据为 SideEffect="4 4 5" + Args="0 100 2 2 100 2 4 100 -1"，
		// 期望效果是“自身攻+2、自身特攻+2、对手速度-1”，而不是“自身速度-1”。
		//
		// 兼容修正：部分“全能力提升”类技能在数据中包含命中(5)提升，但技能描述并不包含“命中提升”。
		// 这里按“描述优先”的规则：若同一个效果参数里同时提升了 0..4（攻防特攻特防速度）这五项，
		// 则忽略对命中(5)的正向提升（不影响那些明确提升命中的技能：它们通常不会同时包含 0..4 全套提升）。
		hasCoreAllUp := false
		if len(args) >= 15 { // 5 组 * (stat chance stages)
			core := map[int]bool{}
			for i := 0; i+2 < len(args); i += 3 {
				stat := args[i]
				chance := 100
				stages := 1
				if i+1 < len(args) {
					chance = args[i+1]
				}
				if i+2 < len(args) {
					stages = args[i+2]
				}
				if chance <= 0 || stages <= 0 {
					continue
				}
				if stat >= 0 && stat <= 4 {
					core[stat] = true
				}
			}
			hasCoreAllUp = core[0] && core[1] && core[2] && core[3] && core[4]
		}
		for i := 0; i+2 < len(args); i += 3 {
			stat, chance, stages := args[i], 100, 1
			if i+1 < len(args) {
				chance = args[i+1]
			}
			if i+2 < len(args) {
				stages = args[i+2]
			}
			if stat < 0 || stat > 5 {
				stat = 0
			}
			if rand.Intn(100) >= chance {
				continue
			}
			// 襲天舞特殊处理：stat=4(速度) 且 stages<0 时，改为降低对手速度等级
			if skill.ID == 20160 && stat == StatSpeed && stages < 0 {
				if enemyBattleLv != nil {
					cur := int(enemyBattleLv[stat])
					cur += stages
					if statDropImmune && cur < 0 {
						cur = 0
					} else if cur < minStatStage {
						cur = minStatStage
					}
					enemyBattleLv[stat] = int8(cur)
				}
				continue
			}
			// “全能力提升”模式下忽略命中(5)正向提升
			if hasCoreAllUp && stat == StatAccuracy && stages > 0 {
				continue
			}
			// 默认：修改自身能力等级
			if playerBattleLv != nil {
				cur := int(playerBattleLv[stat])
				cur += stages
				if cur > maxStatStage {
					cur = maxStatStage
				} else if cur < minStatStage {
					cur = minStatStage
				}
				playerBattleLv[stat] = int8(cur)
			}
		}

	case 5:
		// 降低对方或提升自己能力：stages 负数=降对手，正数=升自己；SideEffectArg 可多组 stat chance stages，如电闪光 "4 100 -1 5 100 -1"（速度、命中各-1）
		for i := 0; i+2 < len(args); i += 3 {
			stat, chance, stages := args[i], 100, -1
			if i+1 < len(args) {
				chance = args[i+1]
			}
			if i+2 < len(args) {
				stages = args[i+2]
			}
			if stat < 0 || stat > 5 {
				stat = 0
			}
			if rand.Intn(100) >= chance {
				continue
			}
			if stages < 0 {
				if enemyBattleLv != nil {
					cur := int(enemyBattleLv[stat])
					cur += stages
					if statDropImmune && cur < 0 {
						cur = 0
					} else if cur < minStatStage {
						cur = minStatStage
					}
					enemyBattleLv[stat] = int8(cur)
				}
			} else {
				if playerBattleLv != nil {
					cur := int(playerBattleLv[stat])
					cur += stages
					if cur > maxStatStage {
						cur = maxStatStage
					}
					playerBattleLv[stat] = int8(cur)
				}
			}
		}
		// 特例：安详之歌/灵光缠绕/幻动光环 等 SideEffect="5 5 16"，
		// 约定 SideEffectArg 最后一项为“令对手陷入睡眠”的概率（百分比）。
		if skill != nil && (skill.ID == 21172 || skill.ID == 21451 || skill.ID == 21763) && !defenderControlImmune && !statusImmune {
			sleepChance := 0
			if len(args) >= 7 {
				sleepChance = args[6]
			}
			if sleepChance <= 0 {
				sleepChance = 60
			}
			if rand.Intn(100) < sleepChance && enemyStatus != nil && enemyStatus[StatusIndexSleep] == 0 {
				enemyStatus[StatusIndexSleep] = randomStatusRounds()
			}
		}

	case 6:
		// 反伤：自身受到伤害的一定比例
		divisor := 4
		if len(args) >= 1 {
			divisor = args[0]
		}
		if divisor <= 0 {
			divisor = 4
		}
		recoil := uint32(math.Floor(float64(damage) / float64(divisor)))
		if recoil > 0 && playerHP != nil {
			if *playerHP <= recoil {
				recoil = *playerHP
				*playerHP = 0
			} else {
				*playerHP -= recoil
			}
			recoilDamage = recoil
		}

	case 7:
		// 同生共死（Lua eid=7）
		// 尤纳斯/哈莫雷特/盖亚/塔克林/塔西亚/雷伊 免疫此效果
		if sptboss.IsSameLifeDeathImmune(defenderPetID) {
			break
		}
		// 需求：直接伤害且只在敌方血量高于我方时生效：
		// - 若 enemyHP > playerHP，则伤害 = enemyHP - playerHP，本质效果是“把对方血量压到与自己相同”
		// - 若 enemyHP <= playerHP，则不造成伤害（伤害=0，不会给对方回血）
		if playerHP != nil && enemyHP != nil {
			// 仅当敌方当前 HP 大于我方当前 HP 时生效
			if *enemyHP > *playerHP {
				original := *enemyHP
				target := *playerHP
				// 上限依然不能超过敌方最大 HP；下限为 0
				if target > enemyMaxHP {
					target = enemyMaxHP
				}
				*enemyHP = target
				// 这里的“直接伤害”由 HP 变化体现（damage 由外部流程记录），不在此额外修改 damage 变量
				_ = original // 预留给后续若需要记录实际伤害值时使用
			}
		}

	case 8:
		// 手下留情：对方HP至少保留1（在 handlers 里对伤害做上限处理，此处不修改HP）

	case 9:
		// 愤怒：受到伤害在范围内则攻击+1级（Lua eid=9）SideEffectArg: minDamage maxDamage
		minD, maxD := 20, 80
		if len(args) >= 1 {
			minD = args[0]
		}
		if len(args) >= 2 {
			maxD = args[1]
		}
		if playerBattleLv != nil && damage >= uint32(minD) && damage <= uint32(maxD) {
			cur := int(playerBattleLv[StatAttack])
			cur++
			if cur > maxStatStage {
				cur = maxStatStage
			}
			playerBattleLv[StatAttack] = int8(cur)
		}

	case 10:
		// 麻痹：客户端 status[0]=持续回合数；所有 BOSS 免疫控制类异常（除谱尼能量封印等）
		if defenderControlImmune {
			break
		}
		if !statusImmune {
			chance := 10
			if len(args) >= 1 {
				chance = args[0]
			}
			if rand.Intn(100) < chance && enemyStatus != nil && enemyStatus[StatusIndexParalysis] == 0 {
				enemyStatus[StatusIndexParalysis] = randomStatusRounds()
			}
		}

	case 12:
		// 烧伤：客户端 status[2]=持续回合数（火焰漩涡等）
		if !statusImmune {
			chance := 10
			if len(args) >= 1 {
				chance = args[0]
			}
			if rand.Intn(100) < chance && enemyStatus != nil && enemyStatus[StatusIndexBurn] == 0 {
				enemyStatus[StatusIndexBurn] = randomDoTRounds()
				applyNewStatusDamageOnce(enemyHP, enemyMaxHP)
			}
		}

	case 11:
		// 中毒：客户端 status[1]=持续回合数
		if !statusImmune {
			chance := 10
			if len(args) >= 1 {
				chance = args[0]
			}
			if rand.Intn(100) < chance && enemyStatus != nil && enemyStatus[StatusIndexPoison] == 0 {
				enemyStatus[StatusIndexPoison] = randomDoTRounds()
				applyNewStatusDamageOnce(enemyHP, enemyMaxHP)
			}
		}

	case 13:
		// 寄生种子：吸取对方最大体力的 1/8（对草系精灵无效）
		// 客户端 status[3]/status[4]=持续回合数
		// 需求：亚伦斯免疫寄生种子
		if sptboss.IsYaLunSiBoss(defenderPetID) {
			break
		}
		rounds := 5
		if len(args) >= 1 && args[0] > 0 {
			rounds = args[0]
		}
		// 对草系精灵无效
		if p := gamepets.GetInstance().Get(defenderPetID); p != nil {
			if p.Type == sptboss.TypeGrass || (p.Type2 > 0 && p.Type2 == sptboss.TypeGrass) {
				break
			}
		}
		if playerStatus != nil && enemyStatus != nil {
			// 若已处于吸取状态则不重复覆盖
			if enemyStatus[StatusIndexDrained] == 0 {
				playerStatus[StatusIndexDrain] = byte(rounds)
				enemyStatus[StatusIndexDrained] = byte(rounds)
			}
		}

	case 28:
		// 削减对方 1/N 的 HP（客户端 Effect_28）：常见于“海洋之心”等
		// 按原版规则，应基于对方“当前剩余体力”计算，而不是最大体力。
		divisor := 4
		if len(args) >= 1 && args[0] > 0 {
			divisor = args[0]
		}
		if divisor <= 0 {
			divisor = 4
		}
		if enemyHP != nil && *enemyHP > 0 {
			d := *enemyHP / uint32(divisor)
			if d == 0 {
				d = 1
			}
			if d > *enemyHP {
				d = *enemyHP
			}
			*enemyHP -= d
		}

	case 43:
		// 回复自身一定比例体力（与前端/配置约定一致）：
		// - 常见于「光合作用/栖息/能量吸收/地之守护/机能修复」等
		// - SideEffectArg 的第一个数字表示分母 N：回复自身最大体力的 1/N
		//   例如：N=2 -> 回复 1/2；N=4 -> 回复 1/4；N=3 -> 回复 1/3。
		// - 部分技能 SideEffect 形如 "43 201"，Go 端解析时只取第一个数字 43。
		divisor := 2
		if len(args) >= 1 && args[0] > 0 {
			divisor = args[0]
		}
		if divisor <= 0 {
			divisor = 2
		}
		if playerHP != nil && playerMaxHP > 0 && *playerHP > 0 {
			heal := uint32(playerMaxHP / uint32(divisor))
			if heal == 0 {
				heal = 1
			}
			// 钳位到满血；飘字统一显示“应回复值”（最大体力的 1/N），
			// 以满足光合作用等技能“显示 + 一半血量”的需求。
			before := *playerHP
			sum := before + heal
			if sum > playerMaxHP {
				sum = playerMaxHP
			}
			*playerHP = sum
			gainHP = int32(heal)
		}

	case 14:
		// 冻伤：客户端 status[5]=持续回合数
		if !statusImmune {
			chance := 10
			if len(args) >= 1 {
				chance = args[0]
			}
			if rand.Intn(100) < chance && enemyStatus != nil && enemyStatus[StatusIndexFreeze] == 0 {
				enemyStatus[StatusIndexFreeze] = randomDoTRounds()
				applyNewStatusDamageOnce(enemyHP, enemyMaxHP)
			}
		}

	case 15:
		// 畏缩：对方本回合无法行动（Lua eid=15）客户端 status[6]=害怕；所有 BOSS 免疫控制类异常（除谱尼能量封印等）
		if defenderControlImmune {
			break
		}
		if !statusImmune {
			chance := 10
			if len(args) >= 1 {
				chance = args[0]
			}
			if rand.Intn(100) < chance && enemyStatus != nil {
				enemyStatus[StatusIndexFear] = randomStatusRounds()
			}
		}

	case 16:
		// 睡眠：客户端 status[8]=持续回合数，效果与麻痹类似（本回合无法行动）；所有 BOSS 免疫控制类异常（除谱尼能量封印等）
		if defenderControlImmune {
			break
		}
		if !statusImmune {
			chance := 10
			if len(args) >= 1 {
				chance = args[0]
			}
			if rand.Intn(100) < chance && enemyStatus != nil && enemyStatus[StatusIndexSleep] == 0 {
				enemyStatus[StatusIndexSleep] = randomStatusRounds()
			}
		}

	case 20:
		// 疲惫：使用后下回合无法行动（Lua eid=20）客户端 status[7]=疲惫
		chance := 100
		if len(args) >= 1 {
			chance = args[0]
		}
		if rand.Intn(100) < chance && playerStatus != nil {
			// 异常状态统一 1~2 回合随机
			playerStatus[StatusIndexFatigue] = randomStatusRounds()
		}

	case 22:
		// 令对手疲惫：n% 概率令对手疲惫 m 回合（Lua eid=22）
		// 与 20 相对：20 是“自身疲惫”，22 是“让对手疲惫”。
		// 疲惫属于控制类异常：若对方免疫控制或免疫异常，则不生效。
		if defenderControlImmune || statusImmune {
			break
		}
		chance := 10
		rounds := 1
		if len(args) >= 1 {
			chance = args[0]
		}
		if len(args) >= 2 && args[1] > 0 {
			rounds = args[1]
		}
		if rounds < 1 {
			rounds = 1
		}
		// 谱尼技能「灵魂干涉」(ID=20499)：描述为“令对手疲惫2回合”，
		// 在当前“回合开始即递减”的实现下，配置 2 实际只会生效 1 回合。
		// 这里对该技能单独 +1，使内部存 3 回合，对应玩家体验为 2 回合。
		if rand.Intn(100) < chance && enemyStatus != nil {
			// 若对手已处于疲惫状态，则覆盖为新的持续回合数
			enemyStatus[StatusIndexFatigue] = byte(rounds)
		}

	case 29:
		// 额外增加 n 点固定伤害（SideEffectArg 第一个数为固定伤害值）
		fixedDmg := 0
		if len(args) >= 1 {
			fixedDmg = args[0]
		}
		if fixedDmg > 0 && enemyHP != nil {
			d := uint32(fixedDmg)
			if d > *enemyHP {
				d = *enemyHP
			}
			*enemyHP -= d
		}

	case 33:
		// 消除对手能力提升状态（对应 skills.xml 中 Move.SideEffect=33，例如“净化/清除术/燃烧殆尽”等）
		// 规则：仅清除对方“强化”(正向能力等级)，弱化(负向)保持不变
		if enemyBattleLv != nil {
			for i := 0; i < 6; i++ {
				if enemyBattleLv[i] > 0 {
					enemyBattleLv[i] = 0
				}
			}
		}

	case 143:
		// 使对手的能力提升效果反转成能力下降效果（Effect_143）
		// 规则：enemyBattleLv 中 >0 的强化全部取反为对应的负值，≤0 的保持不变。
		if enemyBattleLv != nil {
			for i := 0; i < 6; i++ {
				cur := int(enemyBattleLv[i])
				if cur > 0 {
					// 反转为同幅度下降，且不低于最小等级
					cur = -cur
					if cur < minStatStage {
						cur = minStatStage
					}
					enemyBattleLv[i] = int8(cur)
				}
			}
		}

	case 47:
		// 百变随心等（Lua eid=47）：自身在若干回合内免疫能力下降效果。
		// 具体逻辑在 2405 中通过 BattleState.PlayerStatDropImmuneRounds 与 IsPlayerStatDropImmune 实现，
		// 这里只作为占位，不直接修改 battleLv。

	case 52:
		// 回避系技能（Lua eid=52）：1 先手使用且自身速度大于对方时，使对方的下 1 个技能失效。
		// 服务端实现：在 handlers 中判断“是否先手且速度更快”，满足条件时，将 BattleState.EnemyNextAttackMustMiss 置为 true。
		// 这里本身不修改 HP 或能力等级，仅保留占位，SideEffectArg 目前未使用。

	case 62:
		// 镇魂歌（Lua eid=62）：占位实现
		// 需求：所有谱尼（封印/真身/厉害谱尼形态）均免疫镇魂歌效果。
		// 若后续补充完整效果实现，需要在此处保留对 defenderPetID 为谱尼(300) 的免疫判断。
		if defenderPetID == sptboss.PetIDPuni {
			break
		}
		// 其余目标当前不在此实现具体效果（等效于无效果，占位以便未来扩展）。

	case 521:
		// 反转自身能力下降状态为能力提升（Effect_521）
		// 规则：playerBattleLv 中 <0 的弱化全部取反为对应的正值，≥0 的保持不变。
		if playerBattleLv != nil {
			for i := 0; i < 6; i++ {
				cur := int(playerBattleLv[i])
				if cur < 0 {
					cur = -cur
					if cur > maxStatStage {
						cur = maxStatStage
					}
					playerBattleLv[i] = int8(cur)
				}
			}
		}

	case 568:
		// 解除自身能力下降状态，若解除成功则若干回合内躲避所有攻击（Effect_568）
		// SideEffectArg: m（成功后躲避所有攻击的回合数）
		cleared := false
		if playerBattleLv != nil {
			for i := 0; i < 6; i++ {
				if playerBattleLv[i] < 0 {
					playerBattleLv[i] = 0
					cleared = true
				}
			}
		}
		if cleared && playerStatus != nil {
			rounds := 1
			if len(args) >= 1 && args[0] > 0 {
				rounds = args[0]
			}
			if rounds < 1 {
				rounds = 1
			}
			if rounds > 10 {
				rounds = 10
			}
			// 暂时复用疲惫状态索引，表示在这若干回合内“特殊保护”生效；
			// 具体闪避/减伤逻辑可在 battle 层根据该状态扩展。
			playerStatus[StatusIndexFatigue] = byte(rounds)
		}

	case 38:
		// 降低对方 n 点体力（btl_Max 相关：按最大体力比例的固定值）
		// SideEffectArg: n 表示扣减值，常见为 10/25/30 等，多为 n% 最大体力
		n := 0
		if len(args) >= 1 {
			n = args[0]
		}
		if n <= 0 || enemyHP == nil || enemyMaxHP == 0 {
			break
		}
		d := enemyMaxHP * uint32(n) / 100
		if d == 0 {
			d = uint32(n)
		}
		if d > *enemyHP {
			d = *enemyHP
		}
		*enemyHP -= d

	case 39:
		// 减少对方所有技能的 PP 值：命中时有 n% 几率，使对手全部技能 PP 各减少 m 点。
		// Go 版核心战斗逻辑在 BattleState 中维护 PlayerSkillPP/EnemySkillPP，
		// 这里暂不直接修改 PP 数组，而是只解析参数并通过注释约定：
		// - args[0]: 触发几率（百分比，缺省 100）
		// - args[1]: 每个技能减少的 PP 点数（缺省 1）
		// 实际 PP 扣减应在 handlers.go 中，结合 BattleState.PlayerSkillPP/EnemySkillPP 实现。
		_ = args

	case 63:
		// 将能力下降状态反馈给对手：己方 battleLv 中 <0 的项，复制到对方对应项
		if playerBattleLv != nil && enemyBattleLv != nil {
			for i := 0; i < 6; i++ {
				p := int(playerBattleLv[i])
				if p < 0 {
					cur := int(enemyBattleLv[i])
					cur += p
					if cur < minStatStage {
						cur = minStatStage
					}
					enemyBattleLv[i] = int8(cur)
				}
			}
		}

	case 79:
		// 损失1/2体力，提升自身能力；SideEffectArg 可多组 stat stages
		if playerHP != nil && playerMaxHP > 0 {
			loss := playerMaxHP / 2
			if loss > *playerHP {
				loss = *playerHP
			}
			*playerHP -= loss
		}
		// 提升能力：默认攻防特攻特防速度各+1
		if playerBattleLv != nil {
			stages := 1
			if len(args) >= 1 && args[0] > 0 {
				stages = args[0]
			}
			for i := 0; i < 5; i++ {
				cur := int(playerBattleLv[i])
				cur += stages
				if cur > maxStatStage {
					cur = maxStatStage
				}
				playerBattleLv[i] = int8(cur)
			}
		}

	case 80:
		// 损失1/2体力，给予对方等同伤害
		if playerHP != nil && enemyHP != nil && playerMaxHP > 0 {
			loss := playerMaxHP / 2
			if loss > *playerHP {
				loss = *playerHP
			}
			*playerHP -= loss
			dmg := loss
			if dmg > *enemyHP {
				dmg = *enemyHP
			}
			*enemyHP -= dmg
		}

	case 85:
		// 将对手的能力提升效果转化到自身：把对手 battleLv 中的正向等级转移到己方，
		// 同时清除对手对应的正向能力等级（对手的强化消失）。
		// 适用于“圣洁光芒”等技能，当前不使用 SideEffectArg。
		if playerBattleLv != nil && enemyBattleLv != nil {
			for i := 0; i < 6; i++ {
				boost := int(enemyBattleLv[i])
				if boost > 0 {
					cur := int(playerBattleLv[i]) + boost
					if cur > maxStatStage {
						cur = maxStatStage
					}
					playerBattleLv[i] = int8(cur)
					// 对手失去对应的强化
					enemyBattleLv[i] = 0
				}
			}
		}

	case 74:
		// 10%中毒、10%烧伤、10%冻伤（剩下70%无效果）
		if !statusImmune {
			r := rand.Intn(100)
			if r < 10 && enemyStatus != nil && enemyStatus[StatusIndexPoison] == 0 {
				enemyStatus[StatusIndexPoison] = randomDoTRounds()
				applyNewStatusDamageOnce(enemyHP, enemyMaxHP)
			} else if r < 20 && enemyStatus != nil && enemyStatus[StatusIndexBurn] == 0 {
				enemyStatus[StatusIndexBurn] = randomDoTRounds()
				applyNewStatusDamageOnce(enemyHP, enemyMaxHP)
			} else if r < 30 && enemyStatus != nil && enemyStatus[StatusIndexFreeze] == 0 {
				enemyStatus[StatusIndexFreeze] = randomDoTRounds()
				applyNewStatusDamageOnce(enemyHP, enemyMaxHP)
			}
		}

	case 99:
		// n% 令对手混乱（本回合无法行动，复用畏缩）
		if defenderControlImmune {
			break
		}
		if !statusImmune {
			chance := 10
			if len(args) >= 1 {
				chance = args[0]
			}
			if rand.Intn(100) < chance && enemyStatus != nil {
				enemyStatus[StatusIndexFear] = 1
			}
		}

	case 75:
		// 10%麻痹、10%睡眠、10%害怕
		if defenderControlImmune {
			break
		}
		if !statusImmune && enemyStatus != nil {
			r := rand.Intn(100)
			if r < 10 && enemyStatus[StatusIndexParalysis] == 0 {
				enemyStatus[StatusIndexParalysis] = randomStatusRounds()
			} else if r < 20 && enemyStatus[StatusIndexSleep] == 0 {
				enemyStatus[StatusIndexSleep] = randomStatusRounds()
			} else if r < 30 {
				enemyStatus[StatusIndexFear] = randomStatusRounds()
			}
		}

	case 94:
		// n% 令对方石化（本回合无法行动，类似畏缩）
		if defenderControlImmune {
			break
		}
		if !statusImmune {
			chance := 10
			if len(args) >= 1 {
				chance = args[0]
			}
			if rand.Intn(100) < chance && enemyStatus != nil {
				enemyStatus[StatusIndexFear] = 1 // 石化用畏缩表示，1回合
			}
		}

	case 114:
		// 命中后易燃：以 SideEffectArg=p 的几率给目标附加“易燃”状态，处于易燃时火系伤害额外放大。
		// 约定：args[0]=几率(百分比)，args[1]=持续回合数（内部回合数，若缺省则默认 3 回合）。
		if statusImmune {
			break
		}
		chance := 100
		rounds := 3
		if len(args) >= 1 && args[0] > 0 {
			chance = args[0]
		}
		if len(args) >= 2 && args[1] > 0 {
			rounds = args[1]
		}
		if rounds < 1 {
			rounds = 1
		}
		if rand.Intn(100) < chance && enemyStatus != nil {
			// 若目标已处于易燃，则覆盖为新的持续回合数
			enemyStatus[StatusIndexFlammable] = byte(rounds)
		}

	case 121:
		// 同属性时几率麻痹：
		// 若技能属性与目标任一属性相同，则以 SideEffectArg=p 的几率令目标麻痹。
		if defenderControlImmune || statusImmune {
			break
		}
		if enemyStatus == nil || enemyStatus[StatusIndexParalysis] != 0 {
			break
		}
		paralysisChance := 0
		if len(args) >= 1 && args[0] > 0 {
			paralysisChance = args[0]
		}
		if paralysisChance <= 0 {
			break
		}
		sameType := false
		if skill != nil {
			if p := gamepets.GetInstance().Get(defenderPetID); p != nil {
				if skill.Type == p.Type || (p.Type2 > 0 && skill.Type == p.Type2) {
					sameType = true
				}
			}
		}
		if sameType && rand.Intn(100) < paralysisChance {
			enemyStatus[StatusIndexParalysis] = randomStatusRounds()
		}

	case 124:
		// 命中后随机降低对方某项能力：
		// SideEffectArg = [chance, delta]
		if enemyBattleLv == nil {
			break
		}
		chance := 0
		delta := 1
		if len(args) >= 1 {
			chance = args[0]
		}
		if len(args) >= 2 && args[1] != 0 {
			delta = args[1]
		}
		if chance <= 0 {
			break
		}
		if rand.Intn(100) >= chance {
			break
		}
		// 从 0~5 随机选择一项能力
		stat := rand.Intn(6)
		steps := delta
		if steps < 0 {
			steps = -steps
		}
		cur := int(enemyBattleLv[stat])
		cur -= steps
		if statDropImmune && cur < 0 {
			cur = 0
		} else if cur < minStatStage {
			cur = minStatStage
		}
		enemyBattleLv[stat] = int8(cur)

	case 422:
		// 附加所造成伤害值 X% 的固定伤害（对应 SideEffect ID 422）
		// 规则：先按普通流程结算 damage（包括多段伤害折算后的总伤），
		// 然后在该 damage 基础上追加一段固定伤害：extra = damage * X%。
		percent := 0
		if len(args) >= 1 {
			percent = args[0]
		}
		if percent <= 0 {
			break
		}
		if enemyHP != nil && damage > 0 {
			extra := uint32(math.Floor(float64(damage) * float64(percent) / 100.0))
			if extra == 0 {
				extra = 1
			}
			if extra > *enemyHP {
				extra = *enemyHP
			}
			*enemyHP -= extra
		}
	}

	return gainHP, recoilDamage
}
