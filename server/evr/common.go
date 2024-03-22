package evr

import "github.com/gofrs/uuid/v5"

// A message that can be used to validate the session.
type IdentifyingMessage interface {
	SessionID() uuid.UUID
	EvrID() EvrId
}

/*

SNSActivityDailyListRequest
SNSActivityDailyListResponse
SNSActivityDailyRewardFailure
SNSActivityDailyRewardRequest
SNSActivityDailyRewardSuccess
SNSActivityEventListRequest
SNSActivityEventListResponse
SNSActivityEventRewardFailure
SNSActivityEventRewardRequest
SNSActivityEventRewardSuccess
SNSActivityWeeklyListRequest
SNSActivityWeeklyListResponse
SNSActivityWeeklyRewardFailure
SNSActivityWeeklyRewardRequest
SNSActivityWeeklyRewardSuccess
SNSAddTournament

SNSEarlyQuitConfig
SNSEarlyQuitFeatureFlags
SNSEarlyQuitUpdateNotification

SNSFriendAcceptFailure
SNSFriendAcceptNotify
SNSFriendAcceptRequest
SNSFriendAcceptSuccess
SNSFriendInviteFailure
SNSFriendInviteNotify
SNSFriendInviteRequest
SNSFriendInviteSuccess
SNSFriendListRefreshRequest
SNSFriendListResponse
SNSFriendListSubscribeRequest
SNSFriendListUnsubscribeRequest
SNSFriendRejectNotify
SNSFriendRemoveNotify
SNSFriendRemoveRequest
SNSFriendRemoveResponse
SNSFriendStatusNotify
SNSFriendWithdrawnNotify
SNSGenericMessage
SNSGenericMessageNotify
SNSLeaderboardRequestv2
SNSLeaderboardResponse

SNSLobbyDirectoryJson
SNSLobbyDirectoryRequestJsonv2

SNSLobbyPlayerSessionsFailurev3

SNSLoginRemovedNotify

SNSLogOut
SNSMatchEndedv5
SNSNewUnlocksNotification

SNSPartyCreateFailure
SNSPartyCreateRequest
SNSPartyCreateSuccess
SNSPartyInviteListRefreshRequest
SNSPartyInviteListResponse
SNSPartyInviteNotify
SNSPartyInviteRequest
SNSPartyInviteResponse
SNSPartyJoinFailure
SNSPartyJoinNotify
SNSPartyJoinRequest
SNSPartyJoinSuccess
SNSPartyKickFailure
SNSPartyKickNotify
SNSPartyKickRequest
SNSPartyKickSuccess
SNSPartyLeaveFailure
SNSPartyLeaveNotify
SNSPartyLeaveRequest
SNSPartyLeaveSuccess
SNSPartyLockFailure
SNSPartyLockNotify
SNSPartyLockRequest
SNSPartyLockSuccess
SNSPartyPassFailure
SNSPartyPassNotify
SNSPartyPassRequest
SNSPartyPassSuccess
SNSPartyUnlockFailure
SNSPartyUnlockNotify
SNSPartyUnlockRequest
SNSPartyUnlockSuccess
SNSPartyUpdateFailure
SNSPartyUpdateMemberFailure
SNSPartyUpdateMemberNotify
SNSPartyUpdateMemberRequest
SNSPartyUpdateMemberSuccess
SNSPartyUpdateNotify
SNSPartyUpdateRequest
SNSPartyUpdateSuccess
SNSPurchaseItems
SNSPurchaseItemsResult

SNSRemoveTournament
SNSRewardsSettings
SNSServerSettingsResponsev2
SNSTelemetryEvent
SNSTelemetryNotify

SNSUserServerProfileUpdateFailure

*/