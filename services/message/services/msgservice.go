package services

import (
	"context"
	"time"

	"im-server/commons/bases"
	"im-server/commons/errs"
	"im-server/commons/gmicro/actorsystem"
	"im-server/commons/pbdefines/pbobjs"
	"im-server/commons/tools"
	"im-server/services/commonservices"
	"im-server/services/commonservices/logs"

	"google.golang.org/protobuf/proto"
)

func SendPrivateMsg(ctx context.Context, senderId, receiverId string, upMsg *pbobjs.UpMsg) (errs.IMErrorCode, string, int64, int64) {
	appkey := bases.GetAppKeyFromCtx(ctx)
	converId := commonservices.GetConversationId(senderId, receiverId, pbobjs.ChannelType_Private)
	//statistic
	commonservices.ReportUpMsg(appkey, pbobjs.ChannelType_Private, 1)
	commonservices.ReportDispatchMsg(appkey, pbobjs.ChannelType_Private, 1)
	//check block user
	blockUsers := GetBlockUsers(appkey, receiverId)
	if blockUsers.CheckBlockUser(senderId) {
		sendTime := time.Now().UnixMilli()
		msgId := tools.GenerateMsgId(sendTime, int32(pbobjs.ChannelType_Private), receiverId)
		return errs.IMErrorCode_MSG_BLOCK, msgId, sendTime, 0
	}
	//check msg interceptor
	if code := commonservices.CheckMsgInterceptor(ctx, senderId, receiverId, pbobjs.ChannelType_Private, upMsg); code != errs.IMErrorCode_SUCCESS {
		sendTime := time.Now().UnixMilli()
		msgId := tools.GenerateMsgId(sendTime, int32(pbobjs.ChannelType_Private), receiverId)
		return code, msgId, sendTime, 0
	}
	msgConverCache := commonservices.GetMsgConverCache(ctx, converId, pbobjs.ChannelType_Private)
	msgId, sendTime, msgSeq := msgConverCache.GenerateMsgId(converId, pbobjs.ChannelType_Private, time.Now().UnixMilli(), upMsg.Flags)
	preMsgId := bases.GetMsgIdFromCtx(ctx)
	if preMsgId != "" {
		msgId = preMsgId
	}
	downMsg4Sendbox := &pbobjs.DownMsg{
		SenderId:       senderId,
		TargetId:       receiverId,
		ChannelType:    pbobjs.ChannelType_Private,
		MsgType:        upMsg.MsgType,
		MsgId:          msgId,
		MsgSeqNo:       msgSeq,
		MsgContent:     upMsg.MsgContent,
		MsgTime:        sendTime,
		Flags:          upMsg.Flags,
		ClientUid:      upMsg.ClientUid,
		IsSend:         true,
		MentionInfo:    upMsg.MentionInfo,
		ReferMsg:       commonservices.FillReferMsg(ctx, upMsg),
		TargetUserInfo: commonservices.GetTargetDisplayUserInfo(ctx, receiverId),
		MergedMsgs:     upMsg.MergedMsgs,
		PushData:       upMsg.PushData,
	}
	//send to sender's other device
	if !commonservices.IsStateMsg(upMsg.Flags) {
		//save msg to sendbox for sender
		//record conversation for sender
		commonservices.Save2Sendbox(ctx, downMsg4Sendbox)
	} else {
		MsgDirect(ctx, senderId, downMsg4Sendbox)
	}

	if bases.GetOnlySendboxFromCtx(ctx) {
		return errs.IMErrorCode_SUCCESS, msgId, sendTime, msgSeq
	}
	commonservices.SubPrivateMsg(ctx, msgId, downMsg4Sendbox)

	downMsg := &pbobjs.DownMsg{
		SenderId:       senderId,
		TargetId:       senderId,
		ChannelType:    pbobjs.ChannelType_Private,
		MsgType:        upMsg.MsgType,
		MsgId:          msgId,
		MsgSeqNo:       msgSeq,
		MsgContent:     upMsg.MsgContent,
		MsgTime:        sendTime,
		Flags:          upMsg.Flags,
		ClientUid:      upMsg.ClientUid,
		MentionInfo:    upMsg.MentionInfo,
		ReferMsg:       commonservices.FillReferMsg(ctx, upMsg),
		TargetUserInfo: commonservices.GetSenderUserInfo(ctx),
		MergedMsgs:     upMsg.MergedMsgs,
		PushData:       upMsg.PushData,
	}

	//check merged msg
	if commonservices.IsMergedMsg(upMsg.Flags) && upMsg.MergedMsgs != nil && len(upMsg.MergedMsgs.Msgs) > 0 {
		bases.AsyncRpcCall(ctx, "merge_msgs", msgId, &pbobjs.MergeMsgReq{
			ParentMsgId: msgId,
			MergedMsgs:  upMsg.MergedMsgs,
		})
	}

	//save history msg
	if commonservices.IsStoreMsg(upMsg.Flags) {
		commonservices.SaveHistoryMsg(ctx, senderId, receiverId, pbobjs.ChannelType_Private, downMsg, 0)
	}

	//dispatch to receiver
	if senderId != receiverId {
		dispatchMsg(ctx, receiverId, downMsg)
	}

	return errs.IMErrorCode_SUCCESS, msgId, sendTime, msgSeq
}

