package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/intinig/go-openskill/rating"
	"github.com/intinig/go-openskill/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type skillBasedMatchmaker struct{}

var SkillBasedMatchmaker = &skillBasedMatchmaker{}

func (*skillBasedMatchmaker) TeamStrength(team RatedEntryTeam) float64 {
	s := 0.0
	for _, p := range team {
		s += p.Rating.Mu
	}
	return s
}

// Function to be used as a matchmaker function in Nakama (RegisterMatchmakerOverride)
func (m *skillBasedMatchmaker) EvrMatchmakerFn(ctx context.Context, logger runtime.Logger, db *sql.DB, nk runtime.NakamaModule, potentialCandidates [][]runtime.MatchmakerEntry) [][]runtime.MatchmakerEntry {

	candidates := potentialCandidates[:]

	logger.WithFields(map[string]interface{}{
		"num_candidates": len(candidates),
	}).Info("Running skill-based matchmaker.")

	if len(candidates) == 0 || len(candidates[0]) == 0 {
		return nil
	}
	groupID := candidates[0][0].GetProperties()["group_id"].(string)
	if groupID == "" {
		logger.Error("Group ID not found in entry properties.")
	}

	// Store the latest candidates in storage
	data, err := json.Marshal(map[string]interface{}{"candidates": candidates})
	if err != nil {
		logger.WithField("error", err).Error("Error marshalling candidates.")
	} else {
		if _, err := nk.StorageWrite(ctx, []*runtime.StorageWrite{
			{
				UserID:          SystemUserID,
				Collection:      "Matchmaker",
				Key:             "latestCandidates",
				PermissionRead:  0,
				PermissionWrite: 0,
				Value:           string(data),
			},
		}); err != nil {
			logger.WithField("error", err).Error("Error writing latest candidates to storage.")
		}
	}

	if err := nk.StreamSend(StreamModeMatchmaker, groupID, "", "", string(data), nil, false); err != nil {
		logger.WithField("error", err).Warn("Error streaming candidates")
	}

	// If there is a non-hidden presence on the stream, then don't make any matches
	if presences, err := nk.StreamUserList(StreamModeMatchmaker, groupID, "", "", false, true); err != nil {
		logger.WithField("error", err).Warn("Error listing presences on stream.")
	} else if len(presences) > 0 {
		logger.WithField("num_presences", len(presences)).Info("Non-hidden presence on stream, not making matches.")
		return nil
	}

	filterCounts := make(map[string]int)

	// Remove odd sized teams
	filterCounts["odd_size"] = m.removeOddSizedTeams(candidates)

	// Remove duplicate rosters
	filterCounts["duplicates"] = m.removeDuplicateRosters(candidates)

	// Ensure that everyone in the match is within their max_rtt of a common server
	filterCounts["no_matching_servers"] = m.filterWithinMaxRTT(candidates)

	// Create a list of balanced matches with predictions
	predictions := m.buildPredictions(candidates)

	// Sort the matches by their balance
	slices.SortStableFunc(predictions, func(a, b PredictedMatch) int {
		return int((b.Draw - a.Draw) * 1000)
	})

	// Sort by matches that have players who have been waiting more than half the Matchmaking timeout
	// This is to prevent players from waiting too long
	m.sortByPriority(predictions)

	madeMatches := m.composeMatches(predictions)

	logger.WithFields(map[string]interface{}{
		"num_candidates": len(candidates),
		"num_matches":    len(madeMatches),
		"matches":        madeMatches,
		"filter_counts":  filterCounts,
	}).Info("Skill-based matchmaker completed.")

	return madeMatches
}

func (*skillBasedMatchmaker) PredictDraw(teams []RatedEntryTeam) float64 {
	team1 := make(types.Team, 0, len(teams[0]))
	team2 := make(types.Team, 0, len(teams[1]))
	for _, e := range teams[0] {
		team1 = append(team1, e.Rating)
	}
	for _, e := range teams[1] {
		team2 = append(team2, e.Rating)
	}
	return rating.PredictDraw([]types.Team{team1, team2}, nil)
}

