package server

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/go-playground/validator/v10"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/samber/lo"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	DiscordRegistryLookupCollection = "DiscordRegistry"
	DiscordRegistryLookupKey        = "LookupTables"
)

var (
	validate = validator.New(validator.WithRequiredStructEnabled())

	ErrGroupIsNotaGuild = fmt.Errorf("group is not a guild")
)

type LookupTable struct {
	sync.RWMutex
	Store map[string]string `json:"store"`
}

type DiscordRegistry interface {
	Get(discordId string) (nakamaId string, ok bool)
	GetBot() *discordgo.Session
	Store(discordId string, nakamaId string)
	Delete(discordId string)
	GetDiscordIdByUserId(ctx context.Context, userId string) (discordId string, err error)
	GetUserIdByUsername(ctx context.Context, username string, create bool) (userId string, err error)
	UpdateAccount(ctx context.Context, discordId string) error
	GetUserIdByDiscordId(ctx context.Context, discordId string, create bool) (userId string, err error)
	GetGuildByGroupId(ctx context.Context, groupId string) (*discordgo.Guild, error)
	ReplaceMentions(guildID, s string) string
	PopulateCache() (cnt int, err error)
	GetGuildGroupMetadata(ctx context.Context, groupId string) (metadata *GroupMetadata, err error)
	// GetGuildMember looks up the Discord member by the guild ID and member ID. Potentially using the state cache.
	GetGuildMember(ctx context.Context, guildId, memberId string) (*discordgo.Member, error)
	SynchronizeGroup(ctx context.Context, guild *discordgo.Guild) error
	GetGuild(ctx context.Context, guildId string) (*discordgo.Guild, error)
	// GetGuildGroups looks up the guild groups by the user ID
	GetGuildGroups(ctx context.Context, userId string) ([]*api.Group, error)
	// GetUser looks up the Discord user by the user ID. Potentially using the state cache.
	GetUser(ctx context.Context, discordId string) (*discordgo.User, error)
	InitializeDiscordBot(ctx context.Context, logger runtime.Logger, nk runtime.NakamaModule, initializer runtime.Initializer) error
}

// The discord registry is a storage-backed lookup table for discord user ids to nakama user ids.
// It also carries the bot session and a cache for the lookup table.
type LocalDiscordRegistry struct {
	sync.RWMutex
	ctx     context.Context
	nk      runtime.NakamaModule
	logger  runtime.Logger
	metrics Metrics

	bot       *discordgo.Session // The bot
	botUserId string

	cache sync.Map // Generic cache for map[discordId]nakamaId lookup
}

func NewLocalDiscordRegistry(ctx context.Context, nk runtime.NakamaModule, logger runtime.Logger, metrics Metrics) (r *LocalDiscordRegistry) {
	var err error
	var dg *discordgo.Session

	botToken, ok := ctx.Value(ctxDiscordBotTokenKey{}).(string)
	if !ok {
		panic("Bot token is not set in context.")
	}
	// Start the bot
	dg, err = discordgo.New("Bot " + botToken)
	if err != nil {
		logger.Error("Unable to create bot")
	}

	dg.StateEnabled = true

	discordRegistry := &LocalDiscordRegistry{
		ctx:     ctx,
		nk:      nk,
		logger:  logger,
		metrics: metrics,
		bot:     dg,
		cache:   sync.Map{},
	}

	dg.AddHandler(func(s *discordgo.Session, m *discordgo.Ready) {
		discordRegistry.PopulateCache()
	})

	return discordRegistry
}

func (r *LocalDiscordRegistry) GetBot() *discordgo.Session {
	return r.bot
}

