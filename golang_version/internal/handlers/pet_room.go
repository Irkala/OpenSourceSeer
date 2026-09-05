package handlers

import (
	"encoding/binary"
	"fmt"

	"github.com/seer-game/golang-version/internal/core/logger"
	"github.com/seer-game/golang-version/internal/core/userdb"
	"github.com/seer-game/golang-version/internal/server/gameserver"
)

// handlePetRoomShow CMD 2323 房间精灵展示/收回（PET_ROOM_SHOW）
// 请求：count(4) + [catchTime(4)+petID(4)]*count
// - count==0：清空展示列表
// - 1<=count<=5：保存这几只精灵的 catchTime，作为房间中展示的精灵
// 响应：writeUInt32BE(0)
func handlePetRoomShow(ctx *gameserver.HandlerContext) {
	user := ctx.GameServer.GetOrCreateUser(ctx.UserID)

	if len(ctx.Body) < 4 {
		logger.Warning("[2323] PET_ROOM_SHOW 包体过短")
		body := make([]byte, 4)
		binary.BigEndian.PutUint32(body[0:4], 0)
		ctx.GameServer.SendResponse(ctx.ClientData, 2323, ctx.UserID, ctx.SeqID, body)
		return
	}

	count := int(binary.BigEndian.Uint32(ctx.Body[0:4]))
	if count < 0 {
		count = 0
	}
	if count > 5 {
		// 协议上允许传更多，但服务端仅保存前 5 只，避免超出展示上限
		logger.Info(fmt.Sprintf("[2323] 收到展示精灵超过 5 只，截断为 5：uid=%d count=%d", ctx.UserID, count))
		count = 5
	}

	newRoomPets := make([]int, 0, count)
	offset := 4
	for i := 0; i < count; i++ {
		if offset+8 > len(ctx.Body) {
			break
		}
		catchTime := int(binary.BigEndian.Uint32(ctx.Body[offset : offset+4]))
		// petID := binary.BigEndian.Uint32(ctx.Body[offset+4 : offset+8]) // 目前只需要 catchTime
		offset += 8
		if catchTime == 0 {
			continue
		}
		newRoomPets = append(newRoomPets, catchTime)
	}

	// 持久化到 GameData.RoomPets
	if user.RoomPets == nil {
		user.RoomPets = []int{}
	}
	user.RoomPets = newRoomPets

	if ctx.GameServer.UserDB != nil {
		ctx.GameServer.UserDB.SaveGameData(ctx.UserID, user)
	}

	logger.Info(fmt.Sprintf("[2323] 更新房间展示精灵: uid=%d count=%d", ctx.UserID, len(user.RoomPets)))

	// 构建与 2324 相同格式的响应：count(4) + [id(4)+catchTime(4)+skinID(4)+shiny(4)]...
	// 这样 RoomPetManager.onList 可以统一解析 PET_ROOM_LIST / PET_ROOM_SHOW
	if user.StoragePets == nil {
		user.StoragePets = []userdb.Pet{}
	}
	display := make([]userdb.Pet, 0, len(user.RoomPets))
	for _, ct := range user.RoomPets {
		for _, p := range user.StoragePets {
			if p.CatchTime == ct {
				display = append(display, p)
				break
			}
		}
	}
	displayCount := uint32(len(display))
	body := make([]byte, 4+int(displayCount)*16)
	binary.BigEndian.PutUint32(body[0:4], displayCount)
	off := 4
	for _, p := range display {
		binary.BigEndian.PutUint32(body[off:off+4], uint32(p.ID))
		off += 4
		binary.BigEndian.PutUint32(body[off:off+4], uint32(p.CatchTime))
		off += 4
		binary.BigEndian.PutUint32(body[off:off+4], 0) // skinID
		off += 4
		shiny := uint32(0)
		if p.Shiny != 0 {
			shiny = 1
		}
		binary.BigEndian.PutUint32(body[off:off+4], shiny)
		off += 4
	}

	ctx.GameServer.SendResponse(ctx.ClientData, 2323, ctx.UserID, ctx.SeqID, body[:off])
}

