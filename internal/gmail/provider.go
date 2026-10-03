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
	Full(context.Context, string) ([]byte, bool, error)
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

func (p Provider) Full(ctx context.Context, id string) ([]byte, bool, error) {
	m, err := p.API.Users.Messages.Get("me", id).Format("full").Fields("id,payload").Context(ctx).Do()
	if err != nil {
		return nil, false, err
	}
	if m.Payload == nil {
		return nil, false, errors.New("empty message")
	}
	// Gmail prepends its receiver authentication result. Never trust body text as evidence.
	verified := false
	for _, h := range m.Payload.Headers {
		if strings.EqualFold(h.Name, "Authentication-Results") {
			v := strings.ToLower(strings.TrimSpace(h.Value))
			verified = strings.HasPrefix(v, "mx.google.com;") && strings.Contains(v, "dkim=pass") &&
				(strings.Contains(v, "header.i=@bca.co.id") || strings.Contains(v, "header.d=bca.co.id")) &&
				strings.Contains(v, "dmarc=pass") && strings.Contains(v, "header.from=bca.co.id")
			break
		}
	}
	body, err := bestPart(m.Payload)
	return body, verified, err
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
