package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/heroiclabs/nakama/v3/server/evr"
	"github.com/intinig/go-openskill/rating"
	"github.com/intinig/go-openskill/types"
	"go.uber.org/thriftrw/ptr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type (
	RatedMatch []RatedEntryTeam
)

type RatedEntry struct {
	Entry  *MatchmakerEntry
	Rating types.Rating
}

func NewRatedEntryFromMatchmakerEntry(e runtime.MatchmakerEntry) *RatedEntry {
	props := e.GetProperties()
	mu, ok := props["rating_mu"].(float64)
	if !ok {
		mu = 25.0
	}
	sigma, ok := props["rating_sigma"].(float64)
	if !ok {
		sigma = 8.333
	}
	return &RatedEntry{
		Entry: e.(*MatchmakerEntry),
		Rating: rating.NewWithOptions(&types.OpenSkillOptions{
			Mu:    ptr.Float64(mu),
			Sigma: ptr.Float64(sigma),
		}),
	}
}

type RatedEntryTeam []*RatedEntry

func (t RatedEntryTeam) Len() int {
	return len(t)
}

func (t RatedEntryTeam) Strength() float64 {
	s := 0.0
	for _, p := range t {
		s += p.Rating.Mu
	}
	return s
}

type RatedTeam []types.Rating

func (t RatedTeam) Strength() float64 {
	s := 0.0
	for _, p := range t {
		s += p.Mu
	}
	return s
}

func (t RatedTeam) Rating() types.Rating {
	if len(t) == 0 {
		return NewDefaultRating()
	}
	meanMu := t.Strength() / float64(len(t))
	sumSigmaSquared := 0.0
	for _, p := range t {
		sumSigmaSquared += p.Sigma * p.Sigma
	}
	averageSigmaSquared := sumSigmaSquared / float64(len(t))
	rmsSigma := math.Sqrt(averageSigmaSquared)

	return rating.NewWithOptions(&types.OpenSkillOptions{
		Mu:    ptr.Float64(meanMu),
		Sigma: ptr.Float64(rmsSigma),
	})
}

func (t RatedTeam) Ordinal() float64 {
	return rating.Ordinal(t.Rating())
}

type PredictedMatch struct {
	Team1 RatedEntryTeam `json:"team1"`
	Team2 RatedEntryTeam `json:"team2"`
	Draw  float64        `json:"draw"`
}

func (p PredictedMatch) Entrants() RatedEntryTeam {
	return append(p.Team1, p.Team2...)
}

func (p PredictedMatch) Teams() []RatedEntryTeam {
	return []RatedEntryTeam{p.Team1, p.Team2}
}

func NewDefaultRating() types.Rating {
	return rating.NewWithOptions(&types.OpenSkillOptions{
		Mu:    ptr.Float64(25.0),
		Sigma: ptr.Float64(8.333),
	})
}

func CalculateNewPlayerRating(xpID evr.XPID, players []PlayerInfo, teamSize int, blueWins bool) (types.Rating, error) {

	// copy the players slice so as to not modify the original
	players = players[:]

	for i := 0; i < len(players); i++ {

		// Remove players that are not on blue/orange
		if !players[i].IsCompetitor() {
			players = append(players[:i], players[i+1:]...)
			i--
			continue
		}

		// Move the target player to the front of the list
		if players[i].XPID == xpID {
			// Move the player to the front of the list
			players[0], players[i] = players[i], players[0]
		}
	}

	if len(players) == 0 || players[0].XPID != xpID {
		return NewDefaultRating(), fmt.Errorf("player not found in players list")
	}

	// Sort the roster by team
	teams := make(map[TeamIndex]types.Team, 2)
	for _, p := range players {
		teams[p.Team] = append(teams[p.Team], p.Rating())
	}

	// Pad the teams to the team size
	for i := range teams {
		for j := len(teams[i]); j < 4; j++ {
			teams[i] = append(teams[i], NewDefaultRating())
		}
	}

	// Swap the teams if the orangeTeam won
	if blueWins {
		new := rating.Rate([]types.Team{teams[BlueTeam], teams[OrangeTeam]}, nil)
		teams[BlueTeam] = new[0]
		teams[OrangeTeam] = new[1]
	} else {
		new := rating.Rate([]types.Team{teams[OrangeTeam], teams[BlueTeam]}, nil)
		teams[OrangeTeam] = new[0]
		teams[BlueTeam] = new[1]
	}

	// Return the new rating for the target player
	return teams[players[0].Team][0], nil
}

func GetRatingByUserID(ctx context.Context, db *sql.DB, userID string, defaultFallback bool) (r types.Rating, err error) {
	// Look for an existing account.
	query := "SELECT value->>'rating' FROM storage WHERE user_id = $1 AND collection = $2 and key = $3"
	var ratingJSON string
	var found = true
	if err = db.QueryRowContext(ctx, query, userID, GameProfileStorageCollection, GameProfileStorageKey).Scan(&ratingJSON); err != nil {
		if err == sql.ErrNoRows {
			found = false
		} else {
			return r, status.Error(codes.Internal, "error finding rating by user ID")
		}
	}
	if !found {
		if defaultFallback {
			return rating.NewWithOptions(&types.OpenSkillOptions{
				Mu:    ptr.Float64(25.0),
				Sigma: ptr.Float64(8.333),
			}), nil
		} else {
			return r, errors.New("rating not found")
		}
	}
	if err = json.Unmarshal([]byte(ratingJSON), &r); err != nil {
		return r, errors.New("error unmarshalling rating")
	}
	return r, nil
}
