/*
 * Spreed WebRTC.
 * Copyright (C) 2013-2014 struktur AG
 *
 * This file is part of Spreed WebRTC.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package main

import (
	"fmt"
	"log"
	"strings"
	"time"
)

const (
	maxConferenceSize = 100
)

type ChannellingAPI interface {
	OnConnect(Client, *Session)
	OnIncoming(ResponseSender, *Session, *DataIncoming) error
	OnDisconnect(*Session)
}

type channellingAPI struct {
	*Config
	RoomStatusManager
	SessionEncoder
	SessionManager
	StatsCounter
	ContactManager
	TurnDataCreator
	Unicaster
	Broadcaster
	buddyImages ImageCache
}

func NewChannellingAPI(config *Config, roomStatus RoomStatusManager, sessionEncoder SessionEncoder, sessionManager SessionManager, statsCounter StatsCounter, contactManager ContactManager, turnDataCreator TurnDataCreator, unicaster Unicaster, broadcaster Broadcaster, buddyImages ImageCache) ChannellingAPI {
	return &channellingAPI{
		config,
		roomStatus,
		sessionEncoder,
		sessionManager,
		statsCounter,
		contactManager,
		turnDataCreator,
		unicaster,
		broadcaster,
		buddyImages,
	}
}

func (api *channellingAPI) OnConnect(client Client, session *Session) {
	api.Unicaster.OnConnect(client, session)
	if self, err := api.MakeSelf(session); err == nil {
		client.Reply("", self)
	} else {
		log.Println("Error in OnConnect", err)
	}
}

func (api *channellingAPI) OnIncoming(c ResponseSender, session *Session, msg *DataIncoming) error {
	switch msg.Type {
	case "Self":
		self, err := api.MakeSelf(session)
		if err != nil {
			return err
		}
		c.Reply(msg.Iid, self)
	case "Hello":
		//log.Println("Hello", msg.Hello, c.Index())
		// TODO(longsleep): Filter room id and user agent.
		api.UpdateSession(session, &SessionUpdate{Types: []string{"Ua"}, Ua: msg.Hello.Ua})
		if session.Hello && session.Roomid != msg.Hello.Id {
			api.LeaveRoom(session)
			api.Broadcast(session, session.DataSessionLeft("soft"))
		}

		room, err := api.JoinRoom(msg.Hello.Id, msg.Hello.Credentials, session, c)
		if err != nil {
			session.Hello = false
			return err
		}
		session.Hello = true
		session.Roomid = msg.Hello.Id
		if msg.Iid != "" {
			c.Reply(msg.Iid, &DataWelcome{
				Type:  "Welcome",
				Room:  room,
				Users: api.RoomUsers(session),
			})
		}
		api.Broadcast(session, session.DataSessionJoined())
	case "Offer":
		// TODO(longsleep): Validate offer
		api.Unicast(session, msg.Offer.To, msg.Offer)
	case "Candidate":
		// TODO(longsleep): Validate candidate
		api.Unicast(session, msg.Candidate.To, msg.Candidate)
	case "Answer":
		// TODO(longsleep): Validate Answer
		api.Unicast(session, msg.Answer.To, msg.Answer)
	case "Users":
		if session.Hello {
			sessions := &DataSessions{Type: "Users", Users: api.RoomUsers(session)}
			c.Reply(msg.Iid, sessions)
		}
	case "Authentication":
		st := msg.Authentication.Authentication
		if st == nil {
			// TODO(lcooper): Should this return an error?
			return nil
		}

		if err := api.Authenticate(session, st, ""); err != nil {
			return fmt.Errorf("Authentication failed", err, st.Userid, st.Nonce)
		}
		self, err := api.MakeSelf(session)
		if err != nil {
			return err
		}
		log.Println("Authentication success", session.Userid)
		api.BroadcastSessionStatus(session)
		c.Reply(msg.Iid, self)
	case "Bye":
		api.Unicast(session, msg.Bye.To, msg.Bye)
	case "Status":
		//log.Println("Status", msg.Status)
		api.UpdateSession(session, &SessionUpdate{Types: []string{"Status"}, Status: msg.Status.Status})
		api.BroadcastSessionStatus(session)
	case "Chat":
		// TODO(longsleep): Limit sent chat messages per incoming connection.
		if !msg.Chat.Chat.NoEcho {
			api.Unicast(session, session.Id, msg.Chat)
		}
		msg.Chat.Chat.Time = time.Now().Format(time.RFC3339)
		if msg.Chat.To == "" {
			// TODO(longsleep): Check if chat broadcast is allowed.
			if session.Hello {
				api.CountBroadcastChat()
				api.Broadcast(session, msg.Chat)
			}
		} else {
			if msg.Chat.Chat.Status != nil && msg.Chat.Chat.Status.ContactRequest != nil {
				if err := api.contactrequestHandler(session, msg.Chat.To, msg.Chat.Chat.Status.ContactRequest); err != nil {
					return fmt.Errorf("Ignoring invalid contact request. %v", err)
				}
				msg.Chat.Chat.Status.ContactRequest.Userid = session.Userid()
			}
			if msg.Chat.Chat.Status == nil {
				api.CountUnicastChat()
			}

			api.Unicast(session, msg.Chat.To, msg.Chat)
			if msg.Chat.Chat.Mid != "" {
				// Send out delivery confirmation status chat message.
				api.Unicast(session, session.Id, &DataChat{To: msg.Chat.To, Type: "Chat", Chat: &DataChatMessage{Mid: msg.Chat.Chat.Mid, Status: &DataChatStatus{State: "sent"}}})
			}
		}
	case "Conference":
		// Check conference maximum size.
		if len(msg.Conference.Conference) > maxConferenceSize {
			return fmt.Errorf("Refusing to create conference with size %v above limit.", len(msg.Conference.Conference))
		}

		// Send conference update to anyone.
		for _, id := range msg.Conference.Conference {
			if id != session.Id {
				api.Unicast(session, id, msg.Conference)
			}
		}
	case "Alive":
		c.Reply(msg.Iid, msg.Alive)
	case "Sessions":
		var users []*DataSession
		switch msg.Sessions.Sessions.Type {
		case "contact":
			userID, err := api.getContactID(session, msg.Sessions.Sessions.Token)
			if err != nil {
				return err
			}
			users = api.GetUserSessions(session, userID)
		case "session":
			id, err := session.attestation.Decode(msg.Sessions.Sessions.Token)
			if err != nil {
				return fmt.Errorf("Failed to decode incoming attestation %v: %v", msg.Sessions.Sessions.Token, err)
			}
			session, ok := api.GetSession(id)
			if !ok {
				return fmt.Errorf("Cannot retrieve session for id %s", id)
			}
			users = []*DataSession{session.Data()}
		default:
			return fmt.Errorf("Unknown incoming sessions request type %s", msg.Sessions.Sessions.Type)
		}

		// TODO(lcooper): We ought to reply with a *DataError if failed.
		c.Reply(msg.Iid, &DataSessions{Type: "Sessions", Users: users, Sessions: msg.Sessions.Sessions})
	case "Room":
		room, err := api.UpdateRoom(session, msg.Room)
		if err != nil {
			return err
		}
		api.Broadcast(session, room)
		c.Reply(msg.Iid, room)
	default:
		return fmt.Errorf("OnIncoming unhandled message type %s", msg.Type)
	}
	return nil
}

func (api *channellingAPI) OnDisconnect(session *Session) {
	dsl := session.DataSessionLeft("hard")
	if session.Hello {
		api.LeaveRoom(session)
		api.Broadcast(session, dsl)
	}

	session.RunForAllSubscribers(func(session *Session) {
		log.Println("Notifying subscriber that we are gone", session.Id, session.Id)
		api.Unicast(session, session.Id, dsl)
	})

	api.Unicaster.OnDisconnect(session)

	api.buddyImages.Delete(session.Id)
}

func (api *channellingAPI) MakeSelf(session *Session) (*DataSelf, error) {
	token, err := api.EncodeSessionToken(session)
	if err != nil {
		return nil, err
	}
	log.Println("Created new session token", len(token), token)
	return &DataSelf{
		Type:    "Self",
		Id:      session.Id,
		Sid:     session.Sid,
		Userid:  session.Userid(),
		Suserid: api.EncodeSessionUserID(session),
		Token:   token,
		Version: api.Version,
		Turn:    api.CreateTurnData(session),
		Stun:    api.StunURIs,
	}, nil
}

func (api *channellingAPI) UpdateSession(session *Session, s *SessionUpdate) uint64 {
	if s.Status != nil {
		status, ok := s.Status.(map[string]interface{})
		if ok && status["buddyPicture"] != nil {
			pic := status["buddyPicture"].(string)
			if strings.HasPrefix(pic, "data:") {
				imageId := api.buddyImages.Update(session.Id, pic[5:])
				if imageId != "" {
					status["buddyPicture"] = "img:" + imageId
				}
			}
		}
	}

	return session.Update(s)
}

func (api *channellingAPI) BroadcastSessionStatus(session *Session) {
	if session.Hello {
		api.Broadcast(session, session.DataSessionStatus())
	}
}
