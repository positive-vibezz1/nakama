package server

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"fmt"

	"github.com/bwmarrin/discordgo"
	"github.com/gofrs/uuid/v5"
	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/heroiclabs/nakama/v3/server/evr"
	"github.com/samber/lo"
	"go.uber.org/zap"
)

// sendDiscordError sends an error message to the user on discord
func sendDiscordError(e error, discordId string, logger *zap.Logger, bot *discordgo.Session) {
	// Message the user on discord

	if bot != nil && discordId != "" {
		channel, err := bot.UserChannelCreate(discordId)
		if err != nil {
			logger.Warn("Failed to create user channel", zap.Error(err))
		}
		_, err = bot.ChannelMessageSend(channel.ID, fmt.Sprintf("Failed to register game server: %v", e))
		if err != nil {
			logger.Warn("Failed to send message to user", zap.Error(err))
		}
	}
}

// errFailedRegistration sends a failure message to the broadcaster and closes the session
func errFailedRegistration(session *sessionWS, logger *zap.Logger, err error, code evr.BroadcasterRegistrationFailureCode) error {
	logger.Warn("Failed to register game server", zap.Error(err))
	if err := session.SendEvrUnrequire(evr.NewBroadcasterRegistrationFailure(code)); err != nil {
		return fmt.Errorf("failed to send lobby registration failure: %w", err)
	}

	session.Close(err.Error(), runtime.PresenceReasonDisconnect)
	return fmt.Errorf("failed to register game server: %w", err)
}

func (p *EvrPipeline) broadcasterRegistrationRequest(ctx context.Context, logger *zap.Logger, session *sessionWS, in evr.Message) error {
	request := in.(*evr.BroadcasterRegistrationRequest)
	return p.gameServerRegistration(ctx, logger, session, request.ServerId, request.InternalIP, request.Port, request.Region, request.VersionLock, 0)
}

func (p *EvrPipeline) echoToolsGameServerRegistrationRequestV1(ctx context.Context, logger *zap.Logger, session *sessionWS, in evr.Message) error {
	request := in.(*evr.EchoToolsGameServerRegistrationRequestV1)

	return p.gameServerRegistration(ctx, logger, session, request.ServerId, request.InternalIP, request.Port, request.Region, request.VersionLock, request.TimeStepUsecs)
}

