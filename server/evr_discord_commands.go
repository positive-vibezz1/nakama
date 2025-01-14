package server

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gofrs/uuid/v5"
	"github.com/google/go-cmp/cmp"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/heroiclabs/nakama/v3/server/evr"
	"github.com/samber/lo"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type DiscordAppBot struct {
	sync.Mutex

	ctx      context.Context
	cancelFn context.CancelFunc
	logger   runtime.Logger

	config          Config
	metrics         Metrics
	pipeline        *Pipeline
	profileRegistry *ProfileRegistry
	statusRegistry  StatusRegistry
	nk              runtime.NakamaModule
	db              *sql.DB
	dg              *discordgo.Session

	cache *DiscordCache

	debugChannels map[string]string // map[groupID]channelID
	userID        string            // Nakama UserID of the bot

	prepareMatchRatePerMinute rate.Limit
	prepareMatchBurst         int
	prepareMatchRateLimiters  *MapOf[string, *rate.Limiter]
}

func NewDiscordAppBot(logger runtime.Logger, nk runtime.NakamaModule, db *sql.DB, metrics Metrics, pipeline *Pipeline, config Config, discordCache *DiscordCache, profileRegistry *ProfileRegistry, statusRegistry StatusRegistry, dg *discordgo.Session) (*DiscordAppBot, error) {
	ctx, cancelFn := context.WithCancel(context.Background())
	logger = logger.WithField("system", "discordAppBot")

	appbot := DiscordAppBot{
		ctx:      ctx,
		cancelFn: cancelFn,

		logger:   logger,
		nk:       nk,
		db:       db,
		pipeline: pipeline,
		metrics:  metrics,
		config:   config,

		profileRegistry: profileRegistry,
		statusRegistry:  statusRegistry,

		cache: discordCache,

		dg: dg,

		prepareMatchRatePerMinute: 1,
		prepareMatchBurst:         1,
		prepareMatchRateLimiters:  &MapOf[string, *rate.Limiter]{},
		debugChannels:             make(map[string]string),
	}

	bot := dg
	//bot.LogLevel = discordgo.LogDebug
	dg.StateEnabled = true

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

	bot.AddHandlerOnce(func(s *discordgo.Session, m *discordgo.Ready) {

		// Create a user for the bot based on it's discord profile
		userID, _, _, err := nk.AuthenticateCustom(ctx, m.User.ID, s.State.User.Username, true)
		if err != nil {
			logger.Error("Error creating discordbot user: %s", err)
		}
		appbot.userID = userID
		// Synchronize the guilds with nakama groups

		displayName := bot.State.User.GlobalName
		if displayName == "" {
			displayName = bot.State.User.Username
		}

		if err := appbot.RegisterSlashCommands(); err != nil {
			logger.Error("Failed to register slash commands: %w", err)
		}

		logger.Info("Bot `%s` ready in %d guilds", displayName, len(bot.State.Guilds))
	})

	bot.AddHandler(func(s *discordgo.Session, m *discordgo.RateLimit) {
		logger.WithField("rate_limit", m).Warn("Discord rate limit")
	})

	// Update the status with the number of matches and players
	go func() {
		updateTicker := time.NewTicker(1 * time.Minute)
		defer updateTicker.Stop()
		for {
			select {
			case <-updateTicker.C:
				if !bot.DataReady {
					continue
				}

				// Get all the matches
				minSize := 2
				maxSize := MatchLobbyMaxSize + 1
				matches, err := nk.MatchList(ctx, 1000, true, "", &minSize, &maxSize, "*")
				if err != nil {
					logger.WithField("err", err).Warn("Error fetching matches.")
					continue
				}
				playerCount := 0
				matchCount := 0
				for _, match := range matches {
					playerCount += int(match.Size) - 1
					matchCount++

				}
				status := fmt.Sprintf("with %d players in %d matches", playerCount, matchCount)
				if err := bot.UpdateGameStatus(0, status); err != nil {
					logger.WithField("err", err).Warn("Failed to update status")
					continue
				}

			case <-ctx.Done():
				updateTicker.Stop()
				return
			}
		}
	}()

	return &appbot, nil
}

func (e *DiscordAppBot) loadPrepareMatchRateLimiter(userID, groupID string) *rate.Limiter {
	key := strings.Join([]string{userID, groupID}, ":")
	limiter, _ := e.prepareMatchRateLimiters.LoadOrStore(key, rate.NewLimiter(e.prepareMatchRatePerMinute, e.prepareMatchBurst))
	return limiter
}

var (
	vrmlMap = map[string]string{
		"p":  "VRML Season Preseason",
		"1":  "VRML Season 1",
		"1f": "VRML Season 1 Finalist",
		"1c": "VRML Season 1 Champion",
		"2":  "VRML Season 2",
		"2f": "VRML Season 2 Finalist",
		"2c": "VRML Season 2 Champion",
		"3":  "VRML Season 3",
		"3f": "VRML Season 3 Finalist",
		"3c": "VRML Season 3 Champion",
		"4":  "VRML Season 4",
		"4f": "VRML Season 4 Finalist",
		"4c": "VRML Season 4 Champion",
		"5":  "VRML Season 5",
		"5f": "VRML Season 5 Finalist",
		"5c": "VRML Season 5 Champion",
		"6":  "VRML Season 6",
		"6f": "VRML Season 6 Finalist",
		"6c": "VRML Season 6 Champion",
		"7":  "VRML Season 7",
		"7f": "VRML Season 7 Finalist",
		"7c": "VRML Season 7 Champion",
	}

	partyGroupIDPattern = regexp.MustCompile("^[a-z0-9]+$")
	vrmlIDPattern       = regexp.MustCompile("^[-a-zA-Z0-9]{24}$")

	mainSlashCommands = []*discordgo.ApplicationCommand{

		{
			Name:        "link-headset",
			Description: "Link your headset device to your discord account.",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "link-code",
					Description: "Your four character link code.",
					Required:    true,
				},
			},
		},
		{
			Name:        "unlink-headset",
			Description: "Unlink a headset from your discord account.",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:         discordgo.ApplicationCommandOptionString,
					Name:         "device-link",
					Description:  "device link from /whoami",
					Required:     false,
					Autocomplete: true,
				},
			},
		},
		{
			Name:        "check-server",
			Description: "Check if a game server is actively responding on a port.",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "address",
					Description: "host:port of the game server",
					Required:    true,
				},
			},
		},
		{
			Name:        "reset-password",
			Description: "Clear your echo password.",
		},
		{
			Name:        "whoami",
			Description: "Receive your account information (privately).",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionBoolean,
					Name:        "include-detail",
					Description: "Include extra details",
					Required:    false,
				},
			},
		},
		{
			Name:        "set-lobby",
			Description: "Set your default lobby to this Discord server/guild.",
		},
		{
			Name:        "lookup",
			Description: "Lookup information about players.",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionUser,
					Name:        "user",
					Description: "User to lookup",
					Required:    true,
				},
			},
		},
		{
			Name:        "search",
			Description: "Search for a player by display name, user ID, or XPI (i.e. OVR-ORG-).",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "pattern",
					Description: "Partial name to use in search pattern",
					Required:    true,
				},
			},
		},
		{
			Name:        "hash",
			Description: "Convert a string into a symbol hash.",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "token",
					Description: "string to convert",
					Required:    true,
				},
			},
		},
		/*
			{
				Name:        "kick",
				Description: "Force user to go through community values in the social lobby.",
				Options:     []*discordgo.ApplicationCommandOption{},
			},
		*/
		{
			Name:        "trigger-cv",
			Description: "Force user to go through community values in the social lobby.",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:         discordgo.ApplicationCommandOptionUser,
					Name:         "user",
					Description:  "Target user",
					Required:     true,
					Autocomplete: true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "reason",
					Description: "Reason for the CV",
					Required:    true,
				},
			},
		},
		{
			Name:        "join-player",
			Description: "Join a player's session as a moderator.",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionUser,
					Name:        "user",
					Description: "Target user",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "reason",
					Description: "Reason for joining the player's session.",
					Required:    true,
				},
			},
		},
		{
			Name:        "kick-player",
			Description: "Kick a player's sessions.",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionUser,
					Name:        "user",
					Description: "Target user",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "reason",
					Description: "Reason for the kick",
					Required:    true,
				},
			},
		},
		{
			Name:        "jersey-number",
			Description: "Set your in-game jersey number.",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionInteger,
					Name:        "number",
					Description: "Your jersey number, that will be displayed when you select loadout number as your decal.",
					Required:    true,
				},
			},
		},
		{
			Name:        "badges",
			Description: "manage badge entitlements",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Name:        "assign",
					Description: "assign badges to a player",
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Options: []*discordgo.ApplicationCommandOption{
						{
							Type:        discordgo.ApplicationCommandOptionUser,
							Name:        "user",
							Description: "target user",
							Required:    true,
						},
						{
							Type:        discordgo.ApplicationCommandOptionString,
							Name:        "badges",
							Description: "comma seperated list of badges (i.e p,1,2,5c,6f)",
							Required:    true,
						},
					},
				},
			},
		},
		{
			Name:        "stream-list",
			Description: "list presences for a stream",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionInteger,
					Name:        "mode",
					Description: "the stream mode",
					Required:    true,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{
							Name:  "Party",
							Value: StreamModeParty,
						},
						{
							Name:  "Match",
							Value: StreamModeMatchAuthoritative,
						},
						{
							Name:  "GameServer",
							Value: StreamModeGameServer,
						},
						{
							Name:  "Service",
							Value: StreamModeService,
						},
						{
							Name:  "Entrant",
							Value: StreamModeEntrant,
						},
						{
							Name:  "Matchmaking",
							Value: StreamModeMatchmaking,
						},
						{
							Name:  "Channel",
							Value: StreamModeChannel,
						},
						{
							Name:  "Group",
							Value: StreamModeGroup,
						},
						{
							Name:  "DM",
							Value: StreamModeDM,
						},
					},
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "subject",
					Description: "stream subject",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "subcontext",
					Description: "stream subcontext",
					Required:    false,
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "label",
					Description: "stream label",
					Required:    false,
				},
				{
					Type:        discordgo.ApplicationCommandOptionInteger,
					Name:        "limit",
					Description: "limit the number of results",
					Required:    false,
				},
			},
		},
		{
			Name:        "set-roles",
			Description: "link roles to Echo VR features. Non-members can only join private matches.",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionRole,
					Name:        "member",
					Description: "If defined, this role allows joining social lobbies, matchmaking, or creating private matches.",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionRole,
					Name:        "moderator",
					Description: "Allowed access to more detailed `/lookup`information and moderation tools.",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionRole,
					Name:        "serverhost",
					Description: "Allowed to host an Echo VR Game Server for the guild.",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionRole,
					Name:        "suspension",
					Description: "Disallowed from joining any guild matches.",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionRole,
					Name:        "is-linked",
					Description: "Assigned/Removed by Nakama denoting if an account is linked to a headset.",
					Required:    true,
				},
			},
		},
		{
			Name:        "set-roles",
			Description: "link roles to Echo VR features. Non-members can only join private matches.",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionRole,
					Name:        "member",
					Description: "If defined, this role allows joining social lobbies, matchmaking, or creating private matches.",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionRole,
					Name:        "moderator",
					Description: "Allowed access to more detailed `/lookup`information and moderation tools.",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionRole,
					Name:        "serverhost",
					Description: "Allowed to host an Echo VR Game Server for the guild.",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionRole,
					Name:        "allocator",
					Description: "Allowed to reserve game servers.",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionRole,
					Name:        "suspension",
					Description: "Disallowed from joining any guild matches.",
					Required:    true,
				},
			},
		},
		{
			Name:        "allocate",
			Description: "Allocate a session on a game server in a specific region",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "mode",
					Description: "Game mode",
					Required:    true,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{
							Name:  "Echo Arena Private",
							Value: "echo_arena_private",
						},
						{
							Name:  "Echo Arena Public",
							Value: "echo_arena",
						},
						{
							Name:  "Echo Combat Public",
							Value: "echo_combat",
						},
						{
							Name:  "Echo Combat Private",
							Value: "echo_combat_private",
						},
						{
							Name:  "Social Private",
							Value: "social_2.0_private",
						},
					},
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "region",
					Description: "Region to allocate the session in",
					Required:    false,
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "level",
					Description: "Level for the the session",
					Required:    false,
					Choices: func() []*discordgo.ApplicationCommandOptionChoice {
						choices := make([]*discordgo.ApplicationCommandOptionChoice, 0)
						for _, levels := range evr.LevelsByMode {
							for _, level := range levels {
								choices = append(choices, &discordgo.ApplicationCommandOptionChoice{
									Name:  level.Token().String(),
									Value: level,
								})
							}
						}
						return choices
					}(),
				},
			},
		},
		{
			Name:        "create",
			Description: "Create an EVR game session on a game server in a specific region",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "mode",
					Description: "Game mode",
					Required:    true,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{
							Name:  "Private Arena Match",
							Value: "echo_arena_private",
						},
						{
							Name:  "Private Combat Match",
							Value: "echo_combat_private",
						},
						{
							Name:  "Private Social Lobby",
							Value: "social_2.0_private",
						},
						{
							Name:  "Public Arena Match",
							Value: "echo_arena",
						},
						{
							Name:  "Public Combat Match",
							Value: "echo_combat",
						},
					},
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "region",
					Description: "Region to allocate the session in (leave blank to use the best server for you)",
					Required:    false,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{
							Name:  "US Central North (Chicago)",
							Value: "us-central-north",
						},
						{
							Name:  "US Central South (Texas)",
							Value: "us-central-south",
						},
						{
							Name:  "US Central South",
							Value: "us-east",
						},
						{
							Name:  "US West",
							Value: "us-west",
						},
						{
							Name:  "EU West",
							Value: "eu-west",
						},
						{
							Name:  "Japan",
							Value: "jp",
						},
						{
							Name:  "Singapore",
							Value: "sin",
						},
						{
							Name:  "Vibinator",
							Value: "82be4f8d-7504-4b67-8411-ce80c17bdf65",
						},
					},
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "level",
					Description: "Level for the the session (combat only)",
					Required:    false,
					Choices: func() []*discordgo.ApplicationCommandOptionChoice {
						choices := make([]*discordgo.ApplicationCommandOptionChoice, 0)
						for _, level := range evr.LevelsByMode[evr.ModeCombatPublic] {
							choices = append(choices, &discordgo.ApplicationCommandOptionChoice{
								Name:  level.Token().String(),
								Value: level,
							})
						}
						return choices
					}(),
				},
			},
		},
		{
			Name:        "region-status",
			Description: "Get the status of game servers in a specific region",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "region",
					Description: "Region to check the status of",
					Required:    true,
				},
			},
		},
		{
			Name:        "party",
			Description: "Manage EchoVR parties.",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Name:        "group",
					Description: "Set your matchmaking group name.",
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Options: []*discordgo.ApplicationCommandOption{
						{
							Type:        discordgo.ApplicationCommandOptionString,
							Name:        "group-name",
							Description: "Your matchmaking group name.",
							Required:    true,
						},
					},
				},
				{
					Name:        "members",
					Description: "See members of your party.",
					Type:        discordgo.ApplicationCommandOptionSubCommand,
				},
				/*
					{
						Name:        "invite",
						Description: "Invite a user to your party.",
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Options: []*discordgo.ApplicationCommandOption{
							{
								Type:        discordgo.ApplicationCommandOptionUser,
								Name:        "user-option",
								Description: "User to invite to your party.",
								Required:    false,
							},
						},
					},

					{
						Name:        "invite",
						Description: "Invite a user to your party.",
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Options: []*discordgo.ApplicationCommandOption{
							{
								Type:        discordgo.ApplicationCommandOptionUser,
								Name:        "user-option",
								Description: "User to invite to your party.",
								Required:    false,
							},
						},
					},
					{
						Name:        "cancel",
						Description: "cancel a party invite (or leave blank to cancel all).",
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Options: []*discordgo.ApplicationCommandOption{
							{
								Type:        discordgo.ApplicationCommandOptionUser,
								Name:        "user-option",
								Description: "User to cancel invite for.",
								Required:    false,
							},
						},
					},
					{
						Name:        "transfer",
						Description: "Make another user the party leader.",
						Type:        discordgo.ApplicationCommandOptionSubCommand,
						Options: []*discordgo.ApplicationCommandOption{
							{
								Type:        discordgo.ApplicationCommandOptionUser,
								Name:        "user-option",
								Description: "User to transfer party to.",
								Required:    true,
							},
						},
					},
					{
						Name:        "help",
						Description: "Help with party commands.",
						Type:        discordgo.ApplicationCommandOptionSubCommand,
					},
					{
						Name:        "status",
						Description: "Status of your party.",
						Type:        discordgo.ApplicationCommandOptionSubCommand,
					},
					{
						Name:        "warp",
						Description: "Warp your party to your lobby.",
						Type:        discordgo.ApplicationCommandOptionSubCommand,
					},
					{
						Name:        "leave",
						Description: "Leave the party.",
						Type:        discordgo.ApplicationCommandOptionSubCommand,
					},
				*/
			},
		},

		/*
			{
				Name:        "responses",
				Description: "Interaction responses testing initiative",
				Options: []*discordgo.ApplicationCommandOption{
					{
						Name:        "resp-type",
						Description: "Response type",
						Type:        discordgo.ApplicationCommandOptionInteger,
						Choices: []*discordgo.ApplicationCommandOptionChoice{
							{
								Name:  "Channel message with source",
								Value: 4,
							},
							{
								Name:  "Deferred response With Source",
								Value: 5,
							},
						},
						Required: true,
					},
				},
			},
		*/
	}
)

