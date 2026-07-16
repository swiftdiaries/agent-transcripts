// Package publish provides the authenticated hosted upload client.
package publish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
)

type ChildSource struct {
	SourceName string
	Source     io.Reader
}

type Request struct {
	SourceName  string
	Source      io.Reader
	Destination string
	Title       string
	Description string
	Tags        []string
	Children    []ChildSource
}

type Result struct {
	Location string
	Created  bool
}

type Client struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

func (c Client) Upload(ctx context.Context, input Request) (Result, error) {
	if input.Source == nil || input.SourceName == "" || input.Destination == "" {
		return Result{}, errors.New("source name, source, and destination are required")
	}
	base, err := url.Parse(c.BaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return Result{}, errors.New("invalid publish server URL")
	}
	if strings.TrimSpace(c.Token) == "" {
		return Result{}, errors.New("publish token is required")
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("source", path.Base(input.SourceName))
	if err != nil {
		return Result{}, err
	}
	if _, err := io.Copy(part, input.Source); err != nil {
		return Result{}, fmt.Errorf("read source: %w", err)
	}
	children := append([]ChildSource(nil), input.Children...)
	sort.Slice(children, func(i, j int) bool { return children[i].SourceName < children[j].SourceName })
	for _, child := range children {
		if child.Source == nil || child.SourceName == "" {
			return Result{}, errors.New("child source name and source are required")
		}
		part, err := mw.CreateFormFile("child", path.Base(child.SourceName))
		if err != nil {
			return Result{}, err
		}
		if _, err := io.Copy(part, child.Source); err != nil {
			return Result{}, fmt.Errorf("read child source: %w", err)
		}
	}
	for key, value := range map[string]string{"destination": input.Destination, "title": input.Title, "description": input.Description} {
		if err := mw.WriteField(key, value); err != nil {
			return Result{}, err
		}
	}
	for _, tag := range input.Tags {
		if err := mw.WriteField("tag", tag); err != nil {
			return Result{}, err
		}
	}
	if err := mw.Close(); err != nil {
		return Result{}, err
	}
	endpoint := base.ResolveReference(&url.URL{Path: "/api/v1/sessions"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), &body)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.Token)
	hc := c.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("publish failed: %s", resp.Status)
	}
	location, err := safeLocation(base, resp.Header.Get("Location"))
	if err != nil {
		return Result{}, err
	}
	return Result{Location: location, Created: resp.StatusCode == http.StatusCreated}, nil
}

func safeLocation(base *url.URL, value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || value == "" || u.IsAbs() || u.Host != "" || !strings.HasPrefix(u.Path, "/") {
		return "", errors.New("invalid publish location")
	}
	resolved := base.ResolveReference(u)
	if resolved.Scheme != base.Scheme || resolved.Host != base.Host {
		return "", errors.New("publish location is not same-origin")
	}
	return u.String(), nil
}
