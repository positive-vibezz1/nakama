package server

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/heroiclabs/nakama/v3/server/evr"
	"github.com/intinig/go-openskill/types"
	"github.com/samber/lo"
)

type GameProfile interface {
	GetVersion() string
	GetXPID() evr.XPID
	SetLogin(login evr.LoginProfile)
	SetClient(client evr.ClientProfile)
	SetServer(server evr.ServerProfile)
	GetServer() evr.ServerProfile
	GetClient() evr.ClientProfile
	GetLogin() evr.LoginProfile
	GetChannel() uuid.UUID
	SetChannel(c evr.GUID)
	UpdateDisplayName(displayName string)
	UpdateUnlocks(unlocks evr.UnlockedCosmetics) error
	IsStale() bool
	SetStale()
}

type GameProfileData struct {
	Login      evr.LoginProfile                          `json:"login"`
	Client     evr.ClientProfile                         `json:"client"`
	Server     evr.ServerProfile                         `json:"server"`
	Ratings    map[uuid.UUID]map[evr.Symbol]types.Rating `json:"ratings"`
	EarlyQuits EarlyQuitStatistics                       `json:"early_quit"`
	Version    string                                    // The version of the profile from the DB
	Stale      bool                                      // Whether the profile is stale and needs to be updated
}

func NewGameProfile(login evr.LoginProfile, client evr.ClientProfile, server evr.ServerProfile, version string) GameProfileData {
	return GameProfileData{
		Login:   login,
		Client:  client,
		Server:  server,
		Version: version,
		Ratings: make(map[uuid.UUID]map[evr.Symbol]types.Rating),
	}
}

func (p *GameProfileData) GetVersion() string {
	return p.Version
}

func (p *GameProfileData) SetLogin(login evr.LoginProfile) {
	p.Login = login
	p.SetStale()
}

func (p *GameProfileData) SetClient(client evr.ClientProfile) {
	p.Client = client
	p.SetStale()
}

func (p *GameProfileData) SetServer(server evr.ServerProfile) {
	p.Server = server
	p.SetStale()
}

func (p *GameProfileData) SetEarlyQuitStatistics(stats EarlyQuitStatistics) {
	p.EarlyQuits = stats
	p.SetStale()
}

func (p *GameProfileData) GetServer() evr.ServerProfile {
	return p.Server
}

func (p *GameProfileData) GetClient() evr.ClientProfile {
	return p.Client
}

func (p *GameProfileData) GetLogin() evr.LoginProfile {
	return p.Login
}

func (p *GameProfileData) GetEarlyQuitStatistics() *EarlyQuitStatistics {
	return &p.EarlyQuits
}

func (p *GameProfileData) GetRating(groupID uuid.UUID, mode evr.Symbol) types.Rating {
	if p.Ratings == nil || p.Ratings[groupID] == nil || p.Ratings[groupID][mode] == (types.Rating{}) {
		p.SetRating(groupID, mode, NewDefaultRating())
	}
	return p.Ratings[groupID][mode]
}

func (p *GameProfileData) SetRating(groupID uuid.UUID, mode evr.Symbol, rating types.Rating) {
	if p.Ratings == nil {
		p.Ratings = make(map[uuid.UUID]map[evr.Symbol]types.Rating)
	}
	if p.Ratings[groupID] == nil {
		p.Ratings[groupID] = make(map[evr.Symbol]types.Rating)
	}
	p.Ratings[groupID][mode] = rating
	p.SetStale()
}

func (p *GameProfileData) SetXPID(xpID evr.XPID) {
	if p.Server.XPID == xpID && p.Client.XPID == xpID {
		return
	}
	p.Server.XPID = xpID
	p.Client.XPID = xpID
	p.SetStale()

}

func (p *GameProfileData) GetXPID() evr.XPID {
	return p.Server.XPID
}

func (p *GameProfileData) GetChannel() uuid.UUID {
	return uuid.UUID(p.Server.Social.Channel)
}

func (p *GameProfileData) SetJerseyNumber(number int) {
	if p.Server.EquippedCosmetics.Number == number && p.Server.EquippedCosmetics.NumberBody == number {
		return
	}

	p.Server.EquippedCosmetics.Number = number
	p.Server.EquippedCosmetics.NumberBody = number
	p.SetStale()
}

func (p *GameProfileData) SetChannel(c evr.GUID) {
	if p.Server.Social.Channel == c && p.Client.Social.Channel == c {
		return
	}
	p.Server.Social.Channel = c
	p.Client.Social.Channel = p.Server.Social.Channel
	p.SetStale()
}

