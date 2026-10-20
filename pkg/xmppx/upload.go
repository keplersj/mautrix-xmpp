package xmppx

import (
	"context"
	"encoding/xml"
	"strconv"
)

// UploadService is a XEP-0363 HTTP File Upload service.
type UploadService struct {
	JID string
	// MaxFileSize is the server's limit in bytes, or 0 if not advertised.
	MaxFileSize int64
}

// FindUploadService looks for an HTTP upload service among the server's
// disco items (and the server itself). It returns nil if there is none.
func (s *Session) FindUploadService(ctx context.Context) (*UploadService, error) {
	domain := s.bound.Domainpart()
	candidates := []string{domain}
	items, err := s.DiscoItems(ctx, domain)
	if err == nil {
		for _, it := range items {
			if it.Node == "" {
				candidates = append(candidates, it.JID)
			}
		}
	}
	for _, c := range candidates {
		info, err := s.DiscoInfo(ctx, c)
		if err != nil || !info.HasFeature(NSUpload) {
			continue
		}
		svc := &UploadService{JID: c}
		if v := info.FormValue(NSUpload, "max-file-size"); v != "" {
			svc.MaxFileSize, _ = strconv.ParseInt(v, 10, 64)
		}
		return svc, nil
	}
	return nil, nil
}

type uploadRequest struct {
	XMLName     xml.Name `xml:"urn:xmpp:http:upload:0 request"`
	Filename    string   `xml:"filename,attr"`
	Size        int64    `xml:"size,attr"`
	ContentType string   `xml:"content-type,attr,omitempty"`
}

// UploadSlot is where to PUT a file and where it can then be downloaded.
type UploadSlot struct {
	XMLName xml.Name `xml:"urn:xmpp:http:upload:0 slot"`
	Put     struct {
		URL     string `xml:"url,attr"`
		Headers []struct {
			Name  string `xml:"name,attr"`
			Value string `xml:",chardata"`
		} `xml:"header"`
	} `xml:"put"`
	Get struct {
		URL string `xml:"url,attr"`
	} `xml:"get"`
}

// PutHeaders returns the headers the client is allowed to pass through for
// the PUT request (XEP-0363 §5).
func (u *UploadSlot) PutHeaders() map[string]string {
	h := make(map[string]string)
	for _, hd := range u.Put.Headers {
		switch hd.Name {
		case "Authorization", "Cookie", "Expires":
			h[hd.Name] = hd.Value
		}
	}
	return h
}

// RequestUploadSlot asks the upload service for a slot.
func (s *Session) RequestUploadSlot(ctx context.Context, service, filename string, size int64, contentType string) (*UploadSlot, error) {
	var slot UploadSlot
	err := s.IQ(ctx, "get", service, uploadRequest{Filename: filename, Size: size, ContentType: contentType}, &slot)
	if err != nil {
		return nil, err
	}
	return &slot, nil
}
