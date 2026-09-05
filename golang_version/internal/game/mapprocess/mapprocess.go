package mapprocess

import (
	"sync"

	"github.com/seer-game/golang-version/internal/core/logger"
	"github.com/seer-game/golang-version/internal/server/gameserver"
)

// MapProcessHandler 地图处理器接口
// 每个地图可以实现自己的初始化逻辑
type MapProcessHandler interface {
	// Init 地图初始化，在玩家进入地图时调用
	Init(ctx *gameserver.HandlerContext, mapID int)
	// Destroy 地图销毁，在玩家离开地图时调用（可选实现）
	Destroy(ctx *gameserver.HandlerContext, mapID int)
}

// MapProcessRegistry 地图处理器注册表
type MapProcessRegistry struct {
	handlers map[int]MapProcessHandler
}

var (
	registry *MapProcessRegistry
	once     sync.Once
)

// GetRegistry 获取地图处理器注册表单例
func GetRegistry() *MapProcessRegistry {
	once.Do(func() {
		registry = &MapProcessRegistry{
			handlers: make(map[int]MapProcessHandler),
		}
		// 注册所有地图处理器
		registerAllHandlers()
	})
	return registry
}

// Register 注册地图处理器
func (r *MapProcessRegistry) Register(mapID int, handler MapProcessHandler) {
	if handler == nil {
		logger.Warning("尝试注册空的地图处理器: MapID=%d", mapID)
		return
	}
	r.handlers[mapID] = handler
	logger.Info("注册地图处理器: MapID=%d", mapID)
}

// GetHandler 获取指定地图的处理器
func (r *MapProcessRegistry) GetHandler(mapID int) MapProcessHandler {
	return r.handlers[mapID]
}

// ProcessMapInit 处理地图初始化
func (r *MapProcessRegistry) ProcessMapInit(ctx *gameserver.HandlerContext, mapID int) {
	handler := r.GetHandler(mapID)
	if handler != nil {
		logger.Info("执行地图初始化: UID=%d MapID=%d", ctx.UserID, mapID)
		handler.Init(ctx, mapID)
	} else {
		// 没有特定处理器时，执行默认初始化逻辑
		defaultMapInit(ctx, mapID)
	}
}

// ProcessMapDestroy 处理地图销毁
func (r *MapProcessRegistry) ProcessMapDestroy(ctx *gameserver.HandlerContext, mapID int) {
	handler := r.GetHandler(mapID)
	if handler != nil {
		logger.Info("执行地图销毁: UID=%d MapID=%d", ctx.UserID, mapID)
		handler.Destroy(ctx, mapID)
	}
}

// defaultMapInit 默认地图初始化逻辑
func defaultMapInit(ctx *gameserver.HandlerContext, mapID int) {
	// 默认情况下不执行任何操作
	// 可以根据需要添加通用的初始化逻辑
}

// registerAllHandlers 注册所有地图处理器
func registerAllHandlers() {
	r := GetRegistry()

	// 注册地图484处理器
	r.Register(484, &MapProcess484{})

	// 注册地图677处理器
	r.Register(677, &MapProcess677{})

	// 可以继续添加更多地图处理器...
	// r.Register(XXX, &MapProcessXXX{})
}