// PopulateCache populates the lookup cache with all the guilds and their roles
func (r *LocalDiscordRegistry) PopulateCache() (cnt int, err error) {
	botId, err := r.GetUserIdByDiscordId(r.ctx, r.bot.State.User.ID, true)
	if err == nil {
		r.botUserId = botId
	}
	// Populate the cache with all the guild groups
	cnt = 0
	var groups []*api.Group
	cursor := ""
	for {
		groups, cursor, err = r.nk.GroupsList(r.ctx, "", "guild", nil, nil, 100, cursor)
		if err != nil {
			return
		}
		for _, group := range groups {
			// Check if the cache already has this group -> discordId entry
			if d, ok := r.Get(group.Id); ok {
				// Check that the reverse is true
				if g, ok := r.Get(d); ok {
					if g == group.Id {
						continue
					}
				}
			}

			metadata := &GroupMetadata{}
			if err := json.Unmarshal([]byte(group.Metadata), metadata); err != nil {
				r.logger.Warn(fmt.Sprintf("Error unmarshalling group metadata for group %s:  %s", group.Id, err))
			}

			if metadata.GuildId != "" {
				r.Store(metadata.GuildId, group.Id)
				r.Store(group.Id, metadata.GuildId)
				guild, err := r.GetGuild(r.ctx, metadata.GuildId)
				if err != nil {
					r.logger.Warn(fmt.Sprintf("Error getting guild %s: %s", metadata.GuildId, err))
					continue
				}
				r.bot.State.GuildAdd(guild)
				cnt++
			}

			mapping := map[string]string{
				metadata.ModeratorRole:       metadata.ModeratorGroupId,
				metadata.BroadcasterHostRole: metadata.BroadcasterHostGroupId,
			}

			for roleId, groupId := range mapping {
				if roleId != "" && groupId != "" {
					// Verify the cache entry
					entry, found := r.Get(roleId)
					if found && entry != groupId {
						r.logger.Warn(fmt.Sprintf("Role %s does not match group %s", roleId, groupId))
					}
					// Verify the reverse
					entry, found = r.Get(groupId)
					if found && entry != roleId {
						r.logger.Warn(fmt.Sprintf("Group %s does not match role %s", groupId, roleId))
						continue
					}

					// Verify that the role exists on the guild
					_, err := r.bot.State.Role(metadata.GuildId, roleId)
					if err != nil {
						r.logger.Warn(fmt.Sprintf("Error getting role %s for guild %s: %s", roleId, metadata.GuildId, err))
						continue
					}

					// Verify the group exists and has the correct guildId
					groups, err := r.nk.GroupsGetId(r.ctx, []string{groupId})
					if err != nil {
						r.logger.Warn(fmt.Sprintf("Error getting role group %s: %s", groupId, err))
						continue
					}
					if len(groups) == 0 {
						r.logger.Warn(fmt.Sprintf("Role group %s does not exist", groupId))
						continue
					}
					group := groups[0]
					md := &GroupMetadata{}
					if err := json.Unmarshal([]byte(group.GetMetadata()), md); err != nil {
						r.logger.Warn(fmt.Sprintf("Error unmarshalling group metadata for group %s:  %s", group.Id, err))
						continue
					}
					if md.GuildId != metadata.GuildId {
						r.logger.Warn(fmt.Sprintf("Role group %s does not belong to guild %s", groupId, metadata.GuildId))
						continue
					}
					r.Store(roleId, groupId)
					r.Store(groupId, roleId)
					cnt++
				}
			}
		}
		if cursor == "" {
			break
		}
	}

	r.logger.Info("Populated registry lookup cache with %d guilds/roles/users", cnt)
	return
}

// Get looks up the Nakama group ID by the Discord guild or role ID from cache.
func (r *LocalDiscordRegistry) Get(discordId string) (nakamaId string, ok bool) {
	if v, ok := r.cache.Load(discordId); ok {
		return v.(string), ok
	}
	return "", false
}

// Store adds or updates the Nakama group ID by the Discord guild or role ID
func (r *LocalDiscordRegistry) Store(discordId string, nakamaId string) {
	r.cache.Store(discordId, nakamaId)
}

// Delete removes the Nakama group ID by the Discord guild or role ID
func (r *LocalDiscordRegistry) Delete(discordId string) {
	r.cache.Delete(discordId)
}

// GetUser looks up the Discord user by the user ID. Potentially using the state cache.
func (r *LocalDiscordRegistry) GetUser(ctx context.Context, discordId string) (*discordgo.User, error) {
	if discordId == "" {
		return nil, fmt.Errorf("discordId is required")
	}

	// Try to find the user in a guild state first.
	for _, guild := range r.bot.State.Guilds {
		if member, err := r.bot.State.Member(guild.ID, discordId); err == nil {
			if member.User == nil || member.User.Username == "" || member.User.GlobalName == "" {
				continue
			}
			return member.User, nil
		}
	}

	// Get it from the API
	return r.bot.User(discordId)
}

// GetGuild looks up the Discord guild by the guild ID. Potentially using the state cache.
func (r *LocalDiscordRegistry) GetGuild(ctx context.Context, guildId string) (*discordgo.Guild, error) {

	if guildId == "" {
		return nil, fmt.Errorf("guildId is required")
	}
	// Check the cache
	if guild, err := r.bot.State.Guild(guildId); err == nil {
		return guild, nil
	}
	return r.bot.Guild(guildId)
}