func (p *EvrPipeline) gameServerRegistration(ctx context.Context, logger *zap.Logger, session *sessionWS, serverID uint64, internalIP net.IP, listenPort uint16, region evr.Symbol, versionLock uint64, timeStepUsecs uint32) error {

	// broadcasterRegistrationRequest is called when the broadcaster has sent a registration request.

	discordId := ""

	if session.userID.IsNil() {
		return errFailedRegistration(session, logger, errors.New("game servers must authenticate with discordID/password auth"), evr.BroadcasterRegistration_Unknown)
	}

	sessionParams, ok := LoadParams(ctx)
	if !ok {
		return fmt.Errorf("session parameters not provided")
	}

	newParams := *sessionParams

	// Get a list of the user's guild memberships and set to the largest one
	memberships, err := GetGuildGroupMemberships(ctx, p.runtimeModule, session.UserID().String())
	if err != nil {
		return fmt.Errorf("failed to get guild groups: %w", err)
	}
	if len(memberships) == 0 {
		return fmt.Errorf("user is not in any guild groups")
	}

	newParams.Memberships = memberships

	// Get the guilds that the broadcaster wants to host for
	groupIDs := make([]string, 0)
	for _, guildID := range sessionParams.ServerGuilds {
		if guildID == "" {
			continue
		}
		groupID := p.discordCache.GuildIDToGroupID(guildID)
		if groupID == "" {
			continue
		}

		// Any will allow the broadcaster to host on any server they are a member of
		if groupID == "any" {
			groupIDs = make([]string, 0)
			break
		}

		if m, ok := memberships[groupID]; ok && m.IsServerHost {
			groupIDs = append(groupIDs, groupID)
		}
	}

	if len(groupIDs) == 0 {
		for groupID, m := range memberships {
			if m.IsServerHost {
				groupIDs = append(groupIDs, groupID)
			}
		}
	}

	regions := make([]evr.Symbol, 0)
	for _, r := range sessionParams.ServerRegions {
		regions = append(regions, evr.ToSymbol(r))
	}

	if len(regions) == 0 {
		regions = append(regions, evr.DefaultRegion)
	}

	if region != evr.DefaultRegion {
		regions = append(regions, region)
	}

	logger = logger.With(zap.String("discord_id", discordId), zap.Strings("group_ids", groupIDs), zap.Strings("tags", newParams.ServerTags), zap.Strings("regions", lo.Map(regions, func(v evr.Symbol, _ int) string { return v.String() })))

	// Add the server id as a region
	regions = append(regions, evr.ToSymbol(serverID))

	slices.Sort(regions)
	regions = slices.Compact(regions)

	UpdateParams(ctx, &newParams)

	err = session.BroadcasterSession(session.userID, "broadcaster:"+session.Username(), serverID)
	if err != nil {
		return fmt.Errorf("failed to create broadcaster session: %w", err)
	}

	logger = logger.With(zap.String("uid", session.UserID().String()))

	// Set the external address in the request (to use for the registration cache).
	externalIP := net.ParseIP(session.ClientIP())
	externalPort := listenPort

	params, ok := LoadParams(ctx)
	if !ok {
		return errFailedRegistration(session, logger, errors.New("session parameters not provided"), evr.BroadcasterRegistration_Unknown)
	}

	if params.ExternalServerAddr != "" {
		parts := strings.Split(":", params.ExternalServerAddr)
		if len(parts) != 2 {
			return errFailedRegistration(session, logger, fmt.Errorf("invalid external IP address: %s. it must be `ip:port`", params.ExternalServerAddr), evr.BroadcasterRegistration_Unknown)
		}
		externalIP = net.ParseIP(parts[0])
		if externalIP == nil {
			// Try to resolve it as a hostname
			ips, err := net.LookupIP(parts[0])
			if err != nil {
				return errFailedRegistration(session, logger, fmt.Errorf("invalid address `%s`: %v", parts[0], err), evr.BroadcasterRegistration_Unknown)
			}
			externalIP = ips[0]
		}
		if p, err := strconv.Atoi(parts[1]); err != nil {
			return errFailedRegistration(session, logger, fmt.Errorf("invalid external port: %s", parts[1]), evr.BroadcasterRegistration_Unknown)
		} else {
			externalPort = uint16(p)
		}
	}

	if isPrivateIP(externalIP) {
		logger.Warn("Broadcaster is on a private IP, using this systems external IP", zap.String("private_ip", externalIP.String()), zap.String("external_ip", p.externalIP.String()), zap.String("port", fmt.Sprintf("%d", externalPort)))
		externalIP = p.externalIP
	}

	// Create the broadcaster config
	config := broadcasterConfig(session.UserID().String(), session.id.String(), serverID, internalIP, externalIP, externalPort, regions, versionLock, params.ServerTags, params.SupportedFeatures)

	// Add the operators userID to the group ids. this allows any host to spawn on a server they operate.
	groupUUIDs := make([]uuid.UUID, 0, len(groupIDs))
	for _, groupID := range groupIDs {
		groupUUIDs = append(groupUUIDs, uuid.FromStringOrNil(groupID))
	}
	config.GroupIDs = groupUUIDs

	logger = logger.With(zap.String("internal_ip", internalIP.String()), zap.String("external_ip", externalIP.String()), zap.Uint16("port", externalPort))
	// Validate connectivity to the broadcaster.
	// Wait 2 seconds, then check

	time.Sleep(2 * time.Second)

	alive := false

	// Check if the broadcaster is available
	retries := 5
	var rtt time.Duration
	for i := 0; i < retries; i++ {
		rtt, err = BroadcasterHealthcheck(p.localIP, config.Endpoint.ExternalIP, int(config.Endpoint.Port), 500*time.Millisecond)
		if err != nil {
			logger.Warn("Failed to healthcheck broadcaster", zap.Error(err))
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if rtt >= 0 {
			alive = true
			break
		}
	}
	if !alive {
		// If the broadcaster is not available, send an error message to the user on discord
		errorMessage := fmt.Sprintf("Broadcaster (Endpoint ID: %s, Server ID: %d) could not be reached. Error: %v", config.Endpoint.ExternalAddress(), config.ServerID, err)
		go sendDiscordError(errors.New(errorMessage), discordId, logger, p.discordCache.dg)
		return errFailedRegistration(session, logger, errors.New(errorMessage), evr.BroadcasterRegistration_Failure)
	}
	configJson, err := json.Marshal(config)
	if err != nil {
		return errFailedRegistration(session, logger, err, evr.BroadcasterRegistration_Failure)
	}

	if err := p.runtimeModule.StreamUserUpdate(StreamModeGameServer, session.ID().String(), "", StreamLabelGameServerService, session.UserID().String(), session.ID().String(), true, false, string(configJson)); err != nil {
		return errFailedRegistration(session, logger, err, evr.BroadcasterRegistration_Failure)
	}

	p.broadcasterRegistrationBySession.Store(session.ID().String(), config)
	go func() {
		<-session.Context().Done()
		p.broadcasterRegistrationBySession.Delete(session.ID().String())
	}()

	// Create a new parking match
	if err := p.newParkingMatch(logger, session, config); err != nil {
		return errFailedRegistration(session, logger, err, evr.BroadcasterRegistration_Failure)
	}
	// Send the registration success message
	return session.SendEvrUnrequire(evr.NewBroadcasterRegistrationSuccess(config.ServerID, config.Endpoint.ExternalIP))
}

func broadcasterConfig(userId, sessionId string, serverId uint64, internalIP, externalIP net.IP, port uint16, regions []evr.Symbol, versionLock uint64, tags, features []string) *MatchBroadcaster {

	config := &MatchBroadcaster{
		SessionID:  sessionId,
		OperatorID: userId,
		ServerID:   serverId,
		Endpoint: evr.Endpoint{
			InternalIP: internalIP,
			ExternalIP: externalIP,
			Port:       port,
		},
		Regions:     regions,
		VersionLock: evr.ToSymbol(versionLock),
		GroupIDs:    make([]uuid.UUID, 0),
		Features:    features,

		Tags: make([]string, 0),
	}

	if len(tags) > 0 {
		config.Tags = tags
	}
	return config
}

func (p *EvrPipeline) newParkingMatch(logger *zap.Logger, session *sessionWS, config *MatchBroadcaster) error {
	data, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal game server config: %w", err)
	}

	params := map[string]interface{}{
		"gameserver": string(data),
	}

	// Create the match
	matchIDStr, err := p.matchRegistry.CreateMatch(context.Background(), p.runtime.matchCreateFunction, EvrMatchmakerModule, params)
	if err != nil {
		return fmt.Errorf("failed to create parking match: %w", err)
	}

	matchID := MatchIDFromStringOrNil(matchIDStr)

	if err := UpdateGameServerBySessionID(p.runtimeModule, session.userID, session.id, matchID); err != nil {
		return fmt.Errorf("failed to update game server by session ID: %w", err)
	}

	found, allowed, _, reason, _, _ := p.matchRegistry.JoinAttempt(session.Context(), matchID.UUID, matchID.Node, session.UserID(), session.ID(), session.Username(), session.Expiry(), session.Vars(), session.ClientIP(), session.ClientPort(), p.node, nil)
	if !found {
		return fmt.Errorf("match not found: %s", matchID.String())
	}
	if !allowed {
		return fmt.Errorf("join not allowed: %s", reason)
	}

	// Trigger the MatchJoin event.
	stream := PresenceStream{Mode: StreamModeMatchAuthoritative, Subject: matchID.UUID, Label: matchID.Node}
	m := PresenceMeta{
		Username: session.Username(),
		Format:   session.Format(),
	}
	if success, _ := p.tracker.Track(session.Context(), session.ID(), stream, session.UserID(), m); success {
		// Kick the user from any other matches they may be part of.
		// WARNING This cannot be used during transition. It will kick the player from their current match.
		//p.tracker.UntrackLocalByModes(session.ID(), matchStreamModes, stream)
	}

	logger.Debug("New parking match", zap.String("mid", matchIDStr))

	return nil
}

