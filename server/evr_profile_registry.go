package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"sync"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/heroiclabs/nakama/v3/server/evr"
	"github.com/intinig/go-openskill/types"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ProfileRegistry is a registry of user evr profiles.
type ProfileRegistry struct {
	ctx         context.Context
	ctxCancelFn context.CancelFunc
	logger      runtime.Logger
	db          *sql.DB
	nk          runtime.NakamaModule

	tracker Tracker
	metrics Metrics

	// Unlocks by item name
	unlocksByItemName map[string]string

	cacheMu sync.RWMutex
	cache   map[evr.XPID]*json.RawMessage
	// Load out default items
	defaults map[string]string
}

func NewProfileRegistry(nk runtime.NakamaModule, db *sql.DB, logger runtime.Logger, tracker Tracker, metrics Metrics) *ProfileRegistry {
	ctx, cancel := context.WithCancel(context.Background())

	unlocksByFieldName := createUnlocksFieldByKey()

	profileRegistry := &ProfileRegistry{
		ctx:         ctx,
		ctxCancelFn: cancel,
		logger:      logger,
		db:          db,
		nk:          nk,
		tracker:     tracker,
		metrics:     metrics,

		cache: make(map[evr.XPID]*json.RawMessage),

		unlocksByItemName: unlocksByFieldName,
		defaults:          generateDefaultLoadoutMap(),
	}

	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				return
			case <-ticker.C:
				profileRegistry.cacheMu.Lock()
				for evrId := range profileRegistry.cache {
					if tracker.CountByStream(PresenceStream{
						Mode:    StreamModeService,
						Subject: evrId.UUID(),
						Label:   StreamLabelMatchService,
					}) == 0 {
						delete(profileRegistry.cache, evrId)
					}
				}
				profileRegistry.cacheMu.Unlock()
			}
		}
	}()

	return profileRegistry
}

func (r *ProfileRegistry) Stop() {
	select {
	case <-r.ctx.Done():
		return
	default:
		r.ctxCancelFn()
	}
}

// Load the user's profile from memory (or storage if not found)
func (r *ProfileRegistry) Load(ctx context.Context, userID uuid.UUID) (profile *GameProfileData, err error) {
	objs, err := r.nk.StorageRead(ctx, []*runtime.StorageRead{
		{
			Collection: GameProfileStorageCollection,
			Key:        GameProfileStorageKey,
			UserID:     userID.String(),
		},
	})
	if err != nil {
		return nil, err
	}
	profile = &GameProfileData{}
	if len(objs) == 0 {
		profile.Client = evr.NewClientProfile()
		profile.Server = evr.NewServerProfile()
		if err := r.Save(ctx, userID, profile); err != nil {
			return nil, err
		}
		r.metrics.CustomCounter("profile_created", nil, 1)
		return profile, nil
	}

	if err = json.Unmarshal([]byte(objs[0].GetValue()), profile); err != nil {
		return nil, err
	}
	profile.Stale = false

	// Load the user's leaderboard statistics

	return profile, nil
}
func (r *ProfileRegistry) Save(ctx context.Context, userID uuid.UUID, profile GameProfile) error {
	if !profile.IsStale() {
		return nil
	}

	data, err := json.Marshal(profile)
	if err != nil {
		return err
	}
	_, err = r.nk.StorageWrite(ctx, []*runtime.StorageWrite{
		{
			Collection: GameProfileStorageCollection,
			Key:        GameProfileStorageKey,
			UserID:     userID.String(),
			Value:      string(data),
			Version:    profile.GetVersion(),
		},
	})
	if err != nil {
		return err
	}

	// Purge the cache
	r.cacheMu.Lock()
	delete(r.cache, profile.GetXPID())
	r.cacheMu.Unlock()

	return err
}
func (r *ProfileRegistry) SaveAndCache(ctx context.Context, userID uuid.UUID, profile GameProfile) error {
	var err error
	if err := r.Save(ctx, userID, profile); err != nil {
		return err
	}

	serverProfile := profile.GetServer()

	// Extract the "interesting" fields from the server profile
	/*

		{
		    "unlocksetids": {
		        "all": {}
		    },
		    "statgroupids": {
		        "arena": {},
		        "arena_practice_ai": {},
		        "arena_public_ai": {},
		        "combat": {},
		        "daily_2024_11_15": {},
		        "weekly_2024_11_11": {},
		        "active_battle_pass_se": {}
		    }
		}

	*/
	var data json.RawMessage
	data, err = json.Marshal(serverProfile)
	if err != nil {
		return err
	}

	r.cacheMu.Lock()
	r.cache[serverProfile.XPID] = &data
	defer r.cacheMu.Unlock()

	return err
}

// Retrieves the bytes of a server profile from the cache.
func (s *ProfileRegistry) GetCached(ctx context.Context, xpID evr.XPID) (*json.RawMessage, error) {
	s.cacheMu.RLock()
	if data, ok := s.cache[xpID]; ok {
		s.cacheMu.RUnlock()
		return data, nil
	}
	s.cacheMu.RUnlock()

	_, data, err := StorageReadEVRProfileByXPI(ctx, s.db, xpID)
	if err != nil {
		return nil, err
	}
	s.cacheMu.Lock()
	s.cache[xpID] = &data
	s.cacheMu.Unlock()

	return &data, err
}

