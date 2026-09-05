# 地图处理系统 (MapProcess)

## 概述

地图处理系统用于实现每个地图特定的初始化逻辑和功能。该系统参考前端 ActionScript 地图配置文件（`MapProcess_XXX.as`）的设计，在后端实现相应的地图处理逻辑。

## 架构设计

### 核心组件

1. **MapProcessHandler 接口**：定义地图处理器的标准接口
   - `Init(ctx, mapID)`：地图初始化方法
   - `Destroy(ctx, mapID)`：地图销毁方法（可选）

2. **MapProcessRegistry**：地图处理器注册表
   - 单例模式，管理所有地图处理器
   - 提供注册和查找功能

3. **具体地图处理器**：每个地图实现自己的处理器
   - `MapProcess484`：地图484（卡修斯相关）
   - `MapProcess677`：地图677（异能王相关）
   - 更多地图处理器可以逐步添加...

## 使用方法

### 1. 创建新的地图处理器

在 `mapprocess` 包中创建新文件，例如 `map_XXX.go`：

```go
package mapprocess

import (
	"github.com/seer-game/golang-version/internal/server/gameserver"
)

// MapProcessXXX 地图XXX的处理器
type MapProcessXXX struct{}

func (m *MapProcessXXX) Init(ctx *gameserver.HandlerContext, mapID int) {
	// 实现地图初始化逻辑
	// 例如：发送统计命令、初始化NPC、设置地图状态等
}

func (m *MapProcessXXX) Destroy(ctx *gameserver.HandlerContext, mapID int) {
	// 实现地图销毁逻辑（可选）
	// 例如：清理资源、保存状态等
}
```

### 2. 注册地图处理器

在 `mapprocess.go` 的 `registerAllHandlers()` 函数中添加注册：

```go
func registerAllHandlers() {
	r := GetRegistry()
	
	// 注册新地图处理器
	r.Register(XXX, &MapProcessXXX{})
}
```

### 3. 实现地图特定逻辑

根据前端 `MapProcess_XXX.as` 文件中的 `init()` 方法，实现相应的后端逻辑：

#### 常见功能

1. **发送统计命令**（对应前端的 `SocketConnection.send(1022, xxx)`）：
```go
statBody := make([]byte, 4)
binary.BigEndian.PutUint32(statBody, 统计参数)
ctx.GameServer.SendResponse(ctx.ClientData, 1022, ctx.UserID, ctx.SeqID, statBody)
```

2. **初始化NPC交互**：
   - 前端通过事件监听器处理NPC点击
   - 后端可以通过发送特定命令或状态来触发前端行为

3. **设置地图元素显示/隐藏**：
   - 前端通过设置 `visible` 属性控制
   - 后端可以通过发送地图配置数据来控制

4. **打开面板/模块**：
   - 前端通过 `ModuleManager.showModule()` 打开
   - 后端可以发送命令通知客户端打开特定面板

## 示例

### 地图484（卡修斯相关）

前端代码（`MapProcess_484.as`）：
```actionscript
override protected function init() : void {
    SocketConnection.send(1022, 86071040);
    SocketConnection.send(1022, 86067372);
    // ... 其他初始化逻辑
}
```

后端实现（`map_484.go`）：
```go
func (m *MapProcess484) Init(ctx *gameserver.HandlerContext, mapID int) {
    // 发送统计命令
    statBody1 := make([]byte, 4)
    binary.BigEndian.PutUint32(statBody1, 86071040)
    ctx.GameServer.SendResponse(ctx.ClientData, 1022, ctx.UserID, ctx.SeqID, statBody1)
    
    statBody2 := make([]byte, 4)
    binary.BigEndian.PutUint32(statBody2, 86067372)
    ctx.GameServer.SendResponse(ctx.ClientData, 1022, ctx.UserID, ctx.SeqID, statBody2)
}
```

## 集成位置

地图处理系统在 `handlers.go` 的 `handleEnterMap` 函数中调用：

```go
// 执行地图特定的初始化逻辑
if mapId != 515 {  // 教程地图515不执行
    registry := mapprocess.GetRegistry()
    registry.ProcessMapInit(ctx, int(mapId))
}
```

## 注意事项

1. **教程地图515**：不执行地图处理器，因为服务器侧不更新地图状态
2. **统计命令1022**：用于记录玩家行为，参数含义需要参考前端代码或文档
3. **线程安全**：注册表使用单例模式，确保线程安全
4. **性能考虑**：地图处理器应该快速执行，避免阻塞主流程

## 逐步完善

当前已实现：
- ✅ 地图484（卡修斯相关）
- ✅ 地图677（异能王相关）

待实现的地图（参考 `缺失文件列表.txt`）：
- 地图10001-10116系列
- 地图10200-10300系列
- 地图318, 341, 343等
- 地图402-499系列
- 地图662-700系列
- 更多...

可以根据前端配置文件逐步添加对应的后端处理逻辑。
