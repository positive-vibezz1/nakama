package server

import (
	"testing"

	"github.com/heroiclabs/nakama/v3/server/evr"
	"github.com/stretchr/testify/assert"
)

func TestGenerateLinkTicket(t *testing.T) {
	xpID, _ := evr.XPIDFromString("OVR-ORG-12345")

	linkTickets := make(map[string]*LinkTicket)

	loginData := &evr.LoginProfile{
		// Populate with necessary fields
	}

	ticket := generateLinkTicket(linkTickets, xpID, "127.0.0.1", loginData)

	assert.NotNil(t, ticket, "Expected a non-nil link ticket")
	assert.Equal(t, loginData, ticket.LoginProfile, "Expected LoginRequest to match")
	assert.Contains(t, linkTickets, ticket.Code, "Expected linkTickets to contain the generated code")
}

func TestGenerateLinkTicketWithExistingToken(t *testing.T) {

	xpID, _ := evr.XPIDFromString("OVR-ORG-12345")
	linkTickets := map[string]*LinkTicket{
		"existing-code": {
			Code:         "existing-code",
			XPID:         xpID,
			ClientIP:     "127.0.0.1",
			LoginProfile: &evr.LoginProfile{},
		},
	}

	loginData := &evr.LoginProfile{}

	ticket := generateLinkTicket(linkTickets, xpID, "127.0.0.1", loginData)

	assert.NotNil(t, ticket, "Expected a non-nil link ticket")

	assert.Equal(t, loginData, ticket.LoginProfile, "Expected LoginRequest to match")
	assert.Contains(t, linkTickets, ticket.Code, "Expected linkTickets to contain the generated code")
	assert.NotEqual(t, "existing-code", ticket.Code, "Expected a new code to be generated")
}