func (r *ProfileRegistry) NewGameProfile() GameProfileData {

	profile := GameProfileData{
		Client:  evr.NewClientProfile(),
		Server:  evr.NewServerProfile(),
		Ratings: make(map[uuid.UUID]map[evr.Symbol]types.Rating),
		EarlyQuits: EarlyQuitStatistics{
			History: make(map[int64]bool),
		},
	}

	// Apply a community "designed" loadout to the new user
	loadout, err := r.retrieveStarterLoadout(r.ctx)
	if err != nil {
		r.logger.Warn("Failed to retrieve starter loadout: %s", err.Error())
		profile.Server.EquippedCosmetics.Instances.Unified.Slots = loadout
	}
	return profile
}

func (r *ProfileRegistry) retrieveStarterLoadout(ctx context.Context) (evr.CosmeticLoadout, error) {
	defaultLoadout := evr.DefaultCosmeticLoadout()
	// Retrieve a random starter loadout from storage
	ids, _, err := r.nk.StorageList(ctx, uuid.Nil.String(), uuid.Nil.String(), CosmeticLoadoutCollection, 100, "")
	if err != nil {
		return defaultLoadout, fmt.Errorf("failed to list objects: %w", err)
	}
	if len(ids) == 0 {
		return defaultLoadout, nil
	}
	// Pick a random id
	obj := ids[rand.Intn(len(ids))]
	loadout := StoredCosmeticLoadout{}
	if err = json.Unmarshal([]byte(obj.Value), &loadout); err != nil {
		return defaultLoadout, fmt.Errorf("error unmarshalling client profile: %w", err)
	}
	return loadout.Loadout, nil
}

type StoredCosmeticLoadout struct {
	LoadoutID string              `json:"loadout_id"`
	Loadout   evr.CosmeticLoadout `json:"loadout"`
	UserID    string              `json:"user_id"` // the creator
}

func (r *ProfileRegistry) ValidateArenaUnlockByName(i interface{}, itemName string) (bool, error) {
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

func (r *ProfileRegistry) GameProfile(ctx context.Context, logger *zap.Logger, userID uuid.UUID, loginProfile evr.LoginProfile, xpID evr.XPID) (*GameProfileData, error) {
	logger = logger.With(zap.String("xp_id", xpID.String()))

	p, err := r.Load(ctx, userID)
	if err != nil {
		return p, fmt.Errorf("failed to load user profile: %w", err)
	}
	p.SetXPID(xpID)
	p.SetLogin(loginProfile)
	p.Server.PublisherLock = p.Login.PublisherLock
	p.Server.LobbyVersion = p.Login.LobbyVersion

	if p.Server.Statistics == nil || p.Server.Statistics["arena"] == nil || p.Server.Statistics["combat"] == nil {
		p.Server.Statistics = evr.NewStatistics()
	}

	// Apply any unlocks based on the user's groups
	if err := r.UpdateEntitledCosmetics(ctx, userID, p); err != nil {
		return p, fmt.Errorf("failed to update entitled cosmetics: %w", err)
	}

	p.Server.CreateTime = time.Date(2023, 10, 31, 0, 0, 0, 0, time.UTC).Unix()
	p.SetStale()

	r.SaveAndCache(ctx, userID, p)

	return p, nil
}
func (r *ProfileRegistry) UpdateClientProfile(ctx context.Context, logger *zap.Logger, session *sessionWS, update evr.ClientProfile) (err error) {
	// Get the user's profile
	profile, err := r.Load(ctx, session.userID)
	if err != nil {
		return fmt.Errorf("failed to load user profile: %w", err)
	}

	// Validate the client profile.
	// TODO FIXME Validate the profile data
	//if errs := evr.ValidateStruct(request.ClientProfile); errs != nil {
	//	return errFailure(fmt.Errorf("invalid client profile: %s", errs), 400)
	//}

	profile.SetClient(update)

	return r.SaveAndCache(ctx, session.userID, profile)
}

// A fast lookup of existing profile data
func StorageReadEVRProfileByXPI(ctx context.Context, db *sql.DB, xpID evr.XPID) (string, json.RawMessage, error) {
	query := `
	SELECT 
		s.user_id, s.value->>'server' 
	FROM 
		storage s
	WHERE 
		s.collection = $1 AND s.key = $2
		AND 
		s.value->'server'->>'xplatformid' = $3
	ORDER BY s.update_time DESC LIMIT 1;`

	var dbUserID string
	var dbServerProfile string
	var found = true
	if err := db.QueryRowContext(ctx, query, GameProfileStorageCollection, GameProfileStorageKey, xpID.String()).Scan(&dbUserID, &dbServerProfile); err != nil {
		if err == sql.ErrNoRows {
			found = false
		} else {
			return "", nil, status.Error(codes.Internal, "failed to get server profile")
		}
	}
	if !found {
		return "", nil, status.Error(codes.NotFound, "server profile not found")
	}
	return dbUserID, json.RawMessage(dbServerProfile), nil
}

func (p *ProfileRegistry) SetLobbyProfile(ctx context.Context, userID uuid.UUID, xpID evr.XPID, displayName string) error {
	profile, err := p.Load(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to load profile: %w", err)
	}

	profile.SetXPID(xpID)
	profile.UpdateDisplayName(displayName)

	err = p.SaveAndCache(ctx, userID, profile)
	if err != nil {
		return fmt.Errorf("failed to save profile: %w", err)
	}
	return nil
}
