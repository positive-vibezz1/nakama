package server

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/heroiclabs/nakama/v3/server/evr"
	"github.com/samber/lo"
)

const (
	StorageCollectionGroupProfile = "GroupProfile"
	StorageKeyUnlockedItems       = "unlocks"
)

type GroupProfile struct {
	UserID        string       `json:"user_id"`
	GroupID       string       `json:"group_id"`
	UnlockedItems []evr.Symbol `json:"unlocked_items"`
	NewUnlocks    []evr.Symbol `json:"new_unlocks"`
	UpdateTime    time.Time    `json:"update_time"`
}

func (p GroupProfile) GetStorageID() StorageID {
	return StorageID{Collection: StorageCollectionGroupProfile, Key: p.GroupID}

}

func (p *GroupProfile) UpdateUnlockedItems(updated []evr.Symbol) {
	// Update the unlocked items, adding the new ones to newUnlocks
	added, removed := lo.Difference(updated, p.UnlockedItems)

	if len(added) == 0 && len(removed) == 0 {
		return
	}

	p.UnlockedItems = updated
	p.NewUnlocks = append(p.NewUnlocks, added...)

	// Ensure that all new unlocks are unique, and exist in the updated list
	updatedNewUnlocks := make([]evr.Symbol, 0, len(p.NewUnlocks))

	seen := make(map[evr.Symbol]struct{}, len(p.NewUnlocks))
	for _, unlock := range p.NewUnlocks {
		if _, ok := seen[unlock]; !ok {
			seen[unlock] = struct{}{}
			updatedNewUnlocks = append(updatedNewUnlocks, unlock)
		}
	}

	p.NewUnlocks = updatedNewUnlocks
	p.UpdateTime = time.Now()
}

type EVRAccount struct {
	AccountMetadata
	User        *api.User
	Wallet      string
	Email       string
	Devices     []*api.AccountDevice
	CustomId    string
	VerifyTime  time.Time
	DisableTime time.Time
}

func NewEVRAccount(account *api.Account) (*EVRAccount, error) {
	md := AccountMetadata{}
	if err := json.Unmarshal([]byte(account.User.Metadata), &md); err != nil {
		return nil, fmt.Errorf("error unmarshalling account metadata: %w", err)
	}
	md.account = account

	a := &EVRAccount{
		AccountMetadata: md,
		User:            account.User,
		Wallet:          account.Wallet,
		Email:           account.Email,
		Devices:         account.Devices,
		CustomId:        account.CustomId,
	}
	if account.VerifyTime != nil {
		a.VerifyTime = account.VerifyTime.AsTime()
	}
	if account.DisableTime != nil {
		a.DisableTime = account.DisableTime.AsTime()
	}

	return a, nil
}

func (e *EVRAccount) IsDisabled() bool {
	return !e.DisableTime.IsZero()
}

func (e *EVRAccount) IsLinked() bool {
	return len(e.Devices) > 0
}

type AccountMetadata struct {
	account *api.Account

	RandomizeDisplayName   bool              `json:"randomize_display_name"`    // Randomize the display name
	RandomizedDisplayName  string            `json:"randomized_display_name"`   // The randomized display name
	DisplayNameOverride    string            `json:"display_name_override"`     // The display name override
	GlobalBanReason        string            `json:"global_ban_reason"`         // The global ban reason
	ActiveGroupID          string            `json:"active_group_id"`           // The active group ID
	GroupDisplayNames      map[string]string `json:"group_display_names"`       // The display names for each guild map[groupID]displayName
	DisableAFKTimeout      bool              `json:"disable_afk_timeout"`       // Disable AFK detection
	TargetUserID           string            `json:"target_user_id"`            // The target user ID to follow in public spaces
	DiscordDebugMessages   bool              `json:"discord_debug_messages"`    // Enable debug messages in Discord
	RelayMessagesToDiscord bool              `json:"relay_messages_to_discord"` // Relay messages to Discord
	isModified             bool              // Indicates whether the account metadata has been modified
	DiscordUser            *discordgo.User   `json:"discord_user"` // The Discord user object
}

func (a *AccountMetadata) ID() string {
	return a.account.User.Id
}

func (a *AccountMetadata) DiscordID() string {
	return a.account.CustomId
}

func (a *AccountMetadata) Username() string {
	return a.account.User.Username
}

func (a *AccountMetadata) DisplayName() string {
	return a.account.User.DisplayName
}

func (a *AccountMetadata) LangTag() string {
	return a.account.User.LangTag
}

func (a *AccountMetadata) AvatarURL() string {
	return a.account.User.AvatarUrl
}