func BroadcasterHealthcheck(localIP net.IP, remoteIP net.IP, port int, timeout time.Duration) (rtt time.Duration, err error) {
	const (
		pingRequestSymbol               uint64 = 0x997279DE065A03B0
		RawPingAcknowledgeMessageSymbol uint64 = 0x4F7AE556E0B77891
	)

	laddr := &net.UDPAddr{
		IP:   localIP,
		Port: 0,
	}

	raddr := &net.UDPAddr{
		IP:   remoteIP,
		Port: int(port),
	}
	// Establish a UDP connection to the specified address
	conn, err := net.DialUDP("udp", laddr, raddr)
	if err != nil {
		return 0, fmt.Errorf("could not establish connection to %v: %v", raddr, err)
	}
	defer conn.Close() // Ensure the connection is closed when the function ends

	// Set a deadline for the connection to prevent hanging indefinitely
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return 0, fmt.Errorf("could not set deadline for connection to %v: %v", raddr, err)
	}

	// Generate a random 8-byte number for the ping request
	token := make([]byte, 8)
	if _, err := rand.Read(token); err != nil {
		return 0, fmt.Errorf("could not generate random number for ping request: %w", err)
	}

	// Construct the raw ping request message
	request := make([]byte, 16)
	response := make([]byte, 16)
	binary.LittleEndian.PutUint64(request[:8], pingRequestSymbol) // Add the ping request symbol
	copy(request[8:], token)                                      // Add the random number

	// Start the timer
	start := time.Now()

	// Send the ping request to the broadcaster
	if _, err := conn.Write(request); err != nil {
		return 0, fmt.Errorf("could not send ping request to %v: %v", raddr, err)
	}

	// Read the response from the broadcaster
	if _, err := conn.Read(response); err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			return -1, fmt.Errorf("ping request to %v timed out (>%dms)", raddr, timeout.Milliseconds())
		}
		return 0, fmt.Errorf("could not read ping response from %v: %v", raddr, err)
	}
	// Calculate the round trip time
	rtt = time.Since(start)

	// Check if the response's symbol matches the expected acknowledge symbol
	if binary.LittleEndian.Uint64(response[:8]) != RawPingAcknowledgeMessageSymbol {
		return 0, fmt.Errorf("received unexpected response from %v: %v", raddr, response)
	}

	// Check if the response's number matches the sent number, indicating a successful ping
	if ok := binary.LittleEndian.Uint64(response[8:]) == binary.LittleEndian.Uint64(token); !ok {
		return 0, fmt.Errorf("received unexpected response from %v: %v", raddr, response)
	}

	return rtt, nil
}