func (m *skillBasedMatchmaker) removeOddSizedTeams(candidates [][]runtime.MatchmakerEntry) int {
	oddSizedCount := 0
	for i := 0; i < len(candidates); i++ {
		if len(candidates[i])%2 != 0 {
			oddSizedCount++
			candidates = append(candidates[:i], candidates[i+1:]...)
			i--
		}
	}
	return oddSizedCount
}

// Sort the matches by the players that have been waiting for more than 2/3s of the matchmaking timeout
func (m skillBasedMatchmaker) sortByPriority(predictions []PredictedMatch) {
	now := time.Now().UTC().Unix()

	// This is to prevent players from waiting too long
	slices.SortStableFunc(predictions, func(a, b PredictedMatch) int {
		aPriority := false
		bPriority := false
		for _, e := range a.Entrants() {
			if ts, ok := e.Entry.GetProperties()["priority_threshold"].(float64); ok && int64(ts) < now {
				aPriority = true
				break
			}
		}
		for _, e := range b.Entrants() {
			if ts, ok := e.Entry.GetProperties()["priority_threshold"].(float64); ok && int64(ts) < now {
				bPriority = true
				break
			}
		}

		if aPriority && !bPriority {
			return -1
		} else if !aPriority && bPriority {
			return 1
		} else {
			return 0
		}
	})
}

func (m *skillBasedMatchmaker) CreateBalancedMatch(groups [][]*RatedEntry, teamSize int) (RatedEntryTeam, RatedEntryTeam) {
	// Split out the solo players
	solos := make([]*RatedEntry, 0, len(groups))
	parties := make([][]*RatedEntry, 0, len(groups))

	for _, group := range groups {
		if len(group) == 1 {
			solos = append(solos, group[0])
		} else {
			parties = append(parties, group)
		}
	}

	team1 := make(RatedEntryTeam, 0, teamSize)
	team2 := make(RatedEntryTeam, 0, teamSize)

	// Sort parties into teams by strength
	for _, party := range parties {
		if len(team1)+len(party) <= teamSize && (len(team2)+len(party) > teamSize || m.TeamStrength(team1) <= m.TeamStrength(team2)) {
			team1 = append(team1, party...)
		} else if len(team2)+len(party) <= teamSize {
			team2 = append(team2, party...)
		}
	}

	// Sort solo players onto teams by strength
	for _, player := range solos {
		if len(team1) < teamSize && (len(team2) >= teamSize || m.TeamStrength(team1) <= m.TeamStrength(team2)) {
			team1 = append(team1, player)
		} else if len(team2) < teamSize {
			team2 = append(team2, player)
		}
	}

	return team1, team2
}

func (m *skillBasedMatchmaker) balanceByTicket(candidate []runtime.MatchmakerEntry) RatedMatch {
	// Group based on ticket

	ticketMap := make(map[string][]*RatedEntry)
	for _, e := range candidate {
		ticketMap[e.GetTicket()] = append(ticketMap[e.GetTicket()], NewRatedEntryFromMatchmakerEntry(e))
	}

	byTicket := make([][]*RatedEntry, 0)
	for _, entries := range ticketMap {
		byTicket = append(byTicket, entries)
	}

	team1, team2 := m.CreateBalancedMatch(byTicket, len(candidate)/2)
	return RatedMatch{team1, team2}
}

func (m *skillBasedMatchmaker) removeDuplicateRosters(candidates [][]runtime.MatchmakerEntry) int {
	seenRosters := make(map[string]struct{})

	duplicates := 0
	for i := 0; i < len(candidates); i++ {
		roster := make([]string, 0, len(candidates[i]))
		for _, e := range candidates[i] {
			roster = append(roster, e.GetPresence().GetSessionId())
		}
		slices.Sort(roster)
		rosterString := strings.Join(roster, ",")

		if _, ok := seenRosters[rosterString]; ok {
			duplicates++
			candidates = append(candidates[:i], candidates[i+1:]...)
			i--
			continue
		}
		seenRosters[rosterString] = struct{}{}
	}

	return duplicates
}