// GetGuildByGroupId looks up the Discord guild by the group ID. Potentially using the state cache.
func (r *LocalDiscordRegistry) GetGuildByGroupId(ctx context.Context, groupId string) (*discordgo.Guild, error) {
	if groupId == "" {
		return nil, fmt.Errorf("guildId is required")
	}
	// Get the guild group metadata
	md, err := r.GetGuildGroupMetadata(ctx, groupId)
	if err != nil {
		return nil, fmt.Errorf("error getting guild group metadata: %w", err)
	}
	return r.GetGuild(ctx, md.GuildId)

}

// GetUserIdByMemberId looks up the Nakama user ID by the Discord user ID
func (r *LocalDiscordRegistry) GetUserIdByUsername(ctx context.Context, username string, create bool) (string, error) {
	if username == "" {
		return "", fmt.Errorf("username is required")
	}

	// Lookup the user by the username
	users, err := r.nk.UsersGetUsername(ctx, []string{username})
	if err != nil {
		return "", err
	}
	if len(users) == 0 {
		return "", status.Error(codes.NotFound, "User not found")
	}

	return users[0].Id, nil
}

// GetGuildMember looks up the Discord member by the guild ID and member ID. Potentially using the state cache.
func (r *LocalDiscordRegistry) GetGuildMember(ctx context.Context, guildId, memberId string) (*discordgo.Member, error) {
	// Check if guildId and memberId are provided
	if guildId == "" {
		return nil, fmt.Errorf("guildId is required")
	}
	if memberId == "" {
		return nil, fmt.Errorf("memberId is required")
	}

	// Try to find the member in the guild state (cache) first
	if member, err := r.bot.State.Member(guildId, memberId); err == nil {
		return member, nil
	}

	// If member is not found in the cache, get it from the API
	member, err := r.bot.GuildMember(guildId, memberId)
	if err != nil {
		return nil, fmt.Errorf("error getting member %s in guild %s: %w", memberId, guildId, err)
	}

	return member, nil
}

func (r *LocalDiscordRegistry) GetGuildGroupMetadata(ctx context.Context, groupId string) (*GroupMetadata, error) {
	// Check if groupId is provided
	if groupId == "" {
		return nil, fmt.Errorf("groupId is required")
	}

	// Fetch the group using the provided groupId
	groups, err := r.nk.GroupsGetId(ctx, []string{groupId})
	if err != nil {
		return nil, fmt.Errorf("error getting group (%s): %w", groupId, err)
	}

	// Check if the group exists
	if len(groups) == 0 {
		return nil, fmt.Errorf("group not found: %s", groupId)
	}

	if groups[0].LangTag != "guild" {
		return nil, ErrGroupIsNotaGuild
	}
	// Extract the metadata from the group
	data := groups[0].GetMetadata()

	// Unmarshal the metadata into a GroupMetadata struct
	guildGroup := &GroupMetadata{}
	if err := json.Unmarshal([]byte(data), guildGroup); err != nil {
		return nil, fmt.Errorf("error unmarshalling group metadata: %w", err)
	}

	// Update the cache
	r.Store(groupId, guildGroup.GuildId)
	r.Store(guildGroup.GuildId, groupId)
	// Return the unmarshalled GroupMetadata
	return guildGroup, nil
}

// GetGuildGroups looks up the guild groups by the user ID
func (r *LocalDiscordRegistry) GetGuildGroups(ctx context.Context, userId string) ([]*api.Group, error) {
	// Check if userId is provided
	if userId == "" {
		return nil, fmt.Errorf("userId is required")
	}

	// Fetch the groups using the provided userId
	groups, _, err := r.nk.UserGroupsList(ctx, userId, 100, nil, "")
	if err != nil {
		return nil, fmt.Errorf("error getting user `%s`'s group groups: %w", userId, err)
	}
	guildGroups := make([]*api.Group, 0, len(groups))
	for _, g := range groups {
		if g.Group.LangTag == "guild" && g.GetState().GetValue() <= int32(api.UserGroupList_UserGroup_MEMBER) {
			guildGroups = append(guildGroups, g.Group)
		}
	}
	return guildGroups, nil
}

