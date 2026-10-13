package domain

import (
	"fmt"
	"net/url"
	"strings"
)

const (
	MaxLinkLabelLen = 64
	MaxLinkURLLen   = 2048
)

// Link is a labelled reference attached to a report. Only absolute http/https
// URLs are accepted, which is what keeps javascript:, data: and protocol
// relative URLs out of rendered output.
type Link struct {
	Label string
	URL   string
}

func NewLink(label, rawURL string) (Link, error) {
	label = strings.TrimSpace(label)
	rawURL = strings.TrimSpace(rawURL)
	if label == "" || len(label) > MaxLinkLabelLen {
		return Link{}, fmt.Errorf("%w: label", ErrInvalidLink)
	}
	if rawURL == "" || len(rawURL) > MaxLinkURLLen {
		return Link{}, fmt.Errorf("%w: url length", ErrInvalidLink)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return Link{}, fmt.Errorf("%w: %w", ErrInvalidLink, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return Link{}, fmt.Errorf("%w: scheme %q", ErrInvalidLink, u.Scheme)
	}
	if u.Host == "" {
		return Link{}, fmt.Errorf("%w: missing host", ErrInvalidLink)
	}
	return Link{Label: label, URL: rawURL}, nil
}