func (p *GameProfileData) UpdateDisplayName(displayName string) {
	if p.Server.DisplayName == displayName && p.Client.DisplayName == displayName {
		return
	}
	p.Server.DisplayName = displayName
	p.Client.DisplayName = displayName
	p.SetStale()
}

func (p *GameProfileData) LatestStatistics(useGlobal bool, useWeekly bool, useDaily bool) evr.PlayerStatistics {

	allStats := evr.PlayerStatistics{
		"arena":  make(map[string]evr.MatchStatistic),
		"combat": make(map[string]evr.MatchStatistic),
	}

	// Start with all time stats

	if useGlobal {
		for t, s := range p.Server.Statistics {
			if !strings.HasPrefix(t, "daily_") && !strings.HasPrefix(t, "weekly_") {
				allStats[t] = s
			}
		}
	}
	var latestWeekly, latestDaily string

	for t := range p.Server.Statistics {
		if strings.HasPrefix(t, "weekly_") && (latestWeekly == "" || t > latestWeekly) {
			latestWeekly = t
		}
		if strings.HasPrefix(t, "daily_") && (latestDaily == "" || t > latestDaily) {
			latestDaily = t
		}
	}

	if useWeekly && latestWeekly != "" {
		for k, v := range p.Server.Statistics[latestWeekly] {
			allStats["arena"][k] = v
		}
	}

	if useDaily && latestDaily != "" {
		for k, v := range p.Server.Statistics[latestDaily] {
			allStats["arena"][k] = v
		}
	}

	return allStats
}

func (p *GameProfileData) ExpireStatistics(dailyAge time.Duration, weeklyAge time.Duration) {
	updated := false
	for t := range p.Server.Statistics {
		if t == "arena" || t == "combat" {
			continue
		}
		if strings.HasPrefix(t, "daily_") {
			// Parse the date
			date, err := time.Parse("2006_01_02", strings.TrimPrefix(t, "daily_"))
			// Keep anything less than 48 hours old
			if err == nil && time.Since(date) < dailyAge {
				continue
			}
		} else if strings.HasPrefix(t, "weekly_") {
			// Parse the date
			date, err := time.Parse("2006_01_02", strings.TrimPrefix(t, "weekly_"))
			// Keep anything less than 2 weeks old
			if err == nil && time.Since(date) < weeklyAge {
				continue
			}
		}
		delete(p.Server.Statistics, t)
		updated = true
	}
	if updated {
		p.SetStale()
	}
}

func (p *GameProfileData) SetStale() {
	p.Stale = true
	p.Server.UpdateTime = time.Now().UTC().Unix()
	p.Client.ModifyTime = time.Now().UTC().Unix()
}

func (p *GameProfileData) IsStale() bool {
	return p.Stale
}
func (p *GameProfileData) DisableAFKTimeout(enable bool) {

	if enable {
		if p.Server.DeveloperFeatures == nil {
			p.Server.DeveloperFeatures = &evr.DeveloperFeatures{
				DisableAfkTimeout: true,
			}
			p.SetStale()
			return
		}
		if p.Server.DeveloperFeatures.DisableAfkTimeout {
			return
		}
		p.Server.DeveloperFeatures.DisableAfkTimeout = true
		p.SetStale()
		return
	}
	if p.Server.DeveloperFeatures == nil {
		return
	}
	if !p.Server.DeveloperFeatures.DisableAfkTimeout {
		return
	}
	p.Server.DeveloperFeatures.DisableAfkTimeout = false
	p.SetStale()
}

func (r *GameProfileData) UpdateUnlocks(unlocks evr.UnlockedCosmetics) error {
	// Validate the unlocks
	/*
		err := ValidateUnlocks(unlocks)
		if err != nil {
			return fmt.Errorf("failed to validate unlocks: %w", err)
		}
	*/
	// Remove newUnlocks that are not known cosmetics
	var unlocked, updated map[string]map[string]bool

	unlocked = r.Server.UnlockedCosmetics.ToMap()
	updated = unlocks.ToMap()

	// Remove any existing new unlocks that are not known cosmetics

	for i := 0; i < len(r.Client.NewUnlocks); i++ {
		u := evr.Symbol(r.Client.NewUnlocks[i])
		if _, ok := evr.SymbolCache[u]; !ok {
			r.Client.NewUnlocks = append(r.Client.NewUnlocks[:i], r.Client.NewUnlocks[i+1:]...)
			i--
		}
	}

	new := make([]int64, 0, 10)
	seen := make(map[string]struct{})
	for game, unlocks := range updated {
		byGame := unlocked[game]
		for item, u := range unlocks {
			seen[item] = struct{}{}
			if u && (byGame == nil || byGame[item] != u) {
				new = append(new, int64(evr.ToSymbol(item)))
			}
		}
	}

	if len(new) > 0 {
		r.Client.NewUnlocks = append(r.Client.NewUnlocks, new...)
		r.SetStale()
		//r.Client.Customization.NewUnlocksPoiVersion += 1
	}
	if r.Server.UnlockedCosmetics != unlocks {
		r.Server.UnlockedCosmetics = unlocks
		r.SetStale()
	}
	return nil
}