// InitializeDiscordBot initializes the discord bot and synchronizes the guilds with nakama groups. It also registers the bot's handlers.
func (d *DiscordAppBot) InitializeDiscordBot() error {

	bot := d.dg
	if bot == nil {
		return nil
	}

	return nil
}

func (d *DiscordAppBot) UnregisterCommandsAll(ctx context.Context, logger runtime.Logger, dg *discordgo.Session) {
	guilds, err := dg.UserGuilds(100, "", "", false)
	if err != nil {
		logger.Error("Error fetching guilds,", zap.Error(err))
		return
	}
	for _, guild := range guilds {
		d.UnregisterCommands(ctx, logger, dg, guild.ID)
	}

}

// If guildID is empty, it will unregister all global commands.
func (d *DiscordAppBot) UnregisterCommands(ctx context.Context, logger runtime.Logger, dg *discordgo.Session, guildID string) {
	commands, err := dg.ApplicationCommands(dg.State.User.ID, guildID)
	if err != nil {
		logger.Error("Error fetching commands,", zap.Error(err))
		return
	}

	for _, command := range commands {
		err := dg.ApplicationCommandDelete(dg.State.User.ID, guildID, command.ID)
		if err != nil {
			logger.Error("Error deleting command,", zap.Error(err))
		} else {
			logger.Info("Deleted command", zap.String("command", command.Name))
		}
	}
}

type DiscordCommandHandlerFn func(logger runtime.Logger, s *discordgo.Session, i *discordgo.InteractionCreate, user *discordgo.User, member *discordgo.Member, userID string, groupID string) error

