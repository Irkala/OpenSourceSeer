# 称号列表 titles.json

本文件供 GM 后台「发放称号」下拉使用，**ID 与名称必须与客户端成就 XML 一致**，否则会出现“GM 里选的名称和玩家实际获得的称号对不上”的问题。

- **来源**：客户端 `核心/scripts/_assets/40_com.robot.core.config.xml.AchieveXMLInfo_xmlClass.bin` 中的 `Rule` 的 `SpeNameBonus`（称号 ID）与 `title`（显示名）。
- **重新生成**：若客户端更新了成就/称号，可在项目根目录执行：
  ```bash
  cd golang_version && python scripts/extract_titles.py
  ```
  会从上述资源中解析所有称号并覆盖 `data/titles.json`。重启 GM 后端后即可生效。