func BroadcasterPortScan(lIP net.IP, rIP net.IP, startPort, endPort int, timeout time.Duration) (map[int]time.Duration, []error) {

	// Prepare slices to store results
	rtts := make(map[int]time.Duration, endPort-startPort+1)
	errs := make([]error, endPort-startPort+1)

	var wg sync.WaitGroup
	var mu sync.Mutex // Mutex to avoid race condition
	for port := startPort; port <= endPort; port++ {
		wg.Add(1)
		go func(port int) {
			defer wg.Done()

			rtt, err := BroadcasterHealthcheck(lIP, rIP, port, timeout)
			mu.Lock()
			if err != nil {
				errs[port-startPort] = err
			} else {
				rtts[port] = rtt
			}
			mu.Unlock()
		}(port)
	}
	wg.Wait()

	return rtts, errs
}

func BroadcasterRTTcheck(lIP net.IP, rIP net.IP, port, count int, interval, timeout time.Duration) (rtts []time.Duration, err error) {
	// Create a slice to store round trip times (rtts)
	rtts = make([]time.Duration, count)
	// Set a timeout duration

	// Create a WaitGroup to manage concurrency
	var wg sync.WaitGroup
	// Add a task to the WaitGroup
	wg.Add(1)

	// Start a goroutine
	go func() {
		// Ensure the task is marked done on return
		defer wg.Done()
		// Loop 5 times
		for i := 0; i < count; i++ {
			// Perform a health check on the broadcaster
			rtt, err := BroadcasterHealthcheck(lIP, rIP, port, timeout)
			if err != nil {
				// If there's an error, set the rtt to -1
				rtts[i] = -1
				continue
			}
			// Record the rtt
			rtts[i] = rtt
			// Sleep for the duration of the ping interval before the next iteration
			time.Sleep(interval)
		}
	}()

	wg.Wait()
	return rtts, nil
}

func DetermineLocalIPAddress() (net.IP, error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return nil, err
	}
	return conn.LocalAddr().(*net.UDPAddr).IP, nil
}

func DetermineExternalIPAddress() (net.IP, error) {
	response, err := http.Get("https://api.ipify.org?format=text")
	if err != nil {
		return nil, err
	}

	defer response.Body.Close()

	data, _ := io.ReadAll(response.Body)
	addr := net.ParseIP(string(data))
	if addr == nil {
		return nil, errors.New("failed to parse IP address")
	}
	return addr, nil
}