func (d *DiscordAppBot) RegisterSlashCommands() error {
	ctx := d.ctx
	nk := d.nk
	db := d.db
	dg := d.dg

	commandHandlers := map[string]DiscordCommandHandlerFn{

		"hash": func(logger runtime.Logger, s *discordgo.Session, i *discordgo.InteractionCreate, user *discordgo.User, member *discordgo.Member, userID string, groupID string) error {
			options := i.ApplicationCommandData().Options
			if len(options) == 0 {
				return errors.New("no options provided")
			}
			token := options[0].StringValue()
			symbol := evr.ToSymbol(token)
			bytes := binary.LittleEndian.AppendUint64([]byte{}, uint64(symbol))

			return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Flags: discordgo.MessageFlagsEphemeral,
					Embeds: []*discordgo.MessageEmbed{
						{
							Title: token,
							Color: 0xCCCCCC,
							Fields: []*discordgo.MessageEmbedField{
								{
									Name:   "uint64",
									Value:  strconv.FormatUint(uint64(symbol), 10),
									Inline: false,
								},
								{
									Name:   "int64",
									Value:  strconv.FormatInt(int64(symbol), 10),
									Inline: false,
								},
								{
									Name:   "hex",
									Value:  symbol.HexString(),
									Inline: false,
								},
								{
									Name:   "cached?",
									Value:  strconv.FormatBool(lo.Contains(lo.Keys(evr.SymbolCache), symbol)),
									Inline: false,
								},
								{
									Name:   "LE bytes",
									Value:  fmt.Sprintf("%#v", bytes),
									Inline: false,
								},
							},
						},
					},
				},
			})
		},

		"link-headset": func(logger runtime.Logger, s *discordgo.Session, i *discordgo.InteractionCreate, user *discordgo.User, member *discordgo.Member, userID string, groupID string) error {
			options := i.ApplicationCommandData().Options
			if len(options) == 0 {
				return errors.New("no options provided")
			}
			linkCode := options[0].StringValue()

			if user == nil {
				return nil
			}

			// Validate the link code as a 4 character string
			if len(linkCode) != 4 {
				return errors.New("invalid link code: link code must be (4) letters long (i.e. ABCD)")
			}

			if err := func() error {

				// Exchange the link code for a device auth.
				ticket, err := ExchangeLinkCode(ctx, nk, logger, linkCode)
				if err != nil {
					return fmt.Errorf("failed to exchange link code: %w", err)
				}

				// Authenticate/create an account.
				if userID == "" {
					userID, _, _, err = d.nk.AuthenticateCustom(ctx, user.ID, user.Username, true)
					if err != nil {
						return fmt.Errorf("failed to authenticate (or create) user %s: %w", user.ID, err)
					}
				}

				if err := d.nk.GroupUserJoin(ctx, groupID, userID, user.Username); err != nil {
					return fmt.Errorf("error joining group: %w", err)
				}

				if err := nk.LinkDevice(ctx, userID, ticket.XPID.Token()); err != nil {
					return fmt.Errorf("failed to link headset: %w", err)
				}

				// Set the client IP as authorized in the LoginHistory
				history, err := LoginHistoryLoad(ctx, nk, userID)
				if err != nil {
					return fmt.Errorf("failed to load login history: %w", err)
				}

				history.AuthorizeIP(ticket.ClientIP)

				if err := LoginHistoryStore(ctx, nk, userID, history); err != nil {
					return fmt.Errorf("failed to save login history: %w", err)
				}
				return nil
			}(); err != nil {
				logger.WithFields(map[string]interface{}{
					"discord_id": user.ID,
					"link_code":  linkCode,
					"error":      err,
				}).Error("Failed to link headset")
				return err
			}

			content := "Your headset has been linked. Restart EchoVR."

			d.cache.QueueSyncMember(i.GuildID, user.ID)

			// Send the response
			return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Flags:   discordgo.MessageFlagsEphemeral,
					Content: content,
				},
			})
		},
		"kick": func(logger runtime.Logger, s *discordgo.Session, i *discordgo.InteractionCreate, user *discordgo.User, member *discordgo.Member, userID string, groupID string) error {
			return fmt.Errorf("not implemented")
			/*
				response := &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Content: "Select a player to kick",
						Flags:   discordgo.MessageFlagsEphemeral,
						Components: []discordgo.MessageComponent{
							discordgo.ActionsRow{
								Components: []discordgo.MessageComponent{
									discordgo.SelectMenu{
										CustomID:    "select",
										Placeholder: "<select a player to kick>",
										Options:     options,
									},
								},
							},
							discordgo.ActionsRow{
								Components: []discordgo.MessageComponent{
									discordgo.SelectMenu{
										CustomID:    "trigger_cv",
										Placeholder: "Send user through Community Values?",
										Options: []discordgo.SelectMenuOption{
											{
												Label: "Yes",
												Value: "yes",
											},
											{
												Label: "No",
												Value: "no",
											},
										},
									},
								},
							},
							discordgo.ActionsRow{
								Components: []discordgo.MessageComponent{
									discordgo.SelectMenu{
										CustomID:    "reason",
										Placeholder: "<select a reason>",
										Options: []discordgo.SelectMenuOption{
											{
												Label: "Toxicity",
												Value: "toxicity",
											},
											{
												Label: "Poor Sportsmanship",
												Value: "poor_sportsmanship",
											},
											{
												Label: "Other (see below)",
												Value: "custom_reason",
											},
										},
									},
								},
							},
							discordgo.ActionsRow{
								Components: []discordgo.MessageComponent{
									discordgo.TextInput{
										CustomID:    "custom_reason_input",
										Label:       "Custom Reason",
										Style:       discordgo.TextInputParagraph,
										Placeholder: "Enter custom reason here...",
										Required:    false,
									},
								},
							},
						},
					},
				}
				return s.InteractionRespond(i.Interaction, response)
				// Show the players currently in the same match as the user
				// Get the user's current match

					presences, err := nk.StreamUserList(StreamModeService, userID, "", StreamLabelMatchService, false, true)
					if err != nil {
						logger.Error("Failed to get user list", zap.Error(err))
					}

					if len(presences) == 0 {
						if err := simpleInteractionResponse(s, i, "You are not in a match"); err != nil {
							return
						}
					}

					// Get the match label
					label, err := MatchLabelByID(ctx, d.nk, MatchIDFromStringOrNil(presences[0].GetStatus()))
					if err != nil {
						logger.Error("Failed to get match label", zap.Error(err))
					}

					if label == nil {
						if err := simpleInteractionResponse(s, i, "You are not in a match"); err != nil {
							return
						}
					}

					choices = make([]*discordgo.ApplicationCommandOptionChoice, len(presences))
					for i, player := range label.Players {
						choices[i] = &discordgo.ApplicationCommandOptionChoice{
							Name:  player.DisplayName,
							Value: player.UserID,
						}
					}
			*/
		},
		"unlink-headset": func(logger runtime.Logger, s *discordgo.Session, i *discordgo.InteractionCreate, user *discordgo.User, member *discordgo.Member, userID string, groupID string) error {
			options := i.ApplicationCommandData().Options
			if len(options) == 0 {

				account, err := nk.AccountGetId(ctx, userID)
				if err != nil {
					logger.Error("Failed to get account", zap.Error(err))
					return err
				}
				if len(account.Devices) == 0 {
					return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
						Type: discordgo.InteractionResponseChannelMessageWithSource,
						Data: &discordgo.InteractionResponseData{
							Flags:   discordgo.MessageFlagsEphemeral,
							Content: "No headsets are linked to this account.",
						},
					})
				}

				loginHistory, err := LoginHistoryLoad(ctx, nk, userID)
				if err != nil {
					return err
				}

				options := make([]discordgo.SelectMenuOption, 0, len(account.Devices))
				for _, device := range account.Devices {

					description := ""
					if ts, ok := loginHistory.XPIs[device.GetId()]; ok {
						hours := int(time.Since(ts).Hours())
						if hours < 1 {
							minutes := int(time.Since(ts).Minutes())
							if minutes < 1 {
								description = "Just now"
							} else {
								description = fmt.Sprintf("%d minutes ago", minutes)
							}
						} else if hours < 24 {
							description = fmt.Sprintf("%d hours ago", hours)
						} else {
							description = fmt.Sprintf("%d days ago", int(time.Since(ts).Hours()/24))
						}
					}

					options = append(options, discordgo.SelectMenuOption{
						Label: device.GetId(),
						Value: device.GetId(),
						Emoji: &discordgo.ComponentEmoji{
							Name: "🔗",
						},
						Description: description,
					})
				}

				response := &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Content: "Select a device to unlink",
						Flags:   discordgo.MessageFlagsEphemeral,
						Components: []discordgo.MessageComponent{
							discordgo.ActionsRow{
								Components: []discordgo.MessageComponent{
									discordgo.SelectMenu{
										// Select menu, as other components, must have a customID, so we set it to this value.
										CustomID:    "unlink-headset",
										Placeholder: "<select a device to unlink>",
										Options:     options,
									},
								},
							},
						},
					},
				}
				return s.InteractionRespond(i.Interaction, response)
			}
			xpid := options[0].StringValue()
			// Validate the link code as a 4 character string

			if user == nil {
				return nil
			}

			if err := func() error {

				return nk.UnlinkDevice(ctx, userID, xpid)

			}(); err != nil {
				return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Flags:   discordgo.MessageFlagsEphemeral,
						Content: err.Error(),
					},
				})
			}

			content := "Your headset has been unlinked. Restart EchoVR."
			d.cache.QueueSyncMember(i.GuildID, user.ID)

			return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Flags:   discordgo.MessageFlagsEphemeral,
					Content: content,
				},
			})
		},
		"check-server": func(logger runtime.Logger, s *discordgo.Session, i *discordgo.InteractionCreate, user *discordgo.User, member *discordgo.Member, userID string, groupID string) error {

			options := i.ApplicationCommandData().Options
			if len(options) == 0 {
				return errors.New("no options provided")

			}
			target := options[0].StringValue()

			// 1.1.1.1[:6792[-6820]]
			parts := strings.SplitN(target, ":", 2)
			if len(parts) == 0 {
				return errors.New("no address provided")

			}
			if parts[0] == "" {
				return errors.New("invalid address")

			}
			// Parse the address
			remoteIP := net.ParseIP(parts[0])
			if remoteIP == nil {
				// Try resolving the hostname
				ips, err := net.LookupIP(parts[0])
				if err != nil {
					return fmt.Errorf("failed to resolve address: %w", err)
				}
				// Select the ipv4 address
				for _, remoteIP = range ips {
					if remoteIP.To4() != nil {
						break
					}
				}
				if remoteIP == nil {
					return errors.New("failed to resolve address to an ipv4 address")

				}
			}

			// Parse the (optional) port range
			var startPort, endPort int
			var err error
			if len(parts) > 1 {
				// If a port range is specified, scan the specified range
				portRange := strings.SplitN(parts[1], "-", 2)
				if startPort, err = strconv.Atoi(portRange[0]); err != nil {
					return fmt.Errorf("invalid start port: %w", err)
				}
				if len(portRange) == 1 {
					// If a single port is specified, do not scan
					endPort = startPort
				} else {
					// If a port range is specified, scan the specified range
					if endPort, err = strconv.Atoi(portRange[1]); err != nil {
						return fmt.Errorf("invalid end port: %w", err)
					}
				}
			} else {
				// If no port range is specified, scan the default port
				startPort = 6792
				endPort = 6820
			}

			// Do some basic validation
			switch {
			case remoteIP == nil:
				return errors.New("invalid IP address")

			case startPort < 0:
				return errors.New("start port must be greater than or equal to 0")

			case startPort > endPort:
				return errors.New("start port must be less than or equal to end port")

			case endPort-startPort > 100:
				return errors.New("port range must be less than or equal to 100")

			case startPort < 1024:
				return errors.New("start port must be greater than or equal to 1024")

			case endPort > 65535:
				return errors.New("end port must be less than or equal to 65535")

			}
			localIP, err := DetermineLocalIPAddress()
			if startPort == endPort {
				count := 5
				interval := 100 * time.Millisecond
				timeout := 500 * time.Millisecond

				if err != nil {
					return fmt.Errorf("failed to determine local IP address: %w", err)
				}

				// If a single port is specified, do not scan
				rtts, err := BroadcasterRTTcheck(localIP, remoteIP, startPort, count, interval, timeout)
				if err != nil {
					return fmt.Errorf("failed to healthcheck game server: %w", err)
				}
				var sum time.Duration
				// Craft a message that contains the comma-delimited list of the rtts. Use a * for any failed pings (rtt == -1)
				rttStrings := make([]string, len(rtts))
				for i, rtt := range rtts {
					if rtt != -1 {
						sum += rtt
						count++
					}
					if rtt == -1 {
						rttStrings[i] = "*"
					} else {
						rttStrings[i] = fmt.Sprintf("%.0fms", rtt.Seconds()*1000)
					}
				}
				rttMessage := strings.Join(rttStrings, ", ")

				// Calculate the average rtt
				avgrtt := sum / time.Duration(count)

				if avgrtt > 0 {
					return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
						Type: discordgo.InteractionResponseChannelMessageWithSource,

						Data: &discordgo.InteractionResponseData{
							Flags:   discordgo.MessageFlagsEphemeral,
							Content: fmt.Sprintf("game server %s:%d RTTs (AVG: %.0f): %s", remoteIP, startPort, avgrtt.Seconds()*1000, rttMessage),
						},
					})
				} else {
					return errors.New("no response from game server")

				}
			} else {

				// Scan the address for responding game servers and then return the results as a newline-delimited list of ip:port
				responses, _ := BroadcasterPortScan(localIP, remoteIP, 6792, 6820, 500*time.Millisecond)
				if len(responses) == 0 {
					return errors.New("no game servers are responding")
				}

				// Craft a message that contains the newline-delimited list of the responding game servers
				var b strings.Builder
				for port, r := range responses {
					b.WriteString(fmt.Sprintf("%s:%-5d %3.0fms\n", remoteIP, port, r.Seconds()*1000))
				}

				return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Flags:   discordgo.MessageFlagsEphemeral,
						Content: fmt.Sprintf("```%s```", b.String()),
					},
				})

			}
		},
		"reset-password": func(logger runtime.Logger, s *discordgo.Session, i *discordgo.InteractionCreate, user *discordgo.User, member *discordgo.Member, userID string, groupID string) error {

			if user == nil {
				return nil
			}

			if err := func() error {
				// Get the account
				account, err := nk.AccountGetId(ctx, userID)
				if err != nil {
					return err
				}
				// Clear the password
				return nk.UnlinkEmail(ctx, userID, account.GetEmail())

			}(); err != nil {
				return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Flags:   discordgo.MessageFlagsEphemeral,
						Content: err.Error(),
					},
				})
			} else {
				// Send the response
				return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Flags:   discordgo.MessageFlagsEphemeral,
						Content: "Your password has been cleared.",
					},
				})
			}
		},

		"jersey-number": func(logger runtime.Logger, s *discordgo.Session, i *discordgo.InteractionCreate, user *discordgo.User, member *discordgo.Member, userID string, groupID string) error {
			options := i.ApplicationCommandData().Options
			if len(options) == 0 {
				return errors.New("no options provided")
			}
			number := int(options[0].IntValue())
			if number < 0 || number > 99 {
				return errors.New("invalid number. Must be between 0 and 99")
			}
			if userID == "" {
				return errors.New("no user ID")
			}
			// Get the user's profile
			uid := uuid.FromStringOrNil(userID)
			profile, err := d.profileRegistry.Load(ctx, uid)
			if err != nil {
				return fmt.Errorf("failed to load profile: %w", err)
			}

			// Update the jersey number
			profile.SetJerseyNumber(number)

			// Save the profile
			if err := d.profileRegistry.SaveAndCache(ctx, uid, profile); err != nil {
				return fmt.Errorf("failed to save profile: %w", err)
			}

			// Send the response
			return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Flags:   discordgo.MessageFlagsEphemeral,
					Content: fmt.Sprintf("Your jersey number has been set to %d", number),
				},
			})
		},

		"badges": func(logger runtime.Logger, s *discordgo.Session, i *discordgo.InteractionCreate, user *discordgo.User, member *discordgo.Member, userID string, groupID string) error {
			options := i.ApplicationCommandData().Options
			var err error

			if user == nil {
				return nil
			}

			switch options[0].Name {
			case "assign":
				options = options[0].Options
				// Check that the user is a developer

				var isMember bool
				isMember, err = CheckSystemGroupMembership(ctx, db, userID, GroupGlobalBadgeAdmins)
				if err != nil {
					return status.Error(codes.Internal, "failed to check group membership")
				}
				if !isMember {
					return status.Error(codes.PermissionDenied, "you do not have permission to use this command")
				}
				if len(options) < 2 {
					return status.Error(codes.InvalidArgument, "you must specify a user and a badge")
				}
				// Get the target user's discord ID
				target := options[0].UserValue(s)
				if target == nil {
					return status.Error(codes.InvalidArgument, "you must specify a user")
				}
				targetUserID := d.cache.DiscordIDToUserID(target.ID)
				if targetUserID == "" {
					return status.Error(codes.NotFound, "target user not found")
				}

				// Get the badge name
				badgeCodestr := options[1].StringValue()
				badgeCodes := strings.Split(strings.ToLower(badgeCodestr), ",")

				changeset := make(map[string]int64, len(badgeCodes))

				for _, c := range badgeCodes {

					c := strings.TrimSpace(c)
					if c == "" {
						continue
					}
					groupName, ok := vrmlMap[c]
					if !ok {
						return status.Errorf(codes.InvalidArgument, "badge `%s` not found", c)
					}

					changeset[groupName] = 1
				}

				assignerID := d.cache.DiscordIDToUserID(user.ID)
				metadata := map[string]interface{}{
					"assigner_id": assignerID,
					"discord_id":  target.ID,
				}

				nk.WalletUpdate(ctx, targetUserID, changeset, metadata, true)

				// Log the action
				logger.WithFields(map[string]interface{}{
					"badges":     badgeCodestr,
					"user":       target.Username,
					"discord_id": target.ID,
					"assigner":   user.ID,
				}).Debug("assign badges")

				// Send a message to the channel

				channel := "1232462244797874247"
				_, err = s.ChannelMessageSend(channel, fmt.Sprintf("%s assigned VRML cosmetics `%s` to user `%s`", user.Mention(), badgeCodestr, target.Username))
				if err != nil {
					logger.WithFields(map[string]interface{}{
						"error": err,
					}).Error("Failed to send badge channel update message")
				}
				simpleInteractionResponse(s, i, fmt.Sprintf("Assigned VRML cosmetics `%s` to user `%s`", badgeCodestr, target.Username))

			case "set-vrml-username":
				options = options[0].Options
				// Get the user's discord ID
				user := getScopedUser(i)
				if user == nil {
					return nil
				}
				vrmlUsername := options[0].StringValue()

				// Check the vlaue against vrmlIDPattern
				if !vrmlIDPattern.MatchString(vrmlUsername) {
					return fmt.Errorf("invalid VRML username: `%s`", vrmlUsername)
				}

				// Access the VRML HTTP API
				url := fmt.Sprintf("https://api.vrmasterleague.com/EchoArena/Players/Search?name=%s", vrmlUsername)
				var req *http.Request
				req, err = http.NewRequest("GET", url, nil)
				if err != nil {
					return status.Error(codes.Internal, "failed to create request")
				}

				req.Header.Set("User-Agent", "EchoVRCE Discord Bot (contact: @sprockee)")

				// Make the request
				var resp *http.Response
				resp, err = http.DefaultClient.Do(req)
				if err != nil {
					return status.Error(codes.Internal, "failed to make request")
				}

				// Parse the response as JSON...
				// [{"id":"4rPCIjBhKhGpG4uDnfHlfg2","name":"sprockee","image":"/images/logos/users/25d45af7-f6a8-40ef-a035-879a61869c8f.png"}]
				var players []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				}

				if err = json.NewDecoder(resp.Body).Decode(&players); err != nil {
					return status.Error(codes.Internal, "failed to decode response: "+err.Error())
				}

				// Check if the player was found
				if len(players) == 0 {
					return status.Error(codes.NotFound, "player not found")
				}

				// Ensure that only one was returned
				if len(players) > 1 {
					return status.Error(codes.Internal, "multiple players found")
				}

				// Get the player's ID
				playerID := players[0].ID

				type VRMLUser struct {
					UserID        string      `json:"userID"`
					UserName      string      `json:"userName"`
					UserLogo      string      `json:"userLogo"`
					Country       string      `json:"country"`
					Nationality   string      `json:"nationality"`
					DateJoinedUTC string      `json:"dateJoinedUTC"`
					StreamURL     interface{} `json:"streamUrl"`
					DiscordID     float64     `json:"discordID"`
					DiscordTag    string      `json:"discordTag"`
					SteamID       interface{} `json:"steamID"`
					IsTerminated  bool        `json:"isTerminated"`
				}
				type Game struct {
					GameID         string `json:"gameID"`
					GameName       string `json:"gameName"`
					TeamMode       string `json:"teamMode"`
					MatchMode      string `json:"matchMode"`
					URL            string `json:"url"`
					URLShort       string `json:"urlShort"`
					URLComplete    string `json:"urlComplete"`
					HasSubstitutes bool   `json:"hasSubstitutes"`
					HasTies        bool   `json:"hasTies"`
					HasCasters     bool   `json:"hasCasters"`
					HasCameraman   bool   `json:"hasCameraman"`
				}

				type ThisGame struct {
					PlayerID   string `json:"playerID"`
					PlayerName string `json:"playerName"`
					UserLogo   string `json:"userLogo"`
					Game       Game   `json:"game"`
				}

				type playerDetailed struct {
					User     VRMLUser `json:"user"`
					ThisGame ThisGame `json:"thisGame"`
				}
				jsonData, err := json.Marshal(players[0])
				if err != nil {
					return status.Error(codes.Internal, "failed to marshal player data: "+err.Error())
				}

				// Set the VRML ID for the user in their profile as a storage object
				_, err = nk.StorageWrite(ctx, []*runtime.StorageWrite{
					{
						Collection: VRMLStorageCollection,
						Key:        playerID,
						UserID:     user.ID,
						Value:      string(jsonData),
						Version:    "*",
					},
				})
				if err != nil {
					return status.Error(codes.Internal, "failed to set VRML ID: "+err.Error())
				}

				logger.Info("set vrml id", zap.String("discord_id", user.ID), zap.String("discord_username", user.Username), zap.String("vrml_id", playerID))

				return simpleInteractionResponse(s, i, fmt.Sprintf("set VRML username `%s` for user `%s`", vrmlUsername, user.Username))
			}
			return nil
		},
		"whoami": func(logger runtime.Logger, s *discordgo.Session, i *discordgo.InteractionCreate, user *discordgo.User, member *discordgo.Member, userID string, groupID string) error {

			if user == nil {
				return nil
			}
			if member == nil {
				return errors.New("this command must be used from a guild")
			}
			// check for the with-detail boolean option
			d.cache.Purge(user.ID)
			d.cache.QueueSyncMember(i.GuildID, user.ID)

			err := d.handleProfileRequest(ctx, logger, nk, s, i, user.ID, user.Username, true, true)
			logger.Debug("whoami", zap.String("discord_id", user.ID), zap.String("discord_username", user.Username), zap.Error(err))
			return err
		},

		"set-lobby": func(logger runtime.Logger, s *discordgo.Session, i *discordgo.InteractionCreate, user *discordgo.User, member *discordgo.Member, userIDStr string, groupID string) error {
			if member == nil {
				return fmt.Errorf("this command must be used from a guild")
			}

			d.cache.QueueSyncMember(i.GuildID, member.User.ID)

			// Try to find it by searching
			memberships, err := GetGuildGroupMemberships(ctx, d.nk, userIDStr)
			if err != nil {
				return err
			}

			if len(memberships) == 0 {

				return errors.New("guild data stale, please try again in a few seconds")

			}
			_, ok := memberships[groupID]
			if !ok {
				return errors.New("no membership found")
			}

			// Set the metadata
			md, err := GetAccountMetadata(ctx, nk, userIDStr)
			if err != nil {
				return err
			}
			md.SetActiveGroupID(uuid.FromStringOrNil(groupID))

			if err = nk.AccountUpdateId(ctx, userIDStr, "", md.MarshalMap(), "", "", "", "", ""); err != nil {
				return err
			}

			userID := uuid.FromStringOrNil(userIDStr)

			profile, err := d.profileRegistry.Load(ctx, userID)
			if err != nil {
				return err
			}

			profile.SetChannel(evr.GUID(uuid.FromStringOrNil(groupID)))

			if err = d.profileRegistry.SaveAndCache(ctx, userID, profile); err != nil {
				return err
			}

			guild, err := s.Guild(i.GuildID)
			if err != nil {
				logger.Error("Failed to get guild", zap.Error(err))
				return err
			}

			d.profileRegistry.SaveAndCache(ctx, userID, profile)
			return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Flags: discordgo.MessageFlagsEphemeral,
					Content: strings.Join([]string{
						fmt.Sprintf("EchoVR lobby changed to **%s**.", guild.Name),
						"- Matchmaking will prioritize members",
						"- Social lobbies will contain only members",
						"- Private matches that you create will prioritize guild's game servers.",
					}, "\n"),
				},
			})
		},
		"lookup": func(logger runtime.Logger, s *discordgo.Session, i *discordgo.InteractionCreate, user *discordgo.User, member *discordgo.Member, userIDStr string, groupID string) error {

			if user == nil {
				return nil
			}

			options := i.ApplicationCommandData().Options

			if len(options) == 0 {
				return errors.New("no options provided")
			}

			caller := user
			target := options[0].UserValue(s)

			// Clear the cache of the caller and target
			d.cache.Purge(caller.ID)
			d.cache.Purge(target.ID)

			callerUserID := d.cache.DiscordIDToUserID(caller.ID)
			if callerUserID == "" {
				return errors.New("caller not found")
			}

			targetUserID := d.cache.DiscordIDToUserID(target.ID)
			if targetUserID == "" {
				return errors.New("player not found")
			}

			// Get the caller's nakama user ID
			callerGuildGroups, err := UserGuildGroupsList(ctx, d.nk, callerUserID)
			if err != nil {
				return fmt.Errorf("failed to get guild groups: %w", err)
			}

			isGuildModerator := false

			for _, g := range callerGuildGroups {
				if g.GuildID == i.GuildID {
					isGuildModerator = g.PermissionsUser(callerUserID).IsModerator
				}
			}

			// Get the caller's nakama user ID

			isGlobalModerator, err := CheckSystemGroupMembership(ctx, db, userIDStr, GroupGlobalModerators)
			if err != nil {
				return errors.New("error checking global moderator status")
			}

			d.cache.Purge(target.ID)

			return d.handleProfileRequest(ctx, logger, nk, s, i, target.ID, target.Username, isGuildModerator, isGlobalModerator)
		},
		"search": func(logger runtime.Logger, s *discordgo.Session, i *discordgo.InteractionCreate, user *discordgo.User, member *discordgo.Member, userIDStr string, groupID string) error {

			if user == nil {
				return nil
			}

			options := i.ApplicationCommandData().Options

			if len(options) == 0 {
				return errors.New("no options provided")
			}

			partial := options[0].StringValue()

			partial = sanitizeDisplayName(strings.ToLower(partial))

			pattern := fmt.Sprintf(".*%s.*", partial)

			if len(partial) <= 3 {
				// exact match only
				pattern = partial
			}

			histories, err := DisplayNameCacheRegexSearch(ctx, nk, pattern, 10)
			if err != nil {
				logger.Error("Failed to search display name history", zap.Error(err))
				return fmt.Errorf("failed to search display name history: %w", err)
			}

			if len(histories) == 0 {
				return simpleInteractionResponse(s, i, "No results found")
			}

			exactOnly := false
			/*
				if len(histories) > 10 {

					// limit to exact hits
					exactOnly = true
				}
			*/

			resultsByUserID := make(map[string][]DisplayNameHistoryEntry, len(histories))

			for userID, journal := range histories {

				// Only search this guild
				history, ok := journal.Histories[groupID]
				if !ok {
					continue
				}

				for _, e := range history {

					if exactOnly && strings.ToLower(e.DisplayName) != partial {
						continue
					}

					if !strings.Contains(strings.ToLower(e.DisplayName), partial) {
						continue
					}

					resultsByUserID[userID] = append(resultsByUserID[userID], e)
				}
			}

			if len(resultsByUserID) == 0 {
				return simpleInteractionResponse(s, i, "No results found")
			}

			// Sort each players results by update time
			for userID, r := range resultsByUserID {
				sort.Slice(r, func(i, j int) bool {
					return r[i].UpdateTime.After(r[j].UpdateTime)
				})

				// Remove older duplicates
				seen := make(map[string]struct{})
				unique := make([]DisplayNameHistoryEntry, 0, len(r))
				for _, e := range r {
					if _, ok := seen[e.DisplayName]; ok {
						continue
					}
					seen[e.DisplayName] = struct{}{}
					unique = append(unique, e)
				}
				resultsByUserID[userID] = unique

				// Limit the results to the most recent 10
			}

			// Create the embed
			embed := &discordgo.MessageEmbed{
				Title:  "Search Results for `" + partial + "`",
				Color:  0x9656ce,
				Fields: make([]*discordgo.MessageEmbedField, 0, len(resultsByUserID)),
			}

			for userID, results := range resultsByUserID {

				// Get the discord ID
				discordID := d.cache.UserIDToDiscordID(userID)
				if discordID == "" {
					logger.Warn("Failed to get discord ID", zap.Error(err))
					continue
				}

				embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
					Name:   "Player",
					Value:  fmt.Sprintf("<@%s>", discordID),
					Inline: true,
				})

				displayNames := make([]string, 0, len(results))
				for _, r := range results {
					displayNames = append(displayNames, fmt.Sprintf("%s <t:%d:R>", EscapeDiscordMarkdown(r.DisplayName), r.UpdateTime.UTC().Unix()))
				}

				embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
					Name:   "In-Game Names",
					Value:  strings.Join(displayNames, "\n"),
					Inline: false,
				})
			}

			// Send the response
			return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Flags:  discordgo.MessageFlagsEphemeral,
					Embeds: []*discordgo.MessageEmbed{embed},
				},
			})
		},
		"create": func(logger runtime.Logger, s *discordgo.Session, i *discordgo.InteractionCreate, user *discordgo.User, member *discordgo.Member, userID string, groupID string) error {
			options := i.ApplicationCommandData().Options

			if member == nil {
				return simpleInteractionResponse(s, i, "this command must be used from a guild")

			}

			if len(options) == 0 {
				return simpleInteractionResponse(s, i, "no options provided")
			}

			mode := evr.ModeArenaPrivate
			region := evr.DefaultRegion
			level := evr.LevelUnspecified
			for _, o := range options {
				switch o.Name {
				case "region":
					region = evr.ToSymbol(o.StringValue())
				case "mode":
					mode = evr.ToSymbol(o.StringValue())
				case "level":
					level = evr.ToSymbol(o.StringValue())
				}
			}

			if levels, ok := evr.LevelsByMode[mode]; !ok {
				return fmt.Errorf("invalid mode `%s`", mode)
			} else if level != evr.LevelUnspecified && !slices.Contains(levels, level) {
				return fmt.Errorf("invalid level `%s`", level)
			}

			startTime := time.Now().Add(90 * time.Second)

			logger = logger.WithFields(map[string]interface{}{
				"userID":    userID,
				"guildID":   i.GuildID,
				"region":    region.String(),
				"mode":      mode.String(),
				"level":     level.String(),
				"startTime": startTime,
			})

			label, rttMs, err := d.handleCreateMatch(ctx, logger, userID, i.GuildID, region, mode, level, startTime)
			if err != nil {
				return err
			}

			// set the player's next match
			if err := SetNextMatchID(ctx, nk, userID, label.ID, AnyTeam, ""); err != nil {
				logger.Error("Failed to set next match ID", zap.Error(err))
				return fmt.Errorf("failed to set next match ID: %w", err)
			}

			logger.WithFields(map[string]any{
				"match_id": label.ID.String(),
				"rtt_ms":   rttMs,
			}).Info("Match created.")

			content := fmt.Sprintf("Reservation will timeout <t:%d:R>. \n\nClick play or start matchmaking to automatically join your match.", startTime.Unix())

			niceNameMap := map[evr.Symbol]string{
				evr.ModeArenaPrivate:  "Private Arena Match",
				evr.ModeArenaPublic:   "Public Arena Match",
				evr.ModeCombatPrivate: "Private Combat Match",
				evr.ModeCombatPublic:  "Public Combat Match",
				evr.ModeSocialPrivate: "Private Social Lobby",
			}
			prettyName, ok := niceNameMap[mode]
			if !ok {
				prettyName = mode.String()
			}

			serverLocation := "Unknown"

			serverExtIP := label.Broadcaster.Endpoint.ExternalIP.String()
			if ipqs, found := ipqsCache.Load(serverExtIP); found {
				serverLocation = ipqs.Region
			}

			// local the guild name
			guild, err := s.Guild(i.GuildID)
			if err != nil {
				logger.Error("Failed to get guild", zap.Error(err))
			}
			responseContent := &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Flags: discordgo.MessageFlagsEphemeral,
					Embeds: []*discordgo.MessageEmbed{{
						Title:       "Match Created",
						Description: content,
						Color:       0x9656ce,
						Fields: []*discordgo.MessageEmbedField{
							{
								Name:   "Guild",
								Value:  guild.Name,
								Inline: false,
							},
							{
								Name:   "Mode",
								Value:  prettyName,
								Inline: true,
							},
							{
								Name:   "Region Code",
								Value:  region.String(),
								Inline: false,
							},
							{
								Name:  "Server Location",
								Value: serverLocation,
							},
							{
								Name:   "Ping Latency",
								Value:  fmt.Sprintf("%dms", rttMs),
								Inline: false,
							},
							{
								Name:   "Spark Link",
								Value:  fmt.Sprintf("[Spark Link](https://echo.taxi/spark://c/%s)", strings.ToUpper(label.ID.UUID.String())),
								Inline: false,
							},
							{
								Name:  "Participants",
								Value: "No participants yet",
							},
						},
					}},
				},
			}

			go func() {

				// Monitor the match and update the interaction
				for {
					startCheckTimer := time.NewTicker(15 * time.Second)
					updateTicker := time.NewTicker(30 * time.Second)
					select {
					case <-startCheckTimer.C:
					case <-updateTicker.C:
					}

					presences, err := d.nk.StreamUserList(StreamModeMatchAuthoritative, label.ID.UUID.String(), "", label.ID.Node, false, true)
					if err != nil {
						logger.Error("Failed to get user list", zap.Error(err))
					}
					if len(presences) == 0 {
						// Match is gone. update the response.
						responseContent.Data.Embeds[0].Title = "Match Over"
						responseContent.Data.Embeds[0].Description = "The match expired/ended."

						if _, err = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
							Embeds: &responseContent.Data.Embeds,
						}); err != nil {
							logger.Error("Failed to update interaction", zap.Error(err))
							return
						}
					}

					// Update the list of players in the interaction response
					var players strings.Builder
					for _, p := range presences {
						if p.GetSessionId() == label.Broadcaster.SessionID {
							continue
						}
						players.WriteString(fmt.Sprintf("<@%s>\n", d.cache.UserIDToDiscordID(p.GetUserId())))
					}
					responseContent.Data.Embeds[0].Fields[4].Value = players.String()

					if _, err = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
						Embeds: &responseContent.Data.Embeds,
					}); err != nil {
						logger.Error("Failed to update interaction", zap.Error(err))
						return
					}

				}
			}()

			return s.InteractionRespond(i.Interaction, responseContent)
		},
		"allocate": func(logger runtime.Logger, s *discordgo.Session, i *discordgo.InteractionCreate, user *discordgo.User, member *discordgo.Member, userID string, groupID string) error {
			options := i.ApplicationCommandData().Options

			if member == nil {
				return simpleInteractionResponse(s, i, "this command must be used from a guild")

			}
			mode := evr.ModeArenaPrivate
			region := evr.DefaultRegion
			level := evr.LevelUnspecified
			for _, o := range options {
				switch o.Name {
				case "region":
					region = evr.ToSymbol(o.StringValue())
				case "mode":
					mode = evr.ToSymbol(o.StringValue())
				case "level":
					level = evr.ToSymbol(o.StringValue())
				}
			}

			if levels, ok := evr.LevelsByMode[mode]; !ok {
				return fmt.Errorf("invalid mode `%s`", mode)
			} else if level != evr.LevelUnspecified && !slices.Contains(levels, level) {
				return fmt.Errorf("invalid level `%s`", level)
			}

			startTime := time.Now()

			logger = logger.WithFields(map[string]interface{}{
				"userID":    userID,
				"guildID":   i.GuildID,
				"region":    region.String(),
				"mode":      mode.String(),
				"level":     level.String(),
				"startTime": startTime,
			})

			label, _, err := d.handleAllocateMatch(ctx, logger, userID, i.GuildID, region, mode, level, startTime)
			if err != nil {
				return err
			}

			logger.WithField("label", label).Info("Match prepared")
			return simpleInteractionResponse(s, i, fmt.Sprintf("Match prepared with label ```json\n%s\n```\nhttps://echo.taxi/spark://c/%s", label.GetLabelIndented(), strings.ToUpper(label.ID.UUID.String())))
		},
		"trigger-cv": func(logger runtime.Logger, s *discordgo.Session, i *discordgo.InteractionCreate, user *discordgo.User, member *discordgo.Member, userID string, groupID string) error {

			options := i.ApplicationCommandData().Options

			if len(options) == 0 {
				return errors.New("no options provided")
			}

			if user == nil {
				return nil
			}

			target := options[0].UserValue(s)

			targetUserID := d.cache.DiscordIDToUserID(target.ID)
			if targetUserID == "" {
				return errors.New("failed to get target user ID")
			}

			metadata, err := GetGuildGroupMetadata(ctx, d.db, groupID)
			if err != nil {
				return errors.New("failed to get guild group metadata")
			}
			metadata.CommunityValuesUserIDsAdd(targetUserID)

			data, err := metadata.MarshalToMap()
			if err != nil {
				return fmt.Errorf("error marshalling group data: %w", err)
			}

			if err := nk.GroupUpdate(ctx, groupID, SystemUserID, "", "", "", "", "", false, data, 1000000); err != nil {
				return fmt.Errorf("error updating group: %w", err)
			}

			presences, err := d.nk.StreamUserList(StreamModeService, targetUserID, "", StreamLabelMatchService, false, true)
			if err != nil {
				return err
			}

			cnt := 0
			for _, p := range presences {
				disconnectDelay := 0
				if p.GetUserId() == targetUserID {
					cnt += 1
					if label, _ := MatchLabelByID(ctx, d.nk, MatchIDFromStringOrNil(p.GetStatus())); label != nil {

						if label.Broadcaster.SessionID == p.GetSessionId() {
							continue
						}

						if label.GetGroupID().String() != groupID {
							return errors.New("user's match is not from this guild")
						}

						// Kick the player from the match
						if err := KickPlayerFromMatch(ctx, d.nk, label.ID, targetUserID); err != nil {
							return err
						}
						_, _ = d.LogAuditMessage(ctx, groupID, fmt.Sprintf("%s kicked player %s from [%s](https://echo.taxi/spark://c/%s) match.", user.Mention(), target.Mention(), label.Mode.String(), strings.ToUpper(label.ID.UUID.String())), false)
						disconnectDelay = 15
					}

					go func() {
						<-time.After(time.Second * time.Duration(disconnectDelay))
						// Just disconnect the user, wholesale
						if _, err := DisconnectUserID(ctx, d.nk, targetUserID); err != nil {
							logger.Warn("Failed to disconnect user", zap.Error(err))
						} else {
							_, _ = d.LogAuditMessage(ctx, groupID, fmt.Sprintf("%s disconnected player %s from match service.", user.Mention(), target.Mention()), false)
						}
					}()

					cnt++
				}
			}

			return simpleInteractionResponse(s, i, fmt.Sprintf("%s is required to complete *Community Values* when entering the next social lobby. (Disconnected %d sessions)", target.Mention(), cnt))
		},
		"kick-player": func(logger runtime.Logger, s *discordgo.Session, i *discordgo.InteractionCreate, user *discordgo.User, member *discordgo.Member, userID string, groupID string) error {

			if user == nil {
				return nil
			}

			options := i.ApplicationCommandData().Options
			if len(options) == 0 {
				return errors.New("no options provided")
			}

			target := options[0].UserValue(s)
			targetUserID := d.cache.DiscordIDToUserID(target.ID)
			if targetUserID == "" {
				return errors.New("failed to get target user ID")
			}

			presences, err := d.nk.StreamUserList(StreamModeService, targetUserID, "", StreamLabelMatchService, false, true)
			if err != nil {
				return err
			}

			cnt := 0
			for _, p := range presences {
				disconnectDelay := 0
				if p.GetUserId() == targetUserID {
					cnt += 1
					if label, _ := MatchLabelByID(ctx, d.nk, MatchIDFromStringOrNil(p.GetStatus())); label != nil {

						if label.Broadcaster.SessionID == p.GetSessionId() {
							continue
						}

						if label.GetGroupID().String() != groupID {
							return errors.New("user's match is not from this guild")
						}

						// Kick the player from the match
						if err := KickPlayerFromMatch(ctx, d.nk, label.ID, targetUserID); err != nil {
							return err
						}
						_, _ = d.LogAuditMessage(ctx, groupID, fmt.Sprintf("<@%s> kicked player <@%s> from [%s](https://echo.taxi/spark://c/%s) match.", user.ID, target.ID, label.Mode.String(), strings.ToUpper(label.ID.UUID.String())), false)
						disconnectDelay = 15
					}

					go func() {
						<-time.After(time.Second * time.Duration(disconnectDelay))
						// Just disconnect the user, wholesale
						if _, err := DisconnectUserID(ctx, d.nk, targetUserID); err != nil {
							logger.Warn("Failed to disconnect user", zap.Error(err))
						} else {
							_, _ = d.LogAuditMessage(ctx, groupID, fmt.Sprintf("<@%s> disconnected player <@%s> from match service.", user.ID, target.ID), false)
						}
					}()

					cnt++
				}
			}
			return simpleInteractionResponse(s, i, fmt.Sprintf("Disconnected %d sessions.", cnt))
		},
		"join-player": func(logger runtime.Logger, s *discordgo.Session, i *discordgo.InteractionCreate, user *discordgo.User, member *discordgo.Member, userID string, groupID string) error {

			if user == nil {
				return nil
			}

			options := i.ApplicationCommandData().Options
			if len(options) == 0 {
				return errors.New("no options provided")
			}

			target := options[0].UserValue(s)
			targetUserID := d.cache.DiscordIDToUserID(target.ID)
			if targetUserID == "" {
				return errors.New("failed to get target user ID")
			}

			presences, err := d.nk.StreamUserList(StreamModeService, targetUserID, "", StreamLabelMatchService, false, true)
			if err != nil {
				return err
			}

			if len(presences) == 0 {
				return simpleInteractionResponse(s, i, "No sessions found.")
			}
			presence := presences[0]
			if label, _ := MatchLabelByID(ctx, d.nk, MatchIDFromStringOrNil(presence.GetStatus())); label != nil {

				if label.GetGroupID().String() != groupID {
					return errors.New("user's match is not from this guild")
				}

				if err := SetNextMatchID(ctx, d.nk, userID, label.ID, Moderator, ""); err != nil {
					return fmt.Errorf("failed to set next match ID: %w", err)
				}

				_, _ = d.LogAuditMessage(ctx, groupID, fmt.Sprintf("<@%s> join player <@%s> at [%s](https://echo.taxi/spark://c/%s) match.", user.ID, target.ID, label.Mode.String(), strings.ToUpper(label.ID.UUID.String())), false)
				content := fmt.Sprintf("Joining %s [%s](https://echo.taxi/spark://c/%s) match next.", target.Mention(), label.Mode.String(), strings.ToUpper(label.ID.UUID.String()))
				return simpleInteractionResponse(s, i, content)
			}
			return simpleInteractionResponse(s, i, "No match found.")
		},
		"set-roles": func(logger runtime.Logger, s *discordgo.Session, i *discordgo.InteractionCreate, user *discordgo.User, member *discordgo.Member, userID string, groupID string) error {
			options := i.ApplicationCommandData().Options

			// Ensure the user is the owner of the guild
			if user == nil || i.Member == nil || i.Member.User.ID == "" || i.GuildID == "" {
				return nil
			}

			guild, err := s.Guild(i.GuildID)
			if err != nil || guild == nil {
				return errors.New("failed to get guild")
			}

			if guild.OwnerID != user.ID {
				// Check if the user is a global developer
				if ok, err := CheckSystemGroupMembership(ctx, db, userID, GroupGlobalDevelopers); err != nil {
					return errors.New("failed to check group membership")
				} else if !ok {
					return errors.New("you do not have permission to use this command")
				}
			}

			// Get the metadata
			metadata, err := GetGuildGroupMetadata(ctx, d.db, groupID)
			if err != nil {
				return errors.New("failed to get guild group metadata")

			}
			roles := metadata.Roles
			for _, o := range options {
				roleID := o.RoleValue(s, guild.ID).ID
				switch o.Name {
				case "moderator":
					roles.Moderator = roleID
				case "serverhost":
					roles.ServerHost = roleID
				case "suspension":
					roles.Suspended = roleID
				case "member":
					roles.Member = roleID
				case "allocator":
					roles.Allocator = roleID
				case "is_linked":
					roles.AccountLinked = roleID
				}
			}

			data, err := metadata.MarshalToMap()
			if err != nil {
				return fmt.Errorf("error marshalling group data: %w", err)
			}

			if err := nk.GroupUpdate(ctx, groupID, SystemUserID, "", "", "", "", "", false, data, 1000000); err != nil {
				return fmt.Errorf("error updating group: %w", err)
			}

			return simpleInteractionResponse(s, i, "roles set!")
		},

		"region-status": func(logger runtime.Logger, s *discordgo.Session, i *discordgo.InteractionCreate, user *discordgo.User, member *discordgo.Member, userID string, groupID string) error {
			options := i.ApplicationCommandData().Options

			if user == nil {
				return nil
			}

			regionStr := options[0].StringValue()
			if regionStr == "" {
				return errors.New("no region provided")
			}

			return d.createRegionStatusEmbed(ctx, logger, regionStr, i.Interaction.ChannelID, nil)
		},
		"stream-list": func(logger runtime.Logger, s *discordgo.Session, i *discordgo.InteractionCreate, user *discordgo.User, member *discordgo.Member, userID string, groupID string) error {
			options := i.ApplicationCommandData().Options

			if user == nil {
				return nil
			}

			// Limit access to global developers
			if ok, err := CheckSystemGroupMembership(ctx, d.db, userID, GroupGlobalDevelopers); err != nil {
				return errors.New("failed to check group membership")
			} else if !ok {
				return errors.New("you do not have permission to use this command")
			}

			var subject, subcontext, label string
			var mode, limit int64
			for _, o := range options {
				switch o.Name {
				case "mode":
					mode = o.IntValue()
				case "subject":
					subject = o.StringValue()
				case "subcontext":
					subcontext = o.StringValue()
				case "label":
					label = o.StringValue()
				case "limit":
					limit = o.IntValue()
				}
			}

			includeHidden := true
			includeOffline := true

			presences, err := nk.StreamUserList(uint8(mode), subject, subcontext, label, includeHidden, includeOffline)
			if err != nil {
				return errors.New("failed to list stream users")

			}
			if len(presences) == 0 {
				return errors.New("no stream users found")
			}
			channel, err := s.UserChannelCreate(user.ID)
			if err != nil {
				return errors.New("failed to create user channel")
			}
			if err := simpleInteractionResponse(s, i, "Sending stream list to your DMs"); err != nil {
				return errors.New("failed to send interaction response")
			}
			if limit == 0 {
				limit = 10
			}

			var b strings.Builder
			if len(presences) > int(limit) {
				presences = presences[:limit]
			}

			type presenceMessage struct {
				UserID    string
				Username  string
				SessionID string
				Status    any
			}

			messages := make([]string, 0)
			for _, presence := range presences {
				m := presenceMessage{
					UserID:    presence.GetUserId(),
					Username:  presence.GetUsername(),
					SessionID: presence.GetSessionId(),
				}
				// Try to unmarshal the status
				status := make(map[string]any, 0)
				if err := json.Unmarshal([]byte(presence.GetStatus()), &status); err != nil {
					m.Status = presence.GetStatus()
				}
				m.Status = status

				data, err := json.MarshalIndent(m, "", "  ")
				if err != nil {
					return errors.New("failed to marshal presence data")
				}
				if b.Len()+len(data) > 1900 {
					messages = append(messages, b.String())
					b.Reset()
				}
				_, err = b.WriteString(fmt.Sprintf("```json\n%s\n```\n", data))
				if err != nil {
					return errors.New("failed to write presence data")
				}
			}
			messages = append(messages, b.String())

			go func() {
				for _, m := range messages {
					if _, err := s.ChannelMessageSend(channel.ID, m); err != nil {
						logger.Warn("Failed to send message", zap.Error(err))
						return
					}
					// Ensure it's stays below 25 messages per second
					time.Sleep(time.Millisecond * 50)
				}
			}()
			return nil
		},

		"party": func(logger runtime.Logger, s *discordgo.Session, i *discordgo.InteractionCreate, user *discordgo.User, member *discordgo.Member, userID string, groupID string) error {

			if user == nil {
				return nil
			}

			options := i.ApplicationCommandData().Options
			switch options[0].Name {
			case "invite":
				options := options[0].Options
				inviter := i.User
				invitee := options[0].UserValue(s)

				if err := d.sendPartyInvite(ctx, s, i, inviter, invitee); err != nil {
					return err
				}
			case "members":

				// List the other players in this party group
				objs, err := nk.StorageRead(ctx, []*runtime.StorageRead{
					{
						Collection: MatchmakerStorageCollection,
						Key:        MatchmakingConfigStorageKey,
						UserID:     userID,
					},
				})
				if err != nil {
					logger.Error("Failed to read matchmaking config", zap.Error(err))
				}
				matchmakingConfig := &MatchmakingSettings{}
				if len(objs) != 0 {
					if err := json.Unmarshal([]byte(objs[0].Value), matchmakingConfig); err != nil {
						return fmt.Errorf("failed to unmarshal matchmaking config: %w", err)
					}
				}
				if matchmakingConfig.LobbyGroupName == "" {
					return errors.New("set a group ID first with `/party group`")
				}

				//logger = logger.WithField("group_id", matchmakingConfig.GroupID)
				groupName, partyUUID, err := GetLobbyGroupID(ctx, d.db, userID)
				if err != nil {
					return fmt.Errorf("failed to get party group ID: %w", err)
				}
				// Look for presences

				partyMembers, err := nk.StreamUserList(StreamModeParty, partyUUID.String(), "", d.pipeline.node, false, true)
				if err != nil {
					return fmt.Errorf("failed to list stream users: %w", err)
				}

				activeIDs := make([]string, 0, len(partyMembers))
				for _, partyMember := range partyMembers {
					activeIDs = append(activeIDs, d.cache.UserIDToDiscordID(partyMember.GetUserId()))
				}

				// Get a list of the all the inactive users in the party group
				userIDs, err := GetPartyGroupUserIDs(ctx, d.nk, groupName)
				if err != nil {
					return fmt.Errorf("failed to get party group user IDs: %w", err)
				}

				// remove the partymembers from the inactive list
				inactiveIDs := make([]string, 0, len(userIDs))

			OuterLoop:
				for _, u := range userIDs {
					for _, partyMember := range partyMembers {
						if partyMember.GetUserId() == u {
							continue OuterLoop
						}
					}
					inactiveIDs = append(inactiveIDs, d.cache.UserIDToDiscordID(u))
				}

				// Create a list of the members, and the mode of the lobby they are currently in, linked to an echotaxi link, and whether they are matchmaking.
				// <@discord_id> - [mode](https://echo.taxi/spark://c/match_id) (matchmaking)

				var content string
				if len(activeIDs) == 0 && len(inactiveIDs) == 0 {
					content = "No members in your party group."
				} else {
					b := strings.Builder{}
					b.WriteString(fmt.Sprintf("Members in party group `%s`:\n", groupName))
					for i, discordID := range activeIDs {
						if i > 0 {
							b.WriteString(", ")
						}
						b.WriteString("<@" + discordID + ">")
					}
					if len(inactiveIDs) > 0 {
						b.WriteString("\n\nInactive or offline members:\n")
						for i, discordID := range inactiveIDs {
							if i > 0 {
								b.WriteString(", ")
							}
							b.WriteString("<@" + discordID + ">")
						}
					}
					content = b.String()
				}

				// Send the message to the user
				return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Flags:   discordgo.MessageFlagsEphemeral,
						Content: content,
					},
				})

			case "group":

				options := options[0].Options
				groupName := options[0].StringValue()
				// Validate the group is 1 to 12 characters long
				if len(groupName) < 1 || len(groupName) > 12 {
					return errors.New("invalid group ID. It must be between one (1) and eight (8) characters long")
				}
				// Validate the group is alphanumeric
				if !partyGroupIDPattern.MatchString(groupName) {
					return errors.New("invalid group ID. It must be alphanumeric")
				}
				// Validate the group is not a reserved group
				if lo.Contains([]string{"admin", "moderator", "verified", "serverhosts"}, groupName) {
					return errors.New("invalid group ID. It is a reserved group")
				}
				// lowercase the group
				groupName = strings.ToLower(groupName)

				objs, err := nk.StorageRead(ctx, []*runtime.StorageRead{
					{
						Collection: MatchmakerStorageCollection,
						Key:        MatchmakingConfigStorageKey,
						UserID:     userID,
					},
				})
				if err != nil {
					logger.Error("Failed to read matchmaking config", zap.Error(err))
				}
				matchmakingConfig := &MatchmakingSettings{}
				if len(objs) != 0 {
					if err := json.Unmarshal([]byte(objs[0].Value), matchmakingConfig); err != nil {
						return fmt.Errorf("failed to unmarshal matchmaking config: %w", err)
					}
				}
				matchmakingConfig.LobbyGroupName = groupName
				// Store it back

				data, err := json.Marshal(matchmakingConfig)
				if err != nil {
					return fmt.Errorf("failed to marshal matchmaking config: %w", err)
				}

				if _, err := nk.StorageWrite(ctx, []*runtime.StorageWrite{
					{
						Collection:      MatchmakerStorageCollection,
						Key:             MatchmakingConfigStorageKey,
						UserID:          userID,
						Value:           string(data),
						PermissionRead:  1,
						PermissionWrite: 0,
					},
				}); err != nil {
					return fmt.Errorf("failed to write matchmaking config: %w", err)
				}

				// Inform the user of the groupid
				return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Flags:   discordgo.MessageFlagsEphemeral,
						Content: fmt.Sprintf("Your group ID has been set to `%s`. Everyone must matchmake at the same time (~15-30 seconds)", groupName),
					},
				})
			}
			return discordgo.ErrNilState
		},
	}

	dg.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		user, _ := getScopedUserMember(i)

		logger := d.logger.WithFields(map[string]any{
			"discord_id": user.ID,
			"username":   user.Username,
			"guild_id":   i.GuildID,
			"channel_id": i.ChannelID,
			"user_id":    d.cache.DiscordIDToUserID(user.ID),
			"group_id":   d.cache.GuildIDToGroupID(i.GuildID),
		})

		switch i.Type {

		case discordgo.InteractionApplicationCommand:

			appCommandName := i.ApplicationCommandData().Name
			logger = logger.WithFields(map[string]any{
				"app_command": appCommandName,
				"options":     i.ApplicationCommandData().Options,
			})

			logger.Info("Handling application command.")
			if handler, ok := commandHandlers[appCommandName]; ok {
				err := d.handleInteractionApplicationCommand(logger, s, i, appCommandName, handler)
				if err != nil {
					logger.WithField("err", err).Error("Failed to handle interaction")
					// Queue the user to be updated in the cache
					userID := d.cache.DiscordIDToUserID(user.ID)
					groupID := d.cache.GuildIDToGroupID(i.GuildID)
					if userID != "" && groupID != "" {
						d.cache.QueueSyncMember(i.GuildID, user.ID)
					}
					if err := simpleInteractionResponse(s, i, err.Error()); err != nil {
						return
					}
				}
			} else {
				logger.Info("Unhandled command: %v", appCommandName)
			}
		case discordgo.InteractionMessageComponent:

			customID := i.MessageComponentData().CustomID
			commandName, value, _ := strings.Cut(customID, ":")

			logger = logger.WithFields(map[string]any{
				"custom_id": commandName,
				"value":     value,
			})

			logger.Info("Handling interaction message component.")

			err := d.handleInteractionMessageComponent(logger, s, i, commandName, value)
			if err != nil {
				logger.WithField("err", err).Error("Failed to handle interaction message component")
				if err := simpleInteractionResponse(s, i, err.Error()); err != nil {
					return
				}
			}
		case discordgo.InteractionApplicationCommandAutocomplete:

			data := i.ApplicationCommandData()
			appCommandName := i.ApplicationCommandData().Name
			logger = logger.WithFields(map[string]any{
				"app_command_autocomplete": appCommandName,
				"options":                  i.ApplicationCommandData().Options,
			})

			switch appCommandName {
			case "unlink-headset":
				userID := d.cache.DiscordIDToUserID(user.ID)
				if userID == "" {
					logger.Error("Failed to get user ID")
					return
				}

				account, err := nk.AccountGetId(ctx, userID)
				if err != nil {
					logger.Error("Failed to get account", zap.Error(err))
				}

				devices := make([]string, 0, len(account.Devices))
				for _, device := range account.Devices {
					devices = append(devices, device.GetId())
				}

				if data.Options[0].StringValue() != "" {
					// User is typing a custom device name
					for i := 0; i < len(devices); i++ {
						if !strings.Contains(strings.ToLower(devices[i]), strings.ToLower(data.Options[0].StringValue())) {
							devices = append(devices[:i], devices[i+1:]...)
							i--
						}
					}
				}

				choices := make([]*discordgo.ApplicationCommandOptionChoice, len(account.Devices))
				for i, device := range account.Devices {
					choices[i] = &discordgo.ApplicationCommandOptionChoice{
						Name:  device.GetId(),
						Value: device.GetId(),
					}
				}

				if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionApplicationCommandAutocompleteResult,
					Data: &discordgo.InteractionResponseData{
						Choices: choices, // This is basically the whole purpose of autocomplete interaction - return custom options to the user.
					},
				}); err != nil {
					logger.Error("Failed to respond to interaction", zap.Error(err))
				}
			}

		default:
			logger.Info("Unhandled interaction type: %v", i.Type)
		}
	})

	d.logger.Info("Registering slash commands.")
	// Register global guild commands
	d.updateSlashCommands(dg, d.logger, "")
	d.logger.Info("%d Slash commands registered/updated in %d guilds.", len(mainSlashCommands), len(dg.State.Guilds))

	return nil
}
func (d *DiscordAppBot) updateSlashCommands(s *discordgo.Session, logger runtime.Logger, guildID string) {
	// create a map of current commands
	currentCommands := make(map[string]*discordgo.ApplicationCommand, 0)
	for _, command := range mainSlashCommands {
		currentCommands[command.Name] = command
	}

	// Get the bot's current global application commands
	commands, err := s.ApplicationCommands(s.State.Application.ID, guildID)
	if err != nil {
		logger.WithField("err", err).Error("Failed to get application commands.")
		return
	}

	// Create a map for comparison
	registeredCommands := make(map[string]*discordgo.ApplicationCommand, 0)
	for _, command := range commands {
		registeredCommands[command.Name] = command
	}

	add, remove := lo.Difference(lo.Keys(currentCommands), lo.Keys(registeredCommands))

	// Remove any commands that are not in the mainSlashCommands
	for _, name := range remove {
		command := registeredCommands[name]
		logger.Debug("Deleting %s command: %s", guildID, command.Name)
		if err := s.ApplicationCommandDelete(s.State.Application.ID, guildID, command.ID); err != nil {
			logger.WithField("err", err).Error("Failed to delete application command.")
		}
	}

	// Add any commands that are in the mainSlashCommands
	for _, name := range add {
		command := currentCommands[name]
		logger.Debug("Creating %s command: %s", guildID, command.Name)
		if _, err := s.ApplicationCommandCreate(s.State.Application.ID, guildID, command); err != nil {
			logger.WithField("err", err).Error("Failed to create application command: %s", command.Name)
		}
	}

	// Edit existing commands
	for _, command := range currentCommands {
		if registered, ok := registeredCommands[command.Name]; ok {
			command.ID = registered.ID
			if !cmp.Equal(registered, command) {
				logger.Debug("Updating %s command: %s", guildID, command.Name)
				if _, err := s.ApplicationCommandEdit(s.State.Application.ID, guildID, registered.ID, command); err != nil {
					logger.WithField("err", err).Error("Failed to edit application command: %s", command.Name)
				}
			}
		}
	}
}

