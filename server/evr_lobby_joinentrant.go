package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"go.uber.org/zap"
)

func (p *EvrPipeline) LobbySessionGet(ctx context.Context, logger *zap.Logger, matchID MatchID) (*MatchLabel, Session, error) {
	return LobbySessionGet(ctx, logger, p.matchRegistry, p.tracker, p.profileRegistry, p.sessionRegistry, matchID)
}

func LobbySessionGet(ctx context.Context, logger *zap.Logger, matchRegistry MatchRegistry, tracker Tracker, profileRegistry *ProfileRegistry, sessionRegistry SessionRegistry, matchID MatchID) (*MatchLabel, Session, error) {

	match, _, err := matchRegistry.GetMatch(ctx, matchID.String())
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get match: %w", err)
	}

	label := &MatchLabel{}
	if err := json.Unmarshal([]byte(match.GetLabel().GetValue()), label); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal match label: %w", err)
	}

	serverSession := sessionRegistry.Get(uuid.FromStringOrNil(label.Broadcaster.SessionID))
	if serverSession == nil {
		return nil, nil, fmt.Errorf("failed to get server session")
	}

	return label, serverSession, nil
}

func (p *EvrPipeline) LobbyJoinEntrants(logger *zap.Logger, label *MatchLabel, presences ...*EvrMatchPresence) error {
	if len(presences) == 0 {
		return errors.New("no presences")
	}

	session := p.sessionRegistry.Get(presences[0].SessionID)
	if session == nil {
		return errors.New("session not found")
	}

	serverSession := p.sessionRegistry.Get(uuid.FromStringOrNil(label.Broadcaster.SessionID))
	if serverSession == nil {
		return errors.New("server session not found")
	}

	return LobbyJoinEntrants(logger, p.matchRegistry, p.tracker, session, serverSession, label, presences...)
}
func LobbyJoinEntrants(logger *zap.Logger, matchRegistry MatchRegistry, tracker Tracker, session Session, serverSession Session, label *MatchLabel, entrants ...*EvrMatchPresence) error {
	if session == nil || serverSession == nil {
		return errors.New("session is nil")
	}

	for _, e := range entrants {
		for _, feature := range label.RequiredFeatures {
			if !slices.Contains(e.SupportedFeatures, feature) {
				logger.With(zap.String("uid", e.UserID.String()), zap.String("sid", e.SessionID.String())).Warn("Player does not support required feature", zap.String("feature", feature), zap.String("mid", label.ID.UUID.String()), zap.String("uid", e.UserID.String()))
				return NewLobbyErrorf(MissingEntitlement, "player does not support required feature: %s", feature)
			}
		}
	}

	// Additional entrants are considered reservations
	metadata := EntrantMetadata{Presence: entrants[0], Reservations: entrants[1:]}.ToMatchMetadata()

	e := entrants[0]

	sessionCtx := session.Context()

	var err error
	var found, allowed, isNew bool
	var reason string
	var labelStr string

	// Trigger MatchJoinAttempt
	found, allowed, isNew, reason, labelStr, _ = matchRegistry.JoinAttempt(sessionCtx, label.ID.UUID, label.ID.Node, e.UserID, e.SessionID, e.Username, e.SessionExpiry, nil, e.ClientIP, e.ClientPort, label.ID.Node, metadata)
	if !found {
		err = NewLobbyErrorf(ServerDoesNotExist, "join attempt failed: match not found")
	} else if labelStr == "" {
		err = NewLobbyErrorf(ServerDoesNotExist, "join attempt failed: match label empty")
	} else if reason == ErrJoinRejectDuplicateXPID.Error() {
		err = NewLobbyErrorf(BadRequest, "join attempt failed: duplicate evr ID")
	} else if reason == ErrJoinRejectReasonMatchClosed.Error() {
		err = NewLobbyErrorf(ServerIsLocked, "join attempt failed: match closed")
	} else if !allowed {
		err = NewLobbyErrorf(ServerIsFull, "join attempt failed: not allowed: %s", reason)
	}

	if err != nil {
		logger.Warn("failed to join match", zap.Error(err))
		return fmt.Errorf("failed to join match: %w", err)
	}

	entrantStream := PresenceStream{Mode: StreamModeEntrant, Subject: e.EntrantID(label.ID), Label: e.Node}

	if isNew {

		e = &EvrMatchPresence{}
		if err := json.Unmarshal([]byte(reason), e); err != nil {
			err = fmt.Errorf("failed to unmarshal match presence: %w", err)
			return err
		}

		// Update the presence stream for the entrant.
		entrantMeta := PresenceMeta{Format: SessionFormatEVR, Username: e.Username, Status: e.String(), Hidden: false}

		success := tracker.Update(sessionCtx, e.SessionID, entrantStream, e.UserID, entrantMeta)
		if !success {
			return errors.New("failed to track session ID")
		}

	} else {

		// Use the existing entrant metadata.
		entrantMeta := tracker.GetLocalBySessionIDStreamUserID(session.ID(), entrantStream, e.EntrantID(label.ID))
		if entrantMeta == nil {
			return errors.New("failed to get entrant metadata")
		}
		if err := json.Unmarshal([]byte(entrantMeta.Status), e); err != nil {
			return fmt.Errorf("failed to unmarshal entrant metadata: %w", err)
		}
	}

	<-time.After(1 * time.Second)

	matchIDStr := label.ID.String()

	guildGroupStream := PresenceStream{Mode: StreamModeGuildGroup, Subject: label.GetGroupID(), Label: label.Mode.String()}

	ops := []*TrackerOp{
		{
			guildGroupStream,
			PresenceMeta{Format: SessionFormatEVR, Username: e.Username, Status: matchIDStr, Hidden: false},
		},
		{
			PresenceStream{Mode: StreamModeService, Subject: e.SessionID, Label: StreamLabelMatchService},
			PresenceMeta{Format: SessionFormatEVR, Username: e.Username, Status: matchIDStr, Hidden: false},
		},
		{
			PresenceStream{Mode: StreamModeService, Subject: e.LoginSessionID, Label: StreamLabelMatchService},
			PresenceMeta{Format: SessionFormatEVR, Username: e.Username, Status: matchIDStr, Hidden: false},
		},
		{
			PresenceStream{Mode: StreamModeService, Subject: e.UserID, Label: StreamLabelMatchService},
			PresenceMeta{Format: SessionFormatEVR, Username: e.Username, Status: matchIDStr, Hidden: false},
		},
		{
			PresenceStream{Mode: StreamModeService, Subject: e.XPID.UUID(), Label: StreamLabelMatchService},
			PresenceMeta{Format: SessionFormatEVR, Username: e.Username, Status: matchIDStr, Hidden: false},
		},
	}

	// Update the statuses. This is looked up by the pipeline when the game server sends the new entrant message.
	for _, op := range ops {
		if ok := tracker.Update(sessionCtx, e.SessionID, op.Stream, e.UserID, op.Meta); !ok {
			return errors.New("failed to track session ID")
		}
	}

	// Leave any other lobby group stream.
	tracker.UntrackLocalByModes(session.ID(), map[uint8]struct{}{StreamModeMatchmaking: {}, StreamModeGuildGroup: {}}, guildGroupStream)

	connectionSettings := label.GetEntrantConnectMessage(e.RoleAlignment, e.IsPCVR, e.DisableEncryption, e.DisableMAC)

	// Send the lobby session success message to the game server.
	if err := SendEVRMessages(serverSession, false, connectionSettings); err != nil {
		logger.Error("failed to send lobby session success to game server", zap.Error(err))

		return errors.New("failed to send lobby session success to game server")
	}

	// Send the lobby session success message to the game client.
	<-time.After(250 * time.Millisecond)

	err = SendEVRMessages(session, false, connectionSettings)
	if err != nil {
		logger.Error("failed to send lobby session success to game client", zap.Error(err))
		return errors.New("failed to send lobby session success to game client")
	}

	logger.Info("Joined entrant.", zap.String("mid", label.ID.UUID.String()), zap.String("uid", e.UserID.String()), zap.String("sid", e.SessionID.String()))
	return nil
}

