package connector

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"mellium.im/xmpp/jid"

	"github.com/keplersj/mautrix-xmpp/pkg/xmppx"
)

func (c *XMPPClient) GetCapabilities(_ context.Context, portal *bridgev2.Portal) *event.RoomFeatures {
	_, isMUC, _ := parsePortalID(portal.ID)
	c.mu.Lock()
	maxSize := c.main.Config.Media.MaxUploadSize
	if c.upload != nil && c.upload.MaxFileSize > 0 && c.upload.MaxFileSize < maxSize {
		maxSize = c.upload.MaxFileSize
	}
	hasUpload := c.upload != nil
	c.mu.Unlock()
	fileCaps := event.FileFeatureMap{}
	if hasUpload {
		ff := &event.FileFeatures{
			MimeTypes: map[string]event.CapabilitySupportLevel{"*/*": event.CapLevelFullySupported},
			// Captions are sent as a separate message.
			Caption: event.CapLevelPartialSupport,
			MaxSize: maxSize,
		}
		for _, t := range []event.CapabilityMsgType{event.MsgImage, event.MsgVideo, event.MsgAudio, event.MsgFile} {
			fileCaps[t] = ff
		}
	}
	id := "fi.mau.xmpp.capabilities.2026_10_20"
	if isMUC {
		id += "+muc"
	}
	if !hasUpload {
		id += "+noupload"
	}
	return &event.RoomFeatures{
		ID:                  id,
		File:                fileCaps,
		Reply:               event.CapLevelFullySupported,
		Edit:                event.CapLevelFullySupported,
		Delete:              event.CapLevelFullySupported,
		Reaction:            event.CapLevelFullySupported,
		ReadReceipts:        true,
		TypingNotifications: !isMUC,
	}
}

func (c *XMPPClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	j, isMUC, err := parsePortalID(portal.ID)
	if err != nil {
		return nil, err
	}
	if !isMUC {
		return c.dmChatInfo(j), nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	room := c.rooms[j.String()]
	info := &bridgev2.ChatInfo{
		Type:    new(database.RoomTypeDefault),
		Members: &bridgev2.ChatMemberList{MemberMap: bridgev2.ChatMemberMap{}},
	}
	info.Members.MemberMap.Set(bridgev2.ChatMember{EventSender: c.selfSender(), Membership: event.MembershipJoin})
	if room == nil {
		info.Name = new(j.Localpart())
		return info, nil
	}
	name := room.name
	if name == "" {
		name = j.Localpart()
	}
	info.Name = &name
	if room.subject != nil {
		info.Topic = room.subject
	}
	info.CanBackfill = room.mam
	for nick, occ := range room.occupants {
		occJID, err := room.jid.WithResource(nick)
		if err != nil {
			continue
		}
		sender, isMe := c.occupantSenderLocked(room, occJID, "")
		if isMe {
			continue
		}
		n := occ.nick
		info.Members.MemberMap.Add(bridgev2.ChatMember{EventSender: sender, Membership: event.MembershipJoin, Nickname: &n})
	}
	return info, nil
}

func (c *XMPPClient) dmChatInfo(contact jid.JID) *bridgev2.ChatInfo {
	other := makeUserID(contact)
	members := &bridgev2.ChatMemberList{
		IsFull:      true,
		OtherUserID: other,
		MemberMap:   bridgev2.ChatMemberMap{},
	}
	members.MemberMap.Set(bridgev2.ChatMember{EventSender: c.selfSender(), Membership: event.MembershipJoin})
	members.MemberMap.Set(bridgev2.ChatMember{EventSender: bridgev2.EventSender{Sender: other}, Membership: event.MembershipJoin})
	c.mu.Lock()
	canBackfill := c.accountMAM
	c.mu.Unlock()
	return &bridgev2.ChatInfo{
		Type:        new(database.RoomTypeDM),
		Members:     members,
		CanBackfill: canBackfill,
	}
}

func (c *XMPPClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	j, isOccupant, err := parseUserID(ghost.ID)
	if err != nil {
		return nil, err
	}
	if isOccupant {
		nick := j.Resourcepart()
		return &bridgev2.UserInfo{Name: &nick, Identifiers: []string{"xmpp:" + j.String()}}, nil
	}
	info := &bridgev2.UserInfo{Identifiers: []string{"xmpp:" + j.String()}}
	c.mu.Lock()
	item, inRoster := c.roster[j.String()]
	c.mu.Unlock()
	name := ""
	if inRoster {
		name = item.Name
	}
	if s, err := c.session(); err == nil {
		vc, err := s.GetVCard(ctx, j.String())
		if err == nil {
			if name == "" {
				name = vc.FullName
			}
			if name == "" {
				name = vc.Nickname
			}
			if avatar := vcardAvatar(vc); avatar != nil {
				info.Avatar = avatar
			}
		}
	}
	if name == "" {
		name = j.Localpart()
	}
	info.Name = &name
	return info, nil
}

// vcardAvatar returns the inline vCard photo as a bridge avatar.
func vcardAvatar(vc *xmppx.VCard) *bridgev2.Avatar {
	if vc == nil || vc.Photo == nil || strings.TrimSpace(vc.Photo.BinVal) == "" {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(vc.Photo.BinVal), ""))
	if err != nil || len(data) == 0 {
		return nil
	}
	sum := sha1.Sum(data)
	return &bridgev2.Avatar{
		ID: networkid.AvatarID(hex.EncodeToString(sum[:])),
		Get: func(ctx context.Context) ([]byte, error) {
			return data, nil
		},
	}
}