func isPrivateIP(ip net.IP) bool {
	privateRanges := []string{
		"127.0.0.0/8",
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
	}
	for _, cidr := range privateRanges {
		_, subnet, _ := net.ParseCIDR(cidr)
		if subnet.Contains(ip) {
			return true
		}
	}
	return false
}

func (p *EvrPipeline) broadcasterSessionStarted(_ context.Context, logger *zap.Logger, _ *sessionWS, in evr.Message) error {
	request := in.(*evr.BroadcasterSessionStarted)

	logger.Info("Game session started", zap.String("mid", request.LobbySessionID.String()))

	return nil
}

// broadcasterSessionEnded is called when the broadcaster has ended the session.
func (p *EvrPipeline) broadcasterSessionEnded(ctx context.Context, logger *zap.Logger, session *sessionWS, in evr.Message) error {
	matchID, presence, err := GameServerBySessionID(p.runtimeModule, session.ID())
	if err != nil {
		logger.Warn("Failed to get broadcaster's match by session ID", zap.Error(err))
	}

	if err := p.runtimeModule.StreamUserLeave(StreamModeMatchAuthoritative, matchID.UUID.String(), "", p.node, presence.GetUserId(), presence.GetSessionId()); err != nil {
		logger.Warn("Failed to leave match stream", zap.Error(err))
	}

	go func() {
		select {
		case <-session.Context().Done():
			return
		case <-time.After(10 * time.Second):
		}

		// Get the broadcaster registration from the stream.
		presence, err := p.runtimeModule.StreamUserGet(StreamModeGameServer, session.ID().String(), "", StreamLabelGameServerService, session.UserID().String(), session.ID().String())
		if err != nil {
			logger.Error("Failed to get broadcaster presence", zap.Error(err))
			session.Close("Failed to get broadcaster presence", runtime.PresenceReasonUnknown)
		}
		config := &MatchBroadcaster{}
		if err := json.Unmarshal([]byte(presence.GetStatus()), config); err != nil {
			logger.Error("Failed to unmarshal broadcaster presence", zap.Error(err))
			session.Close("Failed to get broadcaster presence", runtime.PresenceReasonUnknown)
		}

		if err := p.newParkingMatch(logger, session, config); err != nil {
			logger.Error("Failed to create new parking match", zap.Error(err))
			session.Close("Failed to get broadcaster presence", runtime.PresenceReasonUnknown)
		}
	}()
	return nil
}

func (p *EvrPipeline) broadcasterPlayerAccept(ctx context.Context, logger *zap.Logger, session *sessionWS, in evr.Message) error {
	request := in.(*evr.GameServerJoinAttempt)

	matchID, _, err := GameServerBySessionID(p.runtimeModule, session.ID())
	if err != nil {
		logger.Warn("Failed to get broadcaster's match by session ID", zap.Error(err))
	}
	baseLogger := logger.With(zap.String("mid", matchID.String()))

	accepted := make([]uuid.UUID, 0, len(request.EntrantIDs))
	rejected := make([]uuid.UUID, 0)

	for _, entrantID := range request.EntrantIDs {
		logger := baseLogger.With(zap.String("entrant_id", entrantID.String()))

		presence, err := PresenceByEntrantID(p.runtimeModule, matchID, entrantID)
		if err != nil || presence == nil {
			logger.Warn("Failed to get player presence by entrant ID", zap.Error(err))
			rejected = append(rejected, entrantID)
			continue
		}

		logger = logger.With(zap.String("entrant_uid", presence.GetUserId()))

		s := p.sessionRegistry.Get(uuid.FromStringOrNil(presence.GetSessionId()))
		if s == nil {
			logger.Warn("Failed to get session by ID")
			rejected = append(rejected, entrantID)
			continue
		}

		ctx := s.Context()
		for _, subject := range []uuid.UUID{presence.SessionID, presence.UserID, presence.EvrID.UUID()} {
			session.tracker.Update(ctx, s.ID(), PresenceStream{Mode: StreamModeService, Subject: subject, Label: StreamLabelMatchService}, s.UserID(), PresenceMeta{Format: s.Format(), Hidden: true, Status: matchID.String()})
		}

		// Trigger the MatchJoin event.
		stream := PresenceStream{Mode: StreamModeMatchAuthoritative, Subject: matchID.UUID, Label: matchID.Node}
		m := PresenceMeta{
			Username: s.Username(),
			Format:   s.Format(),
			Status:   presence.GetStatus(),
		}

		if success, _ := p.tracker.Track(s.Context(), s.ID(), stream, s.UserID(), m); success {
			// Kick the user from any other matches they may be part of.
			// WARNING This cannot be used during transition. It will kick the player from their current match.
			//p.tracker.UntrackLocalByModes(session.ID(), matchStreamModes, stream)
		}

		accepted = append(accepted, entrantID)
	}

	// Only include the message if there are players to accept or reject.
	messages := []evr.Message{}
	if len(accepted) > 0 {
		messages = append(messages, evr.NewGameServerJoinAllowed(accepted...))
	}

	if len(rejected) > 0 {
		messages = append(messages, evr.NewGameServerEntrantRejected(evr.PlayerRejectionReasonBadRequest, rejected...))
	}

	return session.SendEvr(messages...)
}