// UpdateAccount updates the Nakama account with the Discord user data
func (r *LocalDiscordRegistry) UpdateAccount(ctx context.Context, discordId string) error {

	if r.metrics != nil {
		timer := time.Now()

		defer func() { r.logger.Debug("UpdateAccount took %dms", time.Since(timer)/time.Millisecond) }()
		defer func() { r.metrics.CustomTimer("UpdateAccountFn", nil, time.Since(timer)) }()
	}

	// Get the discord User
	u, err := r.GetUser(ctx, discordId)
	if err != nil {
		return fmt.Errorf("error getting discord user: %v", err)
	}

	// Get the nakama account for this discord user
	userId, err := r.GetUserIdByDiscordId(ctx, discordId, true)
	if err != nil {
		return fmt.Errorf("error getting nakama user: %v", err)
	}

	// Map Discord user data onto Nakama account data
	username := u.Username
	s := strings.SplitN(u.Locale, "-", 2)
	langTag := s[0]
	avatar := u.AvatarURL("512")

	// Update the basic account details
	go func() {
		if err := r.nk.AccountUpdateId(ctx, userId, username, nil, "", "", "", langTag, avatar); err != nil {
			r.logger.Error("Error updating account %s: %v", username, err)
		}
	}()

	// Synchronize the user's guilds with nakama groups

	// Get the user's groups
	userGroups, _, err := r.nk.UserGroupsList(ctx, userId, 100, nil, "")
	if err != nil {
		return fmt.Errorf("error getting user groups: %v", err)
	}

	userGroupIds := make([]string, 0)
	for _, g := range userGroups {
		if g.Group.LangTag == "guild" && api.UserGroupList_UserGroup_State(g.State.GetValue()) <= api.UserGroupList_UserGroup_MEMBER {
			userGroupIds = append(userGroupIds, g.Group.Id)
		}
	}
	if r.metrics != nil {
		timer := time.Now()

		defer func() { r.logger.Debug("UpdateAccount (discord part) took %dms", time.Since(timer)/time.Millisecond) }()
		defer func() { r.metrics.CustomTimer("UpdateAccountFn_discord", nil, time.Since(timer)) }()
	}
	guilds, err := r.bot.UserGuilds(100, "", "")
	if err != nil {
		return fmt.Errorf("error getting user guilds: %v", err)
	}

	for _, guild := range guilds {

		// Get the guild's group ID
		groupId, found := r.Get(guild.ID)
		if !found {
			r.logger.Warn("Could not find group for guild %s", guild.ID)
			continue
		}

		// Get the guild's group metadata
		md, err := r.GetGuildGroupMetadata(ctx, groupId)
		if err != nil {
			if err == ErrGroupIsNotaGuild {
				continue
			}
			r.logger.Error("Error getting guild group %s: %w", guild.Name, err)
		}
		if md == nil {
			continue
		}
		guildGroups := []string{
			groupId,
			md.ModeratorGroupId,
			md.BroadcasterHostGroupId,
		}

		// Get the guild member
		member, err := r.GetGuildMember(ctx, guild.ID, discordId)
		if err != nil {
			if slices.Contains(userGroupIds, groupId) {
				// Remove to user from the guild group
				defer r.nk.GroupUsersKick(ctx, SystemUserId, groupId, []string{userId})
			}
		}
		if member == nil {
			continue
		}
		currentRoles := member.Roles

		isSuspended := len(lo.Intersect(currentRoles, md.SuspensionRoles)) > 0
		currentGroups := lo.Intersect(userGroupIds, guildGroups)

		activeGroups := make([]string, 0)
		activeGroups = append(activeGroups, groupId)

		if slices.Contains(currentRoles, md.ModeratorRole) {
			activeGroups = append(activeGroups, md.ModeratorGroupId)
		}

		if slices.Contains(currentRoles, md.BroadcasterHostRole) {
			activeGroups = append(activeGroups, md.BroadcasterHostGroupId)
		}

		adds, removes := lo.Difference(activeGroups, currentGroups)

		if isSuspended {
			removes = append(removes, md.ModeratorGroupId, md.BroadcasterHostGroupId)
			adds = []string{}
		}

		for _, groupId := range removes {
			defer r.nk.GroupUsersKick(ctx, SystemUserId, groupId, []string{userId})
		}

		for _, groupId := range adds {
			defer r.nk.GroupUsersAdd(ctx, SystemUserId, groupId, []string{userId})
		}
	}

	defer r.Store(discordId, userId)
	defer r.Store(userId, discordId)

	return nil
}

// GetUserIdByDiscordId looks up, or creates, the Nakama user ID by the Discord user ID; potentially using the cache.
func (r *LocalDiscordRegistry) GetUserIdByDiscordId(ctx context.Context, discordId string, create bool) (userId string, err error) {
	if discordId == "" {
		return "", fmt.Errorf("discordId is required")
	}

	// Check the cache
	if userId, ok := r.Get(discordId); ok {
		return userId, nil
	}

	// Lookup the nakama user by the discord user id
	u, err := r.GetUser(ctx, discordId)
	if err != nil {
		return "", fmt.Errorf("error getting discord user %s: %w", discordId, err)
	}

	userId, username, _, err := r.nk.AuthenticateCustom(ctx, discordId, u.Username, create)
	if err != nil {
		return "", err
	}

	if u.Username != username {
		r.logger.Warn("Username mismatch: %s != %s, running full account update", u.Username, username)
		go func() {
			if err := r.UpdateAccount(ctx, discordId); err != nil {
				return
			}
		}()
	}

	// Store the discordId and userId in the cache when the function returns
	defer r.Store(discordId, userId)

	return userId, nil
}