func (a *AccountMetadata) DiscordAccountCreationTime() time.Time {
	t, _ := discordgo.SnowflakeTimestamp(a.DiscordID())
	return t
}

func (a *AccountMetadata) GetActiveGroupID() uuid.UUID {
	if a.ActiveGroupID == "" {
		return uuid.Nil
	}
	return uuid.FromStringOrNil(a.ActiveGroupID)
}

func (a *AccountMetadata) SetActiveGroupID(id uuid.UUID) {
	if a.ActiveGroupID == id.String() {
		return
	}
	a.ActiveGroupID = id.String()
	a.isModified = true
}

func (a *AccountMetadata) GetDisplayName(groupID string) string {
	if a.GroupDisplayNames == nil {
		a.GroupDisplayNames = make(map[string]string)
	}
	if dn, ok := a.GroupDisplayNames[groupID]; ok {
		return dn
	}
	return ""
}

func (a *AccountMetadata) GetGroupDisplayNameOrDefault(groupID string) string {

	if a.GroupDisplayNames == nil {
		a.GroupDisplayNames = make(map[string]string)
	}
	if a.DisplayNameOverride != "" {
		return a.DisplayNameOverride
	}
	if dn, ok := a.GroupDisplayNames[groupID]; ok && dn != "" {
		return dn
	}
	if dn, ok := a.GroupDisplayNames[a.ActiveGroupID]; ok && dn != "" {
		return dn
	} else {
		return a.account.User.Username
	}
}

func (a *AccountMetadata) SetGroupDisplayName(groupID, displayName string) bool {
	if a.GroupDisplayNames == nil {
		a.GroupDisplayNames = make(map[string]string)
	}
	if a.GroupDisplayNames[groupID] == displayName {
		return false
	}
	a.GroupDisplayNames[groupID] = displayName
	return true
}

func (a *AccountMetadata) GetActiveGroupDisplayName() string {
	return a.GetGroupDisplayNameOrDefault(a.ActiveGroupID)
}

func (a *AccountMetadata) MarshalMap() map[string]interface{} {
	b, _ := json.Marshal(a)
	var m map[string]interface{}
	_ = json.Unmarshal(b, &m)
	return m
}

func (a *AccountMetadata) NeedsUpdate() bool {
	return a.isModified
}

type AccountCosmetics struct {
	JerseyNumber int64               `json:"number"`           // The loadout number (jersey number)
	Loadout      evr.CosmeticLoadout `json:"cosmetic_loadout"` // The loadout
}

func GetDisplayNameByGroupID(ctx context.Context, nk runtime.NakamaModule, userID, groupID string) (string, error) {
	md, err := GetAccountMetadata(ctx, nk, userID)
	if err != nil {
		return md.account.GetUser().GetDisplayName(), fmt.Errorf("error unmarshalling account user metadata: %w", err)
	}

	if dn := md.GetGroupDisplayNameOrDefault(groupID); dn != "" {
		return dn, nil
	}
	if dn := md.account.GetUser().GetDisplayName(); dn != "" {
		return dn, nil
	} else {
		return md.account.GetUser().GetUsername(), nil
	}
}

func UserGuildGroupsList(ctx context.Context, nk runtime.NakamaModule, userID string) (map[string]*GuildGroup, error) {
	groups := make(map[string]*GuildGroup, 0)
	cursor := ""
	for {
		// Fetch the groups using the provided userId
		userGroups, _, err := nk.UserGroupsList(ctx, userID, 100, nil, cursor)
		if err != nil {
			return nil, fmt.Errorf("error getting user groups: %w", err)
		}

		for _, ug := range userGroups {
			g := ug.GetGroup()
			if g.GetLangTag() != "guild" {
				continue
			}
			gg, err := NewGuildGroup(g)
			if err != nil {
				return nil, fmt.Errorf("error creating guild group: %w", err)
			}

			groups[g.GetId()] = gg
		}
		if cursor == "" {
			break
		}
	}
	return groups, nil
}

func GetGuildGroupMemberships(ctx context.Context, nk runtime.NakamaModule, userID string) (map[string]GuildGroupMembership, error) {

	// Get the caller's nakama user ID
	groups, err := UserGuildGroupsList(ctx, nk, userID)
	if err != nil {
		return nil, fmt.Errorf("error getting user guild groups: %w", err)
	}

	memberships := make(map[string]GuildGroupMembership, 0)
	for _, group := range groups {
		memberships[group.ID().String()] = *group.PermissionsUser(userID)
	}

	return memberships, nil
}