func (d *DiscordAppBot) getPartyDiscordIds(ctx context.Context, partyHandler *PartyHandler) (map[string]string, error) {
	partyHandler.RLock()
	defer partyHandler.RUnlock()
	memberMap := make(map[string]string, len(partyHandler.members.presences)+1)
	leaderID, err := GetDiscordIDByUserID(ctx, d.db, partyHandler.leader.UserPresence.GetUserId())
	if err != nil {
		return nil, err
	}
	memberMap[leaderID] = partyHandler.leader.UserPresence.GetUserId()

	for _, presence := range partyHandler.members.presences {
		if presence.UserPresence.GetUserId() == partyHandler.leader.UserPresence.GetUserId() {
			continue
		}
		discordID, err := GetDiscordIDByUserID(ctx, d.db, presence.UserPresence.GetUserId())
		if err != nil {
			return nil, err
		}
		memberMap[discordID] = presence.UserPresence.UserId
	}
	return memberMap, nil
}

func (d *DiscordAppBot) ManageUserGroups(ctx context.Context, logger runtime.Logger, db *sql.DB, nk runtime.NakamaModule, initializer runtime.Initializer, callerUsername string, action string, usernames []string, groupNames []string) error {
	// FIXME validate the discord caller has rights to add to this group (i.e. is a admin of the group)
	// lookup the nakama group

	// Get nakama User ID from the discord ID
	users, err := nk.UsersGetUsername(ctx, append(usernames, callerUsername))
	if err != nil {
		logger.WithField("err", err).Error("Users get username error.")
	}

	callerId := ""
	userIds := make([]string, 0, len(users))
	for _, user := range users {
		if user.GetUsername() == callerUsername {
			callerId = user.GetId()
			continue
		}
		userIds = append(userIds, user.GetId())
	}

	if callerId == "" {
		logger.WithField("err", err).Error("Users get username error.")
		return fmt.Errorf("users get caller user id error: %w", err)
	}
	if len(userIds) == 0 {
		logger.WithField("err", err).Error("Users get username error.")
		return fmt.Errorf("get user id error: %w", err)
	}

	// Get the group ids
	for _, groupName := range groupNames {
		list, _, err := nk.GroupsList(ctx, groupName, "", nil, nil, 1, "")
		if err != nil || (list == nil) || (len(list) == 0) {
			logger.WithField("err", err).Error("Group list error.")
			return fmt.Errorf("group (%s) list error: %w", groupName, err)
		}
		groupId := list[0].GetId()

		switch action {
		case "add":
			if err := nk.GroupUsersAdd(ctx, callerId, groupId, userIds); err != nil {
				logger.WithField("err", err).Error("Group user add error.")
				return fmt.Errorf("group add user failed: %w", err)
			}
		case "remove":
			if err := nk.GroupUsersKick(ctx, callerId, groupId, userIds); err != nil {
				logger.WithField("err", err).Error("Group user add error.")
				return fmt.Errorf("group add user failed: %w", err)
			}
		case "ban":
			if err := nk.GroupUsersBan(ctx, callerId, groupId, userIds); err != nil {
				logger.WithField("err", err).Error("Group user add error.")
				return fmt.Errorf("group add user failed: %w", err)
			}
		}
	}

	return nil
}

