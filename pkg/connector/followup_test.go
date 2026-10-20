package connector

import (
	"context"
	"fmt"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"

	"github.com/keplersj/mautrix-xmpp/pkg/xmppx"
)

func TestBackfillMaxBatchCount(t *testing.T) {
	tc := newTestClient(t)
	tc.main.br = &bridgev2.Bridge{Config: &bridgeconfig.BridgeConfig{Backfill: bridgeconfig.BackfillConfig{
		Queue: bridgeconfig.BackfillQueueConfig{
			MaxBatches: 0,
			// What bbctl generates.
			MaxBatchesOverride: map[string]int{"channel": 10, "supergroup": 10, "dm": -1, "group_dm": -1},
		},
	}}}
	ctx := context.Background()
	if n := tc.GetBackfillMaxBatchCount(ctx, dmPortal(), nil); n != -1 {
		t.Errorf("dm = %d, want -1", n)
	}
	if n := tc.GetBackfillMaxBatchCount(ctx, mucPortal(), nil); n != 10 {
		t.Errorf("muc = %d, want 10", n)
	}
	tc.main.br.Config.Backfill.Queue.MaxBatchesOverride = nil
	if n := tc.GetBackfillMaxBatchCount(ctx, mucPortal(), nil); n != 0 {
		t.Errorf("without overrides = %d, want max_batches", n)
	}
}

func TestOccupantLeaveRequiresJoin(t *testing.T) {
	tc := newTestClient(t)
	tc.joinRoom()
	tc.HandlePresence(nil, &xmppx.Presence{From: roomJID + "/Anon", Type: "unavailable", MUCUser: &xmppx.MUCUser{}})
	evt := single[*simplevent.ChatInfoChange](t, tc.events)
	member := evt.ChatInfoChange.MemberChanges.MemberMap["occ:"+roomJID+"/Anon"]
	if member.Membership != event.MembershipLeave || member.PrevMembership != event.MembershipJoin {
		t.Errorf("leave = %+v", member)
	}
}

func TestInitialOccupantsCapped(t *testing.T) {
	tc := newTestClient(t)
	tc.joinRoom()
	room := tc.rooms[roomJID]
	for i := range 200 {
		nick := fmt.Sprintf("lurker%03d", i)
		room.occupants[nick] = &mucOccupant{nick: nick}
	}
	room.occupants["Owner"] = &mucOccupant{nick: "Owner", affiliation: "owner"}
	info, err := tc.GetChatInfo(context.Background(), mucPortal())
	if err != nil {
		t.Fatal(err)
	}
	// Self + at most maxInitialOccupants others (self is skipped from the cap).
	if n := len(info.Members.MemberMap); n > maxInitialOccupants+1 {
		t.Errorf("member map has %d entries", n)
	}
	if info.Members.IsFull || info.Members.TotalMemberCount != len(room.occupants) {
		t.Errorf("members = full %v total %d", info.Members.IsFull, info.Members.TotalMemberCount)
	}
	for _, want := range []string{"occ:" + roomJID + "/Owner", bobJID} {
		if _, ok := info.Members.MemberMap[networkid.UserID(want)]; !ok {
			t.Errorf("%s should be prioritized", want)
		}
	}
}

func TestOlderThan(t *testing.T) {
	anchor := time.Date(2026, 10, 20, 6, 24, 42, 0, time.UTC)
	msgs := []*xmppx.ArchivedMessage{
		{ArchiveID: "old", Timestamp: anchor.Add(-time.Hour)},
		{ArchiveID: "same", Timestamp: anchor},
		{ArchiveID: "new", Timestamp: anchor.Add(time.Second)},
		{ArchiveID: "unknown"},
	}
	got := olderThan(msgs, anchor)
	if len(got) != 2 || got[0].ArchiveID != "old" || got[1].ArchiveID != "unknown" {
		t.Errorf("got %+v", got)
	}
	if len(msgs) != 4 || msgs[1].ArchiveID != "same" {
		t.Error("input must not be modified")
	}
}