func (r *GameProfileData) TriggerCommunityValues() {
	r.Client.Social.CommunityValuesVersion = 0
	r.SetStale()
}

func generateDefaultLoadoutMap() map[string]string {
	return evr.DefaultCosmeticLoadout().ToMap()
}

func (r *ProfileRegistry) GetFieldByJSONProperty(i interface{}, itemName string) (bool, error) {
	// Lookup the field name by it's item name (json key)
	fieldName, found := r.unlocksByItemName[itemName]
	if !found {
		return false, fmt.Errorf("unknown item name: %s", itemName)
	}

	// Lookup the field value by it's field name
	value := reflect.ValueOf(i)
	typ := value.Type()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Name == fieldName {
			return value.FieldByName(fieldName).Bool(), nil
		}
	}

	return false, fmt.Errorf("unknown unlock field name: %s", fieldName)
}

func (r *ProfileRegistry) UpdateEquippedItem(profile *GameProfileData, category string, name string) error {

	// Get the current profile.
	unlocksArena := profile.Server.UnlockedCosmetics.Arena
	unlocksCombat := profile.Server.UnlockedCosmetics.Combat

	// Validate that this user has the item unlocked.
	unlocked, err := r.GetFieldByJSONProperty(unlocksArena, name)
	if err != nil {
		// Check if it is a combat unlock
		unlocked, err = r.GetFieldByJSONProperty(unlocksCombat, name)
		if err != nil {
			return fmt.Errorf("failed to validate unlock: %w", err)
		}
	}
	if !unlocked {
		return nil
	}

	alignmentTints := map[string][]string{
		"tint_alignment_a": {
			"tint_blue_a_default",
			"tint_blue_b_default",
			"tint_blue_c_default",
			"tint_blue_d_default",
			"tint_blue_e_default",
			"tint_blue_f_default",
			"tint_blue_g_default",
			"tint_blue_h_default",
			"tint_blue_i_default",
			"tint_blue_j_default",
			"tint_blue_k_default",
			"tint_neutral_summer_a_default",
			"rwd_tint_s3_tint_e",
		},
		"tint_alignment_b": {
			"tint_orange_a_default",
			"tint_orange_b_default",
			"tint_orange_c_default",
			"tint_orange_i_default",
			"tint_neutral_spooky_a_default",
			"tint_neutral_spooky_d_default",
			"tint_neutral_xmas_c_default",
			"rwd_tint_s3_tint_b",
			"tint_orange_j_default",
			"tint_orange_d_default",
			"tint_orange_e_default",
			"tint_orange_f_default",
			"tint_orange_g_default",
			"tint_orange_h_default",
			"tint_orange_k_default",
		},
	}

	s := &profile.Server.EquippedCosmetics.Instances.Unified.Slots

	s.DecalBody = "rwd_decalback_default"
	s.DecalBack = "rwd_decalback_default"

	// Exact mappings
	exactmap := map[string]*string{
		"emissive_default":      &s.Emissive,
		"rwd_decalback_default": &s.PIP,
	}

	if val, ok := exactmap[name]; ok {
		*val = name
	} else {

		switch category {
		case "emote":
			s.Emote = name
			s.SecondEmote = name
		case "decal", "decalback":
			s.Decal = name
			s.DecalBody = name
		case "tint":
			// Assigning a tint to the alignment will also assign it to the body
			if lo.Contains(alignmentTints["tint_alignment_a"], name) {
				s.TintAlignmentA = name
			} else if lo.Contains(alignmentTints["tint_alignment_b"], name) {
				s.TintAlignmentB = name
			}
			if name != "tint_chassis_default" {
				// Equipping "tint_chassis_default" to heraldry tint causes every heraldry equipped to be pitch black.
				// It seems that the tint being pulled from doesn't exist on heraldry equippables.
				s.Tint = name
			}
			s.TintBody = name

		case "pattern":
			s.Pattern = name
			s.PatternBody = name
		case "chassis":
			s.Chassis = name
		case "bracer":
			s.Bracer = name
		case "booster":
			s.Booster = name
		case "title":
			s.Title = name
		case "tag", "heraldry":
			s.Tag = name
		case "banner":
			s.Banner = name
		case "medal":
			s.Medal = name
		case "goal":
			s.GoalFX = name
		case "emissive":
			s.Emissive = name
		//case "decalback":
		//	fallthrough
		case "pip":
			s.PIP = name
		default:
			r.logger.Warn("Unknown cosmetic category `%s`", category)
			return nil
		}
	}
	// Update the timestamp
	profile.SetStale()
	return nil
}