func dispatchMsg(ctx context.Context, receiverId string, msg *pbobjs.DownMsg) {
	data, _ := tools.PbMarshal(msg)
	bases.UnicastRouteWithNoSender(&pbobjs.RpcMessageWraper{
		RpcMsgType:   pbobjs.RpcMsgType_UserPub,
		AppKey:       bases.GetAppKeyFromCtx(ctx),
		Session:      bases.GetSessionFromCtx(ctx),
		Method:       "msg_dispatch",
		RequesterId:  bases.GetRequesterIdFromCtx(ctx),
		ReqIndex:     bases.GetSeqIndexFromCtx(ctx),
		Qos:          bases.GetQosFromCtx(ctx),
		AppDataBytes: data,
		TargetId:     receiverId,
	})
}

func MsgOrNtf(ctx context.Context, targetId string, downMsg *pbobjs.DownMsg) {
	appkey := bases.GetAppKeyFromCtx(ctx)
	userStatus := GetUserStatus(appkey, targetId)
	if userStatus.IsOnline() {
		isNtf := GetUserStatus(appkey, targetId).CheckNtfWithSwitch()
		hasPush := false
		if userStatus.OpenPushSwitch() {
			hasPush = true
			SendPush(ctx, bases.GetRequesterIdFromCtx(ctx), targetId, downMsg)
		}
		if isNtf { //发送通知
			logs.WithContext(ctx).Infof("ntf target_id:%s", targetId)
			rpcNtf := bases.CreateServerPubWraper(ctx, bases.GetRequesterIdFromCtx(ctx), targetId, "ntf", &pbobjs.Notify{
				Type:     pbobjs.NotifyType_Msg,
				SyncTime: downMsg.MsgTime,
			})
			rpcNtf.Qos = 0
			bases.UnicastRouteWithNoSender(rpcNtf)
			bases.UnicastRouteWithCallback(rpcNtf, &SendMsgAckActor{
				appkey:      appkey,
				senderId:    bases.GetRequesterIdFromCtx(ctx),
				targetId:    targetId,
				channelType: downMsg.ChannelType,
				Msg:         downMsg,
				ctx:         ctx,
				IsNotify:    isNtf,
				HasPush:     hasPush,
			}, 5*time.Second)
		} else {
			logs.WithContext(ctx).Infof("msg target_id:%s", targetId)
			rpcMsg := bases.CreateServerPubWraper(ctx, bases.GetRequesterIdFromCtx(ctx), targetId, "msg", downMsg)
			rpcMsg.MsgId = downMsg.MsgId
			rpcMsg.MsgSendTime = downMsg.MsgTime
			bases.UnicastRouteWithCallback(rpcMsg, &SendMsgAckActor{
				appkey:      appkey,
				senderId:    bases.GetRequesterIdFromCtx(ctx),
				targetId:    targetId,
				channelType: downMsg.ChannelType,
				Msg:         downMsg,
				ctx:         ctx,
				IsNotify:    isNtf,
				HasPush:     hasPush,
			}, 5*time.Second)
		}
	} else { //for push
		SendPush(ctx, bases.GetRequesterIdFromCtx(ctx), targetId, downMsg)
	}
}

