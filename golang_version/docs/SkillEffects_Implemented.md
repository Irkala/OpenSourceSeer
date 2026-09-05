# 技能效果实现文档

本文档记录前 1000 号精灵所用技能中已实现的 SideEffect ID，及其实现位置与逻辑说明。

## 实现文件

- **效果逻辑**：`internal/game/skills/effects.go`（`ApplyEffect` 函数）
- **战斗流程与伤害修正**：`internal/handlers/handlers.go`（2405 回合结算）
- **战斗状态**：`internal/server/gameserver/gameserver.go`（`BattleState` 结构体）
- **技能数据**：`data/skills.xml`（`SideEffect`、`SideEffectArg`）

---

## 已实现效果列表

| EffectID | 官方描述 | 实现位置 | 逻辑说明 |
|----------|----------|----------|----------|
| 1 | 吸血 | effects.go | 恢复造成伤害的 N%（默认 50%） |
| 2 | 降低对方能力等级 | effects.go | Args: stat stages |
| 3 | 提升自身能力等级 | effects.go | 多组 stat chance stages |
| 4 | 提升/降低能力（含袭天舞特例） | effects.go | 袭天舞：速度-1 改为降对手速度 |
| 5 | 降低对方或提升自己能力 | effects.go | stages 负数=降对手，正数=升自己 |
| 6 | 反伤 | effects.go | 自身受到伤害的 1/N |
| 7 | 同生共死 | effects.go | 将对方血量压至与己方相同 |
| 8 | 手下留情 | handlers.go | 伤害不致死，留 1 血 |
| 9 | 愤怒 | effects.go | 受伤在 [min,max] 时攻击+1 |
| 10 | 麻痹 | effects.go | status[0]，BOSS 可免疫 |
| 11 | 中毒 | effects.go | status[1]，每回合 1/8 最大HP |
| 12 | 烧伤 | effects.go | status[2] |
| 13 | 寄生种子 | effects.go | status[3/4]，草系免疫 |
| 14 | 冻伤 | effects.go | status[5] |
| 15 | 畏缩 | effects.go | status[6] 本回合无法行动 |
| 16 | 睡眠 | effects.go | status[8] |
| 20 | 疲惫（自身） | effects.go | status[7] 下回合无法行动 |
| 22 | 令对手疲惫 | effects.go | n% 概率，m 回合 |
| 28 | 削减对方当前体力 1/N | effects.go | 如海洋之心 |
| 29 | 额外固定伤害 | effects.go | Args[0] 固定值 |
| 30 | 后出手威力×2 | handlers.go | 伤害计算时 |
| 31 | 多段攻击 | handlers.go | min~max 次折算倍数 |
| 32 | 暴击率 +1/16 持续 N 回合 | handlers.go | PlayerCritBonusRounds |
| 33 | 清除对手能力提升 | effects.go | 仅清正强化 |
| 35 | 惩罚 | handlers.go | 对方正能力等级×20 附加伤害 |
| 21 | m~n 回合反弹对手伤害 1/k | handlers.go | PlayerReflectRounds/Denom |
| 36 | 秒杀 | handlers.go | n% 概率，魔狮迪露免疫 |
| 37 | 自身体力<1/n 时威力 m 倍 | handlers.go | 伤害公式中判定 |
| 38 | 降低对方 n 点体力 | effects.go | 按最大体力 n% 扣减 |
| 39 | （未实现） | - | PP 削减+无法行动，需技能槽逻辑 |
| 40 | 先出手威力×2 | handlers.go | 伤害计算时 |
| 41 | m~n 回合火系伤害减半 | handlers.go | PlayerFireResistRounds |
| 42 | m~n 回合电系伤害×2 | handlers.go | PlayerElectricPowerRounds |
| 43 | 回复自身最大体力 1/N | effects.go | 满血也飘字 |
| 44 | n 回合特攻伤害减半 | handlers.go | PlayerSpecDefRounds |
| 45 | （未实现） | - | 防御力与对手相同，较复杂 |
| 49 | 抵挡 n 点伤害 | handlers.go | PlayerDamageAbsorb 吸收盾 |
| 46 | 抵挡 n 次攻击 | handlers.go | PlayerShieldRounds |
| 47 | 免疫能力下降 N 回合 | handlers.go | PlayerStatDropImmuneRounds |
| 48 | n 回合免疫异常 | handlers.go | PlayerStatusImmuneRounds |
| 50 | n 回合物攻伤害减半 | handlers.go | PlayerPhysDefRounds |
| 52 | 回避 | handlers.go | 先手+速度更快→敌方下次必 miss |
| 53 | n 回合伤害×m | handlers.go | 同 effect 90 |
| 54 | n 回合对方所受伤害 1/m | handlers.go | EnemyDamageHalvedRounds |
| 57 | n 回合每用技能回血 1/m | handlers.go | PlayerHealPerSkillRounds |
| 58 | 下 N 回合必定暴击 | handlers.go | PlayerCritBuffRounds |
| 60 | 多回合固定伤害 | handlers.go | EnemyFixedDotDamage/Rounds |
| 61 | 威力随机 50~150 | handlers.go | 伤害公式中覆盖 power |
| 62 | 镇魂歌 | effects.go | 占位，谱尼免疫 |
| 63 | 能力下降反馈给对手 | effects.go | 己方负 battleLv 复制到对手 |
| 64 | 烧伤/冻伤/中毒时伤害加倍 | handlers.go | 伤害公式中判定 |
| 65 | n 回合某属性技能威力×m | handlers.go | PlayerTypePowerRounds/Type/Mul |
| 66 | 击败对手时回复 1/n 最大体力 | handlers.go | 击杀判定后 |
| 68 | 致死留 1 血 | handlers.go | PlayerEndureRounds |
| 70 | 威力随机 140~220 | handlers.go | 伤害公式中覆盖 power |
| 82 | 目标为雄性200%/雌性50% | handlers.go | 通过精灵 Gender 判定 |
| 83 | 雄性必先手/雌性必暴击 | handlers.go | 使用 FirstPriorityRounds / CritBuff |
| 74 | 10%中毒/10%烧伤/10%冻伤 | effects.go | 三选一随机 |
| 75 | 10%麻痹/10%睡眠/10%害怕 | effects.go | 三选一随机 |
| 76 | m% 几率 n 回合固定伤害 | handlers.go | EnemyFixedDot |
| 77 | n 回合每用技能恢复 m 点体力 | handlers.go | PlayerHealPerSkillPoints |
| 82 | 目标为雄性200%/雌性50% | handlers.go | 同上 |
| 83 | 雄性必先手/雌性必暴击 | handlers.go | 同上 |
| 79 | 损失 1/2 体力提升能力 | effects.go | 攻防特攻特防速度+stages |
| 80 | 损失 1/2 体力给予等同伤害 | effects.go | 自损+敌损 |
| 81 | 下 n 回合必中 | handlers.go | PlayerMustHitRounds |
| 87 | 恢复自身所有 PP | handlers.go | 写回 PlayerSkillPP |
| 88 | n% 伤害 m 倍 | handlers.go | 伤害计算时 |
| 89 | n 回合造成伤害 1/m 吸血 | handlers.go | PlayerVampRounds |
| 90 | n 回合伤害×m | handlers.go | PlayerDmgMulRounds |
| 93 | n% 额外 m 点固定伤害 | handlers.go | 伤害计算时 |
| 94 | n% 石化 | effects.go | 复用 status[6] 畏缩 |
| 95 | 对手睡眠时暴击率 +n/16 | handlers.go | critRate 加成 |
| 96 | 对手烧伤时威力×2 | handlers.go | 伤害计算时 |
| 97 | 对手冻伤时威力×2 | handlers.go | 伤害计算时 |
| 98 | n 回合内对雄性伤害×m | handlers.go | Player/EnemyMaleDmgMulRounds |
| 99 | n% 混乱 | effects.go | 复用畏缩 status[6] |
| 143 | 反转对手能力提升为下降 | effects.go | 正变负 |
| 422 | 附加造成伤害 X% 固定伤害 | effects.go | extra = damage * X% |
| 521 | 反转自身能力下降为提升 | effects.go | 负变正 |
| 568 | 解除能力下降并躲避 N 回合 | effects.go | 解除后 status[7] |