// parseIdentifier accepts a bare JID, optionally as an xmpp: URI.
func parseIdentifier(identifier string) (jid.JID, error) {
	identifier = strings.TrimSpace(identifier)
	identifier = strings.TrimPrefix(identifier, "xmpp:")
	if i := strings.IndexByte(identifier, '?'); i >= 0 {
		identifier = identifier[:i]
	}
	j, err := validateAccountJID(identifier)
	if err != nil {
		return jid.JID{}, bridgev2.WrapRespErrManual(err, "M_INVALID_PARAM", 400)
	}
	return j, nil
}

func (c *XMPPClient) ResolveIdentifier(ctx context.Context, identifier string, createChat bool) (*bridgev2.ResolveIdentifierResponse, error) {
	j, err := parseIdentifier(identifier)
	if err != nil {
		return nil, err
	}
	if j.Equal(c.account) {
		return nil, fmt.Errorf("can't start a chat with yourself")
	}
	userID := makeUserID(j)
	ghost, err := c.main.br.GetGhostByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get ghost: %w", err)
	}
	resp := &bridgev2.ResolveIdentifierResponse{Ghost: ghost, UserID: userID}
	if createChat {
		resp.Chat = &bridgev2.CreateChatResponse{
			PortalKey:  makeDMPortalKey(c.login.ID, j),
			PortalInfo: c.dmChatInfo(j),
		}
	}
	return resp, nil
}

func (c *XMPPClient) GetContactList(ctx context.Context) ([]*bridgev2.ResolveIdentifierResponse, error) {
	c.mu.Lock()
	items := make([]xmppx.RosterItem, 0, len(c.roster))
	for _, item := range c.roster {
		items = append(items, item)
	}
	c.mu.Unlock()
	out := make([]*bridgev2.ResolveIdentifierResponse, 0, len(items))
	for _, item := range items {
		j, err := parseBareJID(item.JID)
		if err != nil || j.Localpart() == "" {
			continue
		}
		userID := makeUserID(j)
		ghost, err := c.main.br.GetGhostByID(ctx, userID)
		if err != nil {
			return nil, err
		}
		resp := &bridgev2.ResolveIdentifierResponse{Ghost: ghost, UserID: userID}
		if item.Name != "" {
			name := item.Name
			resp.UserInfo = &bridgev2.UserInfo{Name: &name, Identifiers: []string{"xmpp:" + j.String()}}
		}
		out = append(out, resp)
	}
	return out, nil
}

// HandleRosterPush keeps the roster cache current.
func (c *XMPPClient) HandleRosterPush(_ *xmppx.Session, items []xmppx.RosterItem) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, item := range items {
		j, err := parseBareJID(item.JID)
		if err != nil {
			continue
		}
		if item.Subscription == "remove" {
			delete(c.roster, j.String())
		} else {
			c.roster[j.String()] = item
		}
	}
}