// Ensure that everyone in the match is within their max_rtt of a common server
func (m *skillBasedMatchmaker) filterWithinMaxRTT(candidates [][]runtime.MatchmakerEntry) int {

	filteredCount := 0
	for i := 0; i < len(candidates); i++ {

		count := 0
		for _, entry := range candidates[i] {

			maxRTT := 500.0
			if rtt, ok := entry.GetProperties()["max_rtt"].(float64); ok && rtt > 0 {
				maxRTT = rtt
			}

			for k, v := range entry.GetProperties() {

				if !strings.HasPrefix(k, "rtt") {
					continue
				}

				if v.(float64) > maxRTT {
					// Server is too far away from this player
					continue
				}

				count++
			}
		}
		if count != len(candidates[i]) {
			// Server is unreachable to one or more players
			candidates = append(candidates[:i], candidates[i+1:]...)
			i--
			filteredCount++
		}
	}
	return filteredCount
}

func (m *skillBasedMatchmaker) eligibleServers(match []runtime.MatchmakerEntry) map[string]int {
	rttsByServer := make(map[string][]int)
	for _, entry := range match {
		props := entry.GetProperties()

		maxRTT := 500.0
		if rtt, ok := props["max_rtt"].(float64); ok && rtt > 0 {
			maxRTT = rtt
		}

		for k, v := range props {

			if !strings.HasPrefix(k, "rtt") {
				continue
			}

			if rtt, ok := v.(float64); ok {
				if rtt > maxRTT {
					// Server is too far away from this player
					continue
				}
				rttsByServer[k] = append(rttsByServer[k], int(rtt))
			}
		}
	}

	averages := make(map[string]int)
	for k, rtts := range rttsByServer {
		if len(rtts) != len(match) {
			// Server is unreachable to one or more players
			continue
		}

		mean := 0
		for _, rtt := range rtts {
			mean += rtt
		}
		averages[k] = mean / len(rtts)
	}

	return averages
}

func (m *skillBasedMatchmaker) buildPredictions(candidates [][]runtime.MatchmakerEntry) []PredictedMatch {
	predictions := make([]PredictedMatch, 0, len(candidates))
	for _, match := range candidates {
		ratedMatch := m.balanceByTicket(match)
		predictions = append(predictions, PredictedMatch{
			Team1: ratedMatch[0],
			Team2: ratedMatch[1],
			Draw:  m.PredictDraw(ratedMatch),
		})
	}
	return predictions
}
func (m *skillBasedMatchmaker) composeMatches(ratedMatches []PredictedMatch) [][]runtime.MatchmakerEntry {
	seen := make(map[string]struct{})
	selected := make([][]runtime.MatchmakerEntry, 0, len(ratedMatches))

OuterLoop:
	for _, p := range ratedMatches {
		// The players are ordered by their team
		match := make([]runtime.MatchmakerEntry, 0, 8)

		// Ensure no player is in more than one match
		for _, e := range p.Entrants() {
			sessionID := e.Entry.GetPresence().GetSessionId()

			if _, ok := seen[sessionID]; ok {
				continue OuterLoop
			}
			seen[sessionID] = struct{}{}
			match = append(match, e.Entry)
		}

		selected = append(selected, match)
	}
	return selected
}

func GetRatingByUserID(ctx context.Context, db *sql.DB, userID string) (rating types.Rating, err error) {
	// Look for an existing account.
	query := "SELECT value->>'rating' FROM storage WHERE user_id = $1 AND collection = $2 and key = $3"
	var ratingJSON string
	var found = true
	if err = db.QueryRowContext(ctx, query, userID, GameProfileStorageCollection, GameProfileStorageKey).Scan(&ratingJSON); err != nil {
		if err == sql.ErrNoRows {
			found = false
		} else {
			return rating, status.Error(codes.Internal, "error finding rating by user ID")
		}
	}
	if !found {
		return rating, status.Error(codes.NotFound, "rating not found")
	}
	if err = json.Unmarshal([]byte(ratingJSON), &rating); err != nil {
		return rating, status.Error(codes.Internal, "error unmarshalling rating")
	}
	return rating, nil
}