---

## 多回合效果递减

所有 `*Rounds` 字段在每回合结束时统一递减，位置：`handlers.go` 中 `afterEnemyTurn` 后的回合结束块。

---

## PvP 同步

多回合效果在 PvP 时需镜像同步：`opponentBattle.Player* = battle.Enemy*`，`opponentBattle.Enemy* = battle.Player*`。
## 尚未在 Go 端实现、但被 ≤1000 号精灵使用的效果（待实现）

> 基于 `list_unimplemented_effects_le1000_accurate.py` 的扫描结果，仅列出 EffectID 和出现次数。
> 具体语义需结合老服 Lua 实现或策划案进一步确认。

| EffectID | 使用次数 | 备注 |
|---------|----------|------|
| 69  | 1 | 仅被 `药剂反噬` 使用，`info="似乎有种异样的变化"`，`SideEffectArg="5"`，具体效果待确认 |
| 103 | 1 | 被 `龙魂冰慑` 使用，`SideEffectArg="10"`，无描述 |
| 104 | 1 | 被 `浩劫之力` 使用（变化技），`SideEffectArg="5 50"`，无描述 |
| 125 | 1 | 多个“变得更坚韧/受到伤害减少/抵抗异常提升”的 Buff 技能使用，`SideEffectArg=[rounds,value]`，推测为多回合减伤/抗性强化 |
| 126 | 1 | `斗志高昂`/`灵动九天` 使用，`info` 提示“逐渐变强/攻速慢慢提升”，`SideEffectArg="3 1"`，推测为多回合成长类能力提升 |
| 132 | 1 | 多个高威力攻技使用，无统一描述，推测为额外收割/破防类伤害加成，需对照老服 |
| 180 | 1 | 攻技+变化技共用；变化技 `邪灵降世` 描述“使对手失去了所有的持续效果”，推测为清除对手全部多回合 Buff/Debuff |
| 195 | 1 | 被多种高威力进攻技能使用，无文本描述，具体机制待确认 |
| 439 | 1 | `森罗万象` 使用，`info="抵御异常状态和能力下降的能力提升了"`，`SideEffectArg="5 200"`，推测为同时免疫异常与能力下降若干回合 |
| 451 | 1 | `石化凝视` 描述“随机使对手进入烧伤，冻伤，中毒中的一种”，其它挂在攻技/属性技上，`SideEffectArg` 多为 100，推测为 n% 几率随机三选一 DOT 异常 |
| 455 | 1 | 被多种高威力或高优先攻技使用，`SideEffectArg` 多为 `1 1`/`2 1`，无描述，具体效果待确认 |
| 478 | 1 | 多个“封印/时停”相关技能使用，如 `退魔封灵`/`远古封印`/`幻术封印`/`时空静止`，推测为在若干回合内封印对手技能或行动 |
| 511 | 1 | 高威力攻技使用，`SideEffectArg` 形如 `[20 2 40]`，无描述，推测为有概率附加大量额外伤害或特殊效果 |
| 1044 | 1 | 仅被 `碎梦袭` 使用，`SideEffectArg="2"`，无描述，具体效果待确认 |