// GetDiscordIdByUserId looks up the Discord user ID by the Nakama user ID; potentially using the cache.
func (r *LocalDiscordRegistry) GetDiscordIdByUserId(ctx context.Context, userId string) (discordId string, err error) {
	if userId == "" {
		return "", fmt.Errorf("userId is required")
	}

	// Check the cache
	if v, ok := r.cache.Load(userId); ok {
		return v.(string), nil
	}

	// Lookup the discord user by the nakama user id
	account, err := r.nk.AccountGetId(ctx, userId)
	if err != nil {
		return "", err
	}

	discordId = account.GetCustomId()

	// Store the discordId and userId in the cache when the function returns
	defer r.Store(discordId, userId)

	return discordId, nil
}

// ReplaceMentions replaces the discord user mentions with the user's display name
func (r *LocalDiscordRegistry) ReplaceMentions(guildID, s string) string {
	s = strings.Replace(s, "\\u003c", " <", -1)
	s = strings.Replace(s, "\\u003e", "> ", -1)
	f := strings.Fields(s)
	for i, v := range f {
		if strings.HasPrefix(v, "<@") && strings.HasSuffix(v, ">") {
			f[i] = strings.Trim(v, "<@>")
			u, err := r.bot.GuildMember(guildID, f[i])
			if err != nil {
				continue
			}
			f[i] = "@" + u.DisplayName()
		}
	}
	return strings.Join(f, " ")
}