func (p *EvrPipeline) lobbyAuthorize(ctx context.Context, session Session, lobbyParams *LobbySessionParameters, groupID string) error {
	userID := session.UserID().String()

	params, ok := LoadParams(ctx)
	if !ok {
		return errors.New("failed to get session parameters")
	}
	var err error
	var groupMetadata *GroupMetadata
	if guildGroup, ok := params.GuildGroupsLoad()[groupID]; ok {
		groupMetadata = &guildGroup.GroupMetadata
	} else {
		groupMetadata, err = GetGuildGroupMetadata(ctx, p.db, groupID)
		if err != nil {
			return fmt.Errorf("failed to get guild group metadata: %w", err)
		}
	}

	sendAuditMessage := groupMetadata.AuditChannelID != ""

	membership, ok := params.MembershipsLoad()[groupID]

	if !ok && groupMetadata.MembersOnlyMatchmaking {

		if sendAuditMessage {
			if _, err := p.appBot.LogAuditMessage(ctx, groupID, fmt.Sprintf("Rejected non-member <@%s>", userID), true); err != nil {
				p.logger.Warn("Failed to send audit message", zap.String("channel_id", groupMetadata.AuditChannelID), zap.Error(err))
			}
		}

		return NewLobbyError(KickedFromLobbyGroup, "user does not have matchmaking permission")
	}

	if membership.IsSuspended {

		if sendAuditMessage {
			if _, err := p.appBot.LogAuditMessage(ctx, groupID, fmt.Sprintf("Rejected suspended user <@%s>", userID), true); err != nil {
				p.logger.Warn("Failed to send audit message", zap.String("channel_id", groupMetadata.AuditChannelID), zap.Error(err))
			}
		}

		return ErrSuspended
	}

	if groupMetadata.MinimumAccountAgeDays > 0 && groupMetadata.IsAccountAgeBypass(userID) {
		// Check the account creation date.
		discordID, err := GetDiscordIDByUserID(ctx, p.db, userID)
		if err != nil {
			return fmt.Errorf("failed to get discord ID by user ID: %w", err)
		}

		t, err := discordgo.SnowflakeTimestamp(discordID)
		if err != nil {
			return fmt.Errorf("failed to get discord snowflake timestamp: %w", err)
		}

		if t.After(time.Now().AddDate(0, 0, -groupMetadata.MinimumAccountAgeDays)) {

			if sendAuditMessage {

				accountAge := time.Since(t).Hours() / 24

				if _, err := p.appBot.dg.ChannelMessageSend(groupMetadata.AuditChannelID, fmt.Sprintf("Rejected user <@%s> because of account age (%d days).", discordID, int(accountAge))); err != nil {
					p.logger.Warn("Failed to send audit message", zap.String("channel_id", groupMetadata.AuditChannelID), zap.Error(err))
				}
			}

			return NewLobbyErrorf(KickedFromLobbyGroup, "account is too young to join this guild")
		}
	}

	if groupMetadata.BlockVPNUsers && params.IsVPN && !groupMetadata.IsVPNBypass(userID) {

		score := p.ipqsClient.Score(session.ClientIP())

		if score >= groupMetadata.FraudScoreThreshold {

			if sendAuditMessage {
				content := fmt.Sprintf("Rejected VPN user <@%s> (score: %d) from %s", lobbyParams.DiscordID, score, session.ClientIP())
				if _, err := p.appBot.LogAuditMessage(ctx, groupID, content, true); err != nil {
					p.logger.Warn("Failed to send audit message", zap.String("channel_id", groupMetadata.AuditChannelID), zap.Error(err))
				}
			}

			return NewLobbyError(KickedFromLobbyGroup, "this guild does not allow VPN users")
		}
	}

	features := params.SupportedFeatures
	if ok && len(features) > 0 {
		allowedFeatures := groupMetadata.AllowedFeatures
		for _, feature := range features {
			if !slices.Contains(allowedFeatures, feature) {
				return NewLobbyError(KickedFromLobbyGroup, "This guild does not allow clients with `feature DLLs`.")
			}
		}
	}

	displayName := params.AccountMetadata.GetGroupDisplayNameOrDefault(groupID)

	if err := p.profileRegistry.SetLobbyProfile(ctx, uuid.FromStringOrNil(userID), params.XPID, displayName); err != nil {
		return fmt.Errorf("failed to set lobby profile: %w", err)
	}

	p.runtimeModule.Event(ctx, &api.Event{
		Name: "lobby_session_authorized",
		Properties: map[string]string{
			"session_id":   session.ID().String(),
			"group_id":     groupID,
			"user_id":      userID,
			"discord_id":   params.DiscordID,
			"display_name": displayName,
		},
		External: true, // used to denote if the event was generated from the client
	})

	session.Logger().Info("Authorized access to lobby session", zap.String("gid", groupID), zap.String("display_name", displayName))

	return nil
}