// Set the user's profile based on their groups
func (r *ProfileRegistry) UpdateEntitledCosmetics(ctx context.Context, userID uuid.UUID, profile *GameProfileData) error {

	account, err := r.nk.AccountGetId(ctx, userID.String())
	if err != nil {
		return fmt.Errorf("failed to get account: %w", err)
	}
	wallet := make(map[string]int64, 0)
	if err := json.Unmarshal([]byte(account.Wallet), &wallet); err != nil {
		return fmt.Errorf("failed to unmarshal wallet: %w", err)
	}

	// Get the user's groups
	// Check if the user has any groups that would grant them cosmetics
	userGroups, _, err := r.nk.UserGroupsList(ctx, userID.String(), 100, nil, "")
	if err != nil {
		return fmt.Errorf("failed to get user groups: %w", err)
	}

	for i := 0; i < len(userGroups); i++ {
		// If the user is not a member of the group, don't include it.
		if userGroups[i].GetState().GetValue() > int32(api.GroupUserList_GroupUser_MEMBER) {
			// Remove the group
			userGroups = append(userGroups[:i], userGroups[i+1:]...)
			i--
		}
	}

	isDeveloper := false

	groupNames := make([]string, 0, len(userGroups))
	for _, userGroup := range userGroups {
		group := userGroup.GetGroup()
		name := group.GetName()
		groupNames = append(groupNames, name)
	}

	for _, name := range groupNames {
		switch name {
		case GroupGlobalDevelopers:
			isDeveloper = true

		}
	}

	// Disable Restricted Cosmetics
	enableAll := isDeveloper

	err = SetCosmeticDefaults(&profile.Server, enableAll)
	if err != nil {
		return fmt.Errorf("failed to disable restricted cosmetics: %w", err)
	}

	// Set the user's unlocked cosmetics based on their groups
	unlocked := profile.Server.UnlockedCosmetics
	arena := &unlocked.Arena

	// Unlock VRML cosmetics
	for k, v := range wallet {
		if v <= 0 || !strings.HasPrefix(k, "VRML ") {
			continue
		}

		arena.DecalVRML = true
		arena.EmoteVRMLA = true

		switch k {
		case "VRML Season Preseason":
			arena.TagVRMLPreseason = true
			arena.MedalVRMLPreseason = true

		case "VRML Season 1 Champion":
			arena.MedalVRMLS1Champion = true
			arena.TagVRMLS1Champion = true
			fallthrough
		case "VRML Season 1 Finalist":
			arena.MedalVRMLS1Finalist = true
			arena.TagVRMLS1Finalist = true
			fallthrough
		case "VRML Season 1":
			arena.TagVRMLS1 = true
			arena.MedalVRMLS1 = true

		case "VRML Season 2 Champion":
			arena.MedalVRMLS2Champion = true
			arena.TagVRMLS2Champion = true
			fallthrough
		case "VRML Season 2 Finalist":
			arena.MedalVRMLS2Finalist = true
			arena.TagVRMLS2Finalist = true
			fallthrough
		case "VRML Season 2":
			arena.TagVRMLS2 = true
			arena.MedalVRMLS2 = true

		case "VRML Season 3 Champion":
			arena.MedalVRMLS3Champion = true
			arena.TagVRMLS3Champion = true
			fallthrough
		case "VRML Season 3 Finalist":
			arena.MedalVRMLS3Finalist = true
			arena.TagVRMLS3Finalist = true
			fallthrough
		case "VRML Season 3":
			arena.MedalVRMLS3 = true
			arena.TagVRMLS3 = true

		case "VRML Season 4 Champion":
			arena.TagVRMLS4Champion = true
			arena.MedalVRMLS4Champion = true
			fallthrough
		case "VRML Season 4 Finalist":
			arena.TagVRMLS4Finalist = true
			arena.MedalVRMLS4Finalist = true
			fallthrough
		case "VRML Season 4":
			arena.TagVRMLS4 = true
			arena.MedalVRMLS4 = true
		case "VRML Season 5 Champion":
			arena.TagVRMLS5Champion = true
			fallthrough
		case "VRML Season 5 Finalist":
			arena.TagVRMLS5Finalist = true
			fallthrough
		case "VRML Season 5":
			arena.TagVRMLS5 = true

		case "VRML Season 6 Champion":
			arena.TagVRMLS6Champion = true
			fallthrough

		case "VRML Season 6 Finalist":
			arena.TagVRMLS6Finalist = true
			fallthrough
		case "VRML Season 6":
			arena.TagVRMLS6 = true

		case "VRML Season 7 Champion":
			arena.TagVRMLS7Champion = true
			fallthrough
		case "VRML Season 7 Finalist":
			arena.TagVRMLS7Finalist = true
			fallthrough
		case "VRML Season 7":
			arena.TagVRMLS7 = true
		}
	}

	// Set the user's unlocked cosmetics based on their groups
	for _, userGroup := range userGroups {
		group := userGroup.GetGroup()

		if group.LangTag != "entitlement" {
			continue
		}

		name := group.GetName()

		switch name {
		case GroupGlobalDevelopers:
			arena.TagDeveloper = true
			fallthrough
		case GroupGlobalModerators:
			arena.TagGameAdmin = true

		case GroupGlobalTesters:
			arena.DecalOneYearA = true
			arena.RWDEmoteGhost = true
		}
	}

	// Unlock if the user has been a quest user.
	if strings.Contains(profile.Login.SystemInfo.HeadsetType, "Quest") {
		arena.DecalQuestLaunchA = true
		arena.PatternQuestA = true
	}

	// Update the unlocks (and the client profile's newunlocks list)
	err = profile.UpdateUnlocks(unlocked)
	if err != nil {
		return fmt.Errorf("failed to update unlocks: %w", err)
	}

	md, err := GetAccountMetadata(ctx, r.nk, userID.String())
	if err != nil {
		return fmt.Errorf("failed to unmarshal metadata: %w", err)
	}

	profile.DisableAFKTimeout(md.DisableAFKTimeout)

	/*
		if err := enforceLoadoutEntitlements(r.logger, &profile.Server.EquippedCosmetics.Instances.Unified.Slots, &profile.Server.UnlockedCosmetics, r.defaults); err != nil {
			return fmt.Errorf("failed to set loadout entitlement: %w", err)
		}
	*/
	return nil
}