// broadcasterPlayerRemoved is called when a player has been removed from the match.
func (p *EvrPipeline) broadcasterPlayerRemoved(ctx context.Context, logger *zap.Logger, session *sessionWS, in evr.Message) error {
	message := in.(*evr.GameServerPlayerRemoved)
	matchID, _, err := GameServerBySessionID(p.runtimeModule, session.ID())
	if err != nil {
		logger.Warn("Failed to get broadcaster's match by session ID", zap.Error(err))
	}

	presence, err := PresenceByEntrantID(p.runtimeModule, matchID, message.EntrantID)
	if err != nil {
		if err != ErrEntrantNotFound {
			logger.Warn("Failed to get player session by ID", zap.Error(err))
		}
		return nil
	}
	if presence != nil {
		// Trigger MatchLeave.
		if err := p.runtimeModule.StreamUserLeave(StreamModeMatchAuthoritative, matchID.UUID.String(), "", matchID.Node, presence.GetUserId(), presence.GetSessionId()); err != nil {
			logger.Warn("Failed to leave match stream", zap.Error(err))
		}

		if err := p.runtimeModule.StreamUserLeave(StreamModeEntrant, message.EntrantID.String(), "", matchID.Node, presence.GetUserId(), presence.GetSessionId()); err != nil {
			logger.Warn("Failed to leave entrant session stream", zap.Error(err))
		}
	}

	return nil
}

func (p *EvrPipeline) broadcasterPlayerSessionsLocked(ctx context.Context, logger *zap.Logger, session *sessionWS, in evr.Message) error {
	matchID, _, err := GameServerBySessionID(p.runtimeModule, session.ID())
	if err != nil {
		logger.Warn("Failed to get broadcaster's match by session ID", zap.Error(err))
	}
	signal := NewSignalEnvelope(session.userID.String(), SignalLockSession, nil)
	if _, err := p.matchRegistry.Signal(ctx, matchID.String(), signal.String()); err != nil {
		logger.Warn("Failed to signal match", zap.Error(err))
	}
	return nil
}

func (p *EvrPipeline) broadcasterPlayerSessionsUnlocked(ctx context.Context, logger *zap.Logger, session *sessionWS, in evr.Message) error {
	matchID, _, err := GameServerBySessionID(p.runtimeModule, session.ID())
	if err != nil {
		logger.Warn("Failed to get broadcaster's match by session ID", zap.Error(err))
	}
	signal := NewSignalEnvelope(session.userID.String(), SignalUnlockSession, nil)
	if _, err := p.matchRegistry.Signal(ctx, matchID.String(), signal.String()); err != nil {
		logger.Warn("Failed to signal match", zap.Error(err))
	}
	return nil
}

func UpdateGameServerBySessionID(nk runtime.NakamaModule, userID uuid.UUID, sessionID uuid.UUID, matchID MatchID) error {
	return nk.StreamUserUpdate(StreamModeGameServer, sessionID.String(), "", StreamLabelMatchService, userID.String(), sessionID.String(), true, false, matchID.String())
}

func GameServerBySessionID(nk runtime.NakamaModule, sessionID uuid.UUID) (MatchID, runtime.Presence, error) {

	presences, err := nk.StreamUserList(StreamModeGameServer, sessionID.String(), "", StreamLabelMatchService, true, true)
	if err != nil {
		return MatchID{}, nil, err
	}

	if len(presences) == 0 {
		return MatchID{}, nil, fmt.Errorf("no game server presence found")
	}

	presence := presences[0]

	matchID, err := MatchIDFromString(presence.GetStatus())
	if err != nil {
		return MatchID{}, nil, err
	}
	return matchID, presence, nil
}