func (d *DiscordAppBot) sendPartyInvite(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, inviter, invitee *discordgo.User) error {
	/*

		if inviter.ID == invitee.ID {
			return fmt.Errorf("you cannot invite yourself to a party")
		}
			// Get the inviter's session id
			userID, sessionID, err := getLoginSessionForUser(ctx, inviter.ID, discordRegistry, pipeline)
			if err != nil {
				return err
			}

			// Get the invitee's session id
			inviteeUserID, inviteeSessionID, err := getLoginSessionForUser(ctx, invitee.ID, discordRegistry, pipeline)
			if err != nil {
				return err
			}

			// Get or create the party
			ph, err := getOrCreateParty(ctx, pipeline, discordRegistry, userID, inviter.Username, sessionID, inviter.ID)
			if err != nil {
				return err
			}

			ph.Lock()
			defer ph.Unlock()
			// Check if the invitee is already in the party
			for _, p := range ph.members.presences {
				if p.UserPresence.UserId == inviteeUserID.String() {
					return fmt.Errorf("<@%s> is already in your party.", invitee.ID)
				}
			}

			// Create join request for invitee
			_, err = pipeline.partyRegistry.PartyJoinRequest(ctx, ph.ID, pipeline.node, &Presence{
				ID: PresenceID{
					Node:      pipeline.node,
					SessionID: inviteeSessionID,
				},
				// Presence stream not needed.
				UserID: inviteeSessionID,
				Meta: PresenceMeta{
					Username: invitee.Username,
					// Other meta fields not needed.
				},
			})
			if err != nil {
				return fmt.Errorf("failed to create join request: %w", err)
			}
	*/

	partyID := uuid.Must(uuid.NewV4())
	inviteeSessionID := uuid.Must(uuid.NewV4())
	// Send ephemeral message to inviter
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Flags:   discordgo.MessageFlagsEphemeral,
			Content: fmt.Sprintf("Invited %s to a party. Waiting for response.", invitee.Username),
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.Button{
							Label:    "Cancel Invitation",
							Style:    discordgo.DangerButton,
							Disabled: false,
							CustomID: fmt.Sprintf("fd_cancel_invite:%s:%s", partyID, inviteeSessionID),
						},
					},
				},
			},
		},
	}); err != nil {
		return fmt.Errorf("failed to send interaction response: %w", err)
	}

	// Send invite message to invitee
	channel, err := s.UserChannelCreate(invitee.ID)
	if err != nil {
		return fmt.Errorf("failed to create user channel: %w", err)
	}

	// Send the invite message
	s.ChannelMessageSendComplex(channel.ID, &discordgo.MessageSend{
		Content: fmt.Sprintf("%s has invited you to their in-game EchoVR party.", inviter.Username),
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					discordgo.Button{
						// Label is what the user will see on the button.
						Label: "Accept",
						// Style provides coloring of the button. There are not so many styles tho.
						Style: discordgo.PrimaryButton,
						// Disabled allows bot to disable some buttons for users.
						Disabled: false,
						// CustomID is a thing telling Discord which data to send when this button will be pressed.
						CustomID: fmt.Sprintf("fd_accept_invite:%s:%s", partyID, inviteeSessionID),
					},
					discordgo.Button{
						Label:    "Decline",
						Style:    discordgo.DangerButton,
						Disabled: false,
						CustomID: fmt.Sprintf("fd_decline_invite:%s:%s", partyID, inviteeSessionID),
					},
				},
			},
		},
	})

	// Set a timer to delete the messages after 30 seconds
	time.AfterFunc(30*time.Second, func() {
		s.ChannelMessageDelete(inviter.ID, i.ID)
	})
	return nil
}