func enforceLoadoutEntitlements(logger runtime.Logger, loadout *evr.CosmeticLoadout, unlocked *evr.UnlockedCosmetics, defaults map[string]string) error {
	unlockMap := unlocked.ToMap()

	loadoutMap := loadout.ToMap()

	for k, v := range loadoutMap {
		for _, unlocks := range unlockMap {
			if _, found := unlocks[v]; !found {
				logger.Warn("User has item equip that does not exist: %s: %s", k, v)
				loadoutMap[k] = defaults[k]
			} else if !unlocks[v] {
				logger.Warn("User does not have entitlement to item: %s: %s", k, v)
			}
		}
	}
	loadout.FromMap(loadoutMap)
	return nil
}

func createUnlocksFieldByKey() map[string]string {
	unlocks := make(map[string]string)
	types := []interface{}{evr.ArenaUnlocks{}, evr.CombatUnlocks{}}
	for _, t := range types {
		for i := 0; i < reflect.TypeOf(t).NumField(); i++ {
			field := reflect.TypeOf(t).Field(i)
			tag := field.Tag.Get("json")
			name := strings.SplitN(tag, ",", 2)[0]
			unlocks[name] = field.Name
		}
	}
	return unlocks
}

// SetCosmeticDefaults sets all the restricted cosmetics to false.
func SetCosmeticDefaults(s *evr.ServerProfile, enableAll bool) error {
	// Set all the VRML cosmetics to false

	structs := []interface{}{&s.UnlockedCosmetics.Arena, &s.UnlockedCosmetics.Combat}
	for _, t := range structs {
		v := reflect.ValueOf(t)
		if v.Kind() == reflect.Ptr {
			v = v.Elem()
		}

		for i := 0; i < v.NumField(); i++ {
			if enableAll {
				v.Field(i).Set(reflect.ValueOf(true))
			} else {
				tag := v.Type().Field(i).Tag.Get("validate")
				disabled := strings.Contains(tag, "restricted") || strings.Contains(tag, "blocked")
				if v.Field(i).CanSet() {
					v.Field(i).Set(reflect.ValueOf(!disabled))
				}
			}
		}
	}
	return nil
}