func getPushLanguage(ctx context.Context, userId string) string {
	appkey := bases.GetAppKeyFromCtx(ctx)
	language := "en_US"
	appinfo, exist := commonservices.GetAppInfo(appkey)
	if exist {
		language = appinfo.PushLanguage
	}
	uSetting := commonservices.GetTargetUserSettings(ctx, userId)
	if uSetting != nil && uSetting.Language != "" {
		language = uSetting.Language
	}
	return language
}

type SendMsgAckActor struct {
	actorsystem.UntypedActor
	appkey      string
	senderId    string
	targetId    string
	channelType pbobjs.ChannelType
	// pushData    *pbobjs.PushData
	Msg      *pbobjs.DownMsg
	ctx      context.Context
	IsNotify bool
	HasPush  bool
}

func (actor *SendMsgAckActor) OnReceive(ctx context.Context, input proto.Message) {
	if rpcMsg, ok := input.(*pbobjs.RpcMessageWraper); ok {
		data := rpcMsg.AppDataBytes
		onlineStatus := &pbobjs.OnlineStatus{}
		err := tools.PbUnMarshal(data, onlineStatus)
		if err == nil {
			logs.WithContext(actor.ctx).Infof("target_id:%s\tonline_type:%d", actor.targetId, onlineStatus.Type)
			if onlineStatus.Type == pbobjs.OnlineType_Offline { //receiver is offline
				RecordUserOnlineStatus(actor.appkey, actor.targetId, false, 0)
				if !actor.HasPush {
					SendPush(actor.ctx, actor.senderId, actor.targetId, actor.Msg)
				}
			}
		}
	}
}

func GetPushData(msg *pbobjs.DownMsg, pushLanguage string) *pbobjs.PushData {
	if msg == nil {
		return nil
	}
	var (
		title  string
		prefix string
	)
	nickName := msg.TargetUserInfo.GetNickname()
	if msg.ChannelType == pbobjs.ChannelType_Group {
		title = msg.GroupInfo.GroupName
		if nickName != "" {
			prefix = nickName + ": "
		}
	} else {
		title = nickName
	}
	retPushData := &pbobjs.PushData{}
	if msg.PushData != nil {
		retPushData = msg.PushData
	}
	if retPushData.Title == "" {
		retPushData.Title = title
	}
	if retPushData.PushText != "" {
		retPushData.PushText = prefix + retPushData.PushText
	} else {
		if msg.MsgType == "jg:text" {
			txtMsg := &commonservices.TextMsg{}
			err := tools.JsonUnMarshal(msg.MsgContent, txtMsg)
			pushText := txtMsg.Content
			charArr := []rune(pushText)
			if len(charArr) > 20 {
				pushText = string(charArr[:20]) + "..."
			}
			if err == nil {
				retPushData.PushText = prefix + pushText
			} else {
				retPushData.PushText = prefix + GetI18nStr(pushLanguage, PlaceholderKey_Text, "[Text]")
			}
		} else if msg.MsgType == "jg:img" {
			retPushData.PushText = prefix + GetI18nStr(pushLanguage, PlaceholderKey_Image, "[Image]")
		} else if msg.MsgType == "jg:voice" {
			retPushData.PushText = prefix + GetI18nStr(pushLanguage, PlaceholderKey_Voice, "[Voice]")
		} else if msg.MsgType == "jg:file" {
			retPushData.PushText = prefix + GetI18nStr(pushLanguage, PlaceholderKey_File, "[File]")
		} else if msg.MsgType == "jg:video" {
			retPushData.PushText = prefix + GetI18nStr(pushLanguage, PlaceholderKey_Video, "[Video]")
		} else if msg.MsgType == "jg:merge" {
			retPushData.PushText = prefix + GetI18nStr(pushLanguage, PlaceholderKey_Merge, "[Merge]")
		} else {
			return nil
		}
	}

	//add internal fields
	retPushData.MsgId = msg.MsgId
	retPushData.SenderId = msg.SenderId
	retPushData.ConverId = msg.TargetId
	retPushData.ChannelType = msg.ChannelType
	return retPushData
}