func getScopedUser(i *discordgo.InteractionCreate) *discordgo.User {
	switch {
	case i.User != nil:
		return i.User
	case i.Member.User != nil:
		return i.Member.User
	default:
		return nil
	}
}

func getScopedUserMember(i *discordgo.InteractionCreate) (user *discordgo.User, member *discordgo.Member) {
	if i.User != nil {
		user = i.User
	}

	if i.Member != nil {
		member = i.Member
		if i.Member.User != nil {
			user = i.Member.User
		}
	}
	return user, member
}

func simpleInteractionResponse(s *discordgo.Session, i *discordgo.InteractionCreate, content string) error {
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Flags:   discordgo.MessageFlagsEphemeral,
			Content: content,
		},
	})
}

func (d *DiscordAppBot) createRegionStatusEmbed(ctx context.Context, logger runtime.Logger, regionStr string, channelID string, existingMessage *discordgo.Message) error {
	// list all the matches

	matches, err := d.nk.MatchList(ctx, 100, true, "", nil, nil, "")
	if err != nil {
		return err
	}

	regionSymbol := evr.ToSymbol(regionStr)

	tracked := make([]*MatchLabel, 0, len(matches))

	for _, match := range matches {

		state := &MatchLabel{}
		if err := json.Unmarshal([]byte(match.GetLabel().GetValue()), state); err != nil {
			logger.Error("Failed to unmarshal match label", zap.Error(err))
			continue
		}

		for _, r := range state.Broadcaster.Regions {
			if regionSymbol == r {
				tracked = append(tracked, state)
				continue
			}
		}
	}
	if len(tracked) == 0 {
		return fmt.Errorf("no matches found in region %s", regionStr)
	}

	// Create a message embed that contains a table of the server, the creation time, the number of players, and the spark link
	embed := &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("Region %s", regionStr),
		Description: fmt.Sprintf("updated <t:%d:f>", time.Now().UTC().Unix()),
		Fields:      make([]*discordgo.MessageEmbedField, 0),
	}

	for _, state := range tracked {
		var status string

		if state.LobbyType == UnassignedLobby {
			status = "Unassigned"
		} else if state.Size == 0 {
			if !state.Started() {
				spawnedBy := "unknown"
				if state.SpawnedBy != "" {
					spawnedBy, err = GetDiscordIDByUserID(ctx, d.db, state.SpawnedBy)
					if err != nil {
						logger.Error("Failed to get discord ID", zap.Error(err))
					}
				}
				status = fmt.Sprintf("Reserved by <@%s> <t:%d:R>", spawnedBy, state.StartTime.UTC().Unix())
			} else {
				status = "Empty"
			}
		} else {
			players := make([]string, 0, state.Size)
			for _, player := range state.Players {
				players = append(players, fmt.Sprintf("`%s` (`%s`)", player.DisplayName, player.Username))
			}
			status = fmt.Sprintf("%s: %s", state.Mode.String(), strings.Join(players, ", "))

		}

		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   strconv.FormatUint(state.Broadcaster.ServerID, 10),
			Value:  status,
			Inline: false,
		})
	}

	if existingMessage != nil {
		t, err := discordgo.SnowflakeTimestamp(existingMessage.ID)
		if err != nil {
			return err
		}

		embed.Footer = &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("Expires %s", t.Format(time.RFC1123)),
		}
		// Update the message for the given region
		_, err = d.dg.ChannelMessageEditEmbed(channelID, existingMessage.ID, embed)
		if err != nil {
			return err
		}

		return nil
	} else {
		// Create the message and update it regularly
		msg, err := d.dg.ChannelMessageSendEmbed(channelID, embed)
		if err != nil {
			return err
		}

		go func() {
			timer := time.NewTimer(24 * time.Hour)
			defer timer.Stop()
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-d.ctx.Done():
					// Delete the message
					if err := d.dg.ChannelMessageDelete(channelID, msg.ID); err != nil {
						logger.Error("Failed to delete region status message: %s", err.Error())

					}
					return
				case <-timer.C:
					// Delete the message
					if err := d.dg.ChannelMessageDelete(channelID, msg.ID); err != nil {
						logger.Error("Failed to delete region status message: %s", err.Error())
					}
					return
				case <-ticker.C:
					// Update the message
					err := d.createRegionStatusEmbed(ctx, logger, regionStr, channelID, msg)
					if err != nil {
						logger.Error("Failed to update region status message: %s", err.Error())
						return
					}
				}
			}
		}()
	}
	return nil
}

