package mapprocess

import (
	"encoding/binary"

	"github.com/seer-game/golang-version/internal/core/logger"
	"github.com/seer-game/golang-version/internal/server/gameserver"
)

// MapProcess484 地图484（卡修斯相关地图）的处理器
type MapProcess484 struct{}

// Init 地图484初始化
// 根据前端代码，需要发送统计命令：
// - SocketConnection.send(1022, 86071040)
// - SocketConnection.send(1022, 86067372)
func (m *MapProcess484) Init(ctx *gameserver.HandlerContext, mapID int) {
	logger.Info("地图484初始化: UID=%d", ctx.UserID)

	// 发送统计命令 1022, 86071040
	statBody1 := make([]byte, 4)
	binary.BigEndian.PutUint32(statBody1, 86071040)
	ctx.GameServer.SendResponse(ctx.ClientData, 1022, ctx.UserID, ctx.SeqID, statBody1)

	// 发送统计命令 1022, 86067372
	statBody2 := make([]byte, 4)
	binary.BigEndian.PutUint32(statBody2, 86067372)
	ctx.GameServer.SendResponse(ctx.ClientData, 1022, ctx.UserID, ctx.SeqID, statBody2)

	logger.Info("地图484统计命令已发送: UID=%d", ctx.UserID)
}

// Destroy 地图484销毁（当前无需特殊处理）
func (m *MapProcess484) Destroy(ctx *gameserver.HandlerContext, mapID int) {
	// 地图484离开时无需特殊处理
}
