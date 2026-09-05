# 地图处理系统快速开始指南

## 快速添加新地图功能

### 步骤1：查看前端配置文件

查看对应的前端地图配置文件：
```
D:\kaiyuanhao\reseer2.5\导入\scripts\com\robot\app\mapProcess\MapProcess_XXX.as
```

重点关注 `init()` 方法中的：
- `SocketConnection.send(1022, xxx)` - 统计命令
- NPC交互逻辑
- 面板打开逻辑
- 地图元素显示/隐藏逻辑

### 步骤2：创建地图处理器文件

创建文件 `map_XXX.go`（XXX为地图ID）：

```go
package mapprocess

import (
	"encoding/binary"
	"fmt"

	"github.com/seer-game/golang-version/internal/core/logger"
	"github.com/seer-game/golang-version/internal/server/gameserver"
)

// MapProcessXXX 地图XXX的处理器
type MapProcessXXX struct{}

func (m *MapProcessXXX) Init(ctx *gameserver.HandlerContext, mapID int) {
	logger.Info(fmt.Sprintf("地图%d初始化: UID=%d", mapID, ctx.UserID))

	// 发送统计命令（如果有）
	// statBody := make([]byte, 4)
	// binary.BigEndian.PutUint32(statBody, 统计参数)
	// ctx.GameServer.SendResponse(ctx.ClientData, 1022, ctx.UserID, ctx.SeqID, statBody)
}

func (m *MapProcessXXX) Destroy(ctx *gameserver.HandlerContext, mapID int) {
	// 清理逻辑（如果需要）
}
```

### 步骤3：注册地图处理器

在 `mapprocess.go` 的 `registerAllHandlers()` 函数中添加：

```go
r.Register(XXX, &MapProcessXXX{})
```

### 步骤4：测试

进入对应地图，查看日志确认地图处理器是否被调用。

## 常见模式

### 发送统计命令

```go
statBody := make([]byte, 4)
binary.BigEndian.PutUint32(statBody, 86071040)
ctx.GameServer.SendResponse(ctx.ClientData, 1022, ctx.UserID, ctx.SeqID, statBody)
```

### 发送多个统计命令

```go
stats := []uint32{86071040, 86067372}
for _, stat := range stats {
	statBody := make([]byte, 4)
	binary.BigEndian.PutUint32(statBody, stat)
	ctx.GameServer.SendResponse(ctx.ClientData, 1022, ctx.UserID, ctx.SeqID, statBody)
}
```

## 已实现的地图

- ✅ 地图484：卡修斯相关地图
- ✅ 地图677：异能王相关地图

## 待实现的地图

参考 `缺失文件列表.txt`，可以逐步添加：
- 地图1：传送舱
- 地图10001-10116：神秘洞穴系列
- 地图662-700：其他地图
- 更多...

## 注意事项

1. 地图515（教程地图）不会执行地图处理器
2. 大部分UI交互逻辑在前端处理，后端主要负责服务器验证和数据发送
3. 统计命令1022的参数含义需要参考前端代码或文档
4. 保持代码简洁，避免阻塞主流程