var discordMarkdownEscapeReplacer = strings.NewReplacer(
	`\`, `\\`,
	"`", "\\`",
	"~", "\\~",
	"|", "\\|",
	"**", "\\**",
	"~~", "\\~~",
	"@", "@\u200B",
	"<", "<\u200B",
	">", ">\u200B",
	"_", "\\_",
)

func EscapeDiscordMarkdown(s string) string {
	return discordMarkdownEscapeReplacer.Replace(s)
}

func (d *DiscordAppBot) SendIPApprovalRequest(ctx context.Context, userID, ip string, ipqs *IPQSResponse) error {
	// Get the user's discord ID
	discordID, err := GetDiscordIDByUserID(ctx, d.db, userID)
	if err != nil {
		return err
	}

	// Send the message to the user
	channel, err := d.dg.UserChannelCreate(discordID)
	if err != nil {
		return err
	}

	// Send the message

	embed := &discordgo.MessageEmbed{
		Title:       "New Login Location",
		Description: "Please verify the login attempt from a new location.",
		Color:       0x00ff00,
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "IP Address",
				Value:  ip,
				Inline: true,
			}},
	}

	if ipqs != nil {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   "Location",
			Value:  fmt.Sprintf("%s, %s", ipqs.City, ipqs.Region),
			Inline: true,
		})

	}
	embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
		Name:   "Note",
		Value:  "If this login attempt was not made by you, please report it to EchoVRCE using the button below.",
		Inline: false,
	})
	components := []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					Label:    "This is Me",
					Style:    discordgo.SuccessButton,
					CustomID: fmt.Sprintf("approve_ip:%s", ip),
				},
				discordgo.Button{
					Label:    "Report",
					Style:    discordgo.LinkButton,
					URL:      "https://discord.gg/AMMYQXcapm",
					Disabled: false,
				},
			},
		},
	}

	_, err = d.dg.ChannelMessageSendComplex(channel.ID, &discordgo.MessageSend{
		Embeds:     []*discordgo.MessageEmbed{embed},
		Components: components,
	})
	if err != nil {
		return err
	}

	return nil
}
