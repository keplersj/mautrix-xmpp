package connector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"

	"github.com/keplersj/mautrix-xmpp/pkg/metrics"
)

var evtTypeMessage = event.EventMessage

var httpClient = &http.Client{Timeout: 5 * time.Minute}

var errTooLarge = errors.New("file is too large")

// downloadFile fetches an HTTP(S) URL with a size limit.
func downloadFile(ctx context.Context, rawURL string, maxSize int64) (data []byte, contentType string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "mautrix-xmpp")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("unexpected HTTP status %s", resp.Status)
	}
	if resp.ContentLength > maxSize {
		return nil, "", errTooLarge
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > maxSize {
		return nil, "", errTooLarge
	}
	return data, resp.Header.Get("Content-Type"), nil
}

// convertAttachment downloads a linked file and reuploads it to Matrix. If
// that fails the link is bridged as text, so nothing is lost.
func (c *XMPPClient) convertAttachment(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, att attachment) *bridgev2.ConvertedMessagePart {
	log := zerolog.Ctx(ctx)
	fileName := fileNameFromURL(att.URL)
	fallback := func(reason string) *bridgev2.ConvertedMessagePart {
		body := att.URL
		if att.Caption != "" {
			body = att.Caption + "\n" + att.URL
		}
		if reason != "" {
			body += "\n(" + reason + ")"
		}
		return &bridgev2.ConvertedMessagePart{
			Type:    evtTypeMessage,
			Content: &event.MessageEventContent{MsgType: event.MsgText, Body: body},
		}
	}
	data, header, err := downloadFile(ctx, att.URL, c.main.Config.Media.MaxDownloadSize)
	if errors.Is(err, errTooLarge) {
		c.main.metrics.MediaTransfer(metrics.MediaDownload, metrics.MediaTooLarge, 0)
		return fallback("file too large to bridge")
	} else if err != nil {
		c.main.metrics.MediaTransfer(metrics.MediaDownload, metrics.MediaFailed, 0)
		log.Warn().Err(err).Msg("Failed to download attachment")
		return fallback("")
	}
	c.main.metrics.MediaTransfer(metrics.MediaDownload, metrics.MediaOK, len(data))
	mimeType := guessMime(header, fileName)
	content := &event.MessageEventContent{
		MsgType: msgTypeForMime(mimeType),
		Body:    fileName,
		Info:    &event.FileInfo{MimeType: mimeType, Size: len(data)},
	}
	if att.Caption != "" {
		content.FileName = fileName
		content.Body = att.Caption
	}
	content.URL, content.File, err = intent.UploadMedia(ctx, portal.MXID, data, fileName, mimeType)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to upload attachment to Matrix")
		return fallback("")
	}
	return &bridgev2.ConvertedMessagePart{Type: evtTypeMessage, Content: content}
}

// uploadToXMPP uploads a file with XEP-0363 and returns its download URL.
func (c *XMPPClient) uploadToXMPP(ctx context.Context, data []byte, fileName, mimeType string) (string, error) {
	result := metrics.MediaFailed
	defer func() { c.main.metrics.MediaTransfer(metrics.MediaUpload, result, len(data)) }()
	s, err := c.session()
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	svc := c.upload
	c.mu.Unlock()
	if svc == nil {
		return "", errors.New("the XMPP server has no HTTP file upload service")
	}
	size := int64(len(data))
	if svc.MaxFileSize > 0 && size > svc.MaxFileSize {
		result = metrics.MediaTooLarge
		return "", fmt.Errorf("file is larger than the server's upload limit (%d bytes)", svc.MaxFileSize)
	}
	if size > c.main.Config.Media.MaxUploadSize {
		result = metrics.MediaTooLarge
		return "", fmt.Errorf("file is larger than the bridge's upload limit (%d bytes)", c.main.Config.Media.MaxUploadSize)
	}
	slot, err := s.RequestUploadSlot(ctx, svc.JID, fileName, size, mimeType)
	if err != nil {
		return "", fmt.Errorf("failed to request upload slot: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, slot.Put.URL, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", mimeType)
	for k, v := range slot.PutHeaders() {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to upload file: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upload failed with HTTP status %s", resp.Status)
	}
	result = metrics.MediaOK
	return slot.Get.URL, nil
}
