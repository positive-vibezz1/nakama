package server

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/samber/lo"
)

type WhoAmI struct {
	NakamaID             uuid.UUID            `json:"nakama_id"`
	Username             string               `json:"username"`
	DiscordID            string               `json:"discord_id"`
	CreateTime           time.Time            `json:"create_time,omitempty"`
	DisplayNames         []string             `json:"display_names"`
	DeviceLinks          []string             `json:"device_links,omitempty"`
	HasPassword          bool                 `json:"has_password"`
	RecentLogins         map[string]time.Time `json:"recent_logins,omitempty"`
	GuildGroups          []*GuildGroup        `json:"guild_groups,omitempty"`
	DefualtActiveGuild   []string             `json:"default_active_guild,omitempty"`
	VRMLSeasons          []string             `json:"vrml_seasons"`
	MatchLabels          []*MatchLabel        `json:"match_labels"`
	DefaultLobbyGroup    string               `json:"active_lobby_group,omitempty"`
	GhostedPlayers       []string             `json:"ghosted_discord_ids,omitempty"`
	LastMatchmakingError error                `json:"last_matchmaking_error,omitempty"`
}

type EvrIdLogins struct {
	EvrId         string `json:"xp_id"`
	LastLoginTime string `json:"login_time"`
	DisplayName   string `json:"display_name,omitempty"`
}

