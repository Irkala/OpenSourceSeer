# -*- coding: utf-8 -*-
"""从客户端成就 XML 提取 SpeNameBonus -> title，生成与游戏内一致的 titles.json"""
import re
import json
import os

bin_path = os.path.join(os.path.dirname(__file__), '..', '..', '核心', 'scripts', '_assets', '40_com.robot.core.config.xml.AchieveXMLInfo_xmlClass.bin')
out_path = os.path.join(os.path.dirname(__file__), '..', 'data', 'titles.json')

with open(bin_path, 'rb') as f:
    raw = f.read()
text = raw.decode('utf-8', errors='ignore')
pat = re.compile(r'SpeNameBonus="(\d+)"[^>]*title="([^"]*)"')
pairs = []
seen = set()
for m in pat.finditer(text):
    sid, name = m.group(1), m.group(2)
    id_ = int(sid)
    name = name.replace('|', '')
    if id_ not in seen:
        seen.add(id_)
        pairs.append((id_, name))
pairs.sort(key=lambda x: x[0])
out = [{'id': p[0], 'name': p[1]} for p in pairs]
out.insert(0, {'id': 0, 'name': '无称号'})
with open(out_path, 'w', encoding='utf-8') as f:
    json.dump(out, f, ensure_ascii=False, indent=2)
print('Wrote %d titles to %s' % (len(out), out_path))
