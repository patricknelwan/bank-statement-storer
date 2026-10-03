package gmail

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"example.com/bca/internal/parser/bca"
	"golang.org/x/net/html/charset"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

type MessageSource interface {
	Metadata(context.Context, string) (string, error)
	Full(context.Context, string) (FullMessage, error)
}

type FullMessage struct {
	Body     []byte
	Subject  string
	Verified bool
}

type SourceInfo struct {
	Subject     string `json:"subject"`
	Date        string `json:"date"`
	GmailSearch string `json:"gmail_search"`
}

type Provider struct{ API *gmail.Service }

func NewProvider(ctx context.Context, client *http.Client) (*Provider, error) {
	api, err := gmail.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return nil, err
	}
	return &Provider{api}, nil
}
func (p Provider) Metadata(ctx context.Context, id string) (string, error) {
	m, err := p.API.Users.Messages.Get("me", id).Format("metadata").MetadataHeaders("From").Fields("id,payload/headers").Context(ctx).Do()
	if err != nil {
		return "", err
	}
	if m.Payload == nil {
		return "", nil
	}
	var from string
	for _, h := range m.Payload.Headers {
		if strings.EqualFold(h.Name, "From") {
			if from != "" {
				return "", nil
			}
			from = h.Value
		}
	}
	return from, nil
}

func AuthorizedSender(ctx context.Context, source MessageSource, id string) (bool, error) {
	from, err := source.Metadata(ctx, id)
	if err != nil {
		return false, err
	}
	return bca.ExactSender(from), nil
}

// SourceMetadata reads headers only, after the caller has passed the From-only gate.
func (p Provider) SourceMetadata(ctx context.Context, id string) (SourceInfo, error) {
	m, err := p.API.Users.Messages.Get("me", id).Format("metadata").MetadataHeaders("From", "Subject", "Date", "Message-ID").Fields("id,payload/headers").Context(ctx).Do()
	if err != nil {
		return SourceInfo{}, err
	}
	if m.Payload == nil {
		return SourceInfo{}, errors.New("empty message metadata")
	}
	var from, messageID string
	info := SourceInfo{}
	for _, h := range m.Payload.Headers {
		switch strings.ToLower(h.Name) {
		case "from":
			if from != "" {
				return SourceInfo{}, errors.New("duplicate sender")
			}
			from = h.Value
		case "subject":
			info.Subject = strings.TrimSpace(h.Value)
		case "date":
			info.Date = strings.TrimSpace(h.Value)
		case "message-id":
			if messageID != "" {
				return SourceInfo{}, errors.New("duplicate message-id")
			}
			messageID = strings.Trim(strings.TrimSpace(h.Value), "<>")
		}
	}
	if !bca.ExactSender(from) {
		return SourceInfo{}, errors.New("sender changed")
	}
	if len(messageID) > 0 && len(messageID) <= 255 && strings.Count(messageID, "@") == 1 && !strings.ContainsAny(messageID, " \t\r\n<>:") {
		info.GmailSearch = "rfc822msgid:" + messageID
	}
	return info, nil
}

func (p Provider) Full(ctx context.Context, id string) (FullMessage, error) {
	m, err := p.API.Users.Messages.Get("me", id).Format("full").Fields("id,payload").Context(ctx).Do()
	if err != nil {
		return FullMessage{}, err
	}
	if m.Payload == nil {
		return FullMessage{}, errors.New("empty message")
	}
	// Gmail prepends its receiver authentication result. Never trust body text as evidence.
	message := FullMessage{}
	subjectSeen, authSeen := false, false
	for _, h := range m.Payload.Headers {
		if strings.EqualFold(h.Name, "Subject") {
			if subjectSeen {
				return FullMessage{}, errors.New("duplicate subject")
			}
			subjectSeen = true
			message.Subject = h.Value
		}
		if strings.EqualFold(h.Name, "Authentication-Results") && !authSeen {
			authSeen = true
			v := strings.ToLower(strings.TrimSpace(h.Value))
			message.Verified = strings.HasPrefix(v, "mx.google.com;") && strings.Contains(v, "dkim=pass") &&
				(strings.Contains(v, "header.i=@bca.co.id") || strings.Contains(v, "header.d=bca.co.id")) &&
				strings.Contains(v, "dmarc=pass") && strings.Contains(v, "header.from=bca.co.id")
		}
	}
	message.Body, err = bestPart(m.Payload)
	return message, err
}
func bestPart(root *gmail.MessagePart) ([]byte, error) {
	var htmlPart, plainPart *gmail.MessagePart
	var walk func(*gmail.MessagePart)
	walk = func(p *gmail.MessagePart) {
		if p == nil {
			return
		}
		switch strings.ToLower(strings.Split(p.MimeType, ";")[0]) {
		case "text/html":
			if htmlPart == nil && p.Body != nil && p.Body.Data != "" {
				htmlPart = p
			}
		case "text/plain":
			if plainPart == nil && p.Body != nil && p.Body.Data != "" {
				plainPart = p
			}
		}
		for _, c := range p.Parts {
			walk(c)
		}
	}
	walk(root)
	p := htmlPart
	if p == nil {
		p = plainPart
	}
	if p == nil && root.Body != nil && root.Body.Data != "" {
		p = root
	}
	if p == nil || p.Body == nil {
		return nil, errors.New("body unavailable")
	}
	if p.Body.Size > bca.MaxBody {
		return nil, errors.New("body too large")
	}
	encoded := p.Body.Data
	if len(encoded) > 2*bca.MaxBody {
		return nil, errors.New("body too large")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(encoded)
	}
	if err != nil || len(raw) > bca.MaxBody {
		return nil, errors.New("invalid or oversized body")
	}
	var contentType string
	for _, h := range p.Headers {
		if strings.EqualFold(h.Name, "Content-Type") {
			contentType = h.Value
			break
		}
	}
	if contentType != "" {
		reader, err := charset.NewReader(strings.NewReader(string(raw)), contentType)
		if err != nil {
			return nil, fmt.Errorf("unsupported charset: %w", err)
		}
		raw, err = io.ReadAll(io.LimitReader(reader, bca.MaxBody+1))
		if err != nil || len(raw) > bca.MaxBody {
			return nil, errors.New("invalid or oversized body")
		}
	}
	return raw, nil
}