func (d *DiscordAppBot) handleProfileRequest(ctx context.Context, logger runtime.Logger, nk runtime.NakamaModule, s *discordgo.Session, i *discordgo.InteractionCreate, targetID string, username string, includePriviledged bool, includePrivate bool) error {
	whoami := &WhoAmI{
		DiscordID:          targetID,
		RecentLogins:       make(map[string]time.Time),
		DisplayNames:       make([]string, 0),
		DeviceLinks:        make([]string, 0),
		GuildGroups:        make([]*GuildGroup, 0),
		DefualtActiveGuild: make([]string, 0),
		MatchLabels:        make([]*MatchLabel, 0),
	}

	// Get the user's ID
	member, err := s.GuildMember(i.GuildID, targetID)
	if err != nil || member == nil || member.User == nil {
		return fmt.Errorf("failed to get guild member: %w", err)
	}

	// Create the account if the user doesn't exist

	userIDStr, err := GetUserIDByDiscordID(ctx, d.db, targetID)
	if err != nil {
		if i.Member.User.ID == targetID {
			userIDStr, _, _, err = d.nk.AuthenticateCustom(ctx, targetID, member.User.Username, true)
			if err != nil {
				return fmt.Errorf("failed to authenticate (or create) user %s: %w", targetID, err)
			}
		} else {
			return fmt.Errorf("player does not exist")
		}
	}
	userID := uuid.FromStringOrNil(userIDStr)

	// Basic account details
	whoami.NakamaID = userID

	account, err := nk.AccountGetId(ctx, whoami.NakamaID.String())
	if err != nil {
		return fmt.Errorf("failed to get account by ID: %w", err)
	}

	if account.GetDisableTime() != nil && !includePriviledged {
		return fmt.Errorf("account is disabled")
	}

	md, err := GetAccountMetadata(ctx, nk, userID.String())
	if err != nil {
		return fmt.Errorf("failed to get account metadata: %w", err)
	}

	if includePriviledged {
		whoami.DefaultLobbyGroup = md.GetActiveGroupID().String()
	}

	whoami.Username = account.GetUser().GetUsername()

	if account.GetUser().GetCreateTime() != nil {
		whoami.CreateTime = account.GetUser().GetCreateTime().AsTime().UTC()
	}

	if includePriviledged {
		// Get the device links from the account
		whoami.DeviceLinks = make([]string, 0, len(account.GetDevices()))
		for _, device := range account.GetDevices() {
			whoami.DeviceLinks = append(whoami.DeviceLinks, fmt.Sprintf("`%s`", device.GetId()))
		}

		whoami.HasPassword = account.GetEmail() != ""
	}

	guildGroups, err := UserGuildGroupsList(ctx, nk, userID.String())
	if err != nil {
		return fmt.Errorf("error getting guild groups: %w", err)
	}

	for _, g := range guildGroups {
		if includePrivate || g.GuildID == i.GuildID {
			whoami.GuildGroups = append(whoami.GuildGroups, g)
		}
	}

	loginHistory, err := LoginHistoryLoad(ctx, nk, userID.String())
	if err != nil {
		return fmt.Errorf("error getting device history: %w", err)
	}

	whoami.RecentLogins = make(map[string]time.Time, 0)

	if includePriviledged {
		for k, e := range loginHistory.History {
			if !includePrivate {
				// Remove the IP address
				k = e.XPID.String()
			}
			whoami.RecentLogins[k] = e.UpdatedAt.UTC()

		}
	}

	displayNameHistory, err := DisplayNameHistoryLoad(ctx, nk, userID.String())
	if err != nil {
		return fmt.Errorf("failed to load display name history: %w", err)
	}

	pastDisplayNames := make(map[string]time.Time)
	for groupID, items := range displayNameHistory.Histories {
		if !includePriviledged && groupID != i.GuildID {
			continue
		}
		for _, item := range items {
			if e, ok := pastDisplayNames[item.DisplayName]; !ok || e.After(item.UpdateTime) {
				pastDisplayNames[item.DisplayName] = item.UpdateTime
			}
		}
	}

	whoami.DisplayNames = make([]string, 0, len(pastDisplayNames))
	for dn := range pastDisplayNames {
		whoami.DisplayNames = append(whoami.DisplayNames, EscapeDiscordMarkdown(dn))
	}

	slices.SortStableFunc(whoami.DisplayNames, func(a, b string) int {
		return int(pastDisplayNames[a].Unix() - pastDisplayNames[b].Unix())
	})

	if len(whoami.DisplayNames) > 10 {
		whoami.DisplayNames = whoami.DisplayNames[len(whoami.DisplayNames)-10:]
	}

	if !includePriviledged && len(whoami.DisplayNames) > 1 {
		// Only show the most recent display name
		whoami.DisplayNames = whoami.DisplayNames[len(whoami.DisplayNames)-1:]
	}

	presences, err := d.nk.StreamUserList(StreamModeService, userID.String(), "", StreamLabelMatchService, false, true)
	if err != nil {
		return err
	}
	if includePriviledged {

		whoami.MatchLabels = make([]*MatchLabel, 0, len(presences))
		for _, p := range presences {
			if p.GetStatus() != "" {
				mid := MatchIDFromStringOrNil(p.GetStatus())
				if mid.IsNil() {
					continue
				}
				label, err := MatchLabelByID(ctx, nk, mid)
				if err != nil {
					logger.Warn("failed to get match label", "error", err)
					continue
				}

				whoami.MatchLabels = append(whoami.MatchLabels, label)
			}
		}

		// If the player is online, Get the most recent matchmaking error for the player.
		if len(presences) > 0 {
			// Get the most recent matchmaking error for the player
			if session := d.pipeline.sessionRegistry.Get(uuid.FromStringOrNil(presences[0].GetSessionId())); session != nil {
				params, ok := LoadParams(session.Context())
				if ok {
					whoami.LastMatchmakingError = params.LastMatchmakingError.Load()
				}
			}
		}
	}

	if includePriviledged {
		friends, err := ListPlayerFriends(ctx, RuntimeLoggerToZapLogger(logger), d.db, d.statusRegistry, userID)
		if err != nil {
			return err
		}

		ghostedDiscordIDs := make([]string, 0)
		for _, f := range friends {
			if api.Friend_State(f.GetState().Value) == api.Friend_BLOCKED {
				discordID := d.cache.UserIDToDiscordID(f.GetUser().GetId())
				ghostedDiscordIDs = append(ghostedDiscordIDs, discordID)
			}
		}
		whoami.GhostedPlayers = ghostedDiscordIDs
	}

	fields := []*discordgo.MessageEmbedField{
		{Name: "Nakama ID", Value: whoami.NakamaID.String(), Inline: true},
		{Name: "Username", Value: whoami.Username, Inline: true},
		{Name: "Discord ID", Value: whoami.DiscordID, Inline: true},
		{Name: "Created", Value: fmt.Sprintf("<t:%d:R>", whoami.CreateTime.Unix()), Inline: false},
		{Name: "Password Set", Value: func() string {
			if whoami.HasPassword {
				return "Yes"
			}
			return ""
		}(), Inline: true},
		{Name: "Online", Value: func() string {
			if len(presences) > 0 {
				return "Yes"
			}
			return "No"
		}(), Inline: false},
		{Name: "Linked Devices", Value: strings.Join(whoami.DeviceLinks, "\n"), Inline: false},
		{Name: "Display Names", Value: strings.Join(whoami.DisplayNames, "\n"), Inline: false},
		{Name: "Recent Logins", Value: func() string {
			lines := lo.MapToSlice(whoami.RecentLogins, func(k string, v time.Time) string {
				if v.IsZero() {
					// Don't use the timestamp
					return k
				} else {
					return fmt.Sprintf("<t:%d:R> - %s", v.Unix(), k)
				}
			})
			slices.Sort(lines)
			slices.Reverse(lines)
			return strings.Join(lines, "\n")
		}(), Inline: false},
		{Name: "Guild Memberships", Value: strings.Join(func() []string {
			output := make([]string, 0, len(whoami.GuildGroups))

			sort.SliceStable(whoami.GuildGroups, func(i, j int) bool {
				return whoami.GuildGroups[i].Name() < whoami.GuildGroups[j].Name()
			})

			for _, group := range whoami.GuildGroups {
				groupStr := group.Name()

				if group.GuildID != i.GuildID && !includePriviledged {
					output = append(output, groupStr)
					continue
				}

				if !includePrivate {
					continue
				}

				m := group.PermissionsUser(userID.String())

				roles := make([]string, 0)
				if m.IsAllowedMatchmaking {
					roles = append(roles, "matchmaking")
				}
				if m.IsModerator {
					roles = append(roles, "moderator")
				}
				if m.IsServerHost {
					roles = append(roles, "server-host")
				}
				if m.IsAllocator {
					roles = append(roles, "allocator")
				}
				if m.IsAPIAccess {
					roles = append(roles, "api-access")
				}
				if m.IsSuspended {
					roles = append(roles, "suspended")
				}
				if m.IsVPNBypass {
					roles = append(roles, "vpn-bypass")
				}
				if len(roles) > 0 {
					groupStr += fmt.Sprintf(" (%s)", strings.Join(roles, ", "))
				}
				output = append(output, groupStr)
			}
			return output
		}(), "\n"), Inline: false},
		{Name: "Match List", Value: strings.Join(lo.Map(whoami.MatchLabels, func(l *MatchLabel, index int) string {
			link := fmt.Sprintf("`%s`: https://echo.taxi/spark://c/%s", l.Mode.String(), strings.ToUpper(l.ID.UUID.String()))
			players := make([]string, 0, len(l.Players))
			for _, p := range l.Players {
				players = append(players, fmt.Sprintf("<@%s>", p.DiscordID))
			}
			return fmt.Sprintf("%s - %s\n%s", l.Mode.String(), link, strings.Join(players, ", "))
		}), "\n"), Inline: false},
	}

	fields = append(fields, &discordgo.MessageEmbedField{
		Name: "Default Matchmaking Guild",
		Value: func() string {
			if whoami.DefaultLobbyGroup != "" {
				for _, group := range whoami.GuildGroups {
					if group.GuildID == whoami.DefaultLobbyGroup {
						return group.Name()
					}
				}
			}
			return ""
		}(),
		Inline: false,
	})

	if whoami.LastMatchmakingError != nil {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Last Matchmaking Error",
			Value:  whoami.LastMatchmakingError.Error(),
			Inline: false,
		})
	}
	// Remove any blank fields
	fields = lo.Filter(fields, func(f *discordgo.MessageEmbedField, _ int) bool {
		return f.Value != ""
	})

	// Send the response
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Flags: discordgo.MessageFlagsEphemeral,
			Embeds: []*discordgo.MessageEmbed{
				{
					Title:  "EchoVRCE Account",
					Color:  0xCCCCCC,
					Fields: fields,
				},
			},
		},
	})
}