func (actor *SendMsgAckActor) CreateInputObj() proto.Message {
	return &pbobjs.RpcMessageWraper{}
}
func (actor *SendMsgAckActor) OnTimeout() {

}

func SendPush(ctx context.Context, senderId, receiverId string, msg *pbobjs.DownMsg) {
	appkey := bases.GetAppKeyFromCtx(ctx)
	appInfo, exist := commonservices.GetAppInfo(appkey)
	if exist && appInfo != nil && appInfo.IsOpenPush {
		if !commonservices.IsUndisturbMsg(msg.Flags) {
			pushData := GetPushData(msg, getPushLanguage(ctx, receiverId))
			if pushData != nil {
				//badge
				userStatus := GetUserStatus(appkey, receiverId)
				pushData.Badge = userStatus.BadgeIncr()
				if userStatus.CanPush > 0 {
					pushRpc := bases.CreateServerPubWraper(ctx, senderId, receiverId, "push", pushData)
					bases.UnicastRouteWithNoSender(pushRpc)
				}
			}
		}
	}
}

func ImportPrivateHisMsg(ctx context.Context, senderId, targetId string, msg *pbobjs.UpMsg) {
	msgId := tools.GenerateMsgId(msg.MsgTime, int32(pbobjs.ChannelType_Private), targetId)
	/*
		downMsg4Sendbox := &pbobjs.DownMsg{
			SenderId:    senderId,
			TargetId:    targetId,
			ChannelType: pbobjs.ChannelType_Private,
			MsgType:     msg.MsgType,
			MsgContent:  msg.MsgContent,
			MsgId:       msgId,
			MsgSeqNo:    -1,
			MsgTime:     msg.MsgTime,
			Flags:       msg.Flags,
			IsSend:      true,
			//TargetUserInfo: commonservices.GetTargetDisplayUserInfo(ctx, targetId),
		}*/
	// add conver for sender
	// if commonservices.IsStoreMsg(msg.Flags) {
	// 	commonservices.BatchSaveConversations(ctx, []string{senderId}, downMsg4Sendbox)
	// }

	downMsg := &pbobjs.DownMsg{
		SenderId:       senderId,
		TargetId:       senderId,
		ChannelType:    pbobjs.ChannelType_Private,
		MsgType:        msg.MsgType,
		MsgContent:     msg.MsgContent,
		MsgId:          msgId,
		MsgSeqNo:       -1,
		MsgTime:        msg.MsgTime,
		Flags:          msg.Flags,
		TargetUserInfo: commonservices.GetSenderUserInfo(ctx),
	}
	//add hismsg
	if commonservices.IsStoreMsg(msg.Flags) {
		commonservices.SaveHistoryMsg(ctx, senderId, targetId, pbobjs.ChannelType_Private, downMsg, 0)

		//add conver for receiver
		//commonservices.BatchSaveConversations(ctx, []string{targetId}, downMsg)
	}
}
