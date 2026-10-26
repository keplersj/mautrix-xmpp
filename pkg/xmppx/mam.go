package xmppx

import (
	"context"
	"encoding/xml"
	"fmt"
	"sync"
	"time"

	"mellium.im/xmpp/jid"
)

// MAMQuery describes a XEP-0313 archive query.
type MAMQuery struct {
	// Archive is the archive to query: "" for the account's own archive, or a
	// room JID.
	Archive string
	// With filters the own archive to one conversation partner.
	With string
	// Start and End filter by time (zero means unbounded).
	Start, End time.Time
	// After pages forward from an archive ID.
	After string
	// Before pages backwards from an archive ID. If BeforeLast is set the
	// last page is requested (RSM empty <before/>).
	Before     string
	BeforeLast bool
	// Max is the page size.
	Max int
}

// ArchivedMessage is one message from an archive.
type ArchivedMessage struct {
	// ArchiveID is the archive's ID for this message (the result id).
	ArchiveID string
	// Timestamp comes from the forwarded delay.
	Timestamp time.Time
	Message   *Message
}

// MAMPage is one page of results.
type MAMPage struct {
	Messages []*ArchivedMessage
	First    string
	Last     string
	Complete bool
	Count    int
}

type mamCollector struct {
	archive jid.JID
	results []*ArchivedMessage
}

type mamQueryElem struct {
	XMLName xml.Name  `xml:"urn:xmpp:mam:2 query"`
	QueryID string    `xml:"queryid,attr"`
	Form    *DataForm `xml:"jabber:x:data x,omitempty"`
	Set     rsmSet    `xml:"http://jabber.org/protocol/rsm set"`
}

type rsmSet struct {
	Max    int     `xml:"max,omitempty"`
	After  *string `xml:"after,omitempty"`
	Before *string `xml:"before,omitempty"`
}

type mamFin struct {
	XMLName  xml.Name `xml:"urn:xmpp:mam:2 fin"`
	Complete string   `xml:"complete,attr"`
	Set      struct {
		First string `xml:"first"`
		Last  string `xml:"last"`
		Count int    `xml:"count"`
	} `xml:"http://jabber.org/protocol/rsm set"`
}

type mamState struct {
	mu      sync.Mutex
	queries map[string]*mamCollector
}

// collectMAMResult is called from the read loop for messages carrying a MAM
// <result/>. It returns true if the message belonged to an active query.
func (s *Session) collectMAMResult(msg *Message) bool {
	res := msg.MAMResult
	s.mam.mu.Lock()
	defer s.mam.mu.Unlock()
	c, ok := s.mam.queries[res.QueryID]
	if !ok {
		return false
	}
	// Results must come from the archive we asked (RFC 6120 §8.1.2.1: no from
	// means our own account).
	from := s.bound.Bare()
	if msg.From != "" {
		j, err := jid.Parse(msg.From)
		if err != nil {
			return true
		}
		from = j
	}
	if !from.Bare().Equal(c.archive) {
		s.log.Warn().Str("from", msg.From).Msg("Dropping MAM result from unexpected sender")
		return true
	}
	if res.Forwarded.Message == nil {
		return true
	}
	am := &ArchivedMessage{ArchiveID: res.ID, Message: res.Forwarded.Message}
	if res.Forwarded.Delay != nil {
		am.Timestamp = res.Forwarded.Delay.Stamp
	}
	c.results = append(c.results, am)
	return true
}

// QueryMAM runs one archive query and returns a page of results in
// chronological order.
func (s *Session) QueryMAM(ctx context.Context, q MAMQuery) (*MAMPage, error) {
	archive := s.bound.Bare()
	if q.Archive != "" {
		j, err := jid.Parse(q.Archive)
		if err != nil {
			return nil, fmt.Errorf("invalid archive JID: %w", err)
		}
		archive = j.Bare()
	}
	queryID := RandomID()
	elem := mamQueryElem{QueryID: queryID}
	fields := []FormField{{Var: "FORM_TYPE", Type: "hidden", Values: []string{NSMAM}}}
	if q.With != "" {
		fields = append(fields, FormField{Var: "with", Values: []string{q.With}})
	}
	if !q.Start.IsZero() {
		fields = append(fields, FormField{Var: "start", Values: []string{q.Start.UTC().Format(time.RFC3339Nano)}})
	}
	if !q.End.IsZero() {
		fields = append(fields, FormField{Var: "end", Values: []string{q.End.UTC().Format(time.RFC3339Nano)}})
	}
	elem.Form = &DataForm{Type: "submit", Fields: fields}
	elem.Set.Max = q.Max
	switch {
	case q.After != "":
		elem.Set.After = &q.After
	case q.Before != "":
		elem.Set.Before = &q.Before
	case q.BeforeLast:
		empty := ""
		elem.Set.Before = &empty
	}

	c := &mamCollector{archive: archive}
	s.mam.mu.Lock()
	s.mam.queries[queryID] = c
	s.mam.mu.Unlock()
	defer func() {
		s.mam.mu.Lock()
		delete(s.mam.queries, queryID)
		s.mam.mu.Unlock()
	}()

	var fin mamFin
	if err := s.iq(ctx, IQOpMAM, "set", q.Archive, elem, &fin); err != nil {
		return nil, err
	}
	s.mam.mu.Lock()
	results := c.results
	s.mam.mu.Unlock()
	s.obs.MAMPage(len(results))
	return &MAMPage{
		Messages: results,
		First:    fin.Set.First,
		Last:     fin.Set.Last,
		Complete: xmlBool(fin.Complete),
		Count:    fin.Set.Count,
	}, nil
}