func parseDuration(s string) (time.Duration, error) {

	f := strings.Fields(s)
	if len(f) != 2 {
		return 0, fmt.Errorf("invalid duration: invalid number of fields: %s", s)
	}
	d, err := strconv.Atoi(f[0])
	if err != nil {
		return 0, fmt.Errorf("invalid duration: unable to parse: %s", s)
	}

	switch f[1][:1] {
	case "s":
		return time.Duration(d) * time.Second, nil
	case "m":
		return time.Duration(d) * time.Minute, nil
	case "h":
		return time.Duration(d) * time.Hour, nil
	case "d":
		return time.Duration(d) * 24 * time.Hour, nil
	case "w":
		return time.Duration(d) * 7 * 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("invalid duration: invalid unit: %s", s)
}

func (r *LocalDiscordRegistry) InitializePartyBot(ctx context.Context, pipeline *Pipeline) error {

	r.bot.Identify.Intents |= discordgo.IntentGuilds
	r.bot.Identify.Intents |= discordgo.IntentGuildMembers
	r.bot.Identify.Intents |= discordgo.IntentDirectMessages
	r.bot.Identify.Intents |= discordgo.IntentDirectMessageReactions

	if err := RegisterPartySlashCommands(ctx, r, pipeline); err != nil {
		return err
	}

	if err := r.bot.Open(); err != nil {
		return err
	}
	return nil
}

// InitializeDiscordBot initializes the discord bot and synchronizes the guilds with nakama groups. It also registers the bot's handlers.
func (r *LocalDiscordRegistry) InitializeDiscordBot(ctx context.Context, logger runtime.Logger, nk runtime.NakamaModule, initializer runtime.Initializer) error {
	var err error
	bot := r.bot
	if bot == nil {
		return nil
	}

	// Verify that this is a runtime model context
	_, ok := ctx.Value(runtime.RUNTIME_CTX_NODE).(string)
	if !ok {
		return fmt.Errorf("context is not a runtime model context")
	}

	bot.Identify.Intents |= discordgo.IntentAutoModerationExecution
	bot.Identify.Intents |= discordgo.IntentMessageContent
	bot.Identify.Intents |= discordgo.IntentGuilds
	bot.Identify.Intents |= discordgo.IntentGuildMembers
	bot.Identify.Intents |= discordgo.IntentGuildBans
	bot.Identify.Intents |= discordgo.IntentGuildEmojis
	bot.Identify.Intents |= discordgo.IntentGuildWebhooks
	bot.Identify.Intents |= discordgo.IntentGuildInvites
	//bot.Identify.Intents |= discordgo.IntentGuildPresences
	bot.Identify.Intents |= discordgo.IntentGuildMessages
	bot.Identify.Intents |= discordgo.IntentGuildMessageReactions
	bot.Identify.Intents |= discordgo.IntentDirectMessages
	bot.Identify.Intents |= discordgo.IntentDirectMessageReactions
	bot.Identify.Intents |= discordgo.IntentMessageContent
	bot.Identify.Intents |= discordgo.IntentAutoModerationConfiguration
	bot.Identify.Intents |= discordgo.IntentAutoModerationExecution

	bot.AddHandler(func(session *discordgo.Session, ready *discordgo.Ready) {
		logger.Info("Discord bot is ready.")
	})

	bot.AddHandler(func(s *discordgo.Session, m *discordgo.Ready) {
		// Create a user for the bot based on it's discord profile
		_, _, _, err := nk.AuthenticateCustom(ctx, m.User.ID, s.State.User.Username, true)
		if err != nil {
			logger.Error("Error creating discordbot user: %s", err)
		}

		// Synchronize the guilds with nakama groups
		logger.Info("Bot is in %d guilds", len(s.State.Guilds))
		for _, g := range m.Guilds {
			g, err := s.Guild(g.ID)
			if err != nil {
				logger.Error("Error getting guild: %w", err)
				return
			}

			if err := r.SynchronizeGroup(ctx, g); err != nil {
				logger.Error("Error synchronizing group: %w", err)
				return
			}
		}
	})

	bot.AddHandler(func(se *discordgo.Session, m *discordgo.MessageCreate) {
		if se == nil || m == nil || m.Author == nil {
			return
		}

		if m.Author.ID == se.State.User.ID {
			return
		}
		if m.Author.ID != "155149108183695360" { // Dyno bot
			return
		}
		if m.Embeds == nil || len(m.Embeds) == 0 {
			return
		}
		guild, err := se.Guild(m.ID)
		if err != nil {
			logger.Error("Error getting guild: %w", err)
			return
		}

		suspensionStatus := &SuspensionStatus{
			GuildName: guild.Name,
			GuildId:   guild.ID,
		}
		e := m.Embeds[0]
		for _, f := range e.Fields {
			switch f.Name {
			case "User":
				suspensionStatus.UserDiscordId = strings.Trim(strings.Replace(strings.Replace(f.Value, "\\u003c", "<", -1), "\\u003e", ">", -1), "<@!>")
				suspensionStatus.UserId, err = r.GetUserIdByDiscordId(ctx, suspensionStatus.UserDiscordId, true)
				if err != nil {
					logger.Error("Error getting user id: %w", err)
					return
				}
			case "Moderator":
				suspensionStatus.ModeratorDiscordId = r.ReplaceMentions(m.GuildID, f.Value)
			case "Length":
				suspensionStatus.Duration, err = parseDuration(f.Value)
				if err != nil || suspensionStatus.Duration <= 0 {
					logger.Error("Error parsing duration: %w", err)
					return
				}
				suspensionStatus.Expiry = m.Timestamp.Add(suspensionStatus.Duration)
			case "Role":
				roles, err := se.GuildRoles(m.GuildID)
				if err != nil {
					logger.Error("Error getting guild roles: %w", err)
					return
				}
				for _, role := range roles {
					if role.Name == f.Value {
						suspensionStatus.RoleId = role.ID
						suspensionStatus.RoleName = role.Name
						break
					}
				}
			case "Reason":
				suspensionStatus.Reason = r.ReplaceMentions(m.GuildID, f.Value)
			}
		}

		if !suspensionStatus.Valid() {
			return
		}

		// Marshal it
		suspensionStatusBytes, err := json.Marshal(suspensionStatus)
		if err != nil {
			logger.Error("Error marshalling suspension status: %w", err)
			return
		}

		// Save the storage object.

		_, err = nk.StorageWrite(ctx, []*runtime.StorageWrite{
			{
				Collection:      SuspensionStatusCollection,
				Key:             m.GuildID,
				UserID:          suspensionStatus.UserId,
				Value:           string(suspensionStatusBytes),
				PermissionRead:  0,
				PermissionWrite: 0,
			},
		})
		if err != nil {
			logger.Error("Error writing suspension status: %w", err)
			return
		}

	})

	bot.AddHandler(func(s *discordgo.Session, m *discordgo.GuildBanAdd) {
		if s == nil || m == nil {
			return
		}

		if groupId, found := r.Get(m.GuildID); found {
			if user, err := r.GetUserIdByDiscordId(ctx, m.User.ID, true); err == nil {
				nk.GroupUsersKick(ctx, SystemUserId, groupId, []string{user})
			}
		}
	})

	bot.AddHandler(func(s *discordgo.Session, m *discordgo.GuildBanRemove) {
		if s == nil || m == nil {
			return
		}

		_, _ = r.GetUserIdByDiscordId(ctx, m.User.ID, true)
	})

	bot.AddHandler(func(s *discordgo.Session, m *discordgo.GuildMemberRemove) {
		if s == nil || m == nil {
			return
		}

		if groupId, found := r.Get(m.GuildID); found {
			if user, err := r.GetUserIdByDiscordId(ctx, m.User.ID, true); err == nil {
				_ = nk.GroupUserLeave(ctx, SystemUserId, groupId, user)
			}
		}
	})

	bot.AddHandler(func(s *discordgo.Session, e *discordgo.GuildMemberUpdate) {
		if s == nil || e == nil {
			return
		}
		discordId := e.User.ID
		// TODO FIXME Make this only update what changed.
		err := r.UpdateAccount(ctx, discordId)
		if err != nil {
			logger.Debug("Error updating account: %w", err)
		}
	})

	bot.AddHandler(func(s *discordgo.Session, m *discordgo.GuildMembersChunk) {
		if err := OnGuildMembersChunk(ctx, s, m, logger, nk, initializer); err != nil {
			logger.Error("Error calling OnGuildMembersChunk: %w", err)
		}
	})

	bot.AddHandler(func(s *discordgo.Session, e *discordgo.Ready) {
		if err = RegisterSlashCommands(ctx, logger, nk, s, r); err != nil {
			logger.Error("Failed to register slash commands: %w", err)
		}
	})

	if err = bot.Open(); err != nil {
		logger.Error("Failed to open discord bot connection: %w", err)
	}

	logger.Info("Discord bot started: %s", bot.State.User.String())
	return nil
}

// Helper function to get or create a group
func (r *LocalDiscordRegistry) findOrCreateGroup(ctx context.Context, groupId, name, description, ownerId, langtype string, guild *discordgo.Guild) (*api.Group, error) {
	nk := r.nk
	var group *api.Group

	// Try to retrieve the group by ID
	if groupId != "" {
		groups, err := nk.GroupsGetId(ctx, []string{groupId})
		if err != nil {
			return nil, fmt.Errorf("error getting group by id: %w", err)
		}
		if len(groups) != 0 {
			group = groups[0]
		}
	}

	// Next attempt to find the group by name.
	if group == nil {
		if groups, _, err := nk.GroupsList(ctx, name, "", nil, nil, 1, ""); err == nil && len(groups) == 1 {
			group = groups[0]
		}
	}
	// If the group was found, update the lookup table

	// If the group wasn't found, create it
	if group == nil {
		md := NewGuildGroupMetadata(guild.ID, "", "", "")
		gm, err := md.MarshalToMap()
		if err != nil {
			return nil, fmt.Errorf("error marshalling group metadata: %w", err)
		}
		// Create the group
		group, err = nk.GroupCreate(ctx, r.botUserId, name, ownerId, langtype, description, guild.IconURL("512"), false, gm, 100000)
		if err != nil {
			return nil, fmt.Errorf("error creating group: %w", err)
		}
	}

	if langtype == "guild" {
		// Set the group in the registry
		r.Store(guild.ID, group.GetId())
	}

	return group, nil
}

func (r *LocalDiscordRegistry) SynchronizeGroup(ctx context.Context, guild *discordgo.Guild) error {
	var err error

	// Get the owner's nakama user
	ownerId, err := r.GetUserIdByDiscordId(ctx, guild.OwnerID, true)
	if err != nil {
		return fmt.Errorf("error getting guild owner id: %w", err)
	}

	// Check the lookup table for the guild group
	groupId, found := r.Get(guild.ID)
	if !found {
		groupId = ""
	}

	// Find or create the guild group
	guildGroup, err := r.findOrCreateGroup(ctx, groupId, guild.Name, guild.Description, ownerId, "guild", guild)
	if err != nil {
		return fmt.Errorf("findcreategroup: %w", err)
	}

	// Unmarshal the guild group's metadata for updating.
	guildMetadata := &GroupMetadata{}
	if err := json.Unmarshal([]byte(guildGroup.GetMetadata()), guildMetadata); err != nil {
		return fmt.Errorf("error unmarshalling group metadata: %w", err)
	}

	// Set the group Id in the metadata so it can be found during an error.
	guildMetadata.GuildId = guild.ID

	// Find or create the moderator role group
	moderatorGroup, err := r.findOrCreateGroup(ctx, guildMetadata.ModeratorGroupId, guild.Name+" Moderators", guild.Name+" Moderators", ownerId, "role", guild)
	if err != nil {
		return fmt.Errorf("error getting or creating moderator group: %w", err)
	}
	guildMetadata.ModeratorGroupId = moderatorGroup.Id

	// Find or create the server role group
	serverGroup, err := r.findOrCreateGroup(ctx, guildMetadata.BroadcasterHostGroupId, guild.Name+" Broadcaster Hosts", guild.Name+" Broadcaster Hosts", ownerId, "role", guild)
	if err != nil {
		return fmt.Errorf("error getting or creating server group: %w", err)
	}
	guildMetadata.BroadcasterHostGroupId = serverGroup.Id

	// Set a default rules, or get the rules from the channel topic
	guildMetadata.RulesText = "No #rules channel found. Please create the channel and set the topic to the rules."
	channels, err := r.bot.GuildChannels(guild.ID)
	if err != nil {
		return fmt.Errorf("error getting guild channels: %w", err)
	}
	for _, channel := range channels {
		if channel.Type == discordgo.ChannelTypeGuildText && channel.Name == "rules" {
			guildMetadata.RulesText = channel.Topic
			break
		}
	}

	// Rewrite the guild groups metadata
	md, err := guildMetadata.MarshalToMap()
	if err != nil {
		return fmt.Errorf("error marshalling group metadata: %w", err)
	}

	// Update the guild group
	if err := r.nk.GroupUpdate(ctx, guildGroup.GetId(), r.botUserId, guild.Name, ownerId, "guild", guild.Description, guild.IconURL("512"), false, md, 100000); err != nil {
		return fmt.Errorf("error updating guild group: %w", err)
	}

	return nil
}

func OnGuildMembersChunk(ctx context.Context, b *discordgo.Session, e *discordgo.GuildMembersChunk, logger runtime.Logger, nk runtime.NakamaModule, initializer runtime.Initializer) error {
	// Get the nakama group for the guild

	// Add all the members of the guild to the group, in chunks
	chunkSize := 10
	logger.Debug("Received guild member chunk %d of %d", chunkSize, len(e.Members))

	for i := 0; i < len(e.Members); i += chunkSize {
		members := e.Members[i:min(i+chunkSize, len(e.Members))]
		usernames := make([]string, len(members))
		for i, member := range members {
			usernames[i] = member.User.Username
		}

		users, err := nk.UsersGetUsername(ctx, usernames)
		if err != nil {
			return fmt.Errorf("error getting account Ids for guild members: %w", err)
		}

		accountIds := make([]string, len(users))
		for _, user := range users {
			accountIds[i] = user.Id
		}
		// Add the member to the group
		if err := nk.GroupUsersAdd(ctx, SystemUserId, members[0].GuildID, accountIds); err != nil {
			return fmt.Errorf("group add users error: %w", err)
		}
	}
	return nil
}

func (r *LocalDiscordRegistry) GetAllSuspensions(ctx context.Context, userId string) ([]*SuspensionStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Get the discordId for the userId
	discordId, err := r.GetDiscordIdByUserId(ctx, userId)
	if err != nil {
		return nil, err
	}

	// Get a list of the bot's guilds
	groups, err := r.GetGuildGroups(ctx, userId)
	if err != nil {
		return nil, err
	}

	// Get the metadata for each guild and it's suspension roles
	suspensions := make([]*SuspensionStatus, 0)
	for _, group := range groups {
		md := &GroupMetadata{}
		if err := json.Unmarshal([]byte(group.GetMetadata()), md); err != nil {
			return nil, fmt.Errorf("error unmarshalling group metadata: %w", err)
		}
		// Get the guild member's roles
		member, err := r.GetGuildMember(ctx, md.GuildId, discordId)
		if err != nil {
			return nil, fmt.Errorf("error getting guild member: %w", err)
		}
		// Look for an intersection between suspension roles and the member's roles
		intersections := lo.Intersect(member.Roles, md.SuspensionRoles)
		for _, roleId := range intersections {
			// Get the role's name
			role, err := r.bot.State.Role(md.GuildId, roleId)
			if err != nil {
				return nil, fmt.Errorf("error getting guild role: %w", err)
			}
			status := &SuspensionStatus{
				GuildId:       group.Id,
				GuildName:     group.Name,
				UserDiscordId: discordId,
				UserId:        userId,
				RoleId:        roleId,
				RoleName:      role.Name,
			}
			// Apppend the suspension status to the list
			suspensions = append(suspensions, status)
		}
	}
	return suspensions, nil
}